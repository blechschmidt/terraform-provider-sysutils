package provider

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const testFileACLResource = "sysutils_file_acl.test"

// testACLDir returns a temporary directory on a file system with ACL
// support, skipping the test if there is none or setfacl/getfacl are
// missing (they are only used to set up and check).
func testACLDir(t *testing.T) string {
	t.Helper()
	dir := testRootDir(t)
	requireACLs(t, dir)
	return dir
}

// getfacl returns the ACL of p as "getfacl -cnp" prints it, one entry per
// line with numeric IDs, or the default ACL if def is set.
func getfacl(p string, def bool) (string, error) {
	args := []string{"-cnpaE"}
	if def {
		args = []string{"-cnpd"}
	}
	out, err := exec.Command("getfacl", append(args, p)...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("getfacl %s: %v: %s", p, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// checkACL checks the access ACL (or with def, the default ACL) of p against
// want, given in setfacl's short form with numeric IDs, such as
// "u::rw-,u:65534:r--,g::r--,m::r--,o::---". An empty want means no default
// ACL.
func checkACL(p string, def bool, want string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		got, err := getfacl(p, def)
		if err != nil {
			return err
		}
		expanded := strings.NewReplacer("u:", "user:", "g:", "group:", "m:", "mask:", "o:", "other:").Replace(want)
		if w := strings.ReplaceAll(expanded, ",", "\n"); got != w {
			return fmt.Errorf("ACL of %s (default: %v):\n%s\nwant:\n%s", p, def, got, w)
		}
		return nil
	}
}

func checkPermBits(p string, want os.FileMode) resource.TestCheckFunc {
	return func(*terraform.State) error {
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if got := info.Mode().Perm(); got != want {
			return fmt.Errorf("mode of %s = %04o, want %04o", p, got, want)
		}
		return nil
	}
}

func mustSetfacl(t *testing.T, args ...string) {
	t.Helper()
	runTool(t, "setfacl", args...)
}

func TestAccFileACL_file(t *testing.T) {
	dir := testACLDir(t)
	p := filepath.Join(dir, "shared.txt")
	mustWrite(t, p, "data\n")
	if err := os.Chmod(p, 0o640); err != nil {
		t.Fatal(err)
	}
	nobody := uidName(65534)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "sysutils_file_acl" "test" {
  path = %q
  entries = [
    { type = "user", name = %q, permissions = "r--" },
    { type = "group", name = "0", permissions = "rw-" },
  ]
}`, p, nobody),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testFileACLResource, "id", p),
					resource.TestCheckResourceAttr(testFileACLResource, "exclusive", "true"),
					resource.TestCheckResourceAttr(testFileACLResource, "entries.#", "2"),
					resource.TestCheckNoResourceAttr(testFileACLResource, "default_entries"),
					checkACL(p, false, "u::rw-,u:65534:r--,g::r--,g:0:rw-,m::rw-,o::---"),
					// The group bits of the mode hold the mask.
					checkPermBits(p, 0o660),
				),
			},
			{
				// The owner's entry, an explicit mask, and a changed
				// permission; the named group is no longer listed.
				Config: fmt.Sprintf(`
resource "sysutils_file_acl" "test" {
  path = %q
  entries = [
    { type = "user", permissions = "r--" },
    { type = "user", name = %q, permissions = "rwx" },
    { type = "mask", permissions = "r--" },
  ]
}`, p, nobody),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(testFileACLResource, plancheck.ResourceActionUpdate),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkACL(p, false, "u::r--,u:65534:rwx,g::r--,m::r--,o::---"),
					checkPermBits(p, 0o440),
				),
			},
			{
				ResourceName:      testFileACLResource,
				ImportState:       true,
				ImportStateId:     p,
				ImportStateVerify: true,
				// Import adopts the named entries and a mask that differs
				// from the computed one, but not the owner's entry, which
				// isn't an extended entry.
				ImportStateVerifyIgnore: []string{"entries"},
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					attrs := states[0].Attributes
					if attrs["entries.#"] != "2" {
						return fmt.Errorf("imported entries: %v", attrs)
					}
					return nil
				},
			},
			{
				// Drift: an entry added and a permission changed outside
				// Terraform are reverted.
				PreConfig: func() {
					mustSetfacl(t, "-m", "u:1:rwx,u:65534:---", p)
				},
				Config: fmt.Sprintf(`
resource "sysutils_file_acl" "test" {
  path = %q
  entries = [
    { type = "user", permissions = "r--" },
    { type = "user", name = %q, permissions = "rwx" },
    { type = "mask", permissions = "r--" },
  ]
}`, p, nobody),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(testFileACLResource, plancheck.ResourceActionUpdate),
				}},
				Check: checkACL(p, false, "u::r--,u:65534:rwx,g::r--,m::r--,o::---"),
			},
		},
		// Destroy removes the extended entries; the owner, group and other
		// permissions stay as they were.
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkACL(p, false, "u::r--,g::r--,o::---"),
			checkPermBits(p, 0o440),
		),
	})
}

func TestAccFileACL_maskDrift(t *testing.T) {
	dir := testACLDir(t)
	p := filepath.Join(dir, "f")
	mustWrite(t, p, "")
	config := fmt.Sprintf(`
resource "sysutils_file_acl" "test" {
  path    = %q
  entries = [{ type = "user", name = "65534", permissions = "rw-" }]
}`, p)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: config, Check: checkACL(p, false, "u::rw-,u:65534:rw-,g::r--,m::rw-,o::r--")},
			{
				// chmod g-w narrows the mask, and with it the named
				// user's effective permissions.
				PreConfig: func() {
					if err := os.Chmod(p, 0o644); err != nil {
						t.Fatal(err)
					}
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(testFileACLResource, plancheck.ResourceActionUpdate),
				}},
				Check: checkACL(p, false, "u::rw-,u:65534:rw-,g::r--,m::rw-,o::r--"),
			},
		},
	})
}

func TestAccFileACL_nonExclusive(t *testing.T) {
	dir := testACLDir(t)
	p := filepath.Join(dir, "f")
	mustWrite(t, p, "")
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	// An entry of another tool, which the resource must keep.
	mustSetfacl(t, "-m", "u:1:r-x", p)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "sysutils_file_acl" "test" {
  path      = %q
  exclusive = false
  entries = [
    { type = "user", name = "65534", permissions = "rw-" },
    { type = "group", name = "65534", permissions = "r--" },
  ]
}`, p),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testFileACLResource, "exclusive", "false"),
					checkACL(p, false, "u::rw-,u:1:r-x,u:65534:rw-,g::---,g:65534:r--,m::rwx,o::---"),
				),
			},
			{
				// Another entry added outside Terraform is no drift.
				PreConfig: func() { mustSetfacl(t, "-m", "g:2:--x", p) },
				Config: fmt.Sprintf(`
resource "sysutils_file_acl" "test" {
  path      = %q
  exclusive = false
  entries = [
    { type = "user", name = "65534", permissions = "rw-" },
  ]
}`, p),
				// The group entry is no longer listed, so it is removed.
				Check: checkACL(p, false, "u::rw-,u:1:r-x,u:65534:rw-,g::---,g:2:--x,m::rwx,o::---"),
			},
		},
		// Destroy removes only the resource's own entries.
		CheckDestroy: checkACL(p, false, "u::rw-,u:1:r-x,g::---,g:2:--x,m::r-x,o::---"),
	})
}

