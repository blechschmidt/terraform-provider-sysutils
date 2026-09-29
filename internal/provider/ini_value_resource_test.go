package provider

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const testIniResource = "sysutils_ini_value.test"

// iniValueConfig renders a sysutils_ini_value resource named "test" with the
// given extra attributes (already formatted HCL).
func iniValueConfig(p, attrs string) string {
	return fmt.Sprintf(`
resource "sysutils_ini_value" "test" {
  path = %q
%s
}`, p, attrs)
}

func TestAccIniValue_lifecycle(t *testing.T) {
	requireRoot(t)

	dir := t.TempDir()
	p := filepath.Join(dir, "app.ini")
	const orig = "; global comment\nname = app\n\n[server]\n# the port\nport = 80\nhost = 0.0.0.0\n\n[log]\nlevel = info\n"
	mustWrite(t, p, orig)
	// Mode and ownership that edits must preserve.
	mustChmod(t, p, 0o640)
	if err := os.Chown(p, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	config := func(value string) string {
		return iniValueConfig(p, fmt.Sprintf(`  section = "server"
  key     = "port"
  value   = %q`, value))
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		// Destroy removes only the key; its comment and everything else stay.
		CheckDestroy: checkFileLineContent(p, strings.Replace(orig, "port = 80\n", "", 1)),
		Steps: []resource.TestStep{
			{
				Config: config("8080"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(p, strings.Replace(orig, "port = 80", "port = 8080", 1)),
					checkFileMode(p, 0o640),
					checkLOwnership(p, "65534", "65534"),
					checkNoTempFiles(dir),
					resource.TestCheckResourceAttr(testIniResource, "id", p+":server:port"),
					resource.TestCheckResourceAttr(testIniResource, "state", "present"),
					resource.TestCheckResourceAttr(testIniResource, "separator", " = "),
					resource.TestCheckResourceAttr(testIniResource, "create", "false"),
				),
			},
			{
				Config: config("8080"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				// Value changed outside Terraform: an in-place update shows
				// the actual value.
				PreConfig: setFile(t, p, strings.Replace(orig, "port = 80", "port=9090", 1)),
				Config:    config("8080"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testIniResource, plancheck.ResourceActionUpdate),
						expectBefore(testIniResource, "value", "9090"),
					},
				},
				Check: checkFileContent(p, strings.Replace(orig, "port = 80", "port = 8080", 1)),
			},
			{
				// Key removed outside Terraform: state becomes "absent".
				// It is re-added where a commented-out copy would be, or
				// else after the section's last key.
				PreConfig: setFile(t, p, strings.Replace(orig, "port = 80\n", "", 1)),
				Config:    config("8080"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testIniResource, plancheck.ResourceActionUpdate),
						expectBefore(testIniResource, "state", "absent"),
					},
				},
				Check: checkFileContent(p, strings.Replace(orig, "port = 80\nhost = 0.0.0.0\n", "host = 0.0.0.0\nport = 8080\n", 1)),
			},
			{
				// A duplicate added outside Terraform shows up as a
				// multi-line value and is removed.
				PreConfig: setFile(t, p, orig+"\n[server]\nport = 8080\n"),
				Config:    config("8080"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testIniResource, plancheck.ResourceActionUpdate),
						expectBefore(testIniResource, "value", "80\n8080"),
					},
				},
				Check: checkFileContent(p, strings.Replace(orig, "port = 80", "port = 8080", 1)+"\n[server]\n"),
			},
			{
				PreConfig: setFile(t, p, strings.Replace(orig, "port = 80", "port = 8080", 1)),
				Config:    config("8080"),
			},
			{
				ResourceName:      testIniResource,
				ImportState:       true,
				ImportStateId:     p + ":server:port",
				ImportStateVerify: true,
			},
		},
	})
}

