package provider

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

func structuredUnitConfig(name, description, extra string) string {
	return fmt.Sprintf(`
resource "sysutils_systemd_unit" "test" {
  name = %q

  unit = {
    description = %q
    after       = ["network-online.target", "time-sync.target"]
    wants       = ["network-online.target"]
    extra = {
      "X-Owner" = ["team-a"]
    }
  }

  service = {
    type              = "simple"
    exec_start_pre    = ["", "/bin/true"]
    exec_start        = ["/bin/sleep infinity"]
    environment       = ["A=1", "\"B=two words\""]
    restart           = "on-failure"
    remain_after_exit = false
    nice              = 5
  }

  install = {
    wanted_by = ["multi-user.target"]
  }

  extra_sections = {
    "X-Meta" = {
      Key = ["v1", "v2"]
    }
  }
%s
}`, name, description, extra)
}

func structuredUnitFile(description string) string {
	return `[Unit]
Description=` + description + `
Wants=network-online.target
After=network-online.target
After=time-sync.target
X-Owner=team-a

[Service]
ExecStartPre=
ExecStartPre=/bin/true
ExecStart=/bin/sleep infinity
Type=simple
Restart=on-failure
RemainAfterExit=false
Nice=5
Environment=A=1
Environment="B=two words"

[Install]
WantedBy=multi-user.target

[X-Meta]
Key=v1
Key=v2
`
}

func TestSystemdUnit_structured(t *testing.T) {
	f := newFakeSystemd(t)
	const name = "tfstructured.service"
	managed := `
  enabled = true
  state   = "running"`

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		CheckDestroy:             checkFakeUnitRemoved(f, name),
		Steps: []resource.TestStep{
			{
				Config: structuredUnitConfig(name, "v1", managed),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						// The file is known at plan time.
						plancheck.ExpectKnownValue(testSystemdUnitResource, tfjsonpath.New("content"), knownvalue.StringExact(structuredUnitFile("v1"))),
						plancheck.ExpectKnownValue(testSystemdUnitResource, tfjsonpath.New("content_sha256"), knownvalue.StringExact(sha256Hex([]byte(structuredUnitFile("v1"))))),
						plancheck.ExpectKnownValue(testSystemdUnitResource, tfjsonpath.New("socket"), knownvalue.Null()),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(f.unitFile(name), structuredUnitFile("v1")),
					resource.TestCheckResourceAttr(testSystemdUnitResource, "content", structuredUnitFile("v1")),
					resource.TestCheckResourceAttr(testSystemdUnitResource, "service.exec_start.0", "/bin/sleep infinity"),
					resource.TestCheckResourceAttr(testSystemdUnitResource, "service.exec_start_pre.0", ""),
					resource.TestCheckResourceAttr(testSystemdUnitResource, "service.nice", "5"),
					resource.TestCheckResourceAttr(testSystemdUnitResource, "service.remain_after_exit", "false"),
					resource.TestCheckNoResourceAttr(testSystemdUnitResource, "service.user"),
					checkFakeUnit(f, name, true, "active"),
				),
			},
			{
				// A changed property restarts the unit.
				Config: structuredUnitConfig(name, "v2", managed),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testSystemdUnitResource, plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue(testSystemdUnitResource, tfjsonpath.New("unit").AtMapKey("description"), knownvalue.StringExact("v2")),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(f.unitFile(name), structuredUnitFile("v2")),
					checkCalls(f, "daemon-reload", "", 2),
					checkCalls(f, "try-restart", name, 1),
					checkCalls(f, "reenable", name, 1),
				),
			},
			{
				// An out-of-band change of a directive is drift of that
				// attribute, and is reverted.
				PreConfig: func() {
					writeTestFile(t, f.unitFile(name), structuredUnitFile("edited"))
				},
				Config: structuredUnitConfig(name, "v2", managed),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testSystemdUnitResource, plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(f.unitFile(name), structuredUnitFile("v2")),
					checkCalls(f, "try-restart", name, 2),
				),
			},
			{
				// A comment or reformatting is reverted too, but systemd
				// reads the same unit, so it is not restarted.
				PreConfig: func() {
					writeTestFile(t, f.unitFile(name), "# edited by hand\n"+structuredUnitFile("v2"))
				},
				Config: structuredUnitConfig(name, "v2", managed),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testSystemdUnitResource, plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(f.unitFile(name), structuredUnitFile("v2")),
					checkCalls(f, "daemon-reload", "", 4),
					checkCalls(f, "try-restart", name, 2),
					checkCalls(f, "reenable", name, 2),
				),
			},
			{
				ResourceName:      testSystemdUnitResource,
				ImportState:       true,
				ImportStateId:     name,
				ImportStateVerify: true,
			},
			{
				// Switching to content with the same file is no change.
				Config: systemdUnitConfig(name, structuredUnitFile("v2"), managed),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}

func writeTestFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A hand-written unit configured with content can be moved to attributes:
// the plan shows the canonical file as the only change.
func TestSystemdUnit_contentToStructured(t *testing.T) {
	f := newFakeSystemd(t)
	const name = "tfmigrate.timer"
	content := "# Daily job\n[Timer]\nOnCalendar = daily\nPersistent=true\n\n[Install]\nWantedBy=timers.target\n"

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		CheckDestroy:             checkFakeUnitRemoved(f, name),
		Steps: []resource.TestStep{
			{
				Config: systemdUnitConfig(name, content, ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testSystemdUnitResource, "timer.on_calendar.#", "1"),
					resource.TestCheckResourceAttr(testSystemdUnitResource, "timer.on_calendar.0", "daily"),
					resource.TestCheckResourceAttr(testSystemdUnitResource, "timer.persistent", "true"),
					resource.TestCheckNoResourceAttr(testSystemdUnitResource, "unit.%"),
				),
			},
			{
				Config: fmt.Sprintf(`
resource "sysutils_systemd_unit" "test" {
  name = %q
  timer = {
    on_calendar = ["daily"]
    persistent  = true
  }
  install = {
    wanted_by = ["timers.target"]
  }
}`, name),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testSystemdUnitResource, plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue(testSystemdUnitResource, tfjsonpath.New("content"),
							knownvalue.StringExact("[Timer]\nOnCalendar=daily\nPersistent=true\n\n[Install]\nWantedBy=timers.target\n")),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(f.unitFile(name), "[Timer]\nOnCalendar=daily\nPersistent=true\n\n[Install]\nWantedBy=timers.target\n"),
					// Only formatting changed, so nothing was restarted.
					checkCalls(f, "try-restart", name, 0),
					checkCalls(f, "reenable", name, 0),
				),
			},
		},
	})
}

