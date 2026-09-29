package provider

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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

const testSysctlResource = "sysutils_sysctl.test"

// sysctlProviderFactories returns provider factories whose sysutils_sysctl
// uses procSys instead of /proc/sys.
func sysctlProviderFactories(procSys string) map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		"sysutils": providerserver.NewProtocol6WithError(&sysutilsProvider{
			version: "test",
			sysctl:  &sysctlConfig{procSys: procSys},
		}),
	}
}

func sysctlHCL(name, value, file, extra string) string {
	return fmt.Sprintf(`
resource "sysutils_sysctl" "test" {
  name  = %q
  value = %q
  file  = %q
%s
}
`, name, value, file, extra)
}

// checkFileText checks that p contains want, or does not exist if want is
// empty.
func checkFileText(p, want string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		data, err := os.ReadFile(p)
		if want == "" {
			if !os.IsNotExist(err) {
				return fmt.Errorf("%s exists (%q, %v), want it removed", p, data, err)
			}
			return nil
		}
		if err != nil {
			return err
		}
		if string(data) != want {
			return fmt.Errorf("%s is\n%s\nwant\n%s", p, data, want)
		}
		return nil
	}
}

func checkFakeSysctl(t *testing.T, root, name, want string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		data, err := os.ReadFile(fakeSysctlPath(t, root, name))
		if err != nil {
			return err
		}
		if string(data) != want+"\n" {
			return fmt.Errorf("%s = %q, want %q", name, data, want+"\n")
		}
		return nil
	}
}

