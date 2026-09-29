package provider

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

const testKernelModuleResource = "sysutils_kernel_module.test"

// moduleTestEnv holds temporary modules-load.d and modprobe.d directories.
type moduleTestEnv struct {
	loadDir, optionsDir string
}

func newModuleTestEnv(t *testing.T) moduleTestEnv {
	t.Helper()
	dir := t.TempDir()
	return moduleTestEnv{
		loadDir:    filepath.Join(dir, "modules-load.d"),
		optionsDir: filepath.Join(dir, "modprobe.d"),
	}
}

func (e moduleTestEnv) loadFile(name string) string { return filepath.Join(e.loadDir, name+".conf") }
func (e moduleTestEnv) optionsFile(name string) string {
	return filepath.Join(e.optionsDir, name+".conf")
}

// kernelModuleProviderFactories returns provider factories whose
// sysutils_kernel_module uses env's directories and l. A nil l selects the
// real modprobe.
func kernelModuleProviderFactories(env moduleTestEnv, l moduleLoader) map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		"sysutils": providerserver.NewProtocol6WithError(&sysutilsProvider{
			version:      "test",
			kernelModule: &kernelModuleConfig{modulesLoadDir: env.loadDir, modprobeDir: env.optionsDir, loader: l},
		}),
	}
}

func kernelModuleHCL(name, extra string) string {
	return fmt.Sprintf(`
resource "sysutils_kernel_module" "test" {
  name = %q
%s
}
`, name, extra)
}

func checkFakeModule(f *fakeModuleLoader, name string, wantLoaded bool, wantParams ...string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		params, loaded := f.params(name)
		switch {
		case loaded != wantLoaded:
			return fmt.Errorf("module %s loaded = %v, want %v", name, loaded, wantLoaded)
		case loaded && !slices.Equal(params, wantParams):
			return fmt.Errorf("module %s loaded with %q, want %q", name, params, wantParams)
		}
		return nil
	}
}

func checkCallCount(f *fakeModuleLoader, prefix string, want int) resource.TestCheckFunc {
	return func(*terraform.State) error {
		if n := f.callCount(prefix); n != want {
			return fmt.Errorf("%d calls starting with %q, want %d (calls: %q)", n, prefix, want, f.calls)
		}
		return nil
	}
}

func TestKernelModuleResource_lifecycle(t *testing.T) {
	env := newModuleTestEnv(t)
	f := newFakeModuleLoader()
	const name = "dummy"
	loadConf := moduleConfHeader + "\ndummy\n"

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: kernelModuleProviderFactories(env, f),
		CheckDestroy: resource.ComposeTestCheckFunc(
			checkFakeModule(f, name, false),
			checkFileText(env.loadFile(name), ""),
			checkFileText(env.optionsFile(name), ""),
		),
		Steps: []resource.TestStep{
			{
				Config: kernelModuleHCL(name, `parameters = { numdummies = "2" }`),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(testKernelModuleResource, tfjsonpath.New("persist"), knownvalue.Bool(true)),
					statecheck.ExpectKnownValue(testKernelModuleResource, tfjsonpath.New("id"), knownvalue.StringExact(name)),
				},
				Check: resource.ComposeTestCheckFunc(
					checkFakeModule(f, name, true, "numdummies=2"),
					checkFileText(env.loadFile(name), loadConf),
					checkFileText(env.optionsFile(name), moduleConfHeader+"\noptions dummy numdummies=2\n"),
				),
			},
			// Changing the parameters reloads the module.
			{
				Config: kernelModuleHCL(name, `parameters = { numdummies = "0" }`),
				Check: resource.ComposeTestCheckFunc(
					checkFakeModule(f, name, true, "numdummies=0"),
					checkFileText(env.optionsFile(name), moduleConfHeader+"\noptions dummy numdummies=0\n"),
					checkCallCount(f, "unload", 1),
					checkCallCount(f, "load", 2),
				),
			},
			// An edited configuration file reads as not persisted and is
			// rewritten without reloading the module.
			{
				PreConfig: func() {
					if err := os.WriteFile(env.optionsFile(name), []byte("options dummy numdummies=5\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				},
				Config: kernelModuleHCL(name, `parameters = { numdummies = "0" }`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testKernelModuleResource, plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue(testKernelModuleResource, tfjsonpath.New("persist"), knownvalue.Bool(true)),
					},
				},
				Check: resource.ComposeTestCheckFunc(
					checkFileText(env.optionsFile(name), moduleConfHeader+"\noptions dummy numdummies=0\n"),
					checkCallCount(f, "unload", 1),
				),
			},
			// Removing the parameters removes the modprobe.d file.
			{
				Config: kernelModuleHCL(name, ""),
				Check: resource.ComposeTestCheckFunc(
					checkFakeModule(f, name, true),
					checkFileText(env.loadFile(name), loadConf),
					checkFileText(env.optionsFile(name), ""),
				),
			},
			// persist = false removes the modules-load.d file.
			{
				Config: kernelModuleHCL(name, "persist = false"),
				Check: resource.ComposeTestCheckFunc(
					checkFakeModule(f, name, true),
					checkFileText(env.loadFile(name), ""),
				),
			},
			// A module unloaded outside Terraform is loaded again.
			{
				PreConfig: func() { f.unloadOutOfBand(name) },
				Config:    kernelModuleHCL(name, "persist = false"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testKernelModuleResource, plancheck.ResourceActionCreate),
					},
				},
				Check: checkFakeModule(f, name, true),
			},
		},
	})
}

