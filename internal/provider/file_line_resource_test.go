package provider

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

const testFileLineResource = "sysutils_file_line.test"

// fileLineConfig renders a sysutils_file_line resource named "test" with the
// given extra attributes (already formatted HCL).
func fileLineConfig(p, attrs string) string {
	return fmt.Sprintf(`
resource "sysutils_file_line" "test" {
  path = %q
%s
}`, p, attrs)
}

// setFile writes content to p, returning a PreConfig function.
func setFile(t *testing.T, p, content string) func() {
	return func() {
		t.Helper()
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// checkFileLineContent is a CheckDestroy-compatible content check.
func checkFileLineContent(p, want string) func(*terraform.State) error {
	return func(*terraform.State) error {
		got, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if string(got) != want {
			return fmt.Errorf("content of %s = %q, want %q", p, got, want)
		}
		return nil
	}
}

func TestAccFileLine_lineLifecycle(t *testing.T) {
	requireRoot(t)

	dir := t.TempDir()
	p := filepath.Join(dir, "hosts")
	const orig = "127.0.0.1 localhost\n::1 localhost\n"
	mustWrite(t, p, orig)
	// Mode and ownership that the edit must preserve.
	mustChmod(t, p, 0o640)
	if err := os.Chown(p, 65534, 65534); err != nil {
		t.Fatal(err)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkFileLineContent(p, orig),
		Steps: []resource.TestStep{
			{
				Config: fileLineConfig(p, `  line = "10.0.0.1 db"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(p, orig+"10.0.0.1 db\n"),
					checkFileMode(p, 0o640),
					checkLOwnership(p, "65534", "65534"),
					checkNoTempFiles(dir),
					resource.TestCheckResourceAttr(testFileLineResource, "id", p+":10.0.0.1 db"),
					resource.TestCheckResourceAttr(testFileLineResource, "create", "false"),
					resource.TestCheckNoResourceAttr(testFileLineResource, "block"),
				),
			},
			{
				// Idempotency: nothing to do on a second plan.
				Config: fileLineConfig(p, `  line = "10.0.0.1 db"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				// Changing the line replaces it where it is.
				PreConfig: setFile(t, p, "127.0.0.1 localhost\n10.0.0.1 db\n::1 localhost\n"),
				Config:    fileLineConfig(p, `  line = "10.0.0.2 db"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testFileLineResource, plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(p, "127.0.0.1 localhost\n10.0.0.2 db\n::1 localhost\n"),
					resource.TestCheckResourceAttr(testFileLineResource, "id", p+":10.0.0.2 db"),
				),
			},
			{
				// Drift: the line was removed outside Terraform.
				PreConfig: setFile(t, p, orig),
				Config:    fileLineConfig(p, `  line = "10.0.0.2 db"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testFileLineResource, plancheck.ResourceActionCreate),
					},
				},
				Check: checkFileContent(p, orig+"10.0.0.2 db\n"),
			},
			{
				ResourceName:      testFileLineResource,
				ImportState:       true,
				ImportStateId:     p + ":10.0.0.2 db",
				ImportStateVerify: true,
			},
		},
	})
}

func TestAccFileLine_regexpReplace(t *testing.T) {
	requireRoot(t)

	p := filepath.Join(t.TempDir(), "sshd_config")
	mustWrite(t, p, "Port 22\n#PermitRootLogin yes\nUsePAM yes\n")
	config := fileLineConfig(p, `
  line   = "PermitRootLogin no"
  regexp = "^#?PermitRootLogin\\s"`)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		// Destroy removes the managed line; the replaced original is gone.
		CheckDestroy: checkFileLineContent(p, "Port 22\nUsePAM yes\n"),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check:  checkFileContent(p, "Port 22\nPermitRootLogin no\nUsePAM yes\n"),
			},
			{
				// Someone flips the setting: refresh shows the actual line as
				// an in-place update, and apply fixes it where it is.
				PreConfig: setFile(t, p, "Port 22\nPermitRootLogin yes\nUsePAM yes\n"),
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testFileLineResource, plancheck.ResourceActionUpdate),
					},
				},
				Check: checkFileContent(p, "Port 22\nPermitRootLogin no\nUsePAM yes\n"),
			},
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}