func TestSysctlResource_lifecycle(t *testing.T) {
	const name = "net.ipv4.ip_forward"
	procSys := newFakeProcSys(t, map[string]string{name: "0"})
	dir := t.TempDir()
	file := filepath.Join(dir, "sysctl.d", "99-terraform.conf")
	other := filepath.Join(dir, "sysctl.d", "50-other.conf")
	if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
		t.Fatal(err)
	}
	// A shared file keeps its comments and other entries.
	const otherPrelude = "# Other settings\nvm.swappiness = 10\n"
	if err := os.WriteFile(other, []byte(otherPrelude), 0o644); err != nil {
		t.Fatal(err)
	}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: sysctlProviderFactories(procSys),
		CheckDestroy: resource.ComposeTestCheckFunc(
			// The running value is left alone on destroy.
			checkFakeSysctl(t, procSys, name, "1"),
			checkFileText(file, ""),
			checkFileText(other, otherPrelude),
		),
		Steps: []resource.TestStep{
			{
				Config: sysctlHCL(name, "1", file, ""),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(testSysctlResource, tfjsonpath.New("persist"), knownvalue.Bool(true)),
					statecheck.ExpectKnownValue(testSysctlResource, tfjsonpath.New("id"), knownvalue.StringExact(name)),
				},
				Check: resource.ComposeTestCheckFunc(
					checkFakeSysctl(t, procSys, name, "1"),
					checkFileText(file, name+" = 1\n"),
				),
			},
			// The running value drifts: the plan shows it, apply restores it.
			{
				PreConfig: func() { setFakeSysctl(t, procSys, name, "0") },
				Config:    sysctlHCL(name, "1", file, ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testSysctlResource, plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue(testSysctlResource, tfjsonpath.New("value"), knownvalue.StringExact("1")),
					},
				},
				Check: checkFakeSysctl(t, procSys, name, "1"),
			},
			// The persisted entry drifts: it reads as not persisted and is
			// rewritten, without touching the running value.
			{
				PreConfig: func() {
					if err := os.WriteFile(file, []byte("# edited\n"+name+"=0\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				},
				Config: sysctlHCL(name, "1", file, ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testSysctlResource, plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue(testSysctlResource, tfjsonpath.New("persist"), knownvalue.Bool(true)),
					},
				},
				Check: checkFileText(file, "# edited\n"+name+" = 1\n"),
			},
			// Moving to another file removes the entry from the old one,
			// which keeps the comment.
			{
				Config: sysctlHCL(name, "1", other, ""),
				Check: resource.ComposeTestCheckFunc(
					checkFileText(file, "# edited\n"),
					checkFileText(other, otherPrelude+name+" = 1\n"),
				),
			},
			// persist = false removes the entry.
			{
				Config: sysctlHCL(name, "1", other, "persist = false"),
				Check: resource.ComposeTestCheckFunc(
					checkFakeSysctl(t, procSys, name, "1"),
					checkFileText(other, otherPrelude),
				),
			},
			{
				PreConfig: func() {
					if err := os.Remove(file); err != nil {
						t.Fatal(err)
					}
				},
				Config: sysctlHCL(name, "1", other, "persist = false"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}

// TestSysctlResource_vectorValue checks that the tabs the kernel separates
// multi-value parameters with don't show up as a diff.
func TestSysctlResource_vectorValue(t *testing.T) {
	const name = "net.ipv4.ip_local_port_range"
	procSys := newFakeProcSys(t, map[string]string{name: "32768\t60999"})
	file := filepath.Join(t.TempDir(), "99-terraform.conf")

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: sysctlProviderFactories(procSys),
		Steps: []resource.TestStep{
			{
				Config: sysctlHCL(name, "1024 65535", file, ""),
				Check: resource.ComposeTestCheckFunc(
					checkFakeSysctl(t, procSys, name, "1024 65535"),
					checkFileText(file, name+" = 1024 65535\n"),
				),
			},
			{
				PreConfig: func() { setFakeSysctl(t, procSys, name, "1024\t65535") },
				Config:    sysctlHCL(name, "1024 65535", file, ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}

func TestSysctlResource_import(t *testing.T) {
	const name = "vm.swappiness"
	procSys := newFakeProcSys(t, map[string]string{name: "60"})
	dir := t.TempDir()
	file := filepath.Join(dir, "99-terraform.conf")
	custom := filepath.Join(dir, "10-custom.conf")
	if err := os.WriteFile(custom, []byte("vm.swappiness=60\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: sysctlProviderFactories(procSys),
		Steps: []resource.TestStep{
			{
				Config: sysctlHCL(name, "10", file, ""),
			},
			// Import by name reads the value from /proc/sys; the file
			// attribute is compared as the default, so it is ignored here.
			{
				ResourceName:            testSysctlResource,
				ImportState:             true,
				ImportStateId:           name,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"file", "persist"},
			},
			// Import by name and file.
			{
				ResourceName:      testSysctlResource,
				ImportState:       true,
				ImportStateId:     name + ":" + file,
				ImportStateVerify: true,
			},
		},
	})

	// A parameter persisted in another file imports with persist = true
	// when the entry matches the running value.
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: sysctlProviderFactories(newFakeProcSys(t, map[string]string{name: "60"})),
		Steps: []resource.TestStep{
			{
				Config:             sysctlHCL(name, "60", custom, ""),
				ResourceName:       testSysctlResource,
				ImportState:        true,
				ImportStateId:      name + ":" + custom,
				ImportStatePersist: true,
			},
			{
				Config: sysctlHCL(name, "60", custom, ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}

func TestSysctlResource_errors(t *testing.T) {
	procSys := newFakeProcSys(t, map[string]string{"net.ipv4.ip_forward": "0"})
	file := filepath.Join(t.TempDir(), "99-terraform.conf")
	if err := os.WriteFile(file, []byte("net.ipv4.ip_forward = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: sysctlProviderFactories(procSys),
		Steps: []resource.TestStep{
			{
				Config:      sysctlHCL("net.//.etc.passwd", "1", file, ""),
				ExpectError: regexp.MustCompile(`would\s+leave\s+/proc/sys`),
				PlanOnly:    true,
			},
			{
				Config:      sysctlHCL("net/ipv4/ip_forward", "1", file, ""),
				ExpectError: regexp.MustCompile(`must\s+be\s+in\s+dotted\s+form`),
				PlanOnly:    true,
			},
			{
				Config:      sysctlHCL("net.ipv4.ip_forward", "1\n2", file, ""),
				ExpectError: regexp.MustCompile(`must\s+not\s+contain\s+line\s+breaks`),
				PlanOnly:    true,
			},
			{
				Config:      sysctlHCL("net.ipv4.ip_forward", "1", "relative.conf", ""),
				ExpectError: regexp.MustCompile(`must\s+be\s+absolute`),
				PlanOnly:    true,
			},
			{
				Config:      sysctlHCL("net.ipv4.no_such_parameter", "1", file+".new.conf", ""),
				ExpectError: regexp.MustCompile(`no\s+such\s+kernel\s+parameter`),
			},
			// An existing entry is not taken over.
			{
				Config:      sysctlHCL("net.ipv4.ip_forward", "1", file, ""),
				ExpectError: regexp.MustCompile(`Parameter\s+already\s+in\s+file`),
			},
		},
	})
	if _, err := os.Stat(file + ".new.conf"); !os.IsNotExist(err) {
		t.Errorf("failed create left a file behind: %v", err)
	}
	checkFakeSysctlNow(t, procSys, "net.ipv4.ip_forward", "0")
}

func checkFakeSysctlNow(t *testing.T, root, name, want string) {
	t.Helper()
	if err := checkFakeSysctl(t, root, name, want)(nil); err != nil {
		t.Error(err)
	}
}

// TestSysctlResource_parameterDisappears checks that a parameter that no
// longer exists, for example because its interface was removed, is removed
// from state instead of failing the refresh.
func TestSysctlResource_parameterDisappears(t *testing.T) {
	const name = "net.ipv4.conf.veth0.forwarding"
	procSys := newFakeProcSys(t, map[string]string{name: "0"})
	file := filepath.Join(t.TempDir(), "99-terraform.conf")

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: sysctlProviderFactories(procSys),
		Steps: []resource.TestStep{
			{
				Config: sysctlHCL(name, "1", file, ""),
			},
			{
				PreConfig: func() {
					if err := os.RemoveAll(filepath.Join(procSys, "net", "ipv4", "conf", "veth0")); err != nil {
						t.Fatal(err)
					}
				},
				Config:             sysctlHCL(name, "1", file, ""),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
}
