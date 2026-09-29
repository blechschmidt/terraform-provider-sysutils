package provider

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// accModuleName is the module the acceptance tests load. With
// numdummies=0 the dummy network driver loads without creating any
// interface, and nothing depends on it.
const accModuleName = "dummy"

// capSysModule is the bit of CAP_SYS_MODULE in the capability sets.
const capSysModule = 16

// requireLoadableModule skips the test unless the real modprobe can load
// name here: it needs root with CAP_SYS_MODULE, which containers usually
// lack, module loading must not be disabled, and the module must be
// installed for the running kernel. A module that is already loaded is not
// used, so that the test never unloads something in use. Anything a failed
// test leaves loaded is unloaded at the end.
func requireLoadableModule(t *testing.T, name string) {
	t.Helper()
	if os.Getenv("TF_ACC") == "" {
		t.Skip("acceptance test; set TF_ACC=1 to run")
	}
	requireRoot(t)
	if !hasEffectiveCapability(t, capSysModule) {
		t.Skip("CAP_SYS_MODULE is not in the effective capability set, as in most containers")
	}
	if data, err := os.ReadFile("/proc/sys/kernel/modules_disabled"); err == nil && strings.TrimSpace(string(data)) == "1" {
		t.Skip("module loading is disabled (kernel.modules_disabled = 1)")
	}
	if _, err := exec.LookPath("modprobe"); err != nil {
		t.Skip("modprobe is not installed")
	}
	if out, err := exec.Command("modprobe", "--dry-run", "--", name).CombinedOutput(); err != nil {
		t.Skipf("module %s is not available for this kernel: %v: %s", name, err, out)
	}
	if loaded, err := isModuleLoaded(realModuleLoader(), name); err != nil {
		t.Fatal(err)
	} else if loaded {
		t.Skipf("module %s is already loaded; not touching it", name)
	}
	t.Cleanup(func() {
		if loaded, _ := isModuleLoaded(realModuleLoader(), name); loaded {
			_ = exec.Command("modprobe", "-r", "--", name).Run()
		}
	})
}

func realModuleLoader() moduleLoader { return (*kernelModuleConfig)(nil).modules() }

// hasEffectiveCapability reports whether capability bit is in CapEff of
// this process.
func hasEffectiveCapability(t *testing.T, bit uint) bool {
	t.Helper()
	f, err := os.Open("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "CapEff:"); ok {
			caps, err := strconv.ParseUint(strings.TrimSpace(v), 16, 64)
			if err != nil {
				t.Fatal(err)
			}
			return caps&(1<<bit) != 0
		}
	}
	t.Fatal("no CapEff in /proc/self/status")
	return false
}

func checkRealModuleLoaded(name string, want bool) resource.TestCheckFunc {
	return func(*terraform.State) error {
		loaded, err := isModuleLoaded(realModuleLoader(), name)
		if err != nil {
			return err
		}
		if loaded != want {
			return fmt.Errorf("module %s loaded = %v, want %v", name, loaded, want)
		}
		return nil
	}
}

func TestAccKernelModule_lifecycle(t *testing.T) {
	requireLoadableModule(t, accModuleName)
	// Temporary configuration directories: the real /etc is never touched.
	env := newModuleTestEnv(t)
	factories := kernelModuleProviderFactories(env, nil)
	opts := func(v string) string {
		return moduleConfHeader + "\noptions " + accModuleName + " numdummies=" + v + "\n"
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		CheckDestroy: resource.ComposeTestCheckFunc(
			checkRealModuleLoaded(accModuleName, false),
			checkFileText(env.loadFile(accModuleName), ""),
			checkFileText(env.optionsFile(accModuleName), ""),
		),
		Steps: []resource.TestStep{
			{
				Config: kernelModuleHCL(accModuleName, `parameters = { numdummies = "0" }`),
				Check: resource.ComposeTestCheckFunc(
					checkRealModuleLoaded(accModuleName, true),
					checkFileText(env.loadFile(accModuleName), moduleConfHeader+"\n"+accModuleName+"\n"),
					checkFileText(env.optionsFile(accModuleName), opts("0")),
				),
			},
			// Changing the parameters reloads the module.
			{
				Config: kernelModuleHCL(accModuleName, `parameters = { numdummies = "1" }`),
				Check: resource.ComposeTestCheckFunc(
					checkRealModuleLoaded(accModuleName, true),
					checkFileText(env.optionsFile(accModuleName), opts("1")),
					func(*terraform.State) error {
						// numdummies=1 creates dummy0.
						if _, err := os.Stat("/sys/class/net/dummy0"); err != nil {
							return fmt.Errorf("the reloaded module did not create dummy0: %w", err)
						}
						return nil
					},
				),
			},
			{
				Config: kernelModuleHCL(accModuleName, `parameters = { numdummies = "0" }`),
				Check:  checkRealModuleLoaded(accModuleName, true),
			},
			// A module unloaded outside Terraform is loaded again.
			{
				PreConfig: func() {
					if err := realModuleLoader().unload(context.Background(), accModuleName); err != nil {
						t.Fatal(err)
					}
				},
				Config: kernelModuleHCL(accModuleName, `parameters = { numdummies = "0" }`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testKernelModuleResource, plancheck.ResourceActionCreate),
					},
				},
				Check: checkRealModuleLoaded(accModuleName, true),
			},
			{
				ResourceName:      testKernelModuleResource,
				ImportState:       true,
				ImportStateId:     accModuleName,
				ImportStateVerify: true,
			},
		},
	})
}

func TestAccKernelModule_missing(t *testing.T) {
	requireLoadableModule(t, accModuleName)
	env := newModuleTestEnv(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: kernelModuleProviderFactories(env, nil),
		Steps: []resource.TestStep{
			{
				Config:      kernelModuleHCL("sysutils_no_such_module", ""),
				ExpectError: regexp.MustCompile(`(?i)module\s+sysutils_no_such_module\s+not\s+found`),
			},
		},
	})
	if _, err := os.Stat(env.loadFile("sysutils_no_such_module")); !os.IsNotExist(err) {
		t.Errorf("failed load left its configuration behind: %v", err)
	}
}
