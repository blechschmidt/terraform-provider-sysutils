package provider

import (
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

const testTemplateFile = "sysutils_template_file.test"

// hclString quotes s as an HCL string literal whose value is exactly s, with
// Terraform's own ${ and %{ interpolation escaped.
func hclString(s string) string {
	s = strings.ReplaceAll(s, "${", "$${")
	s = strings.ReplaceAll(s, "%{", "%%{")
	return fmt.Sprintf("%q", s)
}

func checkTemplateFileDestroyed(s *terraform.State) error {
	for _, rs := range s.RootModule().Resources {
		if rs.Type != "sysutils_template_file" {
			continue
		}
		if _, err := os.Lstat(rs.Primary.Attributes["path"]); !os.IsNotExist(err) {
			return fmt.Errorf("file %s still exists (err=%v)", rs.Primary.Attributes["path"], err)
		}
	}
	return nil
}

func TestAccTemplateFile_goSyntax(t *testing.T) {
	requireRoot(t)
	target := filepath.Join(t.TempDir(), "sub", "app.conf")

	config := func(port int, mode string) string {
		return fmt.Sprintf(`
resource "sysutils_template_file" "test" {
  path     = %q
  mode     = %q
  template = %s
  vars = {
    name  = "web"
    port  = %d
    hosts = ["a", "b"]
    db    = { user = "app" }
  }
}`, target, mode, hclString("name={{ .name }}\nlisten={{ .port }}\n{{ range .hosts }}host={{ . }}\n{{ end }}user={{ .db.user | upper }}\n"), port)
	}
	want := func(port int) string {
		return fmt.Sprintf("name=web\nlisten=%d\nhost=a\nhost=b\nuser=APP\n", port)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkTemplateFileDestroyed,
		Steps: []resource.TestStep{
			{
				Config: config(8080, "0640"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						// Rendered at plan time.
						plancheck.ExpectKnownValue(testTemplateFile, tfjsonpath.New("rendered"), knownvalue.StringExact(want(8080))),
						plancheck.ExpectKnownValue(testTemplateFile, tfjsonpath.New("content_sha256"), knownvalue.StringExact(sha256Hex([]byte(want(8080))))),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(target, want(8080)),
					checkFileMode(target, 0o640),
					resource.TestCheckResourceAttr(testTemplateFile, "rendered", want(8080)),
					resource.TestCheckResourceAttr(testTemplateFile, "content_sha256", sha256Hex([]byte(want(8080)))),
					resource.TestCheckResourceAttr(testTemplateFile, "syntax", "go"),
					resource.TestCheckNoResourceAttr(testTemplateFile, "rendered_sensitive"),
					resource.TestCheckResourceAttr(testTemplateFile, "id", target),
				),
			},
			{
				Config: config(9090, "0600"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testTemplateFile, plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(target, want(9090)),
					checkFileMode(target, 0o600),
					resource.TestCheckResourceAttr(testTemplateFile, "rendered", want(9090)),
				),
			},
			{
				// Unchanged inputs: nothing to do.
				Config: config(9090, "0600"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}

func TestAccTemplateFile_terraformSyntax(t *testing.T) {
	requireRoot(t)
	target := filepath.Join(t.TempDir(), "hosts")

	tmpl := "%{ for name, ip in nodes ~}\n${ip} ${name}\n%{ endfor ~}\n# ${upper(env)} $${literal}\n"
	want := "10.0.0.1 node1\n10.0.0.2 node2\n# PROD ${literal}\n"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkTemplateFileDestroyed,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "sysutils_template_file" "test" {
  path     = %q
  syntax   = "terraform"
  template = %s
  vars = {
    env   = "prod"
    nodes = { node1 = "10.0.0.1", node2 = "10.0.0.2" }
  }
}`, target, hclString(tmpl)),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(target, want),
					resource.TestCheckResourceAttr(testTemplateFile, "rendered", want),
				),
			},
		},
	})
}

func TestAccTemplateFile_drift(t *testing.T) {
	requireRoot(t)
	target := filepath.Join(t.TempDir(), "drift.conf")
	config := fmt.Sprintf(`
resource "sysutils_template_file" "test" {
  path     = %q
  template = "value={{ .v }}\n"
  vars     = { v = "desired" }
}`, target)

	var mtime time.Time
	recordMtime := func(*terraform.State) error {
		info, err := os.Stat(target)
		if err != nil {
			return err
		}
		mtime = info.ModTime()
		return nil
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkTemplateFileDestroyed,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check:  checkFileContent(target, "value=desired\n"),
			},
			{
				// Content edited outside Terraform: the plan shows the
				// actual content being replaced by the rendered one.
				PreConfig: func() {
					if err := os.WriteFile(target, []byte("value=tampered\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testTemplateFile, plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue(testTemplateFile, tfjsonpath.New("rendered"), knownvalue.StringExact("value=desired\n")),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(target, "value=desired\n"),
					resource.TestCheckResourceAttr(testTemplateFile, "rendered", "value=desired\n"),
					recordMtime,
				),
			},
			{
				// Mode changed outside Terraform.
				PreConfig: func() {
					if err := os.Chmod(target, 0o600); err != nil {
						t.Fatal(err)
					}
				},
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				Config: config,
				Check:  checkFileMode(target, 0o644),
			},
			{
				// File deleted outside Terraform: recreated.
				PreConfig: func() {
					if err := os.Remove(target); err != nil {
						t.Fatal(err)
					}
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testTemplateFile, plancheck.ResourceActionCreate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(checkFileContent(target, "value=desired\n"), recordMtime),
			},
			{
				// A template change that renders the same output updates
				// state but does not rewrite the file.
				PreConfig: func() { time.Sleep(20 * time.Millisecond) },
				Config:    strings.Replace(config, "{{ .v }}", "{{.v}}", 1),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testTemplateFile, plancheck.ResourceActionUpdate),
					},
				},
				Check: func(*terraform.State) error {
					info, err := os.Stat(target)
					if err != nil {
						return err
					}
					if !info.ModTime().Equal(mtime) {
						return fmt.Errorf("file was rewritten although the rendered content did not change (mtime %v -> %v)", mtime, info.ModTime())
					}
					return nil
				},
			},
		},
	})
}

func TestAccTemplateFile_planTimeErrors(t *testing.T) {
	requireRoot(t)
	target := filepath.Join(t.TempDir(), "never-written")

	cases := []struct {
		name, body string
		want       *regexp.Regexp
	}{
		{
			name: "go parse error",
			body: `template = "ok\n{{ .name }\n"`,
			want: regexp.MustCompile(`Invalid\s+template[\s\S]*not\s+valid\s+Go\s+template\s+syntax:\s+line\s+2:\s+unexpected`),
		},
		{
			name: "terraform parse error",
			body: `
  syntax   = "terraform"
  template = "a\n$${name + }"
  vars     = { name = "x" }`,
			want: regexp.MustCompile(`Invalid\s+template[\s\S]*Terraform\s+template\s+syntax:\s+line\s+2,\s+column\s+10`),
		},
		{
			name: "undefined function",
			body: `template = "{{ env \"HOME\" }}"`,
			want: regexp.MustCompile(`Invalid\s+template[\s\S]*function\s+"env"\s+not\s+defined`),
		},
		{
			name: "missing variable",
			body: `
  template = "x\n{{ .missing }}"
  vars     = { name = "x" }`,
			want: regexp.MustCompile(`Template\s+rendering\s+failed[\s\S]*line\s+2,\s+column\s+3:[\s\S]*no\s+entry\s+for\s+key\s+"missing"`),
		},
		{
			name: "vars not an object",
			body: `
  template = "x"
  vars     = "nope"`,
			want: regexp.MustCompile(`Invalid\s+template\s+variables[\s\S]*must\s+be\s+an\s+object\s+or\s+a\s+map`),
		},
		{
			name: "duplicate variable",
			body: `
  template       = "x"
  vars           = { a = "1" }
  sensitive_vars = { a = "2" }`,
			want: regexp.MustCompile(`"a"\s+is\s+set\s+in\s+both\s+vars\s+and\s+sensitive_vars`),
		},
		{
			name: "sensitive value redacted",
			body: `
  syntax         = "terraform"
  template       = "$${regex(pattern, \"x\")}"
  sensitive_vars = { pattern = "(hunter2" }`,
			want: regexp.MustCompile(`Template\s+rendering\s+failed[\s\S]*\(sensitive\s+value\)`),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps: []resource.TestStep{
					{
						Config: fmt.Sprintf(`
resource "sysutils_template_file" "test" {
  path = %q
  %s
}`, target, c.body),
						PlanOnly:    true,
						ExpectError: c.want,
					},
				},
			})
			if _, err := os.Lstat(target); !os.IsNotExist(err) {
				t.Fatalf("file must not be written, stat err = %v", err)
			}
		})
	}
}

func TestAccTemplateFile_sensitiveVars(t *testing.T) {
	requireRoot(t)
	target := filepath.Join(t.TempDir(), "secret.conf")
	want := "user=app\npassword=s3cr3t-value\n"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkTemplateFileDestroyed,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "sysutils_template_file" "test" {
  path           = %q
  mode           = "0600"
  template       = "user={{ .user }}\npassword={{ .password }}\n"
  vars           = { user = "app" }
  sensitive_vars = { password = "s3cr3t-value" }
}

output "rendered_is_null" {
  value = sysutils_template_file.test.rendered == null
}`, target),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectSensitiveValue(testTemplateFile, tfjsonpath.New("rendered_sensitive")),
						plancheck.ExpectKnownValue(testTemplateFile, tfjsonpath.New("content_sha256"), knownvalue.StringExact(sha256Hex([]byte(want)))),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(target, want),
					checkFileMode(target, 0o600),
					resource.TestCheckNoResourceAttr(testTemplateFile, "rendered"),
					resource.TestCheckResourceAttr(testTemplateFile, "rendered_sensitive", want),
					resource.TestCheckOutput("rendered_is_null", "true"),
				),
			},
			{
				// Drift is detected through the checksum.
				PreConfig: func() {
					if err := os.WriteFile(target, []byte("password=changed\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				},
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
				Check:              resource.TestCheckResourceAttr(testTemplateFile, "content_sha256", sha256Hex([]byte("password=changed\n"))),
			},
		},
	})
}

func TestAccTemplateFile_unknownInputs(t *testing.T) {
	requireRoot(t)
	target := filepath.Join(t.TempDir(), "late.conf")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkTemplateFileDestroyed,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "terraform_data" "id" {
  input = "x"
}

resource "sysutils_template_file" "test" {
  path     = %q
  template = "id={{ .id }}\n"
  vars     = { id = terraform_data.id.id }
}`, target),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectUnknownValue(testTemplateFile, tfjsonpath.New("rendered")),
						plancheck.ExpectUnknownValue(testTemplateFile, tfjsonpath.New("content_sha256")),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestMatchResourceAttr(testTemplateFile, "rendered", regexp.MustCompile(`^id=.+\n$`)),
				),
			},
		},
	})
}

func TestAccTemplateFile_import(t *testing.T) {
	requireRoot(t)
	target := filepath.Join(t.TempDir(), "imported.conf")
	config := fmt.Sprintf(`
resource "sysutils_template_file" "test" {
  path     = %q
  template = "hello {{ .who }}\n"
  vars     = { who = "world" }
  mode     = "0640"
  owner    = "root"
  group    = "root"
}`, target)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkTemplateFileDestroyed,
		Steps: []resource.TestStep{
			{Config: config},
			{
				ResourceName:      testTemplateFile,
				ImportState:       true,
				ImportStateVerify: true,
				// Configuration-only inputs that cannot be read back from the
				// file. rendered is not recorded on import, so that the
				// content of a file that may hold secrets does not end up in
				// a non-sensitive attribute.
				ImportStateVerifyIgnore: []string{"template", "vars", "sensitive_vars", "syntax", "rendered", "rendered_sensitive"},
			},
			{
				ResourceName:  testTemplateFile,
				ImportState:   true,
				ImportStateId: "relative/path",
				ExpectError:   regexp.MustCompile(`Import\s+ID\s+must\s+be\s+the\s+absolute\s+path`),
			},
		},
	})
}

// TestAccTemplateFile_importThenApply imports an existing file and checks
// that the first apply adopts it without rewriting it when the rendered
// content already matches.
func TestAccTemplateFile_importThenApply(t *testing.T) {
	requireRoot(t)
	target := filepath.Join(t.TempDir(), "existing.conf")
	if err := os.WriteFile(target, []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(target, old, old); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(`
resource "sysutils_template_file" "test" {
  path     = %q
  template = "hello {{ .who }}\n"
  vars     = { who = "world" }
}`, target)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkTemplateFileDestroyed,
		Steps: []resource.TestStep{
			{
				Config:             config,
				ResourceName:       testTemplateFile,
				ImportState:        true,
				ImportStateId:      target,
				ImportStatePersist: true,
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if len(states) != 1 {
						return fmt.Errorf("expected 1 state, got %d", len(states))
					}
					if got := states[0].Attributes["content_sha256"]; got != sha256Hex([]byte("hello world\n")) {
						return fmt.Errorf("content_sha256 = %q", got)
					}
					if _, ok := states[0].Attributes["rendered"]; ok && states[0].Attributes["rendered"] != "" {
						return fmt.Errorf("rendered must not be recorded on import, got %q", states[0].Attributes["rendered"])
					}
					return nil
				},
			},
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testTemplateFile, plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testTemplateFile, "rendered", "hello world\n"),
					func(*terraform.State) error {
						info, err := os.Stat(target)
						if err != nil {
							return err
						}
						if !info.ModTime().Equal(old) {
							return fmt.Errorf("file with matching content was rewritten")
						}
						return nil
					},
				),
			},
			{
				Config:   config,
				PlanOnly: true,
			},
		},
	})
}

func TestAccTemplateFile_refusesSymlink(t *testing.T) {
	requireRoot(t)
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "sysutils_template_file" "test" {
  path     = %q
  template = "pwned\n"
}`, link),
				ExpectError: regexp.MustCompile(`symbolic\s+link`),
			},
		},
	})
	if data, _ := os.ReadFile(victim); string(data) != "keep\n" {
		t.Fatalf("symlink target was modified: %q", data)
	}
}
