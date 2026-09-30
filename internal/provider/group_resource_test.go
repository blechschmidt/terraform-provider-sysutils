package provider

import (
	"fmt"
	"os/exec"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const testGroupResource = "sysutils_group.test"

func TestFindGroupEntry(t *testing.T) {
	const db = `# comment
root:x:0:

adm:x:4:syslog,alice
+nisgroup
-excluded
audio:x:29: pulse , lisa ,
`
	tests := []struct {
		name string
		want *groupEntry
	}{
		{"root", &groupEntry{Name: "root", GID: 0, Members: []string{}}},
		{"adm", &groupEntry{Name: "adm", GID: 4, Members: []string{"syslog", "alice"}}},
		{"audio", &groupEntry{Name: "audio", GID: 29, Members: []string{"pulse", "lisa"}}},
		{"missing", nil},
		{"nisgroup", nil},
		{"+nisgroup", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := findGroupEntry(strings.NewReader(db), tt.name)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestFindGroupEntry_malformed(t *testing.T) {
	for _, db := range []string{
		"root:x:0\n",
		"root:x:zero:\n",
	} {
		if _, err := findGroupEntry(strings.NewReader(db), "root"); err == nil {
			t.Errorf("expected error for %q", db)
		}
	}
}

func TestValidateAccountName(t *testing.T) {
	for _, ok := range []string{"a", "app", "app_svc", "app-svc", "App.Svc", "_apt", "host$", "a1", strings.Repeat("a", 32)} {
		if err := validateAccountName(ok); err != nil {
			t.Errorf("validateAccountName(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "-g", ".hidden", "a:b", "a,b", "a b", "a\nb", "a/b", "a$b", "1234", strings.Repeat("a", 33)} {
		if err := validateAccountName(bad); err == nil {
			t.Errorf("validateAccountName(%q) = nil, want error", bad)
		}
	}
}

func TestAccGroup_lifecycle(t *testing.T) {
	requireRoot(t)

	name := uniqueUsername("tfgrp")
	gid1, gid2 := freeGID(t, 61000), freeGID(t, 61500)

	config := func(gid int64, members string) string {
		return fmt.Sprintf(`
resource "sysutils_group" "test" {
  name    = %q
  gid     = %d
  members = %s
}`, name, gid, members)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkGroupDestroyed(name),
		Steps: []resource.TestStep{
			{
				Config: config(gid1, `["root"]`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testGroupResource, "id", name),
					resource.TestCheckResourceAttr(testGroupResource, "name", name),
					resource.TestCheckResourceAttr(testGroupResource, "gid", strconv.FormatInt(gid1, 10)),
					resource.TestCheckTypeSetElemAttr(testGroupResource, "members.*", "root"),
					resource.TestCheckResourceAttr(testGroupResource, "members.#", "1"),
					checkGroup(name, gid1, "root"),
				),
			},
			{
				// gid and members change in place.
				Config: config(gid2, `["nobody", "daemon"]`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testGroupResource, plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testGroupResource, "gid", strconv.FormatInt(gid2, 10)),
					resource.TestCheckResourceAttr(testGroupResource, "members.#", "2"),
					checkGroup(name, gid2, "daemon", "nobody"),
				),
			},
			{
				// An empty set clears the member list.
				Config: config(gid2, `[]`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testGroupResource, "members.#", "0"),
					checkGroup(name, gid2),
				),
			},
			{
				ResourceName:      testGroupResource,
				ImportState:       true,
				ImportStateVerify: true,
				// Not recorded in /etc/group; memberless groups import with
				// members unset.
				ImportStateVerifyIgnore: []string{"system", "members"},
			},
		},
	})
}

func TestAccGroup_computedGIDAndSystem(t *testing.T) {
	requireRoot(t)

	name := uniqueUsername("tfsys")
	config := func(system bool) string {
		return fmt.Sprintf(`
resource "sysutils_group" "test" {
  name   = %q
  system = %t
}`, name, system)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkGroupDestroyed(name),
		Steps: []resource.TestStep{
			{
				Config: config(true),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(testGroupResource, "gid"),
					resource.TestCheckNoResourceAttr(testGroupResource, "members"),
					checkGroupGIDBelow(name, 1000),
				),
			},
			{
				// The system-assigned gid is kept on unrelated refreshes.
				Config: config(true),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				Config: config(false),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testGroupResource, plancheck.ResourceActionDestroyBeforeCreate),
					},
				},
				Check: checkGroupGIDAtLeast(name, 1000),
			},
		},
	})
}