// Values that are not known until apply make the file unknown at plan time.
func TestSystemdUnit_structuredUnknownValues(t *testing.T) {
	f := newFakeSystemd(t)
	const name = "tfunknown.service"
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		CheckDestroy:             checkFakeUnitRemoved(f, name),
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "terraform_data" "cmd" {
  input = "/bin/sleep infinity"
}

resource "sysutils_systemd_unit" "test" {
  name = %q
  service = {
    exec_start = [terraform_data.cmd.output]
  }
}`, name),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectUnknownValue(testSystemdUnitResource, tfjsonpath.New("content")),
						plancheck.ExpectUnknownValue(testSystemdUnitResource, tfjsonpath.New("content_sha256")),
						plancheck.ExpectKnownValue(testSystemdUnitResource, tfjsonpath.New("unit"), knownvalue.Null()),
					},
				},
				Check: checkFileContent(f.unitFile(name), "[Service]\nExecStart=/bin/sleep infinity\n"),
			},
		},
	})
}

func TestSystemdUnit_structuredInvalid(t *testing.T) {
	f := newFakeSystemd(t)
	cfg := func(name, body string) string {
		return fmt.Sprintf("resource \"sysutils_systemd_unit\" \"test\" {\n  name = %q\n%s\n}", name, body)
	}
	tests := []struct {
		name, unit, body, want string
	}{
		{"section of another type", "a.service", `  timer = { on_calendar = ["daily"] }`, `The\s+\[Timer\]\s+section\s+is\s+only\s+valid\s+for\s+.timer\s+units`},
		{"path section on a service", "a.service", `  path_section = { path_exists = ["/x"] }`, `\[Path\]\s+section\s+is\s+only\s+valid`},
		{"content and sections", "a.service", "  content = \"\"\n  service = {}", `Exactly\s+one\s+of`},
		{"known directive in extra", "a.service", `  service = { extra = { ExecStart = ["/bin/true"] } }`, `Set\s+ExecStart=\s+with\s+the\s+attribute\s+service.exec_start`},
		{"known section in extra_sections", "a.service", `  extra_sections = { Service = { User = ["x"] } }`, `(?i)section\s+"Service"\s+has\s+its\s+own\s+attribute`},
		{"newline in value", "a.service", `  unit = { description = "a\nb" }`, `must\s+not\s+contain\s+line\s+breaks`},
		{"line continuation", "a.service", `  unit = { description = "a\\" }`, `odd\s+number\s+of\s+backslashes`},
		{"leading whitespace", "a.service", `  unit = { description = " a" }`, `must\s+not\s+start\s+or\s+end\s+with\s+whitespace`},
		{"empty list", "a.service", `  unit = { after = [] }`, `at\s+least\s+1`},
		{"null element", "a.service", `  unit = { after = [null] }`, `null`},
		{"null extra section", "a.service", `  extra_sections = { "X-A" = null }`, `null`},
		{"bad extra key", "a.service", `  unit = { extra = { "A=B" = ["x"] } }`, `directive\s+name\s+"A=B"`},
		{"empty extra list", "a.service", `  unit = { extra = { "X-A" = [] } }`, `at\s+least\s+1`},
		{"bad section name", "a.service", `  extra_sections = { "a]b" = {} }`, `section\s+name\s+"a]b"`},
		{"scope unit file", "a.scope", `  unit = {}`, `Invalid\s+unit\s+name`},
		{"template with state", "a@.service", "  state = \"running\"\n  unit = {}", `Templates\s+cannot\s+be\s+started`},
	}
	var steps []resource.TestStep
	for _, tt := range tests {
		steps = append(steps, resource.TestStep{
			Config:      cfg(tt.unit, tt.body),
			PlanOnly:    true,
			ExpectError: regexp.MustCompile("(?i)" + tt.want),
		})
	}
	resource.UnitTest(t, resource.TestCase{ProtoV6ProviderFactories: f.providerFactories(), Steps: steps})
}

func TestSystemdUnit_template(t *testing.T) {
	f := newFakeSystemd(t)
	const name = "tftmpl@.service"
	config := func(description string) string {
		return fmt.Sprintf(`
resource "sysutils_systemd_unit" "test" {
  name    = %q
  enabled = true
  unit = {
    description = "instance %%i, %s"
  }
  service = {
    exec_start = ["/bin/sleep infinity"]
  }
  install = {
    wanted_by        = ["multi-user.target"]
    default_instance = "main"
  }
}`, name, description)
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		CheckDestroy: func(*terraform.State) error {
			if _, err := os.Lstat(f.unitFile(name)); !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("template file left behind: %v", err)
			}
			for _, inst := range []string{"tftmpl@a.service", "tftmpl@b.service"} {
				if _, active := f.get(inst); active != "inactive" {
					return fmt.Errorf("instance %s is still %s", inst, active)
				}
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config: config("v1"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testSystemdUnitResource, "enabled", "true"),
					resource.TestCheckNoResourceAttr(testSystemdUnitResource, "state"),
					resource.TestCheckResourceAttr(testSystemdUnitResource, "install.default_instance", "main"),
					// The template was checked through an instance.
					checkCalls(f, "show", "tftmpl@sysutils-load-check.service", 1),
				),
			},
			{
				// Instances started outside of the resource are restarted
				// when the template changes; stopped ones are not.
				PreConfig: func() { f.set("tftmpl@a.service", false, "active"); f.set("tftmpl@b.service", false, "inactive") },
				Config:    config("v2"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkCalls(f, "try-restart", "tftmpl@a.service", 1),
					checkCalls(f, "try-restart", "tftmpl@b.service", 0),
					checkCalls(f, "reenable", name, 1),
					resource.TestCheckNoResourceAttr(testSystemdUnitResource, "state"),
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
	if got := f.callsOf("stop", "tftmpl@a.service"); got != 1 {
		t.Errorf("running instance stopped %d times on destroy, want 1", got)
	}
}

func TestSystemdUnit_templateRefusesExisting(t *testing.T) {
	f := newFakeSystemd(t)
	writeTestFile(t, f.unitFile("other@.service"), testUnitV1)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		Steps: []resource.TestStep{{
			Config:      systemdUnitConfig("other@.service", testUnitV1, ""),
			ExpectError: regexp.MustCompile(`already\s+exists;\s+import\s+it`),
		}},
	})
}

const testDropInResource = "sysutils_systemd_dropin.test"

func dropInConfig(unitName, name, body string) string {
	return fmt.Sprintf(`
resource "sysutils_systemd_dropin" "test" {
  unit_name = %q
  name      = %q
%s
}`, unitName, name, body)
}

func TestSystemdDropIn_lifecycle(t *testing.T) {
	f := newFakeSystemd(t)
	const unitName = "tfbase.service"
	writeTestFile(t, f.unitFile(unitName), testUnitV1)
	dropIn := filepath.Join(f.unitDir, unitName+".d", "50-env.conf")
	body := func(v string) string {
		return fmt.Sprintf(`  service = {
    environment = ["MODE=%s"]
    exec_start  = ["", "/bin/sleep 1000"]
  }`, v)
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		PreCheck: func() {
			f.mu.Lock()
			f.loaded[unitName] = testUnitV1
			f.active[unitName] = "active"
			f.mu.Unlock()
		},
		CheckDestroy: func(*terraform.State) error {
			if _, err := os.Lstat(filepath.Dir(dropIn)); !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("drop-in directory left behind: %v", err)
			}
			// Removing the drop-in restarted the unit once more.
			return checkCalls(f, "try-restart", unitName, 4)(nil)
		},
		Steps: []resource.TestStep{
			{
				Config: dropInConfig(unitName, "50-env", body("a")),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testDropInResource, "id", unitName+"/50-env"),
					resource.TestCheckResourceAttr(testDropInResource, "path", dropIn),
					resource.TestCheckResourceAttr(testDropInResource, "content", "[Service]\nExecStart=\nExecStart=/bin/sleep 1000\nEnvironment=MODE=a\n"),
					checkFileContent(dropIn, "[Service]\nExecStart=\nExecStart=/bin/sleep 1000\nEnvironment=MODE=a\n"),
					checkFileMode(dropIn, 0o644),
					checkCalls(f, "daemon-reload", "", 1),
					checkCalls(f, "try-restart", unitName, 1),
				),
			},
			{
				Config: dropInConfig(unitName, "50-env", body("b")),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(dropIn, "[Service]\nExecStart=\nExecStart=/bin/sleep 1000\nEnvironment=MODE=b\n"),
					checkCalls(f, "daemon-reload", "", 2),
					checkCalls(f, "try-restart", unitName, 2),
				),
			},
			{
				// Out-of-band edits are drift.
				PreConfig: func() { writeTestFile(t, dropIn, "[Service]\nEnvironment=MODE=x\n") },
				Config:    dropInConfig(unitName, "50-env", body("b")),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testDropInResource, plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(dropIn, "[Service]\nExecStart=\nExecStart=/bin/sleep 1000\nEnvironment=MODE=b\n"),
					checkCalls(f, "try-restart", unitName, 3),
				),
			},
			{
				// restart_on_change = false only reloads.
				Config: dropInConfig(unitName, "50-env", body("c")+"\n  restart_on_change = false"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkCalls(f, "try-restart", unitName, 3),
				),
			},
			{
				Config: dropInConfig(unitName, "50-env", body("c")),
			},
			{
				ResourceName:      testDropInResource,
				ImportState:       true,
				ImportStateId:     unitName + "/50-env",
				ImportStateVerify: true,
			},
		},
	})
}

func TestSystemdDropIn_kinds(t *testing.T) {
	f := newFakeSystemd(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		Steps: []resource.TestStep{
			{
				// Scope units can only be configured by drop-ins.
				Config: dropInConfig("session-tf.scope", "limits", `  scope = { runtime_max_sec = "1h" }
  slice = null
  unit  = { description = "limited" }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(filepath.Join(f.unitDir, "session-tf.scope.d", "limits.conf"), "[Unit]\nDescription=limited\n\n[Scope]\nRuntimeMaxSec=1h\n"),
					checkCalls(f, "try-restart", "", 0),
				),
			},
			{
				// A drop-in for every service.
				Config: dropInConfig("service", "10-all", `  service = { timeout_stop_sec = "20s" }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(filepath.Join(f.unitDir, "service.d", "10-all.conf"), "[Service]\nTimeoutStopSec=20s\n"),
					checkCalls(f, "try-restart", "", 0),
				),
			},
			{
				// A template drop-in is checked through an instance.
				Config: dropInConfig("getty@.service", "override", `  content = "[Service]\nNice=1\n"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testDropInResource, "service.nice", "1"),
					checkCalls(f, "show", "getty@sysutils-load-check.service", 1),
				),
			},
			{
				Config:      dropInConfig("x.service", "a", `  scope = { runtime_max_sec = "1h" }`),
				ExpectError: regexp.MustCompile(`\[Scope\]\s+section\s+is\s+only\s+valid\s+for\s+.scope\s+units`),
			},
			{
				Config:      dropInConfig("x.service", "../a", `  content = ""`),
				ExpectError: regexp.MustCompile(`Invalid\s+drop-in\s+name`),
			},
			{
				Config:      dropInConfig("../x.service", "a", `  content = ""`),
				ExpectError: regexp.MustCompile(`Invalid\s+unit\s+name`),
			},
			{
				Config:      dropInConfig("x.service", "broken", `  content = "[Broken]\n"`),
				ExpectError: regexp.MustCompile(`load\s+state\s+"bad-setting"`),
			},
		},
	})
}

