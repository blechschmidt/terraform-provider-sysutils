package provider

import (
	"context"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/function"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"
)

func TestParseIniSections(t *testing.T) {
	content := "\xef\xbb\xbfroot = true\r\n" +
		"; comment\r\n" +
		"[server]\r\n" +
		"  host = example.org  \r\n" +
		"port=8080\r\n" +
		"#commented = yes\r\n" +
		"[empty]\r\n" +
		"[bad] header\r\n" +
		"flag\r\n" +
		"[server]\r\n" +
		"port = 9090 ; not a comment\r\n" +
		"name = \"quoted\"\r\n" +
		"url = http://x/?a=b"
	got := parseIniSections(content)
	want := map[string]map[string]string{
		"":       {"root": "true"},
		"server": {"host": "example.org", "port": "9090 ; not a comment", "name": `"quoted"`, "url": "http://x/?a=b"},
		// "[bad] header" is not a header, so "flag" is still in [empty];
		// the malformed line itself is skipped.
		"empty": {"flag": ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseIniSections:\n got %#v\nwant %#v", got, want)
	}

	if got := parseIniSections("[a]\nk = v\n"); got[""] != nil {
		t.Errorf("global section present without keys: %#v", got)
	}
	if got := parseIniSections(""); len(got) != 0 {
		t.Errorf("empty content: %#v", got)
	}
}

func TestParsePasswdLine(t *testing.T) {
	e, ok, err := parsePasswdLine("www-data:x:33:33:www-data,,,:/var/www:/usr/sbin/nologin\r\n")
	if err != nil || !ok {
		t.Fatalf("got ok=%v err=%v", ok, err)
	}
	want := passwdLine{name: "www-data", password: "x", uid: 33, gid: 33, gecos: "www-data,,,", home: "/var/www", shell: "/usr/sbin/nologin"}
	if e != want {
		t.Fatalf("got %#v, want %#v", e, want)
	}

	e, ok, err = parsePasswdLine("nobody:*:4294967295:4294967295:::")
	if err != nil || !ok || e.uid != 4294967295 || e.shell != "" {
		t.Fatalf("max ids, empty fields: %#v ok=%v err=%v", e, ok, err)
	}

	for _, skip := range []string{"", "   ", "\n", "# comment", "  #x:y"} {
		if _, ok, err := parsePasswdLine(skip); ok || err != nil {
			t.Errorf("%q: got ok=%v err=%v, want skipped", skip, ok, err)
		}
	}

	for line, msg := range map[string]string{
		"root:x:0:0:root:/root":                    "shell), got 6",
		"root:x:0:0:root:/root:/bin/sh:extra":      "got 8",
		":x:0:0::/:/bin/sh":                        "user name",
		"root:x:-1:0::/:/bin/sh":                   "uid (field 3)",
		"root:x:0:4294967296::/:/bin/sh":           "gid (field 4)",
		"root:x:zero:0::/:/bin/sh":                 "uid",
		"root:x:0:0::/:/bin/sh\nbin:x:1:1::/:/bin": "single line",
	} {
		if _, _, err := parsePasswdLine(line); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("%q: got %v, want error containing %q", line, err, msg)
		}
	}
}

func TestParseFstabFunctionLine(t *testing.T) {
	e, ok, err := parseFstabFunctionLine("UUID=abc\t/mnt/my\\040disk  ext4 noatime,context=\"a:b,c\" 1 2 ignored\n")
	if err != nil || !ok {
		t.Fatalf("got ok=%v err=%v", ok, err)
	}
	want := fstabEntry{device: "UUID=abc", mountPoint: "/mnt/my disk", fstype: "ext4", options: []string{"noatime", `context="a:b,c"`}, dump: 1, pass: 2}
	if !e.equal(want) {
		t.Fatalf("got %#v, want %#v", e, want)
	}

	e, ok, err = parseFstabFunctionLine("tmpfs /tmp")
	if err != nil || !ok || e.fstype != "" || !reflect.DeepEqual(e.options, []string{"defaults"}) || e.dump != 0 || e.pass != 0 {
		t.Fatalf("defaults: %#v ok=%v err=%v", e, ok, err)
	}

	for _, skip := range []string{"", " \t", "# /dev/sda1 / ext4", "   #x"} {
		if _, ok, err := parseFstabFunctionLine(skip); ok || err != nil {
			t.Errorf("%q: got ok=%v err=%v, want skipped", skip, ok, err)
		}
	}
	for line, msg := range map[string]string{
		"/dev/sda1":                       "device and a mount point",
		"/dev/sda1 / ext4 defaults x 0":   "dump and pass",
		"/dev/sda1 / ext4 defaults 0 -1":  "dump and pass",
		"/dev/sda1 / ext4\n/dev/b /b xfs": "single line",
	} {
		if _, _, err := parseFstabFunctionLine(line); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("%q: got %v, want error containing %q", line, err, msg)
		}
	}
}

