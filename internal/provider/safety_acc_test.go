package provider

// Acceptance tests for the symlink, TOCTOU and recursive-deletion guards
// described in safefs.go. Each test plants the kind of entry a local attacker
// could create and verifies that the provider refuses to act on it and leaves
// the attacker's chosen victim untouched.

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

func TestAccDirectory_refusesSymlinkAtPath(t *testing.T) {
	requireRoot(t)

	root := t.TempDir()
	victim := filepath.Join(root, "victim")
	if err := os.Mkdir(victim, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "sysutils_directory" "test" {
  path  = %q
  mode  = "0777"
  owner = "65534"
}`, link),
				ExpectError: regexp.MustCompile(`symbolic\s+link`),
			},
		},
	})

	// Neither chmod nor chown may have followed the link.
	checkVictimDir(t, victim, 0o700, 0)
}

func TestAccDirectory_symlinkSwappedInAfterCreate(t *testing.T) {
	requireRoot(t)

	root := t.TempDir()
	dir := filepath.Join(root, "managed")
	victim := filepath.Join(root, "victim")
	mustWrite(t, filepath.Join(victim, "precious.txt"), "keep")
	if err := os.Chmod(victim, 0o700); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(`
resource "sysutils_directory" "test" {
  path          = %q
  mode          = "0755"
  force_destroy = true
}`, dir)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkDirectoryDestroyed,
		Steps: []resource.TestStep{
			{Config: config},
			{
				// Replace the managed directory with a symlink to the victim.
				// Refresh must fail instead of adopting (and later chmod-ing
				// or deleting) the victim through the link.
				PreConfig: func() {
					if err := os.Remove(dir); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(victim, dir); err != nil {
						t.Fatal(err)
					}
				},
				Config:      config,
				ExpectError: regexp.MustCompile(`symbolic\s+link`),
			},
			{
				// Restore a real directory so the final destroy can proceed.
				PreConfig: func() {
					checkVictimDir(t, victim, 0o700, 0)
					if err := os.Remove(dir); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(dir, 0o755); err != nil {
						t.Fatal(err)
					}
				},
				Config: config,
			},
		},
	})

	checkVictimDir(t, victim, 0o700, 0)
	mustExist(t, filepath.Join(victim, "precious.txt"))
}

func TestAccDirectory_forceDestroyThroughSymlinkRefused(t *testing.T) {
	requireRoot(t)

	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	// Intermediate symlinks are followed for ordinary operations but never
	// for recursive deletion.
	dir := filepath.Join(link, "data")
	precious := filepath.Join(real, "data", "precious.txt")
	config := fmt.Sprintf(`
resource "sysutils_directory" "test" {
  path          = %q
  force_destroy = true
}`, dir)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkDirectoryDestroyed,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: func(*terraform.State) error {
					return os.WriteFile(precious, []byte("keep"), 0o644)
				},
			},
			{
				Config:      config,
				Destroy:     true,
				ExpectError: regexp.MustCompile(`(?s)path component.*is\s+a\s+symbolic\s+link`),
			},
			{
				// The refused destroy must not have deleted anything. Then
				// replace the symlink with the real directory so the final
				// destroy can succeed.
				PreConfig: func() {
					mustExist(t, precious)
					if err := os.Remove(link); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(real, link); err != nil {
						t.Fatal(err)
					}
				},
				Config: config,
			},
		},
	})
}

func TestAccDirectory_forceDestroyProtectedPathRejectedAtPlan(t *testing.T) {
	requireRoot(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "sysutils_directory" "test" {
  path          = "/etc"
  force_destroy = true
}`,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`protected\s+system\s+directory`),
			},
		},
	})
}

func TestAccFile_refusesSymlinkAtPath(t *testing.T) {
	requireRoot(t)

	root := t.TempDir()
	victim := filepath.Join(root, "shadow")
	if err := os.WriteFile(victim, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "app.conf")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "sysutils_file" "test" {
  path    = %q
  content = "pwned"
  mode    = "0666"
}`, link),
				ExpectError: regexp.MustCompile(`symbolic\s+link`),
			},
		},
	})

	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "secret" {
		t.Errorf("victim content = %q, want it untouched", got)
	}
	checkVictimDir(t, victim, 0o600, 0)
}

func TestAccFile_refusesFIFOAtPath(t *testing.T) {
	requireRoot(t)

	// Opening a FIFO for writing blocks until a reader appears; the provider
	// must detect it instead of hanging forever.
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "sysutils_file" "test" {
  path    = %q
  content = "x"
}`, fifo),
				ExpectError: regexp.MustCompile(`not\s+a\s+regular\s+file`),
			},
		},
	})
}

func TestAccFile_setuidSurvivesOwnershipChange(t *testing.T) {
	requireRoot(t)

	target := filepath.Join(t.TempDir(), "tool")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkFileDestroyed,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "sysutils_file" "test" {
  path    = %q
  content = "#!/bin/sh\n"
  mode    = "6755"
  owner   = "65534"
  group   = "65534"
}`, target),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("sysutils_file.test", "mode", "6755"),
					checkModeBits(target, fs.ModeSetuid|fs.ModeSetgid|0o755),
					checkFileUID(target, "65534"),
					checkFileGID(target, "65534"),
				),
			},
		},
	})
}

// checkVictimDir fails the test unless p has exactly permission bits perm
// and is owned by uid.
func checkVictimDir(t *testing.T, p string, perm fs.FileMode, uid uint32) {
	t.Helper()
	info, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != perm {
		t.Errorf("mode of %s = %#o, want %#o (modified through a symlink?)", p, got, perm)
	}
	if st := info.Sys().(*syscall.Stat_t); st.Uid != uid {
		t.Errorf("owner of %s = %d, want %d (modified through a symlink?)", p, st.Uid, uid)
	}
}
