package provider

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

const testPackageResource = "sysutils_package.test"

func packageHCL(name, extra string) string {
	return fmt.Sprintf(`
resource "sysutils_package" "test" {
  name = %q
%s
}
`, name, extra)
}

// checkFakePackage checks the version of name in f; "" means not installed.
func checkFakePackage(f *fakePackageManager, name, want string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		got, ok := f.version(name)
		switch {
		case want == "" && ok:
			return fmt.Errorf("%s %s is installed, want it absent", name, got)
		case want != "" && !ok:
			return fmt.Errorf("%s is not installed, want %s", name, want)
		case got != want:
			return fmt.Errorf("%s %s is installed, want %s", name, got, want)
		}
		return nil
	}
}

// checkPackageChanges checks the changing calls made so far.
func checkPackageChanges(f *fakePackageManager, want ...string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		if got := f.changes(); !slices.Equal(got, want) {
			return fmt.Errorf("package manager calls:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
		}
		return nil
	}
}

func expectInstalledVersion(v string) statecheck.StateCheck {
	if v == "" {
		return statecheck.ExpectKnownValue(testPackageResource, tfjsonpath.New("installed_version"), knownvalue.Null())
	}
	return statecheck.ExpectKnownValue(testPackageResource, tfjsonpath.New("installed_version"), knownvalue.StringExact(v))
}

func TestPackageResource_lifecycle(t *testing.T) {
	f := newFakePackageManager(packageManagerApt)
	f.available["hello"] = []string{"2.10-1", "2.10-2"}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: packageProviderFactories(f),
		CheckDestroy:             checkFakePackage(f, "hello", ""),
		Steps: []resource.TestStep{
			{
				Config: packageHCL("hello", ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectUnknownValue(testPackageResource, tfjsonpath.New("installed_version")),
				}},
				ConfigStateChecks: []statecheck.StateCheck{
					expectInstalledVersion("2.10-2"),
					statecheck.ExpectKnownValue(testPackageResource, tfjsonpath.New("state"), knownvalue.StringExact("present")),
					statecheck.ExpectKnownValue(testPackageResource, tfjsonpath.New("manager"), knownvalue.StringExact("auto")),
					statecheck.ExpectKnownValue(testPackageResource, tfjsonpath.New("update_cache"), knownvalue.Bool(false)),
					statecheck.ExpectKnownValue(testPackageResource, tfjsonpath.New("remove_on_destroy"), knownvalue.Bool(true)),
					statecheck.ExpectKnownValue(testPackageResource, tfjsonpath.New("version"), knownvalue.Null()),
					statecheck.ExpectKnownValue(testPackageResource, tfjsonpath.New("id"), knownvalue.StringExact("hello")),
				},
				Check: resource.ComposeTestCheckFunc(
					checkFakePackage(f, "hello", "2.10-2"),
					checkPackageChanges(f, "install hello "),
				),
			},
			// Pinning an older version downgrades in place, and the plan
			// already knows the resulting installed_version.
			{
				Config: packageHCL("hello", `version = "2.10-1"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(testPackageResource, plancheck.ResourceActionUpdate),
					plancheck.ExpectKnownValue(testPackageResource, tfjsonpath.New("installed_version"), knownvalue.StringExact("2.10-1")),
				}},
				ConfigStateChecks: []statecheck.StateCheck{expectInstalledVersion("2.10-1")},
				Check: resource.ComposeTestCheckFunc(
					checkFakePackage(f, "hello", "2.10-1"),
					checkPackageChanges(f, "install hello ", "install hello 2.10-1"),
				),
			},
			// Dropping the pin leaves the installed version alone.
			{
				Config: packageHCL("hello", ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectKnownValue(testPackageResource, tfjsonpath.New("installed_version"), knownvalue.StringExact("2.10-1")),
				}},
				Check: checkPackageChanges(f, "install hello ", "install hello 2.10-1"),
			},
			{
				Config:            packageHCL("hello", `state = "latest"`),
				ConfigStateChecks: []statecheck.StateCheck{expectInstalledVersion("2.10-2")},
				Check: resource.ComposeTestCheckFunc(
					checkFakePackage(f, "hello", "2.10-2"),
					checkPackageChanges(f, "install hello ", "install hello 2.10-1", "upgrade hello"),
				),
			},
			{
				Config: packageHCL("hello", `state = "absent"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectKnownValue(testPackageResource, tfjsonpath.New("installed_version"), knownvalue.Null()),
				}},
				ConfigStateChecks: []statecheck.StateCheck{expectInstalledVersion("")},
				Check: resource.ComposeTestCheckFunc(
					checkFakePackage(f, "hello", ""),
					checkPackageChanges(f, "install hello ", "install hello 2.10-1", "upgrade hello", "remove hello"),
				),
			},
			// Destroying an absent package does nothing.
			{
				Config:  packageHCL("hello", `state = "absent"`),
				Destroy: true,
				Check:   checkPackageChanges(f, "install hello ", "install hello 2.10-1", "upgrade hello", "remove hello"),
			},
			{
				Config: packageHCL("hello", `state = "present"`),
				Check:  checkFakePackage(f, "hello", "2.10-2"),
			},
		},
	})
}

