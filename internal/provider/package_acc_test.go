package provider

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// accPackageName is the package the acceptance tests install and remove:
// small, without dependencies worth mentioning, nothing depends on it, and
// it is in the main repositories of Debian, Ubuntu, Fedora, RHEL-like
// distributions (EPEL not needed) and Alpine. SYSUTILS_ACC_PACKAGE
// overrides it.
func accPackageName() string {
	if name := os.Getenv("SYSUTILS_ACC_PACKAGE"); name != "" {
		return name
	}
	return "tree"
}

// requirePackageManager skips the test unless it may install and remove
// packages with a real package manager: TF_ACC is set, the process is
// root, a supported package manager is installed, and the test package is
// not installed already, so that the test never removes something the host
// needs. It returns the manager, and removes the package when the test
// ends in case a failed test left it installed.
func requirePackageManager(t *testing.T, name string) packageManager {
	t.Helper()
	if os.Getenv("TF_ACC") == "" {
		t.Skip("acceptance test; set TF_ACC=1 to run")
	}
	requireRoot(t)
	mgr, err := (&packageConfig{}).resolve(packageManagerAuto)
	if err != nil {
		t.Skipf("no supported package manager: %v", err)
	}
	ctx := context.Background()
	info, err := mgr.Query(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if info.Installed {
		t.Skipf("package %s %s is already installed; not touching it", name, info.Version)
	}
	t.Cleanup(func() {
		if info, err := mgr.Query(ctx, name); err == nil && info.Installed {
			if err := mgr.Remove(ctx, name); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
	})
	return mgr
}

// checkRealPackage checks the package database: want is the expected
// version, "" for not installed, or "*" for any version. installed_version
// in the state must agree.
func checkRealPackage(mgr packageManager, name, want string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		info, err := mgr.Query(context.Background(), name)
		if err != nil {
			return err
		}
		switch {
		case want == "" && info.Installed:
			return fmt.Errorf("%s %s is installed, want it absent", name, info.Version)
		case want != "" && !info.Installed:
			return fmt.Errorf("%s is not installed", name)
		case want != "" && want != "*" && !samePackageVersion(info.Version, want):
			return fmt.Errorf("%s %s is installed, want %s", name, info.Version, want)
		}
		if rs, ok := s.RootModule().Resources[testPackageResource]; ok {
			if got := rs.Primary.Attributes["installed_version"]; info.Installed && !samePackageVersion(got, info.Version) {
				return fmt.Errorf("installed_version = %q, but %s is installed", got, info.Version)
			}
		}
		return nil
	}
}

// availablePackageVersions lists the versions of name in the package
// index, as the package database would report them once installed. The
// index must be up to date.
func availablePackageVersions(t *testing.T, kind, name string) []string {
	t.Helper()
	var argv []string
	switch kind {
	case packageManagerApt:
		argv = []string{"apt-cache", "madison", name}
	case packageManagerDnf, packageManagerYum:
		argv = []string{kind, "repoquery", "-q", "--qf", "%{epoch}:%{version}-%{release}\n", name}
	case packageManagerApk:
		argv = []string{"apk", "policy", name}
	}
	out, err := exec.Command(argv[0], argv[1:]...).Output()
	if err != nil {
		t.Fatalf("%s: %v", strings.Join(argv, " "), err)
	}
	seen := map[string]bool{}
	var versions []string
	for _, line := range strings.Split(string(out), "\n") {
		var v string
		switch kind {
		case packageManagerApt:
			// "  tree | 2.1.1-2ubuntu3 | http://... Packages"
			if f := strings.Split(line, "|"); len(f) == 3 && strings.TrimSpace(f[0]) == name && strings.Contains(f[2], "Packages") {
				v = strings.TrimSpace(f[1])
			}
		case packageManagerDnf, packageManagerYum:
			v = normalizePackageVersion(strings.TrimSpace(line))
		case packageManagerApk:
			// "  2.2.1-r0:"
			if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") && strings.HasSuffix(line, ":") {
				v = strings.TrimSuffix(strings.TrimSpace(line), ":")
			}
		}
		if v != "" && !seen[v] {
			seen[v] = true
			versions = append(versions, v)
		}
	}
	if len(versions) == 0 {
		t.Fatalf("no versions of %s found in the package index:\n%s", name, out)
	}
	return versions
}

