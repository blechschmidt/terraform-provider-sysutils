package provider

import (
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

const testMountResource = "sysutils_mount.test"

func mountConfigHCL(mountPoint, extra string) string {
	return fmt.Sprintf(`
resource "sysutils_mount" "test" {
  path   = %q
  device = "tmpfs"
  fstype = "tmpfs"
%s
}
`, mountPoint, extra)
}

func checkFstab(env mountTestEnv, want string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		data, err := os.ReadFile(env.fstab)
		if err != nil {
			return err
		}
		if string(data) != want {
			return fmt.Errorf("fstab is\n%s\nwant\n%s", data, want)
		}
		return nil
	}
}

func checkFakeMount(f *fakeMounter, target, source, fstype string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		e := f.top(target)
		switch {
		case e == nil && source == "":
			return nil
		case e == nil:
			return fmt.Errorf("nothing mounted at %s", target)
		case source == "":
			return fmt.Errorf("%s (%s) still mounted at %s", e.source, e.fstype, target)
		case e.source != source || e.fstype != fstype:
			return fmt.Errorf("mounted at %s: %s (%s), want %s (%s)", target, e.source, e.fstype, source, fstype)
		}
		return nil
	}
}

func TestMountResource_lifecycle(t *testing.T) {
	env := newMountTestEnv(t)
	f := newFakeMounter()

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: mountProviderFactories(env.fstab, f),
		CheckDestroy: resource.ComposeTestCheckFunc(
			checkFakeMount(f, env.mountPoint, "", ""),
			checkFstab(env, testFstabPrelude),
		),
		Steps: []resource.TestStep{
			{
				Config: mountConfigHCL(env.mountPoint, ""),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(testMountResource, tfjsonpath.New("options"), knownvalue.ListExact([]knownvalue.Check{knownvalue.StringExact("defaults")})),
					statecheck.ExpectKnownValue(testMountResource, tfjsonpath.New("persist"), knownvalue.Bool(true)),
					statecheck.ExpectKnownValue(testMountResource, tfjsonpath.New("mounted"), knownvalue.Bool(true)),
					statecheck.ExpectKnownValue(testMountResource, tfjsonpath.New("dump"), knownvalue.Int64Exact(0)),
					statecheck.ExpectKnownValue(testMountResource, tfjsonpath.New("id"), knownvalue.StringExact(env.mountPoint)),
				},
				Check: resource.ComposeTestCheckFunc(
					checkFakeMount(f, env.mountPoint, "tmpfs", "tmpfs"),
					checkFstab(env, testFstabPrelude+"tmpfs "+env.mountPoint+" tmpfs defaults 0 0\n"),
				),
			},
			// Changing options remounts and rewrites the entry in place.
			{
				Config: mountConfigHCL(env.mountPoint, `options = ["size=1m", "ro"]
  dump = 1
  pass = 2`),
				Check: resource.ComposeTestCheckFunc(
					checkFstab(env, testFstabPrelude+"tmpfs "+env.mountPoint+" tmpfs size=1m,ro 1 2\n"),
					func(*terraform.State) error {
						if n := f.callCount("remount"); n != 1 {
							return fmt.Errorf("remounted %d times, want 1", n)
						}
						if n := f.callCount("mount "); n != 1 {
							return fmt.Errorf("mounted %d times, want 1", n)
						}
						if e := f.top(env.mountPoint); e == nil || !e.readOnly() {
							return fmt.Errorf("mount is not read-only: %+v", e)
						}
						return nil
					},
				),
			},
			// Changing only dump rewrites fstab but leaves the mount alone.
			{
				Config: mountConfigHCL(env.mountPoint, `options = ["size=1m", "ro"]
  pass = 2`),
				Check: resource.ComposeTestCheckFunc(
					checkFstab(env, testFstabPrelude+"tmpfs "+env.mountPoint+" tmpfs size=1m,ro 0 2\n"),
					func(*terraform.State) error {
						if n := f.callCount("remount"); n != 1 {
							return fmt.Errorf("remounted %d times, want 1", n)
						}
						return nil
					},
				),
			},
			// A new device is mounted in place of the old one.
			{
				Config: `
resource "sysutils_mount" "test" {
  path   = "` + env.mountPoint + `"
  device = "scratch"
  fstype = "tmpfs"
}
`,
				Check: resource.ComposeTestCheckFunc(
					checkFakeMount(f, env.mountPoint, "scratch", "tmpfs"),
					checkFstab(env, testFstabPrelude+"scratch "+env.mountPoint+" tmpfs defaults 0 0\n"),
				),
			},
			// persist = false removes the fstab entry but keeps the mount.
			{
				Config: mountConfigHCL(env.mountPoint, "persist = false"),
				Check: resource.ComposeTestCheckFunc(
					checkFakeMount(f, env.mountPoint, "tmpfs", "tmpfs"),
					checkFstab(env, testFstabPrelude),
				),
			},
			// mounted = false unmounts but keeps the fstab entry.
			{
				Config: mountConfigHCL(env.mountPoint, "mounted = false"),
				Check: resource.ComposeTestCheckFunc(
					checkFakeMount(f, env.mountPoint, "", ""),
					checkFstab(env, testFstabPrelude+"tmpfs "+env.mountPoint+" tmpfs defaults 0 0\n"),
				),
			},
			{
				Config: mountConfigHCL(env.mountPoint, ""),
				Check:  checkFakeMount(f, env.mountPoint, "tmpfs", "tmpfs"),
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

func TestMountResource_drift(t *testing.T) {
	env := newMountTestEnv(t)
	f := newFakeMounter()
	line := "tmpfs " + env.mountPoint + " tmpfs size=1m 0 0\n"
	config := mountConfigHCL(env.mountPoint, `options = ["size=1m"]`)
	editFstabFile := func(from, to string) func() {
		return func() {
			data := env.readFstab(t)
			if !strings.Contains(data, from) {
				t.Fatalf("fstab does not contain %q:\n%s", from, data)
			}
			if err := os.WriteFile(env.fstab, []byte(strings.Replace(data, from, to, 1)), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: mountProviderFactories(env.fstab, f),
		Steps: []resource.TestStep{
			{Config: config, Check: checkFstab(env, testFstabPrelude+line)},
			// Options edited in fstab.
			{
				PreConfig: editFstabFile("size=1m 0 0", "size=9m,noexec 0 0"),
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testMountResource, plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue(testMountResource, tfjsonpath.New("options"), knownvalue.ListExact([]knownvalue.Check{knownvalue.StringExact("size=1m")})),
					},
				},
				Check: checkFstab(env, testFstabPrelude+line),
			},
			// fstab entry deleted, then fs unmounted.
			{
				PreConfig: editFstabFile(line, ""),
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testMountResource, plancheck.ResourceActionUpdate)},
				},
				Check: checkFstab(env, testFstabPrelude+line),
			},
			{
				PreConfig: func() { f.unmountOutOfBand(env.mountPoint) },
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testMountResource, plancheck.ResourceActionUpdate)},
				},
				Check: checkFakeMount(f, env.mountPoint, "tmpfs", "tmpfs"),
			},
			// A different file system mounted on top is replaced.
			{
				PreConfig: func() { f.mountOutOfBand("/dev/other", env.mountPoint, "ext4") },
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testMountResource, plancheck.ResourceActionUpdate)},
				},
				// Unmounting the intruder uncovers the managed tmpfs, which
				// must not be mounted a second time.
				Check: resource.ComposeTestCheckFunc(
					checkFakeMount(f, env.mountPoint, "tmpfs", "tmpfs"),
					func(*terraform.State) error {
						if n := f.depth(env.mountPoint); n != 1 {
							return fmt.Errorf("%d file systems stacked at %s, want 1", n, env.mountPoint)
						}
						return nil
					},
				),
			},
			// Remounted writable although "ro" is configured.
			{
				Config: mountConfigHCL(env.mountPoint, `options = ["ro"]`),
				Check:  func(*terraform.State) error { return nil },
			},
			{
				PreConfig: func() {
					if err := f.remount(t.Context(), "tmpfs", env.mountPoint, []string{"rw"}); err != nil {
						t.Fatal(err)
					}
				},
				Config: mountConfigHCL(env.mountPoint, `options = ["ro"]`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testMountResource, plancheck.ResourceActionUpdate)},
				},
				Check: func(*terraform.State) error {
					if e := f.top(env.mountPoint); e == nil || !e.readOnly() {
						return fmt.Errorf("mount is not read-only: %+v", e)
					}
					return nil
				},
			},
			{
				Config:             mountConfigHCL(env.mountPoint, `options = ["ro"]`),
				PlanOnly:           true,
				ExpectNonEmptyPlan: false,
			},
		},
	})
}

