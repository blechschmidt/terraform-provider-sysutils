package provider

import (
	"context"
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
	"golang.org/x/sys/unix"
)

// recursiveFixture is a pre-populated directory tree with a symlink to a file
// and a symlink to a directory that both live outside the tree.
type recursiveFixture struct {
	root, sub, deep            string
	fileA, fileB, fileC        string
	linkOut, linkOutDir        string
	outside, outsideDir, inOut string
}

func newRecursiveFixture(t *testing.T) recursiveFixture {
	t.Helper()
	base := t.TempDir()
	f := recursiveFixture{
		root:       filepath.Join(base, "tree"),
		outside:    filepath.Join(base, "outside.txt"),
		outsideDir: filepath.Join(base, "outside-dir"),
	}
	f.sub = filepath.Join(f.root, "sub")
	f.deep = filepath.Join(f.sub, "deeper")
	f.fileA = filepath.Join(f.root, "a.txt")
	f.fileB = filepath.Join(f.sub, "b.txt")
	f.fileC = filepath.Join(f.deep, "c.sh")
	f.linkOut = filepath.Join(f.sub, "link-out")
	f.linkOutDir = filepath.Join(f.root, "link-out-dir")
	f.inOut = filepath.Join(f.outsideDir, "victim.txt")

	for _, d := range []string{f.deep, f.outsideDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{f.fileA, f.fileB, f.fileC, f.outside, f.inOut} {
		if err := os.WriteFile(p, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Deliberately inconsistent modes inside the tree.
	mustChmod(t, f.sub, 0o700)
	mustChmod(t, f.deep, 0o777)
	mustChmod(t, f.fileA, 0o666)
	mustChmod(t, f.fileC, 0o755)
	// The outside targets have a distinctive mode that must survive.
	mustChmod(t, f.outside, 0o600)
	mustChmod(t, f.outsideDir, 0o711)
	mustChmod(t, f.inOut, 0o600)
	if err := os.Symlink(f.outside, f.linkOut); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.outsideDir, f.linkOutDir); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestAccDirectory_recursive(t *testing.T) {
	requireRoot(t)
	nobody, nogroup := lookupNobody(t)

	f := newRecursiveFixture(t)
	recursiveConfig := fmt.Sprintf(`
resource "sysutils_directory" "test" {
  path            = %q
  owner           = "nobody"
  group           = %q
  mode            = "0750"
  file_mode       = "0640"
  recursive_owner = true
  recursive_mode  = true
  force_destroy   = true
}`, f.root, nogroup.Name)
	plainConfig := fmt.Sprintf(`
resource "sysutils_directory" "test" {
  path          = %q
  owner         = "nobody"
  group         = %q
  mode          = "0750"
  force_destroy = true
}`, f.root, nogroup.Name)

	checkTreeConforms := resource.ComposeAggregateTestCheckFunc(
		resource.TestCheckResourceAttr(testDirResource, "nonconforming_entries", "0"),
		resource.TestCheckResourceAttr(testDirResource, "recursive_owner", "true"),
		resource.TestCheckResourceAttr(testDirResource, "recursive_mode", "true"),
		resource.TestCheckResourceAttr(testDirResource, "file_mode", "0640"),
		// Directories get mode, regular files get file_mode.
		checkFileMode(f.root, 0o750),
		checkFileMode(f.sub, 0o750),
		checkFileMode(f.deep, 0o750),
		checkFileMode(f.fileA, 0o640),
		checkFileMode(f.fileB, 0o640),
		checkFileMode(f.fileC, 0o640),
		// Every entry, including the symlinks themselves, is owned by the
		// directory's owner and group.
		checkLOwnership(f.root, nobody.Uid, nogroup.Gid),
		checkLOwnership(f.sub, nobody.Uid, nogroup.Gid),
		checkLOwnership(f.deep, nobody.Uid, nogroup.Gid),
		checkLOwnership(f.fileA, nobody.Uid, nogroup.Gid),
		checkLOwnership(f.fileB, nobody.Uid, nogroup.Gid),
		checkLOwnership(f.fileC, nobody.Uid, nogroup.Gid),
		checkLOwnership(f.linkOut, nobody.Uid, nogroup.Gid),
		checkLOwnership(f.linkOutDir, nobody.Uid, nogroup.Gid),
		// Symlink targets outside the tree are never touched, nor is
		// anything inside a directory reached through a symlink.
		checkUntouched(f.outside, 0o600),
		checkUntouched(f.outsideDir, 0o711),
		checkUntouched(f.inOut, 0o600),
	)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkDirectoryDestroyed,
		Steps: []resource.TestStep{
			{
				// Adopting an existing, inconsistent tree fixes all of it.
				Config: recursiveConfig,
				Check:  checkTreeConforms,
			},
			{
				// A conforming tree yields an empty plan.
				Config:   recursiveConfig,
				PlanOnly: true,
			},
			{
				// Drift deep inside the tree is counted on refresh ...
				PreConfig: func() {
					mustChmod(t, f.fileC, 0o4755)
					if err := os.Lchown(f.deep, 0, 0); err != nil {
						t.Fatal(err)
					}
					if err := os.Lchown(f.linkOut, 0, -1); err != nil {
						t.Fatal(err)
					}
				},
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testDirResource, "nonconforming_entries", "3"),
					// The directory itself is unchanged.
					resource.TestCheckResourceAttr(testDirResource, "mode", "0750"),
					resource.TestCheckResourceAttr(testDirResource, "owner", "nobody"),
				),
			},
			{
				// ... plans an in-place update ...
				Config:             recursiveConfig,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				// ... and apply repairs it.
				Config: recursiveConfig,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testDirResource, plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkTreeConforms,
					checkNoSpecialBits(f.fileC),
				),
			},
			{
				// Turning recursion off leaves the tree as it is ...
				Config: plainConfig,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testDirResource, plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testDirResource, "recursive_owner", "false"),
					resource.TestCheckResourceAttr(testDirResource, "recursive_mode", "false"),
					resource.TestCheckNoResourceAttr(testDirResource, "file_mode"),
					resource.TestCheckResourceAttr(testDirResource, "nonconforming_entries", "0"),
					checkFileMode(f.fileB, 0o640),
					checkLOwnership(f.fileB, nobody.Uid, nogroup.Gid),
				),
			},
			{
				// ... and changes inside it are no longer drift.
				PreConfig: func() {
					mustChmod(t, f.fileB, 0o600)
					if err := os.Lchown(f.fileB, 0, 0); err != nil {
						t.Fatal(err)
					}
				},
				Config:   plainConfig,
				PlanOnly: true,
			},
			{
				Config: plainConfig,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testDirResource, "nonconforming_entries", "0"),
					checkFileMode(f.fileB, 0o600),
					checkLOwnership(f.fileB, "0", "0"),
				),
			},
		},
	})

	// force_destroy removed the tree but not the outside targets.
	for _, p := range []string{f.outside, f.inOut} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("symlink target %s should survive destroy: %v", p, err)
		}
	}
}

