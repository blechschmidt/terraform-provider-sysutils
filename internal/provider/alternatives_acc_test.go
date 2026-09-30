package provider

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// altAccEnv is a link group of the host's real alternatives system, with
// a unique name, its master link and two alternatives in a temporary
// directory. The group is removed when the test ends.
type altAccEnv struct {
	tool *alternativesTool
	name string
	link string
	a, b string
}

func newAltAccEnv(t *testing.T) *altAccEnv {
	t.Helper()
	if os.Getenv("TF_ACC") == "" {
		t.Skip("acceptance test; set TF_ACC=1 to run")
	}
	requireRoot(t)
	tool, err := (*alternativesConfig)(nil).tool()
	if err != nil {
		t.Skipf("no alternatives tool: %v", err)
	}
	dir := t.TempDir()
	e := &altAccEnv{
		tool: tool,
		name: "tfacc-sysutils-alt-" + randomID(),
		link: filepath.Join(dir, "bin", "tfacc-tool"),
		a:    filepath.Join(dir, "a", "tool"),
		b:    filepath.Join(dir, "b", "tool"),
	}
	for _, p := range []string{e.a, e.b} {
		mustWrite(t, p, "#!/bin/sh\n")
		if err := os.Chmod(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(e.link), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command(tool.bin, "--remove-all", e.name).Run()
	})
	return e
}

// run runs the real tool outside Terraform.
func (e *altAccEnv) run(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command(e.tool.bin, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v: %s", e.tool.bin, strings.Join(args, " "), err, out)
	}
}

// check checks the real link group's mode and selection, or that it is
// gone if mode is "gone".
func (e *altAccEnv) check(mode, value string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		st, err := e.tool.Query(context.Background(), e.name)
		if err != nil {
			return err
		}
		if mode == "gone" {
			if st.Found {
				return fmt.Errorf("link group %s still exists: %+v", e.name, st)
			}
			return nil
		}
		if !st.Found {
			return fmt.Errorf("link group %s does not exist", e.name)
		}
		if st.Mode != mode || st.selected() != value {
			return fmt.Errorf("link group %s is in %s mode pointing to %q, want %s mode pointing to %q", e.name, st.Mode, st.Value, mode, value)
		}
		// The master link resolves to the selection.
		if target, err := filepath.EvalSymlinks(e.link); err != nil || target != value {
			return fmt.Errorf("%s resolves to %q (%v), want %q", e.link, target, err, value)
		}
		return nil
	}
}

func TestAccAlternatives_lifecycle(t *testing.T) {
	e := newAltAccEnv(t)
	install := func(p string) string {
		return alternativesTF(e.name, p, fmt.Sprintf(`  link     = %q
  priority = 10`, e.link))
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		// Destroy returns to automatic mode, which picks the alternative
		// with the higher priority, and unregisters nothing.
		CheckDestroy: e.check(alternativesModeAuto, e.b),
		Steps: []resource.TestStep{
			{
				Config: install(e.a),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testAlternativesResource, "mode", "manual"),
					resource.TestCheckResourceAttr(testAlternativesResource, "path", e.a),
					resource.TestCheckResourceAttr(testAlternativesResource, "link", e.link),
					resource.TestCheckResourceAttr(testAlternativesResource, "priority", "10"),
					e.check(alternativesModeManual, e.a),
				),
			},
			{
				// A package registers a better alternative and the group
				// is returned to automatic mode: drift, and reverted.
				PreConfig: func() {
					e.run(t, "--install", e.link, e.name, e.b, "50")
					e.run(t, "--auto", e.name)
				},
				Config: install(e.a),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testAlternativesResource, plancheck.ResourceActionUpdate)},
				},
				Check: e.check(alternativesModeManual, e.a),
			},
			{
				// Another alternative selected by hand.
				PreConfig: func() { e.run(t, "--set", e.name, e.b) },
				Config:    install(e.a),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testAlternativesResource, plancheck.ResourceActionUpdate)},
				},
				Check: e.check(alternativesModeManual, e.a),
			},
			{
				// Priority changed by re-registering: drift.
				PreConfig:          func() { e.run(t, "--install", e.link, e.name, e.a, "20") },
				Config:             install(e.a),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				Config: install(e.a),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testAlternativesResource, "priority", "10"),
					e.check(alternativesModeManual, e.a),
				),
			},
			{
				// Select the alternative registered outside Terraform.
				Config: alternativesTF(e.name, e.b, ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testAlternativesResource, "path", e.b),
					resource.TestCheckNoResourceAttr(testAlternativesResource, "link"),
					e.check(alternativesModeManual, e.b),
				),
			},
			{
				ResourceName:      testAlternativesResource,
				ImportState:       true,
				ImportStateId:     e.name,
				ImportStateVerify: true,
			},
			{
				Config:      alternativesTF(e.name, filepath.Join(filepath.Dir(e.a), "other"), ""),
				ExpectError: regexp.MustCompile(`is\s+not\s+an\s+alternative\s+of`),
			},
		},
	})
}

func TestAccAlternatives_removeOnDestroy(t *testing.T) {
	e := newAltAccEnv(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		// The only alternative was removed, and the group and its link
		// with it.
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			e.check("gone", ""),
			checkPathGone(e.link),
		),
		Steps: []resource.TestStep{
			{
				Config: alternativesTF(e.name, e.a, fmt.Sprintf(`  link              = %q
  priority          = -5
  remove_on_destroy = true`, e.link)),
				Check: e.check(alternativesModeManual, e.a),
			},
			{
				// Group removed outside Terraform: recreated.
				PreConfig: func() { e.run(t, "--remove-all", e.name) },
				Config: alternativesTF(e.name, e.a, fmt.Sprintf(`  link              = %q
  priority          = -5
  remove_on_destroy = true`, e.link)),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testAlternativesResource, plancheck.ResourceActionCreate)},
				},
				Check: e.check(alternativesModeManual, e.a),
			},
		},
	})
}

func TestAccAlternatives_unknownGroup(t *testing.T) {
	e := newAltAccEnv(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config:      alternativesTF(e.name, e.a, ""),
			ExpectError: regexp.MustCompile(`Unknown\s+link\s+group`),
		}},
	})
}
