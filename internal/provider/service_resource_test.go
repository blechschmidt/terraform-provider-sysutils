package provider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const testServiceResource = "sysutils_service.test"

func serviceTF(name, extra string) string {
	return fmt.Sprintf(`
resource "sysutils_service" "test" {
  name = %q
%s
}`, name, extra)
}

// checkFakeService checks the fake init system's view of a service.
func checkFakeService(f *fakeInit, name string, wantEnabled, wantRunning bool) resource.TestCheckFunc {
	return func(*terraform.State) error {
		s := f.get(name)
		enabled := s.enabled
		if f.kind == initSystemOpenRC {
			enabled = slices.Contains(s.runlevels, defaultOpenRCRunlevel)
		}
		if enabled != wantEnabled || s.running != wantRunning {
			return fmt.Errorf("service %s: enabled=%v running=%v, want enabled=%v running=%v", name, enabled, s.running, wantEnabled, wantRunning)
		}
		return nil
	}
}

// checkChanges checks the commands that changed services since the last
// clearCalls, and clears them.
func checkChanges(f *fakeInit, want ...string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		defer f.clearCalls()
		if got := f.changes(); !slices.Equal(got, want) {
			return fmt.Errorf("service changes = %q, want %q", got, want)
		}
		return nil
	}
}