func TestAccFileACL_directoryDefaults(t *testing.T) {
	dir := testACLDir(t)
	d := filepath.Join(dir, "shared")
	if err := os.Mkdir(d, 0o750); err != nil {
		t.Fatal(err)
	}
	// Names as import records them, so that ImportStateVerify can compare
	// entries.
	nobody, nogroup := uidName(65534), gidName(65534)
	withDefaults := fmt.Sprintf(`
resource "sysutils_file_acl" "test" {
  path = %q
  entries = [
    { type = "group", name = %[2]q, permissions = "r-x" },
  ]
  default_entries = [
    { type = "group", name = %[2]q, permissions = "r-x" },
    { type = "user", name = %[3]q, permissions = "rwx" },
    { type = "other", permissions = "---" },
  ]
}`, d, nogroup, nobody)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: withDefaults,
				Check: resource.ComposeAggregateTestCheckFunc(
					checkACL(d, false, "u::rwx,g::r-x,g:65534:r-x,m::r-x,o::---"),
					// Base entries come from the access ACL.
					checkACL(d, true, "u::rwx,u:65534:rwx,g::r-x,g:65534:r-x,m::rwx,o::---"),
					func(*terraform.State) error {
						// New files inherit the default ACL.
						f := filepath.Join(d, "new")
						if err := os.WriteFile(f, nil, 0o666); err != nil {
							return err
						}
						defer func() { _ = os.Remove(f) }()
						got, err := getfacl(f, false)
						if err != nil {
							return err
						}
						if !strings.Contains(got, "user:65534:rw") {
							return fmt.Errorf("new file did not inherit the default ACL:\n%s", got)
						}
						return nil
					},
				),
			},
			{
				ResourceName:      testFileACLResource,
				ImportState:       true,
				ImportStateId:     d,
				ImportStateVerify: true,
				// The other entry is a base entry, which import doesn't
				// adopt.
				ImportStateVerifyIgnore: []string{"default_entries"},
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					attrs := states[0].Attributes
					if attrs["default_entries.#"] != "2" || attrs["entries.#"] != "1" {
						return fmt.Errorf("imported entries: %v", attrs)
					}
					return nil
				},
			},
			{
				// The default ACL is removed outside Terraform.
				PreConfig: func() { mustSetfacl(t, "-k", d) },
				Config:    withDefaults,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(testFileACLResource, plancheck.ResourceActionUpdate),
				}},
				Check: checkACL(d, true, "u::rwx,u:65534:rwx,g::r-x,g:65534:r-x,m::rwx,o::---"),
			},
			{
				// No longer managing the default ACL removes it.
				Config: fmt.Sprintf(`
resource "sysutils_file_acl" "test" {
  path = %q
  entries = [
    { type = "group", name = %q, permissions = "r-x" },
  ]
}`, d, nogroup),
				Check: checkACL(d, true, ""),
			},
			{
				Config: withDefaults,
				Check:  checkACL(d, true, "u::rwx,u:65534:rwx,g::r-x,g:65534:r-x,m::rwx,o::---"),
			},
		},
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkACL(d, false, "u::rwx,g::r-x,o::---"),
			checkACL(d, true, ""),
			checkPermBits(d, 0o750),
		),
	})
}

