package provider

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const testAlternativesResource = "sysutils_alternatives.test"

func alternativesTF(name, p, extra string) string {
	return fmt.Sprintf(`
resource "sysutils_alternatives" "test" {
  name = %q
  path = %q
%s
}
`, name, p, extra)
}

// checkAltGroup checks the fake's link group: its mode and selection, or
// that it is gone if value is "gone".
func checkAltGroup(f *fakeAlternatives, name string, manual bool, value string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		g := f.get(name)
		if value == "gone" {
			if g != nil {
				return fmt.Errorf("link group %s still exists: %+v", name, g)
			}
			return nil
		}
		if g == nil {
			return fmt.Errorf("link group %s does not exist", name)
		}
		if g.manual != manual || g.value != value {
			return fmt.Errorf("link group %s: manual=%t value=%q, want manual=%t value=%q", name, g.manual, g.value, manual, value)
		}
		return nil
	}
}

// checkAltChanges checks the commands that changed link groups since the
// last check, and forgets them.
func checkAltChanges(f *fakeAlternatives, want ...string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		got := f.changes()
		f.clearCalls()
		if !slices.Equal(got, want) {
			return fmt.Errorf("commands:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
		}
		return nil
	}
}

func addEditorGroup(f *fakeAlternatives) {
	f.add("editor", "/usr/bin/editor",
		alternativeEntry{Path: "/bin/nano", Priority: 40},
		alternativeEntry{Path: "/usr/bin/vim.basic", Priority: 30},
		alternativeEntry{Path: "/bin/ed", Priority: -100})
}

func TestAlternatives_selectLifecycle(t *testing.T) {
	for _, kind := range []string{alternativesDebian, alternativesRHEL} {
		t.Run(kind, func(t *testing.T) {
			f := newFakeAlternatives(t, kind)
			addEditorGroup(f)
			vim := alternativesTF("editor", "/usr/bin/vim.basic", "")
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: f.providerFactories(),
				// Destroy returns to automatic mode and keeps every
				// alternative.
				CheckDestroy: resource.ComposeAggregateTestCheckFunc(
					checkAltGroup(f, "editor", false, "/bin/nano"),
					checkAltChanges(f, "auto editor"),
					func(*terraform.State) error {
						if n := len(f.get("editor").alts); n != 3 {
							return fmt.Errorf("%d alternatives left, want 3", n)
						}
						return nil
					},
				),
				Steps: []resource.TestStep{
					{
						Config: vim,
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckResourceAttr(testAlternativesResource, "id", "editor"),
							resource.TestCheckResourceAttr(testAlternativesResource, "path", "/usr/bin/vim.basic"),
							resource.TestCheckResourceAttr(testAlternativesResource, "mode", "manual"),
							resource.TestCheckResourceAttr(testAlternativesResource, "remove_on_destroy", "false"),
							resource.TestCheckNoResourceAttr(testAlternativesResource, "link"),
							resource.TestCheckNoResourceAttr(testAlternativesResource, "priority"),
							checkAltGroup(f, "editor", true, "/usr/bin/vim.basic"),
							checkAltChanges(f, "set editor /usr/bin/vim.basic"),
						),
					},
					{
						// Returned to automatic mode outside Terraform.
						PreConfig: func() { f.set("editor", false, "") },
						Config:    vim,
						ConfigPlanChecks: resource.ConfigPlanChecks{
							PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testAlternativesResource, plancheck.ResourceActionUpdate)},
						},
						Check: resource.ComposeAggregateTestCheckFunc(
							checkAltGroup(f, "editor", true, "/usr/bin/vim.basic"),
							checkAltChanges(f, "set editor /usr/bin/vim.basic"),
						),
					},
					{
						// Another alternative selected outside Terraform.
						PreConfig: func() { f.set("editor", true, "/bin/ed") },
						Config:    vim,
						ConfigPlanChecks: resource.ConfigPlanChecks{
							PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testAlternativesResource, plancheck.ResourceActionUpdate)},
						},
						Check: resource.ComposeAggregateTestCheckFunc(
							checkAltGroup(f, "editor", true, "/usr/bin/vim.basic"),
							checkAltChanges(f, "set editor /usr/bin/vim.basic"),
						),
					},
					{
						// Selecting the best alternative still pins it.
						Config: alternativesTF("editor", "/bin/nano", ""),
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckResourceAttr(testAlternativesResource, "path", "/bin/nano"),
							checkAltGroup(f, "editor", true, "/bin/nano"),
							checkAltChanges(f, "set editor /bin/nano"),
						),
					},
					{
						ResourceName:      testAlternativesResource,
						ImportState:       true,
						ImportStateId:     "editor",
						ImportStateVerify: true,
					},
					{
						// Nothing to do when nothing changed.
						Config: alternativesTF("editor", "/bin/nano", ""),
						Check:  checkAltChanges(f),
					},
				},
			})
		})
	}
}