func TestMountResource_refusesExisting(t *testing.T) {
	env := newMountTestEnv(t)
	f := newFakeMounter()
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: mountProviderFactories(env.fstab, f),
		Steps: []resource.TestStep{
			{
				PreConfig:   func() { f.mountOutOfBand("/dev/sdz1", env.mountPoint, "ext4") },
				Config:      mountConfigHCL(env.mountPoint, ""),
				ExpectError: regexp.MustCompile(`(?s)Already\s+mounted.*/dev/sdz1\s+\(type\s+ext4\)\s+is\s+already\s+mounted`),
			},
			{
				PreConfig: func() {
					f.unmountOutOfBand(env.mountPoint)
					if err := os.WriteFile(env.fstab, []byte(testFstabPrelude+"/dev/sdz1 "+env.mountPoint+" ext4 defaults 0 2\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				},
				Config:      mountConfigHCL(env.mountPoint, ""),
				ExpectError: regexp.MustCompile(`(?s)Mount\s+point\s+already\s+in\s+fstab.*terraform\s+import`),
			},
		},
	})
	if n := f.callCount("mount "); n != 0 {
		t.Errorf("mounted %d times after refusing", n)
	}
}

func TestMountResource_importFromFstabOnly(t *testing.T) {
	env := newMountTestEnv(t)
	f := newFakeMounter()
	fstab := testFstabPrelude + "LABEL=data " + env.mountPoint + "/ ext4 noatime,nodev 0 2\n"
	if err := os.WriteFile(env.fstab, []byte(fstab), 0o644); err != nil {
		t.Fatal(err)
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: mountProviderFactories(env.fstab, f),
		Steps: []resource.TestStep{
			{
				Config: `
resource "sysutils_mount" "test" {
  path    = "` + env.mountPoint + `"
  device  = "LABEL=data"
  fstype  = "ext4"
  options = ["noatime", "nodev"]
  pass    = 2
  mounted = false
}
`,
				ResourceName:       testMountResource,
				ImportState:        true,
				ImportStateId:      env.mountPoint,
				ImportStatePersist: true,
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					a := states[0].Attributes
					want := map[string]string{"device": "LABEL=data", "fstype": "ext4", "options.#": "2", "options.0": "noatime", "options.1": "nodev",
						"dump": "0", "pass": "2", "persist": "true", "mounted": "false", "id": env.mountPoint}
					for k, v := range want {
						if a[k] != v {
							return fmt.Errorf("%s = %q, want %q", k, a[k], v)
						}
					}
					return nil
				},
			},
			// The imported state matches the configuration.
			{
				Config: `
resource "sysutils_mount" "test" {
  path    = "` + env.mountPoint + `"
  device  = "LABEL=data"
  fstype  = "ext4"
  options = ["noatime", "nodev"]
  pass    = 2
  mounted = false
}
`,
				PlanOnly: true,
			},
		},
	})
	// Destroy removed only the entry for the mount point.
	if got := env.readFstab(t); got != testFstabPrelude {
		t.Errorf("fstab after destroy:\n%s", got)
	}
}

