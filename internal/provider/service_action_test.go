package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"
)

// Actions need Terraform 1.14. OpenTofu has no actions: it rejects action
// blocks as unsupported. The OpenTofu check comes first, so that OpenTofu
// is not reported as a Terraform version below 1.14.
var actionVersionChecks = []tfversion.TerraformVersionCheck{
	skipOpenTofu{reason: "OpenTofu does not support provider-defined actions"},
	tfversion.SkipBelow(tfversion.Version1_14_0),
}

// skipOpenTofu is a TerraformVersionCheck that skips the test on every
// OpenTofu release, for features that OpenTofu lacks. Its message matches
// that of skipOpenTofuVersions, which the acceptance scripts allow as a
// skip reason.
type skipOpenTofu struct {
	reason string
}

func (s skipOpenTofu) CheckTerraformVersion(_ context.Context, _ tfversion.CheckTerraformVersionRequest, resp *tfversion.CheckTerraformVersionResponse) {
	tofu, err := openTofuVersion()
	switch {
	case err != nil:
		resp.Error = err
	case tofu != nil:
		resp.Skip = fmt.Sprintf("OpenTofu %s is skipped: %s", tofu, s.reason)
	}
}

// invokeAction runs a's ModifyPlan and, if it succeeds, Invoke with attrs
// as the configuration, the way Terraform does for a triggered action. It
// returns the diagnostics of both and the progress messages sent.
func invokeAction(t *testing.T, a action.Action, attrs map[string]tftypes.Value) (*action.InvokeResponse, []string) {
	t.Helper()
	ctx := context.Background()
	var sresp action.SchemaResponse
	a.Schema(ctx, action.SchemaRequest{}, &sresp)
	objType, ok := sresp.Schema.Type().TerraformType(ctx).(tftypes.Object)
	if !ok {
		t.Fatalf("schema type is %T", sresp.Schema.Type().TerraformType(ctx))
	}
	vals := map[string]tftypes.Value{}
	for name, typ := range objType.AttributeTypes {
		vals[name] = tftypes.NewValue(typ, nil)
	}
	for name, v := range attrs {
		if _, ok := vals[name]; !ok {
			t.Fatalf("unknown attribute %q", name)
		}
		vals[name] = v
	}
	config := tfsdk.Config{Schema: sresp.Schema, Raw: tftypes.NewValue(objType, vals)}

	var progress []string
	resp := &action.InvokeResponse{SendProgress: func(e action.InvokeProgressEvent) { progress = append(progress, e.Message) }}
	if m, ok := a.(action.ActionWithModifyPlan); ok {
		var mresp action.ModifyPlanResponse
		m.ModifyPlan(ctx, action.ModifyPlanRequest{Config: config}, &mresp)
		resp.Diagnostics.Append(mresp.Diagnostics...)
		if mresp.Diagnostics.HasError() {
			return resp, progress
		}
	}
	a.Invoke(ctx, action.InvokeRequest{Config: config}, resp)
	return resp, progress
}

func str(s string) tftypes.Value { return tftypes.NewValue(tftypes.String, s) }

// actionDiagText joins the summaries and details of diags.
func actionDiagText(resp *action.InvokeResponse) string {
	var sb strings.Builder
	for _, d := range resp.Diagnostics {
		fmt.Fprintf(&sb, "%s: %s\n", d.Summary(), d.Detail())
	}
	return sb.String()
}

func newTestServiceAction(f *fakeInit, root *fsRoot) *serviceAction {
	return &serviceAction{hostAction{cfg: f.config(), fsRoot: root}}
}

