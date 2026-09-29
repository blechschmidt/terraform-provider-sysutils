package provider

import (
	"fmt"
	"os"
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

const testCronJobResource = "sysutils_cron_job.test"

// newCronTestDir returns a cron.d directory in a temporary directory; it
// does not exist yet, so that the tests cover its creation.
func newCronTestDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "cron.d")
}

// cronProviderFactories returns provider factories whose sysutils_cron_job
// writes to dir, with files owned by the user running the tests.
func cronProviderFactories(dir string) map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		"sysutils": providerserver.NewProtocol6WithError(&sysutilsProvider{
			version: "test",
			cron:    &cronConfig{dir: dir, uid: uint32(os.Getuid()), gid: uint32(os.Getgid())},
		}),
	}
}

func cronJobHCL(name, body string) string {
	return fmt.Sprintf(`
resource "sysutils_cron_job" "test" {
  name = %q
%s
}
`, name, body)
}

// checkCronJobFile checks that the job file p contains want, with mode
// 0644 and owned by the test user.
func checkCronJobFile(p, want string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if string(data) != want {
			return fmt.Errorf("%s =\n%s\nwant\n%s", p, data, want)
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if info.Mode() != cronFileMode {
			return fmt.Errorf("%s has mode %v, want %v", p, info.Mode(), cronFileMode)
		}
		return nil
	}
}

func checkCronJobGone(p string) func(*terraform.State) error {
	return func(*terraform.State) error {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			return fmt.Errorf("%s still exists (err = %v)", p, err)
		}
		return nil
	}
}

func mutateCronFile(t *testing.T, p string, f func(string) string) {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(f(string(data))), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCronJobResource_lifecycle(t *testing.T) {
	dir := newCronTestDir(t)
	p := filepath.Join(dir, "backup")
	owner, group := uidName(uint32(os.Getuid())), gidName(uint32(os.Getgid()))

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: cronProviderFactories(dir),
		CheckDestroy:             checkCronJobGone(p),
		Steps: []resource.TestStep{
			{
				Config: cronJobHCL("backup", `
  schedule = "30 2 * * 1-5"
  command  = "/usr/local/bin/backup --quiet"
`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectKnownValue(testCronJobResource, tfjsonpath.New("path"), knownvalue.StringExact(p)),
						plancheck.ExpectKnownValue(testCronJobResource, tfjsonpath.New("content"),
							knownvalue.StringExact(cronFileHeader+"\n30 2 * * 1-5 root /usr/local/bin/backup --quiet\n")),
					},
				},
				Check: checkCronJobFile(p, cronFileHeader+"\n30 2 * * 1-5 root /usr/local/bin/backup --quiet\n"),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(testCronJobResource, tfjsonpath.New("id"), knownvalue.StringExact("backup")),
					statecheck.ExpectKnownValue(testCronJobResource, tfjsonpath.New("user"), knownvalue.StringExact("root")),
					statecheck.ExpectKnownValue(testCronJobResource, tfjsonpath.New("path"), knownvalue.StringExact(p)),
					statecheck.ExpectKnownValue(testCronJobResource, tfjsonpath.New("mode"), knownvalue.StringExact("0644")),
					statecheck.ExpectKnownValue(testCronJobResource, tfjsonpath.New("owner"), knownvalue.StringExact(owner)),
					statecheck.ExpectKnownValue(testCronJobResource, tfjsonpath.New("group"), knownvalue.StringExact(group)),
					statecheck.ExpectKnownValue(testCronJobResource, tfjsonpath.New("environment"), knownvalue.Null()),
					statecheck.ExpectKnownValue(testCronJobResource, tfjsonpath.New("comment"), knownvalue.Null()),
				},
			},
			{
				// Every attribute changes in place.
				Config: cronJobHCL("backup", `
  schedule = "@daily"
  user     = "nobody"
  command  = "date +\\%F >> /tmp/backup.log"
  comment  = "Nightly backup.\nOwner: ops"
  environment = {
    MAILTO = ""
    PATH   = "/usr/local/bin:/usr/bin:/bin"
    GREET  = "hello world"
  }
`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testCronJobResource, plancheck.ResourceActionUpdate),
					},
				},
				Check: checkCronJobFile(p, cronFileHeader+`
# Nightly backup.
# Owner: ops
GREET="hello world"
MAILTO=""
PATH=/usr/local/bin:/usr/bin:/bin
@daily nobody date +\%F >> /tmp/backup.log
`),
			},
			{
				// Removing the environment and comment removes their lines.
				Config: cronJobHCL("backup", `
  schedule = "@daily"
  user     = "nobody"
  command  = "date +\\%F >> /tmp/backup.log"
`),
				Check: checkCronJobFile(p, cronFileHeader+"\n@daily nobody date +\\%F >> /tmp/backup.log\n"),
			},
			{
				// An empty comment and environment write nothing and do not
				// cause a perpetual diff.
				Config: cronJobHCL("backup", `
  schedule    = "@daily"
  user        = "nobody"
  command     = "date +\\%F >> /tmp/backup.log"
  comment     = ""
  environment = {}
`),
				Check: checkCronJobFile(p, cronFileHeader+"\n@daily nobody date +\\%F >> /tmp/backup.log\n"),
			},
			{
				// A new name replaces the resource: the old file is removed.
				Config: cronJobHCL("backup2", `
  schedule = "@daily"
  command  = "true"
`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testCronJobResource, plancheck.ResourceActionDestroyBeforeCreate),
					},
				},
				Check: resource.ComposeTestCheckFunc(
					checkCronJobGone(p),
					checkCronJobFile(filepath.Join(dir, "backup2"), cronFileHeader+"\n@daily root true\n"),
				),
			},
		},
	})
}

