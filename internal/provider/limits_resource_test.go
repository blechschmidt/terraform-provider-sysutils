package provider

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// limitsConfig renders a provider block with root_dir and the given
// resources.
func limitsConfig(root, resources string) string {
	return fmt.Sprintf(`
provider "sysutils" {
  root_dir = %q
}
%s`, root, resources)
}

// limitsHCL renders a sysutils_limits resource.
func limitsHCL(name, domain, typ, item, value, extra string) string {
	return fmt.Sprintf(`
resource "sysutils_limits" %q {
  domain = %q
  type   = %q
  item   = %q
  value  = %q
%s
}
`, name, domain, typ, item, value, extra)
}

// TestAccLimits_lifecycle manages two entries of one domain, which share
// the default file below root_dir, through create, drift, an equivalent
// spelling, import and destroy.
func TestAccLimits_lifecycle(t *testing.T) {
	root := t.TempDir()
	const managed = "/etc/security/limits.d/90-terraform-user-alice.conf"
	p := filepath.Join(root, managed)
	soft := limitsEntry{"alice", "soft", "nofile", "4096"}
	hard := limitsEntry{"alice", "hard", "nofile", "unlimited"}
	config := func(softValue, hardValue string) string {
		return limitsConfig(root,
			limitsHCL("soft", "alice", "soft", "nofile", softValue, "")+
				limitsHCL("hard", "alice", "hard", "nofile", hardValue, ""))
	}
	want := limitsFileHeader + "\n" + soft.line() + "\n" + hard.line() + "\n"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		// The provider created the file, so it goes with the last entry.
		CheckDestroy: checkPathGone(p),
		Steps: []resource.TestStep{
			{
				Config: config("4096", "unlimited"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectKnownValue("sysutils_limits.soft", tfjsonpath.New("file"), knownvalue.StringExact("90-terraform-user-alice.conf")),
						plancheck.ExpectKnownValue("sysutils_limits.soft", tfjsonpath.New("path"), knownvalue.StringExact(managed)),
						plancheck.ExpectKnownValue("sysutils_limits.soft", tfjsonpath.New("id"), knownvalue.StringExact(managed+":alice:soft:nofile")),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					// Created in either order; both lines follow the header.
					checkLimitsFileLines(p, limitsFileHeader, soft.line(), hard.line()),
					checkFileMode(p, 0o644),
					checkNoTempFiles(filepath.Dir(p)),
					resource.TestCheckResourceAttr("sysutils_limits.soft", "file", "90-terraform-user-alice.conf"),
					resource.TestCheckResourceAttr("sysutils_limits.soft", "path", managed),
					resource.TestCheckResourceAttr("sysutils_limits.hard", "id", managed+":alice:hard:nofile"),
				),
			},
			{
				// Normalise the order for the exact checks below.
				PreConfig: setFile(t, p, want),
				Config:    config("4096", "unlimited"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				// Another spelling of the same values is not drift.
				PreConfig: setFile(t, p, limitsFileHeader+"\nalice soft nofile 04096\nalice hard nofile infinity\n"),
				Config:    config("4096", "unlimited"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				// A value changed outside Terraform is drift, repaired in
				// place, and the plan shows the value on disk. Comments and
				// other lines added meanwhile are kept.
				PreConfig: setFile(t, p, limitsFileHeader+"\n# tuned by hand\nalice soft nofile 999\nalice hard nofile infinity\nbob soft nproc 10\n"),
				Config:    config("4096", "unlimited"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("sysutils_limits.soft", plancheck.ResourceActionUpdate),
						plancheck.ExpectResourceAction("sysutils_limits.hard", plancheck.ResourceActionNoop),
						expectBefore("sysutils_limits.soft", "value", "999"),
					},
				},
				Check: checkFileContent(p, limitsFileHeader+"\n# tuned by hand\n"+soft.line()+"\nalice hard nofile infinity\nbob soft nproc 10\n"),
			},
			{
				// A changed value in the configuration.
				Config: config("8192", "unlimited"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("sysutils_limits.soft", plancheck.ResourceActionUpdate),
					},
				},
				Check: checkFileContent(p, limitsFileHeader+"\n# tuned by hand\n"+
					limitsEntry{"alice", "soft", "nofile", "8192"}.line()+"\nalice hard nofile infinity\nbob soft nproc 10\n"),
			},
			{
				// An entry removed outside Terraform is created again.
				PreConfig: setFile(t, p, limitsFileHeader+"\nalice hard nofile infinity\n"),
				Config:    config("8192", "unlimited"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("sysutils_limits.soft", plancheck.ResourceActionCreate),
					},
				},
				Check: checkFileContent(p, limitsFileHeader+"\nalice hard nofile infinity\n"+
					limitsEntry{"alice", "soft", "nofile", "8192"}.line()+"\n"),
			},
			{
				// The file removed outside Terraform: both are created again.
				PreConfig: func() { _ = os.Remove(p) },
				Config:    config("8192", "unlimited"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("sysutils_limits.soft", plancheck.ResourceActionCreate),
						plancheck.ExpectResourceAction("sysutils_limits.hard", plancheck.ResourceActionCreate),
					},
				},
				Check: checkLimitsFileLines(p, limitsFileHeader,
					limitsEntry{"alice", "soft", "nofile", "8192"}.line(), hard.line()),
			},
			{
				ResourceName:      "sysutils_limits.soft",
				ImportState:       true,
				ImportStateId:     managed + ":alice:soft:nofile",
				ImportStateVerify: true,
			},
			{
				// Removing one resource keeps the file for the other.
				Config: limitsConfig(root, limitsHCL("hard", "alice", "hard", "nofile", "unlimited", "")),
				Check:  checkFileContent(p, limitsFileHeader+"\n"+hard.line()+"\n"),
			},
		},
	})
}