func TestAccGroup_drift(t *testing.T) {
	requireRoot(t)

	name := uniqueUsername("tfdrift")
	gid, otherGID := freeGID(t, 62000), freeGID(t, 62500)
	config := fmt.Sprintf(`
resource "sysutils_group" "test" {
  name    = %q
  gid     = %d
  members = ["root", "nobody"]
}`, name, gid)

	mutate := func(name string, args ...string) func() {
		return func() {
			if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
				t.Fatalf("%s %v: %v: %s", name, args, err, out)
			}
		}
	}
	expectUpdate := resource.ConfigPlanChecks{
		PreApply: []plancheck.PlanCheck{
			plancheck.ExpectResourceAction(testGroupResource, plancheck.ResourceActionUpdate),
		},
	}
	inSync := checkGroup(name, gid, "nobody", "root")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkGroupDestroyed(name),
		Steps: []resource.TestStep{
			{Config: config, Check: inSync},
			{
				PreConfig:          mutate("groupmod", "-g", strconv.FormatInt(otherGID, 10), name),
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{Config: config, ConfigPlanChecks: expectUpdate, Check: inSync},
			{
				PreConfig:          mutate("gpasswd", "-M", "daemon", name),
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{Config: config, ConfigPlanChecks: expectUpdate, Check: inSync},
			{
				// Deleted out of band: recreated.
				PreConfig: mutate("groupdel", name),
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testGroupResource, plancheck.ResourceActionCreate),
					},
				},
				Check: inSync,
			},
		},
	})
}

// TestAccGroup_unmanagedMembers checks that members added outside Terraform
// are ignored while "members" is unset.
func TestAccGroup_unmanagedMembers(t *testing.T) {
	requireRoot(t)

	name := uniqueUsername("tfunm")
	config := fmt.Sprintf(`
resource "sysutils_group" "test" {
  name = %q
}`, name)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkGroupDestroyed(name),
		Steps: []resource.TestStep{
			{Config: config},
			{
				PreConfig: func() {
					if out, err := exec.Command("gpasswd", "-M", "nobody", name).CombinedOutput(); err != nil {
						t.Fatalf("gpasswd: %v: %s", err, out)
					}
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.TestCheckNoResourceAttr(testGroupResource, "members"),
			},
		},
	})
}

func TestAccGroup_importUnmanaged(t *testing.T) {
	requireRoot(t)

	name := uniqueUsername("tfimp")
	gid := freeGID(t, 63000)
	if out, err := exec.Command("groupadd", "-g", strconv.FormatInt(gid, 10), name).CombinedOutput(); err != nil {
		t.Fatalf("groupadd: %v: %s", err, out)
	}
	if out, err := exec.Command("gpasswd", "-M", "root,nobody", name).CombinedOutput(); err != nil {
		t.Fatalf("gpasswd: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("groupdel", name).Run() })

	config := fmt.Sprintf(`
resource "sysutils_group" "test" {
  name    = %q
  gid     = %d
  members = ["nobody", "root"]
  system  = true
}`, name, gid)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkGroupDestroyed(name),
		Steps: []resource.TestStep{
			{
				Config:             config,
				ResourceName:       testGroupResource,
				ImportState:        true,
				ImportStateId:      name,
				ImportStatePersist: true,
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if len(states) != 1 {
						return fmt.Errorf("expected 1 imported state, got %d", len(states))
					}
					want := map[string]string{
						"id":        name,
						"name":      name,
						"gid":       strconv.FormatInt(gid, 10),
						"members.#": "2",
					}
					for k, v := range want {
						if got := states[0].Attributes[k]; got != v {
							return fmt.Errorf("imported %s = %q, want %q", k, got, v)
						}
					}
					return nil
				},
			},
			{
				// system is not known after import; adopting it from the
				// configuration must not replace the group.
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testGroupResource, plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testGroupResource, "system", "true"),
					checkGroup(name, gid, "nobody", "root"),
				),
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

func TestAccGroup_importErrors(t *testing.T) {
	requireRoot(t)

	config := `
resource "sysutils_group" "test" {
  name = "placeholder"
}`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:        config,
				ResourceName:  testGroupResource,
				ImportState:   true,
				ImportStateId: uniqueUsername("tfnone"),
				ExpectError:   regexp.MustCompile(`Cannot import non-existent remote object`),
			},
			{
				Config:        config,
				ResourceName:  testGroupResource,
				ImportState:   true,
				ImportStateId: "-bad",
				ExpectError:   regexp.MustCompile(`Invalid import ID`),
				SkipFunc:      skipDashImportID,
			},
		},
	})
}

