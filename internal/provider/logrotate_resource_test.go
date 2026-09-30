package provider

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
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

const testLogrotateResource = "sysutils_logrotate.test"

// logrotateHCL renders a sysutils_logrotate resource named test.
func logrotateHCL(name, body string) string {
	return fmt.Sprintf(`
resource "sysutils_logrotate" "test" {
  name = %q
%s
}
`, name, body)
}

func logrotateProviderFactories(cfg *logrotateConfig) map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		"sysutils": providerserver.NewProtocol6WithError(&sysutilsProvider{version: "test", logrotate: cfg}),
	}
}

// requireLogrotate skips tests that run the real logrotate.
func requireLogrotate(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("logrotate")
	if err != nil {
		t.Skip("logrotate is not installed")
	}
	return bin
}

const logrotateFullBody = `
  paths         = ["/var/log/myapp/*.log", "/var/log/my app/out.log"]
  frequency     = "daily"
  rotate        = 14
  compress      = true
  delaycompress = true
  missingok     = true
  notifempty    = true
  sharedscripts = true
  create_mode   = "0640"
  create_owner  = "root"
  create_group  = "root"
  postrotate    = <<-EOT
    systemctl kill -s HUP myapp.service
  EOT
  extra_directives = ["maxsize 100M", "dateext"]
`

const logrotateFullContent = logrotateFileHeader + `
/var/log/myapp/*.log "/var/log/my app/out.log" {
    daily
    rotate 14
    compress
    delaycompress
    missingok
    notifempty
    create 0640 root root
    sharedscripts
    maxsize 100M
    dateext
    postrotate
systemctl kill -s HUP myapp.service
    endscript
}
`

// TestAccLogrotate_lifecycle manages a drop-in below root_dir through
// create, drift of content and mode, import, update, removal outside
// Terraform and destroy.
func TestAccLogrotate_lifecycle(t *testing.T) {
	root := t.TempDir()
	const managed = "/etc/logrotate.d/myapp"
	p := filepath.Join(root, managed)
	uid, gid := processOwner()
	owner, group := uidName(uid), gidName(gid)
	updated := logrotateFileHeader + "\n/var/log/myapp/*.log {\n    weekly\n    rotate 4\n    nocompress\n}\n"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkPathGone(p),
		Steps: []resource.TestStep{
			{
				Config: limitsConfig(root, logrotateHCL("myapp", logrotateFullBody)),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectKnownValue(testLogrotateResource, tfjsonpath.New("path"), knownvalue.StringExact(managed)),
						plancheck.ExpectKnownValue(testLogrotateResource, tfjsonpath.New("content"), knownvalue.StringExact(logrotateFullContent)),
						plancheck.ExpectKnownValue(testLogrotateResource, tfjsonpath.New("mode"), knownvalue.StringExact("0644")),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(p, logrotateFullContent),
					checkFileMode(p, 0o644),
					checkNoTempFiles(filepath.Dir(p)),
				),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(testLogrotateResource, tfjsonpath.New("id"), knownvalue.StringExact("myapp")),
					statecheck.ExpectKnownValue(testLogrotateResource, tfjsonpath.New("validate"), knownvalue.Bool(true)),
					statecheck.ExpectKnownValue(testLogrotateResource, tfjsonpath.New("owner"), knownvalue.StringExact(owner)),
					statecheck.ExpectKnownValue(testLogrotateResource, tfjsonpath.New("group"), knownvalue.StringExact(group)),
				},
			},
			{
				// Import parses the file back into every attribute.
				ResourceName:      testLogrotateResource,
				ImportState:       true,
				ImportStateId:     "myapp",
				ImportStateVerify: true,
			},
			{
				// A change outside Terraform is drift: the plan shows the
				// content on disk, and apply restores the file.
				PreConfig: setFile(t, p, logrotateFullContent+"# tuned by hand\n"),
				Config:    limitsConfig(root, logrotateHCL("myapp", logrotateFullBody)),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testLogrotateResource, plancheck.ResourceActionUpdate),
						expectBefore(testLogrotateResource, "content", logrotateFullContent+"# tuned by hand\n"),
					},
				},
				Check: checkFileContent(p, logrotateFullContent),
			},
			{
				// So is a mode that makes logrotate ignore the file.
				PreConfig: func() { mustChmod(t, p, 0o666) },
				Config:    limitsConfig(root, logrotateHCL("myapp", logrotateFullBody)),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testLogrotateResource, plancheck.ResourceActionUpdate),
						expectBefore(testLogrotateResource, "mode", "0666"),
					},
				},
				Check: checkFileMode(p, 0o644),
			},
			{
				Config: limitsConfig(root, logrotateHCL("myapp", logrotateFullBody)),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				// Unset attributes are not written at all.
				Config: limitsConfig(root, logrotateHCL("myapp", `
  paths     = ["/var/log/myapp/*.log"]
  frequency = "weekly"
  rotate    = 4
  compress  = false
`)),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testLogrotateResource, plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue(testLogrotateResource, tfjsonpath.New("content"), knownvalue.StringExact(updated)),
					},
				},
				Check: checkFileContent(p, updated),
			},
			{
				// A file removed outside Terraform is created again.
				PreConfig: func() { _ = os.Remove(p) },
				Config: limitsConfig(root, logrotateHCL("myapp", `
  paths     = ["/var/log/myapp/*.log"]
  frequency = "weekly"
  rotate    = 4
  compress  = false
`)),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testLogrotateResource, plancheck.ResourceActionCreate),
					},
				},
				Check: checkFileContent(p, updated),
			},
		},
	})
}

