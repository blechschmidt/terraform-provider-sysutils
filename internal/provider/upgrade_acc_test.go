package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/go-version"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
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
const defaultUpgradeFromVersion = "1.2.0"

const (
	upgradeProviderHost      = "registry.terraform.io"
	upgradeProviderNamespace = "blechschmidt"
)

// upgradeFirstRelease is the first release that contains a resource. An empty
// value means the resource has not been released yet: there is no state out
// in the wild to be compatible with, so its upgrade test is skipped until the
// release that ships it (set the version here then).
var upgradeFirstRelease = map[string]string{
	"sysutils_file":          "1.0.0",
	"sysutils_exec":          "1.0.0",
	"sysutils_directory":     "1.1.0",
	"sysutils_firewall_rule": "1.1.0",
	"sysutils_swap":          "1.1.0",
}

func upgradeFromVersion() string {
	if v := os.Getenv("SYSUTILS_UPGRADE_FROM_VERSION"); v != "" {
		return v
	}
	return defaultUpgradeFromVersion
}

// upgradeSteps returns the steps of an upgrade test for resourceType from
// the released provider: the steps of upgradeCheckSteps (apply config with
// the released provider, then check that the current build plans no
// changes and keeps the stored attributes), followed by an apply of updated
// with the current build to show that the upgraded state is usable. The
// test case destroys with the current build.
func upgradeSteps(t *testing.T, resourceType, config, updated string, checks ...statecheck.StateCheck) []resource.TestStep {
	t.Helper()
	return upgradeStepsWith(t, resourceType, config, updated, upgradeOptions{}, checks...)
}

// upgradeStepsWith is upgradeSteps with options.
func upgradeStepsWith(t *testing.T, resourceType, config, updated string, opts upgradeOptions, checks ...statecheck.StateCheck) []resource.TestStep {
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

	waitForRegistryVersion(t, from)

	// The in-process provider must have the released provider's address.
	// This also overrides TF_ACC_PROVIDER_HOST=registry.opentofu.org, which
	// the container script sets for OpenTofu: the released provider is
	// installed from registry.terraform.io with every CLI.
	t.Setenv("TF_ACC_PROVIDER_HOST", upgradeProviderHost)
	t.Setenv("TF_ACC_PROVIDER_NAMESPACE", upgradeProviderNamespace)

	released := resource.ExternalProvider{
		Source:            upgradeProviderHost + "/" + upgradeProviderNamespace + "/sysutils",
		VersionConstraint: from,
	}
	return append(upgradeCheckSteps(released, config, opts, checks...), resource.TestStep{
		ProtoV6ProviderFactories: opts.providerFactories(),
		Config:                   updated,
	})
}

// upgradeOptions adjusts the steps of an upgrade test.
type upgradeOptions struct {
	// changed lists stored attributes, as "<resource address>.<attribute>",
	// that the upgrade changes on purpose; they may, but need not, change.
	changed []string
	// factories serve the current build; testAccProtoV6ProviderFactories
	// if nil. Tests that confine the provider, such as to a network
	// namespace, pass their own.
	factories map[string]func() (tfprotov6.ProviderServer, error)
}

func (o upgradeOptions) providerFactories() map[string]func() (tfprotov6.ProviderServer, error) {
	if o.factories != nil {
		return o.factories
	}
	return testAccProtoV6ProviderFactories
}