func TestPackageResource_drift(t *testing.T) {
	f := newFakePackageManager(packageManagerApt)
	f.available["hello"] = []string{"2.10-1", "2.10-2"}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: packageProviderFactories(f),
		CheckDestroy:             checkFakePackage(f, "hello", ""),
		Steps: []resource.TestStep{
			{
				Config: packageHCL("hello", `version = "2.10-1"`),
				Check:  checkFakePackage(f, "hello", "2.10-1"),
			},
			// Upgraded outside Terraform: the plan shows the version
			// changing back, and apply downgrades.
			{
				PreConfig: func() { f.set("hello", "2.10-2") },
				Config:    packageHCL("hello", `version = "2.10-1"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(testPackageResource, plancheck.ResourceActionUpdate),
				}},
				Check: checkFakePackage(f, "hello", "2.10-1"),
			},
			// Removed outside Terraform: the plan shows state changing from
			// absent to present, and apply reinstalls.
			{
				PreConfig: func() { f.set("hello", "") },
				Config:    packageHCL("hello", `version = "2.10-1"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(testPackageResource, plancheck.ResourceActionUpdate),
				}},
				Check: checkFakePackage(f, "hello", "2.10-1"),
			},
			{
				Config: packageHCL("hello", `state = "latest"`),
				Check:  checkFakePackage(f, "hello", "2.10-2"),
			},
			// A newer version in the index turns "latest" into "present"
			// on refresh, and apply upgrades.
			{
				PreConfig: func() {
					f.mu.Lock()
					f.available["hello"] = append(f.available["hello"], "2.10-3")
					f.mu.Unlock()
				},
				Config: packageHCL("hello", `state = "latest"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(testPackageResource, plancheck.ResourceActionUpdate),
					plancheck.ExpectUnknownValue(testPackageResource, tfjsonpath.New("installed_version")),
				}},
				ConfigStateChecks: []statecheck.StateCheck{expectInstalledVersion("2.10-3")},
				Check:             checkFakePackage(f, "hello", "2.10-3"),
			},
			// Installed outside Terraform although it should be absent.
			{
				Config: packageHCL("hello", `state = "absent"`),
				Check:  checkFakePackage(f, "hello", ""),
			},
			{
				PreConfig: func() { f.set("hello", "2.10-1") },
				Config:    packageHCL("hello", `state = "absent"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(testPackageResource, plancheck.ResourceActionUpdate),
				}},
				Check: checkFakePackage(f, "hello", ""),
			},
		},
	})
}

// A version that differs only by an explicit zero epoch is the same version
// and causes no diff.
func TestPackageResource_zeroEpoch(t *testing.T) {
	f := newFakePackageManager(packageManagerDnf)
	f.available["tree"] = []string{"0:2.2.1-1.fc42"}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: packageProviderFactories(f),
		Steps: []resource.TestStep{
			{
				PreConfig:         func() { f.set("tree", "0:2.2.1-1.fc42") },
				Config:            packageHCL("tree", `version = "2.2.1-1.fc42"`),
				ConfigStateChecks: []statecheck.StateCheck{expectInstalledVersion("2.2.1-1.fc42")},
				Check:             checkPackageChanges(f),
			},
		},
	})
}

