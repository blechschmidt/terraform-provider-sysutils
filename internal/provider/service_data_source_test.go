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
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

func serviceDataHCL(label, name, extra string) string {
	return fmt.Sprintf(`
data "sysutils_service" %q {
  name = %q
%s
}
`, label, name, extra)
}

// expectServiceData returns state checks of data.sysutils_service.<label>;
// attrs maps attribute names to their expected values, nil for null.
func expectServiceData(label string, attrs map[string]any) []statecheck.StateCheck {
	var checks []statecheck.StateCheck
	for attr, v := range attrs {
		var c knownvalue.Check
		switch v := v.(type) {
		case nil:
			c = knownvalue.Null()
		case bool:
			c = knownvalue.Bool(v)
		case string:
			c = knownvalue.StringExact(v)
		default:
			panic(fmt.Sprintf("unsupported value %T", v))
		}
		checks = append(checks, statecheck.ExpectKnownValue("data.sysutils_service."+label, tfjsonpath.New(attr), c))
	}
	return checks
}

func TestServiceDataSource_systemd(t *testing.T) {
	f := newFakeInit(t, initSystemSystemd)
	f.add("app.service", fakeService{enabled: true, running: true})
	f.add("ssh.service", fakeService{running: true})
	f.aliases["sshd.service"] = "ssh.service"
	f.add("static.service", fakeService{static: true})

	config := serviceDataHCL("app", "app", "") +
		serviceDataHCL("alias", "sshd", "") +
		serviceDataHCL("static", "static.service", "") +
		serviceDataHCL("missing", "nosuch.service", "")
	checks := expectServiceData("app", map[string]any{
		"id": "app", "init_system": "systemd", "supported": true, "exists": true, "unit": "app.service",
		"enabled": true, "enabled_state": "enabled", "running": true, "active_state": "active", "runlevel": nil,
	})
	checks = append(checks, expectServiceData("alias", map[string]any{
		"exists": true, "unit": "ssh.service", "enabled": false, "enabled_state": "disabled", "running": true, "active_state": "active",
	})...)
	checks = append(checks, expectServiceData("static", map[string]any{
		"exists": true, "enabled": false, "enabled_state": "static", "running": false, "active_state": "inactive",
	})...)
	checks = append(checks, expectServiceData("missing", map[string]any{
		"init_system": "systemd", "supported": true, "exists": false, "unit": nil,
		"enabled": false, "enabled_state": nil, "running": false, "active_state": nil,
	})...)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		Steps: []resource.TestStep{
			{
				Config:            config,
				ConfigStateChecks: checks,
				// Reading changes nothing.
				Check: checkChanges(f),
			},
			{
				// Changes outside Terraform show up on the next read.
				PreConfig: func() { f.set("app.service", false, false) },
				Config:    serviceDataHCL("app", "app", ""),
				ConfigStateChecks: expectServiceData("app", map[string]any{
					"enabled": false, "enabled_state": "disabled", "running": false, "active_state": "inactive",
				}),
				Check: checkChanges(f),
			},
		},
	})
}

func TestServiceDataSource_openrc(t *testing.T) {
	f := newFakeInit(t, initSystemOpenRC)
	f.add("sshd", fakeService{runlevels: []string{defaultOpenRCRunlevel}, running: true})
	f.add("crond", fakeService{runlevels: []string{"boot"}})

	config := serviceDataHCL("sshd", "sshd", "") +
		serviceDataHCL("crond", "crond", "") +
		serviceDataHCL("crond_boot", "crond", `  runlevel = "boot"`) +
		serviceDataHCL("missing", "nosuch", "")
	checks := expectServiceData("sshd", map[string]any{
		"init_system": "openrc", "supported": true, "exists": true, "unit": "sshd",
		"enabled": true, "enabled_state": "enabled", "running": true, "active_state": "started",
	})
	checks = append(checks, expectServiceData("crond", map[string]any{
		"exists": true, "enabled": false, "enabled_state": "disabled", "running": false, "active_state": "stopped",
	})...)
	checks = append(checks, expectServiceData("crond_boot", map[string]any{
		"runlevel": "boot", "enabled": true, "enabled_state": "enabled",
	})...)
	checks = append(checks, expectServiceData("missing", map[string]any{
		"exists": false, "unit": nil, "enabled": false, "running": false,
	})...)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		Steps: []resource.TestStep{
			{
				Config:            config,
				ConfigStateChecks: checks,
				Check:             checkChanges(f),
			},
			{
				// A systemd unit name is not an OpenRC service name.
				Config:      serviceDataHCL("unit", "getty@tty1.service", ""),
				ExpectError: regexp.MustCompile(`Invalid\s+service\s+name`),
			},
		},
	})
}

// TestServiceDataSource_unsupportedInitSystem checks that reading succeeds
// without systemd or OpenRC, and runs no command.
func TestServiceDataSource_unsupportedInitSystem(t *testing.T) {
	for pid1, want := range map[string]any{"init": "sysvinit", "sleep": nil} {
		t.Run(pid1, func(t *testing.T) {
			f := newFakeInit(t, "")
			procDir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(procDir, "1"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(procDir, "1", "comm"), []byte(pid1+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg := f.config()
			cfg.procDir = procDir
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: map[string]func() (tfprotov6.ProviderServer, error){
					"sysutils": providerserver.NewProtocol6WithError(&sysutilsProvider{version: "test", service: cfg}),
				},
				Steps: []resource.TestStep{{
					Config: serviceDataHCL("test", "nginx", ""),
					ConfigStateChecks: expectServiceData("test", map[string]any{
						"id": "nginx", "init_system": want, "supported": false, "exists": nil, "unit": nil,
						"enabled": nil, "enabled_state": nil, "running": nil, "active_state": nil,
					}),
				}},
			})
			if len(f.calls) != 0 {
				t.Errorf("commands ran without a supported init system: %q", f.calls)
			}
		})
	}
}

func TestServiceDataSource_errors(t *testing.T) {
	f := newFakeInit(t, initSystemSystemd)
	f.add("app.service", fakeService{})
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		Steps: []resource.TestStep{
			{
				Config:      serviceDataHCL("test", "-app", ""),
				ExpectError: regexp.MustCompile(`Invalid\s+service\s+name`),
			},
			{
				Config:      serviceDataHCL("test", "app", `  runlevel = "../x"`),
				ExpectError: regexp.MustCompile(`Invalid\s+runlevel`),
			},
			{
				Config: fmt.Sprintf(`
provider "sysutils" {
  root_dir = %q
}
`, t.TempDir()) + serviceDataHCL("test", "app", ""),
				ExpectError: regexp.MustCompile(`Not\s+supported\s+with\s+root_dir`),
			},
		},
	})
	if len(f.calls) != 0 {
		t.Errorf("commands ran for invalid configurations: %q", f.calls)
	}

	// systemd runs, but systemctl is missing.
	f = newFakeInit(t, initSystemSystemd)
	cfg := f.config()
	cfg.lookPath = func(name string) (string, error) { return "", fmt.Errorf("%s: not found", name) }
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: map[string]func() (tfprotov6.ProviderServer, error){
			"sysutils": providerserver.NewProtocol6WithError(&sysutilsProvider{version: "test", service: cfg}),
		},
		Steps: []resource.TestStep{{
			Config:      serviceDataHCL("test", "app", ""),
			ExpectError: regexp.MustCompile(`systemctl\s+was\s+not\s+found`),
		}},
	})
}