func TestCronJobResource_drift(t *testing.T) {
	dir := newCronTestDir(t)
	p := filepath.Join(dir, "drift")
	config := cronJobHCL("drift", `
  schedule    = "*/5 * * * *"
  command     = "/usr/bin/true"
  comment     = "Keep me."
  environment = { A = "1" }
`)
	want := cronFileHeader + "\n# Keep me.\nA=1\n*/5 * * * * root /usr/bin/true\n"
	expectUpdate := resource.ConfigPlanChecks{
		PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testCronJobResource, plancheck.ResourceActionUpdate)},
	}
	edit := func(f func(string) string) func() { return func() { mutateCronFile(t, p, f) } }

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: cronProviderFactories(dir),
		CheckDestroy:             checkCronJobGone(p),
		Steps: []resource.TestStep{
			{Config: config, Check: checkCronJobFile(p, want)},
			{
				// A changed schedule is read back and reverted.
				PreConfig: edit(func(s string) string { return strings.Replace(s, "*/5 * * * *", "0 * * * *", 1) }),
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testCronJobResource, plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue(testCronJobResource, tfjsonpath.New("schedule"), knownvalue.StringExact("*/5 * * * *")),
					},
				},
				Check: checkCronJobFile(p, want),
			},
			{
				PreConfig:        edit(func(s string) string { return strings.Replace(s, "/usr/bin/true", "/usr/bin/false", 1) }),
				Config:           config,
				ConfigPlanChecks: expectUpdate,
				Check:            checkCronJobFile(p, want),
			},
			{
				PreConfig:        edit(func(s string) string { return strings.Replace(s, "A=1", "A=2\nB=3", 1) }),
				Config:           config,
				ConfigPlanChecks: expectUpdate,
				Check:            checkCronJobFile(p, want),
			},
			{
				PreConfig:        edit(func(s string) string { return strings.Replace(s, "# Keep me.", "# Changed.", 1) }),
				Config:           config,
				ConfigPlanChecks: expectUpdate,
				Check:            checkCronJobFile(p, want),
			},
			{
				// A second job, or anything else the parsed attributes do
				// not cover, shows up through content.
				PreConfig:        edit(func(s string) string { return s + "@reboot root /tmp/evil\n" }),
				Config:           config,
				ConfigPlanChecks: expectUpdate,
				Check:            checkCronJobFile(p, want),
			},
			{
				PreConfig:        edit(func(s string) string { return strings.TrimPrefix(s, cronFileHeader+"\n") }),
				Config:           config,
				ConfigPlanChecks: expectUpdate,
				Check:            checkCronJobFile(p, want),
			},
			{
				// cron ignores a group- or world-writable file.
				PreConfig: func() {
					if err := os.Chmod(p, 0o666); err != nil {
						t.Fatal(err)
					}
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testCronJobResource, plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue(testCronJobResource, tfjsonpath.New("mode"), knownvalue.StringExact("0644")),
					},
				},
				Check: checkCronJobFile(p, want),
			},
			{
				// A deleted file is recreated.
				PreConfig: func() {
					if err := os.Remove(p); err != nil {
						t.Fatal(err)
					}
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testCronJobResource, plancheck.ResourceActionCreate)},
				},
				Check: checkCronJobFile(p, want),
			},
			{
				// Nothing to do once in sync.
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}

