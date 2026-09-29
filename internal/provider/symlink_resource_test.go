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
	"strings"
	"syscall"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const testSymlinkResource = "sysutils_symlink.test"

func symlinkConfig(link, target string) string {
	return fmt.Sprintf(`
resource "sysutils_symlink" "test" {
  path   = %q
  target = %q
}`, link, target)
}

func TestValidateSymlinkTarget(t *testing.T) {
	for _, s := range []string{"/etc/hosts", "relative", "../up", "./dot", "/does/not/exist", "with space"} {
		if err := validateSymlinkTarget(s); err != nil {
			t.Errorf("validateSymlinkTarget(%q) = %v, want nil", s, err)
		}
	}
	for _, s := range []string{"", "a\x00b"} {
		if err := validateSymlinkTarget(s); err == nil {
			t.Errorf("validateSymlinkTarget(%q) = nil, want error", s)
		}
	}
}

// TestAccSymlink_rejectInvalidConfig checks that invalid values are rejected
// at validation time, so it does not require root.
func TestAccSymlink_rejectInvalidConfig(t *testing.T) {
	cases := []struct {
		link, target, wantErr string
	}{
		{"relative/link", "/etc/hosts", "must be absolute"},
		{"/tmp/sysutils-symlink-test/", "/etc/hosts", "canonical form"},
		{"/", "/etc/hosts", "filesystem root"},
		{"/tmp/sysutils-symlink-test", "", "must not be empty"},
	}
	steps := make([]resource.TestStep, 0, len(cases))
	for _, c := range cases {
		steps = append(steps, resource.TestStep{
			Config:      symlinkConfig(c.link, c.target),
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(regexp.QuoteMeta(c.wantErr)),
		})
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps:                    steps,
	})
}

func TestAccSymlink_createAndChangeTarget(t *testing.T) {
	requireRoot(t)

	base := t.TempDir()
	first := filepath.Join(base, "first.txt")
	second := filepath.Join(base, "second.txt")
	for _, p := range []string{first, second} {
		if err := os.WriteFile(p, []byte(filepath.Base(p)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	linkDir := filepath.Join(base, "links", "nested")
	link := filepath.Join(linkDir, "current")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeTestCheckFunc(
			checkSymlinkDestroyed,
			// Only the link is removed, never what it points to.
			checkFileContent(first, "first.txt"),
			checkFileContent(second, "second.txt"),
		),
		Steps: []resource.TestStep{
			{
				Config: symlinkConfig(link, first),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testSymlinkResource, "path", link),
					resource.TestCheckResourceAttr(testSymlinkResource, "id", link),
					resource.TestCheckResourceAttr(testSymlinkResource, "target", first),
					resource.TestCheckResourceAttr(testSymlinkResource, "owner", "root"),
					resource.TestCheckResourceAttr(testSymlinkResource, "group", "root"),
					checkSymlinkTarget(link, first),
					checkFileContent(link, "first.txt"),
					checkNoTempLinks(linkDir),
				),
			},
			{
				// Changing the target re-links atomically in place.
				Config: symlinkConfig(link, second),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testSymlinkResource, plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testSymlinkResource, "target", second),
					checkSymlinkTarget(link, second),
					checkFileContent(link, "second.txt"),
					checkFileContent(first, "first.txt"),
					checkNoTempLinks(linkDir),
				),
			},
			{
				// Relative and dangling targets are stored verbatim.
				Config: symlinkConfig(link, "../does-not-exist"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testSymlinkResource, plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testSymlinkResource, "target", "../does-not-exist"),
					checkSymlinkTarget(link, "../does-not-exist"),
					checkNoTempLinks(linkDir),
				),
			},
		},
	})
}

