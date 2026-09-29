package provider

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const testDirResource = "sysutils_directory.test"

func TestAccDirectory_createDefaultsUpdateModeInPlace(t *testing.T) {
	requireRoot(t)

	// A restrictive umask must not leak into the resulting mode.
	oldUmask := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(oldUmask) })

	parent := t.TempDir()
	dir := filepath.Join(parent, "data")
	var inode uint64

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkDirectoryDestroyed,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "sysutils_directory" "test" {
  path = %q
}`, dir),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testDirResource, "path", dir),
					resource.TestCheckResourceAttr(testDirResource, "id", dir),
					resource.TestCheckResourceAttr(testDirResource, "mode", "0755"),
					resource.TestCheckResourceAttr(testDirResource, "owner", "root"),
					resource.TestCheckResourceAttr(testDirResource, "group", "root"),
					resource.TestCheckResourceAttr(testDirResource, "create_parents", "true"),
					resource.TestCheckResourceAttr(testDirResource, "force_destroy", "false"),
					checkIsDir(dir),
					checkFileMode(dir, 0o755),
					recordInode(dir, &inode),
				),
			},
			{
				Config: fmt.Sprintf(`
resource "sysutils_directory" "test" {
  path = %q
  mode = "0700"
}`, dir),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testDirResource, plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testDirResource, "mode", "0700"),
					checkFileMode(dir, 0o700),
					checkSameInode(dir, &inode),
				),
			},
			{
				// Special bits round-trip through state and disk.
				Config: fmt.Sprintf(`
resource "sysutils_directory" "test" {
  path = %q
  mode = "1777"
}`, dir),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testDirResource, "mode", "1777"),
					checkFileMode(dir, 0o777),
					checkModeBits(dir, fs.ModeSticky),
					checkSameInode(dir, &inode),
				),
			},
		},
	})

	// Destroying the leaf must leave the (test-owned) parent in place.
	if _, err := os.Stat(parent); err != nil {
		t.Fatalf("parent directory should survive destroy: %v", err)
	}
}

func TestAccDirectory_ownership(t *testing.T) {
	requireRoot(t)

	nobody, err := user.Lookup("nobody")
	if err != nil {
		t.Skipf("nobody user not available: %v", err)
	}
	nogroup, err := user.LookupGroup("nogroup")
	if err != nil {
		t.Skipf("nogroup group not available: %v", err)
	}

	dir := filepath.Join(t.TempDir(), "owned")
	config := func(owner, group string) string {
		return fmt.Sprintf(`
resource "sysutils_directory" "test" {
  path  = %q
  owner = %q
  group = %q
}`, dir, owner, group)
	}
	var inode uint64

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkDirectoryDestroyed,
		Steps: []resource.TestStep{
			{
				Config: config("root", "root"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileUID(dir, "0"),
					checkFileGID(dir, "0"),
					recordInode(dir, &inode),
				),
			},
			{
				Config: config("nobody", "nogroup"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testDirResource, plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testDirResource, "owner", "nobody"),
					resource.TestCheckResourceAttr(testDirResource, "group", "nogroup"),
					checkFileUID(dir, nobody.Uid),
					checkFileGID(dir, nogroup.Gid),
					checkSameInode(dir, &inode),
				),
			},
			{
				// Numeric IDs are accepted and kept verbatim, so the
				// post-apply plan is empty (checked by the harness).
				Config: config(nobody.Uid, nogroup.Gid),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testDirResource, "owner", nobody.Uid),
					resource.TestCheckResourceAttr(testDirResource, "group", nogroup.Gid),
					checkFileUID(dir, nobody.Uid),
					checkFileGID(dir, nogroup.Gid),
					checkSameInode(dir, &inode),
				),
			},
		},
	})
}

func TestAccDirectory_nestedCreateParents(t *testing.T) {
	requireRoot(t)

	base := t.TempDir()
	dir := filepath.Join(base, "a", "b", "c")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeTestCheckFunc(
			checkDirectoryDestroyed,
			// Only the managed leaf is removed; auto-created parents remain.
			checkIsDir(filepath.Join(base, "a", "b")),
		),
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "sysutils_directory" "test" {
  path           = %q
  mode           = "0750"
  create_parents = true
}`, dir),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkIsDir(dir),
					checkFileMode(dir, 0o750),
					checkIsDir(filepath.Join(base, "a")),
					checkIsDir(filepath.Join(base, "a", "b")),
				),
			},
		},
	})
}

func TestAccDirectory_noCreateParentsMissingParent(t *testing.T) {
	requireRoot(t)

	base := t.TempDir()
	missingParent := filepath.Join(base, "missing")
	dir := filepath.Join(missingParent, "child")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkDirectoryDestroyed,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "sysutils_directory" "test" {
  path           = %q
  create_parents = false
}`, dir),
				ExpectError: regexp.MustCompile(`create_parents is false`),
			},
		},
	})

	if _, err := os.Lstat(missingParent); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("parent must not be created when create_parents = false, stat err = %v", err)
	}
}

func TestAccDirectory_noCreateParentsExistingParent(t *testing.T) {
	requireRoot(t)

	dir := filepath.Join(t.TempDir(), "child")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkDirectoryDestroyed,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "sysutils_directory" "test" {
  path           = %q
  create_parents = false
}`, dir),
				Check: checkIsDir(dir),
			},
		},
	})
}

