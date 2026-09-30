package provider

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

const testJournaldResource = "sysutils_journald_config.test"

// journaldHCL renders a sysutils_journald_config resource named test.
func journaldHCL(name, body string) string {
	return fmt.Sprintf(`
resource "sysutils_journald_config" "test" {
  name = %q
%s
}
`, name, body)
}

const journaldFullBody = `
  storage           = "persistent"
  system_max_use    = "500M"
  max_retention_sec = "1month"
  compress          = true
  forward_to_syslog = false
  extra = {
    RateLimitBurst = "10000"
    MaxFileSec     = "1week"
  }
`

const journaldFullContent = journaldFileHeader + `
[Journal]
Storage=persistent
SystemMaxUse=500M
MaxRetentionSec=1month
Compress=yes
ForwardToSyslog=no
MaxFileSec=1week
RateLimitBurst=10000
`

// TestAccJournaldConfig_lifecycle manages a drop-in below root_dir through
// create, drift of content and mode, import, update, removal outside
// Terraform and destroy.
func TestAccJournaldConfig_lifecycle(t *testing.T) {
	root := t.TempDir()
	const managed = "/etc/systemd/journald.conf.d/90-hardening.conf"
	p := filepath.Join(root, managed)
	uid, gid := processOwner()
	updatedBody := `  storage = "volatile"
  extra   = { Compress = "64K" }`
	updated := journaldFileHeader + "\n[Journal]\nStorage=volatile\nCompress=64K\n"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkPathGone(p),
		Steps: []resource.TestStep{
			{
				Config: limitsConfig(root, journaldHCL("90-hardening", journaldFullBody)),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectKnownValue(testJournaldResource, tfjsonpath.New("path"), knownvalue.StringExact(managed)),
						plancheck.ExpectKnownValue(testJournaldResource, tfjsonpath.New("content"), knownvalue.StringExact(journaldFullContent)),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(p, journaldFullContent),
					checkFileMode(p, 0o644),
					checkFileMode(filepath.Dir(p), 0o755),
					checkNoTempFiles(filepath.Dir(p)),
				),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(testJournaldResource, tfjsonpath.New("id"), knownvalue.StringExact("90-hardening")),
					statecheck.ExpectKnownValue(testJournaldResource, tfjsonpath.New("restart"), knownvalue.Bool(false)),
					statecheck.ExpectKnownValue(testJournaldResource, tfjsonpath.New("owner"), knownvalue.StringExact(uidName(uid))),
					statecheck.ExpectKnownValue(testJournaldResource, tfjsonpath.New("group"), knownvalue.StringExact(gidName(gid))),
				},
			},
			{
				ResourceName:      testJournaldResource,
				ImportState:       true,
				ImportStateId:     "90-hardening",
				ImportStateVerify: true,
			},
			{
				// A change outside Terraform is drift, which apply reverts.
				PreConfig: setFile(t, p, strings.Replace(journaldFullContent, "Storage=persistent", "Storage=none", 1)),
				Config:    limitsConfig(root, journaldHCL("90-hardening", journaldFullBody)),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testJournaldResource, plancheck.ResourceActionUpdate),
						expectBefore(testJournaldResource, "content", strings.Replace(journaldFullContent, "Storage=persistent", "Storage=none", 1)),
					},
				},
				Check: checkFileContent(p, journaldFullContent),
			},
			{
				PreConfig: func() { mustChmod(t, p, 0o600) },
				Config:    limitsConfig(root, journaldHCL("90-hardening", journaldFullBody)),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testJournaldResource, plancheck.ResourceActionUpdate),
						expectBefore(testJournaldResource, "mode", "0600"),
					},
				},
				Check: checkFileMode(p, 0o644),
			},
			{
				Config: limitsConfig(root, journaldHCL("90-hardening", journaldFullBody)),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				// Unset settings are not written; extra can hold a value
				// the typed attribute can't.
				Config: limitsConfig(root, journaldHCL("90-hardening", updatedBody)),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testJournaldResource, plancheck.ResourceActionUpdate),
					},
				},
				Check: checkFileContent(p, updated),
			},
			{
				ResourceName:      testJournaldResource,
				ImportState:       true,
				ImportStateId:     "90-hardening",
				ImportStateVerify: true,
			},
			{
				PreConfig: func() { _ = os.Remove(p) },
				Config:    limitsConfig(root, journaldHCL("90-hardening", updatedBody)),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testJournaldResource, plancheck.ResourceActionCreate),
					},
				},
				Check: checkFileContent(p, updated),
			},
		},
	})
}

