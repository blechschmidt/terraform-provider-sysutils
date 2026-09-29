package provider

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// installTestUnit writes a long-running service unit to
// /etc/systemd/system and removes it again when the test ends.
func installTestUnit(t *testing.T) string {
	t.Helper()
	name := "tfacc-sysutils-svc-" + randomID() + ".service"
	unitPath := filepath.Join(defaultSystemdUnitDir, name)
	content := "[Unit]\nDescription=sysutils_service test\n\n[Service]\nExecStart=/bin/sleep infinity\n\n[Install]\nWantedBy=multi-user.target\n"
	if err := os.WriteFile(unitPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("systemctl", "stop", "--", name).Run()
		_ = exec.Command("systemctl", "disable", "--", name).Run()
		_ = os.Remove(unitPath)
		_ = exec.Command("systemctl", "daemon-reload").Run()
	})
	if out, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
		t.Fatalf("daemon-reload: %v: %s", err, out)
	}
	return name
}

func systemctlRun(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("systemctl", args...).CombinedOutput(); err != nil {
		t.Fatalf("systemctl %s: %v: %s", strings.Join(args, " "), err, out)
	}
}

func TestAccService_systemdLifecycle(t *testing.T) {
	requireSystemd(t)
	name := installTestUnit(t)
	cfg := func(enabled bool, state, trigger string) string {
		return fmt.Sprintf(`
resource "sysutils_service" "test" {
  name              = %q
  enabled           = %t
  state             = %q
  restart_on_change = { config = %q }
  timeout           = "30s"
}`, name, enabled, state, trigger)
	}
	var invocation string
	saveInvocation := func(*terraform.State) error {
		invocation = systemctlOutput(t, "show", "--property=InvocationID", "--value", "--", name)
		if invocation == "" {
			return errors.New("no InvocationID")
		}
		return nil
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		// Destroy leaves the service as the last step configured it.
		CheckDestroy: checkRealUnit(name, "disabled", "inactive"),
		Steps: []resource.TestStep{
			{
				Config: cfg(true, "running", "v1"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testServiceResource, "init_system", "systemd"),
					resource.TestCheckResourceAttr(testServiceResource, "enabled", "true"),
					resource.TestCheckResourceAttr(testServiceResource, "state", "running"),
					checkRealUnit(name, "enabled", "active"),
					saveInvocation,
				),
			},
			{
				// A changed trigger restarts the service.
				Config: cfg(true, "running", "v2"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkRealUnit(name, "enabled", "active"),
					func(*terraform.State) error {
						if got := systemctlOutput(t, "show", "--property=InvocationID", "--value", "--", name); got == invocation {
							return errors.New("the service was not restarted after restart_on_change changed")
						}
						return nil
					},
					saveInvocation,
				),
			},
			{
				// An unchanged trigger does not.
				Config: cfg(true, "running", "v2"),
				Check: func(*terraform.State) error {
					if got := systemctlOutput(t, "show", "--property=InvocationID", "--value", "--", name); got != invocation {
						return errors.New("the service was restarted although nothing changed")
					}
					return nil
				},
			},
			{
				// Drift: stopped and disabled outside Terraform.
				PreConfig: func() {
					systemctlRun(t, "disable", "--now", "--", name)
				},
				Config: cfg(true, "running", "v2"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testServiceResource, plancheck.ResourceActionUpdate)},
				},
				Check: checkRealUnit(name, "enabled", "active"),
			},
			{
				ResourceName:            testServiceResource,
				ImportState:             true,
				ImportStateId:           name,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"restart_on_change", "timeout"},
			},
			{
				Config: cfg(false, "stopped", "v3"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testServiceResource, "enabled", "false"),
					resource.TestCheckResourceAttr(testServiceResource, "state", "stopped"),
					checkRealUnit(name, "disabled", "inactive"),
				),
			},
		},
	})
}

// TestAccService_withSystemdUnit checks the documented combination: the
// unit file from sysutils_systemd_unit, its runtime state from
// sysutils_service.
func TestAccService_withSystemdUnit(t *testing.T) {
	requireSystemd(t)
	name := "tfacc-sysutils-svc-" + randomID() + ".service"
	cfg := func(description string) string {
		return fmt.Sprintf(`
resource "sysutils_systemd_unit" "test" {
  name    = %q
  content = "[Unit]\nDescription=%s\n\n[Service]\nExecStart=/bin/sleep infinity\n\n[Install]\nWantedBy=multi-user.target\n"
  timeout = "30s"
}

resource "sysutils_service" "test" {
  name    = sysutils_systemd_unit.test.name
  enabled = true
  state   = "running"
  timeout = "30s"
}`, name, description)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkRealUnitRemoved(name),
		Steps: []resource.TestStep{
			{
				// The unit resource's own enabled and state were recorded
				// before the service resource changed them; the refresh of
				// the post-apply plan picks up the new values without a
				// diff, since they are not set in its configuration.
				Config: cfg("v1"),
				Check:  checkRealUnit(name, "enabled", "active"),
			},
			{
				RefreshState: true,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testSystemdUnitResource, "enabled", "true"),
					resource.TestCheckResourceAttr(testSystemdUnitResource, "state", "running"),
				),
			},
			{
				Config: cfg("v2"),
				Check:  checkRealUnit(name, "enabled", "active"),
			},
		},
	})
}

