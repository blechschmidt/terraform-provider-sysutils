package provider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/hashicorp/go-version"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"
)

// Ephemeral resources need Terraform 1.10 or OpenTofu 1.11. Tests that pass
// their values to write-only attributes use writeOnlyVersionChecks (1.11
// for both) instead.
var ephemeralVersionChecks = []tfversion.TerraformVersionCheck{
	tfversion.SkipBelow(tfversion.Version1_10_0),
	skipOpenTofuBelow{minimum: tfversion.Version1_11_0, feature: "ephemeral resources"},
}

// ephemeralWriteOnlyVersionChecks gate the tests that apply configurations
// with ephemeral resources. OpenTofu 1.13.0 fails to render a saved plan of
// such a configuration as JSON ("no schema found for ephemeral...") and
// plugin-testing does that after every apply; plan and apply themselves
// work. Add later releases here if they still have the bug.
var ephemeralWriteOnlyVersionChecks = append(slices.Clone(writeOnlyVersionChecks),
	skipOpenTofuVersions{versions: []string{"1.13.0"}, reason: "it cannot show saved plans with ephemeral resources as JSON"})

// skipOpenTofuVersions is a TerraformVersionCheck that skips the test on the
// given OpenTofu releases. The acceptance scripts allow its message as a skip
// reason.
type skipOpenTofuVersions struct {
	versions []string
	reason   string
}

func (s skipOpenTofuVersions) CheckTerraformVersion(_ context.Context, _ tfversion.CheckTerraformVersionRequest, resp *tfversion.CheckTerraformVersionResponse) {
	tofu, err := openTofuVersion()
	if err != nil {
		resp.Error = err
		return
	}
	if tofu == nil {
		return
	}
	for _, v := range s.versions {
		if tofu.Equal(version.Must(version.NewVersion(v))) {
			resp.Skip = fmt.Sprintf("OpenTofu %s is skipped: %s", tofu, s.reason)
			return
		}
	}
}