func TestServiceAction_invoke(t *testing.T) {
	for _, kind := range []string{initSystemSystemd, initSystemOpenRC} {
		t.Run(kind, func(t *testing.T) {
			svc := "app"
			if kind == initSystemSystemd {
				svc = "app.service"
			}
			for _, tc := range []struct {
				verb        string
				running     bool
				wantRunning bool
				wantChanges []string
				wantReloads int
			}{
				{verb: "restart", running: true, wantRunning: true},
				{verb: "restart", running: false, wantRunning: true},
				{verb: "reload", running: true, wantRunning: true, wantReloads: 1},
				{verb: "start", running: false, wantRunning: true},
				{verb: "start", running: true, wantRunning: true, wantChanges: []string{}},
				{verb: "stop", running: true, wantRunning: false},
				{verb: "stop", running: false, wantRunning: false, wantChanges: []string{}},
			} {
				t.Run(fmt.Sprintf("%s/running=%v", tc.verb, tc.running), func(t *testing.T) {
					f := newFakeInit(t, kind)
					f.add(svc, fakeService{running: tc.running})
					resp, progress := invokeAction(t, newTestServiceAction(f, nil), map[string]tftypes.Value{
						"name": str("app"), "action": str(tc.verb), "timeout": str("10s"),
					})
					if resp.Diagnostics.HasError() {
						t.Fatalf("unexpected error: %s", actionDiagText(resp))
					}
					got := f.get(svc)
					if got.running != tc.wantRunning || got.reloads != tc.wantReloads {
						t.Errorf("running=%v reloads=%d, want running=%v reloads=%d", got.running, got.reloads, tc.wantRunning, tc.wantReloads)
					}
					want := tc.wantChanges
					if want == nil {
						want = []string{tc.verb + " " + svc}
						if kind == initSystemSystemd && tc.verb == "restart" {
							// Restart always reloads systemd first.
							want = []string{"daemon-reload", "restart " + svc}
						}
					}
					if changes := f.changes(); !slices.Equal(changes, want) {
						t.Errorf("changes = %q, want %q", changes, want)
					}
					if len(progress) == 0 {
						t.Error("no progress was reported")
					}
				})
			}
		})
	}
}