func TestAlternatives_installAndRemove(t *testing.T) {
	for _, kind := range []string{alternativesDebian, alternativesRHEL} {
		t.Run(kind, func(t *testing.T) {
			f := newFakeAlternatives(t, kind)
			cfg := func(link string, priority int) string {
				return alternativesTF("tfjava", "/opt/jdk-21/bin/java", fmt.Sprintf(`  link              = %q
  priority          = %d
  remove_on_destroy = true`, link, priority))
			}
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: f.providerFactories(),
				// The last alternative was removed, and the group with it.
				CheckDestroy: resource.ComposeAggregateTestCheckFunc(
					checkAltGroup(f, "tfjava", false, "gone"),
					checkAltChanges(f, "remove tfjava /opt/jdk-21/bin/java"),
				),
				Steps: []resource.TestStep{
					{
						Config: cfg("/usr/local/bin/tfjava", 100),
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckResourceAttr(testAlternativesResource, "link", "/usr/local/bin/tfjava"),
							resource.TestCheckResourceAttr(testAlternativesResource, "priority", "100"),
							resource.TestCheckResourceAttr(testAlternativesResource, "mode", "manual"),
							checkAltGroup(f, "tfjava", true, "/opt/jdk-21/bin/java"),
							checkAltChanges(f,
								"install /usr/local/bin/tfjava tfjava /opt/jdk-21/bin/java 100",
								"set tfjava /opt/jdk-21/bin/java"),
						),
					},
					{
						// Priority changed outside Terraform: registered
						// again, but already selected.
						PreConfig: func() {
							f.mu.Lock()
							f.groups["tfjava"].alts[0].Priority = 5
							f.syncAdminFile("tfjava")
							f.mu.Unlock()
						},
						Config: cfg("/usr/local/bin/tfjava", 100),
						ConfigPlanChecks: resource.ConfigPlanChecks{
							PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testAlternativesResource, plancheck.ResourceActionUpdate)},
						},
						Check: checkAltChanges(f, "install /usr/local/bin/tfjava tfjava /opt/jdk-21/bin/java 100"),
					},
					{
						// A new link and a negative priority.
						Config: cfg("/usr/bin/tfjava", -5),
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckResourceAttr(testAlternativesResource, "link", "/usr/bin/tfjava"),
							resource.TestCheckResourceAttr(testAlternativesResource, "priority", "-5"),
							checkAltChanges(f, "install /usr/bin/tfjava tfjava /opt/jdk-21/bin/java -5"),
						),
					},
					{
						// Link renamed outside Terraform.
						PreConfig: func() {
							f.mu.Lock()
							f.groups["tfjava"].link = "/usr/bin/java-other"
							f.syncAdminFile("tfjava")
							f.mu.Unlock()
						},
						Config:             cfg("/usr/bin/tfjava", -5),
						PlanOnly:           true,
						ExpectNonEmptyPlan: true,
					},
					{
						Config: cfg("/usr/bin/tfjava", -5),
						Check:  checkAltChanges(f, "install /usr/bin/tfjava tfjava /opt/jdk-21/bin/java -5"),
					},
					{
						// Unregistered outside Terraform: registered and
						// selected again.
						PreConfig: func() {
							f.mu.Lock()
							delete(f.groups, "tfjava")
							f.syncAdminFile("tfjava")
							f.mu.Unlock()
						},
						Config: cfg("/usr/bin/tfjava", -5),
						Check: resource.ComposeAggregateTestCheckFunc(
							checkAltGroup(f, "tfjava", true, "/opt/jdk-21/bin/java"),
							checkAltChanges(f,
								"install /usr/bin/tfjava tfjava /opt/jdk-21/bin/java -5",
								"set tfjava /opt/jdk-21/bin/java"),
						),
					},
				},
			})
		})
	}
}

// TestAlternatives_removeKeepsOtherAlternatives checks remove_on_destroy
// on a group with other alternatives: only path goes, and the group returns
// to automatic mode with the best remaining alternative.
func TestAlternatives_removeKeepsOtherAlternatives(t *testing.T) {
	f := newFakeAlternatives(t, alternativesDebian)
	addEditorGroup(f)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkAltGroup(f, "editor", false, "/bin/nano"),
			checkAltChanges(f, "remove editor /opt/editor"),
		),
		Steps: []resource.TestStep{{
			Config: alternativesTF("editor", "/opt/editor", `  link              = "/usr/bin/editor"
  priority          = 10
  remove_on_destroy = true`),
			Check: resource.ComposeAggregateTestCheckFunc(
				checkAltGroup(f, "editor", true, "/opt/editor"),
				checkAltChanges(f, "install /usr/bin/editor editor /opt/editor 10", "set editor /opt/editor"),
			),
		}},
	})
}

