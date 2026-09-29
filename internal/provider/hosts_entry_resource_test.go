package provider

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

const testHostsResource = "sysutils_hosts_entry.test"

// testHostsFile is a hosts file as distributions ship it, with tabs,
// spaces, comments and blank lines that edits must keep.
const testHostsFile = "# Static table lookup for hostnames.\n" +
	"127.0.0.1\tlocalhost\n" +
	"127.0.1.1\tbuild.example.com build\n" +
	"\n" +
	"# The following lines are desirable for IPv6 capable hosts\n" +
	"::1     localhost ip6-localhost ip6-loopback\n" +
	"ff02::1 ip6-allnodes\n"

// hostsEntryConfig renders a sysutils_hosts_entry resource named "test".
func hostsEntryConfig(p, attrs string) string {
	return fmt.Sprintf(`
resource "sysutils_hosts_entry" "test" {
  path = %q
%s
}`, p, attrs)
}

func TestAccHostsEntry_lifecycle(t *testing.T) {
	requireRoot(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "hosts")
	mustWrite(t, p, testHostsFile)
	// Mode and ownership that edits must preserve.
	mustChmod(t, p, 0o640)
	if err := os.Chown(p, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	config := func(ip, names, comment string) string {
		attrs := fmt.Sprintf("  ip        = %q\n  hostnames = %s", ip, names)
		if comment != "" {
			attrs += fmt.Sprintf("\n  comment   = %q", comment)
		}
		return hostsEntryConfig(p, attrs)
	}
	with := func(line string) string { return testHostsFile + line + "\n" }

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		// Destroy removes only the managed line.
		CheckDestroy: checkFileLineContent(p, testHostsFile),
		Steps: []resource.TestStep{
			{
				Config: config("10.0.0.5", `["db.internal", "db"]`, "database"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(p, with("10.0.0.5\tdb.internal db # database")),
					checkFileMode(p, 0o640),
					checkLOwnership(p, "65534", "65534"),
					checkNoTempFiles(dir),
					resource.TestCheckResourceAttr(testHostsResource, "id", p+":10.0.0.5"),
					resource.TestCheckResourceAttr(testHostsResource, "allow_duplicate", "false"),
				),
			},
			{
				Config: config("10.0.0.5", `["db.internal", "db"]`, "database"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				// Reformatting the line outside Terraform is not drift.
				PreConfig: setFile(t, p, with("10.0.0.5    DB.internal   db   #database")),
				Config:    config("10.0.0.5", `["DB.internal", "db"]`, "database"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				// An alias edited outside Terraform: an in-place update
				// that keeps the line's spacing.
				PreConfig: setFile(t, p, with("10.0.0.5    db.internal   postgres   #database")),
				Config:    config("10.0.0.5", `["db.internal", "db"]`, "database"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testHostsResource, plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue(testHostsResource, tfjsonpath.New("hostnames"),
							knownvalue.ListExact([]knownvalue.Check{knownvalue.StringExact("db.internal"), knownvalue.StringExact("db")})),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(p, with("10.0.0.5    db.internal   db   #database")),
					checkFileMode(p, 0o640),
					checkLOwnership(p, "65534", "65534"),
				),
			},
			{
				// Comment edited outside Terraform.
				PreConfig: setFile(t, p, with("10.0.0.5\tdb.internal db # something else")),
				Config:    config("10.0.0.5", `["db.internal", "db"]`, "database"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testHostsResource, plancheck.ResourceActionUpdate),
						expectBefore(testHostsResource, "comment", "something else"),
					},
				},
				Check: checkFileContent(p, with("10.0.0.5\tdb.internal db # database")),
			},
			{
				// Line removed outside Terraform: planned for creation.
				PreConfig: setFile(t, p, testHostsFile),
				Config:    config("10.0.0.5", `["db.internal", "db"]`, "database"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testHostsResource, plancheck.ResourceActionCreate)},
				},
				Check: checkFileContent(p, with("10.0.0.5\tdb.internal db # database")),
			},
			{
				// Update: new alias, no comment.
				Config: config("10.0.0.5", `["db.internal", "db", "postgres"]`, ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testHostsResource, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(p, with("10.0.0.5\tdb.internal db postgres")),
					resource.TestCheckNoResourceAttr(testHostsResource, "comment"),
				),
			},
			{
				// Changing the address and the canonical name rewrites the
				// line in place, below a line added meanwhile.
				PreConfig: setFile(t, p, with("10.0.0.5\tdb.internal db postgres")+"10.0.0.7 cache\n"),
				Config:    config("10.0.0.6", `["pg.internal", "db"]`, ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testHostsResource, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(p, with("10.0.0.6\tpg.internal db")+"10.0.0.7 cache\n"),
					resource.TestCheckResourceAttr(testHostsResource, "id", p+":10.0.0.6"),
				),
			},
			{
				ResourceName:      testHostsResource,
				ImportState:       true,
				ImportStateId:     p + ":10.0.0.6",
				ImportStateVerify: true,
			},
			{
				// Leave the unrelated line in place for CheckDestroy.
				PreConfig: setFile(t, p, with("10.0.0.6\tpg.internal db")),
				Config:    config("10.0.0.6", `["pg.internal", "db"]`, ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}

func TestAccHostsEntry_ipv6AndImport(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hosts")
	// An existing entry, spelled out in full and with a comment.
	mustWrite(t, p, testHostsFile+"fd00:0:0:0:0:0:0:5\tdb6.internal db6 # v6 database\n")
	config := hostsEntryConfig(p, `  ip        = "fd00::5"
  hostnames = ["db6.internal", "db6"]
  comment   = "v6 database"`)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkFileLineContent(p, testHostsFile),
		Steps: []resource.TestStep{
			{
				// Import by address; the address is compared as such.
				Config:             config,
				ResourceName:       testHostsResource,
				ImportState:        true,
				ImportStateId:      p + ":fd00::5",
				ImportStatePersist: true,
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if len(states) != 1 {
						return fmt.Errorf("imported %d resources, want 1", len(states))
					}
					a := states[0].Attributes
					for k, want := range map[string]string{
						"ip": "fd00::5", "hostnames.#": "2", "hostnames.0": "db6.internal", "hostnames.1": "db6",
						"comment": "v6 database", "path": p, "allow_duplicate": "false", "id": p + ":fd00::5",
					} {
						if a[k] != want {
							return fmt.Errorf("imported %s = %q, want %q", k, a[k], want)
						}
					}
					return nil
				},
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

func TestAccHostsEntry_importErrors(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hosts")
	mustWrite(t, p, testHostsFile)
	config := hostsEntryConfig(p, `  ip        = "10.0.0.5"
  hostnames = ["db"]`)
	step := func(id, expect string) resource.TestStep {
		return resource.TestStep{
			Config:        config,
			ResourceName:  testHostsResource,
			ImportState:   true,
			ImportStateId: id,
			ExpectError:   regexp.MustCompile(expect),
		}
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			step("10.0.0.5", `must\s+have\s+the\s+form`),
			step("relative/hosts:10.0.0.5", `Invalid\s+path`),
			step(p+":10.0.0", `not\s+an\s+IPv4\s+or\s+IPv6\s+address`),
			step(p+":10.0.0.5", `(?i)cannot\s+import\s+non-existent`),
		},
	})
}

func TestAccHostsEntry_duplicateHostname(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hosts")
	const orig = testHostsFile + "10.0.0.9\told-db.internal DB\n"
	mustWrite(t, p, orig)
	config := func(allow bool) string {
		return hostsEntryConfig(p, fmt.Sprintf(`  ip              = "10.0.0.5"
  hostnames       = ["db.internal", "db"]
  allow_duplicate = %t`, allow))
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkFileLineContent(p, orig),
		Steps: []resource.TestStep{
			{
				Config:      config(false),
				ExpectError: regexp.MustCompile(`(?s)"DB"\s+is\s+mapped\s+to\s+10\.0\.0\.9\s+on\s+line\s+8.*allow_duplicate`),
			},
			{
				// The file is unchanged after the failed apply.
				PreConfig: func() {
					if err := checkFileLineContent(p, orig)(nil); err != nil {
						t.Fatal(err)
					}
				},
				Config: config(true),
				Check:  checkFileContent(p, orig+"10.0.0.5\tdb.internal db\n"),
			},
		},
	})
}

// TestAccHostsEntry_dualStackAndSharedAddress checks that a name with an
// IPv4 and an IPv6 address is not a duplicate, and that an entry sharing
// its address with an unmanaged line leaves that line alone.
func TestAccHostsEntry_dualStackAndSharedAddress(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hosts")
	mustWrite(t, p, testHostsFile)
	config := `
resource "sysutils_hosts_entry" "v4" {
  path      = "` + p + `"
  ip        = "127.0.0.1"
  hostnames = ["app.local"]
}

resource "sysutils_hosts_entry" "v6" {
  path      = "` + p + `"
  ip        = "::1"
  hostnames = ["app.local"]
  comment   = "dual stack"
}
`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkFileLineContent(p, testHostsFile),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: func(s *terraform.State) error {
					data, err := os.ReadFile(p)
					if err != nil {
						return err
					}
					// The two resources are applied in either order.
					got := string(data)
					for _, want := range []string{testHostsFile, "\n127.0.0.1\tapp.local\n", "\n::1\tapp.local # dual stack\n"} {
						if !strings.Contains(got, want) {
							return fmt.Errorf("hosts file %q does not contain %q", got, want)
						}
					}
					return nil
				},
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

func TestAccHostsEntry_createsFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hosts")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		// Destroy never deletes the file.
		CheckDestroy: checkFileLineContent(p, ""),
		Steps: []resource.TestStep{
			{
				Config: hostsEntryConfig(p, `  ip        = "10.0.0.5"
  hostnames = ["db"]`),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(p, "10.0.0.5\tdb\n"),
					checkFileMode(p, 0o644),
				),
			},
			{
				// The file removed outside Terraform: the entry is gone.
				PreConfig: func() {
					if err := os.Remove(p); err != nil {
						t.Fatal(err)
					}
				},
				Config: hostsEntryConfig(p, `  ip        = "10.0.0.5"
  hostnames = ["db"]`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testHostsResource, plancheck.ResourceActionCreate)},
				},
				Check: checkFileContent(p, "10.0.0.5\tdb\n"),
			},
		},
	})
}

