package provider

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const testHostnameResource = "sysutils_hostname.test"

func hostnameResourceConfig(root, attrs string) string {
	p := ""
	if root != "" {
		p = rootedProvider(root)
	}
	return p + fmt.Sprintf(`
resource "sysutils_hostname" "test" {
%s
}
`, attrs)
}

const hostnameTestHosts = "127.0.0.1\tlocalhost\n127.0.1.1\tbefore\n\n# IPv6\n::1\tlocalhost ip6-localhost ip6-loopback\n"

func expectHostnameUpdate() resource.ConfigPlanChecks {
	return resource.ConfigPlanChecks{
		PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testHostnameResource, plancheck.ResourceActionUpdate)},
	}
}

func TestAccHostname_rootDir(t *testing.T) {
	root := testRootDir(t)
	etc := filepath.Join(root, "etc")
	hostnameFile, hosts, machineInfo := filepath.Join(etc, "hostname"), filepath.Join(etc, "hosts"), filepath.Join(etc, "machine-info")
	mustWrite(t, hostnameFile, "before\n")
	mustWrite(t, hosts, hostnameTestHosts)
	mustWrite(t, machineInfo, "ICON_NAME=computer-vm\n")
	config := func(name string) string {
		return hostnameResourceConfig(root, fmt.Sprintf(`  hostname           = %q
  pretty_hostname    = "Web server \"1\""
  manage_hosts_entry = true
  restore_on_destroy = true`, name))
	}
	hostsWith := func(line string) string {
		return strings.Replace(hostnameTestHosts, "127.0.1.1\tbefore", line, 1)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		// Destroy puts back exactly what was there before.
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkFileContent(hostnameFile, "before\n"),
			checkFileContent(hosts, hostnameTestHosts),
			checkFileContent(machineInfo, "ICON_NAME=computer-vm\n"),
		),
		Steps: []resource.TestStep{
			{
				Config: config("web1.example.com"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(hostnameFile, "web1.example.com\n"),
					checkFileContent(hosts, hostsWith("127.0.1.1\tweb1.example.com web1")),
					checkFileContent(machineInfo, "ICON_NAME=computer-vm\nPRETTY_HOSTNAME=\"Web server \\\"1\\\"\"\n"),
					checkNoTempFiles(etc),
					resource.TestCheckResourceAttr(testHostnameResource, "id", "system"),
					resource.TestCheckResourceAttr(testHostnameResource, "hostname", "web1.example.com"),
					resource.TestCheckResourceAttr(testHostnameResource, "pretty_hostname", `Web server "1"`),
					resource.TestCheckResourceAttr(testHostnameResource, "manage_hosts_entry", "true"),
					// The kernel hostname is not managed below root_dir.
					resource.TestCheckNoResourceAttr(testHostnameResource, "transient_hostname"),
				),
			},
			{
				Config:           config("web1.example.com"),
				ConfigPlanChecks: expectEmptyPlan,
			},
			{
				// /etc/hostname changed outside Terraform.
				PreConfig:        func() { mustWrite(t, hostnameFile, "# set by hand\nintruder\n") },
				Config:           config("web1.example.com"),
				ConfigPlanChecks: expectHostnameUpdate(),
				Check:            checkFileContent(hostnameFile, "web1.example.com\n"),
			},
			{
				// The /etc/hosts line was removed.
				PreConfig:        func() { mustWrite(t, hosts, hostsWith("")) },
				Config:           config("web1.example.com"),
				ConfigPlanChecks: expectHostnameUpdate(),
				Check:            checkFileContent(hosts, hostsWith("")+"127.0.1.1\tweb1.example.com web1\n"),
			},
			{
				// The /etc/hosts line lost its alias, and the pretty
				// hostname changed.
				PreConfig: func() {
					mustWrite(t, hosts, hostsWith("127.0.1.1 web1.example.com"))
					mustWrite(t, machineInfo, "ICON_NAME=computer-vm\nPRETTY_HOSTNAME=other\n")
				},
				Config:           config("web1.example.com"),
				ConfigPlanChecks: expectHostnameUpdate(),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(hosts, hostsWith("127.0.1.1 web1.example.com web1")),
					checkFileContent(machineInfo, "ICON_NAME=computer-vm\nPRETTY_HOSTNAME=\"Web server \\\"1\\\"\"\n"),
				),
			},
			{
				// A new hostname rewrites the line in place.
				Config: config("web2"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(hostnameFile, "web2\n"),
					checkFileContent(hosts, hostsWith("127.0.1.1 web2")),
					resource.TestCheckResourceAttr(testHostnameResource, "hostname", "web2"),
				),
			},
			{
				Config:           config("web2"),
				ConfigPlanChecks: expectEmptyPlan,
			},
			{
				ResourceName:      testHostnameResource,
				ImportState:       true,
				ImportStateId:     "system",
				ImportStateVerify: true,
				// Not read back on import: whether to manage the pretty
				// hostname and the hosts line, and the destroy behaviour.
				ImportStateVerifyIgnore: []string{"pretty_hostname", "manage_hosts_entry", "restore_on_destroy"},
			},
		},
	})
}