func TestAlternatives_notRegistered(t *testing.T) {
	for _, kind := range []string{alternativesDebian, alternativesRHEL} {
		t.Run(kind, func(t *testing.T) {
			f := newFakeAlternatives(t, kind)
			addEditorGroup(f)
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: f.providerFactories(),
				Steps: []resource.TestStep{
					{
						Config:      alternativesTF("editor", "/usr/bin/emacs", ""),
						ExpectError: regexp.MustCompile(`(?s)/usr/bin/emacs\s+is\s+not\s+an\s+alternative\s+of\s+editor.*/bin/nano,\s+/usr/bin/vim.basic,\s+/bin/ed.*Set\s+link\s+and\s+priority`),
					},
					{
						Config:      alternativesTF("nosuch", "/usr/bin/emacs", ""),
						ExpectError: regexp.MustCompile(`(?s)Unknown\s+link\s+group.*knows\s+no\s+link\s+group\s+"nosuch"`),
					},
				},
			})
			if got := f.changes(); len(got) != 0 {
				t.Errorf("failed plans changed link groups: %q", got)
			}
		})
	}
}

// TestAlternatives_removedOutsideTerraform checks that a link group that no
// longer exists is dropped from state and recreated.
func TestAlternatives_removedOutsideTerraform(t *testing.T) {
	f := newFakeAlternatives(t, alternativesDebian)
	cfg := alternativesTF("tfed", "/opt/ed", `  link     = "/usr/local/bin/tfed"
  priority = 1`)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		Steps: []resource.TestStep{
			{Config: cfg},
			{
				PreConfig: func() {
					f.mu.Lock()
					delete(f.groups, "tfed")
					f.mu.Unlock()
				},
				Config: cfg,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testAlternativesResource, plancheck.ResourceActionCreate)},
				},
				Check: checkAltGroup(f, "tfed", true, "/opt/ed"),
			},
		},
	})
}

// TestAlternatives_deletedTargetIsDrift checks that a selected alternative
// that is no longer registered, which dpkg still reports as the value,
// shows up as drift.
func TestAlternatives_deletedTargetIsDrift(t *testing.T) {
	f := newFakeAlternatives(t, alternativesDebian)
	addEditorGroup(f)
	cfg := alternativesTF("editor", "/usr/bin/vim.basic", "")
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		Steps: []resource.TestStep{
			{Config: cfg},
			{
				PreConfig: func() {
					f.mu.Lock()
					g := f.groups["editor"]
					g.alts = slices.DeleteFunc(g.alts, func(a alternativeEntry) bool { return a.Path == "/usr/bin/vim.basic" })
					f.mu.Unlock()
				},
				Config:      cfg,
				ExpectError: regexp.MustCompile(`/usr/bin/vim.basic\s+is\s+not\s+an\s+alternative\s+of\s+editor`),
			},
			{
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
				Check: func(s *terraform.State) error {
					attrs := s.RootModule().Resources[testAlternativesResource].Primary.Attributes
					if p, ok := attrs["path"]; !ok || p != "" {
						return fmt.Errorf("path = %q (set: %t), want an empty string", p, ok)
					}
					return nil
				},
			},
			{
				Config: alternativesTF("editor", "/bin/ed", ""),
				Check:  checkAltGroup(f, "editor", true, "/bin/ed"),
			},
		},
	})
}

// TestAlternatives_destroyAfterTargetUnregistered checks that destroy with
// remove_on_destroy succeeds without a command once the alternative was
// unregistered outside Terraform, which leaves path empty in state.
func TestAlternatives_destroyAfterTargetUnregistered(t *testing.T) {
	f := newFakeAlternatives(t, alternativesDebian)
	addEditorGroup(f)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkAltChanges(f),
			checkAltGroup(f, "editor", true, "/opt/editor"),
		),
		Steps: []resource.TestStep{
			{
				Config: alternativesTF("editor", "/opt/editor", `  link              = "/usr/bin/editor"
  priority          = 10
  remove_on_destroy = true`),
			},
			{
				// dpkg keeps reporting the unregistered alternative as the
				// value until the group is next changed.
				PreConfig: func() {
					f.mu.Lock()
					g := f.groups["editor"]
					g.alts = slices.DeleteFunc(g.alts, func(a alternativeEntry) bool { return a.Path == "/opt/editor" })
					f.mu.Unlock()
					f.clearCalls()
				},
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
				Check:              resource.TestCheckResourceAttr(testAlternativesResource, "path", ""),
			},
		},
	})
}