func TestValidateServiceName(t *testing.T) {
	for _, ok := range []string{"nginx", "nginx.service", "getty@tty1.service", "a:b.service", `srv-my\x2ddata.mount`, "net.eth0", "_x", "backup.timer"} {
		if err := validateServiceName(ok); err != nil {
			t.Errorf("validateServiceName(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "-nginx", ".nginx", "../nginx", "a/b", "a b", "nginx\n", "nginx;id", "$(id)", strings.Repeat("a", 256)} {
		if err := validateServiceName(bad); err == nil {
			t.Errorf("validateServiceName(%q) = nil, want error", bad)
		}
	}
	for _, bad := range []string{"getty@tty1", "a:b", `a\x2d`} {
		if err := validateServiceNameFor(initSystemOpenRC, bad); err == nil {
			t.Errorf("validateServiceNameFor(openrc, %q) = nil, want error", bad)
		}
		if err := validateServiceNameFor(initSystemSystemd, bad); err != nil {
			t.Errorf("validateServiceNameFor(systemd, %q) = %v, want nil", bad, err)
		}
	}
	for _, ok := range []string{"default", "boot", "sysinit", "my-level_2"} {
		if err := validateRunlevel(ok); err != nil {
			t.Errorf("validateRunlevel(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-default", "../boot", "a b", "a/b"} {
		if err := validateRunlevel(bad); err == nil {
			t.Errorf("validateRunlevel(%q) = nil, want error", bad)
		}
	}
}

func TestParseRCUpdateShow(t *testing.T) {
	out := "                acpid |      default\n               crond | boot default\n        crond-extra | default\n"
	for _, tc := range []struct {
		name, level string
		want        bool
	}{
		{"acpid", "default", true},
		{"crond", "default", true},
		{"crond", "boot", true},
		{"crond", "sysinit", false},
		{"cron", "default", false},
		{"sshd", "default", false},
	} {
		if got := parseRCUpdateShow(out, tc.name, tc.level); got != tc.want {
			t.Errorf("parseRCUpdateShow(%s, %s) = %v, want %v", tc.name, tc.level, got, tc.want)
		}
	}
}

func TestServiceInitSystemDetection(t *testing.T) {
	all := func(name string) (string, error) { return "/usr/bin/" + name, nil }
	none := func(name string) (string, error) { return "", fmt.Errorf("%s: not found", name) }
	mk := func(t *testing.T, systemd, openrc bool) string {
		dir := t.TempDir()
		if systemd {
			if err := os.MkdirAll(filepath.Join(dir, "systemd", "system"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if openrc {
			if err := os.MkdirAll(filepath.Join(dir, "openrc"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "openrc", "softlevel"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	for _, tc := range []struct {
		name            string
		systemd, openrc bool
		lookPath        func(string) (string, error)
		want, err       string
	}{
		{"systemd", true, false, all, initSystemSystemd, ""},
		{"systemd wins", true, true, all, initSystemSystemd, ""},
		{"openrc", false, true, all, initSystemOpenRC, ""},
		{"systemd without systemctl", true, false, none, "", "systemd is running, but systemctl was not found"},
		{"openrc without tools", false, true, none, "", "OpenRC booted this host, but rc-service and rc-update not found"},
		{"none", false, false, none, "", "no supported init system found"},
		{"installed but not booted", false, false, all, "", "systemctl is installed, but systemd is not PID 1; OpenRC is installed, but"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &serviceConfig{lookPath: tc.lookPath, runDir: mk(t, tc.systemd, tc.openrc)}
			got, err := cfg.detect()
			switch {
			case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
				t.Errorf("detect() error = %v, want %q", err, tc.err)
			case tc.err == "" && err != nil:
				t.Errorf("detect() error = %v", err)
			case got != tc.want:
				t.Errorf("detect() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestServiceBackendCommands checks the exact command lines, which the
// fake init system does not: it drops "--".
func TestServiceBackendCommands(t *testing.T) {
	ctx := context.Background()

	s := &scriptedRunner{rules: []scriptedRule{
		{prefix: "systemctl --no-ask-password --no-pager show", stdout: "LoadState=loaded\nId=ssh.service\n"},
		{prefix: "systemctl --no-ask-password --no-pager is-enabled", exit: 1, stdout: "disabled\n"},
		{prefix: "systemctl --no-ask-password --no-pager is-active", stdout: "active\n"},
		{prefix: "systemctl"},
	}}
	cfg := &serviceConfig{run: s.run}
	m, err := cfg.manager(initSystemSystemd, time.Minute, defaultOpenRCRunlevel)
	if err != nil {
		t.Fatal(err)
	}
	st, err := m.Status(ctx, "sshd")
	if err != nil {
		t.Fatal(err)
	}
	if want := (serviceStatus{Found: true, Unit: "ssh.service", Enabled: false, Running: true, EnabledState: "disabled", ActiveState: "active", Detail: `is-enabled "disabled", is-active "active"`}); st != want {
		t.Errorf("Status = %+v, want %+v", st, want)
	}
	if err := m.SetEnabled(ctx, st.Unit, true); err != nil {
		t.Fatal(err)
	}
	if err := m.Restart(ctx, st.Unit); err != nil {
		t.Fatal(err)
	}
	checkCommands(t, s,
		"systemctl --no-ask-password --no-pager show --property=LoadState --property=Id -- sshd",
		"systemctl --no-ask-password --no-pager is-enabled -- ssh.service",
		"systemctl --no-ask-password --no-pager is-active -- ssh.service",
		"systemctl --no-ask-password --no-pager enable -- ssh.service",
		"systemctl --no-ask-password --no-pager daemon-reload",
		"systemctl --no-ask-password --no-pager restart -- ssh.service",
	)

	// A changed unit file or drop-in is reloaded before starting.
	s.specs = nil
	s.rules[0].stdout = "NeedDaemonReload=yes\n"
	if err := m.SetRunning(ctx, st.Unit, true); err != nil {
		t.Fatal(err)
	}
	if err := m.SetRunning(ctx, st.Unit, false); err != nil {
		t.Fatal(err)
	}
	checkCommands(t, s,
		"systemctl --no-ask-password --no-pager show --property=NeedDaemonReload -- ssh.service",
		"systemctl --no-ask-password --no-pager daemon-reload",
		"systemctl --no-ask-password --no-pager start -- ssh.service",
		"systemctl --no-ask-password --no-pager stop -- ssh.service",
	)
	for _, spec := range s.specs {
		if spec.Timeout != time.Minute {
			t.Errorf("%q: timeout %s, want 1m", spec.Argv, spec.Timeout)
		}
	}

	s = &scriptedRunner{rules: []scriptedRule{
		{prefix: "rc-service --exists crond"},
		{prefix: "rc-update show -- boot", stdout: "               crond | boot\n"},
		{prefix: "rc-service -- crond status", exit: 3, stdout: " * status: stopped\n"},
		{prefix: "rc-service -- crond start", exit: 1, stderr: " * ERROR: crond failed to start\n"},
		{prefix: "rc-update del"},
	}}
	cfg = &serviceConfig{run: s.run}
	m, err = cfg.manager(initSystemOpenRC, time.Minute, "boot")
	if err != nil {
		t.Fatal(err)
	}
	st, err = m.Status(ctx, "crond")
	if err != nil {
		t.Fatal(err)
	}
	if want := (serviceStatus{Found: true, Unit: "crond", Enabled: true, Running: false, EnabledState: "enabled", ActiveState: "stopped", Detail: "in runlevel boot, stopped"}); st != want {
		t.Errorf("Status = %+v, want %+v", st, want)
	}
	if err := m.SetEnabled(ctx, "crond", false); err != nil {
		t.Fatal(err)
	}
	err = m.SetRunning(ctx, "crond", true)
	if err == nil || !strings.Contains(err.Error(), "rc-service -- crond start: exit status 1: * ERROR: crond failed to start") {
		t.Errorf("SetRunning error = %v", err)
	}
	checkCommands(t, s,
		"rc-service --exists crond",
		"rc-update show -- boot",
		"rc-service -- crond status",
		"rc-update del -- crond boot",
		"rc-service -- crond start",
	)
	for _, spec := range s.specs {
		if !slices.Contains(spec.Env, "EINFO_COLOR=NO") {
			t.Errorf("%q: EINFO_COLOR=NO not set", spec.Argv)
		}
	}

	// rc-service exits with 1 when it fails itself, as opposed to
	// reporting a stopped service.
	s = &scriptedRunner{rules: []scriptedRule{
		{prefix: "rc-service --exists"},
		{prefix: "rc-update show"},
		{prefix: "rc-service -- crond status", exit: 1, stdout: " * You are attempting to run an openrc service on a\n"},
	}}
	cfg = &serviceConfig{run: s.run}
	m, _ = cfg.manager(initSystemOpenRC, time.Minute, defaultOpenRCRunlevel)
	if _, err := m.Status(ctx, "crond"); err == nil || !strings.Contains(err.Error(), "You are attempting") {
		t.Errorf("Status error = %v, want the rc-service message", err)
	}
	// Never pass a name OpenRC can't have, whatever the caller checked.
	if _, err := m.Status(ctx, "getty@tty1"); err == nil {
		t.Error("Status accepted an invalid OpenRC service name")
	}

	// A command that times out is an error, not a stopped service.
	cfg = &serviceConfig{run: func(_ context.Context, spec execSpec) (*execResult, error) {
		return &execResult{ExitCode: -1, TimedOut: true, Stdout: newCappedOutput(1), Stderr: newCappedOutput(1)}, nil
	}}
	for _, kind := range []string{initSystemSystemd, initSystemOpenRC} {
		m, _ = cfg.manager(kind, time.Second, defaultOpenRCRunlevel)
		if _, err := m.Status(ctx, "crond"); err == nil || !strings.Contains(err.Error(), "timed out after 1s") {
			t.Errorf("%s: Status error = %v, want a timeout", kind, err)
		}
	}
}

func TestService_systemdLifecycle(t *testing.T) {
	f := newFakeInit(t, initSystemSystemd)
	const name = "app.service"
	f.add(name, fakeService{})

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		// Destroying the resource leaves the service alone.
		CheckDestroy: checkFakeService(f, name, false, false),
		Steps: []resource.TestStep{
			{
				Config: serviceTF(name, `  enabled = true
  state   = "running"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testServiceResource, "id", name),
					resource.TestCheckResourceAttr(testServiceResource, "enabled", "true"),
					resource.TestCheckResourceAttr(testServiceResource, "state", "running"),
					resource.TestCheckResourceAttr(testServiceResource, "init_system", "systemd"),
					resource.TestCheckResourceAttr(testServiceResource, "runlevel", "default"),
					resource.TestCheckResourceAttr(testServiceResource, "timeout", "2m"),
					resource.TestCheckNoResourceAttr(testServiceResource, "restart_on_change"),
					checkFakeService(f, name, true, true),
					checkChanges(f, "enable app.service", "start app.service"),
				),
			},
			{
				// Drift: stopped and disabled outside Terraform.
				PreConfig: func() { f.set(name, false, false) },
				Config: serviceTF(name, `  enabled = true
  state   = "running"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testServiceResource, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFakeService(f, name, true, true),
					checkChanges(f, "enable app.service", "start app.service"),
				),
			},
			{
				Config: serviceTF(name, `  enabled = false
  state   = "stopped"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testServiceResource, "enabled", "false"),
					resource.TestCheckResourceAttr(testServiceResource, "state", "stopped"),
					checkFakeService(f, name, false, false),
					checkChanges(f, "disable app.service", "stop app.service"),
				),
			},
			{
				// Started outside Terraform: the plan shows it.
				PreConfig: func() { f.set(name, false, true) },
				Config: serviceTF(name, `  enabled = false
  state   = "stopped"`),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				ResourceName:      testServiceResource,
				ImportState:       true,
				ImportStateId:     name,
				ImportStateVerify: true,
				// The service is running now; the state was not
				// refreshed since.
				ImportStateVerifyIgnore: []string{"state"},
			},
			{
				Config: serviceTF(name, `  enabled = false
  state   = "stopped"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFakeService(f, name, false, false),
					checkChanges(f, "stop app.service"),
				),
			},
		},
	})
}

func TestService_unmanagedValuesAreReported(t *testing.T) {
	f := newFakeInit(t, initSystemSystemd)
	f.add("app.service", fakeService{enabled: true, running: true})

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		Steps: []resource.TestStep{
			{
				// No suffix: systemctl completes it to app.service.
				Config: serviceTF("app", ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testServiceResource, "id", "app"),
					resource.TestCheckResourceAttr(testServiceResource, "enabled", "true"),
					resource.TestCheckResourceAttr(testServiceResource, "state", "running"),
					checkChanges(f),
				),
			},
			{
				// Changes outside Terraform are recorded, not reverted.
				PreConfig: func() { f.set("app.service", false, false) },
				Config:    serviceTF("app", ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testServiceResource, "enabled", "false"),
					resource.TestCheckResourceAttr(testServiceResource, "state", "stopped"),
					checkChanges(f),
				),
			},
		},
	})
}

func TestService_restartOnChange(t *testing.T) {
	f := newFakeInit(t, initSystemSystemd)
	const name = "app.service"
	f.add(name, fakeService{running: true})
	cfg := func(state, triggers string) string {
		extra := ""
		if state != "" {
			extra = fmt.Sprintf("  state = %q\n", state)
		}
		if triggers != "" {
			extra += fmt.Sprintf("  restart_on_change = { config = %q }\n", triggers)
		}
		return serviceTF(name, extra)
	}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		Steps: []resource.TestStep{
			{
				// Create never restarts.
				Config: cfg("running", "v1"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testServiceResource, "restart_on_change.config", "v1"),
					checkChanges(f),
				),
			},
			{
				Config: cfg("running", "v2"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testServiceResource, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testServiceResource, "restart_on_change.config", "v2"),
					checkFakeService(f, name, false, true),
					checkChanges(f, "daemon-reload", "restart app.service"),
				),
			},
			{
				// Unchanged triggers: no restart.
				Config: cfg("running", "v2"),
				Check:  checkChanges(f),
			},
			{
				// Stopped by the same apply: no restart.
				Config: cfg("stopped", "v3"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFakeService(f, name, false, false),
					checkChanges(f, "stop app.service"),
				),
			},
			{
				// Started by the same apply: no second restart.
				Config: cfg("running", "v4"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFakeService(f, name, false, true),
					checkChanges(f, "start app.service"),
				),
			},
			{
				// Unmanaged state and stopped: a change does not start it.
				PreConfig: func() { f.set(name, false, false) },
				Config:    cfg("", "v5"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testServiceResource, "state", "stopped"),
					checkChanges(f),
				),
			},
			{
				// Unmanaged state and running: restarted.
				PreConfig: func() { f.set(name, false, true) },
				Config:    cfg("", "v6"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testServiceResource, "state", "running"),
					checkChanges(f, "daemon-reload", "restart app.service"),
				),
			},
			{
				// Starting a unit whose files changed reloads systemd
				// first.
				PreConfig: func() { f.set(name, false, false); f.mu.Lock(); f.services[name].needReload = true; f.mu.Unlock() },
				Config:    cfg("running", "v7"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testServiceResource, "state", "running"),
					checkChanges(f, "daemon-reload", "start app.service"),
				),
			},
			{
				// Removing the map does not restart.
				Config: cfg("", ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(testServiceResource, "restart_on_change.%"),
					checkChanges(f),
				),
			},
		},
	})
}

// TestService_failedRestartIsRetried checks that a failed restart keeps
// the old restart_on_change in state, so that the next apply retries it.
func TestService_failedRestartIsRetried(t *testing.T) {
	f := newFakeInit(t, initSystemSystemd)
	const name = "app.service"
	f.add(name, fakeService{running: true})
	cfg := func(v string) string {
		return serviceTF(name, fmt.Sprintf("  restart_on_change = { config = %q }", v))
	}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		Steps: []resource.TestStep{
			{Config: cfg("v1")},
			{
				PreConfig:   func() { f.mu.Lock(); f.services[name].failRestart = true; f.mu.Unlock() },
				Config:      cfg("v2"),
				ExpectError: regexp.MustCompile(`(?s)Restarting\s+service.*control\s+process\s+exited`),
			},
			{
				PreConfig: func() {
					f.mu.Lock()
					f.services[name].failRestart = false
					f.services[name].running = true
					f.mu.Unlock()
					f.clearCalls()
				},
				Config: cfg("v2"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testServiceResource, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testServiceResource, "restart_on_change.config", "v2"),
					resource.TestCheckResourceAttr(testServiceResource, "state", "running"),
					checkChanges(f, "daemon-reload", "restart app.service"),
				),
			},
		},
	})
}