func TestSystemdDropIn_refusesExisting(t *testing.T) {
	f := newFakeSystemd(t)
	if err := os.MkdirAll(filepath.Join(f.unitDir, "x.service.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(f.unitDir, "x.service.d", "a.conf"), "[Service]\n")
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		Steps: []resource.TestStep{{
			Config:      dropInConfig("x.service", "a", `  content = "[Service]\nNice=1\n"`),
			ExpectError: regexp.MustCompile(`Drop-in\s+already\s+exists`),
		}},
	})
}

// A unit file for a mount that exists without one adds settings to it.
// Destroy removes the file but leaves the mount, which was active before.
func TestSystemdUnit_preexistingMount(t *testing.T) {
	f := newFakeSystemd(t)
	const name = "srv-data.mount"
	f.implicit[name] = "active"
	config := func(name string) string {
		return fmt.Sprintf(`
resource "sysutils_systemd_unit" "test" {
  name = %q
  unit = {
    description = "data"
  }
  mount = {
    what  = "/dev/vdb1"
    where = "/srv/data"
  }
}`, name)
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		Steps: []resource.TestStep{
			{Config: config(name)},
		},
		CheckDestroy: func(*terraform.State) error {
			if got := f.callsOf("stop", name); got != 0 {
				return fmt.Errorf("the pre-existing mount was stopped %d times", got)
			}
			if _, err := os.Lstat(f.unitFile(name)); !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("unit file left behind: %v", err)
			}
			return nil
		},
	})
}
