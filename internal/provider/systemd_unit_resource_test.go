package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

const testSystemdUnitResource = "sysutils_systemd_unit.test"

const testUnitV1 = `[Unit]
Description=sysutils test v1

[Service]
ExecStart=/bin/sleep infinity

[Install]
WantedBy=multi-user.target
`

const testUnitV2 = `[Unit]
Description=sysutils test v2

[Service]
ExecStart=/bin/sleep infinity

[Install]
WantedBy=multi-user.target
`

func TestValidateUnitName(t *testing.T) {
	for _, ok := range []string{"app.service", "a.socket", "backup.timer", "srv-data.mount", "my_app-2.target", "x.path", "system-foo.slice", "dev-sdb1.swap", "a:b.service", `srv-my\x2ddata.mount`} {
		if err := validateUnitName(ok); err != nil {
			t.Errorf("validateUnitName(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "app", ".service", "app.conf", "-app.service", ".app.service", "app@.service", "app@1.service", "../app.service", "a/b.service", "a b.service", "app.service\n", "app.scope", "sda.device", strings.Repeat("a", 250) + ".service"} {
		if err := validateUnitName(bad); err == nil {
			t.Errorf("validateUnitName(%q) = nil, want error", bad)
		}
	}
}

func TestUnitRunState(t *testing.T) {
	for active, want := range map[string]string{
		"active": unitStateRunning, "reloading": unitStateRunning, "refreshing": unitStateRunning,
		"inactive": unitStateStopped, "failed": unitStateStopped, "activating": unitStateStopped,
		"deactivating": unitStateStopped, "maintenance": unitStateStopped,
	} {
		if got := unitRunState(active); got != want {
			t.Errorf("unitRunState(%q) = %q, want %q", active, got, want)
		}
	}
	for state, want := range map[string]bool{
		"enabled": true, "enabled-runtime": false, "disabled": false, "static": false,
		"masked": false, "indirect": false, "not-found": false,
	} {
		if got := unitFileEnabled(state); got != want {
			t.Errorf("unitFileEnabled(%q) = %v, want %v", state, got, want)
		}
	}
}

func TestParseShowOutput(t *testing.T) {
	got := parseShowOutput("LoadState=loaded\nFragmentPath=/etc/systemd/system/a.service\nEnvironment=A=B\n\n")
	want := map[string]string{"LoadState": "loaded", "FragmentPath": "/etc/systemd/system/a.service", "Environment": "A=B"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("parseShowOutput = %v, want %v", got, want)
	}
}

// cannedRunner returns a commandRunner that answers every command with the
// given output and records the last spec it received.
func cannedRunner(last *execSpec, code int, stdout, stderr string, timedOut bool) commandRunner {
	return func(_ context.Context, spec execSpec) (*execResult, error) {
		*last = spec
		res := &execResult{ExitCode: code, TimedOut: timedOut,
			Stdout: newCappedOutput(spec.MaxOutputBytes), Stderr: newCappedOutput(spec.MaxOutputBytes)}
		_, _ = res.Stdout.Write([]byte(stdout))
		_, _ = res.Stderr.Write([]byte(stderr))
		return res, nil
	}
}

func TestSystemctl_queries(t *testing.T) {
	ctx := context.Background()
	var spec execSpec

	sc := systemctl{run: cannedRunner(&spec, 1, "disabled\n", "", false), timeout: time.Minute}
	if got, err := sc.isEnabled(ctx, "a.service"); err != nil || got != "disabled" {
		t.Errorf("isEnabled = %q, %v; want disabled", got, err)
	}
	wantArgv := []string{"systemctl", "--no-ask-password", "--no-pager", "is-enabled", "--", "a.service"}
	if fmt.Sprint(spec.Argv) != fmt.Sprint(wantArgv) {
		t.Errorf("argv = %q, want %q", spec.Argv, wantArgv)
	}
	if spec.Timeout != time.Minute {
		t.Errorf("timeout = %s, want 1m", spec.Timeout)
	}

	// Older systemd reports a missing unit file only on stderr.
	sc.run = cannedRunner(&spec, 1, "", "Failed to get unit file state for a.service: No such file or directory\n", false)
	if got, err := sc.isEnabled(ctx, "a.service"); err != nil || got != "not-found" {
		t.Errorf("isEnabled = %q, %v; want not-found", got, err)
	}

	sc.run = cannedRunner(&spec, 1, "", "Failed to connect to bus: No medium found\n", false)
	if _, err := sc.isActive(ctx, "a.service"); err == nil || !strings.Contains(err.Error(), "Failed to connect to bus") {
		t.Errorf("isActive error = %v, want the stderr message", err)
	}

	sc.run = cannedRunner(&spec, 5, "", "Unit a.service not found.\n", false)
	err := sc.do(ctx, "start", "--", "a.service")
	if err == nil || !strings.Contains(err.Error(), "exit status 5: Unit a.service not found.") {
		t.Errorf("do error = %v", err)
	}

	sc.run = cannedRunner(&spec, -1, "", "", true)
	if err := sc.do(ctx, "start", "--", "a.service"); err == nil || !strings.Contains(err.Error(), "timed out after 1m0s") {
		t.Errorf("do error = %v, want a timeout", err)
	}

	sc.run = func(context.Context, execSpec) (*execResult, error) {
		return nil, errors.New("executable file not found")
	}
	if _, err := sc.isActive(ctx, "a.service"); err == nil || !strings.Contains(err.Error(), "executable file not found") {
		t.Errorf("isActive error = %v", err)
	}
}

func systemdUnitConfig(name, content string, extra string) string {
	return fmt.Sprintf(`
resource "sysutils_systemd_unit" "test" {
  name    = %q
  content = %q
%s
}`, name, content, extra)
}

// checkFakeUnit checks the fake systemd's view of a unit.
func checkFakeUnit(f *fakeSystemd, name string, wantEnabled bool, wantActive string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		enabled, active := f.get(name)
		if enabled != wantEnabled || active != wantActive {
			return fmt.Errorf("unit %s: enabled=%v active=%q, want enabled=%v active=%q", name, enabled, active, wantEnabled, wantActive)
		}
		return nil
	}
}

func checkCalls(f *fakeSystemd, verb, unit string, want int) resource.TestCheckFunc {
	return func(*terraform.State) error {
		if got := f.callsOf(verb, unit); got != want {
			return fmt.Errorf("systemctl %s %s ran %d times, want %d", verb, unit, got, want)
		}
		return nil
	}
}

func checkFakeUnitRemoved(f *fakeSystemd, name string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		if _, err := os.Lstat(f.unitFile(name)); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("unit file still exists: %v", err)
		}
		return checkFakeUnit(f, name, false, "inactive")(nil)
	}
}