func TestAccDirectory_recursiveOwnerOnlyAndModeOnly(t *testing.T) {
	requireRoot(t)
	nobody, _ := lookupNobody(t)

	f := newRecursiveFixture(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkDirectoryDestroyed,
		Steps: []resource.TestStep{
			{
				// Ownership only: modes inside the tree are left alone.
				Config: fmt.Sprintf(`
resource "sysutils_directory" "test" {
  path            = %q
  owner           = "nobody"
  recursive_owner = true
  force_destroy   = true
}`, f.root),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkLOwnership(f.fileC, nobody.Uid, "0"),
					checkLOwnership(f.linkOut, nobody.Uid, "0"),
					checkFileMode(f.fileA, 0o666),
					checkFileMode(f.deep, 0o777),
					checkUntouched(f.outside, 0o600),
				),
			},
			{
				// Mode only, without file_mode: files get mode, like chmod -R.
				Config: fmt.Sprintf(`
resource "sysutils_directory" "test" {
  path           = %q
  owner          = "nobody"
  mode           = "0700"
  recursive_mode = true
  force_destroy  = true
}`, f.root),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testDirResource, "nonconforming_entries", "0"),
					checkFileMode(f.fileA, 0o700),
					checkFileMode(f.fileC, 0o700),
					checkFileMode(f.deep, 0o700),
					checkUntouched(f.outside, 0o600),
					checkUntouched(f.outsideDir, 0o711),
				),
			},
			{
				// Ownership drift is not tracked with recursive_owner = false.
				PreConfig: func() {
					if err := os.Lchown(f.fileA, 0, 0); err != nil {
						t.Fatal(err)
					}
				},
				RefreshState: true,
				Check:        resource.TestCheckResourceAttr(testDirResource, "nonconforming_entries", "0"),
			},
		},
	})
}

func TestAccDirectory_recursiveSkipsMountPoints(t *testing.T) {
	requireRoot(t)

	f := newRecursiveFixture(t)
	mnt := filepath.Join(f.root, "mnt")
	if err := os.Mkdir(mnt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mount("tmpfs", mnt, "tmpfs", 0, "mode=0711"); err != nil {
		t.Skipf("cannot mount tmpfs: %v", err)
	}
	t.Cleanup(func() { _ = unix.Unmount(mnt, unix.MNT_DETACH) })
	inMount := filepath.Join(mnt, "other-fs.txt")
	if err := os.WriteFile(inMount, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	config := fmt.Sprintf(`
resource "sysutils_directory" "test" {
  path            = %q
  owner           = "nobody"
  mode            = "0755"
  recursive_owner = true
  recursive_mode  = true
  force_destroy   = true
}`, f.root)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkDirectoryDestroyed,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testDirResource, "nonconforming_entries", "0"),
					checkFileMode(f.fileA, 0o755),
					// The mounted filesystem, including its root, is untouched.
					checkUntouched(mnt, 0o711),
					checkUntouched(inMount, 0o600),
				),
			},
			{
				// force_destroy refuses to cross the mount point, so unmount
				// before the final destroy.
				PreConfig: func() {
					if err := unix.Unmount(mnt, unix.MNT_DETACH); err != nil {
						t.Fatal(err)
					}
				},
				// The directory that was hidden by the mount is now part of
				// the tree and gets fixed.
				Config: config,
				Check:  checkFileMode(mnt, 0o755),
			},
		},
	})
}