func TestAccFileACL_importFile(t *testing.T) {
	dir := testACLDir(t)
	p := filepath.Join(dir, "f")
	mustWrite(t, p, "")
	mustSetfacl(t, "--set", "u::rw-,u:65534:r--,g::r--,g:0:rw-,m::rw-,o::---", p)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "sysutils_file_acl" "test" {
  path = %q
  entries = [
    { type = "user", name = %q, permissions = "r--" },
    { type = "group", name = %q, permissions = "rw-" },
  ]
}`, p, uidName(65534), gidName(0)),
				ImportState:        true,
				ImportStateId:      p,
				ImportStatePersist: true,
				ResourceName:       testFileACLResource,
			},
			{
				// The imported state matches the configuration.
				Config: fmt.Sprintf(`
resource "sysutils_file_acl" "test" {
  path = %q
  entries = [
    { type = "user", name = %q, permissions = "r--" },
    { type = "group", name = %q, permissions = "rw-" },
  ]
}`, p, uidName(65534), gidName(0)),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectEmptyPlan(),
				}},
			},
		},
	})
}

func TestAccFileACL_pathRemoved(t *testing.T) {
	dir := testACLDir(t)
	p := filepath.Join(dir, "f")
	mustWrite(t, p, "")
	config := fmt.Sprintf(`
resource "sysutils_file_acl" "test" {
  path    = %q
  entries = [{ type = "user", name = "65534", permissions = "r--" }]
}`, p)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: config},
			{
				// A deleted file removes the resource from the state; it
				// is created again once the file is back.
				PreConfig: func() {
					if err := os.Remove(p); err != nil {
						t.Fatal(err)
					}
				},
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				PreConfig: func() { mustWrite(t, p, "") },
				Config:    config,
				Check:     checkACL(p, false, "u::rw-,u:65534:r--,g::r--,m::r--,o::r--"),
			},
		},
	})
}

func TestAccFileACL_rootDir(t *testing.T) {
	root := testACLDir(t)
	p := filepath.Join(root, "srv", "data")
	mustWrite(t, p, "")
	// A symlink inside the tree is resolved inside it.
	mustSymlink(t, "/srv", filepath.Join(root, "link"))
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: fmt.Sprintf(`
provider "sysutils" {
  root_dir = %q
}

resource "sysutils_file_acl" "test" {
  path    = "/link/data"
  entries = [{ type = "group", name = "65534", permissions = "rw-" }]
}`, root),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(testFileACLResource, "path", "/link/data"),
				checkACL(p, false, "u::rw-,g::r--,g:65534:rw-,m::rw-,o::r--"),
			),
		}},
		CheckDestroy: checkACL(p, false, "u::rw-,g::r--,o::r--"),
	})
}

func TestAccFileACL_errors(t *testing.T) {
	dir := testACLDir(t)
	file := filepath.Join(dir, "file")
	mustWrite(t, file, "")
	link := filepath.Join(dir, "link")
	mustSymlink(t, file, link)
	cfg := func(p, entries string) string {
		return fmt.Sprintf(`
resource "sysutils_file_acl" "test" {
  path    = %q
  %s
}`, p, entries)
	}
	const named = `entries = [{ type = "user", name = "65534", permissions = "r--" }]`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      cfg(link, named),
				ExpectError: regexp.MustCompile(`is\s+a\s+symbolic\s+link;\s+refusing\s+to\s+follow\s+it`),
			},
			{
				Config:      cfg(filepath.Join(dir, "missing"), named),
				ExpectError: regexp.MustCompile(`no\s+such\s+file\s+or\s+directory`),
			},
			{
				Config:      cfg(file, named+"\n  default_entries = []"),
				ExpectError: regexp.MustCompile(`only\s+directories\s+have\s+a\s+default\s+ACL`),
			},
			{
				Config:      cfg(file, `entries = [{ type = "mask", name = "x", permissions = "r--" }]`),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`A\s+"mask"\s+entry\s+has\s+no\s+name`),
			},
			{
				Config:      cfg(file, `entries = [{ type = "user", permissions = "rw" }]`),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`must\s+be\s+three\s+characters`),
			},
			{
				Config: cfg(file, `entries = [
    { type = "user", name = "65534", permissions = "r--" },
    { type = "user", name = "65534", permissions = "rw-" },
  ]`),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`listed\s+more\s+than\s+once`),
			},
			{
				Config: cfg(file, fmt.Sprintf(`entries = [
    { type = "group", name = %q, permissions = "r--" },
    { type = "group", name = "0", permissions = "rw-" },
  ]`, gidName(0))),
				ExpectError: regexp.MustCompile(`both\s+refer\s+to`),
			},
			{
				Config:      cfg(file, `entries = [{ type = "user", name = "no-such-user-sysutils", permissions = "r--" }]`),
				ExpectError: regexp.MustCompile(`Looking\s+up\s+user`),
			},
			{
				Config:      cfg("relative/path", named),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`must\s+be\s+absolute`),
			},
		},
	})
	// Nothing was changed.
	if err := checkACL(file, false, "u::rw-,g::r--,o::r--")(nil); err != nil {
		t.Error(err)
	}
}
