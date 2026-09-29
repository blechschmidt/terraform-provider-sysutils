package provider

import (
	"fmt"
	"os/exec"
	"os/user"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// getentPasswdFields returns the passwd fields of name as reported by the
// system, independently of the provider's own parsing.
func getentPasswdFields(t *testing.T, name string) []string {
	t.Helper()
	out, err := exec.Command("getent", "passwd", name).Output()
	if err != nil {
		t.Fatalf("getent passwd %s: %v", name, err)
	}
	return strings.Split(strings.TrimRight(string(out), "\n"), ":")
}

func TestAccUserDataSource_root(t *testing.T) {
	requireRoot(t)
	root := getentPasswdFields(t, "root")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "sysutils_user" "by_name" {
  name = "root"
}

data "sysutils_user" "by_uid" {
  uid = 0
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data.sysutils_user.by_name", "id", "root"),
					resource.TestCheckResourceAttr("data.sysutils_user.by_name", "uid", "0"),
					resource.TestCheckResourceAttr("data.sysutils_user.by_name", "gid", "0"),
					resource.TestCheckResourceAttr("data.sysutils_user.by_name", "home", root[5]),
					resource.TestCheckResourceAttr("data.sysutils_user.by_name", "shell", root[6]),
					resource.TestCheckResourceAttr("data.sysutils_user.by_name", "comment", root[4]),
					resource.TestCheckResourceAttrSet("data.sysutils_user.by_name", "groups.#"),

					resource.TestCheckResourceAttr("data.sysutils_user.by_uid", "id", "root"),
					resource.TestCheckResourceAttr("data.sysutils_user.by_uid", "name", "root"),
					resource.TestCheckResourceAttr("data.sysutils_user.by_uid", "uid", "0"),
					resource.TestCheckResourceAttrPair("data.sysutils_user.by_uid", "home", "data.sysutils_user.by_name", "home"),
					resource.TestCheckResourceAttrPair("data.sysutils_user.by_uid", "shell", "data.sysutils_user.by_name", "shell"),
					resource.TestCheckResourceAttrPair("data.sysutils_user.by_uid", "comment", "data.sysutils_user.by_name", "comment"),
					resource.TestCheckResourceAttrPair("data.sysutils_user.by_uid", "groups.#", "data.sysutils_user.by_name", "groups.#"),
				),
			},
		},
	})
}

// TestAccUserDataSource_managedUser reads back a user created in the same
// configuration, covering the full GECOS field and supplementary groups.
func TestAccUserDataSource_managedUser(t *testing.T) {
	requireRoot(t)

	name := uniqueUsername("tfdsu")
	group := uniqueUsername("tfdsg")
	primary := uniqueUsername("tfdsp")

	config := fmt.Sprintf(`
resource "sysutils_group" "supplementary" {
  name = %q
}

resource "sysutils_group" "primary" {
  name = %q
}

resource "sysutils_user" "test" {
  name    = %q
  gid     = sysutils_group.primary.gid
  home    = "/nonexistent/%s"
  shell   = "/bin/sh"
  comment = "Data Source Test,Room 42,,"
  groups  = [sysutils_group.supplementary.name]
}

data "sysutils_user" "by_name" {
  name       = sysutils_user.test.name
  depends_on = [sysutils_user.test]
}

data "sysutils_user" "by_uid" {
  uid = sysutils_user.test.uid
}`, group, primary, name, name)

	const byName, byUID = "data.sysutils_user.by_name", "data.sysutils_user.by_uid"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeTestCheckFunc(
			checkGroupDestroyed(group),
			checkGroupDestroyed(primary),
		),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(byName, "id", name),
					resource.TestCheckResourceAttrPair(byName, "uid", "sysutils_user.test", "uid"),
					resource.TestCheckResourceAttrPair(byName, "gid", "sysutils_group.primary", "gid"),
					resource.TestCheckResourceAttr(byName, "home", "/nonexistent/"+name),
					resource.TestCheckResourceAttr(byName, "shell", "/bin/sh"),
					resource.TestCheckResourceAttr(byName, "comment", "Data Source Test,Room 42,,"),
					resource.TestCheckResourceAttr(byName, "groups.#", "1"),
					resource.TestCheckTypeSetElemAttr(byName, "groups.*", group),

					resource.TestCheckResourceAttr(byUID, "name", name),
					resource.TestCheckResourceAttr(byUID, "id", name),
					resource.TestCheckResourceAttrPair(byUID, "uid", "sysutils_user.test", "uid"),
					resource.TestCheckResourceAttr(byUID, "comment", "Data Source Test,Room 42,,"),
					resource.TestCheckResourceAttr(byUID, "groups.#", "1"),
					resource.TestCheckTypeSetElemAttr(byUID, "groups.*", group),
				),
			},
			{
				// Both data sources are stable once the user exists.
				Config:   config,
				PlanOnly: true,
			},
		},
	})
}

func TestAccUserDataSource_notFound(t *testing.T) {
	uid := unusedID(t, func(id string) error { _, err := user.LookupId(id); return err })

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "sysutils_user" "test" {
  name = "tf_no_such_user"
}`,
				ExpectError: regexp.MustCompile(`User not found[\s\S]*No user named "tf_no_such_user"\s+exists`),
			},
			{
				Config: fmt.Sprintf(`
data "sysutils_user" "test" {
  uid = %d
}`, uid),
				ExpectError: regexp.MustCompile(`User not found[\s\S]*No user with uid ` + strconv.Itoa(uid) + `\s+exists`),
			},
		},
	})
}

func TestAccUserDataSource_invalidKeys(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "sysutils_user" "test" {
  name = "root"
  uid  = 0
}`,
				ExpectError: regexp.MustCompile(`Invalid Attribute Combination[\s\S]*2 attributes specified when one \(and\s+only\s+one\) of`),
			},
			{
				Config:      `data "sysutils_user" "test" {}`,
				ExpectError: regexp.MustCompile(`Invalid Attribute Combination[\s\S]*No attribute specified when one \(and\s+only\s+one\) of`),
			},
			{
				Config: `
data "sysutils_user" "test" {
  name = "-root"
}`,
				ExpectError: regexp.MustCompile(`must not start\s+with\s+` + "`-`"),
			},
			{
				Config: `
data "sysutils_user" "test" {
  uid = 4294967295
}`,
				ExpectError: regexp.MustCompile(`Invalid Attribute Value`),
			},
		},
	})
}