// checkLimitsFileLines checks that p consists of first followed by the
// other lines in any order.
func checkLimitsFileLines(p, first string, others ...string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
		if len(lines) != len(others)+1 || lines[0] != first {
			return fmt.Errorf("content of %s = %q, want %q and %q", p, data, first, others)
		}
		for _, o := range others {
			found := false
			for _, l := range lines[1:] {
				found = found || l == o
			}
			if !found {
				return fmt.Errorf("content of %s = %q, want the line %q", p, data, o)
			}
		}
		return nil
	}
}

// TestAccLimits_existingFile adds entries to a file the provider did not
// create: every other line is kept, the file keeps its mode, and destroy
// leaves it as it was, even though no entry would be left otherwise.
func TestAccLimits_existingFile(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "etc", "security", "limits.d")
	p := filepath.Join(dir, "20-site.conf")
	const orig = "# Site limits\n*          soft    nproc     4096 # distro default\n\n@audio     -       rtprio    95\n"
	mustWrite(t, p, orig)
	mustChmod(t, p, 0o640)
	empty := filepath.Join(dir, "30-empty.conf")
	mustWrite(t, empty, "")

	config := limitsConfig(root,
		limitsHCL("core", "*", "hard", "core", "0", `  file = "20-site.conf"`)+
			limitsHCL("gid", "@1000:1999", "soft", "memlock", "65536", `  file = "20-site.conf"`)+
			limitsHCL("logins", "%admins", "-", "maxlogins", "4", `  file = "30-empty.conf"`)+
			limitsHCL("nice", "@audio", "-", "nice", "-10", `  file = "20-site.conf"`))
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkFileLineContent(p, orig),
			checkFileLineContent(empty, ""),
			checkFileMode(p, 0o640),
		),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					checkLimitsFileLines(p, "# Site limits",
						"*          soft    nproc     4096 # distro default", "", "@audio     -       rtprio    95",
						limitsEntry{"*", "hard", "core", "0"}.line(),
						limitsEntry{"@1000:1999", "soft", "memlock", "65536"}.line(),
						limitsEntry{"@audio", "-", "nice", "-10"}.line()),
					checkFileMode(p, 0o640),
					// Not created by the provider, so no header.
					checkFileContent(empty, limitsEntry{"%admins", "-", "maxlogins", "4"}.line()+"\n"),
					resource.TestCheckResourceAttr("sysutils_limits.gid", "id", "/etc/security/limits.d/20-site.conf:@1000:1999:soft:memlock"),
				),
			},
			{
				// Import IDs whose domain contains a colon.
				ResourceName:      "sysutils_limits.gid",
				ImportState:       true,
				ImportStateId:     "/etc/security/limits.d/20-site.conf:@1000:1999:soft:memlock",
				ImportStateVerify: true,
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

// TestAccLimits_takeoverRefused checks that create refuses an entry that is
// already in the file, which destroy would remove, and points to import.
func TestAccLimits_takeoverRefused(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "etc", "security", "limits.d", "90-terraform-user-alice.conf")
	mustMkdir(t, filepath.Dir(p))
	const orig = "alice soft nofile 1024\n"
	mustWrite(t, p, orig)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkFileLineContent(p, orig),
		Steps: []resource.TestStep{{
			Config:      limitsConfig(root, limitsHCL("test", "alice", "soft", "nofile", "2048", "")),
			ExpectError: regexp.MustCompile(`already\s+sets\s+alice\s+soft\s+nofile\s+to\s+1024(.|\n)*terraform\s+import`),
		}},
	})
}