func TestAlternatives_setWithoutEffect(t *testing.T) {
	f := newFakeAlternatives(t, alternativesDebian)
	addEditorGroup(f)
	f.ignoreSet = true
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		Steps: []resource.TestStep{{
			Config:      alternativesTF("editor", "/bin/ed", ""),
			ExpectError: regexp.MustCompile(`(?s)Alternative\s+not\s+selected.*--set\s+succeeded,\s+but\s+editor\s+points\s+to\s+"/bin/nano"\s+in\s+auto\s+mode`),
		}},
	})
}

func TestAlternatives_setFails(t *testing.T) {
	f := newFakeAlternatives(t, alternativesDebian)
	addEditorGroup(f)
	f.failSet = true
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		Steps: []resource.TestStep{{
			Config:      alternativesTF("editor", "/bin/ed", ""),
			ExpectError: regexp.MustCompile(`(?s)Selecting\s+alternative.*exit\s+status\s+2:\s+update-alternatives:\s+error:\s+cannot\s+set`),
		}},
	})
}

func TestAlternatives_importUnknown(t *testing.T) {
	f := newFakeAlternatives(t, alternativesRHEL)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		Steps: []resource.TestStep{{
			Config:        alternativesTF("nosuch", "/bin/ed", ""),
			ResourceName:  testAlternativesResource,
			ImportState:   true,
			ImportStateId: "nosuch",
			ExpectError:   regexp.MustCompile(`Cannot\s+import\s+alternatives`),
		}, {
			Config:        alternativesTF("nosuch", "/bin/ed", ""),
			ResourceName:  testAlternativesResource,
			ImportState:   true,
			ImportStateId: "-x",
			ExpectError:   regexp.MustCompile(`Import\s+ID\s+must\s+be\s+the\s+name\s+of\s+a\s+link\s+group`),
		}},
	})
}

func TestAlternatives_invalidConfig(t *testing.T) {
	f := newFakeAlternatives(t, alternativesDebian)
	cases := []struct {
		name, path, extra string
		want              string
	}{
		{"-editor", "/bin/ed", "", `Invalid\s+alternatives\s+name`},
		{"a/b", "/bin/ed", "", `Invalid\s+alternatives\s+name`},
		{"a b", "/bin/ed", "", `Invalid\s+alternatives\s+name`},
		{"editor", "bin/ed", "", `must\s+be\s+absolute`},
		{"editor", "--auto", "", `must\s+be\s+absolute`},
		{"editor", "/bin/../ed", "", `canonical\s+form`},
		{"editor", "/opt/my editor", "", `must\s+not\s+contain\s+white\s+space`},
		{"editor", "/opt/ed\nx", "", `must\s+not\s+contain\s+white\s+space`},
		{"editor", "/", "", `must\s+not\s+be\s+the\s+filesystem\s+root`},
		{"editor", "/bin/ed", `  link = "/usr/bin/editor"`, `(?s)must\s+be\s+configured\s+together:\s+\[link,priority\]`},
		{"editor", "/bin/ed", `  priority = 3`, `(?s)must\s+be\s+configured\s+together:\s+\[link,priority\]`},
		{"editor", "/bin/ed", "  link = \"/bin/ed\"\n  priority = 3", `link\s+must\s+differ\s+from\s+path`},
		{"editor", "/bin/ed", "  link = \"-l\"\n  priority = 3", `must\s+be\s+absolute`},
		{"editor", "/bin/ed", "  link = \"/usr/bin/editor\"\n  priority = 2147483648", `between`},
	}
	var steps []resource.TestStep
	for _, c := range cases {
		steps = append(steps, resource.TestStep{
			Config:      alternativesTF(c.name, c.path, c.extra),
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(c.want),
		})
	}
	resource.UnitTest(t, resource.TestCase{ProtoV6ProviderFactories: f.providerFactories(), Steps: steps})
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) != 0 {
		t.Errorf("invalid configurations ran commands: %q", f.calls)
	}
}

func TestAlternatives_rootDirRefused(t *testing.T) {
	f := newFakeAlternatives(t, alternativesDebian)
	addEditorGroup(f)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		Steps: []resource.TestStep{{
			Config: fmt.Sprintf(`
provider "sysutils" {
  root_dir = %q
}
`, t.TempDir()) + alternativesTF("editor", "/bin/ed", ""),
			ExpectError: regexp.MustCompile(`Not\s+supported\s+with\s+root_dir`),
		}},
	})
}

func TestAlternatives_noTool(t *testing.T) {
	f := newFakeAlternatives(t, "none")
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: f.providerFactories(),
		Steps: []resource.TestStep{{
			Config:      alternativesTF("editor", "/bin/ed", ""),
			ExpectError: regexp.MustCompile(`(?s)Alternatives\s+tool\s+not\s+found.*Neither\s+update-alternatives`),
		}},
	})
}