func TestAccHostname_rootDirDefaults(t *testing.T) {
	root := testRootDir(t)
	etc := filepath.Join(root, "etc")
	mustWrite(t, filepath.Join(etc, "hosts"), hostnameTestHosts)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		// By default destroy leaves the hostname as it is.
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkFileContent(filepath.Join(etc, "hostname"), "node7\n"),
			checkFileContent(filepath.Join(etc, "hosts"), hostnameTestHosts),
			checkNotExist(filepath.Join(etc, "machine-info")),
		),
		Steps: []resource.TestStep{
			{
				Config: hostnameResourceConfig(root, `  hostname = "node7"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(filepath.Join(etc, "hostname"), "node7\n"),
					// Neither the hosts file nor machine-info are touched.
					checkFileContent(filepath.Join(etc, "hosts"), hostnameTestHosts),
					checkNotExist(filepath.Join(etc, "machine-info")),
					resource.TestCheckResourceAttr(testHostnameResource, "manage_hosts_entry", "false"),
					resource.TestCheckResourceAttr(testHostnameResource, "restore_on_destroy", "false"),
					resource.TestCheckNoResourceAttr(testHostnameResource, "pretty_hostname"),
				),
			},
			{
				// /etc/hostname was removed.
				PreConfig: func() {
					if err := os.Remove(filepath.Join(etc, "hostname")); err != nil {
						t.Fatal(err)
					}
				},
				Config:           hostnameResourceConfig(root, `  hostname = "node7"`),
				ConfigPlanChecks: expectHostnameUpdate(),
				Check:            checkFileContent(filepath.Join(etc, "hostname"), "node7\n"),
			},
		},
	})
}

func TestAccHostname_invalid(t *testing.T) {
	root := testRootDir(t)
	for _, tc := range []struct{ attrs, err string }{
		{`hostname = "-web"`, `starts\s+or\s+ends\s+with\s+a\s+hyphen`},
		{`hostname = "web_1"`, `only\s+letters,\s+digits\s+and\s+hyphens`},
		{fmt.Sprintf(`hostname = %q`, strings.Repeat("a", 64)), `longer\s+than\s+63`},
		{`hostname = "web1."`, `empty\s+label`},
		{"hostname = \"web1\"\n  pretty_hostname = \"two\\nlines\"", `control\s+characters`},
		{"hostname = \"web1\"\n  pretty_hostname = \"\"", `must\s+not\s+be\s+empty`},
	} {
		resource.Test(t, resource.TestCase{
			ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
			Steps: []resource.TestStep{{
				Config:      hostnameResourceConfig(root, "  "+tc.attrs),
				ExpectError: regexp.MustCompile(tc.err),
			}},
		})
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config:        hostnameResourceConfig(root, `  hostname = "web1"`),
			ResourceName:  testHostnameResource,
			ImportState:   true,
			ImportStateId: "web1",
			ExpectError:   regexp.MustCompile(`import\s+ID\s+of\s+sysutils_hostname\s+must\s+be\s+"system"`),
		}},
	})
}

// fakeHostnameProviders serves the provider with a fake kernel hostname
// and hostnamectl that treat the files below root as the running host's.
func fakeHostnameProviders(t *testing.T, f *fakeHostnamectl) map[string]func() (tfprotov6.ProviderServer, error) {
	cfg := f.config(t)
	return map[string]func() (tfprotov6.ProviderServer, error){
		"sysutils": providerserver.NewProtocol6WithError(&sysutilsProvider{version: "test", hostname: cfg}),
	}
}

func TestAccHostname_fakeHost(t *testing.T) {
	root := testRootDir(t)
	etc := filepath.Join(root, "etc")
	mustWrite(t, filepath.Join(etc, "hostname"), "before\n")
	f := &fakeHostnamectl{t: t, root: root, kernel: "before"}
	config := hostnameResourceConfig(root, `  hostname           = "web1"
  restore_on_destroy = true`)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: fakeHostnameProviders(t, f),
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkFileContent(filepath.Join(etc, "hostname"), "before\n"),
			func(*terraform.State) error {
				if f.kernel != "before" {
					return fmt.Errorf("kernel hostname %q after destroy, want before", f.kernel)
				}
				return nil
			},
		),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(filepath.Join(etc, "hostname"), "web1\n"),
					resource.TestCheckResourceAttr(testHostnameResource, "transient_hostname", "web1"),
					func(*terraform.State) error {
						if len(f.calls) != 1 || f.calls[0][2] != "--static" {
							return fmt.Errorf("hostnamectl calls %q, want one --static", f.calls)
						}
						return nil
					},
				),
			},
			{
				Config:           config,
				ConfigPlanChecks: expectEmptyPlan,
			},
			{
				// The kernel hostname changed, for example by DHCP; the
				// static hostname is still right.
				PreConfig:        func() { f.kernel = "dhcp-lease-name" },
				Config:           config,
				ConfigPlanChecks: expectHostnameUpdate(),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testHostnameResource, "transient_hostname", "web1"),
					func(*terraform.State) error {
						if f.kernel != "web1" {
							return fmt.Errorf("kernel hostname %q, want web1", f.kernel)
						}
						return nil
					},
				),
			},
			{
				// The kernel refuses names longer than 64 characters; the
				// plan fails before anything is changed.
				Config:      hostnameResourceConfig(root, fmt.Sprintf(`  hostname = "%s.example.com"`, strings.Repeat("a", 60))),
				ExpectError: regexp.MustCompile(`the\s+kernel\s+allows\s+at\s+most\s+64`),
			},
		},
	})
}