func TestAccSymlink_ownership(t *testing.T) {
	requireRoot(t)

	nobody, err := user.Lookup("nobody")
	if err != nil {
		t.Skipf("nobody user not available: %v", err)
	}
	nogroup, err := user.LookupGroup("nogroup")
	if err != nil {
		t.Skipf("nogroup group not available: %v", err)
	}

	base := t.TempDir()
	targetDir := filepath.Join(base, "target")
	if err := os.Mkdir(targetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	config := func(target, owner, group string) string {
		return fmt.Sprintf(`
resource "sysutils_symlink" "test" {
  path   = %q
  target = %q
  owner  = %q
  group  = %q
}`, link, target, owner, group)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeTestCheckFunc(
			checkSymlinkDestroyed,
			checkIsDir(targetDir),
		),
		Steps: []resource.TestStep{
			{
				Config: config(targetDir, "nobody", "nogroup"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testSymlinkResource, "owner", "nobody"),
					resource.TestCheckResourceAttr(testSymlinkResource, "group", "nogroup"),
					checkLinkOwnership(link, nobody.Uid, nogroup.Gid),
					// lchown must not touch the target.
					checkFileUID(targetDir, "0"),
					checkFileGID(targetDir, "0"),
				),
			},
			{
				// Numeric IDs are accepted and kept verbatim, so the
				// post-apply plan is empty (checked by the harness).
				Config: config(targetDir, nobody.Uid, nogroup.Gid),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testSymlinkResource, "owner", nobody.Uid),
					resource.TestCheckResourceAttr(testSymlinkResource, "group", nogroup.Gid),
					checkLinkOwnership(link, nobody.Uid, nogroup.Gid),
				),
			},
			{
				// Re-linking to a new target preserves the link's ownership.
				Config: config(base, nobody.Uid, nogroup.Gid),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkSymlinkTarget(link, base),
					checkLinkOwnership(link, nobody.Uid, nogroup.Gid),
				),
			},
			{
				// Ownership-only change is applied in place with lchown.
				Config: config(base, "root", "root"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testSymlinkResource, plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testSymlinkResource, "owner", "root"),
					checkLinkOwnership(link, "0", "0"),
					checkFileUID(targetDir, "0"),
				),
			},
		},
	})
}

func TestAccSymlink_externalChanges(t *testing.T) {
	requireRoot(t)

	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.WriteFile(target, []byte("target"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	config := symlinkConfig(link, target)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkSymlinkDestroyed,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check:  checkSymlinkTarget(link, target),
			},
			{
				// Link deleted out of band: it must be planned for re-creation.
				PreConfig: func() {
					if err := os.Remove(link); err != nil {
						t.Fatal(err)
					}
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testSymlinkResource, plancheck.ResourceActionCreate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkSymlinkTarget(link, target),
					checkFileContent(target, "target"),
				),
			},
			{
				// Link re-pointed out of band: refresh detects the drift and
				// the apply repairs it in place.
				PreConfig: func() {
					if err := os.Remove(link); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink("/elsewhere", link); err != nil {
						t.Fatal(err)
					}
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testSymlinkResource, plancheck.ResourceActionUpdate),
					},
				},
				Check: checkSymlinkTarget(link, target),
			},
			{
				// Link replaced by a regular file: the resource is dropped from
				// state and planned for creation, but the file is not clobbered.
				PreConfig: func() {
					if err := os.Remove(link); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(link, []byte("keep me"), 0o600); err != nil {
						t.Fatal(err)
					}
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testSymlinkResource, plancheck.ResourceActionCreate),
					},
				},
				ExpectError: regexp.MustCompile(`exists\s+but\s+is\s+not\s+a\s+symlink`),
			},
			{
				PreConfig: func() {
					if err := checkFileContent(link, "keep me")(nil); err != nil {
						t.Fatal(err)
					}
					if err := os.Remove(link); err != nil {
						t.Fatal(err)
					}
				},
				Config: config,
				Check:  checkSymlinkTarget(link, target),
			},
		},
	})
}

func TestAccSymlink_replacesExistingSymlink(t *testing.T) {
	requireRoot(t)

	base := t.TempDir()
	link := filepath.Join(base, "link")
	if err := os.Symlink("/old/target", link); err != nil {
		t.Fatal(err)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkSymlinkDestroyed,
		Steps: []resource.TestStep{
			{
				Config: symlinkConfig(link, "/new/target"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkSymlinkTarget(link, "/new/target"),
					checkNoTempLinks(base),
				),
			},
		},
	})
}