func TestAccFileLine_blockLifecycle(t *testing.T) {
	requireRoot(t)

	dir := t.TempDir()
	p := filepath.Join(dir, "hosts")
	const orig = "127.0.0.1 localhost\n# end of static entries\n::1 localhost\n"
	mustWrite(t, p, orig)
	config := func(block string) string {
		return fileLineConfig(p, fmt.Sprintf(`
  marker       = "# {mark} cluster nodes"
  insert_after = "^# end of static"
  block        = <<-EOT
%sEOT`, block))
	}
	withBlock := func(body string) string {
		return "127.0.0.1 localhost\n# end of static entries\n# BEGIN cluster nodes\n" + body + "# END cluster nodes\n::1 localhost\n"
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkFileLineContent(p, orig),
		Steps: []resource.TestStep{
			{
				Config: config("    10.0.0.1 node1\n    10.0.0.2 node2\n"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(p, withBlock("10.0.0.1 node1\n10.0.0.2 node2\n")),
					checkNoTempFiles(dir),
					resource.TestCheckResourceAttr(testFileLineResource, "id", p+":# {mark} cluster nodes"),
				),
			},
			{
				Config: config("    10.0.0.1 node1\n    10.0.0.2 node2\n"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				// Content changed between the markers.
				PreConfig: setFile(t, p, withBlock("10.9.9.9 evil\n")),
				Config:    config("    10.0.0.1 node1\n    10.0.0.2 node2\n"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testFileLineResource, plancheck.ResourceActionUpdate),
						// Refresh recorded the actual content, so the plan diff is readable.
						plancheck.ExpectKnownValue(testFileLineResource, tfjsonpath.New("block"), knownvalue.StringExact("10.0.0.1 node1\n10.0.0.2 node2\n")),
					},
				},
				Check: checkFileContent(p, withBlock("10.0.0.1 node1\n10.0.0.2 node2\n")),
			},
			{
				// Updating the configuration rewrites the block in place.
				Config: config("    10.0.0.3 node3\n"),
				Check:  checkFileContent(p, withBlock("10.0.0.3 node3\n")),
			},
			{
				// Markers removed outside Terraform: the block is recreated.
				PreConfig: setFile(t, p, orig),
				Config:    config("    10.0.0.3 node3\n"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testFileLineResource, plancheck.ResourceActionCreate),
					},
				},
				Check: checkFileContent(p, withBlock("10.0.0.3 node3\n")),
			},
			{
				ResourceName:            testFileLineResource,
				ImportState:             true,
				ImportStateId:           p + ":# {mark} cluster nodes",
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"insert_after"},
			},
		},
	})
}