func TestKernelModuleResource_import(t *testing.T) {
	env := newModuleTestEnv(t)
	f := newFakeModuleLoader()
	f.loadOutOfBand("br_netfilter")
	if err := os.MkdirAll(env.optionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A hand-written file: the parameters are imported, and it is rewritten
	// in the resource's format by the first apply.
	if err := os.WriteFile(env.optionsFile("br_netfilter"), []byte("options br_netfilter debug=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := kernelModuleHCL("br_netfilter", `parameters = { debug = "1" }`)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: kernelModuleProviderFactories(env, f),
		Steps: []resource.TestStep{
			{
				Config:             cfg,
				ResourceName:       testKernelModuleResource,
				ImportState:        true,
				ImportStateId:      "br_netfilter",
				ImportStatePersist: true,
				ImportStateCheck: func(s []*terraform.InstanceState) error {
					if len(s) != 1 || s[0].Attributes["parameters.debug"] != "1" || s[0].Attributes["persist"] != "false" {
						return fmt.Errorf("imported state: %v", s[0].Attributes)
					}
					return nil
				},
			},
			{
				Config: cfg,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testKernelModuleResource, plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeTestCheckFunc(
					checkFileText(env.optionsFile("br_netfilter"), moduleConfHeader+"\noptions br_netfilter debug=1\n"),
					checkFileText(env.loadFile("br_netfilter"), moduleConfHeader+"\nbr_netfilter\n"),
					// The parameters did not change, so no reload.
					checkCallCount(f, "unload", 0),
				),
			},
			{
				Config:        cfg,
				ResourceName:  testKernelModuleResource,
				ImportState:   true,
				ImportStateId: "not_loaded",
				ExpectError:   regexp.MustCompile(`not_loaded\s+is\s+not\s+loaded`),
			},
		},
	})
}

func TestKernelModuleResource_alreadyLoaded(t *testing.T) {
	env := newModuleTestEnv(t)
	f := newFakeModuleLoader()
	f.loadOutOfBand("dummy")

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: kernelModuleProviderFactories(env, f),
		Steps: []resource.TestStep{
			// A module loaded before is taken over without reloading it,
			// even with parameters; they apply at the next load.
			{
				Config: kernelModuleHCL("dummy", `parameters = { numdummies = "0" }`),
				Check: resource.ComposeTestCheckFunc(
					checkCallCount(f, "unload", 0),
					checkCallCount(f, "load", 0),
					checkFileText(env.optionsFile("dummy"), moduleConfHeader+"\noptions dummy numdummies=0\n"),
				),
			},
		},
	})
}