func TestNormaliseMode(t *testing.T) {
	for in, want := range map[string]string{
		// Octal.
		"644": "0644", "0644": "0644", "1777": "1777", "0755": "0755", "000": "0000", "7777": "7777",
		// ls -l.
		"rw-r--r--": "0644", "-rw-r--r--": "0644", "rwxr-xr-x": "0755", "drwxrwxrwt": "1777",
		"rwsr-xr-x": "4755", "rwSr--r--": "4644", "rwxr-sr-x": "2755", "rwxrwxrwT": "1776", "---------": "0000",
		// chmod symbolic.
		"u=rw,go=r": "0644", "a=rx,u+w": "0755", "ug=rwx,o=": "0770", "=r": "0444", "+x": "0111",
		"u=rwxs,g=rx": "4750", "g=rxs": "2050", "a=rwx,+t": "1777", "+t,a=rwx": "0777", "06444": "6444", "o+t": "1000", "g+t": "0000",
		"a=rwx,go-w": "0755", "u=rw,u-w": "0400", "u=rw+x-r": "0300", "a=rwx,u=": "0077", "o=s": "0000",
	} {
		got, err := normaliseMode(in)
		if err != nil || got != want {
			t.Errorf("normaliseMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for in, msg := range map[string]string{
		"":           "must not be empty",
		"8644":       "expected +, - or =",
		"rw-r--r-":   "expected +, - or =",
		"xrwxr-xr-x": "expected +, - or =",
		"u=rwX":      "depends on an existing file",
		"g=u":        "depends on an existing file",
		"u":          "no operator",
		"u=rw,":      "no operator",
		"u=rw q":     "unknown permission",
		"z=rw":       "expected +, - or =",
	} {
		if _, err := normaliseMode(in); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("normaliseMode(%q): got %v, want error containing %q", in, err, msg)
		}
	}
	if got, err := normaliseMode("rw-r--r-x"); err != nil || got != "0645" {
		t.Errorf("rw-r--r-x: %q %v", got, err)
	}
}

// runFunction calls a provider function the way the framework does.
func runFunction(t *testing.T, f function.Function, arg string) (attr.Value, *function.FuncError) {
	t.Helper()
	ctx := context.Background()
	var def function.DefinitionResponse
	f.Definition(ctx, function.DefinitionRequest{}, &def)
	var validate function.DefinitionValidateResponse
	def.Definition.ValidateImplementation(ctx, function.DefinitionValidateRequest{}, &validate)
	if validate.Diagnostics.HasError() {
		t.Fatalf("invalid definition: %v", validate.Diagnostics)
	}
	result, ferr := def.Definition.Return.NewResultData(ctx)
	if ferr != nil {
		t.Fatal(ferr)
	}
	resp := function.RunResponse{Result: result}
	f.Run(ctx, function.RunRequest{Arguments: function.NewArgumentsData([]attr.Value{types.StringValue(arg)})}, &resp)
	return resp.Result.Value(), resp.Error
}

func TestProviderFunctionsRun(t *testing.T) {
	names := map[string]bool{}
	for _, newFunc := range providerFunctions() {
		var md function.MetadataResponse
		newFunc().Metadata(context.Background(), function.MetadataRequest{}, &md)
		names[md.Name] = true
	}
	for _, n := range []string{"parse_ini", "parse_os_release", "parse_passwd_line", "parse_fstab_line", "mode_to_octal"} {
		if !names[n] {
			t.Errorf("function %s is not registered", n)
		}
	}

	v, ferr := runFunction(t, newParsePasswdLineFunction(), "# comment")
	if ferr != nil || !v.IsNull() {
		t.Errorf("parse_passwd_line comment: %v %v", v, ferr)
	}
	v, ferr = runFunction(t, newParseFstabLineFunction(), "/dev/sda1 / ext4 ro,noatime 0 1")
	if ferr != nil || !strings.Contains(v.String(), `"options":["ro","noatime"]`) {
		t.Errorf("parse_fstab_line: %v %v", v, ferr)
	}
	_, ferr = runFunction(t, newModeToOctalFunction(), "u=rwX")
	if ferr == nil || ferr.FunctionArgument == nil || *ferr.FunctionArgument != 0 || !strings.Contains(ferr.Text, "Mode \"u=rwX\" is neither") {
		t.Errorf("mode_to_octal error: %#v", ferr)
	}
	_, ferr = runFunction(t, newParseOSReleaseFunction(), strings.Repeat("#", maxOSReleaseSize+1))
	if ferr == nil || !strings.Contains(ferr.Text, "limit") {
		t.Errorf("parse_os_release size limit: %#v", ferr)
	}
}

// Provider-defined functions need Terraform 1.8 or later (OpenTofu 1.7);
// CI also runs Terraform 1.5, where these tests are skipped.
var functionVersionChecks = []tfversion.TerraformVersionCheck{tfversion.SkipBelow(tfversion.Version1_8_0)}

func TestAccFunctions(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		TerraformVersionChecks:   functionVersionChecks,
		Steps: []resource.TestStep{{
			Config: `
locals {
  passwd = <<-EOT
    root:x:0:0:root:/root:/bin/bash
    # comment

    www-data:x:33:33:www-data:/var/www:/usr/sbin/nologin
  EOT
  fstab = <<-EOT
    # <device> <mount point> <type> <options> <dump> <pass>
    UUID=1234 /         ext4 errors=remount-ro 0 1
    tmpfs     /mnt/a\040b tmpfs size=10%,mode=1777
  EOT
  users  = [for l in split("\n", local.passwd) : provider::sysutils::parse_passwd_line(l) if provider::sysutils::parse_passwd_line(l) != null]
  mounts = [for l in split("\n", local.fstab) : provider::sysutils::parse_fstab_line(l) if provider::sysutils::parse_fstab_line(l) != null]
}

output "ini" {
  value = provider::sysutils::parse_ini("top = 1\n[Service]\nUser = app\n; x = y\n[Install]\nWantedBy=multi-user.target\n")
}

output "os_release" {
  value = provider::sysutils::parse_os_release("ID=debian\nVERSION_ID=\"12\"\nPRETTY_NAME='Debian GNU/Linux 12 (bookworm)'\n")
}

output "users" {
  value = { for u in local.users : u.name => u.uid }
}

output "www_shell" {
  value = local.users[1].shell
}

output "mounts" {
  value = { for m in local.mounts : m.mount_point => m.options }
}

output "root_pass" {
  value = local.mounts[0].pass
}

output "modes" {
  value = [for m in ["644", "rwxr-xr-x", "u=rwx,go=rx", "drwxrwxrwt"] : provider::sysutils::mode_to_octal(m)]
}
`,
			ConfigStateChecks: []statecheck.StateCheck{
				statecheck.ExpectKnownOutputValue("ini", knownvalue.MapExact(map[string]knownvalue.Check{
					"":        knownvalue.MapExact(map[string]knownvalue.Check{"top": knownvalue.StringExact("1")}),
					"Service": knownvalue.MapExact(map[string]knownvalue.Check{"User": knownvalue.StringExact("app")}),
					"Install": knownvalue.MapExact(map[string]knownvalue.Check{"WantedBy": knownvalue.StringExact("multi-user.target")}),
				})),
				statecheck.ExpectKnownOutputValue("os_release", knownvalue.MapExact(map[string]knownvalue.Check{
					"ID":          knownvalue.StringExact("debian"),
					"VERSION_ID":  knownvalue.StringExact("12"),
					"PRETTY_NAME": knownvalue.StringExact("Debian GNU/Linux 12 (bookworm)"),
				})),
				statecheck.ExpectKnownOutputValue("users", knownvalue.MapExact(map[string]knownvalue.Check{
					"root":     knownvalue.Int64Exact(0),
					"www-data": knownvalue.Int64Exact(33),
				})),
				statecheck.ExpectKnownOutputValue("www_shell", knownvalue.StringExact("/usr/sbin/nologin")),
				statecheck.ExpectKnownOutputValue("mounts", knownvalue.MapExact(map[string]knownvalue.Check{
					"/":        knownvalue.ListExact([]knownvalue.Check{knownvalue.StringExact("errors=remount-ro")}),
					"/mnt/a b": knownvalue.ListExact([]knownvalue.Check{knownvalue.StringExact("size=10%"), knownvalue.StringExact("mode=1777")}),
				})),
				statecheck.ExpectKnownOutputValue("root_pass", knownvalue.Int64Exact(1)),
				statecheck.ExpectKnownOutputValue("modes", knownvalue.ListExact([]knownvalue.Check{
					knownvalue.StringExact("0644"), knownvalue.StringExact("0755"), knownvalue.StringExact("0755"), knownvalue.StringExact("1777"),
				})),
			},
		}},
	})
}

func TestAccFunctionErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		expr string
		err  string
	}{
		"mode":   {`provider::sysutils::mode_to_octal("u=rwX")`, `(?s)"mode"\s+parameter:\s+Mode\s+"u=rwX".*depends\s+on\s+an\s+existing\s+file`},
		"passwd": {`provider::sysutils::parse_passwd_line("root:x:0:0")`, `7\s+colon-separated\s+fields`},
		"fstab":  {`provider::sysutils::parse_fstab_line("/dev/sda1 / ext4 defaults x 0")`, `dump\s+and\s+pass`},
	} {
		t.Run(name, func(t *testing.T) {
			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				TerraformVersionChecks:   functionVersionChecks,
				Steps: []resource.TestStep{{
					Config:      "output \"x\" {\n  value = " + tc.expr + "\n}\n",
					ExpectError: regexp.MustCompile(tc.err),
				}},
			})
		})
	}
}