// upgradeCheckSteps returns the steps that upgrade config from the provider
// baseline to the current build:
//
//  1. apply config with baseline and record the state;
//  2. plan config with the current build, which must plan no changes, then
//     apply that empty plan, which stores the upgraded state; checks run
//     here, and every attribute of the recorded state must still have its
//     value (attributes the baseline did not have may be added);
//  3. plan config again with the current build, with and without a
//     refresh: both plans must be empty.
//
// Step 2 plans with a refresh, as "terraform plan" does. A plan without one
// is only checked after the upgraded state has been stored: until then,
// Terraform plans an in-place update without any changed value for a
// resource whose schema gained a sensitive attribute since the baseline,
// because the state has no sensitivity marks for it yet.
func upgradeCheckSteps(baseline resource.ExternalProvider, config string, opts upgradeOptions, checks ...statecheck.StateCheck) []resource.TestStep {
	var before map[string]map[string]string
	return []resource.TestStep{
		{
			ExternalProviders: map[string]resource.ExternalProvider{"sysutils": baseline},
			Config:            config,
			Check: func(s *terraform.State) error {
				before = upgradeStateAttributes(s)
				if len(before) == 0 {
					return errors.New("the baseline apply left no resources in state")
				}
				return nil
			},
		},
		{
			ProtoV6ProviderFactories: opts.providerFactories(),
			Config:                   config,
			ConfigPlanChecks: resource.ConfigPlanChecks{
				PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
			},
			ConfigStateChecks: checks,
			Check: func(s *terraform.State) error {
				return upgradeCompareState(before, upgradeStateAttributes(s), opts.changed)
			},
		},
		{
			ProtoV6ProviderFactories: opts.providerFactories(),
			Config:                   config,
			PlanOnly:                 true,
			ExpectNonEmptyPlan:       false,
		},
	}
}

// upgradeRegistryWait bounds how long waitForRegistryVersion polls.
const upgradeRegistryWait = 10 * time.Minute

// waitForRegistryVersion waits until the registry lists version, which is
// the request "init" makes. For a while after a release, some of the
// registry's CDN edges still serve the old list of versions, and "init"
// fails with "no available releases match the given constraints".
func waitForRegistryVersion(t *testing.T, version string) {
	t.Helper()
	url := fmt.Sprintf("https://%s/v1/providers/%s/sysutils/versions", upgradeProviderHost, upgradeProviderNamespace)
	client := &http.Client{Timeout: 30 * time.Second}
	listed := func() (bool, error) {
		resp, err := client.Get(url)
		if err != nil {
			return false, err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return false, fmt.Errorf("GET %s: %s", url, resp.Status)
		}
		var body struct {
			Versions []struct {
				Version string `json:"version"`
			} `json:"versions"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return false, fmt.Errorf("GET %s: %w", url, err)
		}
		for _, v := range body.Versions {
			if v.Version == version {
				return true, nil
			}
		}
		return false, nil
	}
	deadline := time.Now().Add(upgradeRegistryWait)
	for {
		ok, err := listed()
		if ok {
			return
		}
		why := fmt.Sprintf("%s does not list version %s", url, version)
		if err != nil {
			why = err.Error()
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s after %s; was it released?", why, upgradeRegistryWait)
		}
		t.Logf("%s; retrying", why)
		time.Sleep(15 * time.Second)
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

func TestAccUpgrade_firewallRule(t *testing.T) {
	ns := newFirewallNetns(t, "nft", "iptables", "ip6tables")
	ns.runTerraformInside()
	resource.Test(t, resource.TestCase{
		Steps: upgradeStepsWith(t, "sysutils_firewall_rule",
			firewallUpgradeConfig("22, 2222"),
			firewallUpgradeConfig("22"),
			upgradeOptions{factories: ns.providerFactories()},
			firewallUpgradeChecks()...,
		),
		CheckDestroy: ns.checkFirewallUpgradeDestroyed(),
	})
}

func TestAccUpgrade_swap(t *testing.T) {
	env := newSwapAccEnv(t)
	resource.Test(t, resource.TestCase{
		Steps: upgradeStepsWith(t, "sysutils_swap",
			swapUpgradeConfig(env.swapfile, 7),
			swapUpgradeConfig(env.swapfile, 8),
			upgradeOptions{factories: swapProviderFactories(env.fstab, nil)},
			swapUpgradeChecks()...,
		),
		CheckDestroy: env.checkSwapUpgradeDestroyed(t),
	})
}