// TestAccJournaldConfig_existingFile checks that create refuses an existing
// drop-in, which can be imported instead.
func TestAccJournaldConfig_existingFile(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "etc", "systemd", "journald.conf.d", "size.conf")
	const orig = "[Journal]\nSystemMaxUse=1G\nForwardToSyslog=yes\n"
	mustMkdir(t, filepath.Dir(p))
	mustWrite(t, p, orig)
	config := limitsConfig(root, journaldHCL("size", `
  system_max_use    = "1G"
  forward_to_syslog = true
`))
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkPathGone(p),
		Steps: []resource.TestStep{
			{
				Config:      config,
				ExpectError: regexp.MustCompile(`journald\.conf\.d/size\.conf\s+already\s+exists(.|\n)*terraform\s+import`),
			},
			{
				PreConfig:          func() { assertFileContent(t, p, orig) },
				Config:             config,
				ResourceName:       testJournaldResource,
				ImportState:        true,
				ImportStateId:      "size",
				ImportStatePersist: true,
			},
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						// Only the header is new.
						plancheck.ExpectResourceAction(testJournaldResource, plancheck.ResourceActionUpdate),
						expectBefore(testJournaldResource, "content", orig),
					},
				},
				Check: checkFileContent(p, journaldFileHeader+"\n"+orig),
			},
		},
	})
}

// TestAccJournaldConfig_validation checks the plan-time validation of every
// attribute.
func TestAccJournaldConfig_validation(t *testing.T) {
	root := t.TempDir()
	step := func(name, body, want string) resource.TestStep {
		return resource.TestStep{
			Config:      limitsConfig(root, journaldHCL(name, body)),
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(want),
		}
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			step("../../../passwd", "", `must\s+consist\s+of\s+letters`),
			step("90-x.conf", "", `must\s+not\s+end\s+with\s+"\.conf"`),
			step(".hidden", "", `must\s+not\s+start\s+with`),
			step("x", `  storage = "disk"`, `value\s+must\s+be\s+one\s+of`),
			step("x", `  system_max_use = "500MB"`, `base\s+1024`),
			step("x", `  max_retention_sec = "1 fortnight"`, `systemd\s+time\s+span`),
			step("x", `  extra = { "rate_limit" = "1" }`, `journald\.conf\s+setting\s+name`),
			step("x", `  extra = { "Storage\n[Unit]\nFoo" = "1" }`, `journald\.conf\s+setting\s+name`),
			step("x", `  extra = { RateLimitBurst = "1\n[Journal]\nStorage=none" }`, `single\s+line`),
			step("x", `  extra = { RateLimitBurst = "1\\" }`, `must\s+not\s+end\s+with\s+a\s+backslash`),
			step("x", "  storage = \"auto\"\n  extra = { Storage = \"none\" }", `Storage\s+is\s+also\s+set\s+by\s+storage`),
			// restart can't restart the journald of a tree.
			step("x", `  restart = true`, `restart\s+the\s+journald\s+of\s+the\s+running\s+host,\s+not\s+of\s+the\s+tree`),
		},
	})
}

func TestAccJournaldConfig_importErrors(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "etc", "systemd", "journald.conf.d")
	mustMkdir(t, dir)
	mustWrite(t, filepath.Join(dir, "unit.conf"), "[Journal]\nStorage=auto\n[Unit]\nDescription=x\n")
	config := limitsConfig(root, journaldHCL("x", ""))
	step := func(id, want string) resource.TestStep {
		return resource.TestStep{
			Config:        config,
			ResourceName:  testJournaldResource,
			ImportState:   true,
			ImportStateId: id,
			ExpectError:   regexp.MustCompile(want),
		}
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			step("x.conf", `without\s+the\s+"\.conf"\s+suffix`),
			step("../x", `without\s+the\s+"\.conf"\s+suffix`),
			step("missing", `journald\.conf\.d/missing\.conf\s+does\s+not\s+exist`),
			step("unit", `section\s+\[Unit\]\s+is\s+not\s+supported`),
		},
	})
}

// journaldFakeFactories returns provider factories whose sysutils_journald_config
// writes to dir and restarts journald through the fake init f.
func journaldFakeFactories(f *fakeInit, dir string) map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		"sysutils": providerserver.NewProtocol6WithError(&sysutilsProvider{version: "test", service: f.config(), journald: &journaldConfig{dir: dir}}),
	}
}

// expectChanges is a check that the fake init saw exactly want since the
// last call, and clears its record.
func expectChanges(f *fakeInit, want ...string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		got := f.changes()
		f.clearCalls()
		if !slices.Equal(got, want) {
			return fmt.Errorf("service changes = %q, want %q", got, want)
		}
		return nil
	}
}

// setFailRestart makes restarting the fake service name fail, or succeed.
func (f *fakeInit) setFailRestart(name string, fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.services[name].failRestart = fail
}