// TestAccLogrotate_existingFile checks that create refuses a file that
// exists, and that a foreign file can be imported and then managed.
func TestAccLogrotate_existingFile(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "etc", "logrotate.d", "rsyslog")
	const orig = `/var/log/syslog
/var/log/mail.log
{
	rotate 4
	weekly
	missingok
	notifempty
	compress
	delaycompress
	sharedscripts
	postrotate
		/usr/lib/rsyslog/rsyslog-rotate
	endscript
}
`
	mustMkdir(t, filepath.Dir(p))
	mustWrite(t, p, orig)
	mustChmod(t, p, 0o644)
	config := limitsConfig(root, logrotateHCL("rsyslog", `
  paths         = ["/var/log/syslog", "/var/log/mail.log"]
  frequency     = "weekly"
  rotate        = 4
  missingok     = true
  notifempty    = true
  compress      = true
  delaycompress = true
  sharedscripts = true
  postrotate    = "\t\t/usr/lib/rsyslog/rsyslog-rotate\n"
`))
	rendered := logrotateFileHeader + "\n/var/log/syslog /var/log/mail.log {\n    weekly\n    rotate 4\n    compress\n    delaycompress\n    missingok\n    notifempty\n    sharedscripts\n    postrotate\n\t\t/usr/lib/rsyslog/rsyslog-rotate\n    endscript\n}\n"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkPathGone(p),
		Steps: []resource.TestStep{
			{
				Config:      config,
				ExpectError: regexp.MustCompile(`/etc/logrotate\.d/rsyslog\s+already\s+exists(.|\n)*terraform\s+import`),
			},
			{
				// The file is untouched, and can be imported.
				PreConfig:          func() { assertFileContent(t, p, orig) },
				Config:             config,
				ResourceName:       testLogrotateResource,
				ImportState:        true,
				ImportStateId:      "rsyslog",
				ImportStatePersist: true,
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if len(states) != 1 || states[0].Attributes["content"] != orig || states[0].Attributes["frequency"] != "weekly" {
						return fmt.Errorf("imported state = %v", states)
					}
					return nil
				},
			},
			{
				// The attributes match; only the formatting differs.
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testLogrotateResource, plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue(testLogrotateResource, tfjsonpath.New("content"), knownvalue.StringExact(rendered)),
					},
				},
				Check: checkFileContent(p, rendered),
			},
		},
	})
}

