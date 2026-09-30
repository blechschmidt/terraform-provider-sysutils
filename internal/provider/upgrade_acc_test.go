package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/go-version"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// Upgrade tests: apply a configuration with the last released provider from
// the registry, then plan it with the current build. The plan must be empty,
// which catches schema changes that break existing state (new attributes with
// defaults or RequiresReplace, renamed or retyped attributes, a missing state
// upgrader) before a release ships them.
//
// Both steps use the registry address of the released provider, so that the
// in-process provider of the second step takes over the state of the first.
//
// Resources that are not in a release yet are covered by the upgrade tests
// from a local baseline build in upgrade_local_acc_test.go.

// defaultUpgradeFromVersion is the release the upgrade tests start from.
// SYSUTILS_UPGRADE_FROM_VERSION overrides it. Bump it after every release.
const defaultUpgradeFromVersion = "1.1.0"

const (
	upgradeProviderHost      = "registry.terraform.io"
	upgradeProviderNamespace = "blechschmidt"
)

// upgradeFirstRelease is the first release that contains a resource. An empty
// value means the resource has not been released yet: there is no state out
// in the wild to be compatible with, so its upgrade test is skipped until the
// release that ships it (set the version here then).
var upgradeFirstRelease = map[string]string{
	"sysutils_file":      "1.0.0",
	"sysutils_exec":      "1.0.0",
	"sysutils_directory": "1.1.0",
}

func upgradeFromVersion() string {
	if v := os.Getenv("SYSUTILS_UPGRADE_FROM_VERSION"); v != "" {
		return v
	}
	return defaultUpgradeFromVersion
}

// upgradeSteps returns the steps of an upgrade test for resourceType: apply
// config with the released provider, check that the current build plans no
// changes, then apply updated with the current build to show that the
// upgraded state is usable. The test case destroys with the current build.
func upgradeSteps(t *testing.T, resourceType, config, updated string, checks ...statecheck.StateCheck) []resource.TestStep {
	t.Helper()

	from := upgradeFromVersion()
	first, ok := upgradeFirstRelease[resourceType]
	if !ok {
		t.Fatalf("upgradeFirstRelease has no entry for %s", resourceType)
	}
	if first == "" {
		t.Skipf("%s is not in any release yet, so there is no released state to upgrade from", resourceType)
	}
	if version.Must(version.NewVersion(from)).LessThan(version.Must(version.NewVersion(first))) {
		t.Skipf("%s is not in any release yet: it was added in %s, upgrade tests start from %s", resourceType, first, from)
	}

	// The in-process provider must have the released provider's address.
	// This also overrides TF_ACC_PROVIDER_HOST=registry.opentofu.org, which
	// the container script sets for OpenTofu: the released provider is
	// installed from registry.terraform.io with every CLI.
	t.Setenv("TF_ACC_PROVIDER_HOST", upgradeProviderHost)
	t.Setenv("TF_ACC_PROVIDER_NAMESPACE", upgradeProviderNamespace)

	return []resource.TestStep{
		{
			ExternalProviders: map[string]resource.ExternalProvider{
				"sysutils": {
					Source:            upgradeProviderHost + "/" + upgradeProviderNamespace + "/sysutils",
					VersionConstraint: from,
				},
			},
			Config: config,
		},
		{
			ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
			Config:                   config,
			ConfigPlanChecks: resource.ConfigPlanChecks{
				PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
			},
			ConfigStateChecks: checks,
		},
		{
			ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
			Config:                   updated,
		},
	}
}

func TestAccUpgrade_file(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "app.conf")
	config := func(content, mode string) string {
		return fmt.Sprintf(`
resource "sysutils_file" "test" {
  path    = %q
  content = %q
  mode    = %q
}
`, target, content, mode)
	}

	sum := sha256.Sum256([]byte("listen = 8080\n"))

	resource.Test(t, resource.TestCase{
		Steps: upgradeSteps(t, "sysutils_file",
			config("listen = 8080\n", "0640"),
			config("listen = 9090\n", "0600"),
			// Attributes that the released provider did not have are
			// filled in by the current build on refresh.
			statecheck.ExpectKnownValue("sysutils_file.test", tfjsonpath.New("content_sha256"),
				knownvalue.StringExact(hex.EncodeToString(sum[:]))),
		),
		CheckDestroy: func(*terraform.State) error {
			if _, err := os.Lstat(target); !os.IsNotExist(err) {
				return fmt.Errorf("%s still exists after destroy (err=%v)", target, err)
			}
			return nil
		},
	})
}

func TestAccUpgrade_exec(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "runs")
	config := func(trigger string) string {
		return fmt.Sprintf(`
resource "sysutils_exec" "test" {
  command  = ["/bin/sh", "-c", "echo run >> %s; echo out; echo err >&2"]
  triggers = { rev = %q }
}
`, marker, trigger)
	}

	resource.Test(t, resource.TestCase{
		Steps: append(upgradeSteps(t, "sysutils_exec",
			config("1"),
			config("2"),
			statecheck.ExpectKnownValue("sysutils_exec.test", tfjsonpath.New("stdout"), knownvalue.StringExact("out\n")),
			statecheck.ExpectKnownValue("sysutils_exec.test", tfjsonpath.New("max_output_bytes"), knownvalue.Int64Exact(defaultMaxOutputBytes)),
			statecheck.ExpectKnownValue("sysutils_exec.test", tfjsonpath.New("truncated"), knownvalue.Bool(false)),
		), resource.TestStep{
			// Upgrading must not re-run the command: it ran once with the
			// released provider and once more for the trigger change.
			ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
			Config:                   config("2"),
			Check: func(*terraform.State) error {
				b, err := os.ReadFile(marker)
				if err != nil {
					return err
				}
				if got, want := string(b), "run\nrun\n"; got != want {
					return fmt.Errorf("command ran %q, want %q", got, want)
				}
				return nil
			},
		}),
	})
}

func TestAccUpgrade_directory(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "data")
	config := func(mode string) string {
		return fmt.Sprintf(`
resource "sysutils_directory" "test" {
  path = %q
  mode = %q
}
`, target, mode)
	}

	resource.Test(t, resource.TestCase{
		Steps: upgradeSteps(t, "sysutils_directory", config("0750"), config("0700")),
	})
}