// TestAccJournaldConfig_restart checks when restart restarts journald: on
// create, on a change of the file, on destroy, and again after a failed
// restart, but not for a plan without changes.
func TestAccJournaldConfig_restart(t *testing.T) {
	f := newFakeInit(t, initSystemSystemd)
	f.add(journaldUnit, fakeService{enabled: true, running: true, static: true})
	dir := filepath.Join(t.TempDir(), "journald.conf.d")
	p := filepath.Join(dir, "retention.conf")
	config := func(retention string, restart bool) string {
		return journaldHCL("retention", fmt.Sprintf("  max_retention_sec = %q\n  restart = %t", retention, restart))
	}
	restarted := "restart " + journaldUnit
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: journaldFakeFactories(f, dir),
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkPathGone(p),
			expectChanges(f, restarted),
		),
		Steps: []resource.TestStep{
			{
				Config: config("1month", true),
				Check:  resource.ComposeAggregateTestCheckFunc(checkFileContent(p, journaldFileHeader+"\n[Journal]\nMaxRetentionSec=1month\n"), expectChanges(f, restarted)),
			},
			{
				// No change, no restart; turning restart off and on alone
				// does not restart either.
				Config: config("1month", false),
				Check:  expectChanges(f),
			},
			{
				Config: config("1month", true),
				Check:  expectChanges(f),
			},
			{
				// A changed file without restart is not restarted...
				Config: config("2weeks", false),
				Check:  expectChanges(f),
			},
			{
				// ...and drift repaired with restart is.
				PreConfig: setFile(t, p, "[Journal]\nMaxRetentionSec=1y\n"),
				Config:    config("2weeks", true),
				Check:     expectChanges(f, restarted),
			},
			{
				// A failed restart fails the apply, after the file was
				// written...
				PreConfig:   func() { f.setFailRestart(journaldUnit, true) },
				Config:      config("1week", true),
				ExpectError: regexp.MustCompile(`restarting\s+journald\s+failed(.|\n)*control\s+process\s+exited`),
				Check:       checkFileContent(p, journaldFileHeader+"\n[Journal]\nMaxRetentionSec=1week\n"),
			},
			{
				// ...and the next plan retries it, with a warning.
				PreConfig: func() { f.setFailRestart(journaldUnit, false); f.clearCalls() },
				Config:    config("1week", true),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testJournaldResource, plancheck.ResourceActionUpdate),
					},
				},
				Check: expectChanges(f, restarted),
			},
			{
				Config: config("1week", true),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: expectChanges(f),
			},
		},
	})
}

// TestAccJournaldConfig_restartNeedsSystemd checks that restart is refused
// at plan time on a host that systemd does not run.
func TestAccJournaldConfig_restartNeedsSystemd(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journald.conf.d")
	for _, kind := range []string{initSystemOpenRC, ""} {
		f := newFakeInit(t, kind)
		resource.Test(t, resource.TestCase{
			ProtoV6ProviderFactories: journaldFakeFactories(f, dir),
			Steps: []resource.TestStep{{
				Config:      journaldHCL("x", `  restart = true`),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`Restarting\s+journald\s+needs\s+systemd`),
			}},
		})
	}
}

// journaldMainPID returns the main PID of the running journald.
func journaldMainPID(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("systemctl", "show", "--property=MainPID", "--value", journaldUnit).Output()
	if err != nil {
		t.Fatalf("systemctl show %s: %v", journaldUnit, err)
	}
	return strings.TrimSpace(string(out))
}

// TestAccJournaldConfig_system writes a drop-in into the host's real
// journald.conf.d that repeats a default setting, and restarts the real
// journald, which must come back running with it.
func TestAccJournaldConfig_system(t *testing.T) {
	requireSystemd(t)
	if systemctlOutput(t, "is-active", journaldUnit) != "active" {
		t.Skip("systemd-journald is not running")
	}
	name := "zz-sysutils-acc-" + randomID()
	p := filepath.Join(defaultJournaldDir, name+".conf")
	t.Cleanup(func() {
		if _, err := os.Lstat(p); err == nil {
			_ = os.Remove(p)
			_ = exec.Command("systemctl", "restart", journaldUnit).Run()
		}
	})
	before := journaldMainPID(t)
	// The default of journald.conf(5); the drop-in changes nothing.
	body := "  extra = { SyncIntervalSec = \"5m\" }\n  restart = true"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkPathGone(p),
		Steps: []resource.TestStep{
			{
				Config: journaldHCL(name, body),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileMode(p, 0o644),
					checkLOwnership(p, "0", "0"),
					func(*terraform.State) error {
						// journald is restarted, and running again.
						deadline := time.Now().Add(30 * time.Second)
						for systemctlOutput(t, "is-active", journaldUnit) != "active" {
							if time.Now().After(deadline) {
								return fmt.Errorf("%s is not active after the restart", journaldUnit)
							}
							time.Sleep(200 * time.Millisecond)
						}
						if after := journaldMainPID(t); after == before || after == "0" {
							return fmt.Errorf("%s has main PID %s, before %s: not restarted", journaldUnit, after, before)
						}
						return nil
					},
				),
			},
			{
				ResourceName:            testJournaldResource,
				ImportState:             true,
				ImportStateId:           name,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"restart"},
			},
		},
	})
}
