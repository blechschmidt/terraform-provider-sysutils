package provider

import (
	"context"
	"errors"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// fakeNft emulates nft for a single accepting rule named "r" in the input
// chain of the provider's table. If timeoutOnce is set, the first script
// that adds the rule is applied but reported as timed out, like an nft
// that was killed after committing its transaction.
type fakeNft struct {
	mu          sync.Mutex
	hasRule     bool
	timeoutOnce bool
}

func (f *fakeNft) run(_ context.Context, spec execSpec) (*execResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	res := &execResult{Stdout: newCappedOutput(1 << 20), Stderr: newCappedOutput(1 << 20)}
	const table = `{"table": {"family": "inet", "name": "terraform_sysutils", "handle": 1}}`
	switch strings.Join(spec.Argv, " ") {
	case "nft -j list tables":
		out := `{"nftables": [{"metainfo": {}}]}`
		if f.hasRule {
			out = `{"nftables": [{"metainfo": {}}, ` + table + `]}`
		}
		_, _ = res.Stdout.Write([]byte(out))
	case "nft -j list table inet terraform_sysutils":
		if !f.hasRule {
			res.ExitCode = 1
			break
		}
		_, _ = res.Stdout.Write([]byte(`{"nftables": [{"metainfo": {}}, ` + table + `,
{"chain": {"family": "inet", "table": "terraform_sysutils", "name": "input", "handle": 1, "type": "filter", "hook": "input", "prio": 0, "policy": "accept"}},
{"rule": {"family": "inet", "table": "terraform_sysutils", "chain": "input", "handle": 2, "comment": "tf-sysutils:r", "expr": [{"accept": null}]}}]}`))
	case "nft -f -":
		switch {
		case strings.Contains(spec.Stdin, "add rule"):
			f.hasRule = true
			if f.timeoutOnce {
				f.timeoutOnce = false
				res.TimedOut, res.ExitCode = true, -1
			}
		case strings.Contains(spec.Stdin, "delete table"), strings.Contains(spec.Stdin, "delete rule"):
			f.hasRule = false
		}
	default:
		res.ExitCode = 1
	}
	return res, nil
}

func fakeNftProviderFactories(f *fakeNft) map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		"sysutils": providerserver.NewProtocol6WithError(&sysutilsProvider{
			version: "test",
			firewall: &firewallConfig{run: f.run, lookPath: func(tool string) (string, error) {
				if tool == "nft" {
					return "/usr/sbin/nft", nil
				}
				return "", exec.ErrNotFound
			}},
		}),
	}
}

// TestFirewallRule_createOutcomeUnknown checks that a rule added by an nft
// command whose outcome was not reported is still recorded in state, so
// that the next apply replaces it rather than refusing to create a rule
// that "already exists".
func TestFirewallRule_createOutcomeUnknown(t *testing.T) {
	f := &fakeNft{timeoutOnce: true}
	config := `
resource "sysutils_firewall_rule" "test" {
  name    = "r"
  chain   = "input"
  action  = "accept"
  backend = "nftables"
}
`
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: fakeNftProviderFactories(f),
		CheckDestroy: func(*terraform.State) error {
			if f.hasRule {
				return errors.New("the rule was not removed")
			}
			return nil
		},
		Steps: []resource.TestStep{
			{Config: config, ExpectError: regexp.MustCompile(`timed\s+out`)},
			{
				Config: config,
				Check:  resource.TestCheckResourceAttr("sysutils_firewall_rule.test", "active_backend", "nftables"),
			},
		},
	})
}
