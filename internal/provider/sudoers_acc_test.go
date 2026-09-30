package provider

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// checkSudoersRootOwned checks that p is owned by root:root with mode 0440.
func checkSudoersRootOwned(p string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		st := info.Sys().(*syscall.Stat_t)
		if st.Uid != 0 || st.Gid != 0 || info.Mode() != sudoersFileMode {
			return fmt.Errorf("%s has owner %d:%d and mode %v, want 0:0 and %v", p, st.Uid, st.Gid, info.Mode(), sudoersFileMode)
		}
		return nil
	}
}

// checkSudoListsRule checks that sudo itself reads the drop-in: "sudo -l -U
// user" lists the rules of user from all of sudoers, including sudoers.d.
func checkSudoListsRule(user, want string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		out, err := exec.Command("sudo", "-n", "-l", "-U", user).CombinedOutput()
		if err != nil {
			return fmt.Errorf("sudo -l -U %s: %v: %s", user, err, out)
		}
		if !strings.Contains(string(out), want) {
			return fmt.Errorf("sudo -l -U %s does not list %q:\n%s", user, want, out)
		}
		return nil
	}
}

// TestAccSudoersResource_system manages a drop-in in the real
// /etc/sudoers.d with the provider's defaults and the host's visudo. The
// rule is for the root user, who may run anything anyway, so it grants
// nothing.
func TestAccSudoersResource_system(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("acceptance test; set TF_ACC=1 to run")
	}
	requireRoot(t)
	requireVisudo(t)
	name := "sysutils-acc-test-" + randomID()
	p := filepath.Join(defaultSudoersDir, name)
	t.Cleanup(func() { _ = os.Remove(p) })
	config := sudoersHCL(name, `
  rules = [{
    users    = ["root"]
    runas    = "root"
    nopasswd = true
    commands = ["/usr/bin/true"]
  }]
`)
	want := sudoersFileHeader + "\nroot ALL = (root) NOPASSWD: /usr/bin/true\n"
	// visudo's parser, not the resource's validators, rejects this.
	invalid := sudoersHCL(name, `  content = "root ALL = (root NOPASSWD: /usr/bin/true\n"`)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkSudoersGone(p),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeTestCheckFunc(
					checkSudoersRootOwned(p),
					checkSudoersDropIn(p, want),
					checkSudoListsRule("root", "(root) NOPASSWD: /usr/bin/true"),
				),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(testSudoersResource, tfjsonpath.New("path"), knownvalue.StringExact(p)),
					statecheck.ExpectKnownValue(testSudoersResource, tfjsonpath.New("owner"), knownvalue.StringExact("root")),
					statecheck.ExpectKnownValue(testSudoersResource, tfjsonpath.New("group"), knownvalue.StringExact(gidName(0))),
				},
			},
			{
				// The real visudo rejects a syntax error, and the file
				// stays as it was.
				Config:      invalid,
				ExpectError: regexp.MustCompile(`Invalid\s+sudoers\s+file[\s\S]*` + regexp.QuoteMeta(p) + `:1:\d+:\s+syntax\s+error`),
			},
			{
				PreConfig: func() {
					assertSudoersFile(t, p, want)
					entries, err := os.ReadDir(defaultSudoersDir)
					if err != nil {
						t.Fatal(err)
					}
					for _, e := range entries {
						if strings.Contains(e.Name(), name) && e.Name() != name {
							t.Errorf("temporary file %s left behind", e.Name())
						}
					}
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				// A changed owner or mode is drift that apply reverts.
				PreConfig: func() {
					if err := os.Chown(p, 65534, 65534); err != nil {
						t.Fatal(err)
					}
					if err := os.Chmod(p, 0o644); err != nil {
						t.Fatal(err)
					}
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testSudoersResource, plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue(testSudoersResource, tfjsonpath.New("owner"), knownvalue.StringExact("root")),
						plancheck.ExpectKnownValue(testSudoersResource, tfjsonpath.New("mode"), knownvalue.StringExact("0440")),
					},
				},
				Check: checkSudoersRootOwned(p),
			},
			{
				// Content changed by hand is drift, too.
				PreConfig: func() {
					if err := os.WriteFile(p, []byte(want+"root ALL = ALL\n"), 0o440); err != nil {
						t.Fatal(err)
					}
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testSudoersResource, plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue(testSudoersResource, tfjsonpath.New("content"), knownvalue.StringExact(want)),
					},
				},
				Check: checkSudoersDropIn(p, want),
			},
			{
				ResourceName:            testSudoersResource,
				ImportState:             true,
				ImportStateId:           name,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"rules"},
			},
		},
	})
}
