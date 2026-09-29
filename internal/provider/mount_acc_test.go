package provider

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"golang.org/x/sys/unix"
)

// newMountAccEnv prepares a real mount point and a temporary fstab for
// acceptance tests that mount a tmpfs with mount(8). The real /etc/fstab is
// never touched. Anything a failed test leaves mounted is unmounted before
// the temporary directory is removed.
func newMountAccEnv(t *testing.T) mountTestEnv {
	t.Helper()
	if os.Getenv("TF_ACC") == "" {
		t.Skip("acceptance test; set TF_ACC=1 to run")
	}
	requireRoot(t)
	env := newMountTestEnv(t)
	// Registered after t.TempDir, so it runs before the directory is removed.
	t.Cleanup(func() {
		for unix.Unmount(env.mountPoint, 0) == nil {
		}
	})
	return env
}

func realMount(t *testing.T, target string) *mountEntry {
	t.Helper()
	e, err := lookupMount(systemMounter{mountInfo: defaultMountInfoPath}, target)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func checkRealMount(t *testing.T, target, source string, wantSuperOptions ...string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		e := realMount(t, target)
		if source == "" {
			if e != nil {
				return fmt.Errorf("%s (%s) is still mounted at %s", e.source, e.fstype, target)
			}
			return nil
		}
		if e == nil {
			return fmt.Errorf("nothing is mounted at %s", target)
		}
		if e.fstype != "tmpfs" || e.source != source {
			return fmt.Errorf("mounted at %s: %s (%s), want %s (tmpfs)", target, e.source, e.fstype, source)
		}
		for _, o := range wantSuperOptions {
			if !slices.Contains(e.superOptions, o) && !slices.Contains(e.options, o) {
				return fmt.Errorf("mount at %s has options %v / %v, missing %q", target, e.options, e.superOptions, o)
			}
		}
		return nil
	}
}

func runMountCommand(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
		t.Fatalf("%v: %v: %s", args, err, out)
	}
}

func TestAccMount_tmpfsLifecycle(t *testing.T) {
	env := newMountAccEnv(t)
	marker := filepath.Join(env.mountPoint, "marker")
	entry := func(options string) string {
		return testFstabPrelude + "tfacc-tmpfs " + env.mountPoint + " tmpfs " + options + " 0 0\n"
	}
	config := func(options string) string {
		return fmt.Sprintf(`
resource "sysutils_mount" "test" {
  path    = %q
  device  = "tfacc-tmpfs"
  fstype  = "tmpfs"
  options = %s
}
`, env.mountPoint, options)
	}
	updatePlanned := resource.ConfigPlanChecks{
		PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testMountResource, plancheck.ResourceActionUpdate)},
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: mountProviderFactories(env.fstab, nil),
		CheckDestroy: resource.ComposeTestCheckFunc(
			checkRealMount(t, env.mountPoint, ""),
			checkFstab(env, testFstabPrelude),
		),
		Steps: []resource.TestStep{
			{
				Config: config(`["size=1m", "mode=0750"]`),
				Check: resource.ComposeTestCheckFunc(
					checkRealMount(t, env.mountPoint, "tfacc-tmpfs", "size=1024k", "mode=750", "rw"),
					checkFstab(env, entry("size=1m,mode=0750")),
					func(*terraform.State) error { return os.WriteFile(marker, []byte("x"), 0o644) },
				),
			},
			// New options are applied by remounting, which keeps the contents.
			{
				Config:           config(`["size=2m", "mode=0750"]`),
				ConfigPlanChecks: updatePlanned,
				Check: resource.ComposeTestCheckFunc(
					checkRealMount(t, env.mountPoint, "tfacc-tmpfs", "size=2048k"),
					checkFstab(env, entry("size=2m,mode=0750")),
					func(*terraform.State) error {
						_, err := os.Stat(marker)
						return err
					},
				),
			},
			{
				Config:           config(`["size=2m", "ro"]`),
				ConfigPlanChecks: updatePlanned,
				Check:            checkRealMount(t, env.mountPoint, "tfacc-tmpfs", "ro"),
			},
			// Remounted writable out of band.
			{
				PreConfig:        func() { runMountCommand(t, "mount", "-o", "remount,rw", "--", env.mountPoint) },
				Config:           config(`["size=2m", "ro"]`),
				ConfigPlanChecks: updatePlanned,
				Check:            checkRealMount(t, env.mountPoint, "tfacc-tmpfs", "ro"),
			},
			// Unmounted out of band.
			{
				PreConfig:        func() { runMountCommand(t, "umount", "--", env.mountPoint) },
				Config:           config(`["size=2m"]`),
				ConfigPlanChecks: updatePlanned,
				Check:            checkRealMount(t, env.mountPoint, "tfacc-tmpfs", "size=2048k", "rw"),
			},
			// Another file system mounted on top out of band.
			{
				PreConfig:        func() { runMountCommand(t, "mount", "-t", "tmpfs", "--", "intruder", env.mountPoint) },
				Config:           config(`["size=2m"]`),
				ConfigPlanChecks: updatePlanned,
				Check:            checkRealMount(t, env.mountPoint, "tfacc-tmpfs", "size=2048k"),
			},
			// fstab entry edited out of band.
			{
				PreConfig: func() {
					if err := os.WriteFile(env.fstab, []byte(entry("size=5m")), 0o644); err != nil {
						t.Fatal(err)
					}
				},
				Config:           config(`["size=2m"]`),
				ConfigPlanChecks: updatePlanned,
				Check:            checkFstab(env, entry("size=2m")),
			},
			{
				ResourceName:      testMountResource,
				ImportState:       true,
				ImportStateId:     env.mountPoint,
				ImportStateVerify: true,
			},
		},
	})
}

