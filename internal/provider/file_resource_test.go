package provider

import (
	"fmt"
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

func TestAccFile_createReadUpdateDelete(t *testing.T) {
	requireRoot(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "hello.txt")

	config := func(content, mode string) string {
		return fmt.Sprintf(`
resource "sysutils_file" "test" {
  path    = %q
  content = %q
  mode    = %q
}`, path, content, mode)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config("hello\n", "0640"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("sysutils_file.test", "content", "hello\n"),
					resource.TestCheckResourceAttr("sysutils_file.test", "mode", "0640"),
					checkFileContent(path, "hello\n"),
					checkFileMode(path, 0o640),
				),
			},
			{
				Config: config("updated\n", "0600"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("sysutils_file.test", "content", "updated\n"),
					resource.TestCheckResourceAttr("sysutils_file.test", "mode", "0600"),
					checkFileContent(path, "updated\n"),
					checkFileMode(path, 0o600),
				),
			},
		},
	})

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected file to be deleted, stat err = %v", err)
	}
}

func TestAccFile_ownership(t *testing.T) {
	requireRoot(t)

	// Use an existing low-privilege account that's guaranteed to exist in a
	// Debian-based container. "nobody" is also in the "nogroup" group there.
	u, err := user.Lookup("nobody")
	if err != nil {
		t.Skipf("nobody user not available: %v", err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "owned.txt")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "sysutils_file" "owned" {
  path    = %q
  content = "x"
  owner   = "nobody"
}`, path),
				Check: checkFileUID(path, u.Uid),
			},
		},
	})
}

const testFileResource = "sysutils_file.test"

func TestAccFile_import(t *testing.T) {
	requireRoot(t)

	target := filepath.Join(t.TempDir(), "imported.txt")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkFileDestroyed,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "sysutils_file" "test" {
  path    = %q
  content = "imported\n"
  mode    = "0600"
  owner   = "root"
  group   = "root"
}`, target),
			},
			{
				ResourceName:      testFileResource,
				ImportState:       true,
				ImportStateVerify: true,
			},
			{
				ResourceName:  testFileResource,
				ImportState:   true,
				ImportStateId: "relative/path",
				ExpectError:   regexp.MustCompile(`Invalid import ID`),
			},
		},
	})
}

// TestAccFile_importUnmanaged imports a file that Terraform did not create and
// checks that its attributes are populated from disk.
func TestAccFile_importUnmanaged(t *testing.T) {
	requireRoot(t)

	target := filepath.Join(t.TempDir(), "existing.txt")
	if err := os.WriteFile(target, []byte("pre-existing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o640); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(`
resource "sysutils_file" "test" {
  path    = %q
  content = "pre-existing\n"
  mode    = "0640"
}`, target)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:             config,
				ResourceName:       testFileResource,
				ImportState:        true,
				ImportStateId:      target,
				ImportStatePersist: true,
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if len(states) != 1 {
						return fmt.Errorf("expected 1 imported state, got %d", len(states))
					}
					want := map[string]string{
						"id":      target,
						"path":    target,
						"content": "pre-existing\n",
						"mode":    "0640",
						"owner":   "root",
						"group":   "root",
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
				// The imported state matches the configuration: nothing to do.
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectEmptyPlan(),
					},
				},
			},
		},
	})
}

func TestAccFile_importMissing(t *testing.T) {
	requireRoot(t)

	target := filepath.Join(t.TempDir(), "missing.txt")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "sysutils_file" "test" {
  path    = %q
  content = "x"
}`, target),
				ResourceName:  testFileResource,
				ImportState:   true,
				ImportStateId: target,
				ExpectError:   regexp.MustCompile(`Cannot import non-existent remote object`),
			},
		},
	})
}