func TestAccHostsEntry_invalidConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hosts")
	step := func(attrs, expect string) resource.TestStep {
		return resource.TestStep{
			Config:      hostsEntryConfig(p, attrs),
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(expect),
		}
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			step(`  ip = "10.0.0.256"
  hostnames = ["db"]`, `not\s+an\s+IPv4\s+or\s+IPv6\s+address`),
			step(`  ip = "fe80::1%eth0"
  hostnames = ["db"]`, `must\s+not\s+have\s+a\s+zone`),
			step(`  ip = "10.0.0.5"
  hostnames = []`, `(?i)at\s+least\s+1`),
			step(`  ip = "10.0.0.5"
  hostnames = ["db_1"]`, `contains\s+'_'`),
			step(`  ip = "10.0.0.5"
  hostnames = ["db.internal."]`, `empty\s+label`),
			step(`  ip = "10.0.0.5"
  hostnames = ["db", "web", "DB"]`, `"DB"\s+is\s+listed\s+more\s+than\s+once`),
			step(`  ip = "10.0.0.5"
  hostnames = ["db"]
  comment = "a\nb"`, `single\s+line`),
			step(`  ip = "10.0.0.5"
  hostnames = ["db"]
  comment = ""`, `must\s+not\s+be\s+empty`),
		},
	})
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: `
resource "sysutils_hosts_entry" "test" {
  path      = "etc/hosts"
  ip        = "10.0.0.5"
  hostnames = ["db"]
}`,
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(`(?i)absolute`),
		}},
	})
}