func TestKernelModuleResource_errors(t *testing.T) {
	env := newModuleTestEnv(t)
	f := newFakeModuleLoader()
	f.set(f.missing, "nope")
	f.set(f.builtin, "builtin")
	f.badParams["bad=1"] = true
	if err := os.MkdirAll(env.optionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const foreign = "options iwlwifi power_save=0\n"
	if err := os.WriteFile(env.optionsFile("iwlwifi"), []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: kernelModuleProviderFactories(env, f),
		Steps: []resource.TestStep{
			{
				Config:      kernelModuleHCL("../../etc/passwd", ""),
				ExpectError: regexp.MustCompile(`Invalid\s+module\s+name`),
				PlanOnly:    true,
			},
			{
				Config:      kernelModuleHCL("-r", ""),
				ExpectError: regexp.MustCompile(`Invalid\s+module\s+name`),
				PlanOnly:    true,
			},
			{
				Config:      kernelModuleHCL("dummy", `parameters = { x = "a b" }`),
				ExpectError: regexp.MustCompile(`Invalid\s+module\s+parameter\s+value`),
				PlanOnly:    true,
			},
			{
				Config:      kernelModuleHCL("dummy", `parameters = { "x y" = "1" }`),
				ExpectError: regexp.MustCompile(`Invalid\s+module\s+parameter\s+name`),
				PlanOnly:    true,
			},
			{
				Config:      kernelModuleHCL("nope", ""),
				ExpectError: regexp.MustCompile(`Module\s+nope\s+not\s+found`),
			},
			{
				Config:      kernelModuleHCL("builtin", ""),
				ExpectError: regexp.MustCompile(`may\s+be\s+built\s+into\s+the\s+kernel`),
			},
			// Parameters the module rejects are not persisted.
			{
				Config:      kernelModuleHCL("dummy", `parameters = { bad = "1" }`),
				ExpectError: regexp.MustCompile(`Invalid\s+argument`),
			},
			// A configuration file the resource did not write is not taken
			// over.
			{
				Config:      kernelModuleHCL("iwlwifi", ""),
				ExpectError: regexp.MustCompile(`was\s+not\s+written\s+by\s+sysutils_kernel_module`),
			},
		},
	})
	for _, p := range []string{env.loadFile("nope"), env.loadFile("dummy"), env.optionsFile("dummy"), env.loadFile("iwlwifi")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("failed create left %s behind: %v", p, err)
		}
	}
	if data, err := os.ReadFile(env.optionsFile("iwlwifi")); err != nil || string(data) != foreign {
		t.Errorf("foreign file changed: %q, %v", data, err)
	}
	if n := f.callCount("load iwlwifi"); n != 0 {
		t.Errorf("iwlwifi was loaded %d times despite the refusal", n)
	}
}

func TestKernelModuleResource_inUse(t *testing.T) {
	env := newModuleTestEnv(t)
	f := newFakeModuleLoader()

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: kernelModuleProviderFactories(env, f),
		// A module in use cannot be unloaded; destroy only warns, but
		// removes the configuration.
		CheckDestroy: resource.ComposeTestCheckFunc(
			checkFakeModule(f, "dummy", true, "numdummies=1"),
			checkFileText(env.loadFile("dummy"), ""),
			checkFileText(env.optionsFile("dummy"), ""),
		),
		Steps: []resource.TestStep{
			{
				Config: kernelModuleHCL("dummy", `parameters = { numdummies = "1" }`),
				Check:  func(*terraform.State) error { f.set(f.inUse, "dummy"); return nil },
			},
			// Changing parameters fails without changing anything.
			{
				Config:      kernelModuleHCL("dummy", `parameters = { numdummies = "2" }`),
				ExpectError: regexp.MustCompile(`Module\s+dummy\s+is\s+in\s+use`),
			},
			{
				Config: kernelModuleHCL("dummy", `parameters = { numdummies = "1" }`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.ComposeTestCheckFunc(
					checkFakeModule(f, "dummy", true, "numdummies=1"),
					checkFileText(env.optionsFile("dummy"), moduleConfHeader+"\noptions dummy numdummies=1\n"),
				),
			},
		},
	})
}

// TestKernelModuleResource_failedReloadRestores checks that parameters the
// module rejects on reload are not persisted, and the module is loaded again
// with its previous parameters.
func TestKernelModuleResource_failedReloadRestores(t *testing.T) {
	env := newModuleTestEnv(t)
	f := newFakeModuleLoader()
	f.badParams["numdummies=9"] = true
	opts := moduleConfHeader + "\noptions dummy numdummies=1\n"

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: kernelModuleProviderFactories(env, f),
		Steps: []resource.TestStep{
			{
				Config: kernelModuleHCL("dummy", `parameters = { numdummies = "1" }`),
			},
			{
				Config:      kernelModuleHCL("dummy", `parameters = { numdummies = "9" }`),
				ExpectError: regexp.MustCompile(`Invalid\s+argument`),
			},
			{
				Config: kernelModuleHCL("dummy", `parameters = { numdummies = "1" }`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.ComposeTestCheckFunc(
					checkFakeModule(f, "dummy", true, "numdummies=1"),
					checkFileText(env.optionsFile("dummy"), opts),
				),
			},
		},
	})
}