func TestServiceAction_errors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kind  string
		svc   fakeService
		attrs map[string]tftypes.Value
		root  *fsRoot
		want  string
	}{
		{
			name:  "not found",
			kind:  initSystemSystemd,
			attrs: map[string]tftypes.Value{"name": str("missing"), "action": str("restart")},
			want:  `Service not found: systemd has no unit named "missing"`,
		},
		{
			name:  "reload stopped",
			kind:  initSystemSystemd,
			svc:   fakeService{},
			attrs: map[string]tftypes.Value{"name": str("app"), "action": str("reload")},
			want:  "Service not running: app.service cannot be reloaded because it is not running",
		},
		{
			name:  "restart fails",
			kind:  initSystemOpenRC,
			svc:   fakeService{running: true, failRestart: true},
			attrs: map[string]tftypes.Value{"name": str("app"), "action": str("restart")},
			want:  "Service restart failed: rc-service -- app restart: exit status 1: * ERROR: app failed to start",
		},
		{
			name:  "exits after start",
			kind:  initSystemSystemd,
			svc:   fakeService{exits: true},
			attrs: map[string]tftypes.Value{"name": str("app"), "action": str("start")},
			want:  "Changing service state: The start command succeeded, but systemd reports",
		},
		{
			name:  "invalid OpenRC name",
			kind:  initSystemOpenRC,
			attrs: map[string]tftypes.Value{"name": str("getty@tty1"), "action": str("restart")},
			want:  "Invalid service name",
		},
		{
			name:  "invalid action",
			kind:  initSystemSystemd,
			svc:   fakeService{running: true},
			attrs: map[string]tftypes.Value{"name": str("app"), "action": str("kill")},
			want:  "Invalid action",
		},
		{
			name:  "invalid timeout",
			kind:  initSystemSystemd,
			attrs: map[string]tftypes.Value{"name": str("app"), "action": str("restart"), "timeout": str("soon")},
			want:  "Invalid timeout",
		},
		{
			name:  "no init system",
			kind:  "",
			attrs: map[string]tftypes.Value{"name": str("app"), "action": str("restart")},
			want:  "No supported init system",
		},
		{
			name:  "root_dir",
			kind:  initSystemSystemd,
			svc:   fakeService{running: true},
			attrs: map[string]tftypes.Value{"name": str("app"), "action": str("restart")},
			root:  &fsRoot{dir: "/srv/rootfs"},
			want:  `Not supported with root_dir: This action changes the running host, not files below root_dir, so it cannot be used with root_dir = "/srv/rootfs"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeInit(t, tc.kind)
			svcName := "app"
			if tc.kind == initSystemSystemd {
				svcName = "app.service"
			}
			f.add(svcName, tc.svc)
			before := f.get(svcName)
			resp, _ := invokeAction(t, newTestServiceAction(f, tc.root), tc.attrs)
			if !resp.Diagnostics.HasError() {
				t.Fatalf("expected an error containing %q", tc.want)
			}
			if got := actionDiagText(resp); !strings.Contains(got, tc.want) {
				t.Errorf("diagnostics:\n%s\nwant them to contain %q", got, tc.want)
			}
			if tc.root != nil || tc.name == "invalid action" || tc.name == "invalid timeout" {
				if after := f.get(svcName); after.running != before.running || len(f.changes()) > 0 {
					t.Errorf("the service was changed: %q", f.changes())
				}
			}
		})
	}
}

func TestSystemdDaemonReloadAction_invoke(t *testing.T) {
	f := newFakeInit(t, initSystemSystemd)
	f.add("app.service", fakeService{needReload: true})
	a := &systemdDaemonReloadAction{hostAction{cfg: f.config()}}
	resp, progress := invokeAction(t, a, nil)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected error: %s", actionDiagText(resp))
	}
	if got := f.changes(); !slices.Equal(got, []string{"daemon-reload"}) {
		t.Errorf("changes = %q, want daemon-reload", got)
	}
	if f.get("app.service").needReload {
		t.Error("daemon-reload did not run")
	}
	if len(progress) != 1 {
		t.Errorf("progress = %q", progress)
	}

	for _, tc := range []struct {
		name string
		kind string
		root *fsRoot
		want string
	}{
		{"openrc", initSystemOpenRC, nil, "systemd is not running: sysutils_systemd_daemon_reload needs systemd, but openrc manages services on this host"},
		{"none", "", nil, "systemd is not running: No supported init system found"},
		{"root_dir", initSystemSystemd, &fsRoot{dir: "/srv/rootfs"}, "Not supported with root_dir"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeInit(t, tc.kind)
			resp, _ := invokeAction(t, &systemdDaemonReloadAction{hostAction{cfg: f.config(), fsRoot: tc.root}}, nil)
			if got := actionDiagText(resp); !strings.Contains(got, tc.want) {
				t.Errorf("diagnostics:\n%s\nwant them to contain %q", got, tc.want)
			}
			if got := f.changes(); len(got) > 0 {
				t.Errorf("commands ran: %q", got)
			}
		})
	}
}

// actionTriggerTF is a sysutils_file at path whose creation and updates
// trigger the sysutils_service action "svc".
func actionTriggerTF(path, content, name, verb string) string {
	return fmt.Sprintf(`
resource "sysutils_file" "config" {
  path    = %q
  content = %q

  lifecycle {
    action_trigger {
      events  = [after_create, after_update]
      actions = [action.sysutils_service.svc]
    }
  }
}

action "sysutils_service" "svc" {
  config {
    name    = %q
    action  = %q
    timeout = "30s"
  }
}
`, path, content, name, verb)
}

// TestAccServiceAction_fakeTrigger checks that Terraform invokes the action
// through action_trigger when, and only when, the file is created or
// changed. It uses the fake init system, so it needs neither root nor an
// init system.
func TestAccServiceAction_fakeTrigger(t *testing.T) {
	f := newFakeInit(t, initSystemSystemd)
	f.add("app.service", fakeService{running: true})
	path := filepath.Join(t.TempDir(), "app.conf")

	resource.Test(t, resource.TestCase{
		TerraformVersionChecks:   actionVersionChecks,
		ProtoV6ProviderFactories: f.providerFactories(),
		Steps: []resource.TestStep{
			{
				Config: actionTriggerTF(path, "v1\n", "app", "restart"),
				Check:  checkChanges(f, "daemon-reload", "restart app.service"),
			},
			{
				// Unchanged: no invocation.
				Config: actionTriggerTF(path, "v1\n", "app", "restart"),
				Check:  checkChanges(f),
			},
			{
				Config: actionTriggerTF(path, "v2\n", "app", "reload"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkChanges(f, "reload app.service"),
					func(*terraform.State) error {
						if got := f.get("app.service").reloads; got != 1 {
							return fmt.Errorf("reloads = %d, want 1", got)
						}
						return nil
					},
				),
			},
			{
				Config: actionTriggerTF(path, "v3\n", "app", "stop"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkChanges(f, "stop app.service"),
					checkFakeService(f, "app.service", false, false),
				),
			},
			{
				Config: actionTriggerTF(path, "v4\n", "app", "start"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkChanges(f, "start app.service"),
					checkFakeService(f, "app.service", false, true),
				),
			},
			{
				// The file is written before the action fails.
				Config:      actionTriggerTF(path, "v5\n", "missing", "restart"),
				ExpectError: regexp.MustCompile(`systemd has no unit named "missing"`),
			},
		},
	})
}

// TestAccServiceAction_planErrors checks the errors that fail the plan,
// before anything changes.
func TestAccServiceAction_planErrors(t *testing.T) {
	f := newFakeInit(t, initSystemOpenRC)
	dir := t.TempDir()
	path := filepath.Join(dir, "app.conf")

	resource.Test(t, resource.TestCase{
		TerraformVersionChecks:   actionVersionChecks,
		ProtoV6ProviderFactories: f.providerFactories(),
		Steps: []resource.TestStep{
			{
				Config:      actionTriggerTF(path, "v1\n", "app", "kill"),
				ExpectError: regexp.MustCompile(`(?s)value must be one of:.*"restart"`),
			},
			{
				Config:      actionTriggerTF(path, "v1\n", "getty@tty1", "restart"),
				ExpectError: regexp.MustCompile(`not\s+a\s+valid\s+OpenRC\s+service\s+name`),
			},
			{
				Config: actionTriggerTF(path, "v1\n", "app", "restart") + `
action "sysutils_systemd_daemon_reload" "this" {}

resource "terraform_data" "reload" {
  input = "v1"
  lifecycle {
    action_trigger {
      events  = [after_create]
      actions = [action.sysutils_systemd_daemon_reload.this]
    }
  }
}
`,
				ExpectError: regexp.MustCompile(`needs\s+systemd,\s+but\s+openrc\s+manages\s+services`),
			},
			{
				Config: `
provider "sysutils" {
  root_dir = ` + fmt.Sprintf("%q", dir) + `
}
` + actionTriggerTF("/app.conf", "v1\n", "app", "restart"),
				ExpectError: regexp.MustCompile(`This\s+action\s+changes\s+the\s+running\s+host`),
			},
		},
	})
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a failed plan wrote %s: %v", path, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "app.conf")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a failed plan wrote below root_dir: %v", err)
	}
}

// installActionTestUnit installs a service that copies the file conf to
// seen and prints $GREETING into greeting when it starts, and then keeps
// running.
func installActionTestUnit(t *testing.T, conf, seen, greeting string) string {
	t.Helper()
	name := "tfacc-sysutils-act-" + randomID() + ".service"
	unitPath := filepath.Join(defaultSystemdUnitDir, name)
	content := fmt.Sprintf("[Unit]\nDescription=sysutils_service action test\n\n[Service]\nExecStart=/bin/sh -c 'cp %s %s; echo \"$GREETING\" > %s; exec sleep infinity'\nExecReload=/bin/sh -c 'cp %s %s'\n",
		conf, seen, greeting, conf, seen)
	if err := os.WriteFile(unitPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("systemctl", "stop", "--", name).Run()
		_ = os.RemoveAll(unitPath + ".d")
		_ = os.Remove(unitPath)
		_ = exec.Command("systemctl", "daemon-reload").Run()
	})
	systemctlRun(t, "daemon-reload")
	return name
}

// TestAccServiceAction_systemdRestartOnConfigChange is the documented
// pattern: a changed configuration file restarts the service that reads
// it, and an unchanged one leaves it alone.
func TestAccServiceAction_systemdRestartOnConfigChange(t *testing.T) {
	requireSystemd(t)
	dir := t.TempDir()
	conf, seen := filepath.Join(dir, "app.conf"), filepath.Join(dir, "seen")
	name := installActionTestUnit(t, conf, seen, filepath.Join(dir, "greeting"))
	systemctlRun(t, "start", "--", name)
	invocation := func() string {
		return systemctlOutput(t, "show", "--property=InvocationID", "--value", "--", name)
	}
	var last string
	save := func(*terraform.State) error { last = invocation(); return nil }
	restarted := func(want bool) resource.TestCheckFunc {
		return func(*terraform.State) error {
			if got := invocation(); (got != last) != want {
				return fmt.Errorf("restarted = %v, want %v", got != last, want)
			}
			return nil
		}
	}

	resource.Test(t, resource.TestCase{
		TerraformVersionChecks:   actionVersionChecks,
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				PreConfig: func() { last = invocation() },
				Config:    actionTriggerTF(conf, "v1\n", name, "restart"),
				Check: resource.ComposeAggregateTestCheckFunc(
					restarted(true),
					checkFileContent(seen, "v1\n"),
					checkRealUnit(name, "static", "active"),
					save,
				),
			},
			{
				Config: actionTriggerTF(conf, "v1\n", name, "restart"),
				Check:  restarted(false),
			},
			{
				Config: actionTriggerTF(conf, "v2\n", name, "restart"),
				Check: resource.ComposeAggregateTestCheckFunc(
					restarted(true),
					checkFileContent(seen, "v2\n"),
					save,
				),
			},
			{
				// ExecReload copies the file without a restart.
				Config: actionTriggerTF(conf, "v3\n", name, "reload"),
				Check: resource.ComposeAggregateTestCheckFunc(
					restarted(false),
					checkFileContent(seen, "v3\n"),
				),
			},
			{
				Config: actionTriggerTF(conf, "v4\n", name, "stop"),
				Check:  checkRealUnit(name, "static", "inactive"),
			},
			{
				Config: actionTriggerTF(conf, "v5\n", name, "start"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkRealUnit(name, "static", "active"),
					checkFileContent(seen, "v5\n"),
				),
			},
		},
	})
}

// TestAccSystemdDaemonReloadAction_dropIn writes a drop-in with
// sysutils_file, which does not reload systemd itself, and checks that the
// triggered daemon-reload makes systemd load it without restarting the
// unit, and that a following restart action starts the unit with it.
func TestAccSystemdDaemonReloadAction_dropIn(t *testing.T) {
	requireSystemd(t)
	dir := t.TempDir()
	conf, greeting := filepath.Join(dir, "app.conf"), filepath.Join(dir, "greeting")
	if err := os.WriteFile(conf, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	name := installActionTestUnit(t, conf, filepath.Join(dir, "seen"), greeting)
	dropInDir := filepath.Join(defaultSystemdUnitDir, name+".d")
	if err := os.Mkdir(dropInDir, 0o755); err != nil {
		t.Fatal(err)
	}
	systemctlRun(t, "start", "--", name)
	firstInvocation := systemctlOutput(t, "show", "--property=InvocationID", "--value", "--", name)

	cfg := func(greeting string, restart bool) string {
		actions := "action.sysutils_systemd_daemon_reload.this"
		if restart {
			actions += ", action.sysutils_service.restart"
		}
		return fmt.Sprintf(`
resource "sysutils_file" "dropin" {
  path    = %q
  content = "[Service]\nEnvironment=GREETING=%s\n"

  lifecycle {
    action_trigger {
      events  = [after_create, after_update]
      actions = [%s]
    }
  }
}

action "sysutils_systemd_daemon_reload" "this" {
  config {
    timeout = "1m"
  }
}

action "sysutils_service" "restart" {
  config {
    name   = %q
    action = "restart"
  }
}
`, filepath.Join(dropInDir, "greeting.conf"), greeting, actions, name)
	}
	environment := func(want string) resource.TestCheckFunc {
		return func(*terraform.State) error {
			if got := systemctlOutput(t, "show", "--property=Environment", "--value", "--", name); got != want {
				return fmt.Errorf("Environment = %q, want %q", got, want)
			}
			return nil
		}
	}

	resource.Test(t, resource.TestCase{
		TerraformVersionChecks:   actionVersionChecks,
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: cfg("hello", false),
				Check: resource.ComposeAggregateTestCheckFunc(
					environment("GREETING=hello"),
					func(*terraform.State) error {
						if systemctlOutput(t, "show", "--property=NeedDaemonReload", "--value", "--", name) != "no" {
							return errors.New("systemd still needs a daemon-reload")
						}
						if got := systemctlOutput(t, "show", "--property=InvocationID", "--value", "--", name); got != firstInvocation {
							return errors.New("daemon-reload restarted the unit")
						}
						return nil
					},
					checkFileContent(greeting, "\n"),
				),
			},
			{
				// Actions run in the order listed.
				Config: cfg("world", true),
				Check: resource.ComposeAggregateTestCheckFunc(
					environment("GREETING=world"),
					checkFileContent(greeting, "world\n"),
				),
			},
		},
	})
}

// TestAccServiceAction_openrc restarts and reloads a real OpenRC service.
func TestAccServiceAction_openrc(t *testing.T) {
	requireOpenRC(t)
	dir := t.TempDir()
	conf, seen := filepath.Join(dir, "app.conf"), filepath.Join(dir, "seen")
	name := "tfacc-sysutils-act-" + randomID()
	script := filepath.Join("/etc/init.d", name)
	content := fmt.Sprintf(`#!/sbin/openrc-run
command=/bin/sleep
command_args=1000000
command_background=yes
pidfile=/run/%[1]s.pid
extra_started_commands="reload"

start_pre() {
	cp %[2]s %[3]s
}

reload() {
	ebegin "Reloading %[1]s"
	cp %[2]s %[3]s
	eend $?
}
`, name, conf, seen)
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("rc-service", name, "stop").Run()
		_ = os.Remove(script)
	})
	pid := func() string {
		data, _ := os.ReadFile("/run/" + name + ".pid")
		return strings.TrimSpace(string(data))
	}
	var lastPID string

	resource.Test(t, resource.TestCase{
		TerraformVersionChecks:   actionVersionChecks,
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: actionTriggerTF(conf, "v1\n", name, "start"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkRealOpenRCService(name, false, true),
					checkFileContent(seen, "v1\n"),
					func(*terraform.State) error { lastPID = pid(); return nil },
				),
			},
			{
				Config: actionTriggerTF(conf, "v2\n", name, "restart"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkRealOpenRCService(name, false, true),
					checkFileContent(seen, "v2\n"),
					func(*terraform.State) error {
						if p := pid(); p == "" || p == lastPID {
							return fmt.Errorf("pid %q, was %q; not restarted", p, lastPID)
						}
						lastPID = pid()
						return nil
					},
				),
			},
			{
				Config: actionTriggerTF(conf, "v3\n", name, "reload"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(seen, "v3\n"),
					func(*terraform.State) error {
						if p := pid(); p != lastPID {
							return fmt.Errorf("pid %q, was %q; reload restarted the service", p, lastPID)
						}
						return nil
					},
				),
			},
			{
				Config: actionTriggerTF(conf, "v4\n", name, "stop"),
				Check:  checkRealOpenRCService(name, false, false),
			},
		},
	})
}