func TestService_systemdAliasAndHints(t *testing.T) {
	f := newFakeInit(t, initSystemSystemd)
	f.add("ssh.service", fakeService{})
	f.aliases["sshd.service"] = "ssh.service"
	f.add("static.service", fakeService{static: true})
	f.add("oneshot.service", fakeService{exits: true})

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		Steps: []resource.TestStep{
			{
				// enable refuses aliases; the unit they name is enabled.
				Config: serviceTF("sshd.service", "  enabled = true"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testServiceResource, "enabled", "true"),
					checkFakeService(f, "ssh.service", true, false),
					checkChanges(f, "enable ssh.service"),
				),
			},
			{
				Config:      serviceTF("static.service", "  enabled = true"),
				ExpectError: regexp.MustCompile(`(?s)systemd\s+still\s+reports\s+is-enabled\s+"static".*\[Install\]`),
			},
			{
				Config:      serviceTF("oneshot.service", "  state = \"running\""),
				ExpectError: regexp.MustCompile(`(?s)The\s+start\s+command\s+succeeded.*RemainAfterExit=yes`),
			},
		},
	})
}

func TestService_notFound(t *testing.T) {
	for _, kind := range []string{initSystemSystemd, initSystemOpenRC} {
		t.Run(kind, func(t *testing.T) {
			f := newFakeInit(t, kind)
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: f.providerFactories(),
				Steps: []resource.TestStep{{
					Config:      serviceTF("nosuch", "  state = \"running\""),
					ExpectError: regexp.MustCompile(`Service\s+not\s+found`),
				}},
			})
		})
	}
}