func TestAccPackage_lifecycle(t *testing.T) {
	name := accPackageName()
	mgr := requirePackageManager(t, name)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkRealPackage(mgr, name, ""),
		Steps: []resource.TestStep{
			// Fresh containers ship without a package index.
			{
				Config: packageHCL(name, `update_cache = true`),
				Check:  checkRealPackage(mgr, name, "*"),
			},
			{
				ResourceName:            testPackageResource,
				ImportState:             true,
				ImportStateId:           name,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"update_cache"},
			},
			{
				Config: packageHCL(name, `state = "absent"`),
				Check:  checkRealPackage(mgr, name, ""),
			},
			// Installed outside Terraform although it should be absent.
			{
				PreConfig: func() {
					if err := mgr.Install(context.Background(), name, ""); err != nil {
						t.Fatal(err)
					}
				},
				Config: packageHCL(name, `state = "absent"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(testPackageResource, plancheck.ResourceActionUpdate),
				}},
				Check: checkRealPackage(mgr, name, ""),
			},
			{
				Config: packageHCL(name, `state = "latest"`),
				Check:  checkRealPackage(mgr, name, "*"),
			},
			// Removed outside Terraform: reinstalled.
			{
				PreConfig: func() {
					if err := mgr.Remove(context.Background(), name); err != nil {
						t.Fatal(err)
					}
				},
				Config: packageHCL(name, `state = "latest"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(testPackageResource, plancheck.ResourceActionUpdate),
				}},
				Check: checkRealPackage(mgr, name, "*"),
			},
		},
	})
}

// TestAccPackage_version pins the oldest available version, which is a
// downgrade if the index has several, and then upgrades to the newest.
func TestAccPackage_version(t *testing.T) {
	name := accPackageName()
	mgr := requirePackageManager(t, name)
	if err := mgr.UpdateCache(context.Background()); err != nil {
		t.Fatal(err)
	}
	versions := availablePackageVersions(t, mgr.Kind(), name)
	oldest := versions[len(versions)-1]
	if mgr.Kind() != packageManagerApt {
		// apt lists the newest version first, dnf and apk the oldest.
		oldest = versions[0]
	}
	t.Logf("%s versions available with %s: %v", name, mgr.Kind(), versions)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkRealPackage(mgr, name, ""),
		Steps: []resource.TestStep{
			{
				Config: packageHCL(name, `state = "latest"`),
				Check:  checkRealPackage(mgr, name, "*"),
			},
			{
				Config: packageHCL(name, fmt.Sprintf(`version = %q`, oldest)),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(testPackageResource, plancheck.ResourceActionUpdate),
				}},
				Check: checkRealPackage(mgr, name, oldest),
			},
			{
				Config: packageHCL(name, `state = "latest"`),
				Check:  checkRealPackage(mgr, name, "*"),
			},
		},
	})
}

func TestAccPackage_errors(t *testing.T) {
	const missing = "sysutils-acc-no-such-package"
	requirePackageManager(t, missing)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      packageHCL(missing, ""),
				ExpectError: regexp.MustCompile(`Error:\s+Installing\s+package`),
			},
			{
				Config:        packageHCL(missing, ""),
				ResourceName:  testPackageResource,
				ImportState:   true,
				ImportStateId: missing,
				ExpectError:   regexp.MustCompile(`Package\s+sysutils-acc-no-such-package\s+is\s+not\s+installed`),
			},
		},
	})
}
