package provider

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// testRootDir returns a fresh temporary directory for root_dir, with
// symlinks in its own path resolved so host paths compare literally.
func testRootDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func rootedProvider(root string) string {
	return fmt.Sprintf("provider \"sysutils\" {\n  root_dir = %q\n}\n", root)
}

func checkNotExist(p string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			return fmt.Errorf("%s exists (err=%v); expected nothing to be created there", p, err)
		}
		return nil
	}
}

// TestAccRootDir_tree manages a small root filesystem tree under a temporary
// root_dir with every rooted resource and data source, and checks that
// everything lands inside the root, that absolute symlinks inside the tree
// are resolved relative to it, and that state keeps the in-root paths.
func TestAccRootDir_tree(t *testing.T) {
	requireRoot(t)
	root := testRootDir(t)
	// Paths that must not be touched on the host. The test is meaningless
	// if they happen to exist, so pick names that cannot.
	hostApp := "/opt/sysutils-rootdir-test-" + filepath.Base(root)
	if _, err := os.Lstat(hostApp); !os.IsNotExist(err) {
		t.Fatalf("%s unexpectedly exists on the host", hostApp)
	}
	app := "/opt/" + filepath.Base(hostApp)

	mustMkdir(t, filepath.Join(root, "etc"))
	if err := os.WriteFile(filepath.Join(root, "etc", "hosts"), []byte("127.0.0.1 localhost\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	config := rootedProvider(root) + fmt.Sprintf(`
resource "sysutils_directory" "app" {
  path = %[1]q
  mode = "0750"
}

resource "sysutils_file" "config" {
  path    = "${sysutils_directory.app.path}/config"
  content = "a=1\n"
}

resource "sysutils_template_file" "conf" {
  path     = "${sysutils_directory.app.path}/app.conf"
  template = "port={{ .port }}\n"
  vars     = { port = 80 }
}

resource "sysutils_file_line" "hosts" {
  path = "/etc/hosts"
  line = "10.0.0.1 app"
}

resource "sysutils_symlink" "current" {
  path   = "/srv/current"
  target = sysutils_directory.app.path
}

# Absolute link target: resolved inside root_dir, not on the host.
data "sysutils_file" "via_link" {
  path       = "/srv/current/config"
  depends_on = [sysutils_file.config, sysutils_symlink.current]
}

resource "sysutils_symlink" "config" {
  path   = "/srv/config-link"
  target = "../${sysutils_file.config.path}"
}

data "sysutils_file" "follow" {
  path            = "/srv/config-link"
  follow_symlinks = true
  depends_on      = [sysutils_file.config, sysutils_symlink.config]
}

data "sysutils_directory" "app" {
  path       = "/srv/current"
  depends_on = [sysutils_file.config, sysutils_template_file.conf, sysutils_symlink.current]
}
`, app)

	hostPath := func(p string) string { return filepath.Join(root, p) }

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkNotExist(hostPath(app)),
			checkNotExist(hostPath("/srv/current")),
			checkNotExist(hostPath("/srv/config-link")),
			checkFileContent(hostPath("/etc/hosts"), "127.0.0.1 localhost\n"),
		),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileMode(hostPath(app), 0o750),
					checkFileContent(hostPath(app+"/config"), "a=1\n"),
					checkFileContent(hostPath(app+"/app.conf"), "port=80\n"),
					checkFileContent(hostPath("/etc/hosts"), "127.0.0.1 localhost\n10.0.0.1 app\n"),
					checkSymlinkTarget(hostPath("/srv/current"), app),
					checkNotExist(hostApp),
					// State and ids keep the paths inside the root.
					resource.TestCheckResourceAttr("sysutils_directory.app", "path", app),
					resource.TestCheckResourceAttr("sysutils_directory.app", "id", app),
					resource.TestCheckResourceAttr("sysutils_file.config", "id", app+"/config"),
					resource.TestCheckResourceAttr("sysutils_file_line.hosts", "id", "/etc/hosts:line"),
					resource.TestCheckResourceAttr("data.sysutils_file.via_link", "content", "a=1\n"),
					resource.TestCheckResourceAttr("data.sysutils_file.via_link", "path", "/srv/current/config"),
					resource.TestCheckResourceAttr("data.sysutils_file.follow", "content", "a=1\n"),
					resource.TestCheckResourceAttr("data.sysutils_file.follow", "id", "/srv/config-link"),
					resource.TestCheckResourceAttr("data.sysutils_directory.app", "exists", "true"),
					resource.TestCheckResourceAttr("data.sysutils_directory.app", "mode", "0750"),
					resource.TestCheckResourceAttr("data.sysutils_directory.app", "entries.#", "2"),
					resource.TestCheckResourceAttr("data.sysutils_directory.app", "entries.0", "app.conf"),
					resource.TestCheckResourceAttr("data.sysutils_directory.app", "entries.1", "config"),
				),
			},
			{
				// follow_symlinks on a link to a directory finds the
				// directory inside the root; the host has none.
				Config: config + `
data "sysutils_file" "dir" {
  path            = "/srv/current"
  follow_symlinks = true
}`,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`is\s+a\s+directory`),
			},
			{
				// Drift inside the root is detected and shown as a diff.
				PreConfig: func() {
					if err := os.WriteFile(hostPath(app+"/config"), []byte("a=2\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("sysutils_file.config", plancheck.ResourceActionUpdate),
						expectBefore("sysutils_file.config", "content", "a=2\n"),
					},
				},
			},
			{
				ResourceName:      "sysutils_file.config",
				ImportState:       true,
				ImportStateId:     app + "/config",
				ImportStateVerify: true,
				Config:            config,
			},
		},
	})
}

// TestAccRootDir_escapeAttempts plants symlinks in the root that point out
// of it and checks that no resource or data source acts outside the root.
func TestAccRootDir_escapeAttempts(t *testing.T) {
	requireRoot(t)
	root := testRootDir(t)
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("host secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	up := strings.Repeat("../", strings.Count(root, "/")+1)
	// Relative link climbing out of the root to the outside directory.
	mustSymlink(t, up+strings.TrimPrefix(outside, "/"), filepath.Join(root, "escape"))
	// Absolute link to the outside directory: means <root>/<outside> here.
	mustSymlink(t, outside, filepath.Join(root, "abs"))
	// Absolute link to the secret: means <root>/<outside>/secret here.
	mustSymlink(t, filepath.Join(outside, "secret"), filepath.Join(root, "secretlink"))

	outsideUntouched := func(*terraform.State) error {
		entries, err := os.ReadDir(outside)
		if err != nil {
			return err
		}
		if len(entries) != 1 || entries[0].Name() != "secret" {
			return fmt.Errorf("directory outside root_dir was modified: %v", entries)
		}
		return checkFileContent(filepath.Join(outside, "secret"), "host secret\n")(nil)
	}
	provider := rootedProvider(root)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             outsideUntouched,
		Steps: []resource.TestStep{
			{
				Config: provider + `
resource "sysutils_file" "escape" {
  path    = "/escape/pwned"
  content = "x"
}`,
				ExpectError: regexp.MustCompile(`escapes\s+root_dir`),
			},
			{
				Config: provider + `
resource "sysutils_directory" "escape" {
  path = "/escape/pwned"
}`,
				ExpectError: regexp.MustCompile(`escapes\s+root_dir`),
			},
			{
				Config: provider + `
resource "sysutils_file_line" "escape" {
  path   = "/escape/secret"
  line   = "pwned"
  create = true
}`,
				ExpectError: regexp.MustCompile(`escapes\s+root_dir`),
			},
			{
				Config: provider + `
resource "sysutils_template_file" "escape" {
  path     = "/escape/pwned"
  template = "x"
}`,
				ExpectError: regexp.MustCompile(`escapes\s+root_dir`),
			},
			{
				Config: provider + `
resource "sysutils_symlink" "escape" {
  path   = "/escape/pwned"
  target = "/x"
}`,
				ExpectError: regexp.MustCompile(`escapes\s+root_dir`),
			},
			{
				// A new link whose own target leads out of the root is
				// refused at plan time.
				Config: provider + `
resource "sysutils_symlink" "out" {
  path   = "/lib/out"
  target = "../../etc/shadow"
}`,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`escapes\s+root_dir`),
			},
			{
				Config: provider + `
data "sysutils_file" "escape" {
  path = "/escape/secret"
}`,
				ExpectError: regexp.MustCompile(`escapes\s+root_dir`),
			},
			{
				Config: provider + `
data "sysutils_directory" "escape" {
  path = "/escape"
}`,
				ExpectError: regexp.MustCompile(`escapes\s+root_dir`),
			},
			{
				// Absolute links are resolved inside the root, so the host's
				// secret is not found rather than read.
				Config: provider + `
data "sysutils_file" "abs" {
  path            = "/secretlink"
  follow_symlinks = true
}`,
				ExpectError: regexp.MustCompile(`symbolic\s+link\s+whose\s+target\s+does\s+not\s+exist`),
			},
			{
				// A file below an absolute link is written inside the root.
				Config: provider + `
resource "sysutils_file" "abs" {
  path    = "/abs/inside"
  content = "inside\n"
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(filepath.Join(root, outside, "inside"), "inside\n"),
					outsideUntouched,
				),
			},
		},
	})
}

