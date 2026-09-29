package provider

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// checkRootOwned checks that p is owned by root:root with mode 0644, as
// cron requires.
func checkRootOwned(p string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		st := info.Sys().(*syscall.Stat_t)
		if st.Uid != 0 || st.Gid != 0 || info.Mode() != cronFileMode {
			return fmt.Errorf("%s has owner %d:%d and mode %v, want 0:0 and %v", p, st.Uid, st.Gid, info.Mode(), cronFileMode)
		}
		return nil
	}
}

// TestAccCronJobResource_system manages a job in the real /etc/cron.d with
// the provider's defaults. The job runs once a year and does nothing.
func TestAccCronJobResource_system(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("acceptance test; set TF_ACC=1 to run")
	}
	requireRoot(t)
	name := "sysutils-acc-test-" + randomID()
	p := filepath.Join(defaultCronDir, name)
	t.Cleanup(func() { _ = os.Remove(p) })
	config := cronJobHCL(name, `
  schedule = "0 0 1 1 *"
  command  = "true"
  comment  = "Created by the sysutils acceptance tests; safe to delete."
`)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkCronJobGone(p),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeTestCheckFunc(
					checkRootOwned(p),
					checkCronJobFile(p, cronFileHeader+"\n# Created by the sysutils acceptance tests; safe to delete.\n0 0 1 1 * root true\n"),
				),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(testCronJobResource, tfjsonpath.New("path"), knownvalue.StringExact(p)),
					statecheck.ExpectKnownValue(testCronJobResource, tfjsonpath.New("owner"), knownvalue.StringExact("root")),
					statecheck.ExpectKnownValue(testCronJobResource, tfjsonpath.New("group"), knownvalue.StringExact(gidName(0))),
				},
			},
			{
				// cron ignores files not owned by root; a changed owner or
				// group is drift that apply reverts.
				PreConfig: func() {
					if err := os.Chown(p, 65534, 65534); err != nil {
						t.Fatal(err)
					}
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testCronJobResource, plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue(testCronJobResource, tfjsonpath.New("owner"), knownvalue.StringExact("root")),
					},
				},
				Check: checkRootOwned(p),
			},
			{
				ResourceName:      testCronJobResource,
				ImportState:       true,
				ImportStateId:     name,
				ImportStateVerify: true,
			},
		},
	})
}