func TestAccDirectory_fileModeRequiresRecursiveMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d")
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "sysutils_directory" "test" {
  path      = %q
  file_mode = "0644"
}`, dir),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`file_mode\s+requires\s+recursive_mode`),
			},
		},
	})
}

// TestConformTree exercises the walker directly, including the count
// reported before and after fixing.
func TestConformTree(t *testing.T) {
	requireRoot(t)
	f := newRecursiveFixture(t)

	spec, err := newTreeSpec(true, true, "0750", "0640")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(f.root, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	// Everything below the root: sub, deeper, 3 files, 2 symlinks.
	n, err := conformTree(context.Background(), f.root, spec, false)
	if err != nil {
		t.Fatal(err)
	}
	if n != 7 {
		t.Fatalf("nonconforming before fix = %d, want 7", n)
	}
	if n, err = conformTree(context.Background(), f.root, spec, true); err != nil || n != 7 {
		t.Fatalf("fix: n=%d err=%v, want 7, nil", n, err)
	}
	if n, err = conformTree(context.Background(), f.root, spec, false); err != nil || n != 0 {
		t.Fatalf("after fix: n=%d err=%v, want 0, nil", n, err)
	}

	// A disabled spec never touches the filesystem.
	if n, err = conformTree(context.Background(), "/nonexistent", treeSpec{}, false); err != nil || n != 0 {
		t.Fatalf("disabled spec: n=%d err=%v", n, err)
	}
	// The root itself must not be a symlink.
	if _, err = conformTree(context.Background(), f.linkOutDir, spec, false); err == nil {
		t.Fatal("expected an error for a symlinked root")
	}

	// Cancellation stops the walk.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = conformTree(ctx, f.root, spec, false); err == nil {
		t.Fatal("expected an error for a cancelled context")
	}
}

func TestChmodPathFD(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(p, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()

	for _, tc := range []struct {
		name  string
		chmod func(int, uint32) error
		mode  uint32
	}{
		{"fchmodat2 or fallback", chmodPathFD, 0o640},
		{"proc fallback", chmodViaProcFD, 0o604},
	} {
		if err := tc.chmod(fd, tc.mode); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		info, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := uint32(info.Mode().Perm()); got != tc.mode {
			t.Fatalf("%s: mode = %#o, want %#o", tc.name, got, tc.mode)
		}
	}
}

func lookupNobody(t *testing.T) (*user.User, *user.Group) {
	t.Helper()
	nobody, err := user.Lookup("nobody")
	if err != nil {
		t.Skipf("nobody user not available: %v", err)
	}
	nogroup, err := user.LookupGroup("nogroup")
	if err != nil {
		t.Skipf("nogroup group not available: %v", err)
	}
	return nobody, nogroup
}

func mustChmod(t *testing.T, p string, mode uint32) {
	t.Helper()
	if err := unix.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

// checkLOwnership checks the UID and GID of p itself, not following a
// symlink at p.
func checkLOwnership(p, wantUID, wantGID string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		st := info.Sys().(*syscall.Stat_t)
		uid, gid := strconv.FormatUint(uint64(st.Uid), 10), strconv.FormatUint(uint64(st.Gid), 10)
		if uid != wantUID || gid != wantGID {
			return fmt.Errorf("ownership of %s is %s:%s, want %s:%s", p, uid, gid, wantUID, wantGID)
		}
		return nil
	}
}

// checkUntouched checks that p (not following symlinks) still has its
// original root ownership and the given permission bits.
func checkUntouched(p string, perm fs.FileMode) resource.TestCheckFunc {
	return resource.ComposeAggregateTestCheckFunc(
		checkLOwnership(p, "0", "0"),
		func(*terraform.State) error {
			info, err := os.Lstat(p)
			if err != nil {
				return err
			}
			if got := info.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky); got != perm {
				return fmt.Errorf("mode of %s changed to %s, want %s", p, got, perm)
			}
			return nil
		},
	)
}

func checkNoSpecialBits(p string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if info.Mode()&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0 {
			return fmt.Errorf("%s still has special bits: %s", p, info.Mode())
		}
		return nil
	}
}