func TestAccFileLine_defaultMarkerAndBOF(t *testing.T) {
	requireRoot(t)

	p := filepath.Join(t.TempDir(), "profile")
	mustWrite(t, p, "export A=1")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkFileLineContent(p, "export A=1"),
		Steps: []resource.TestStep{
			{
				Config: fileLineConfig(p, `
  insert_before = "BOF"
  block         = "export B=2"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(p, "# BEGIN MANAGED BY TERRAFORM\nexport B=2\n# END MANAGED BY TERRAFORM\nexport A=1"),
					resource.TestCheckResourceAttr(testFileLineResource, "marker", defaultFileLineMarker),
				),
			},
			{
				// Switching from a block to a line replaces it in place.
				Config: fileLineConfig(p, `  line = "export B=3"`),
				Check:  checkFileContent(p, "export B=3\nexport A=1"),
			},
		},
	})
}

func TestAccFileLine_create(t *testing.T) {
	requireRoot(t)

	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "new.conf")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		// The file itself is kept, only the line is removed.
		CheckDestroy: checkFileLineContent(p, ""),
		Steps: []resource.TestStep{
			{
				Config:      fileLineConfig(p, `  line = "a = 1"`),
				ExpectError: regexp.MustCompile(`(?s)does\s+not\s+exist.*create\s+=\s+true`),
			},
			{
				Config: fileLineConfig(p, `
  line   = "a = 1"
  create = true`),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(p, "a = 1\n"),
					checkFileMode(p, 0o644),
				),
			},
		},
	})
}

func TestAccFileLine_manyResourcesOneFile(t *testing.T) {
	requireRoot(t)

	p := filepath.Join(t.TempDir(), "many")
	mustWrite(t, p, "")
	var config strings.Builder
	var want []string
	for i := range 12 {
		fmt.Fprintf(&config, "resource \"sysutils_file_line\" \"l%d\" {\n  path = %q\n  line = \"line %d\"\n}\n", i, p, i)
		want = append(want, fmt.Sprintf("line %d", i))
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkFileLineContent(p, ""),
		Steps: []resource.TestStep{
			{
				// Terraform applies these in parallel; none may be lost.
				Config: config.String(),
				Check: func(*terraform.State) error {
					got, err := os.ReadFile(p)
					if err != nil {
						return err
					}
					lines := strings.Split(strings.TrimSuffix(string(got), "\n"), "\n")
					for _, w := range want {
						if !slices.Contains(lines, w) {
							return fmt.Errorf("line %q missing from %q", w, got)
						}
					}
					if len(lines) != len(want) {
						return fmt.Errorf("got %d lines, want %d: %q", len(lines), len(want), got)
					}
					return nil
				},
			},
		},
	})
}

func TestAccFileLine_refusesSymlinkAtPath(t *testing.T) {
	requireRoot(t)

	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	mustWrite(t, victim, "root:x:0:0\n")
	link := filepath.Join(dir, "link")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      fileLineConfig(link, `  line = "evil::0:0"`),
				ExpectError: regexp.MustCompile(`symbolic\s+link`),
			},
			{
				Config: fileLineConfig(link, `
  block  = "evil::0:0"
  create = true`),
				ExpectError: regexp.MustCompile(`symbolic\s+link`),
			},
		},
	})

	if got, _ := os.ReadFile(victim); string(got) != "root:x:0:0\n" {
		t.Errorf("victim modified through symlink: %q", got)
	}
	if target, err := os.Readlink(link); err != nil || target != victim {
		t.Errorf("symlink modified: %q, %v", target, err)
	}
}

func TestAccFileLine_symlinkSwappedInAfterCreate(t *testing.T) {
	requireRoot(t)

	dir := t.TempDir()
	p := filepath.Join(dir, "managed")
	mustWrite(t, p, "a\n")
	victim := filepath.Join(dir, "victim")
	mustWrite(t, victim, "keep\nmanaged line\n")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: fileLineConfig(p, `  line = "managed line"`)},
			{
				PreConfig: func() {
					if err := os.Remove(p); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(victim, p); err != nil {
						t.Fatal(err)
					}
				},
				Config:      fileLineConfig(p, `  line = "managed line"`),
				ExpectError: regexp.MustCompile(`symbolic\s+link`),
			},
			{
				// Restore a regular file so that the final destroy can run.
				PreConfig: func() {
					if err := os.Remove(p); err != nil {
						t.Fatal(err)
					}
					mustWrite(t, p, "a\n")
				},
				Config: fileLineConfig(p, `  line = "managed line"`),
				Check:  checkFileContent(p, "a\nmanaged line\n"),
			},
		},
	})

	if got, _ := os.ReadFile(victim); string(got) != "keep\nmanaged line\n" {
		t.Errorf("victim modified through symlink: %q", got)
	}
}

func TestAccFileLine_rejectsInvalidConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	cases := []struct {
		attrs string
		want  string
	}{
		{``, `(?i)one\s+of`},
		{`  line = "a"
  block = "b"`, `(?i)only\s+one|conflict|exactly\s+one`},
		{`  block = "b"
  regexp = "x"`, `(?i)cannot\s+be\s+configured|conflict`},
		{`  line = "b"
  marker = "# {mark}"`, `(?i)cannot\s+be\s+configured|conflict`},
		{`  line = "a\nb"`, `line\s+breaks`},
		{`  line = "a"
  regexp = "("`, `(?i)regular\s+expression`},
		{`  block = "b"
  marker = "# managed"`, `\{mark\}`},
		{`  line = "a"
  insert_after = "EOF"
  insert_before = "BOF"`, `(?i)cannot\s+be\s+configured|conflict`},
		{`  block = "# BEGIN MANAGED BY TERRAFORM"`, `marker\s+line`},
	}
	steps := make([]resource.TestStep, 0, len(cases))
	for _, c := range cases {
		steps = append(steps, resource.TestStep{
			Config:      fileLineConfig(p, c.attrs),
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(c.want),
		})
	}
	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps:                    steps,
	})
	if _, err := os.Stat(p); err == nil {
		t.Errorf("%s was created by an invalid configuration", p)
	}
}

func TestAccFileLine_importInvalidID(t *testing.T) {
	requireRoot(t)

	p := filepath.Join(t.TempDir(), "f")
	mustWrite(t, p, "present\n")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:        fileLineConfig(p, `  line = "present"`),
				ResourceName:  testFileLineResource,
				ImportState:   true,
				ImportStateId: p,
				ExpectError:   regexp.MustCompile(`<path>:<marker>`),
			},
			{
				Config:        fileLineConfig(p, `  line = "present"`),
				ResourceName:  testFileLineResource,
				ImportState:   true,
				ImportStateId: p + ":absent",
				ExpectError:   regexp.MustCompile(`(?i)non-existent|cannot\s+import`),
			},
		},
	})
}

// checkNoTempFiles fails if an atomic-write temporary file was left in dir.
func checkNoTempFiles(dir string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if strings.Contains(e.Name(), ".sysutils-tmp-") {
				return fmt.Errorf("temporary file %s left in %s", e.Name(), dir)
			}
		}
		return nil
	}
}
