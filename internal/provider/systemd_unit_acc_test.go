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

// requireSystemd skips tests that need a real systemd: they run as root on a
// host (or container) where systemd is PID 1.
func requireSystemd(t *testing.T) {
	t.Helper()
	if os.Getenv("TF_ACC") == "" {
		t.Skip("acceptance test; set TF_ACC=1 to run")
	}
	requireRoot(t)
	comm, err := os.ReadFile("/proc/1/comm")
	if err != nil || strings.TrimSpace(string(comm)) != "systemd" {
		t.Skip("systemd is not PID 1")
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		t.Skip("systemctl not found")
	}
}

func systemctlOutput(t *testing.T, args ...string) string {
	t.Helper()
	out, _ := exec.Command("systemctl", args...).Output()
	return strings.TrimSpace(string(out))
}

func checkRealUnit(name, wantEnabled, wantActive string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		enabled, _ := exec.Command("systemctl", "is-enabled", "--", name).Output()
		active, _ := exec.Command("systemctl", "is-active", "--", name).Output()
		gotEnabled, gotActive := strings.TrimSpace(string(enabled)), strings.TrimSpace(string(active))
		if gotEnabled != wantEnabled || gotActive != wantActive {
			return fmt.Errorf("unit %s: is-enabled=%q is-active=%q, want %q and %q", name, gotEnabled, gotActive, wantEnabled, wantActive)
		}
		return nil
	}
}

func checkRealUnitRemoved(name string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		if _, err := os.Lstat(filepath.Join(defaultSystemdUnitDir, name)); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("unit file for %s still exists: %v", name, err)
		}
		if out, _ := exec.Command("systemctl", "is-active", "--", name).Output(); strings.TrimSpace(string(out)) != "inactive" {
			return fmt.Errorf("unit %s is still %q", name, strings.TrimSpace(string(out)))
		}
		out, _ := exec.Command("systemctl", "show", "--property=LoadState", "--", name).Output()
		if got := strings.TrimSpace(string(out)); got != "LoadState=not-found" {
			return fmt.Errorf("unit %s is still known to systemd: %s", name, got)
		}
		return nil
	}
}

func TestAccSystemdUnit_lifecycle(t *testing.T) {
	requireSystemd(t)
	name := "tfacc-sysutils-" + randomID() + ".service"
	unit := func(description string) string {
		return fmt.Sprintf("[Unit]\nDescription=%s\n\n[Service]\nExecStart=/bin/sleep infinity\n\n[Install]\nWantedBy=multi-user.target\n", description)
	}
	config := func(content, extra string) string {
		return fmt.Sprintf(`
resource "sysutils_systemd_unit" "test" {
  name    = %q
  content = %q
  timeout = "30s"
%s
}`, name, content, extra)
	}
	running := "  enabled = true\n  state   = \"running\""
	var firstInvocation string

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkRealUnitRemoved(name),
		Steps: []resource.TestStep{
			{
				Config: config(unit("v1"), running),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testSystemdUnitResource, "path", filepath.Join(defaultSystemdUnitDir, name)),
					resource.TestCheckResourceAttr(testSystemdUnitResource, "enabled", "true"),
					resource.TestCheckResourceAttr(testSystemdUnitResource, "state", "running"),
					checkFileMode(filepath.Join(defaultSystemdUnitDir, name), 0o644),
					checkRealUnit(name, "enabled", "active"),
					func(*terraform.State) error {
						firstInvocation = systemctlOutput(t, "show", "--property=InvocationID", "--value", "--", name)
						return nil
					},
				),
			},
			{
				// The new description is loaded and the service restarted.
				Config: config(unit("v2"), running),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkRealUnit(name, "enabled", "active"),
					func(*terraform.State) error {
						if got := systemctlOutput(t, "show", "--property=Description", "--value", "--", name); got != "v2" {
							return fmt.Errorf("Description = %q after update; daemon-reload did not run", got)
						}
						if got := systemctlOutput(t, "show", "--property=InvocationID", "--value", "--", name); got == firstInvocation {
							return errors.New("the service was not restarted after its unit file changed")
						}
						return nil
					},
				),
			},
			{
				PreConfig: func() {
					if out, err := exec.Command("systemctl", "disable", "--now", "--", name).CombinedOutput(); err != nil {
						t.Fatalf("systemctl disable --now: %v: %s", err, out)
					}
				},
				Config: config(unit("v2"), running),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testSystemdUnitResource, plancheck.ResourceActionUpdate),
					},
				},
				Check: checkRealUnit(name, "enabled", "active"),
			},
			{
				Config: config(unit("v2"), "  enabled = false\n  state   = \"stopped\""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testSystemdUnitResource, "enabled", "false"),
					resource.TestCheckResourceAttr(testSystemdUnitResource, "state", "stopped"),
					checkRealUnit(name, "disabled", "inactive"),
				),
			},
			{
				ResourceName:      testSystemdUnitResource,
				ImportState:       true,
				ImportStateId:     name,
				ImportStateVerify: true,
				// Import sets the default timeout; the configuration's is
				// applied by the next plan.
				ImportStateVerifyIgnore: []string{"timeout"},
			},
			{
				// Destroy stops and disables a running unit.
				Config: config(unit("v2"), running),
				Check:  checkRealUnit(name, "enabled", "active"),
			},
		},
	})
}

// A unit that already exists elsewhere, here a runtime unit in
// /run/systemd/system, is never overridden.
func TestAccSystemdUnit_refusesExistingUnit(t *testing.T) {
	requireSystemd(t)
	name := "tfacc-sysutils-" + randomID() + ".service"
	runtimeFile := filepath.Join("/run/systemd/system", name)
	if err := os.MkdirAll(filepath.Dir(runtimeFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimeFile, []byte("[Service]\nExecStart=/bin/sleep infinity\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Remove(runtimeFile)
		_ = exec.Command("systemctl", "daemon-reload").Run()
	})
	if out, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
		t.Fatalf("daemon-reload: %v: %s", err, out)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "sysutils_systemd_unit" "test" {
  name    = %q
  content = "[Service]\nExecStart=/bin/true\n"
}`, name),
				ExpectError: regexp.MustCompile(`already\s+exists\s+in\s+systemd`),
			},
		},
	})
	if _, err := os.Lstat(filepath.Join(defaultSystemdUnitDir, name)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a unit file was written over an existing unit: %v", err)
	}
}