func TestPackageResource_import(t *testing.T) {
	f := newFakePackageManager(packageManagerApk)
	f.available["curl"] = []string{"8.12.1-r0"}
	f.set("curl", "8.12.1-r0")

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: packageProviderFactories(f),
		Steps: []resource.TestStep{
			{
				Config:        packageHCL("curl", ""),
				ResourceName:  testPackageResource,
				ImportState:   true,
				ImportStateId: "curl",
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if len(states) != 1 {
						return fmt.Errorf("imported %d resources, want 1", len(states))
					}
					want := map[string]string{
						"name": "curl", "id": "curl", "state": "present", "manager": "auto",
						"update_cache": "false", "remove_on_destroy": "true", "installed_version": "8.12.1-r0",
					}
					for k, v := range want {
						if got := states[0].Attributes[k]; got != v {
							return fmt.Errorf("%s = %q, want %q", k, got, v)
						}
					}
					if _, ok := states[0].Attributes["version"]; ok {
						return fmt.Errorf("version = %q, want null", states[0].Attributes["version"])
					}
					return nil
				},
				ImportStatePersist: true,
			},
			// After import, the configuration matches and nothing is
			// installed again.
			{
				Config: packageHCL("curl", ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectEmptyPlan(),
				}},
				Check: checkPackageChanges(f),
			},
			{
				Config:        packageHCL("curl", `remove_on_destroy = false`),
				ResourceName:  testPackageResource,
				ImportState:   true,
				ImportStateId: "wget",
				ExpectError:   regexp.MustCompile(`Package\s+wget\s+is\s+not\s+installed\s+according\s+to\s+apk`),
			},
			{
				Config:        packageHCL("curl", `remove_on_destroy = false`),
				ResourceName:  testPackageResource,
				ImportState:   true,
				ImportStateId: "curl;id",
				ExpectError:   regexp.MustCompile(`Import\s+ID\s+must\s+be\s+a\s+package\s+name`),
			},
		},
	})
}

// A package that is already installed is adopted without running the
// package manager.
func TestPackageResource_adoptsInstalled(t *testing.T) {
	f := newFakePackageManager(packageManagerApt)
	f.available["vim"] = []string{"9.1-1"}
	f.set("vim", "9.1-1")

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: packageProviderFactories(f),
		// remove_on_destroy = false keeps the package.
		CheckDestroy: resource.ComposeTestCheckFunc(
			checkFakePackage(f, "vim", "9.1-1"),
			checkPackageChanges(f),
		),
		Steps: []resource.TestStep{
			{
				Config:            packageHCL("vim", `remove_on_destroy = false`),
				ConfigStateChecks: []statecheck.StateCheck{expectInstalledVersion("9.1-1")},
				Check:             checkPackageChanges(f),
			},
		},
	})
}

// update_cache refreshes the index once per provider run, however many
// resources set it, and only when something is to be installed.
func TestPackageResource_updateCache(t *testing.T) {
	f := newFakePackageManager(packageManagerApt)
	f.stale["hello"] = []string{"2.10-1"}
	f.stale["tree"] = []string{"2.1.1-2"}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: packageProviderFactories(f),
		Steps: []resource.TestStep{
			{
				Config:      packageHCL("hello", ""),
				ExpectError: regexp.MustCompile(`Unable\s+to\s+locate\s+package\s+hello`),
			},
			{
				Config: `
resource "sysutils_package" "hello" {
  name         = "hello"
  update_cache = true
}

resource "sysutils_package" "tree" {
  name         = "tree"
  update_cache = true
}
`,
				Check: resource.ComposeTestCheckFunc(
					checkFakePackage(f, "hello", "2.10-1"),
					checkFakePackage(f, "tree", "2.1.1-2"),
					func(*terraform.State) error {
						n := 0
						for _, c := range f.changes() {
							if c == "update" {
								n++
							}
						}
						if n != 1 {
							return fmt.Errorf("index updated %d times, want 1: %v", n, f.changes())
						}
						return nil
					},
				),
			},
		},
	})
}

// Package managers can succeed without doing what was asked; the result is
// verified against the package database.
func TestPackageResource_verifiesResult(t *testing.T) {
	f := newFakePackageManager(packageManagerApt)
	f.available["vim"] = []string{"9.1-1"}
	f.provides["editor"] = "vim"
	f.available["hello"] = []string{"2.10-1", "2.10-2"}
	f.set("hello", "2.10-1")
	f.held["hello"] = true

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: packageProviderFactories(f),
		Steps: []resource.TestStep{
			{
				Config:      packageHCL("editor", ""),
				ExpectError: regexp.MustCompile(`(?s)does\s+not\s+list\s+editor\s+as\s+installed.*virtual\s+package`),
			},
			{
				Config: packageHCL("hello", `version = "2.10-2"
  remove_on_destroy = false`),
				ExpectError: regexp.MustCompile(`hello\s+2\.10-1\s+is\s+installed\s+instead\s+of\s+2\.10-2`),
			},
			{
				Config: packageHCL("hello", `state = "latest"
  remove_on_destroy = false`),
				ExpectError: regexp.MustCompile(`a\s+newer\s+version\s+of\s+hello\s+than\s+2\.10-1\s+is\s+still\s+available`),
			},
			{
				Config:      packageHCL("hello", `state = "absent"`),
				ExpectError: regexp.MustCompile(`hello\s+2\.10-1\s+is\s+still\s+installed`),
			},
			// Keep the held package when the failed resource is destroyed.
			{
				Config: packageHCL("hello", `state = "absent"`),
				PreConfig: func() {
					f.mu.Lock()
					delete(f.held, "hello")
					f.mu.Unlock()
				},
				Check: checkFakePackage(f, "hello", ""),
			},
		},
	})
}