func TestAccMount_fstabOnlyThenMounted(t *testing.T) {
	env := newMountAccEnv(t)
	config := func(mounted bool) string {
		return fmt.Sprintf(`
resource "sysutils_mount" "test" {
  path    = %q
  device  = "tfacc-tmpfs"
  fstype  = "tmpfs"
  options = ["size=1m", "nodev", "nosuid"]
  mounted = %t
}
`, env.mountPoint, mounted)
	}
	entry := testFstabPrelude + "tfacc-tmpfs " + env.mountPoint + " tmpfs size=1m,nodev,nosuid 0 0\n"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: mountProviderFactories(env.fstab, nil),
		CheckDestroy: resource.ComposeTestCheckFunc(
			checkRealMount(t, env.mountPoint, ""),
			checkFstab(env, testFstabPrelude),
		),
		Steps: []resource.TestStep{
			{
				Config: config(false),
				Check: resource.ComposeTestCheckFunc(
					checkRealMount(t, env.mountPoint, ""),
					checkFstab(env, entry),
				),
			},
			{
				Config: config(true),
				Check: resource.ComposeTestCheckFunc(
					checkRealMount(t, env.mountPoint, "tfacc-tmpfs", "nodev", "nosuid", "size=1024k"),
					checkFstab(env, entry),
				),
			},
			{
				Config: config(false),
				Check: resource.ComposeTestCheckFunc(
					checkRealMount(t, env.mountPoint, ""),
					checkFstab(env, entry),
				),
			},
		},
	})
}

// A mount made outside Terraform, without an fstab entry, can be imported.
func TestAccMount_importLiveMount(t *testing.T) {
	env := newMountAccEnv(t)
	runMountCommand(t, "mount", "-t", "tmpfs", "-o", "size=1m", "--", "tfacc-tmpfs", env.mountPoint)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: mountProviderFactories(env.fstab, nil),
		CheckDestroy: resource.ComposeTestCheckFunc(
			checkRealMount(t, env.mountPoint, ""),
			checkFstab(env, testFstabPrelude),
		),
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "sysutils_mount" "test" {
  path    = %q
  device  = "tfacc-tmpfs"
  fstype  = "tmpfs"
  persist = false
}
`, env.mountPoint),
				ResourceName:       testMountResource,
				ImportState:        true,
				ImportStateId:      env.mountPoint,
				ImportStatePersist: true,
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					a := states[0].Attributes
					want := map[string]string{"device": "tfacc-tmpfs", "fstype": "tmpfs", "options.#": "1", "options.0": "defaults",
						"persist": "false", "mounted": "true"}
					for k, v := range want {
						if a[k] != v {
							return fmt.Errorf("%s = %q, want %q", k, a[k], v)
						}
					}
					return nil
				},
			},
			{
				Config: fmt.Sprintf(`
resource "sysutils_mount" "test" {
  path    = %q
  device  = "tfacc-tmpfs"
  fstype  = "tmpfs"
  persist = false
}
`, env.mountPoint),
				PlanOnly: true,
			},
		},
	})
}