func TestAccFile_driftDetection(t *testing.T) {
	requireRoot(t)

	nobody, nogroup := lookupNobody(t)
	nobodyUID, _ := strconv.Atoi(nobody.Uid)
	nogroupGID, _ := strconv.Atoi(nogroup.Gid)

	target := filepath.Join(t.TempDir(), "drift.txt")
	config := fmt.Sprintf(`
resource "sysutils_file" "test" {
  path    = %q
  content = "managed\n"
  mode    = "0640"
  owner   = "root"
  group   = "root"
}`, target)

	expectUpdate := resource.ConfigPlanChecks{
		PreApply: []plancheck.PlanCheck{
			plancheck.ExpectResourceAction(testFileResource, plancheck.ResourceActionUpdate),
		},
	}
	inSync := resource.ComposeAggregateTestCheckFunc(
		resource.TestCheckResourceAttr(testFileResource, "content", "managed\n"),
		resource.TestCheckResourceAttr(testFileResource, "mode", "0640"),
		resource.TestCheckResourceAttr(testFileResource, "owner", "root"),
		resource.TestCheckResourceAttr(testFileResource, "group", "root"),
		checkFileContent(target, "managed\n"),
		checkFileMode(target, 0o640),
		checkFileUID(target, "0"),
		checkFileGID(target, "0"),
	)
	mutate := func(fn func() error) func() {
		return func() {
			if err := fn(); err != nil {
				t.Fatal(err)
			}
		}
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkFileDestroyed,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check:  inSync,
			},
			{
				// Mode changed out of band: refresh must surface it as a diff.
				PreConfig:          mutate(func() error { return os.Chmod(target, 0o666) }),
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				Config:           config,
				ConfigPlanChecks: expectUpdate,
				Check:            inSync,
			},
			{
				// Owner and group changed out of band.
				PreConfig:          mutate(func() error { return os.Chown(target, nobodyUID, nogroupGID) }),
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				Config:           config,
				ConfigPlanChecks: expectUpdate,
				Check:            inSync,
			},
			{
				// Content changed out of band; writing keeps the file's mode.
				PreConfig:          mutate(func() error { return os.WriteFile(target, []byte("tampered\n"), 0o600) }),
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				Config:           config,
				ConfigPlanChecks: expectUpdate,
				Check:            inSync,
			},
			{
				// File deleted out of band: it must be planned for re-creation.
				PreConfig: mutate(func() error { return os.Remove(target) }),
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testFileResource, plancheck.ResourceActionCreate),
					},
				},
				Check: inSync,
			},
		},
	})
}

// TestAccFile_driftDefaultMode checks that the default mode is enforced when
// mode is not configured, and that equivalent mode spellings do not cause a
// perpetual diff.
func TestAccFile_driftDefaultMode(t *testing.T) {
	requireRoot(t)

	target := filepath.Join(t.TempDir(), "default.txt")
	config := func(mode string) string {
		attr := ""
		if mode != "" {
			attr = fmt.Sprintf("mode = %q", mode)
		}
		return fmt.Sprintf(`
resource "sysutils_file" "test" {
  path    = %q
  content = "x"
  %s
}`, target, attr)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkFileDestroyed,
		Steps: []resource.TestStep{
			{
				Config: config(""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testFileResource, "mode", defaultFileMode),
					checkFileMode(target, 0o644),
				),
			},
			{
				PreConfig: func() {
					if err := os.Chmod(target, 0o600); err != nil {
						t.Fatal(err)
					}
				},
				Config: config(""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testFileResource, plancheck.ResourceActionUpdate),
					},
				},
				Check: checkFileMode(target, 0o644),
			},
			{
				// "644" is equivalent to the on-disk 0644: kept verbatim and
				// the post-apply plan is empty (checked by the harness).
				Config: config("644"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testFileResource, "mode", "644"),
					checkFileMode(target, 0o644),
				),
			},
		},
	})
}

func TestAccFile_notRegularFile(t *testing.T) {
	requireRoot(t)

	target := filepath.Join(t.TempDir(), "subdir")
	if err := os.Mkdir(target, 0o755); err != nil {
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
}`, target),
				ExpectError: regexp.MustCompile(`not\s+a\s+regular\s+file`),
			},
		},
	})
}

// checkFileDestroyed verifies that every sysutils_file in the pre-destroy
// state no longer exists on disk.
func checkFileDestroyed(s *terraform.State) error {
	for _, rs := range s.RootModule().Resources {
		if rs.Type != "sysutils_file" {
			continue
		}
		p := rs.Primary.Attributes["path"]
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			return fmt.Errorf("file %s still exists after destroy (stat err = %v)", p, err)
		}
	}
	return nil
}

func checkFileContent(path, want string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		got, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if string(got) != want {
			return fmt.Errorf("content mismatch at %s: got %q want %q", path, string(got), want)
		}
		return nil
	}
}

func checkFileMode(path string, want os.FileMode) resource.TestCheckFunc {
	return func(*terraform.State) error {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if got := info.Mode().Perm(); got != want {
			return fmt.Errorf("mode mismatch at %s: got %#o want %#o", path, got, want)
		}
		return nil
	}
}

func checkFileUID(path, wantUID string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("stat_t unavailable on this platform")
		}
		got := strconv.FormatUint(uint64(st.Uid), 10)
		if got != wantUID {
			return fmt.Errorf("uid mismatch at %s: got %s want %s", path, got, wantUID)
		}
		return nil
	}
}
