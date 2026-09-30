package provider

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// accInstalledPackage returns a package that every host with the package
// manager kind has installed: the package manager's own.
func accInstalledPackage(kind string) string {
	switch kind {
	case packageManagerApt:
		return "dpkg"
	case packageManagerApk:
		return "apk-tools"
	default:
		return "rpm"
	}
}

// realPackageFacts returns the version and architecture of the installed
// package name as the package manager's own tools print them, independently
// of the backends.
func realPackageFacts(t *testing.T, kind, name string) (version, arch string) {
	t.Helper()
	run := func(argv ...string) string {
		out, err := exec.Command(argv[0], argv[1:]...).Output()
		if err != nil {
			t.Fatalf("%s: %v", strings.Join(argv, " "), err)
		}
		return strings.TrimSpace(string(out))
	}
	switch kind {
	case packageManagerApt:
		version, arch, _ = strings.Cut(run("dpkg-query", "--show", "--showformat=${Version} ${Architecture}", name), " ")
	case packageManagerApk:
		version = strings.TrimPrefix(run("apk", "info", "--installed", "--verbose", name), name+"-")
		arch = run("apk", "--print-arch")
	default:
		version, arch, _ = strings.Cut(run("rpm", "--query", "--queryformat", "%|EPOCH?{%{EPOCH}:}:{}|%{VERSION}-%{RELEASE} %{ARCH}", name), " ")
	}
	return version, arch
}

// TestAccPackageDataSource_readOnly reads an installed and an unknown
// package from the real package database. It needs neither root nor
// network access, and changes nothing.
func TestAccPackageDataSource_readOnly(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("acceptance test; set TF_ACC=1 to run")
	}
	mgr, err := (&packageConfig{}).resolve(packageManagerAuto)
	if err != nil {
		t.Skipf("no supported package manager: %v", err)
	}
	kind := mgr.Kind()
	name := accInstalledPackage(kind)
	version, arch := realPackageFacts(t, kind, name)
	missing := "tfacc-sysutils-nosuch-" + randomID()

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: packageDataHCL(name, "") + fmt.Sprintf(`
data "sysutils_package" "missing" {
  name    = %q
  manager = %q
}
`, missing, kind),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(testPackageDataSource, "package_manager", kind),
				resource.TestCheckResourceAttr(testPackageDataSource, "installed", "true"),
				resource.TestCheckResourceAttr(testPackageDataSource, "version", version),
				resource.TestCheckResourceAttr(testPackageDataSource, "architecture", arch),
				resource.TestCheckResourceAttr("data.sysutils_package.missing", "package_manager", kind),
				resource.TestCheckResourceAttr("data.sysutils_package.missing", "installed", "false"),
				resource.TestCheckNoResourceAttr("data.sysutils_package.missing", "version"),
				resource.TestCheckNoResourceAttr("data.sysutils_package.missing", "architecture"),
				resource.TestCheckNoResourceAttr("data.sysutils_package.missing", "available_version"),
			),
		}},
	})
}

// TestAccPackageDataSource_availableVersion refreshes the package index
// with refresh_cache, and checks that available_version is the version
// sysutils_package then installs. It needs root and the repositories.
func TestAccPackageDataSource_availableVersion(t *testing.T) {
	name := accPackageName()
	mgr := requirePackageManager(t, name)
	var available string

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkRealPackage(mgr, name, ""),
		Steps: []resource.TestStep{
			{
				Config: packageDataHCL(name, "  refresh_cache = true"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testPackageDataSource, "installed", "false"),
					resource.TestCheckNoResourceAttr(testPackageDataSource, "version"),
					func(s *terraform.State) error {
						available = s.RootModule().Resources[testPackageDataSource].Primary.Attributes["available_version"]
						if available == "" {
							return fmt.Errorf("available_version of %s is null after refreshing the index", name)
						}
						return nil
					},
				),
			},
			{
				// The data source depends on the package, so it is read
				// after the install.
				Config: packageHCL(name, "") + `
data "sysutils_package" "test" {
  name = sysutils_package.test.name
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					checkRealPackage(mgr, name, "*"),
					func(s *terraform.State) error {
						attrs := s.RootModule().Resources[testPackageDataSource].Primary.Attributes
						installed := s.RootModule().Resources[testPackageResource].Primary.Attributes["installed_version"]
						if !samePackageVersion(installed, available) {
							return fmt.Errorf("sysutils_package installed %s, but available_version was %s", installed, available)
						}
						if attrs["installed"] != "true" || attrs["version"] != installed || attrs["architecture"] == "" {
							return fmt.Errorf("data source after install: installed=%s version=%s architecture=%s, want true, %s and an architecture",
								attrs["installed"], attrs["version"], attrs["architecture"], installed)
						}
						return nil
					},
				),
			},
		},
	})
}