func TestAccSymlink_existingNonSymlink(t *testing.T) {
	requireRoot(t)

	base := t.TempDir()
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "dir")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{file, dir} {
		resource.Test(t, resource.TestCase{
			ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
			Steps: []resource.TestStep{
				{
					Config:      symlinkConfig(p, "/etc/hosts"),
					ExpectError: regexp.MustCompile(`exists\s+but\s+is\s+not\s+a\s+symlink`),
				},
			},
		})
	}

	if err := checkFileContent(file, "keep me")(nil); err != nil {
		t.Fatal(err)
	}
	if err := checkIsDir(dir)(nil); err != nil {
		t.Fatal(err)
	}
	if err := checkNoTempLinks(base)(nil); err != nil {
		t.Fatal(err)
	}
}

func TestAccSymlink_import(t *testing.T) {
	requireRoot(t)

	base := t.TempDir()
	link := filepath.Join(base, "link")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkSymlinkDestroyed,
		Steps: []resource.TestStep{
			{
				Config: symlinkConfig(link, "relative/target"),
			},
			{
				ResourceName:      testSymlinkResource,
				ImportState:       true,
				ImportStateVerify: true,
			},
			{
				ResourceName:  testSymlinkResource,
				ImportState:   true,
				ImportStateId: "relative/path",
				ExpectError:   regexp.MustCompile(`Invalid import ID`),
			},
			{
				ResourceName:  testSymlinkResource,
				ImportState:   true,
				ImportStateId: filepath.Join(base, "missing"),
				ExpectError:   regexp.MustCompile(`Cannot import non-existent remote object`),
			},
		},
	})
}

// TestAccSymlink_importUnmanaged imports a link that Terraform did not create
// and checks that its attributes are populated from disk.
func TestAccSymlink_importUnmanaged(t *testing.T) {
	requireRoot(t)

	link := filepath.Join(t.TempDir(), "existing")
	if err := os.Symlink("/etc/hosts", link); err != nil {
		t.Fatal(err)
	}
	config := symlinkConfig(link, "/etc/hosts")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:             config,
				ResourceName:       testSymlinkResource,
				ImportState:        true,
				ImportStateId:      link,
				ImportStatePersist: true,
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if len(states) != 1 {
						return fmt.Errorf("expected 1 imported state, got %d", len(states))
					}
					want := map[string]string{
						"id":     link,
						"path":   link,
						"target": "/etc/hosts",
						"owner":  "root",
						"group":  "root",
					}
					for k, v := range want {
						if got := states[0].Attributes[k]; got != v {
							return fmt.Errorf("imported %s = %q, want %q", k, got, v)
						}
					}
					return nil
				},
			},
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectEmptyPlan(),
					},
				},
			},
		},
	})

	// Destroying the imported link must leave its target alone.
	if _, err := os.Stat("/etc/hosts"); err != nil {
		t.Fatalf("symlink target removed on destroy: %v", err)
	}
}

// checkSymlinkDestroyed verifies that every sysutils_symlink in the
// pre-destroy state no longer exists on disk.
func checkSymlinkDestroyed(s *terraform.State) error {
	for _, rs := range s.RootModule().Resources {
		if rs.Type != "sysutils_symlink" {
			continue
		}
		p := rs.Primary.Attributes["path"]
		if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("symlink %s still exists after destroy (lstat err = %v)", p, err)
		}
	}
	return nil
}

func checkSymlinkTarget(link, want string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		info, err := os.Lstat(link)
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			return fmt.Errorf("%s is not a symlink (mode %s)", link, info.Mode())
		}
		got, err := os.Readlink(link)
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("%s points to %q, want %q", link, got, want)
		}
		return nil
	}
}

// checkLinkOwnership checks the UID and GID of the link itself (not its target).
func checkLinkOwnership(link, wantUID, wantGID string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		info, err := os.Lstat(link)
		if err != nil {
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("stat_t unavailable on this platform")
		}
		uid := strconv.FormatUint(uint64(st.Uid), 10)
		gid := strconv.FormatUint(uint64(st.Gid), 10)
		if uid != wantUID || gid != wantGID {
			return fmt.Errorf("ownership of link %s is %s:%s, want %s:%s", link, uid, gid, wantUID, wantGID)
		}
		return nil
	}
}

// checkNoTempLinks verifies that no temporary re-link entries were left behind.
func checkNoTempLinks(dir string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if strings.Contains(e.Name(), ".sysutils-tmp-") {
				return fmt.Errorf("leftover temporary symlink %s in %s", e.Name(), dir)
			}
		}
		return nil
	}
}