func TestAccDirectory_existingRegularFile(t *testing.T) {
	requireRoot(t)

	target := filepath.Join(t.TempDir(), "occupied")
	if err := os.WriteFile(target, []byte("keep me\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "sysutils_directory" "test" {
  path = %q
}`, target),
				ExpectError: regexp.MustCompile(`exists\s+but\s+is\s+not\s+a\s+directory`),
			},
		},
	})

	// The provider must not clobber the pre-existing file.
	if err := checkFileContent(target, "keep me\n")(nil); err != nil {
		t.Fatal(err)
	}
	if err := checkFileMode(target, 0o600)(nil); err != nil {
		t.Fatal(err)
	}
}

func TestAccDirectory_driftDetection(t *testing.T) {
	requireRoot(t)

	dir := filepath.Join(t.TempDir(), "drift")
	config := fmt.Sprintf(`
resource "sysutils_directory" "test" {
  path = %q
  mode = "0750"
}`, dir)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkDirectoryDestroyed,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check:  checkFileMode(dir, 0o750),
			},
			{
				// Mode changed out of band: refresh must surface it as a diff.
				PreConfig: func() {
					if err := os.Chmod(dir, 0o777); err != nil {
						t.Fatal(err)
					}
				},
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				// Applying the same config repairs the drift in place.
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testDirResource, plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testDirResource, "mode", "0750"),
					checkFileMode(dir, 0o750),
				),
			},
			{
				// Directory deleted out of band: it must be planned for re-creation.
				PreConfig: func() {
					if err := os.Remove(dir); err != nil {
						t.Fatal(err)
					}
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testDirResource, plancheck.ResourceActionCreate),
					},
				},
				Check: checkFileMode(dir, 0o750),
			},
		},
	})
}

func TestAccDirectory_destroyNonEmpty(t *testing.T) {
	requireRoot(t)

	dir := filepath.Join(t.TempDir(), "nonempty")
	config := func(forceDestroy bool) string {
		return fmt.Sprintf(`
resource "sysutils_directory" "test" {
  path          = %q
  force_destroy = %t
}`, dir, forceDestroy)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkDirectoryDestroyed,
		Steps: []resource.TestStep{
			{
				Config: config(false),
				Check: func(*terraform.State) error {
					if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
						return err
					}
					return os.WriteFile(filepath.Join(dir, "sub", "file.txt"), []byte("x"), 0o644)
				},
			},
			{
				Config:      config(false),
				Destroy:     true,
				ExpectError: regexp.MustCompile(`Directory not empty`),
			},
			{
				// The failed destroy must leave the directory and its contents intact.
				Config: config(false),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkIsDir(dir),
					checkFileContent(filepath.Join(dir, "sub", "file.txt"), "x"),
				),
			},
			{
				// Enabling force_destroy is an in-place update; the post-test
				// destroy then removes the tree recursively (see CheckDestroy).
				Config: config(true),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testDirResource, plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.TestCheckResourceAttr(testDirResource, "force_destroy", "true"),
			},
		},
	})
}

func TestAccDirectory_import(t *testing.T) {
	requireRoot(t)

	dir := filepath.Join(t.TempDir(), "imported")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkDirectoryDestroyed,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "sysutils_directory" "test" {
  path = %q
  mode = "0711"
}`, dir),
			},
			{
				ResourceName:      testDirResource,
				ImportState:       true,
				ImportStateVerify: true,
			},
			{
				ResourceName:  testDirResource,
				ImportState:   true,
				ImportStateId: "relative/path",
				ExpectError:   regexp.MustCompile(`Invalid import ID`),
			},
		},
	})
}

// checkDirectoryDestroyed verifies that every sysutils_directory in the
// pre-destroy state no longer exists on disk.
func checkDirectoryDestroyed(s *terraform.State) error {
	for _, rs := range s.RootModule().Resources {
		if rs.Type != "sysutils_directory" {
			continue
		}
		p := rs.Primary.Attributes["path"]
		if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("directory %s still exists after destroy (stat err = %v)", p, err)
		}
	}
	return nil
}

func checkIsDir(path string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory (mode %s)", path, info.Mode())
		}
		return nil
	}
}

func checkModeBits(path string, bits fs.FileMode) resource.TestCheckFunc {
	return func(*terraform.State) error {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if info.Mode()&bits != bits {
			return fmt.Errorf("mode of %s is %s, want bits %s set", path, info.Mode(), bits)
		}
		return nil
	}
}

func checkFileGID(path, wantGID string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("stat_t unavailable on this platform")
		}
		if got := strconv.FormatUint(uint64(st.Gid), 10); got != wantGID {
			return fmt.Errorf("gid mismatch at %s: got %s want %s", path, got, wantGID)
		}
		return nil
	}
}

func inodeOf(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("stat_t unavailable on this platform")
	}
	return st.Ino, nil
}

// recordInode stores the inode of path so later steps can assert that the
// directory was modified in place rather than recreated.
func recordInode(path string, dst *uint64) resource.TestCheckFunc {
	return func(*terraform.State) error {
		ino, err := inodeOf(path)
		*dst = ino
		return err
	}
}

func checkSameInode(path string, want *uint64) resource.TestCheckFunc {
	return func(*terraform.State) error {
		ino, err := inodeOf(path)
		if err != nil {
			return err
		}
		if ino != *want {
			return fmt.Errorf("%s was recreated: inode %d, want %d", path, ino, *want)
		}
		return nil
	}
}
