package provider

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeFirewall is a commandRunner that records the commands it is given and
// answers them from a table of canned results keyed by the joined argv.
type fakeFirewall struct {
	mu      sync.Mutex
	calls   []string
	stdin   []string
	results map[string]fakeResult
}

type fakeResult struct {
	exit   int
	stdout string
}

func (f *fakeFirewall) run(_ context.Context, spec execSpec) (*execResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := strings.Join(spec.Argv, " ")
	f.calls = append(f.calls, key)
	f.stdin = append(f.stdin, spec.Stdin)
	r := f.results[key]
	res := &execResult{ExitCode: r.exit, Stdout: newCappedOutput(1 << 20), Stderr: newCappedOutput(1 << 20)}
	_, _ = res.Stdout.Write([]byte(r.stdout))
	if r.exit != 0 {
		_, _ = res.Stderr.Write([]byte("failed"))
	}
	return res, nil
}

func TestIptablesBackendApply(t *testing.T) {
	want := baseRule()
	want.name, want.protocol, want.destPorts = "web", firewallProtoTCP, []portRange{{80, 80}}
	args := strings.Join(want.withFamily(firewallFamilyIPv4).iptablesArgs(), " ")
	stale := `-A INPUT -p tcp -m multiport --dports 81 -m comment --comment "tf-sysutils:web" -j ACCEPT`
	f := &fakeFirewall{results: map[string]fakeResult{
		"iptables -w -S":  {stdout: "-P INPUT ACCEPT\n" + stale + "\n-A INPUT -m comment --comment \"tf-sysutils:webby\" -j DROP\n"},
		"ip6tables -w -S": {stdout: "-P INPUT ACCEPT\n"},
		// The IPv4 rule is missing; the IPv6 one exists already, as after
		// an interrupted apply.
		"iptables -w -C INPUT " + args:  {exit: 1},
		"ip6tables -w -C INPUT " + args: {exit: 0},
	}}
	b := &iptablesBackend{run: f.run, v4: true, v6: true}
	found, err := b.find(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	if len(found.rules) != 1 {
		t.Fatalf("found %d rules, want only the one tagged web", len(found.rules))
	}
	f.calls = nil
	changed, err := b.apply(context.Background(), want, found)
	if err != nil || !changed {
		t.Fatalf("apply: %v, %v", changed, err)
	}
	wantCalls := []string{
		"iptables -w -C INPUT " + args,
		// The replacement takes the place of the stale rule, the first of
		// the chain...
		"iptables -w -I INPUT 1 " + args,
		"ip6tables -w -C INPUT " + args,
		// ...which is then deleted by the words iptables printed for it.
		`iptables -w -D INPUT -p tcp -m multiport --dports 81 -m comment --comment tf-sysutils:web -j ACCEPT`,
	}
	if !slices.Equal(f.calls, wantCalls) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(wantCalls, "\n"))
	}

	// inet needs both commands.
	b.v6 = false
	if _, err := b.apply(context.Background(), want, &firewallFound{}); err == nil || !strings.Contains(err.Error(), "ip6tables") {
		t.Errorf("inet without ip6tables: %v", err)
	}
}

func TestNftablesBackendApply(t *testing.T) {
	want := baseRule()
	want.name = "r"
	ctx := context.Background()

	// A new rule: the table and chain are declared in the same transaction.
	f := &fakeFirewall{}
	b := &nftablesBackend{run: f.run}
	if changed, err := b.apply(ctx, want, &firewallFound{}); err != nil || !changed {
		t.Fatalf("apply: %v, %v", changed, err)
	}
	wantScript := "add table inet terraform_sysutils\n" + nftChainDecl("input") + "\nadd rule inet terraform_sysutils input " + want.nftExpr() + "\n"
	if len(f.stdin) != 1 || f.stdin[0] != wantScript || f.calls[0] != "nft -f -" {
		t.Errorf("script:\n%q\nwant:\n%q", f.stdin, wantScript)
	}

	// An unchanged rule is left alone.
	f = &fakeFirewall{}
	b.run = f.run
	found := &firewallFound{rules: []foundRule{{rule: want, nftChain: "input", nftHandle: 7}}}
	if changed, err := b.apply(ctx, want, found); err != nil || changed || len(f.calls) != 0 {
		t.Errorf("unchanged rule: %v, %v, %q", changed, err, f.calls)
	}

	// A changed rule is replaced in place, copies are deleted.
	changedRule := want
	changedRule.action = firewallActionDrop
	found.rules = append(found.rules, foundRule{rule: want, nftChain: "input", nftHandle: 9})
	if _, err := b.apply(ctx, changedRule, found); err != nil {
		t.Fatal(err)
	}
	wantScript = "add table inet terraform_sysutils\n" + nftChainDecl("input") +
		"\nreplace rule inet terraform_sysutils input handle 7 " + changedRule.nftExpr() +
		"\ndelete rule inet terraform_sysutils input handle 9\n"
	if f.stdin[0] != wantScript {
		t.Errorf("script:\n%s\nwant:\n%s", f.stdin[0], wantScript)
	}

	// Removing the last rule removes the table; otherwise only the rule.
	f = &fakeFirewall{}
	b.run = f.run
	if err := b.remove(ctx, &firewallFound{rules: found.rules[:1]}); err != nil || f.stdin[0] != "delete table inet terraform_sysutils\n" {
		t.Errorf("remove last: %v, %q", err, f.stdin)
	}
	f.stdin = nil
	if err := b.remove(ctx, &firewallFound{rules: found.rules[:1], otherRules: 1}); err != nil || f.stdin[0] != "delete rule inet terraform_sysutils input handle 7\n" {
		t.Errorf("remove: %v, %q", err, f.stdin)
	}
	// Chain names from the listing are checked before they go into a
	// script.
	if err := b.remove(ctx, &firewallFound{rules: []foundRule{{nftChain: "input; flush ruleset", nftHandle: 1}}, otherRules: 1}); err == nil {
		t.Error("unsafe chain name accepted")
	}
}

