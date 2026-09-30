package provider

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

func TestListPasswdEntries(t *testing.T) {
	const db = `# comment
root:x:0:0:root:/root:/bin/bash

  # indented comment
alice:x:1000:1000:Alice Example,Room 42,,:/home/alice:/bin/zsh
+@netgroup::::::
-bob::::::
nologin:x:65534:65534::/nonexistent:
`
	got, err := listPasswdEntries(strings.NewReader(db), "/etc/passwd")
	if err != nil {
		t.Fatal(err)
	}
	want := []passwdLine{
		{name: "root", password: "x", uid: 0, gid: 0, gecos: "root", home: "/root", shell: "/bin/bash"},
		{name: "alice", password: "x", uid: 1000, gid: 1000, gecos: "Alice Example,Room 42,,", home: "/home/alice", shell: "/bin/zsh"},
		{name: "nologin", password: "x", uid: 65534, gid: 65534, gecos: "", home: "/nonexistent", shell: ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	if got, err := listPasswdEntries(strings.NewReader(""), "/etc/passwd"); err != nil || got == nil || len(got) != 0 {
		t.Errorf("empty database: got %#v, %v; want an empty, non-nil list", got, err)
	}

	for _, bad := range []string{
		"root:x:0:0:root:/root\n",
		"root:x:zero:0:root:/root:/bin/sh\n",
		":x:0:0:root:/root:/bin/sh\n",
	} {
		_, err := listPasswdEntries(strings.NewReader("ok:x:1:1::/:/bin/sh\n"+bad), "/etc/passwd")
		if err == nil || !strings.HasPrefix(err.Error(), "/etc/passwd:2: ") {
			t.Errorf("%q: got error %v, want one for /etc/passwd:2", bad, err)
		}
	}
}

func TestListGroupEntries(t *testing.T) {
	const db = `# comment
root:x:0:
+nisgroup
adm:x:4:syslog, alice
admdup:x:4:
`
	got, err := listGroupEntries(strings.NewReader(db), "/etc/group")
	if err != nil {
		t.Fatal(err)
	}
	want := []groupEntry{
		{Name: "root", GID: 0, Members: []string{}},
		{Name: "adm", GID: 4, Members: []string{"syslog", "alice"}},
		{Name: "admdup", GID: 4, Members: []string{}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	// Unlike the lookups, the list reports an entry with a malformed gid
	// instead of silently leaving it out.
	for _, bad := range []string{"broken:x:notanumber:\n", "short:x:1\n"} {
		_, err := listGroupEntries(strings.NewReader("root:x:0:\n"+bad), "/etc/group")
		if err == nil || !strings.HasPrefix(err.Error(), "/etc/group:2: ") {
			t.Errorf("%q: got error %v, want one for /etc/group:2", bad, err)
		}
	}
}

// accountListTree is an image root filesystem with its own account
// databases, which differ from any host's.
var accountListTree = map[string]string{
	"etc/passwd": `root:x:0:0:root:/root:/bin/ash
daemon:x:1:1:daemon:/usr/sbin:/sbin/nologin
app:x:900:900:App service:/srv/app:/sbin/nologin
alice:x:1000:1000:Alice Example,Room 42,,:/home/alice:/bin/bash
bob:x:1001:100::/home/bob:/bin/bash
+@netgroup::::::
alice2:x:1000:1000:Alice's twin:/home/alice:
`,
	"etc/group": `root:x:0:
daemon:x:1:
users:x:100:alice,bob
wheel:x:10:alice
app:x:900:
alice:x:1000:
dup:x:1001:carol,carol
`,
}

func rootDirProvider(dir string) string {
	return fmt.Sprintf("provider \"sysutils\" {\n  root_dir = %q\n}\n", dir)
}

func expectUsers(attr string, v knownvalue.Check) statecheck.StateCheck {
	return statecheck.ExpectKnownValue("data.sysutils_users.test", tfjsonpath.New(attr), v)
}

func expectGroups(attr string, v knownvalue.Check) statecheck.StateCheck {
	return statecheck.ExpectKnownValue("data.sysutils_groups.test", tfjsonpath.New(attr), v)
}

func namesExact(names ...string) knownvalue.Check {
	checks := make([]knownvalue.Check, 0, len(names))
	for _, n := range names {
		checks = append(checks, knownvalue.StringExact(n))
	}
	return knownvalue.ListExact(checks)
}

func TestAccUsersDataSource_rootDir(t *testing.T) {
	dir := t.TempDir()
	writeTestTree(t, dir, accountListTree)
	provider := rootDirProvider(dir)
	users := func(body string) string {
		return provider + "data \"sysutils_users\" \"test\" {\n" + body + "\n}\n"
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: users(""),
				ConfigStateChecks: []statecheck.StateCheck{
					expectUsers("id", knownvalue.StringExact("/etc/passwd")),
					// File order; NIS compat lines are skipped and
					// entries sharing a uid are all listed.
					expectUsers("names", namesExact("root", "daemon", "app", "alice", "bob", "alice2")),
					statecheck.ExpectKnownValue("data.sysutils_users.test", tfjsonpath.New("users").AtSliceIndex(3), knownvalue.ObjectExact(map[string]knownvalue.Check{
						"name":    knownvalue.StringExact("alice"),
						"uid":     knownvalue.Int64Exact(1000),
						"gid":     knownvalue.Int64Exact(1000),
						"comment": knownvalue.StringExact("Alice Example,Room 42,,"),
						"home":    knownvalue.StringExact("/home/alice"),
						"shell":   knownvalue.StringExact("/bin/bash"),
					})),
					statecheck.ExpectKnownValue("data.sysutils_users.test", tfjsonpath.New("users").AtSliceIndex(5).AtMapKey("shell"), knownvalue.StringExact("")),
				},
			},
			{
				Config: users("uid_min = 1000\nuid_max = 1000"),
				ConfigStateChecks: []statecheck.StateCheck{
					expectUsers("names", namesExact("alice", "alice2")),
				},
			},
			{
				Config: users("uid_min = 1\nshell = \"/sbin/nologin\""),
				ConfigStateChecks: []statecheck.StateCheck{
					expectUsers("names", namesExact("daemon", "app")),
				},
			},
			{
				Config: users("shell = \"\""),
				ConfigStateChecks: []statecheck.StateCheck{
					expectUsers("names", namesExact("alice2")),
				},
			},
			{
				Config: users("name_regex = \"^a\"\ngid = 1000"),
				ConfigStateChecks: []statecheck.StateCheck{
					expectUsers("names", namesExact("alice", "alice2")),
				},
			},
			{
				// Unanchored expressions match anywhere in the name.
				Config: users("name_regex = \"o\""),
				ConfigStateChecks: []statecheck.StateCheck{
					expectUsers("names", namesExact("root", "daemon", "bob")),
				},
			},
			{
				Config: users("uid_min = 5000"),
				ConfigStateChecks: []statecheck.StateCheck{
					expectUsers("users", knownvalue.ListExact([]knownvalue.Check{})),
					expectUsers("names", knownvalue.ListExact([]knownvalue.Check{})),
				},
			},
		},
	})
}

func TestAccGroupsDataSource_rootDir(t *testing.T) {
	dir := t.TempDir()
	writeTestTree(t, dir, accountListTree)
	provider := rootDirProvider(dir)
	groups := func(body string) string {
		return provider + "data \"sysutils_groups\" \"test\" {\n" + body + "\n}\n"
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: groups(""),
				ConfigStateChecks: []statecheck.StateCheck{
					expectGroups("id", knownvalue.StringExact("/etc/group")),
					expectGroups("names", namesExact("root", "daemon", "users", "wheel", "app", "alice", "dup")),
					// A member listed twice is listed once.
					statecheck.ExpectKnownValue("data.sysutils_groups.test", tfjsonpath.New("groups").AtSliceIndex(6).AtMapKey("members"), knownvalue.SetExact([]knownvalue.Check{knownvalue.StringExact("carol")})),
					statecheck.ExpectKnownValue("data.sysutils_groups.test", tfjsonpath.New("groups").AtSliceIndex(2), knownvalue.ObjectExact(map[string]knownvalue.Check{
						"name": knownvalue.StringExact("users"),
						"gid":  knownvalue.Int64Exact(100),
						"members": knownvalue.SetExact([]knownvalue.Check{
							knownvalue.StringExact("alice"), knownvalue.StringExact("bob"),
						}),
					})),
					statecheck.ExpectKnownValue("data.sysutils_groups.test", tfjsonpath.New("groups").AtSliceIndex(0).AtMapKey("members"), knownvalue.SetExact([]knownvalue.Check{})),
				},
			},
			{
				Config: groups("gid_min = 10\ngid_max = 900"),
				ConfigStateChecks: []statecheck.StateCheck{
					expectGroups("names", namesExact("users", "wheel", "app")),
				},
			},
			{
				Config: groups("member = \"alice\""),
				ConfigStateChecks: []statecheck.StateCheck{
					expectGroups("names", namesExact("users", "wheel")),
				},
			},
			{
				Config: groups("name_regex = \"^(root|app)$\""),
				ConfigStateChecks: []statecheck.StateCheck{
					expectGroups("names", namesExact("root", "app")),
				},
			},
		},
	})
}