// assertFileContent fails the test unless p contains want.
func assertFileContent(t *testing.T, p, want string) {
	t.Helper()
	if err := checkFileContent(p, want)(nil); err != nil {
		t.Fatal(err)
	}
}

// TestAccLogrotate_validation checks the plan-time validation of every
// attribute.
func TestAccLogrotate_validation(t *testing.T) {
	root := t.TempDir()
	step := func(name, body, want string) resource.TestStep {
		return resource.TestStep{
			Config:      limitsConfig(root, logrotateHCL(name, body)),
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(want),
		}
	}
	const paths = `  paths = ["/var/log/x.log"]` + "\n"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			step("../passwd", paths, `must\s+consist\s+of\s+letters`),
			step(".hidden", paths, `must\s+not\s+start\s+with`),
			step("app.dpkg-old", paths, `logrotate\s+skips\s+files`),
			step("app", `  paths = []`, `at\s+least\s+1`),
			step("app", `  paths = ["var/log/x.log"]`, `must\s+be\s+absolute`),
			step("app", `  paths = ["/var/log/../../etc/shadow"]`, `must\s+not\s+contain\s+".."`),
			step("app", `  paths = ["/var/log/x.log {\n  postrotate"]`, `must\s+not\s+contain\s+"\{"`),
			step("app", `  paths = ["/var/log/x.log", "/var/log/x.log"]`, `duplicate`),
			step("app", paths+`  frequency = "fortnightly"`, `value\s+must\s+be\s+one\s+of`),
			step("app", paths+`  rotate = -2`, `must\s+be\s+between\s+-1`),
			step("app", paths+`  create_mode = "rw-r--r--"`, `octal`),
			step("app", paths+`  create_owner = "root"`, `create_owner\s+requires\s+create_mode`),
			step("app", paths+"  create_mode = \"0640\"\n  create_group = \"adm\"", `create_group\s+requires\s+create_owner`),
			step("app", paths+"  create_mode = \"0640\"\n  create_owner = \"root\"", `create_owner\s+requires\s+create_group(.|\n)*3\.22`),
			step("app", paths+"  create_mode = \"0640\"\n  create_owner = \"root adm\"\n  create_group = \"adm\"", `must\s+consist\s+of\s+letters`),
			step("app", paths+`  postrotate = "kill -HUP 1\nendscript\nrm -rf /"`, `must\s+not\s+contain\s+a\s+line\s+"endscript"`),
			step("app", paths+`  extra_directives = ["prerotate"]`, `scripts\s+span\s+several\s+lines`),
			step("app", paths+`  extra_directives = ["}\n/etc/shadow {"]`, `must\s+not\s+contain\s+"{"`),
			step("app", paths+`  extra_directives = ["include /etc/other"]`, `include\s+is\s+not\s+valid`),
			step("app", paths+"  compress = true\n  extra_directives = [\"nocompress\"]", `conflicts\s+with\s+compress`),
		},
	})
}

func TestAccLogrotate_importErrors(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "etc", "logrotate.d")
	mustMkdir(t, dir)
	mustWrite(t, filepath.Join(dir, "two"), "/var/log/a {\n}\n/var/log/b {\n}\n")
	mustWrite(t, filepath.Join(dir, "pre"), "/var/log/a {\n  prerotate\n    true\n  endscript\n}\n")
	config := limitsConfig(root, logrotateHCL("x", `  paths = ["/var/log/x.log"]`))
	step := func(id, want string) resource.TestStep {
		return resource.TestStep{
			Config:        config,
			ResourceName:  testLogrotateResource,
			ImportState:   true,
			ImportStateId: id,
			ExpectError:   regexp.MustCompile(want),
		}
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			step("../../etc/passwd", `Import\s+ID\s+must\s+be\s+the\s+name\s+of\s+a\s+file`),
			step("missing", `/etc/logrotate\.d/missing\s+does\s+not\s+exist`),
			step("two", `single\s+block\s+only`),
			step("pre", `prerotate\s+scripts\s+are\s+not\s+supported`),
		},
	})
}

