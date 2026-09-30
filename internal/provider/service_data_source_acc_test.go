package provider

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

const testServiceDataSource = "data.sysutils_service.test"

// TestAccServiceDataSource_systemd reads a test unit before and after it is
// enabled and started outside Terraform, and a unit that does not exist.
func TestAccServiceDataSource_systemd(t *testing.T) {
	requireSystemd(t)
	name := installTestUnit(t)
	missing := "tfacc-sysutils-nosuch-" + randomID() + ".service"
	config := serviceDataHCL("test", name, "") + serviceDataHCL("missing", missing, "")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testServiceDataSource, "init_system", "systemd"),
					resource.TestCheckResourceAttr(testServiceDataSource, "supported", "true"),
					resource.TestCheckResourceAttr(testServiceDataSource, "exists", "true"),
					resource.TestCheckResourceAttr(testServiceDataSource, "unit", name),
					resource.TestCheckResourceAttr(testServiceDataSource, "enabled", "false"),
					resource.TestCheckResourceAttr(testServiceDataSource, "enabled_state", "disabled"),
					resource.TestCheckResourceAttr(testServiceDataSource, "running", "false"),
					resource.TestCheckResourceAttr(testServiceDataSource, "active_state", "inactive"),
					resource.TestCheckResourceAttr("data.sysutils_service.missing", "exists", "false"),
					resource.TestCheckNoResourceAttr("data.sysutils_service.missing", "unit"),
					resource.TestCheckResourceAttr("data.sysutils_service.missing", "running", "false"),
					// Reading changed nothing.
					checkRealUnit(name, "disabled", "inactive"),
				),
			},
			{
				PreConfig: func() { systemctlRun(t, "enable", "--now", "--", name) },
				// Without the suffix, as systemctl accepts it.
				Config: serviceDataHCL("test", name[:len(name)-len(".service")], ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testServiceDataSource, "unit", name),
					resource.TestCheckResourceAttr(testServiceDataSource, "enabled", "true"),
					resource.TestCheckResourceAttr(testServiceDataSource, "enabled_state", "enabled"),
					resource.TestCheckResourceAttr(testServiceDataSource, "running", "true"),
					resource.TestCheckResourceAttr(testServiceDataSource, "active_state", "active"),
				),
			},
		},
	})
}

// TestAccServiceDataSource_openrc does the same with OpenRC, as on Alpine.
func TestAccServiceDataSource_openrc(t *testing.T) {
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
	missing := "tfacc-sysutils-nosuch-" + randomID()
	config := serviceDataHCL("test", name, "") + serviceDataHCL("missing", missing, "")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testServiceDataSource, "init_system", "openrc"),
					resource.TestCheckResourceAttr(testServiceDataSource, "supported", "true"),
					resource.TestCheckResourceAttr(testServiceDataSource, "exists", "true"),
					resource.TestCheckResourceAttr(testServiceDataSource, "unit", name),
					resource.TestCheckResourceAttr(testServiceDataSource, "enabled", "false"),
					resource.TestCheckResourceAttr(testServiceDataSource, "enabled_state", "disabled"),
					resource.TestCheckResourceAttr(testServiceDataSource, "running", "false"),
					resource.TestCheckResourceAttr(testServiceDataSource, "active_state", "stopped"),
					resource.TestCheckResourceAttr("data.sysutils_service.missing", "exists", "false"),
					resource.TestCheckNoResourceAttr("data.sysutils_service.missing", "unit"),
					checkRealOpenRCService(name, false, false),
				),
			},
			{
				PreConfig: func() {
					for _, argv := range [][]string{{"rc-update", "add", name, defaultOpenRCRunlevel}, {"rc-service", name, "start"}} {
						if out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput(); err != nil {
							t.Fatalf("%v: %v: %s", argv, err, out)
						}
					}
				},
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testServiceDataSource, "enabled", "true"),
					resource.TestCheckResourceAttr(testServiceDataSource, "enabled_state", "enabled"),
					resource.TestCheckResourceAttr(testServiceDataSource, "running", "true"),
					resource.TestCheckResourceAttr(testServiceDataSource, "active_state", "started"),
				),
			},
			{
				// Enabled in another runlevel only.
				Config: serviceDataHCL("test", name, `  runlevel = "boot"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testServiceDataSource, "enabled", "false"),
					resource.TestCheckResourceAttr(testServiceDataSource, "running", "true"),
				),
			},
		},
	})
}

// TestAccServiceDataSource_unsupportedInitSystem checks that reading
// succeeds where neither systemd nor OpenRC runs, as in most containers. It
// needs neither root nor an init system.
func TestAccServiceDataSource_unsupportedInitSystem(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("acceptance test; set TF_ACC=1 to run")
	}
	kind := (&serviceConfig{}).probe().kind
	if kind == initSystemSystemd || kind == initSystemOpenRC {
		t.Skipf("%s is running, which the data source supports", kind)
	}
	initSystem := resource.TestCheckNoResourceAttr(testServiceDataSource, "init_system")
	if kind != "" {
		initSystem = resource.TestCheckResourceAttr(testServiceDataSource, "init_system", kind)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: serviceDataHCL("test", "sshd", ""),
			Check: resource.ComposeAggregateTestCheckFunc(
				initSystem,
				resource.TestCheckResourceAttr(testServiceDataSource, "supported", "false"),
				resource.TestCheckNoResourceAttr(testServiceDataSource, "exists"),
				resource.TestCheckNoResourceAttr(testServiceDataSource, "enabled"),
				resource.TestCheckNoResourceAttr(testServiceDataSource, "running"),
			),
		}},
	})
}