// TestAccAccountListDataSources_rootDirSymlinks checks that the databases
// are read the way a chrooted process would read them: an absolute symlink
// is resolved inside root_dir, and a relative one that climbs out of it is
// refused rather than followed to the host's file.
func TestAccAccountListDataSources_rootDirSymlinks(t *testing.T) {
	inside := t.TempDir()
	writeTestTree(t, inside, map[string]string{
		"usr/share/base/passwd": "imageuser:x:4242:4242::/:/bin/sh\n",
		"usr/share/base/group":  "imagegroup:x:4242:\n",
		"etc/passwd":            "->/usr/share/base/passwd",
		"etc/group":             "->/usr/share/base/group",
	})
	escape := t.TempDir()
	writeTestTree(t, escape, map[string]string{
		"etc/passwd": "->../../../../../../../../etc/passwd",
		"etc/group":  "->../../../../../../../../etc/group",
	})
	const both = `
data "sysutils_users" "test" {}
data "sysutils_groups" "test" {}
`
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: rootDirProvider(inside) + both,
				ConfigStateChecks: []statecheck.StateCheck{
					expectUsers("names", namesExact("imageuser")),
					expectGroups("names", namesExact("imagegroup")),
				},
			},
			{
				Config:      rootDirProvider(escape) + `data "sysutils_users" "test" {}`,
				ExpectError: regexp.MustCompile(`Reading\s+passwd\s+database(.|\n)*escapes\s+root_dir`),
			},
			{
				Config:      rootDirProvider(escape) + `data "sysutils_groups" "test" {}`,
				ExpectError: regexp.MustCompile(`Reading\s+group\s+database(.|\n)*escapes\s+root_dir`),
			},
		},
	})
}