// TestAccLogrotate_symlinks checks that a symlink at the drop-in is
// refused rather than written through, and that logrotate.d is resolved
// inside root_dir.
func TestAccLogrotate_symlinks(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "etc"))
	mustMkdir(t, filepath.Join(root, "usr", "etc", "logrotate.d"))
	// An absolute link inside the tree points into the tree.
	mustSymlink(t, "/usr/etc/logrotate.d", filepath.Join(root, "etc", "logrotate.d"))
	outside := filepath.Join(t.TempDir(), "outside")
	mustWrite(t, outside, "")
	link := filepath.Join(root, "usr", "etc", "logrotate.d", "link")
	const body = `  paths = ["/var/log/x.log"]`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: limitsConfig(root, logrotateHCL("app", body)),
				Check:  checkFileContent(filepath.Join(root, "usr", "etc", "logrotate.d", "app"), logrotateFileHeader+"\n/var/log/x.log {\n}\n"),
			},
			{
				PreConfig:   func() { mustSymlink(t, outside, link) },
				Config:      limitsConfig(root, logrotateHCL("link", body)),
				ExpectError: regexp.MustCompile(`symbolic\s+link`),
			},
		},
		CheckDestroy: func(*terraform.State) error {
			assertFileContent(t, outside, "")
			return checkPathGone(filepath.Join(root, "usr", "etc", "logrotate.d", "app"))(nil)
		},
	})
}

// TestAccLogrotate_check checks the logrotate -d check with a fake
// logrotate: it rejects a file before it is installed, leaving the old one
// in place, and it is skipped with validate = false and below root_dir.
func TestAccLogrotate_check(t *testing.T) {
	f := &fakeLogrotate{output: func(content, file string) (int, string) {
		if regexp.MustCompile(`(?m)^\s*bogus`).MatchString(content) {
			return 1, "error: " + file + ":4 unknown option 'bogus' -- ignoring line\n"
		}
		return 0, ""
	}}
	cfg := &logrotateConfig{
		dir:      filepath.Join(t.TempDir(), "logrotate.d"),
		run:      f.run,
		lookPath: func(string) (string, error) { return "/usr/sbin/logrotate", nil },
	}
	p := filepath.Join(cfg.dir, "app")
	good := `  paths = ["/var/log/app.log"]`
	bad := good + "\n  extra_directives = [\"bogus\"]"
	goodContent := logrotateFileHeader + "\n/var/log/app.log {\n}\n"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: logrotateProviderFactories(cfg),
		CheckDestroy:             checkPathGone(p),
		Steps: []resource.TestStep{
			{
				Config:      logrotateHCL("app", bad),
				ExpectError: regexp.MustCompile(`logrotate\s+-d\s+rejected\s+the\s+new\s+contents\s+of\s+` + regexp.QuoteMeta(p) + `(.|\n)*` + regexp.QuoteMeta(p) + `:4\s+unknown\s+option`),
				Check:       checkPathGone(p),
			},
			{
				Config: logrotateHCL("app", good),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(p, goodContent),
					checkNoTempFiles(cfg.dir),
					func(*terraform.State) error {
						if len(f.checked) != 2 {
							return fmt.Errorf("logrotate checked %d files, want 2", len(f.checked))
						}
						return nil
					},
				),
			},
			{
				Config:      logrotateHCL("app", bad),
				ExpectError: regexp.MustCompile(`logrotate\s+-d\s+rejected`),
				Check:       checkFileContent(p, goodContent),
			},
			{
				// Unchecked on request.
				Config: logrotateHCL("app", bad+"\n  validate = false"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(p, logrotateFileHeader+"\n/var/log/app.log {\n    bogus\n}\n"),
					func(*terraform.State) error {
						if len(f.checked) != 3 {
							return fmt.Errorf("logrotate checked %d files, want 3", len(f.checked))
						}
						return nil
					},
				),
			},
		},
	})
}