// TestAccRootDir_invalid checks the validation of root_dir itself.
func TestAccRootDir_invalid(t *testing.T) {
	root := testRootDir(t)
	missing := filepath.Join(root, "missing")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
provider "sysutils" {
  root_dir = "relative/dir"
}
data "sysutils_directory" "x" {
  path = "/"
}`,
				ExpectError: regexp.MustCompile(`must\s+be\s+absolute`),
			},
			{
				Config: `
provider "sysutils" {
  root_dir = "/srv/rootfs/"
}
data "sysutils_directory" "x" {
  path = "/"
}`,
				ExpectError: regexp.MustCompile(`canonical\s+form`),
			},
			{
				Config: rootedProvider(missing) + `
data "sysutils_directory" "x" {
  path = "/etc"
}`,
				ExpectError: regexp.MustCompile(`(?s)use\s+".*missing"\s+as\s+root_dir:.*no\s+such\s+file`),
			},
			{
				// "/" is the default and behaves like the host.
				Config: rootedProvider("/") + fmt.Sprintf(`
data "sysutils_directory" "x" {
  path = %q
}`, root),
				Check: resource.TestCheckResourceAttr("data.sysutils_directory.x", "exists", "true"),
			},
		},
	})
}

// expectBeforeValue is a plan check that the prior value of a top-level
// string attribute, as shown by the plan, is want. It verifies that plans
// show the actual content found on disk.
type expectBeforeValue struct {
	address, attr, want string
}

func expectBefore(address, attr, want string) plancheck.PlanCheck {
	return expectBeforeValue{address: address, attr: attr, want: want}
}

func (e expectBeforeValue) CheckPlan(_ context.Context, req plancheck.CheckPlanRequest, resp *plancheck.CheckPlanResponse) {
	for _, rc := range req.Plan.ResourceChanges {
		if rc.Address != e.address {
			continue
		}
		before, ok := rc.Change.Before.(map[string]any)
		if !ok {
			resp.Error = fmt.Errorf("%s: no prior state in plan", e.address)
			return
		}
		if got := before[e.attr]; got != e.want {
			resp.Error = fmt.Errorf("%s: planned prior value of %s is %#v, want %#v", e.address, e.attr, got, e.want)
		}
		return
	}
	resp.Error = fmt.Errorf("%s: not in plan", e.address)
}

// expectSensitive is a plan check that attr is marked sensitive before (if
// there is a prior state) and after the change, so the plan does not show
// its value.
type expectSensitiveValue struct {
	address, attr string
}

func expectSensitive(address, attr string) plancheck.PlanCheck {
	return expectSensitiveValue{address: address, attr: attr}
}

func (e expectSensitiveValue) CheckPlan(_ context.Context, req plancheck.CheckPlanRequest, resp *plancheck.CheckPlanResponse) {
	for _, rc := range req.Plan.ResourceChanges {
		if rc.Address != e.address {
			continue
		}
		for name, v := range map[string]any{"before": rc.Change.BeforeSensitive, "after": rc.Change.AfterSensitive} {
			if name == "before" && rc.Change.Before == nil {
				continue // Create.
			}
			m, _ := v.(map[string]any)
			if m[e.attr] != true {
				resp.Error = fmt.Errorf("%s: %s is not marked sensitive %s the change (%#v)", e.address, e.attr, name, v)
				return
			}
		}
		return
	}
	resp.Error = fmt.Errorf("%s: not in plan", e.address)
}

// TestAccContentDiff_file checks that drift of sysutils_file content shows
// up in the plan as the actual content for content and content_base64, and
// only as checksums for sensitive_content.
func TestAccContentDiff_file(t *testing.T) {
	requireRoot(t)
	dir := t.TempDir()
	text, bin, secret := filepath.Join(dir, "text"), filepath.Join(dir, "bin"), filepath.Join(dir, "secret")
	want := []byte{0, 1, 2, 0xff}
	drifted := []byte{0, 1, 2, 0xfe}

	config := fmt.Sprintf(`
resource "sysutils_file" "text" {
  path    = %q
  content = "wanted\n"
}
resource "sysutils_file" "bin" {
  path           = %q
  content_base64 = %q
}
resource "sysutils_file" "secret" {
  path              = %q
  sensitive_content = "password=hunter2\n"
  mode              = "0600"
}`, text, bin, base64.StdEncoding.EncodeToString(want), secret)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						expectSensitive("sysutils_file.secret", "sensitive_content"),
						plancheck.ExpectKnownValue("sysutils_file.secret", tfjsonpath.New("content_sha256"),
							knownvalue.StringExact(sha256Hex([]byte("password=hunter2\n")))),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(secret, "password=hunter2\n"),
					checkFileMode(secret, 0o600),
				),
			},
			{
				PreConfig: func() {
					for p, data := range map[string][]byte{
						text:   []byte("edited\n"),
						bin:    drifted,
						secret: []byte("password=changed\n"),
					} {
						if err := os.WriteFile(p, data, 0o600); err != nil {
							t.Fatal(err)
						}
					}
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("sysutils_file.text", plancheck.ResourceActionUpdate),
						expectBefore("sysutils_file.text", "content", "edited\n"),
						plancheck.ExpectResourceAction("sysutils_file.bin", plancheck.ResourceActionUpdate),
						expectBefore("sysutils_file.bin", "content_base64", base64.StdEncoding.EncodeToString(drifted)),
						plancheck.ExpectResourceAction("sysutils_file.secret", plancheck.ResourceActionUpdate),
						expectSensitive("sysutils_file.secret", "sensitive_content"),
						expectBefore("sysutils_file.secret", "content_sha256", sha256Hex([]byte("password=changed\n"))),
						plancheck.ExpectKnownValue("sysutils_file.secret", tfjsonpath.New("content_sha256"),
							knownvalue.StringExact(sha256Hex([]byte("password=hunter2\n")))),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(text, "wanted\n"),
					checkFileContent(bin, string(want)),
					checkFileContent(secret, "password=hunter2\n"),
				),
			},
			{
				// A non-canonical but equivalent base64 value must not cause
				// a perpetual diff: after this apply, the refreshed plan
				// (checked by the test framework) must be empty.
				Config: strings.Replace(config, base64.StdEncoding.EncodeToString(want), "AAEC\\n/w==", 1),
				Check:  checkFileContent(bin, string(want)),
			},
		},
	})
}

// TestAccContentDiff_fileLine checks content_sha256 of sysutils_file_line,
// which keeps changes visible when the line itself is sensitive.
func TestAccContentDiff_fileLine(t *testing.T) {
	requireRoot(t)
	target := filepath.Join(t.TempDir(), "sshd_config")
	if err := os.WriteFile(target, []byte("Port 22\nPasswordAuthentication yes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(`
variable "line" {
  type      = string
  sensitive = true
  default   = "PasswordAuthentication no"
}
resource "sysutils_file_line" "test" {
  path   = %q
  line   = var.line
  regexp = "^PasswordAuthentication "
}
resource "sysutils_file_line" "block" {
  path  = %q
  block = "a\nb\n"
}`, target, target)
	lineSum := sha256Hex([]byte("PasswordAuthentication no\n"))
	blockSum := sha256Hex([]byte("# BEGIN MANAGED BY TERRAFORM\na\nb\n# END MANAGED BY TERRAFORM\n"))

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectKnownValue("sysutils_file_line.test", tfjsonpath.New("content_sha256"), knownvalue.StringExact(lineSum)),
						plancheck.ExpectKnownValue("sysutils_file_line.block", tfjsonpath.New("content_sha256"), knownvalue.StringExact(blockSum)),
					},
				},
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue("sysutils_file_line.test", tfjsonpath.New("content_sha256"), knownvalue.StringExact(lineSum)),
					statecheck.ExpectKnownValue("sysutils_file_line.block", tfjsonpath.New("content_sha256"), knownvalue.StringExact(blockSum)),
				},
			},
			{
				PreConfig: func() {
					data := "Port 22\nPasswordAuthentication yes\n# BEGIN MANAGED BY TERRAFORM\na\nc\n# END MANAGED BY TERRAFORM\n"
					if err := os.WriteFile(target, []byte(data), 0o644); err != nil {
						t.Fatal(err)
					}
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("sysutils_file_line.test", plancheck.ResourceActionUpdate),
						expectSensitive("sysutils_file_line.test", "line"),
						expectBefore("sysutils_file_line.test", "content_sha256", sha256Hex([]byte("PasswordAuthentication yes\n"))),
						plancheck.ExpectResourceAction("sysutils_file_line.block", plancheck.ResourceActionUpdate),
						expectBefore("sysutils_file_line.block", "block", "a\nc\n"),
						expectBefore("sysutils_file_line.block", "content_sha256",
							sha256Hex([]byte("# BEGIN MANAGED BY TERRAFORM\na\nc\n# END MANAGED BY TERRAFORM\n"))),
					},
				},
				Check: checkFileContent(target, "Port 22\nPasswordAuthentication no\n# BEGIN MANAGED BY TERRAFORM\na\nb\n# END MANAGED BY TERRAFORM\n"),
			},
		},
	})
}