func TestPackageResource_failedDestroy(t *testing.T) {
	f := newFakePackageManager(packageManagerApt)
	f.available["essential"] = []string{"1.0"}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: packageProviderFactories(f),
		Steps: []resource.TestStep{
			{
				Config: packageHCL("essential", ""),
				Check:  checkFakePackage(f, "essential", "1.0"),
			},
			{
				Config:      packageHCL("essential", ""),
				Destroy:     true,
				ExpectError: regexp.MustCompile(`Removing\s+essential\s+system-critical\s+packages\s+is\s+not\s+permitted`),
			},
			{
				Config: packageHCL("essential", `remove_on_destroy = false`),
			},
		},
	})
}

func TestPackageResource_validation(t *testing.T) {
	f := newFakePackageManager(packageManagerApt)
	f.available["hello"] = []string{"2.10-1"}

	for name, tc := range map[string]struct {
		hcl  string
		want string
	}{
		"version with latest": {packageHCL("hello", `
  version = "2.10-1"
  state   = "latest"`), `version\s+can\s+only\s+be\s+set\s+with\s+state\s+=\s+"present",\s+not\s+"latest"`},
		"version with absent": {packageHCL("hello", `
  version = "2.10-1"
  state   = "absent"`), `version\s+can\s+only\s+be\s+set\s+with\s+state`},
		"bad state":           {packageHCL("hello", `state = "installed"`), `value\s+must\s+be\s+one\s+of`},
		"bad manager":         {packageHCL("hello", `manager = "pacman"`), `value\s+must\s+be\s+one\s+of`},
		"shell in name":       {packageHCL("hello;reboot", ""), `Invalid\s+package\s+name`},
		"option as name":      {packageHCL("-y", ""), `Invalid\s+package\s+name`},
		"glob in name":        {packageHCL("hel*", ""), `Invalid\s+package\s+name`},
		"arch in name":        {packageHCL("hello:i386", ""), `Invalid\s+package\s+name`},
		"path as name":        {packageHCL("/usr/bin/hello", ""), `Invalid\s+package\s+name`},
		"space in version":    {packageHCL("hello", `version = "1.0 -o"`), `Invalid\s+package\s+version`},
		"constraint version":  {packageHCL("hello", `version = ">=1.0"`), `Invalid\s+package\s+version`},
		"option as version":   {packageHCL("hello", `version = "-1"`), `Invalid\s+package\s+version`},
		"apt uppercase name":  {packageHCL("Hello", `manager = "apt"`), `not\s+a\s+valid\s+Debian\s+package\s+name`},
		"apt version":         {packageHCL("hello", `manager = "apt"`+"\n"+`version = "v1"`), `not\s+a\s+valid\s+Debian\s+version`},
		"dnf without release": {packageHCL("hello", `manager = "dnf"`+"\n"+`version = "2.12"`), `the\s+release\s+is\s+required`},
		"apk without release": {packageHCL("hello", `manager = "apk"`+"\n"+`version = "2.12"`), `not\s+a\s+valid\s+Alpine\s+package\s+version`},
		// With manager = "auto", the stricter rules apply once the
		// manager is known.
		"auto uppercase name": {packageHCL("Hello", ""), `not\s+a\s+valid\s+Debian\s+package\s+name`},
		"manager unavailable": {packageHCL("hello", `manager = "dnf"`), `(?i)package\s+manager\s+dnf\s+is\s+not\s+available`},
	} {
		t.Run(name, func(t *testing.T) {
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: packageProviderFactories(f),
				Steps: []resource.TestStep{{
					Config:      tc.hcl,
					ExpectError: regexp.MustCompile(tc.want),
				}},
			})
		})
	}
	if c := f.changes(); len(c) > 0 {
		t.Errorf("invalid configurations ran the package manager: %v", c)
	}
}

func TestPackageResource_rootDirRefused(t *testing.T) {
	f := newFakePackageManager(packageManagerApt)
	f.available["hello"] = []string{"2.10-1"}
	root := t.TempDir()

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: packageProviderFactories(f),
		Steps: []resource.TestStep{{
			Config: fmt.Sprintf(`
provider "sysutils" {
  root_dir = %q
}
`, root) + packageHCL("hello", ""),
			ExpectError: regexp.MustCompile(`Not\s+supported\s+with\s+root_dir`),
		}},
	})
	if c := f.changes(); len(c) > 0 {
		t.Errorf("package manager ran with root_dir set: %v", c)
	}
}