func TestAccAccountListDataSources_errors(t *testing.T) {
	missing := t.TempDir()
	malformed := t.TempDir()
	writeTestTree(t, malformed, map[string]string{
		"etc/passwd": "root:x:0:0:root:/root:/bin/sh\nbroken:x:0:0\n",
		"etc/group":  "root:x:0:\nbroken:x:NaN:\n",
	})
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      `data "sysutils_users" "test" { name_regex = "a(" }`,
				ExpectError: regexp.MustCompile(`Invalid\s+regular\s+expression(.|\n)*missing\s+closing\s+\)`),
			},
			{
				Config:      `data "sysutils_groups" "test" { name_regex = "[z-a]" }`,
				ExpectError: regexp.MustCompile(`Invalid\s+regular\s+expression(.|\n)*invalid\s+character\s+class\s+range`),
			},
			{
				Config:      "data \"sysutils_users\" \"test\" {\n  uid_min = 10\n  uid_max = 9\n}",
				ExpectError: regexp.MustCompile(`Invalid\s+uid\s+range(.|\n)*uid_max\s+\(9\)\s+must\s+not\s+be\s+less\s+than\s+uid_min\s+\(10\)`),
			},
			{
				Config:      "data \"sysutils_groups\" \"test\" {\n  gid_min = 10\n  gid_max = 9\n}",
				ExpectError: regexp.MustCompile(`Invalid\s+gid\s+range`),
			},
			{
				Config:      `data "sysutils_users" "test" { uid_min = -1 }`,
				ExpectError: regexp.MustCompile(`uid_min\s+value\s+must\s+be\s+between`),
			},
			{
				Config:      `data "sysutils_groups" "test" { member = "-rf" }`,
				ExpectError: regexp.MustCompile(`must\s+not\s+start\s+with\s+` + "`-`"),
			},
			{
				Config:      rootDirProvider(missing) + `data "sysutils_users" "test" {}`,
				ExpectError: regexp.MustCompile(`Reading\s+passwd\s+database(.|\n)*no\s+such\s+file`),
			},
			{
				Config:      rootDirProvider(missing) + `data "sysutils_groups" "test" {}`,
				ExpectError: regexp.MustCompile(`Reading\s+group\s+database(.|\n)*no\s+such\s+file`),
			},
			{
				Config:      rootDirProvider(malformed) + `data "sysutils_users" "test" {}`,
				ExpectError: regexp.MustCompile(`Reading\s+passwd\s+database(.|\n)*/etc/passwd:2:\s+a\s+passwd\s+line\s+has\s+7`),
			},
			{
				Config:      rootDirProvider(malformed) + `data "sysutils_groups" "test" {}`,
				ExpectError: regexp.MustCompile(`Reading\s+group\s+database(.|\n)*/etc/group:2:\s+invalid\s+gid\s+"NaN"`),
			},
		},
	})
}