// TestService_removedOutsideTerraform checks that a service that no longer
// exists is dropped from state, and the plan fails on create instead of
// silently doing nothing.
func TestService_removedOutsideTerraform(t *testing.T) {
	f := newFakeInit(t, initSystemSystemd)
	f.add("app.service", fakeService{})

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		Steps: []resource.TestStep{
			{Config: serviceTF("app.service", "")},
			{
				PreConfig:          func() { f.remove("app.service") },
				Config:             serviceTF("app.service", ""),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				Config:      serviceTF("app.service", ""),
				ExpectError: regexp.MustCompile(`Service\s+not\s+found`),
			},
		},
	})
}

func TestService_noInitSystem(t *testing.T) {
	f := newFakeInit(t, "")
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		Steps: []resource.TestStep{{
			Config:      serviceTF("app.service", "  state = \"running\""),
			ExpectError: regexp.MustCompile(`(?s)No\s+supported\s+init\s+system.*systemd\s+is\s+not\s+running.*OpenRC\s+is\s+not\s+running`),
		}},
	})
	if len(f.calls) != 0 {
		t.Errorf("commands ran without an init system: %q", f.calls)
	}
}

func TestService_openrcLifecycle(t *testing.T) {
	f := newFakeInit(t, initSystemOpenRC)
	const name = "crond"
	f.add(name, fakeService{})

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		CheckDestroy:             checkFakeService(f, name, false, true),
		Steps: []resource.TestStep{
			{
				Config: serviceTF(name, `  enabled = true
  state   = "running"
  restart_on_change = { v = "1" }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testServiceResource, "init_system", "openrc"),
					resource.TestCheckResourceAttr(testServiceResource, "enabled", "true"),
					resource.TestCheckResourceAttr(testServiceResource, "state", "running"),
					checkFakeService(f, name, true, true),
					checkChanges(f, "add crond default", "start crond"),
				),
			},
			{
				PreConfig: func() { f.set(name, false, false) },
				Config: serviceTF(name, `  enabled = true
  state   = "running"
  restart_on_change = { v = "1" }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFakeService(f, name, true, true),
					checkChanges(f, "add crond default", "start crond"),
				),
			},
			{
				Config: serviceTF(name, `  enabled = true
  state   = "running"
  restart_on_change = { v = "2" }`),
				Check: checkChanges(f, "restart crond"),
			},
			{
				// Moving to another runlevel: enabled is looked up there.
				Config: serviceTF(name, `  runlevel = "boot"
  restart_on_change = { v = "2" }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testServiceResource, "enabled", "false"),
					checkChanges(f),
				),
			},
			{
				Config: serviceTF(name, `  runlevel = "boot"
  enabled  = true
  restart_on_change = { v = "2" }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testServiceResource, "enabled", "true"),
					checkChanges(f, "add crond boot"),
				),
			},
			{
				Config: serviceTF(name, `  enabled = false
  state   = "running"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFakeService(f, name, false, true),
					checkChanges(f, "del crond default"),
				),
			},
			{
				ResourceName:            testServiceResource,
				ImportState:             true,
				ImportStateId:           name,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"restart_on_change"},
			},
			{
				Config: serviceTF(name, `  runlevel = "nosuchlevel"
  enabled  = true`),
				ExpectError: regexp.MustCompile(`not\s+a\s+valid\s+runlevel`),
			},
			{
				Config:      serviceTF("getty@tty1", ""),
				ExpectError: regexp.MustCompile(`not\s+a\s+valid\s+OpenRC\s+service\s+name`),
			},
		},
	})
}

func TestService_rootDirRefused(t *testing.T) {
	f := newFakeInit(t, initSystemSystemd)
	f.add("app.service", fakeService{})
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		Steps: []resource.TestStep{{
			Config: fmt.Sprintf(`
provider "sysutils" {
  root_dir = %q
}
`, t.TempDir()) + serviceTF("app.service", "  state = \"running\""),
			ExpectError: regexp.MustCompile(`Not\s+supported\s+with\s+root_dir`),
		}},
	})
	if got := f.get("app.service"); got.running {
		t.Error("the service was started although root_dir is set")
	}
}

func TestService_invalidConfig(t *testing.T) {
	f := newFakeInit(t, initSystemSystemd)
	for _, tc := range []struct{ extra, err string }{
		{`  state = "started"`, `value must be one of`},
		{`  timeout = "0s"`, `must\s+be\s+greater\s+than\s+zero`},
		{`  runlevel = "../x"`, `Invalid\s+runlevel`},
	} {
		resource.UnitTest(t, resource.TestCase{
			ProtoV6ProviderFactories: f.providerFactories(),
			Steps: []resource.TestStep{{
				Config:      serviceTF("app.service", tc.extra),
				ExpectError: regexp.MustCompile(tc.err),
			}},
		})
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		Steps: []resource.TestStep{{
			Config:      serviceTF("../../bin/sh", ""),
			ExpectError: regexp.MustCompile(`Invalid\s+service\s+name`),
		}},
	})
}