func TestAccService_systemdNotFound(t *testing.T) {
	requireSystemd(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config:      serviceTF("tfacc-sysutils-nosuch-"+randomID()+".service", `  state = "running"`),
			ExpectError: regexp.MustCompile(`Service\s+not\s+found`),
		}},
	})
}

// requireOpenRC skips tests that need a real OpenRC: they run as root on
// a host or container that OpenRC booted, such as Alpine Linux.
func requireOpenRC(t *testing.T) {
	t.Helper()
	if os.Getenv("TF_ACC") == "" {
		t.Skip("acceptance test; set TF_ACC=1 to run")
	}
	requireRoot(t)
	if _, err := os.Stat("/run/systemd/system"); err == nil {
		t.Skip("systemd is running")
	}
	if _, err := os.Stat("/run/openrc/softlevel"); err != nil {
		t.Skip("OpenRC did not boot this host")
	}
	for _, tool := range []string{"rc-service", "rc-update", "openrc-run"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found", tool)
		}
	}
}

func checkRealOpenRCService(name string, wantEnabled, wantRunning bool) resource.TestCheckFunc {
	return func(*terraform.State) error {
		_, err := os.Lstat(filepath.Join("/etc/runlevels", defaultOpenRCRunlevel, name))
		enabled := err == nil
		running := exec.Command("rc-service", name, "status").Run() == nil
		if enabled != wantEnabled || running != wantRunning {
			return fmt.Errorf("service %s: enabled=%v running=%v, want enabled=%v running=%v", name, enabled, running, wantEnabled, wantRunning)
		}
		return nil
	}
}

func TestAccService_openrcLifecycle(t *testing.T) {
	requireOpenRC(t)
	name := "tfacc-sysutils-svc-" + randomID()
	script := filepath.Join("/etc/init.d", name)
	content := fmt.Sprintf("#!/sbin/openrc-run\ncommand=/bin/sleep\ncommand_args=1000000\ncommand_background=yes\npidfile=/run/%s.pid\n", name)
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("rc-service", name, "stop").Run()
		_ = exec.Command("rc-update", "del", name, defaultOpenRCRunlevel).Run()
		_ = os.Remove(script)
	})
	cfg := func(enabled bool, state, trigger string) string {
		return serviceTF(name, fmt.Sprintf("  enabled = %t\n  state = %q\n  restart_on_change = { v = %q }\n  timeout = \"30s\"", enabled, state, trigger))
	}
	pid := func() string {
		data, _ := os.ReadFile("/run/" + name + ".pid")
		return strings.TrimSpace(string(data))
	}
	var firstPID string

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkRealOpenRCService(name, false, false),
		Steps: []resource.TestStep{
			{
				Config: cfg(true, "running", "v1"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testServiceResource, "init_system", "openrc"),
					checkRealOpenRCService(name, true, true),
					func(*terraform.State) error { firstPID = pid(); return nil },
				),
			},
			{
				Config: cfg(true, "running", "v2"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkRealOpenRCService(name, true, true),
					func(*terraform.State) error {
						if p := pid(); p == "" || p == firstPID {
							return fmt.Errorf("pid %q after restart_on_change changed, was %q; not restarted", p, firstPID)
						}
						return nil
					},
				),
			},
			{
				PreConfig: func() {
					_ = exec.Command("rc-service", name, "stop").Run()
					_ = exec.Command("rc-update", "del", name, defaultOpenRCRunlevel).Run()
				},
				Config: cfg(true, "running", "v2"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testServiceResource, plancheck.ResourceActionUpdate)},
				},
				Check: checkRealOpenRCService(name, true, true),
			},
			{
				ResourceName:            testServiceResource,
				ImportState:             true,
				ImportStateId:           name,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"restart_on_change", "timeout"},
			},
			{
				Config: cfg(false, "stopped", "v2"),
				Check:  checkRealOpenRCService(name, false, false),
			},
		},
	})
}

// TestAccService_systemdDropInReload checks that a restart triggered by a
// changed drop-in picks the drop-in up: systemd would otherwise restart the
// unit with the configuration it loaded before.
func TestAccService_systemdDropInReload(t *testing.T) {
	requireSystemd(t)
	name := installTestUnit(t)
	dropInDir := filepath.Join(defaultSystemdUnitDir, name+".d")
	t.Cleanup(func() { _ = os.RemoveAll(dropInDir) })
	cfg := func(trigger string) string {
		return serviceTF(name, fmt.Sprintf("  state = \"running\"\n  restart_on_change = { dropin = %q }\n  timeout = \"30s\"", trigger))
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: cfg("v1")},
			{
				PreConfig: func() {
					if err := os.MkdirAll(dropInDir, 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dropInDir, "override.conf"), []byte("[Unit]\nDescription=from drop-in\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				},
				Config: cfg("v2"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkRealUnit(name, "disabled", "active"),
					func(*terraform.State) error {
						if got := systemctlOutput(t, "show", "--property=Description", "--value", "--", name); got != "from drop-in" {
							return fmt.Errorf("Description = %q after restart; the drop-in was not loaded", got)
						}
						if got := systemctlOutput(t, "show", "--property=NeedDaemonReload", "--value", "--", name); got != "no" {
							return fmt.Errorf("NeedDaemonReload = %q after restart", got)
						}
						return nil
					},
				),
			},
		},
	})
}