// TestAccAccountListDataSources_host reads the host's own databases and
// compares them with the single-entry data sources.
func TestAccAccountListDataSources_host(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "sysutils_users" "test" {
  uid_max    = 0
  name_regex = "^root$"
}

data "sysutils_groups" "test" {
  gid_max    = 0
  name_regex = "^root$"
}

data "sysutils_users" "all" {}

data "sysutils_user" "root" {
  uid = 0
}

data "sysutils_group" "root" {
  gid = 0
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data.sysutils_users.test", "users.#", "1"),
					resource.TestCheckResourceAttrPair("data.sysutils_users.test", "users.0.name", "data.sysutils_user.root", "name"),
					resource.TestCheckResourceAttrPair("data.sysutils_users.test", "users.0.home", "data.sysutils_user.root", "home"),
					resource.TestCheckResourceAttrPair("data.sysutils_users.test", "users.0.shell", "data.sysutils_user.root", "shell"),
					resource.TestCheckResourceAttrPair("data.sysutils_users.test", "users.0.comment", "data.sysutils_user.root", "comment"),
					resource.TestCheckResourceAttr("data.sysutils_groups.test", "groups.#", "1"),
					resource.TestCheckResourceAttrPair("data.sysutils_groups.test", "groups.0.name", "data.sysutils_group.root", "name"),
					resource.TestCheckResourceAttrPair("data.sysutils_groups.test", "groups.0.members.#", "data.sysutils_group.root", "members.#"),
					resource.TestCheckResourceAttrWith("data.sysutils_users.all", "names.#", func(v string) error {
						data, err := os.ReadFile(filepath.Clean("/etc/passwd"))
						if err != nil {
							return err
						}
						entries, err := listPasswdEntries(strings.NewReader(string(data)), "/etc/passwd")
						if err != nil {
							return err
						}
						if want := fmt.Sprint(len(entries)); v != want {
							return fmt.Errorf("got %s users, want %s", v, want)
						}
						return nil
					}),
				),
			},
		},
	})
}
