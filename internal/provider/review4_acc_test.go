package provider

// Acceptance regression tests for the root_dir findings of the security
// review of the mount, sysctl, kernel_module and cron_job resources.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// With root_dir set, the cron.d file is written inside the root, as for the
// file resources, and never to the host's /etc/cron.d, where it would be run
// by the host's cron as root. Symlinks in the tree are resolved inside it.
func TestAccRootDir_cronJob(t *testing.T) {
	requireRoot(t)
	root := testRootDir(t)
	name := "sysutils-rootdir-test-" + randomID()
	hostFile := filepath.Join(defaultCronDir, name)
	t.Cleanup(func() { _ = os.Remove(hostFile) })
	// "/etc" in the tree is an absolute link, which means <root>/sysroot/etc.
	mustMkdir(t, filepath.Join(root, "sysroot", "etc"))
	mustSymlink(t, "/sysroot/etc", filepath.Join(root, "etc"))
	inTree := filepath.Join(root, "sysroot", "etc", "cron.d", name)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeTestCheckFunc(
			checkNotExist(inTree),
			checkNotExist(hostFile),
		),
		Steps: []resource.TestStep{
			{
				Config: rootedProvider(root) + cronJobHCL(name, `
  schedule = "0 0 1 1 *"
  command  = "true"
`),
				Check: resource.ComposeTestCheckFunc(
					checkNotExist(hostFile),
					checkRootOwned(inTree),
					checkCronJobFile(inTree, cronFileHeader+"\n0 0 1 1 * root true\n"),
				),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(testCronJobResource, tfjsonpath.New("path"), knownvalue.StringExact(filepath.Join(defaultCronDir, name))),
				},
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

// A symlink in the tree that leads above root_dir stops the cron job
// instead of redirecting it.
func TestAccRootDir_cronJobEscape(t *testing.T) {
	requireRoot(t)
	root := testRootDir(t)
	outside := testRootDir(t)
	up := strings.Repeat("../", strings.Count(root, "/")+1)
	mustSymlink(t, up+strings.TrimPrefix(outside, "/"), filepath.Join(root, "etc"))
	name := "sysutils-rootdir-test-" + randomID()
	t.Cleanup(func() { _ = os.Remove(filepath.Join(defaultCronDir, name)) })

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: rootedProvider(root) + cronJobHCL(name, `
  schedule = "0 0 1 1 *"
  command  = "true"
`),
				ExpectError: regexp.MustCompile(`escapes\s+root_dir`),
			},
		},
	})
	if _, err := os.Lstat(filepath.Join(outside, "cron.d")); !os.IsNotExist(err) {
		t.Errorf("cron.d created outside root_dir: %v", err)
	}
}

// Resources that change the running host refuse to plan with root_dir set,
// rather than changing the host of a configuration meant for an image tree.
func TestAccRootDir_hostOnlyResources(t *testing.T) {
	requireRoot(t)
	root := testRootDir(t)
	provider := rootedProvider(root)
	configs := map[string]string{
		"sysctl": `
resource "sysutils_sysctl" "test" {
  name  = "net.ipv4.ip_forward"
  value = "1"
}`,
		"mount": fmt.Sprintf(`
resource "sysutils_mount" "test" {
  path   = %q
  device = "tmpfs"
  fstype = "tmpfs"
}`, filepath.Join(root, "mnt")),
		"kernel_module": `
resource "sysutils_kernel_module" "test" {
  name = "dummy"
}`,
	}
	for name, config := range configs {
		t.Run(name, func(t *testing.T) {
			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps: []resource.TestStep{{
					Config:      provider + config,
					PlanOnly:    true,
					ExpectError: regexp.MustCompile(`Not\s+supported\s+with\s+root_dir`),
				}},
			})
		})
	}
}

// force_destroy holds the path the configured path really resolves to in the
// tree to the list of protected directories: "/a/etc" with "/a" a symlink to
// "/" is the tree's "/etc".
func TestAccRootDir_forceDestroyProtectedViaSymlink(t *testing.T) {
	requireRoot(t)
	root := testRootDir(t)
	mustSymlink(t, "/", filepath.Join(root, "a"))
	etc := filepath.Join(root, "etc")
	keep := filepath.Join(etc, "passwd")
	config := func(force bool) string {
		return rootedProvider(root) + fmt.Sprintf(`
resource "sysutils_directory" "test" {
  path          = "/a/etc"
  force_destroy = %t
}`, force)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config(true),
				Check: func(_ *terraform.State) error {
					return os.WriteFile(keep, []byte("root:x:0:0::/root:/bin/sh\n"), 0o644)
				},
			},
			{
				Config:      config(true),
				Destroy:     true,
				ExpectError: regexp.MustCompile(`protected\s+system\s+directory`),
			},
			{
				// Leave something that the post-test destroy can remove.
				PreConfig: func() {
					if _, err := os.Stat(keep); err != nil {
						t.Errorf("force_destroy deleted the tree's /etc: %v", err)
					}
					_ = os.Remove(keep)
				},
				Config: config(false),
			},
		},
	})
}

// A relative symlink target is checked against the link's real location in
// the tree, not its configured path: "/a/b/link" with "/a/b" a symlink to "/"
// is "/link", where "../x" leads above root_dir.
func TestAccRootDir_symlinkTargetViaSymlinkedParent(t *testing.T) {
	requireRoot(t)
	root := testRootDir(t)
	mustMkdir(t, filepath.Join(root, "a"))
	mustSymlink(t, "/", filepath.Join(root, "a", "b"))

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: rootedProvider(root) + `
resource "sysutils_symlink" "test" {
  path   = "/a/b/link"
  target = "../outside"
}`,
				ExpectError: regexp.MustCompile(`escapes\s+root_dir`),
			},
		},
	})
	if _, err := os.Lstat(filepath.Join(root, "link")); !os.IsNotExist(err) {
		t.Errorf("symlink leading above root_dir was created: %v", err)
	}
}