// TestAccIniValue_createFileAndSection creates a systemd-style drop-in that
// does not exist yet, then adds a second key to the section it created.
func TestAccIniValue_createFileAndSection(t *testing.T) {
	requireRoot(t)

	p := filepath.Join(t.TempDir(), "app.service.d", "override.conf")
	config := `
resource "sysutils_ini_value" "restart" {
  path      = %[1]q
  section   = "Service"
  key       = "Restart"
  value     = "on-failure"
  separator = "="
  create    = true
}

resource "sysutils_ini_value" "user" {
  path      = %[1]q
  section   = "Service"
  key       = "User"
  value     = "app"
  separator = "="
  create    = true
  # Serialise the two creates so the key order in the file is known.
  depends_on = [sysutils_ini_value.restart]
}
`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		// Destroy leaves the file and the section header in place.
		CheckDestroy: checkFileLineContent(p, "[Service]\n"),
		Steps: []resource.TestStep{
			{
				// Without create, a missing file is an error.
				Config:      iniValueConfig(p, "  key = \"k\"\n  value = \"v\""),
				ExpectError: regexp.MustCompile(`does\s+not\s+exist.*\s+create\s+=\s+true`),
			},
			{
				Config: fmt.Sprintf(config, p),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(p, "[Service]\nRestart=on-failure\nUser=app\n"),
					checkFileMode(p, 0o644),
				),
			},
		},
	})
}

// TestAccIniValue_absent removes a key and detects it being added again.
func TestAccIniValue_absent(t *testing.T) {
	requireRoot(t)

	p := filepath.Join(t.TempDir(), "php.ini")
	const orig = "[PHP]\n; display_errors = On\ndisplay_errors = On\nmemory_limit = 128M\n"
	const want = "[PHP]\n; display_errors = On\nmemory_limit = 128M\n"
	mustWrite(t, p, orig)
	config := iniValueConfig(p, `  section = "PHP"
  key     = "display_errors"
  state   = "absent"`)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		// Destroy of an absent key changes nothing.
		CheckDestroy: checkFileLineContent(p, want),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(p, want),
					resource.TestCheckResourceAttr(testIniResource, "state", "absent"),
					resource.TestCheckNoResourceAttr(testIniResource, "value"),
				),
			},
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				PreConfig: setFile(t, p, orig),
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testIniResource, plancheck.ResourceActionUpdate),
						expectBefore(testIniResource, "state", "present"),
						expectBefore(testIniResource, "value", "On"),
					},
				},
				Check: checkFileContent(p, want),
			},
		},
	})
}

// TestAccIniValue_globalCRLFNoTrailingNewline manages a global key with a
// whitespace separator in a "\r\n" file without a final line break, and
// imports it with an empty section.
func TestAccIniValue_globalCRLFNoTrailingNewline(t *testing.T) {
	requireRoot(t)

	p := filepath.Join(t.TempDir(), "sshd_config")
	mustWrite(t, p, "Port 22\r\n#PermitRootLogin yes\r\nUsePAM yes")
	config := func(value string) string {
		return iniValueConfig(p, fmt.Sprintf(`  key       = "PermitRootLogin"
  value     = %q
  separator = " "`, value))
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkFileLineContent(p, "Port 22\r\n#PermitRootLogin yes\r\nUsePAM yes"),
		Steps: []resource.TestStep{
			{
				Config: config("no"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(p, "Port 22\r\n#PermitRootLogin yes\r\nPermitRootLogin no\r\nUsePAM yes"),
					resource.TestCheckResourceAttr(testIniResource, "id", p+"::PermitRootLogin"),
					resource.TestCheckResourceAttr(testIniResource, "section", ""),
				),
			},
			{
				Config: config("prohibit-password"),
				Check:  checkFileContent(p, "Port 22\r\n#PermitRootLogin yes\r\nPermitRootLogin prohibit-password\r\nUsePAM yes"),
			},
			{
				ResourceName:      testIniResource,
				ImportState:       true,
				ImportStateId:     p + "::PermitRootLogin",
				ImportStateVerify: true,
			},
		},
	})
}

// TestAccIniValue_parallelEdits applies many keys of one file in parallel;
// the provider's per-file lock must keep every edit.
func TestAccIniValue_parallelEdits(t *testing.T) {
	requireRoot(t)

	p := filepath.Join(t.TempDir(), "many.ini")
	mustWrite(t, p, "[a]\n")
	config := fmt.Sprintf(`
resource "sysutils_ini_value" "k" {
  count   = 20
  path    = %q
  section = count.index %% 2 == 0 ? "a" : "b"
  key     = "key${count.index}"
  value   = "v${count.index}"
}
`, p)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkFileLineContent(p, "[a]\n\n[b]\n"),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: func(*terraform.State) error {
					data, err := os.ReadFile(p)
					if err != nil {
						return err
					}
					for i := range 20 {
						if !strings.Contains(string(data), fmt.Sprintf("key%d = v%d\n", i, i)) {
							return fmt.Errorf("key%d missing from:\n%s", i, data)
						}
					}
					return nil
				},
			},
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}