func TestAccGroup_invalidConfig(t *testing.T) {
	requireRoot(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "sysutils_group" "test" {
  name = "-r"
}`,
				ExpectError: regexp.MustCompile(`Invalid name`),
			},
			{
				Config: `
resource "sysutils_group" "test" {
  name    = "tfvalid"
  members = ["root", "a,b"]
}`,
				ExpectError: regexp.MustCompile(`Invalid member`),
			},
			{
				// gpasswd rejects unknown users; the error surfaces verbatim.
				Config: fmt.Sprintf(`
resource "sysutils_group" "test" {
  name    = %q
  members = ["tf_no_such_user_x"]
}`, uniqueUsername("tfbad")),
				ExpectError: regexp.MustCompile(`does\s+not\s+exist`),
			},
		},
	})
}

// TestAccGroup_userPrimaryGroup wires a user's primary gid to a managed group,
// the intended way to combine the two resources.
func TestAccGroup_userPrimaryGroup(t *testing.T) {
	requireRoot(t)

	group := uniqueUsername("tfpg")
	userName := uniqueUsername("tfpu")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkGroupDestroyed(group),
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "sysutils_group" "test" {
  name   = %q
  system = true
}

resource "sysutils_user" "u" {
  name        = %q
  gid         = sysutils_group.test.gid
  system      = true
  create_home = false
}`, group, userName),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair("sysutils_user.u", "gid", testGroupResource, "gid"),
				),
			},
		},
	})
}

// freeGID returns the first gid at or above start that is not in use.
func freeGID(t *testing.T, start int64) int64 {
	t.Helper()
	for gid := start; gid < start+500; gid++ {
		if err := exec.Command("getent", "group", strconv.FormatInt(gid, 10)).Run(); err != nil {
			return gid
		}
	}
	t.Fatalf("no free gid in [%d, %d)", start, start+500)
	return 0
}

func checkGroup(name string, wantGID int64, wantMembers ...string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		e, err := lookupGroupEntry(name)
		if err != nil {
			return err
		}
		if e == nil {
			return fmt.Errorf("group %s not found", name)
		}
		if e.GID != wantGID {
			return fmt.Errorf("gid of %s = %d, want %d", name, e.GID, wantGID)
		}
		got := append([]string{}, e.Members...)
		sort.Strings(got)
		want := append([]string{}, wantMembers...)
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) {
			return fmt.Errorf("members of %s = %v, want %v", name, got, want)
		}
		return nil
	}
}

func checkGroupGIDBelow(name string, limit int64) resource.TestCheckFunc {
	return func(*terraform.State) error {
		e, err := lookupGroupEntry(name)
		if err != nil || e == nil {
			return fmt.Errorf("group %s not found: %v", name, err)
		}
		if e.GID >= limit {
			return fmt.Errorf("gid of system group %s = %d, want < %d", name, e.GID, limit)
		}
		return nil
	}
}

func checkGroupGIDAtLeast(name string, limit int64) resource.TestCheckFunc {
	return func(*terraform.State) error {
		e, err := lookupGroupEntry(name)
		if err != nil || e == nil {
			return fmt.Errorf("group %s not found: %v", name, err)
		}
		if e.GID < limit {
			return fmt.Errorf("gid of regular group %s = %d, want >= %d", name, e.GID, limit)
		}
		return nil
	}
}

func checkGroupDestroyed(name string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		e, err := lookupGroupEntry(name)
		if err != nil {
			return err
		}
		if e != nil {
			return fmt.Errorf("group %s still exists", name)
		}
		return nil
	}
}