// TestAccHostsEntry_symlinkRefused checks that a symlink at path is never
// written through.
func TestAccHostsEntry_symlinkRefused(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	mustWrite(t, target, testHostsFile)
	link := filepath.Join(dir, "hosts")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: hostsEntryConfig(link, `  ip        = "10.0.0.5"
  hostnames = ["db"]`),
			ExpectError: regexp.MustCompile(`(?i)symlink`),
		}},
	})
	if err := checkFileContent(target, testHostsFile)(nil); err != nil {
		t.Error(err)
	}
}

func TestAccHostsEntry_rootDir(t *testing.T) {
	root := testRootDir(t)
	mustMkdir(t, filepath.Join(root, "etc"))
	hostPath := filepath.Join(root, "etc", "hosts")
	mustWrite(t, hostPath, testHostsFile)
	config := rootedProvider(root) + `
resource "sysutils_hosts_entry" "test" {
  ip        = "10.0.0.5"
  hostnames = ["db.internal", "db"]
}
`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkFileLineContent(hostPath, testHostsFile),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(hostPath, testHostsFile+"10.0.0.5\tdb.internal db\n"),
					resource.TestCheckResourceAttr(testHostsResource, "path", "/etc/hosts"),
					resource.TestCheckResourceAttr(testHostsResource, "id", "/etc/hosts:10.0.0.5"),
				),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(testHostsResource, tfjsonpath.New("path"), knownvalue.StringExact("/etc/hosts")),
				},
			},
			{
				// Drift inside the root is detected.
				PreConfig: setFile(t, hostPath, testHostsFile+"10.0.0.5\tdb.internal\n"),
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testHostsResource, plancheck.ResourceActionUpdate)},
				},
				Check: checkFileContent(hostPath, testHostsFile+"10.0.0.5\tdb.internal db\n"),
			},
			{
				ResourceName:      testHostsResource,
				ImportState:       true,
				ImportStateId:     "/etc/hosts:10.0.0.5",
				ImportStateVerify: true,
			},
		},
	})
}

// TestAccHostsEntry_importByName imports one of several lines that map the
// same address by adding its canonical name to the import ID.
func TestAccHostsEntry_importByName(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hosts")
	mustWrite(t, p, testHostsFile+"127.0.0.1\tapp.local app # app\n")
	config := hostsEntryConfig(p, `  ip        = "127.0.0.1"
  hostnames = ["app.local", "app"]
  comment   = "app"`)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		// localhost, which shares the address, is kept.
		CheckDestroy: checkFileLineContent(p, testHostsFile),
		Steps: []resource.TestStep{
			{
				Config:             config,
				ResourceName:       testHostsResource,
				ImportState:        true,
				ImportStateId:      p + ":127.0.0.1,APP.local",
				ImportStatePersist: true,
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					a := states[0].Attributes
					if a["hostnames.#"] != "2" || a["hostnames.0"] != "app.local" || a["hostnames.1"] != "app" || a["comment"] != "app" {
						return fmt.Errorf("imported %v, want the app.local line", a)
					}
					return nil
				},
			},
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				Config:        config,
				ResourceName:  testHostsResource,
				ImportState:   true,
				ImportStateId: p + ":127.0.0.1,db_1",
				ExpectError:   regexp.MustCompile(`contains\s+'_'`),
			},
		},
	})
}