// TestAccIniValue_rootDir confines the file to root_dir, and refuses both a
// symlink at path and a symlinked parent leading out of the root.
func TestAccIniValue_rootDir(t *testing.T) {
	requireRoot(t)

	root := testRootDir(t)
	outside := t.TempDir()
	mustMkdir(t, filepath.Join(root, "etc"))
	mustWrite(t, filepath.Join(root, "etc", "app.ini"), "[main]\n")
	mustWrite(t, filepath.Join(outside, "victim.ini"), "[main]\n")
	if err := os.Symlink(filepath.Join(outside, "victim.ini"), filepath.Join(root, "etc", "link.ini")); err != nil {
		t.Fatal(err)
	}
	// An absolute link inside the root resolves inside the root, so this
	// one leads above it only through "..".
	if err := os.Symlink("../../../../../../../.."+outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	resourceFor := func(p string) string {
		return rootedProvider(root) + iniValueConfig(p, `  section = "main"
  key     = "k"
  value   = "v"`)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkFileLineContent(filepath.Join(root, "etc", "app.ini"), "[main]\n"),
		Steps: []resource.TestStep{
			{
				Config:      resourceFor("/etc/link.ini"),
				ExpectError: regexp.MustCompile(`symbolic\s+link`),
			},
			{
				Config:      resourceFor("/escape/victim.ini"),
				ExpectError: regexp.MustCompile(`escapes\s+root_dir`),
			},
			{
				Config: resourceFor("/etc/app.ini"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(filepath.Join(root, "etc", "app.ini"), "[main]\nk = v\n"),
					checkFileContent(filepath.Join(outside, "victim.ini"), "[main]\n"),
					resource.TestCheckResourceAttr(testIniResource, "id", "/etc/app.ini:main:k"),
				),
			},
		},
	})
}

func TestAccIniValue_invalidConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.ini")
	for _, tc := range []struct {
		path  string // Defaults to p.
		attrs string
		want  string
	}{
		{"", `  key = "k"`, `"value"\s+is\s+required`},
		{"", "  key = \"k\"\n  value = \"v\"\n  state = \"absent\"", `must\s+not\s+be\s+set\s+when`},
		{"", "  key = \"k\"\n  value = \"v\"\n  state = \"gone\"", `value\s+must\s+be\s+one\s+of`},
		{"", "  key = \"a=b\"\n  value = \"v\"", `must\s+not\s+contain\s+the\s+separator`},
		{"", "  key = \"#k\"\n  value = \"v\"", `must\s+not\s+start\s+with`},
		{"", "  key = \"a b\"\n  value = \"v\"\n  separator = \" \"", `must\s+not\s+contain\s+whitespace`},
		{"", "  key = \"k\"\n  value = \" v\"", `(?i)start\s+or\s+end\s+with\s+whitespace`},
		{"", "  key = \"k\"\n  value = \"a\\nb\"", `line\s+breaks`},
		{"", "  key = \"k\"\n  value = \"v\"\n  section = \"[s]\"", `must\s+not\s+contain\s+'\['`},
		{"", "  key = \"k\"\n  value = \"v\"\n  separator = \"\"", `(?i)separator\s+must\s+not\s+be\s+empty`},
		{"relative", "  key = \"k\"\n  value = \"v\"", `absolute`},
	} {
		t.Run(tc.want, func(t *testing.T) {
			target := tc.path
			if target == "" {
				target = p
			}
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps: []resource.TestStep{{
					Config:      iniValueConfig(target, tc.attrs),
					PlanOnly:    true,
					ExpectError: regexp.MustCompile(tc.want),
				}},
			})
		})
	}
}

func TestAccIniValue_invalidImportID(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.ini")
	for _, tc := range []struct{ id, want string }{
		{p, `<path>:<section>:<key>`},
		{p + ":k", `<path>:<section>:<key>`},
		{"relative:s:k", `Invalid\s+path`},
		{p + ":s:", `(?i)key\s+must\s+not\s+be\s+empty`},
		{p + ":[s]:k", `must\s+not\s+contain\s+'\['`},
	} {
		t.Run(tc.id, func(t *testing.T) {
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps: []resource.TestStep{{
					Config:        iniValueConfig(p, "  key = \"k\"\n  value = \"v\""),
					ResourceName:  testIniResource,
					ImportState:   true,
					ImportStateId: tc.id,
					ExpectError:   regexp.MustCompile(tc.want),
				}},
			})
		})
	}
}