func TestMountResource_importNothing(t *testing.T) {
	env := newMountTestEnv(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: mountProviderFactories(env.fstab, newFakeMounter()),
		Steps: []resource.TestStep{
			{
				Config:        mountConfigHCL(env.mountPoint, ""),
				ResourceName:  testMountResource,
				ImportState:   true,
				ImportStateId: env.mountPoint,
				ExpectError:   regexp.MustCompile(`Nothing\s+is\s+mounted\s+at`),
			},
			{
				Config:        mountConfigHCL(env.mountPoint, ""),
				ResourceName:  testMountResource,
				ImportState:   true,
				ImportStateId: "relative/path",
				ExpectError:   regexp.MustCompile(`Invalid\s+import\s+ID`),
			},
		},
	})
}

func TestMountResource_errors(t *testing.T) {
	env := newMountTestEnv(t)
	f := newFakeMounter()
	missing := filepath.Join(env.dir, "missing")
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: mountProviderFactories(env.fstab, f),
		Steps: []resource.TestStep{
			{
				Config:      mountConfigHCL(env.mountPoint, "persist = false\n  mounted = false"),
				ExpectError: regexp.MustCompile(`At\s+least\s+one\s+of\s+persist\s+and\s+mounted\s+must\s+be\s+true`),
			},
			{
				Config:      mountConfigHCL(env.mountPoint, `options = ["size=1m,mode=0755"]`),
				ExpectError: regexp.MustCompile(`contains\s+a\s+comma`),
			},
			{
				Config:      mountConfigHCL(env.mountPoint, `options = []`),
				ExpectError: regexp.MustCompile(`at\s+least\s+1`),
			},
			{
				Config:      mountConfigHCL(env.mountPoint+"/", ""),
				ExpectError: regexp.MustCompile(`Invalid\s+Attribute\s+Value|canonical`),
			},
			{
				Config:      mountConfigHCL(missing, ""),
				ExpectError: regexp.MustCompile(`does\s+not\s+exist;\s+create\s+it\s+first`),
			},
			// A failed mount leaves no fstab entry behind.
			{
				Config: `
resource "sysutils_mount" "test" {
  path   = "` + env.mountPoint + `"
  device = "tmpfs"
  fstype = "bogus"
}
`,
				ExpectError: regexp.MustCompile(`unknown\s+filesystem\s+type`),
				Check:       checkFstab(env, testFstabPrelude),
			},
		},
	})
	if got := env.readFstab(t); got != testFstabPrelude {
		t.Errorf("fstab after failed mounts:\n%s", got)
	}
}

func TestMountResource_busyOnDestroy(t *testing.T) {
	env := newMountTestEnv(t)
	f := newFakeMounter()
	line := "tmpfs " + env.mountPoint + " tmpfs defaults 0 0\n"
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: mountProviderFactories(env.fstab, f),
		Steps: []resource.TestStep{
			{Config: mountConfigHCL(env.mountPoint, "")},
			{
				PreConfig:   func() { f.setBusy(env.mountPoint, true) },
				Config:      mountConfigHCL(env.mountPoint, ""),
				Destroy:     true,
				ExpectError: regexp.MustCompile(`target\s+is\s+busy`),
				// The failed unmount keeps the fstab entry.
				Check: checkFstab(env, testFstabPrelude+line),
			},
			{
				PreConfig: func() {
					if got := env.readFstab(t); got != testFstabPrelude+line {
						t.Errorf("fstab after failed destroy:\n%s", got)
					}
					f.setBusy(env.mountPoint, false)
				},
				Config: mountConfigHCL(env.mountPoint, ""),
			},
		},
	})
}