// writeSecretFile writes content to p with mode 0600, whatever the umask.
func writeSecretFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReadTrustedFile(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	writeSecretFile(t, secret, "s3cret\n")

	t.Run("ok", func(t *testing.T) {
		got, err := readTrustedFile(secret, 7)
		if err != nil || string(got) != "s3cret\n" {
			t.Fatalf("got %q, %v", got, err)
		}
	})
	t.Run("larger than limit", func(t *testing.T) {
		if _, err := readTrustedFile(secret, 6); err == nil || !strings.Contains(err.Error(), "larger than max_size") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("empty file with zero limit", func(t *testing.T) {
		empty := filepath.Join(dir, "empty")
		writeSecretFile(t, empty, "")
		if got, err := readTrustedFile(empty, 0); err != nil || len(got) != 0 {
			t.Fatalf("got %q, %v", got, err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		link := filepath.Join(dir, "link")
		if err := os.Symlink(secret, link); err != nil {
			t.Fatal(err)
		}
		if _, err := readTrustedFile(link, 100); err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Fatalf("got %v", err)
		}
	})
	for _, mode := range []os.FileMode{0o620, 0o602, 0o666} {
		t.Run(fmt.Sprintf("mode %04o", mode), func(t *testing.T) {
			p := filepath.Join(dir, fmt.Sprintf("w%o", mode))
			writeSecretFile(t, p, "x")
			if err := os.Chmod(p, mode); err != nil {
				t.Fatal(err)
			}
			if _, err := readTrustedFile(p, 100); err == nil || !strings.Contains(err.Error(), "writable by its group or other users") {
				t.Fatalf("got %v", err)
			}
		})
	}
	t.Run("directory", func(t *testing.T) {
		if _, err := readTrustedFile(dir, 100); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("fifo", func(t *testing.T) {
		fifo := filepath.Join(dir, "fifo")
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Fatal(err)
		}
		// Must fail rather than block waiting for a writer.
		if _, err := readTrustedFile(fifo, 100); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("missing", func(t *testing.T) {
		if _, err := readTrustedFile(filepath.Join(dir, "missing"), 100); err == nil || !os.IsNotExist(err) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("directory writable by others", func(t *testing.T) {
		open := filepath.Join(dir, "open")
		if err := os.Mkdir(open, 0o700); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(open, "secret")
		writeSecretFile(t, p, "x")
		if err := os.Chmod(open, 0o777); err != nil {
			t.Fatal(err)
		}
		if _, err := readTrustedFile(p, 100); err == nil || !strings.Contains(err.Error(), "writable by other users") {
			t.Fatalf("got %v", err)
		}
		// A sticky directory is fine for a file of the provider's user.
		if err := os.Chmod(open, 0o777|os.ModeSticky); err != nil {
			t.Fatal(err)
		}
		if _, err := readTrustedFile(p, 100); err != nil {
			t.Fatalf("sticky directory: %v", err)
		}
	})
	t.Run("parent is a symlink", func(t *testing.T) {
		// Symlinks above the file are followed, and what they lead to must
		// be trusted as well.
		real := filepath.Join(dir, "real")
		if err := os.Mkdir(real, 0o700); err != nil {
			t.Fatal(err)
		}
		writeSecretFile(t, filepath.Join(real, "secret"), "via link")
		if err := os.Symlink(real, filepath.Join(dir, "alias")); err != nil {
			t.Fatal(err)
		}
		if got, err := readTrustedFile(filepath.Join(dir, "alias", "secret"), 100); err != nil || string(got) != "via link" {
			t.Fatalf("got %q, %v", got, err)
		}
		if err := os.Chmod(real, 0o777); err != nil {
			t.Fatal(err)
		}
		if _, err := readTrustedFile(filepath.Join(dir, "alias", "secret"), 100); err == nil || !strings.Contains(err.Error(), "writable by other users") {
			t.Fatalf("got %v", err)
		}
	})
}

// TestReadTrustedFileOtherOwner needs root to create files and directories
// that belong to another user.
func TestReadTrustedFileOtherOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to chown")
	}
	const nobody = 65534
	dir := t.TempDir()

	foreign := filepath.Join(dir, "foreign")
	writeSecretFile(t, foreign, "x")
	if err := os.Chown(foreign, nobody, nobody); err != nil {
		t.Fatal(err)
	}
	if _, err := readTrustedFile(foreign, 100); err == nil || !strings.Contains(err.Error(), "belongs to user 65534") {
		t.Fatalf("file of another user: got %v", err)
	}

	// Another user's directory: they can replace the file in it.
	theirs := filepath.Join(dir, "theirs")
	if err := os.Mkdir(theirs, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(theirs, "secret")
	writeSecretFile(t, p, "x")
	if err := os.Chown(theirs, nobody, nobody); err != nil {
		t.Fatal(err)
	}
	if _, err := readTrustedFile(p, 100); err == nil || !strings.Contains(err.Error(), "belongs to user 65534") {
		t.Fatalf("directory of another user: got %v", err)
	}

	// In a sticky directory, an entry of another user on the way is refused.
	sticky := filepath.Join(dir, "sticky")
	if err := os.Mkdir(sticky, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sticky, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(sticky, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSecretFile(t, filepath.Join(sub, "secret"), "x")
	if err := os.Chown(sub, nobody, nobody); err != nil {
		t.Fatal(err)
	}
	if _, err := readTrustedFile(filepath.Join(sub, "secret"), 100); err == nil || !strings.Contains(err.Error(), "belongs to user 65534") {
		t.Fatalf("sticky directory entry of another user: got %v", err)
	}
}

// openEphemeral calls Open of e with a configuration in which the
// attributes in attrs are set and all others are null, and returns the
// response. It calls the framework types directly, so it needs no CLI.
func openEphemeral(t *testing.T, e ephemeral.EphemeralResource, attrs map[string]tftypes.Value) *ephemeral.OpenResponse {
	t.Helper()
	ctx := context.Background()
	var sresp ephemeral.SchemaResponse
	e.Schema(ctx, ephemeral.SchemaRequest{}, &sresp)
	objType, ok := sresp.Schema.Type().TerraformType(ctx).(tftypes.Object)
	if !ok {
		t.Fatalf("schema type is %T", sresp.Schema.Type().TerraformType(ctx))
	}
	vals := map[string]tftypes.Value{}
	for name, typ := range objType.AttributeTypes {
		vals[name] = tftypes.NewValue(typ, nil)
	}
	for name, v := range attrs {
		if _, ok := vals[name]; !ok {
			t.Fatalf("unknown attribute %q", name)
		}
		vals[name] = v
	}
	raw := tftypes.NewValue(objType, vals)
	resp := &ephemeral.OpenResponse{Result: tfsdk.EphemeralResultData{Schema: sresp.Schema, Raw: raw}}
	e.Open(ctx, ephemeral.OpenRequest{Config: tfsdk.Config{Schema: sresp.Schema, Raw: raw}}, resp)
	return resp
}

func strList(elems ...string) tftypes.Value {
	vals := make([]tftypes.Value, len(elems))
	for i, e := range elems {
		vals[i] = tftypes.NewValue(tftypes.String, e)
	}
	return tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, vals)
}

func diagText(resp *ephemeral.OpenResponse) string {
	var sb strings.Builder
	for _, d := range resp.Diagnostics {
		fmt.Fprintf(&sb, "%s: %s\n", d.Summary(), d.Detail())
	}
	return sb.String()
}

func TestEphemeralExecOpen(t *testing.T) {
	run := func(t *testing.T, attrs map[string]tftypes.Value) (execEphemeralModel, string) {
		t.Helper()
		resp := openEphemeral(t, NewExecEphemeralResource(), attrs)
		var m execEphemeralModel
		if !resp.Diagnostics.HasError() {
			resp.Diagnostics.Append(resp.Result.Get(context.Background(), &m)...)
		}
		return m, diagText(resp)
	}

	t.Run("stdout stderr and environment", func(t *testing.T) {
		m, diags := run(t, map[string]tftypes.Value{
			"command": strList("/bin/sh", "-c", `printf '%s' "$SECRET"; printf 'warn' >&2; cat`),
			"environment": tftypes.NewValue(tftypes.Map{ElementType: tftypes.String},
				map[string]tftypes.Value{"SECRET": tftypes.NewValue(tftypes.String, "tok-1")}),
			"stdin": tftypes.NewValue(tftypes.String, "+in"),
		})
		if diags != "" {
			t.Fatal(diags)
		}
		if m.Stdout.ValueString() != "tok-1+in" || m.Stderr.ValueString() != "warn" || m.ExitCode.ValueInt64() != 0 {
			t.Fatalf("got %+v", m)
		}
	})
	t.Run("inherit_parent_environment false", func(t *testing.T) {
		t.Setenv("SYSUTILS_EPHEMERAL_PARENT", "leaked")
		m, diags := run(t, map[string]tftypes.Value{
			"command":                    strList("/bin/sh", "-c", `printf '%s' "${SYSUTILS_EPHEMERAL_PARENT:-unset}"`),
			"inherit_parent_environment": tftypes.NewValue(tftypes.Bool, false),
		})
		if diags != "" || m.Stdout.ValueString() != "unset" {
			t.Fatalf("got %q, %s", m.Stdout.ValueString(), diags)
		}
	})
	t.Run("working_directory", func(t *testing.T) {
		dir := t.TempDir()
		m, diags := run(t, map[string]tftypes.Value{
			"command":           strList("/bin/sh", "-c", "pwd"),
			"working_directory": tftypes.NewValue(tftypes.String, dir),
		})
		if diags != "" || strings.TrimSpace(m.Stdout.ValueString()) != dir {
			t.Fatalf("got %q, %s", m.Stdout.ValueString(), diags)
		}
	})
	t.Run("non-zero exit withholds stdout", func(t *testing.T) {
		_, diags := run(t, map[string]tftypes.Value{
			"command": strList("/bin/sh", "-c", "printf 'stdout-secret-7f3a'; printf 'boom' >&2; exit 3"),
		})
		if !strings.Contains(diags, "non-zero status 3") || !strings.Contains(diags, "boom") || strings.Contains(diags, "stdout-secret-7f3a") {
			t.Fatalf("got %s", diags)
		}
	})
	t.Run("non-zero exit allowed", func(t *testing.T) {
		m, diags := run(t, map[string]tftypes.Value{
			"command":         strList("/bin/sh", "-c", "printf out; exit 7"),
			"fail_on_nonzero": tftypes.NewValue(tftypes.Bool, false),
		})
		if diags != "" || m.ExitCode.ValueInt64() != 7 || m.Stdout.ValueString() != "out" {
			t.Fatalf("got %+v, %s", m, diags)
		}
	})
	t.Run("output larger than max_output_bytes", func(t *testing.T) {
		for _, stream := range []string{"", ">&2"} {
			_, diags := run(t, map[string]tftypes.Value{
				"command":          strList("/bin/sh", "-c", "printf 'stdout-secret-7f3a'"+stream),
				"max_output_bytes": tftypes.NewValue(tftypes.Number, 8),
			})
			if !strings.Contains(diags, "Command output too large") || strings.Contains(diags, "stdout-secret-7f3a") {
				t.Fatalf("stream %q: got %s", stream, diags)
			}
		}
		m, diags := run(t, map[string]tftypes.Value{
			"command":          strList("/bin/sh", "-c", "printf 12345678"),
			"max_output_bytes": tftypes.NewValue(tftypes.Number, 8),
		})
		if diags != "" || m.Stdout.ValueString() != "12345678" {
			t.Fatalf("exactly max_output_bytes: got %q, %s", m.Stdout.ValueString(), diags)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		_, diags := run(t, map[string]tftypes.Value{
			"command": strList("/bin/sh", "-c", "sleep 30"),
			"timeout": tftypes.NewValue(tftypes.String, "200ms"),
		})
		if !strings.Contains(diags, "timed out after 200ms") {
			t.Fatalf("got %s", diags)
		}
	})
	t.Run("invalid UTF-8", func(t *testing.T) {
		m, diags := run(t, map[string]tftypes.Value{"command": strList("/bin/sh", "-c", `printf 'a\377b'`)})
		if diags != "" || m.Stdout.ValueString() != "a�b" {
			t.Fatalf("got %q, %s", m.Stdout.ValueString(), diags)
		}
	})
	t.Run("missing executable", func(t *testing.T) {
		_, diags := run(t, map[string]tftypes.Value{"command": strList("/nonexistent/sysutils-test")})
		if !strings.Contains(diags, "Command failed to run") {
			t.Fatalf("got %s", diags)
		}
	})
	t.Run("long stderr is cut in the diagnostic", func(t *testing.T) {
		_, diags := run(t, map[string]tftypes.Value{
			"command": strList("/bin/sh", "-c", "head -c 10000 /dev/zero | tr '\\0' e >&2; exit 1"),
		})
		if !strings.Contains(diags, "[...]") || strings.Count(diags, "e") > maxEphemeralStderrInDiag+200 {
			t.Fatalf("got %d bytes: %.200s", len(diags), diags)
		}
	})
}

func TestEphemeralFileOpen(t *testing.T) {
	dir := t.TempDir()
	text := filepath.Join(dir, "text")
	writeSecretFile(t, text, "pw\n")
	bin := filepath.Join(dir, "bin")
	writeSecretFile(t, bin, "\xff\x00")

	open := func(e *fileEphemeralResource, p string, maxSize any) (fileEphemeralModel, string) {
		t.Helper()
		attrs := map[string]tftypes.Value{"path": tftypes.NewValue(tftypes.String, p)}
		if maxSize != nil {
			attrs["max_size"] = tftypes.NewValue(tftypes.Number, maxSize)
		}
		resp := openEphemeral(t, e, attrs)
		var m fileEphemeralModel
		if !resp.Diagnostics.HasError() {
			resp.Diagnostics.Append(resp.Result.Get(context.Background(), &m)...)
		}
		return m, diagText(resp)
	}

	m, diags := open(&fileEphemeralResource{}, text, nil)
	if diags != "" || m.Content.ValueString() != "pw\n" || m.ContentBase64.ValueString() != "cHcK" ||
		m.SHA256.ValueString() != "3f0ba3cd87ca54b484ebe82e0c123deddddb37c2641d391c91487df6af1832e2" {
		t.Fatalf("text file: got %+v, %s", m, diags)
	}
	m, diags = open(&fileEphemeralResource{}, bin, nil)
	if diags != "" || !m.Content.IsNull() || m.ContentBase64.ValueString() != "/wA=" {
		t.Fatalf("binary file: got %+v, %s", m, diags)
	}
	if _, diags = open(&fileEphemeralResource{}, text, 2); !strings.Contains(diags, "larger than max_size (2 bytes)") {
		t.Fatalf("max_size: got %s", diags)
	}
	if _, diags = open(&fileEphemeralResource{}, text, maxEphemeralFileSize+1); !strings.Contains(diags, "Invalid max_size") {
		t.Fatalf("max_size above the cap: got %s", diags)
	}

	// With root_dir, the path is inside the root, and a symlink in the
	// tree cannot lead out of it.
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSecretFile(t, filepath.Join(root, "etc", "secret"), "rooted")
	rooted := &fileEphemeralResource{fsRoot: &fsRoot{dir: root}}
	if m, diags = open(rooted, "/etc/secret", nil); diags != "" || m.Content.ValueString() != "rooted" {
		t.Fatalf("root_dir: got %+v, %s", m, diags)
	}
	if err := os.Symlink("/../../../../../../"+strings.TrimPrefix(dir, "/"), filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, diags = open(rooted, "/escape/text", nil); !strings.Contains(diags, "escapes root_dir") {
		t.Fatalf("escaping symlink: got %s", diags)
	}
}

func TestAccEphemeralFile_contentWO(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "source.env"), filepath.Join(dir, "target.env")
	stateDir := harnessStateDir(t)
	const (
		secret1 = "TOKEN=eph-file-one-41c7\n"
		secret2 = "TOKEN=eph-file-two-8d02\n"
	)
	allSecrets := []string{"eph-file-one-41c7", "eph-file-two-8d02"}
	config := func(expr string, version int) string {
		return fmt.Sprintf(`
ephemeral "sysutils_file" "secret" {
  path = %q
}

resource "sysutils_file" "test" {
  path               = %q
  content_wo         = %s
  content_wo_version = %d
  mode               = "0600"
}
`, src, dst, expr, version)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		TerraformVersionChecks:   ephemeralWriteOnlyVersionChecks,
		CheckDestroy:             checkPathGone(dst),
		Steps: []resource.TestStep{
			{
				PreConfig: func() { writeSecretFile(t, src, secret1) },
				Config:    config("ephemeral.sysutils_file.secret.content", 1),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(dst, secret1),
					checkStateHasNoPlaintext(stateDir, secret1, allSecrets...),
				),
			},
			{
				// A new secret is written only with a new version.
				PreConfig:          func() { writeSecretFile(t, src, secret2) },
				Config:             config("ephemeral.sysutils_file.secret.content", 1),
				PlanOnly:           true,
				ExpectNonEmptyPlan: false,
			},
			{
				Config: config("base64decode(ephemeral.sysutils_file.secret.content_base64)", 2),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(dst, secret2),
					checkStateHasNoPlaintext(stateDir, secret2, allSecrets...),
				),
			},
		},
	})
}

func TestAccEphemeralFile_rootDir(t *testing.T) {
	root := t.TempDir()
	writeSecretFile(t, filepath.Join(root, "secret"), "rooted-eph-secret-5e1b")
	stateDir := harnessStateDir(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		TerraformVersionChecks:   ephemeralWriteOnlyVersionChecks,
		CheckDestroy:             checkPathGone(filepath.Join(root, "copy")),
		Steps: []resource.TestStep{{
			Config: fmt.Sprintf(`
provider "sysutils" {
  root_dir = %q
}

ephemeral "sysutils_file" "secret" {
  path = "/secret"
}

resource "sysutils_file" "test" {
  path               = "/copy"
  content_wo         = ephemeral.sysutils_file.secret.content
  content_wo_version = 1
}
`, root),
			Check: resource.ComposeAggregateTestCheckFunc(
				checkFileContent(filepath.Join(root, "copy"), "rooted-eph-secret-5e1b"),
				checkStateHasNoPlaintext(stateDir, "rooted-eph-secret-5e1b", "rooted-eph-secret-5e1b"),
			),
		}},
	})
}

func TestAccEphemeralExec_contentWO(t *testing.T) {
	dir := t.TempDir()
	dst, code := filepath.Join(dir, "token"), filepath.Join(dir, "code")
	stateDir := harnessStateDir(t)
	const secret = "eph-exec-secret-a93f"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		TerraformVersionChecks:   ephemeralWriteOnlyVersionChecks,
		CheckDestroy:             resource.ComposeAggregateTestCheckFunc(checkPathGone(dst), checkPathGone(code)),
		Steps: []resource.TestStep{{
			// The secret is passed in the environment and on stdin: neither
			// the configuration of an ephemeral resource nor its result
			// reaches the state.
			Config: fmt.Sprintf(`
ephemeral "sysutils_exec" "token" {
  command = ["/bin/sh", "-c", "printf '%%s-' \"$PART\"; cat"]
  environment = {
    PART = "eph-exec"
  }
  stdin = "secret-a93f"
}

ephemeral "sysutils_exec" "status" {
  command         = ["/bin/sh", "-c", "exit 7"]
  fail_on_nonzero = false
}

resource "sysutils_file" "token" {
  path               = %q
  content_wo         = ephemeral.sysutils_exec.token.stdout
  content_wo_version = 1
  mode               = "0600"
}

resource "sysutils_file" "code" {
  path               = %q
  content_wo         = tostring(ephemeral.sysutils_exec.status.exit_code)
  content_wo_version = 1
}
`, dst, code),
			Check: resource.ComposeAggregateTestCheckFunc(
				checkFileContent(dst, secret),
				checkFileContent(code, "7"),
				checkStateHasNoPlaintext(stateDir, secret, secret, "secret-a93f"),
			),
		}},
	})
}

func TestAccEphemeral_errors(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	writeSecretFile(t, secret, "0123456789")
	link := filepath.Join(dir, "link")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	groupWritable := filepath.Join(dir, "group-writable")
	writeSecretFile(t, groupWritable, "x")
	if err := os.Chmod(groupWritable, 0o660); err != nil {
		t.Fatal(err)
	}
	file := func(p, extra string) string {
		return fmt.Sprintf("ephemeral \"sysutils_file\" \"x\" {\n  path = %q\n%s}\n", p, extra)
	}
	exec := func(body string) string {
		return fmt.Sprintf("ephemeral \"sysutils_exec\" \"x\" {\n%s}\n", body)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		TerraformVersionChecks:   ephemeralVersionChecks,
		Steps: []resource.TestStep{
			{Config: file(link, ""), ExpectError: regexp.MustCompile(`is a symbolic link;\s+refusing to follow it`)},
			{Config: file(groupWritable, ""), ExpectError: regexp.MustCompile(`writable\s+by\s+its\s+group\s+or\s+other\s+users`)},
			{Config: file(secret, "  max_size = 4\n"), ExpectError: regexp.MustCompile(`larger\s+than\s+max_size\s+\(4\s+bytes\)`)},
			{Config: file(secret, "  max_size = 16777217\n"), ExpectError: regexp.MustCompile(`must\s+be\s+between\s+0\s+and\s+16777216`)},
			{Config: file("relative", ""), ExpectError: regexp.MustCompile(`absolute`)},
			{
				Config:      exec(`  command = ["/bin/sh", "-c", "echo withheld; echo boom >&2; exit 3"]` + "\n"),
				ExpectError: regexp.MustCompile(`(?s)non-zero status 3.*stderr:\s+boom`),
			},
			{
				Config:      exec(`  command = ["/bin/sh", "-c", "sleep 30"]` + "\n" + `  timeout = "1s"` + "\n"),
				ExpectError: regexp.MustCompile(`Command timed out after 1s`),
			},
			{
				Config:      exec(`  command = ["/bin/sh", "-c", "echo 123456789"]` + "\n  max_output_bytes = 4\n"),
				ExpectError: regexp.MustCompile(`Command output too large`),
			},
			{Config: exec("  command = []\n"), ExpectError: regexp.MustCompile(`at\s+least\s+1`)},
		},
	})
}