func TestFirewallDetect(t *testing.T) {
	ctx := context.Background()
	lookPath := func(have ...string) func(string) (string, error) {
		return func(name string) (string, error) {
			if slices.Contains(have, name) {
				return "/usr/sbin/" + name, nil
			}
			return "", &fakeNotFound{name}
		}
	}
	ok := &fakeFirewall{results: map[string]fakeResult{"nft -j list tables": {stdout: `{"nftables": []}`}}}
	broken := &fakeFirewall{results: map[string]fakeResult{"nft -j list tables": {exit: 1}}}
	for _, tc := range []struct {
		have []string
		run  *fakeFirewall
		want string
	}{
		{[]string{"nft", "iptables"}, ok, firewallBackendNftables},
		{[]string{"nft", "iptables"}, broken, firewallBackendIptables},
		{[]string{"ip6tables"}, ok, firewallBackendIptables},
		{nil, ok, ""},
	} {
		c := &firewallConfig{run: tc.run.run, lookPath: lookPath(tc.have...)}
		got, err := c.detect(ctx)
		if got != tc.want || (err != nil) != (tc.want == "") {
			t.Errorf("%v: got %q, %v", tc.have, got, err)
		}
	}
}

type fakeNotFound struct{ name string }

func (e *fakeNotFound) Error() string { return e.name + ": not found" }

// TestNftablesRemoveKeepsForeignObjects checks that removing the last rule
// deletes the provider's table only if it holds nothing the provider did
// not create: sets, other chains and changed base chains survive.
func TestNftablesRemoveKeepsForeignObjects(t *testing.T) {
	ctx := context.Background()
	const table = `{"table": {"family": "inet", "name": "terraform_sysutils", "handle": 2}}`
	const chain = `{"chain": {"family": "inet", "table": "terraform_sysutils", "name": "input", "handle": 1, "type": "filter", "hook": "input", "prio": 0, "policy": "accept"}}`
	const rule = `{"rule": {"family": "inet", "table": "terraform_sysutils", "chain": "input", "handle": 3, "comment": "tf-sysutils:r", "expr": [{"accept": null}]}}`
	for _, tc := range []struct {
		name, extra, want string
	}{
		{"only the rule", "", "delete table inet terraform_sysutils\n"},
		{"set added by hand", `, {"set": {"family": "inet", "name": "blocked", "table": "terraform_sysutils", "type": "ipv4_addr", "handle": 4}}`,
			"delete rule inet terraform_sysutils input handle 3\n"},
		{"chain added by hand", `, {"chain": {"family": "inet", "table": "terraform_sysutils", "name": "mine", "handle": 5}}`,
			"delete rule inet terraform_sysutils input handle 3\n"},
		{"policy changed by hand", `, {"chain": {"family": "inet", "table": "terraform_sysutils", "name": "forward", "handle": 6, "type": "filter", "hook": "forward", "prio": 0, "policy": "drop"}}`,
			"delete rule inet terraform_sysutils input handle 3\n"},
	} {
		f := &fakeFirewall{results: map[string]fakeResult{
			"nft -j list tables":                        {stdout: `{"nftables": [` + table + `]}`},
			"nft -j list table inet terraform_sysutils": {stdout: `{"nftables": [{"metainfo": {}}, ` + table + `, ` + chain + `, ` + rule + tc.extra + `]}`},
		}}
		b := &nftablesBackend{run: f.run}
		found, err := b.find(ctx, "r")
		if err != nil || len(found.rules) != 1 {
			t.Fatalf("%s: find: %+v, %v", tc.name, found, err)
		}
		f.stdin = nil
		if err := b.remove(ctx, found); err != nil {
			t.Fatalf("%s: remove: %v", tc.name, err)
		}
		if len(f.stdin) != 1 || f.stdin[0] != tc.want {
			t.Errorf("%s: script %q, want %q", tc.name, f.stdin, tc.want)
		}
	}
}