// TestAccLogrotate_checkSkippedBelowRootDir checks that the host's
// logrotate never checks files below root_dir: it would resolve users,
// groups and log files on the host rather than in the tree.
func TestAccLogrotate_checkSkippedBelowRootDir(t *testing.T) {
	f := &fakeLogrotate{output: func(string, string) (int, string) { return 1, "error: rejected everything\n" }}
	cfg := &logrotateConfig{run: f.run, lookPath: func(string) (string, error) { return "/usr/sbin/logrotate", nil }}
	root := t.TempDir()
	p := filepath.Join(root, "etc", "logrotate.d", "app")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: logrotateProviderFactories(cfg),
		CheckDestroy:             checkPathGone(p),
		Steps: []resource.TestStep{{
			Config: limitsConfig(root, logrotateHCL("app", `  paths = ["/var/log/app.log"]`)),
			Check: resource.ComposeAggregateTestCheckFunc(
				checkFileContent(p, logrotateFileHeader+"\n/var/log/app.log {\n}\n"),
				func(*terraform.State) error {
					if len(f.checked) != 0 {
						return fmt.Errorf("logrotate checked %q, want nothing", f.checked)
					}
					return nil
				},
			),
		}},
	})
}

// TestAccLogrotate_realCheck runs the real logrotate -d on the rendered
// files in a temporary logrotate.d.
func TestAccLogrotate_realCheck(t *testing.T) {
	requireLogrotate(t)
	cfg := &logrotateConfig{dir: filepath.Join(t.TempDir(), "logrotate.d")}
	p := filepath.Join(cfg.dir, "app")
	logs := filepath.Join(t.TempDir(), "missing")
	body := fmt.Sprintf(`
  paths        = [%q]
  frequency    = "daily"
  rotate       = 7
  create_mode  = "0640"
  create_owner = "root"
  create_group = "root"
  postrotate   = "true"
`, logs+"/*.log")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: logrotateProviderFactories(cfg),
		CheckDestroy:             checkPathGone(p),
		Steps: []resource.TestStep{
			{
				// Missing log files are not an error in the file.
				Config: logrotateHCL("app", body),
				Check:  checkFileMode(p, 0o644),
			},
			{
				// logrotate 3.22 and later only warn about this one.
				Config:      logrotateHCL("app", body+`  extra_directives = ["bogusdirective"]`),
				ExpectError: regexp.MustCompile(`logrotate\s+-d\s+rejected(.|\n)*` + regexp.QuoteMeta(p) + `:\d+\s+unknown\s+option`),
			},
			{
				Config:      logrotateHCL("app", body+`  extra_directives = ["maxsize lots"]`),
				ExpectError: regexp.MustCompile(`logrotate\s+-d\s+rejected(.|\n)*` + regexp.QuoteMeta(p) + `:\d+`),
			},
			{
				Config:      logrotateHCL("app", strings.Replace(body, `create_group = "root"`, `create_group = "sysutils-no-such-group"`, 1)),
				ExpectError: regexp.MustCompile(`logrotate\s+-d\s+rejected(.|\n)*sysutils-no-such-group`),
			},
		},
	})
}

// TestAccLogrotate_system manages a drop-in in the host's real
// /etc/logrotate.d for a log file that does not exist, checked by the real
// logrotate if it is installed.
func TestAccLogrotate_system(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("acceptance test; set TF_ACC=1 to run")
	}
	requireRoot(t)
	name := "sysutils-acc-" + randomID()
	p := filepath.Join(defaultLogrotateDir, name)
	t.Cleanup(func() { _ = os.Remove(p) })
	body := fmt.Sprintf(`
  paths     = ["/var/log/%s/*.log"]
  frequency = "weekly"
  rotate    = 2
  missingok = true
`, name)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkPathGone(p),
		Steps: []resource.TestStep{
			{
				Config: logrotateHCL(name, body),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileMode(p, 0o644),
					checkLOwnership(p, "0", "0"),
					resource.TestCheckResourceAttr(testLogrotateResource, "path", p),
					resource.TestCheckResourceAttr(testLogrotateResource, "owner", "root"),
				),
			},
			{
				ResourceName:      testLogrotateResource,
				ImportState:       true,
				ImportStateId:     name,
				ImportStateVerify: true,
			},
		},
	})
}