func TestSystemdUnit_lifecycle(t *testing.T) {
	f := newFakeSystemd(t)
	const name = "tftest.service"
	managed := `
  enabled = true
  state   = "running"`

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		CheckDestroy:             checkFakeUnitRemoved(f, name),
		Steps: []resource.TestStep{
			{
				Config: systemdUnitConfig(name, testUnitV1, managed),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testSystemdUnitResource, "id", name),
					resource.TestCheckResourceAttr(testSystemdUnitResource, "path", f.unitFile(name)),
					resource.TestCheckResourceAttr(testSystemdUnitResource, "enabled", "true"),
					resource.TestCheckResourceAttr(testSystemdUnitResource, "state", "running"),
					resource.TestCheckResourceAttr(testSystemdUnitResource, "restart_on_change", "true"),
					resource.TestCheckResourceAttr(testSystemdUnitResource, "timeout", "2m"),
					resource.TestCheckResourceAttr(testSystemdUnitResource, "content_sha256", sha256Hex([]byte(testUnitV1))),
					checkFileContent(f.unitFile(name), testUnitV1),
					checkFileMode(f.unitFile(name), 0o644),
					checkFakeUnit(f, name, true, "active"),
					checkCalls(f, "daemon-reload", "", 1),
				),
			},
			{
				// A content change reloads systemd, re-enables the unit and
				// restarts it.
				Config: systemdUnitConfig(name, testUnitV2, managed),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testSystemdUnitResource, plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue(testSystemdUnitResource, tfjsonpath.New("content_sha256"), knownvalue.StringExact(sha256Hex([]byte(testUnitV2)))),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(f.unitFile(name), testUnitV2),
					checkCalls(f, "daemon-reload", "", 2),
					checkCalls(f, "reenable", name, 1),
					checkCalls(f, "try-restart", name, 1),
					checkFakeUnit(f, name, true, "active"),
				),
			},
			{
				// Stopping and disabling out of band is drift that apply reverts.
				PreConfig: func() { f.set(name, false, "inactive") },
				Config:    systemdUnitConfig(name, testUnitV2, managed),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testSystemdUnitResource, plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFakeUnit(f, name, true, "active"),
					// No content change, so no reload or restart.
					checkCalls(f, "daemon-reload", "", 2),
					checkCalls(f, "try-restart", name, 1),
				),
			},
			{
				// Editing the unit file out of band is drift, too.
				PreConfig: func() {
					if err := os.WriteFile(f.unitFile(name), []byte(testUnitV1), 0o644); err != nil {
						t.Fatal(err)
					}
				},
				Config: systemdUnitConfig(name, testUnitV2, managed),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testSystemdUnitResource, plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(f.unitFile(name), testUnitV2),
					checkCalls(f, "daemon-reload", "", 3),
				),
			},
			{
				Config: systemdUnitConfig(name, testUnitV2, `
  enabled = false
  state   = "stopped"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testSystemdUnitResource, "enabled", "false"),
					resource.TestCheckResourceAttr(testSystemdUnitResource, "state", "stopped"),
					checkFakeUnit(f, name, false, "inactive"),
				),
			},
			{
				ResourceName:      testSystemdUnitResource,
				ImportState:       true,
				ImportStateId:     name,
				ImportStateVerify: true,
			},
		},
	})
}

// With enabled and state unset, they are reported but never changed.
func TestSystemdUnit_unmanagedState(t *testing.T) {
	f := newFakeSystemd(t)
	const name = "tfunmanaged.service"

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		CheckDestroy:             checkFakeUnitRemoved(f, name),
		Steps: []resource.TestStep{
			{
				Config: systemdUnitConfig(name, testUnitV1, ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testSystemdUnitResource, "enabled", "false"),
					resource.TestCheckResourceAttr(testSystemdUnitResource, "state", "stopped"),
					checkCalls(f, "enable", "", 0),
					checkCalls(f, "start", "", 0),
				),
			},
			{
				PreConfig: func() { f.set(name, true, "active") },
				Config:    systemdUnitConfig(name, testUnitV1, ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testSystemdUnitResource, "enabled", "true"),
					resource.TestCheckResourceAttr(testSystemdUnitResource, "state", "running"),
				),
			},
			{
				// A running unit is restarted when its file changes.
				Config: systemdUnitConfig(name, testUnitV2, ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkCalls(f, "try-restart", name, 1),
					checkCalls(f, "reenable", name, 1),
					checkFakeUnit(f, name, true, "active"),
				),
			},
			{
				Config: systemdUnitConfig(name, testUnitV1, "  restart_on_change = false"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkCalls(f, "try-restart", name, 1),
					checkCalls(f, "daemon-reload", "", 3),
				),
			},
			{
				// Dropping the [Install] section makes the unit static. With
				// enabled unmanaged, that is reported rather than an error.
				Config: systemdUnitConfig(name, "[Service]\nExecStart=/bin/sleep infinity\n", ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectUnknownValue(testSystemdUnitResource, tfjsonpath.New("enabled")),
						plancheck.ExpectUnknownValue(testSystemdUnitResource, tfjsonpath.New("state")),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testSystemdUnitResource, "enabled", "false"),
					resource.TestCheckResourceAttr(testSystemdUnitResource, "state", "running"),
					checkCalls(f, "enable", "", 0),
					checkCalls(f, "disable", "", 0),
				),
			},
		},
	})
}

func TestSystemdUnit_source(t *testing.T) {
	f := newFakeSystemd(t)
	const name = "tfsource.timer"
	src := filepath.Join(t.TempDir(), "unit.timer")
	writeSrc := func(content string) {
		if err := os.WriteFile(src, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const timerV1 = "[Timer]\nOnCalendar=daily\n\n[Install]\nWantedBy=timers.target\n"
	const timerV2 = "[Timer]\nOnCalendar=weekly\n\n[Install]\nWantedBy=timers.target\n"
	writeSrc(timerV1)
	config := fmt.Sprintf(`
resource "sysutils_systemd_unit" "test" {
  name    = %q
  source  = %q
  enabled = true
}`, name, src)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		CheckDestroy:             checkFakeUnitRemoved(f, name),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testSystemdUnitResource, "content_sha256", sha256Hex([]byte(timerV1))),
					resource.TestCheckNoResourceAttr(testSystemdUnitResource, "content"),
					checkFileContent(f.unitFile(name), timerV1),
					checkFakeUnit(f, name, true, "inactive"),
				),
			},
			{
				// A changed source plans an update without a config change.
				PreConfig: func() { writeSrc(timerV2) },
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testSystemdUnitResource, plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testSystemdUnitResource, "content_sha256", sha256Hex([]byte(timerV2))),
					checkFileContent(f.unitFile(name), timerV2),
					checkCalls(f, "daemon-reload", "", 2),
				),
			},
		},
	})
}

// A unit file deleted out of band is recreated, even though systemd still
// has the unit loaded from it.
func TestSystemdUnit_fileDeleted(t *testing.T) {
	f := newFakeSystemd(t)
	const name = "tfdeleted.service"
	config := systemdUnitConfig(name, testUnitV1, `  state = "running"`)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		CheckDestroy:             checkFakeUnitRemoved(f, name),
		Steps: []resource.TestStep{
			{Config: config},
			{
				PreConfig: func() {
					if err := os.Remove(f.unitFile(name)); err != nil {
						t.Fatal(err)
					}
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testSystemdUnitResource, plancheck.ResourceActionCreate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(f.unitFile(name), testUnitV1),
					checkFakeUnit(f, name, false, "active"),
				),
			},
		},
	})
}

func TestSystemdUnit_refusesExistingUnits(t *testing.T) {
	f := newFakeSystemd(t)
	f.vendor["ssh.service"] = "/usr/lib/systemd/system/ssh.service"
	if err := os.WriteFile(f.unitFile("local.service"), []byte(testUnitV1), 0o644); err != nil {
		t.Fatal(err)
	}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		Steps: []resource.TestStep{
			{
				Config:      systemdUnitConfig("ssh.service", testUnitV1, ""),
				ExpectError: regexp.MustCompile(`A\s+unit\s+named\s+"ssh.service"\s+already\s+exists\s+in\s+systemd`),
			},
			{
				Config:      systemdUnitConfig("local.service", testUnitV1, ""),
				ExpectError: regexp.MustCompile(`already\s+exists;\s+import\s+it`),
			},
		},
	})
	if _, err := os.Lstat(f.unitFile("ssh.service")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a unit file was written for a vendor unit: %v", err)
	}
	if got := f.callsOf("daemon-reload", ""); got != 0 {
		t.Errorf("daemon-reload ran %d times", got)
	}
}

func TestSystemdUnit_errors(t *testing.T) {
	tests := []struct {
		name    string
		unit    string
		content string
		extra   string
		setup   func(*fakeSystemd)
		want    string
	}{
		{
			name:    "static unit cannot be enabled",
			unit:    "tfstatic.service",
			content: "[Service]\nExecStart=/bin/sleep infinity\n",
			extra:   "  enabled = true",
			want:    `Only\s+units\s+with\s+an\s+\[Install\]\s+section`,
		},
		{
			name:    "start fails",
			unit:    "tffail.service",
			content: "[Service]\nExecStart=/bin/false\n",
			extra:   `  state = "running"`,
			want:    `Job\s+for\s+tffail.service\s+failed`,
		},
		{
			name:    "oneshot exits",
			unit:    "tfoneshot.service",
			content: "[Service]\nType=oneshot\nExecStart=/bin/true\n",
			extra:   `  state = "running"`,
			want:    `RemainAfterExit=yes`,
		},
		{
			name:    "unit does not load",
			unit:    "tfbroken.service",
			content: "[Broken]\n",
			want:    `load\s+state\s+"bad-setting"`,
		},
		{
			name:    "timeout",
			unit:    "tfslow.service",
			content: testUnitV1,
			extra:   "  state = \"running\"\n  timeout = \"5s\"",
			setup:   func(f *fakeSystemd) { f.timeouts["start"] = true },
			want:    `systemctl\s+start\s+--\s+tfslow.service:\s+timed\s+out\s+after\s+5s`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeSystemd(t)
			if tt.setup != nil {
				tt.setup(f)
			}
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: f.providerFactories(),
				Steps: []resource.TestStep{
					{
						Config:      systemdUnitConfig(tt.unit, tt.content, tt.extra),
						ExpectError: regexp.MustCompile(tt.want),
					},
				},
			})
			// The failed resource was recorded (tainted) and destroyed
			// again, so nothing is left behind.
			if _, err := os.Lstat(f.unitFile(tt.unit)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("unit file left behind: %v", err)
			}
		})
	}
}

func TestSystemdUnit_invalidConfig(t *testing.T) {
	f := newFakeSystemd(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		Steps: []resource.TestStep{
			{
				Config:      systemdUnitConfig("app", testUnitV1, ""),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`Invalid\s+unit\s+name`),
			},
			{
				Config:      systemdUnitConfig("app@.service", testUnitV1, ""),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`template\s+and\s+instance\s+units`),
			},
			{
				Config:      systemdUnitConfig("app.service", testUnitV1, `  state = "started"`),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`value\s+must\s+be\s+one\s+of`),
			},
			{
				Config:      systemdUnitConfig("app.service", testUnitV1, `  source = "/etc/hostname"`),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`Invalid\s+Attribute\s+Combination`),
			},
			{
				Config:      systemdUnitConfig("app.service", testUnitV1, `  timeout = "0s"`),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`must\s+be\s+greater\s+than\s+zero`),
			},
			{
				Config: `
resource "sysutils_systemd_unit" "test" {
  name = "app.service"
}`,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`Missing\s+Attribute\s+Configuration`),
			},
		},
	})
	if got := f.callsOf("daemon-reload", ""); got != 0 {
		t.Errorf("daemon-reload ran %d times", got)
	}
}
