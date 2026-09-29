package provider

import (
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

func TestFindGroupEntryByGID(t *testing.T) {
	const db = `# comment
root:x:0:
broken:x:notanumber:
adm:x:4:syslog,alice
+nisgroup
admdup:x:4:
`
	tests := []struct {
		gid  int64
		want *groupEntry
	}{
		{0, &groupEntry{Name: "root", GID: 0, Members: []string{}}},
		// The first of several entries sharing a gid wins, and an entry
		// with a malformed gid is skipped rather than reported.
		{4, &groupEntry{Name: "adm", GID: 4, Members: []string{"syslog", "alice"}}},
		{1234, nil},
	}
	for _, tt := range tests {
		t.Run(strconv.FormatInt(tt.gid, 10), func(t *testing.T) {
			got, err := findGroupEntryByGID(strings.NewReader(db), tt.gid)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}

	if _, err := findGroupEntryByGID(strings.NewReader("root:x:0\n"), 0); err == nil {
		t.Error("expected error for a line with too few fields")
	}
}

func TestAccGroupDataSource_root(t *testing.T) {
	requireRoot(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "sysutils_group" "by_name" {
  name = "root"
}

data "sysutils_group" "by_gid" {
  gid = 0
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data.sysutils_group.by_name", "id", "root"),
					resource.TestCheckResourceAttr("data.sysutils_group.by_name", "gid", "0"),
					resource.TestCheckResourceAttrSet("data.sysutils_group.by_name", "members.#"),
					resource.TestCheckResourceAttr("data.sysutils_group.by_gid", "id", "root"),
					resource.TestCheckResourceAttr("data.sysutils_group.by_gid", "name", "root"),
					resource.TestCheckResourceAttrPair("data.sysutils_group.by_gid", "members.#", "data.sysutils_group.by_name", "members.#"),
				),
			},
		},
	})
}

// TestAccGroupDataSource_managedGroup reads back a group created in the same
// configuration, by name and by gid, including a membership change.
func TestAccGroupDataSource_managedGroup(t *testing.T) {
	requireRoot(t)

	name := uniqueUsername("tfdsg")
	gid := freeGID(t, 62000)

	config := func(members string) string {
		return fmt.Sprintf(`
resource "sysutils_group" "test" {
  name    = %q
  gid     = %d
  members = %s
}

data "sysutils_group" "by_name" {
  name       = sysutils_group.test.name
  depends_on = [sysutils_group.test]
}

data "sysutils_group" "by_gid" {
  gid        = sysutils_group.test.gid
  depends_on = [sysutils_group.test]
}`, name, gid, members)
	}

	const byName, byGID = "data.sysutils_group.by_name", "data.sysutils_group.by_gid"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkGroupDestroyed(name),
		Steps: []resource.TestStep{
			{
				Config: config(`["root", "daemon"]`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(byName, "id", name),
					resource.TestCheckResourceAttr(byName, "gid", strconv.FormatInt(gid, 10)),
					resource.TestCheckResourceAttr(byName, "members.#", "2"),
					resource.TestCheckTypeSetElemAttr(byName, "members.*", "root"),
					resource.TestCheckTypeSetElemAttr(byName, "members.*", "daemon"),

					resource.TestCheckResourceAttr(byGID, "id", name),
					resource.TestCheckResourceAttr(byGID, "name", name),
					resource.TestCheckResourceAttr(byGID, "members.#", "2"),
					resource.TestCheckTypeSetElemAttr(byGID, "members.*", "root"),
					resource.TestCheckTypeSetElemAttr(byGID, "members.*", "daemon"),
				),
			},
			{
				Config: config(`[]`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(byName, "members.#", "0"),
					resource.TestCheckResourceAttr(byGID, "members.#", "0"),
				),
			},
		},
	})
}

func TestAccGroupDataSource_notFound(t *testing.T) {
	gid := freeGID(t, 62500)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "sysutils_group" "test" {
  name = "tf_no_such_group"
}`,
				ExpectError: regexp.MustCompile(`Group not found[\s\S]*No group named "tf_no_such_group"\s+exists\s+in\s+/etc/group`),
			},
			{
				Config: fmt.Sprintf(`
data "sysutils_group" "test" {
  gid = %d
}`, gid),
				ExpectError: regexp.MustCompile(`Group not found[\s\S]*No group with gid ` + strconv.FormatInt(gid, 10) + `\s+exists`),
			},
		},
	})
}

func TestAccGroupDataSource_invalidKeys(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "sysutils_group" "test" {
  name = "root"
  gid  = 0
}`,
				ExpectError: regexp.MustCompile(`Invalid Attribute Combination[\s\S]*2 attributes specified when one \(and\s+only\s+one\) of`),
			},
			{
				Config:      `data "sysutils_group" "test" {}`,
				ExpectError: regexp.MustCompile(`Invalid Attribute Combination[\s\S]*No attribute specified when one \(and\s+only\s+one\) of`),
			},
			{
				Config: `
data "sysutils_group" "test" {
  gid = -1
}`,
				ExpectError: regexp.MustCompile(`Invalid Attribute Value`),
			},
		},
	})
}