// TestAccLimits_symlinks checks that the limits.d directory is resolved
// inside root_dir, and that a symlink at the file itself is refused.
func TestAccLimits_symlinks(t *testing.T) {
	root := t.TempDir()
	// An absolute link inside the tree points into the tree, not to the
	// host's /etc/security.
	mustMkdir(t, filepath.Join(root, "etc"))
	mustMkdir(t, filepath.Join(root, "usr", "etc", "security"))
	mustSymlink(t, "/usr/etc/security", filepath.Join(root, "etc", "security"))
	p := filepath.Join(root, "usr", "etc", "security", "limits.d", "90-terraform-default.conf")

	outside := filepath.Join(t.TempDir(), "outside.conf")
	mustWrite(t, outside, "")
	link := filepath.Join(root, "usr", "etc", "security", "limits.d", "50-link.conf")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: limitsConfig(root, limitsHCL("test", "*", "soft", "core", "0", "")),
				Check:  checkFileContent(p, limitsFileHeader+"\n"+limitsEntry{"*", "soft", "core", "0"}.line()+"\n"),
			},
			{
				PreConfig:   func() { mustSymlink(t, outside, link) },
				Config:      limitsConfig(root, limitsHCL("test", "*", "soft", "core", "0", "")+limitsHCL("link", "*", "hard", "core", "0", `  file = "50-link.conf"`)),
				ExpectError: regexp.MustCompile(`symbolic\s+link`),
			},
		},
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkPathGone(p),
			checkFileLineContent(outside, ""),
		),
	})
}

// TestAccLimits_validation checks the plan-time validation of every
// attribute.
func TestAccLimits_validation(t *testing.T) {
	root := t.TempDir()
	step := func(domain, typ, item, value, extra, want string) resource.TestStep {
		return resource.TestStep{
			Config:      limitsConfig(root, limitsHCL("test", domain, typ, item, value, extra)),
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(want),
		}
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			step("al ice", "soft", "nofile", "1", "", `contains\s+the\s+character`),
			step("1000", "soft", "nofile", "1", "", `Use\s+":1000"`),
			step("2000:1000", "soft", "nofile", "1", "", `greater\s+than\s+the\s+maximum`),
			step("alice", "both", "nofile", "1", "", `must\s+be\s+"soft",\s+"hard"\s+or\s+"-"`),
			step("alice", "soft", "files", "1", "", `is\s+not\s+one\s+of`),
			step("alice", "soft", "nofile", "lots", "", `non-negative\s+integer`),
			step("alice", "soft", "nofile", "1 2", "", `single\s+field`),
			step("alice", "soft", "nice", "unlimited", "", `from\s+-20\s+to\s+19`),
			step("alice", "soft", "nonewprivs", "2", "", `must\s+be\s+0\s+or\s+1`),
			step("%admins", "soft", "nofile", "1", "", `only\s+applies\s+to\s+the\s+maxlogins`),
			step("alice", "soft", "nofile", "1", `  file = "../../passwd.conf"`, `without\s+"/"`),
			step("alice", "soft", "nofile", "1", `  file = "limits"`, `must\s+end\s+in\s+"\.conf"`),
			step("alice", "soft", "nofile", "1", `  file = ".hidden.conf"`, `must\s+not\s+start`),
		},
	})
}