func TestCronJobResource_import(t *testing.T) {
	dir := newCronTestDir(t)
	p := filepath.Join(dir, "handmade")
	config := cronJobHCL("handmade", `
  schedule    = "17 * * * *"
  command     = "cd / && run-parts --report /etc/cron.hourly"
  environment = { SHELL = "/bin/sh" }
`)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: cronProviderFactories(dir),
		CheckDestroy:             checkCronJobGone(p),
		Steps: []resource.TestStep{
			{
				// A file written by hand is imported as cron reads it.
				PreConfig: func() {
					if err := os.MkdirAll(dir, 0o755); err != nil {
						t.Fatal(err)
					}
					data := "SHELL = /bin/sh\n\n17 *  * * *\troot    cd / && run-parts --report /etc/cron.hourly\n"
					if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
						t.Fatal(err)
					}
				},
				Config:             config,
				ResourceName:       testCronJobResource,
				ImportState:        true,
				ImportStateId:      "handmade",
				ImportStatePersist: true,
				ImportStateCheck: func(s []*terraform.InstanceState) error {
					if len(s) != 1 {
						return fmt.Errorf("got %d instances", len(s))
					}
					a := s[0].Attributes
					want := map[string]string{
						"name":              "handmade",
						"schedule":          "17 * * * *",
						"user":              "root",
						"command":           "cd / && run-parts --report /etc/cron.hourly",
						"environment.%":     "1",
						"environment.SHELL": "/bin/sh",
						"path":              p,
						"mode":              "0644",
					}
					for k, v := range want {
						if a[k] != v {
							return fmt.Errorf("%s = %q, want %q", k, a[k], v)
						}
					}
					return nil
				},
			},
			{
				// Only the formatting differs, which apply normalizes.
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testCronJobResource, plancheck.ResourceActionUpdate),
					},
				},
				Check: checkCronJobFile(p, cronFileHeader+"\nSHELL=/bin/sh\n17 * * * * root cd / && run-parts --report /etc/cron.hourly\n"),
			},
			{
				// A managed file imports into exactly the same state.
				Config:            config,
				ResourceName:      testCronJobResource,
				ImportState:       true,
				ImportStateVerify: true,
			},
			{
				ResourceName:  testCronJobResource,
				ImportState:   true,
				ImportStateId: "missing",
				ExpectError:   regexp.MustCompile(`missing\s+does\s+not\s+exist`),
			},
			{
				ResourceName:  testCronJobResource,
				ImportState:   true,
				ImportStateId: "../passwd",
				ExpectError:   regexp.MustCompile(`Invalid\s+import\s+ID`),
			},
		},
	})
}

func TestCronJobResource_importWithoutJob(t *testing.T) {
	dir := newCronTestDir(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: cronProviderFactories(dir),
		Steps: []resource.TestStep{
			{
				PreConfig: func() {
					if err := os.MkdirAll(dir, 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dir, "empty"), []byte("# nothing here\nA=1\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				},
				Config:        cronJobHCL("empty", `schedule = "@daily"`+"\n"+`command = "true"`),
				ResourceName:  testCronJobResource,
				ImportState:   true,
				ImportStateId: "empty",
				ExpectError:   regexp.MustCompile(`does\s+not\s+contain\s+a\s+job\s+line`),
			},
		},
	})
}

func TestCronJobResource_existingFile(t *testing.T) {
	dir := newCronTestDir(t)
	p := filepath.Join(dir, "taken")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: cronProviderFactories(dir),
		Steps: []resource.TestStep{
			{
				PreConfig: func() {
					if err := os.MkdirAll(dir, 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(p, []byte("@daily root keep\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				},
				Config:      cronJobHCL("taken", `schedule = "@daily"`+"\n"+`command = "overwrite"`),
				ExpectError: regexp.MustCompile(`Cron\s+job\s+already\s+exists[\s\S]*terraform\s+import`),
			},
			{
				// The existing file is untouched.
				PreConfig: func() {
					if data, err := os.ReadFile(p); err != nil || string(data) != "@daily root keep\n" {
						t.Fatalf("existing file was modified: %q, %v", data, err)
					}
				},
				Config:   cronJobHCL("taken", `schedule = "@daily"`+"\n"+`command = "overwrite"`),
				PlanOnly: true,
				// The resource was never created, so it is still planned.
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

func TestCronJobResource_validation(t *testing.T) {
	dir := newCronTestDir(t)
	cases := []struct {
		name, body string
		want       string
	}{
		{"backup.cron", `schedule = "@daily"` + "\n" + `command = "true"`, `Invalid\s+cron\s+job\s+name`},
		{"../x", `schedule = "@daily"` + "\n" + `command = "true"`, `Invalid\s+cron\s+job\s+name`},
		{"x", `schedule = "* * * *"` + "\n" + `command = "true"`, `Invalid\s+cron\s+schedule[\s\S]*five\s+fields`},
		{"x", `schedule = "60 * * * *"` + "\n" + `command = "true"`, `Invalid\s+cron\s+schedule[\s\S]*out\s+of\s+range`},
		{"x", `schedule = "@sometimes"` + "\n" + `command = "true"`, `Invalid\s+cron\s+schedule`},
		{"x", `schedule = "@daily"` + "\n" + `command = "a\nb"`, `Invalid\s+cron\s+command`},
		{"x", `schedule = "@daily"` + "\n" + `command = ""`, `Invalid\s+cron\s+command`},
		{"x", `schedule = "@daily"` + "\n" + `command = "true"` + "\n" + `user = "-x"`, `Invalid\s+name[\s\S]*user\s+=\s+"-x"`},
		{"x", `schedule = "@daily"` + "\n" + `command = "true"` + "\n" + `environment = { "A-B" = "1" }`, `Invalid\s+environment\s+variable\s+name`},
		{"x", `schedule = "@daily"` + "\n" + `command = "true"` + "\n" + `environment = { A = "1\n2" }`, `Invalid\s+environment\s+variable\s+value`},
	}
	steps := make([]resource.TestStep, 0, len(cases))
	for _, c := range cases {
		steps = append(steps, resource.TestStep{
			Config:      cronJobHCL(c.name, c.body),
			ExpectError: regexp.MustCompile(c.want),
		})
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: cronProviderFactories(dir),
		Steps:                    steps,
	})
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("validation errors created %s (err = %v)", dir, err)
	}
}