func TestAccLimits_importErrors(t *testing.T) {
	root := t.TempDir()
	config := limitsConfig(root, limitsHCL("test", "alice", "soft", "nofile", "1", ""))
	step := func(id, want string) resource.TestStep {
		return resource.TestStep{
			Config:        config,
			ResourceName:  "sysutils_limits.test",
			ImportState:   true,
			ImportStateId: id,
			ExpectError:   regexp.MustCompile(want),
		}
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			step("alice:soft:nofile", `Import\s+ID\s+must\s+have\s+the\s+form`),
			step("x.conf:alice:soft:nofile", `must\s+be\s+a\s+file\s+directly\s+in`),
			step("/etc/security/limits.conf:alice:soft:nofile", `must\s+be\s+a\s+file\s+directly\s+in`),
			step("/etc/security/limits.d/../../passwd:alice:soft:nofile", `must\s+be\s+a\s+file\s+directly\s+in`),
			step("/etc/security/limits.d/x.conf:alice:nofile", `Import\s+ID\s+must\s+have\s+the\s+form`),
			step("/etc/security/limits.d/x.conf:alice:soft:files", `is\s+not\s+one\s+of`),
			step("/etc/security/limits.d/x.conf:alice:soft:nofile", `has\s+no\s+entry\s+alice\s+soft\s+nofile`),
		},
	})
}

// TestAccLimits_system manages an entry in the host's real
// /etc/security/limits.d, for a user that does not exist, so it limits
// nobody.
func TestAccLimits_system(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("acceptance test; set TF_ACC=1 to run")
	}
	requireRoot(t)
	domain := "sysutils-acc-" + randomID()
	p := filepath.Join(defaultLimitsDir, defaultLimitsFileName(domain))
	t.Cleanup(func() { _ = os.Remove(p) })
	config := fmt.Sprintf(`
resource "sysutils_limits" "test" {
  domain = %q
  type   = "-"
  item   = "nofile"
  value  = "65536"
}`, domain)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkPathGone(p),
		Steps: []resource.TestStep{{
			Config: config,
			Check: resource.ComposeAggregateTestCheckFunc(
				checkFileContent(p, limitsFileHeader+"\n"+limitsEntry{domain, "-", "nofile", "65536"}.line()+"\n"),
				checkFileMode(p, 0o644),
				checkLOwnership(p, "0", "0"),
				resource.TestCheckResourceAttr("sysutils_limits.test", "path", p),
			),
		}},
	})
}

// TestAccLimits_pamApplies checks that pam_limits itself reads the entry:
// a session opened with su for the user "nobody" starts with the soft limit.
func TestAccLimits_pamApplies(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("acceptance test; set TF_ACC=1 to run")
	}
	requireRoot(t)
	if !pamSessionUsesLimits("su", 0) {
		t.Skip("the su PAM session does not use pam_limits on this host")
	}
	if _, err := exec.LookPath("su"); err != nil {
		t.Skip("su is not installed")
	}
	file := "91-sysutils-acc-" + randomID() + ".conf"
	p := filepath.Join(defaultLimitsDir, file)
	t.Cleanup(func() { _ = os.Remove(p) })
	softNofile := func(want string) resource.TestCheckFunc {
		return func(*terraform.State) error {
			out, err := exec.Command("su", "-s", "/bin/sh", "nobody", "-c", "ulimit -Sn").CombinedOutput()
			if err != nil {
				return fmt.Errorf("su nobody: %v: %s", err, out)
			}
			if got := strings.TrimSpace(string(out)); got != want {
				return fmt.Errorf("soft nofile limit of a new session of nobody = %s, want %s", got, want)
			}
			return nil
		}
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkPathGone(p),
		Steps: []resource.TestStep{{
			Config: fmt.Sprintf(`
resource "sysutils_limits" "test" {
  domain = "nobody"
  type   = "soft"
  item   = "nofile"
  value  = "777"
  file   = %q
}`, file),
			Check: softNofile("777"),
		}},
	})
}

// pamSessionUsesLimits reports whether the session stack of the PAM
// service in /etc/pam.d runs pam_limits, following "include", "substack"
// and Debian's "@include". Commented-out lines, as Debian ships for su, do
// not count.
func pamSessionUsesLimits(service string, depth int) bool {
	if depth > 8 || strings.Contains(service, "/") {
		return false
	}
	data, err := os.ReadFile(filepath.Join("/etc/pam.d", service))
	if err != nil {
		return false
	}
	for _, l := range strings.Split(string(data), "\n") {
		f := strings.Fields(l)
		switch {
		case len(f) == 2 && f[0] == "@include":
			if pamSessionUsesLimits(f[1], depth+1) {
				return true
			}
		case len(f) >= 3 && strings.TrimPrefix(f[0], "-") == "session":
			if f[1] == "include" || f[1] == "substack" {
				if pamSessionUsesLimits(f[2], depth+1) {
					return true
				}
			} else if strings.HasSuffix(f[2], "pam_limits.so") {
				return true
			}
		}
	}
	return false
}
