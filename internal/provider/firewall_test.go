package provider

import (
	"encoding/json"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"
)

func TestParseFirewallAddress(t *testing.T) {
	for in, want := range map[string]string{
		"10.0.0.0/8":    "10.0.0.0/8",
		"10.0.0.1":      "10.0.0.1",
		"10.0.0.1/32":   "10.0.0.1",
		"0.0.0.0/0":     "0.0.0.0/0",
		"2001:db8::/32": "2001:db8::/32",
		"2001:DB8::1":   "2001:db8::1",
		"fd00::1/128":   "fd00::1",
	} {
		p, err := parseFirewallAddress(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if got := formatFirewallAddress(p); got != want {
			t.Errorf("%q: formatted as %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{"", "10.0.0.5/8", "10.0.0.0/33", "fe80::1%eth0", "::ffff:10.0.0.1", "host.example", "10.0.0.0/", " 10.0.0.1", "10.0.0.1/8 "} {
		if _, err := parseFirewallAddress(in); err == nil {
			t.Errorf("%q: accepted", in)
		}
	}
}

func TestParsePorts(t *testing.T) {
	got, err := parsePorts([]string{"8000-8100", "22", "443"})
	if err != nil {
		t.Fatal(err)
	}
	if s := portStrings(got); !slices.Equal(s, []string{"22", "443", "8000-8100"}) {
		t.Errorf("got %v", s)
	}
	for _, bad := range [][]string{
		{"0"}, {"65536"}, {"022"}, {"-1"}, {"1-"}, {"80-80"}, {"90-80"}, {"a"}, {" 22"}, {"22,23"}, {"1:2"},
		{"22", "22"}, {"20-30", "25"}, {"20-30", "30-40"},
		{"1-2", "3-4", "5-6", "7-8", "9-10", "11-12", "13-14", "15", "16"}, // 16 entries
	} {
		if _, err := parsePorts(bad); err == nil {
			t.Errorf("%q: accepted", bad)
		}
	}
	if _, err := parsePorts([]string{"1-2", "3-4", "5-6", "7-8", "9-10", "11-12", "13-14"}); err != nil {
		t.Errorf("14 entries: %v", err)
	}
}

func TestValidateFirewallNames(t *testing.T) {
	for _, ok := range []string{"ssh", "a", "web-80.v4_x", strings.Repeat("a", 48)} {
		if err := validateFirewallName(ok); err != nil {
			t.Errorf("name %q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-x", ".x", "a b", "a\"b", "a:b", "a;b", strings.Repeat("a", 49), "ä"} {
		if validateFirewallName(bad) == nil {
			t.Errorf("name %q accepted", bad)
		}
	}
	for _, ok := range []string{"allow ssh", "x", "from 10.0.0.0/8 (office)", strings.Repeat("c", 64)} {
		if err := validateFirewallComment(ok); err != nil {
			t.Errorf("comment %q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", " x", "x ", "a  b", "a\"b", "a\\b", "a;b", "a$b", "a\nb", "a#b", "a'b", strings.Repeat("c", 65)} {
		if validateFirewallComment(bad) == nil {
			t.Errorf("comment %q accepted", bad)
		}
	}
	for _, ok := range []string{"eth0", "br-1a2b", "wg0", "enp0s31f6", "veth.1@x", "abcdefghijklmno"} {
		if err := validateInterfaceName(ok); err != nil {
			t.Errorf("interface %q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-i", ".", "..", "eth0/1", "eth 0", "eth+", "eth*", "abcdefghijklmnop", "a\"b"} {
		if validateInterfaceName(bad) == nil {
			t.Errorf("interface %q accepted", bad)
		}
	}
}

func baseRule() firewallRule {
	return firewallRule{name: "r", family: firewallFamilyInet, chain: firewallChainInput, protocol: firewallProtoAll, action: firewallActionAccept}
}

func TestFirewallRuleValidate(t *testing.T) {
	ok := []func(r *firewallRule){
		func(r *firewallRule) {},
		func(r *firewallRule) {
			r.family, r.source, r.protocol = firewallFamilyIPv4, netip.MustParsePrefix("10.0.0.0/8"), firewallProtoTCP
			r.destPorts = []portRange{{22, 22}}
		},
		func(r *firewallRule) { r.family, r.protocol = firewallFamilyIPv6, firewallProtoICMPv6 },
		func(r *firewallRule) { r.chain, r.inIface, r.outIface = firewallChainForward, "eth0", "eth1" },
		func(r *firewallRule) { r.chain, r.outIface = firewallChainOutput, "eth1" },
	}
	for i, mod := range ok {
		r := baseRule()
		mod(&r)
		if err := r.validate(); err != nil {
			t.Errorf("valid rule %d: %v", i, err)
		}
	}
	bad := map[string]func(r *firewallRule){
		"inet with source": func(r *firewallRule) { r.source = netip.MustParsePrefix("10.0.0.0/8") },
		"ipv6 with IPv4 address": func(r *firewallRule) {
			r.family, r.destination = firewallFamilyIPv6, netip.MustParsePrefix("10.0.0.1/32")
		},
		"ipv4 with IPv6 address": func(r *firewallRule) { r.family, r.source = firewallFamilyIPv4, netip.MustParsePrefix("fd00::/8") },
		"icmp with inet":         func(r *firewallRule) { r.protocol = firewallProtoICMP },
		"icmpv6 with ipv4":       func(r *firewallRule) { r.family, r.protocol = firewallFamilyIPv4, firewallProtoICMPv6 },
		"ports without protocol": func(r *firewallRule) { r.destPorts = []portRange{{22, 22}} },
		"ports with icmp": func(r *firewallRule) {
			r.family, r.protocol, r.sourcePorts = firewallFamilyIPv4, firewallProtoICMP, []portRange{{1, 1}}
		},
		"in_interface on output": func(r *firewallRule) { r.chain, r.inIface = firewallChainOutput, "eth0" },
		"out_interface on input": func(r *firewallRule) { r.outIface = "eth0" },
		"bad family":             func(r *firewallRule) { r.family = "ip" },
		"bad chain":              func(r *firewallRule) { r.chain = "INPUT" },
		"bad action":             func(r *firewallRule) { r.action = "log" },
		"bad protocol":           func(r *firewallRule) { r.protocol = "gre" },
		"bad name":               func(r *firewallRule) { r.name = "a b" },
		"bad comment":            func(r *firewallRule) { r.comment = `x" drop comment "` },
		"bad interface":          func(r *firewallRule) { r.inIface = `x" drop` },
		"host bits":              func(r *firewallRule) { r.family, r.source = firewallFamilyIPv4, netip.MustParsePrefix("10.0.0.1/8") },
		"overlapping ports":      func(r *firewallRule) { r.protocol, r.destPorts = firewallProtoTCP, []portRange{{1, 10}, {5, 5}} },
	}
	for name, mod := range bad {
		r := baseRule()
		mod(&r)
		if err := r.validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func fullRule() firewallRule {
	return firewallRule{
		name: "ssh", family: firewallFamilyIPv4, chain: firewallChainInput, protocol: firewallProtoTCP,
		source: netip.MustParsePrefix("10.0.0.0/8"), destination: netip.MustParsePrefix("192.168.1.1/32"),
		sourcePorts: []portRange{{1000, 2000}}, destPorts: []portRange{{22, 22}, {80, 80}, {8000, 8100}},
		inIface: "eth0", action: firewallActionAccept, comment: "hello world",
	}
}

func TestFirewallRuleNftExpr(t *testing.T) {
	want := `meta nfproto ipv4 iifname "eth0" ip saddr 10.0.0.0/8 ip daddr 192.168.1.1 meta l4proto tcp tcp sport { 1000-2000 } tcp dport { 22, 80, 8000-8100 } accept comment "tf-sysutils:ssh hello world"`
	if got := fullRule().nftExpr(); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	r := baseRule()
	r.action = firewallActionReject
	if got := r.nftExpr(); got != `reject comment "tf-sysutils:r"` {
		t.Errorf("inet rule: %s", got)
	}
	r = baseRule()
	r.family, r.protocol, r.chain, r.outIface, r.action = firewallFamilyIPv6, firewallProtoICMPv6, firewallChainOutput, "lo", firewallActionDrop
	if got := r.nftExpr(); got != `meta nfproto ipv6 oifname "lo" meta l4proto ipv6-icmp drop comment "tf-sysutils:r"` {
		t.Errorf("ipv6 rule: %s", got)
	}
	if got := nftChainDecl("forward"); got != "add chain inet terraform_sysutils forward { type filter hook forward priority 0; policy accept; }" {
		t.Errorf("chain: %s", got)
	}
}

func TestFirewallRuleIptablesArgs(t *testing.T) {
	want := []string{"-s", "10.0.0.0/8", "-d", "192.168.1.1/32", "-i", "eth0", "-p", "tcp",
		"-m", "multiport", "--sports", "1000:2000", "-m", "multiport", "--dports", "22,80,8000:8100",
		"-m", "comment", "--comment", "tf-sysutils:ssh hello world", "-j", "ACCEPT"}
	if got := fullRule().iptablesArgs(); !slices.Equal(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	r := baseRule()
	r.family, r.protocol, r.action = firewallFamilyIPv6, firewallProtoICMPv6, firewallActionReject
	if got := r.iptablesArgs(); !slices.Equal(got, []string{"-p", "ipv6-icmp", "-m", "comment", "--comment", "tf-sysutils:r", "-j", "REJECT"}) {
		t.Errorf("got %q", got)
	}
}

// nftTestListing is "nft -j list table inet terraform_sysutils" as printed
// by nft 1.0.9 for rules added with the provider's rendering and by hand.
const nftTestListing = `{"nftables": [{"metainfo": {"version": "1.0.9", "release_name": "Old Doc Yak #3", "json_schema_version": 1}},
{"table": {"family": "inet", "name": "terraform_sysutils", "handle": 2}},
{"chain": {"family": "inet", "table": "terraform_sysutils", "name": "input", "handle": 1, "type": "filter", "hook": "input", "prio": 0, "policy": "accept"}},
{"rule": {"family": "inet", "table": "terraform_sysutils", "chain": "input", "handle": 3, "comment": "tf-sysutils:ssh hello world", "expr": [
  {"match": {"op": "==", "left": {"meta": {"key": "iifname"}}, "right": "eth0"}},
  {"match": {"op": "==", "left": {"payload": {"protocol": "ip", "field": "saddr"}}, "right": {"prefix": {"addr": "10.0.0.0", "len": 8}}}},
  {"match": {"op": "==", "left": {"payload": {"protocol": "ip", "field": "daddr"}}, "right": "192.168.1.1"}},
  {"match": {"op": "==", "left": {"payload": {"protocol": "tcp", "field": "sport"}}, "right": {"range": [1000, 2000]}}},
  {"match": {"op": "==", "left": {"payload": {"protocol": "tcp", "field": "dport"}}, "right": {"set": [22, 80, {"range": [8000, 8100]}]}}},
  {"accept": null}]}},
{"rule": {"family": "inet", "table": "terraform_sysutils", "chain": "input", "handle": 4, "comment": "tf-sysutils:x", "expr": [
  {"match": {"op": "==", "left": {"meta": {"key": "l4proto"}}, "right": "ipv6-icmp"}}, {"drop": null}]}},
{"rule": {"family": "inet", "table": "terraform_sysutils", "chain": "input", "handle": 5, "comment": "tf-sysutils:y", "expr": [
  {"match": {"op": "==", "left": {"meta": {"key": "l4proto"}}, "right": "udp"}}, {"reject": {"type": "icmpx", "expr": "port-unreachable"}}]}},
{"rule": {"family": "inet", "table": "terraform_sysutils", "chain": "input", "handle": 6, "comment": "tf-sysutils:z", "expr": [
  {"match": {"op": "==", "left": {"payload": {"protocol": "ip6", "field": "saddr"}}, "right": {"prefix": {"addr": "fd00::", "len": 8}}}},
  {"match": {"op": "==", "left": {"payload": {"protocol": "udp", "field": "dport"}}, "right": 53}},
  {"reject": {"type": "icmpv6", "expr": "port-unreachable"}}]}},
{"rule": {"family": "inet", "table": "terraform_sysutils", "chain": "input", "handle": 7, "comment": "tf-sysutils:counted", "expr": [
  {"match": {"op": "==", "left": {"meta": {"key": "l4proto"}}, "right": "icmp"}}, {"counter": {"packets": 0, "bytes": 0}}, {"drop": null}]}},
{"rule": {"family": "inet", "table": "terraform_sysutils", "chain": "input", "handle": 8, "comment": "tf-sysutils:v4", "expr": [
  {"match": {"op": "==", "left": {"meta": {"key": "nfproto"}}, "right": "ipv4"}}, {"drop": null}]}},
{"rule": {"family": "inet", "table": "terraform_sysutils", "chain": "input", "handle": 9, "comment": "tf-sysutils:neg", "expr": [
  {"match": {"op": "!=", "left": {"meta": {"key": "iifname"}}, "right": "lo"}}, {"drop": null}]}},
{"rule": {"family": "inet", "table": "terraform_sysutils", "chain": "input", "handle": 10, "comment": "tf-sysutils:rst", "expr": [
  {"match": {"op": "==", "left": {"meta": {"key": "l4proto"}}, "right": "tcp"}}, {"reject": {"type": "tcp reset"}}]}},
{"rule": {"family": "inet", "table": "terraform_sysutils", "chain": "input", "handle": 11, "expr": [{"drop": null}]}}
]}`

func TestParseNftRules(t *testing.T) {
	l, err := parseNftListing([]byte(nftTestListing))
	if err != nil {
		t.Fatal(err)
	}
	if !l.hasNftTable() {
		t.Error("table not found")
	}
	rules := l.rules()
	if len(rules) != 9 {
		t.Fatalf("got %d rules", len(rules))
	}
	byName := map[string]firewallRule{}
	failed := map[string]bool{}
	for _, lr := range rules {
		r, err := parseNftRule(lr)
		if err != nil {
			failed[strings.TrimPrefix(lr.Comment, firewallTagPrefix)] = true
			continue
		}
		byName[r.name] = r
	}
	if r := byName["ssh"]; !r.equal(fullRule()) {
		t.Errorf("ssh: got %+v", r)
	}
	x := baseRule()
	x.name, x.family, x.protocol, x.action = "x", firewallFamilyIPv6, firewallProtoICMPv6, firewallActionDrop
	if !byName["x"].equal(x) {
		t.Errorf("x: got %+v", byName["x"])
	}
	y := baseRule()
	y.name, y.protocol, y.action = "y", firewallProtoUDP, firewallActionReject
	if !byName["y"].equal(y) {
		t.Errorf("y: got %+v", byName["y"])
	}
	z := baseRule()
	z.name, z.family, z.protocol, z.action = "z", firewallFamilyIPv6, firewallProtoUDP, firewallActionReject
	z.source, z.destPorts = netip.MustParsePrefix("fd00::/8"), []portRange{{53, 53}}
	if !byName["z"].equal(z) {
		t.Errorf("z: got %+v", byName["z"])
	}
	v4 := baseRule()
	v4.name, v4.family, v4.action = "v4", firewallFamilyIPv4, firewallActionDrop
	if !byName["v4"].equal(v4) {
		t.Errorf("v4: got %+v", byName["v4"])
	}
	// Counters, negations, other reject types and untagged rules are not
	// what the provider adds.
	for _, n := range []string{"counted", "neg", "rst", ""} {
		if !failed[n] {
			t.Errorf("rule %q: parsed", n)
		}
	}

	tables, err := parseNftListing([]byte(`{"nftables": [{"metainfo": {}}, {"table": {"family": "ip", "name": "terraform_sysutils"}}, {"table": {"family": "inet", "name": "filter"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if tables.hasNftTable() {
		t.Error("table of another family or name taken for the provider's")
	}
}

func TestParseNftRuleConflicts(t *testing.T) {
	mk := func(exprs ...string) nftListedRule {
		lr := nftListedRule{Chain: "input", Comment: "tf-sysutils:c"}
		for _, e := range exprs {
			lr.Expr = append(lr.Expr, json.RawMessage(e))
		}
		return lr
	}
	nfv6 := `{"match": {"op": "==", "left": {"meta": {"key": "nfproto"}}, "right": "ipv6"}}`
	ip4 := `{"match": {"op": "==", "left": {"payload": {"protocol": "ip", "field": "saddr"}}, "right": "10.0.0.1"}}`
	tcp := `{"match": {"op": "==", "left": {"meta": {"key": "l4proto"}}, "right": "tcp"}}`
	udpPort := `{"match": {"op": "==", "left": {"payload": {"protocol": "udp", "field": "dport"}}, "right": 53}}`
	for name, lr := range map[string]nftListedRule{
		"family conflict":   mk(nfv6, ip4, `{"drop": null}`),
		"protocol conflict": mk(tcp, udpPort, `{"drop": null}`),
		"no verdict":        mk(tcp),
		"two verdicts":      mk(`{"drop": null}`, `{"accept": null}`),
		"other chain":       {Chain: "prerouting", Comment: "tf-sysutils:c", Expr: []json.RawMessage{json.RawMessage(`{"drop": null}`)}},
		"jump":              mk(`{"jump": {"target": "x"}}`),
	} {
		if _, err := parseNftRule(lr); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	r, err := parseNftRule(mk(tcp, `{"match": {"op": "==", "left": {"payload": {"protocol": "tcp", "field": "dport"}}, "right": {"range": [8000, 8100]}}}`, `{"accept": null}`))
	if err != nil || !slices.Equal(r.destPorts, []portRange{{8000, 8100}}) || r.family != firewallFamilyInet {
		t.Errorf("range: %+v, %v", r, err)
	}
}

func TestSplitIptablesLine(t *testing.T) {
	words, err := splitIptablesLine(`-A INPUT -s 10.0.0.0/8 -m comment --comment "tf-sysutils:ssh hello \"q\" \\ world" -j ACCEPT`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-A", "INPUT", "-s", "10.0.0.0/8", "-m", "comment", "--comment", `tf-sysutils:ssh hello "q" \ world`, "-j", "ACCEPT"}
	if !slices.Equal(words, want) {
		t.Errorf("got %q", words)
	}
	for _, bad := range []string{`-A INPUT --comment "x`, `-A INPUT --comment "x\`} {
		if _, err := splitIptablesLine(bad); err == nil {
			t.Errorf("%q: split", bad)
		}
	}
}

func TestParseIptablesRules(t *testing.T) {
	parse := func(family, line string) (firewallRule, error) {
		t.Helper()
		words, err := splitIptablesLine(line)
		if err != nil {
			t.Fatal(err)
		}
		return parseIptablesRule(family, words)
	}
	// As printed by iptables 1.8.10 for the provider's rendering.
	r, err := parse(firewallFamilyIPv4, `-A INPUT -s 10.0.0.0/8 -d 192.168.1.1/32 -i eth0 -p tcp -m multiport --sports 1000:2000 -m multiport --dports 22,80,8000:8100 -m comment --comment "tf-sysutils:ssh hello world" -j ACCEPT`)
	if err != nil || !r.equal(fullRule()) {
		t.Errorf("ssh: %+v, %v", r, err)
	}
	r, err = parse(firewallFamilyIPv6, `-A OUTPUT -o lo -p ipv6-icmp -m comment --comment "tf-sysutils:x" -j REJECT --reject-with icmp6-port-unreachable`)
	want := baseRule()
	want.name, want.family, want.chain, want.outIface, want.protocol, want.action = "x", firewallFamilyIPv6, firewallChainOutput, "lo", firewallProtoICMPv6, firewallActionReject
	if err != nil || !r.equal(want) {
		t.Errorf("x: %+v, %v", r, err)
	}
	// Single-port matches of the tcp module are understood as well.
	r, err = parse(firewallFamilyIPv4, `-A FORWARD -p udp -m udp --dport 53 -m comment --comment tf-sysutils:dns -j DROP`)
	if err != nil || !slices.Equal(r.destPorts, []portRange{{53, 53}}) || r.protocol != firewallProtoUDP || r.chain != firewallChainForward {
		t.Errorf("dns: %+v, %v", r, err)
	}
	for _, bad := range []struct{ family, line string }{
		{firewallFamilyIPv4, `-A INPUT ! -s 10.0.0.0/8 -m comment --comment "tf-sysutils:n" -j DROP`},
		{firewallFamilyIPv4, `-A INPUT -m comment --comment "tf-sysutils:n" -j LOG`},
		{firewallFamilyIPv4, `-A INPUT -m state --state NEW -m comment --comment "tf-sysutils:n" -j DROP`},
		{firewallFamilyIPv4, `-A INPUT -p tcp -m comment --comment "tf-sysutils:n" -j REJECT --reject-with tcp-reset`},
		{firewallFamilyIPv4, `-A DOCKER -m comment --comment "tf-sysutils:n" -j DROP`},
		{firewallFamilyIPv4, `-A INPUT -j DROP`},
		{firewallFamilyIPv4, `-A INPUT -m comment --comment "other" -j DROP`},
		{firewallFamilyIPv4, `-A INPUT -m comment --comment "tf-sysutils:n"`},
		{firewallFamilyIPv4, `-A INPUT -m comment --comment "tf-sysutils:n" -j DROP -i`},
		{firewallFamilyIPv6, `-A INPUT -s 10.0.0.0/8 -m comment --comment "tf-sysutils:n" -j DROP`},
	} {
		if _, err := parse(bad.family, bad.line); err == nil {
			t.Errorf("%q: parsed", bad.line)
		}
	}
}

func TestParseFirewallTag(t *testing.T) {
	for in, want := range map[string][2]string{
		"tf-sysutils:ssh":               {"ssh", ""},
		"tf-sysutils:ssh allow the web": {"ssh", "allow the web"},
	} {
		n, c, ok := parseFirewallTag(in)
		if !ok || n != want[0] || c != want[1] {
			t.Errorf("%q: %q %q %v", in, n, c, ok)
		}
	}
	for _, in := range []string{"", "ssh", "tf-sysutils:", "tf-sysutils: x", "TF-SYSUTILS:x"} {
		if _, _, ok := parseFirewallTag(in); ok {
			t.Errorf("%q: accepted", in)
		}
	}
	r := fullRule()
	if n, c, _ := parseFirewallTag(r.tag()); n != r.name || c != r.comment {
		t.Errorf("round trip: %q %q", n, c)
	}
}

// TestIptablesRoundTrip checks that the words the provider passes to
// iptables -A parse back into the same rule, since iptables -S prints them
// in that order and form.
func TestIptablesRoundTrip(t *testing.T) {
	rules := []firewallRule{fullRule()}
	r := baseRule()
	r.family, r.chain, r.inIface, r.outIface, r.action = firewallFamilyIPv6, firewallChainForward, "eth0", "wg0", firewallActionDrop
	r.destination = netip.MustParsePrefix("2001:db8::/32")
	rules = append(rules, r)
	r = baseRule()
	r.family, r.protocol, r.sourcePorts = firewallFamilyIPv4, firewallProtoSCTP, []portRange{{5, 5}}
	rules = append(rules, r)
	for _, want := range rules {
		if err := want.validate(); err != nil {
			t.Fatal(err)
		}
		words := append([]string{"-A", iptablesChain(want.chain)}, want.iptablesArgs()...)
		got, err := parseIptablesRule(want.family, words)
		if err != nil || !got.equal(want) {
			t.Errorf("%q: %+v, %v", words, got, err)
		}
	}
}

func TestFirewallFoundMerged(t *testing.T) {
	v4 := baseRule().withFamily(firewallFamilyIPv4)
	v6 := baseRule().withFamily(firewallFamilyIPv6)
	f := &firewallFound{rules: []foundRule{{rule: v4, ipFamily: firewallFamilyIPv4}, {rule: v6, ipFamily: firewallFamilyIPv6}}}
	got, err := f.merged()
	if err != nil || got.family != firewallFamilyInet {
		t.Errorf("v4+v6: %+v, %v", got, err)
	}
	// Two copies in one family are not one rule.
	f = &firewallFound{rules: []foundRule{{rule: v4, ipFamily: firewallFamilyIPv4}, {rule: v4, ipFamily: firewallFamilyIPv4}}}
	if _, err := f.merged(); err == nil {
		t.Error("duplicate merged")
	}
	// Nor are two rules that differ.
	other := v6
	other.action = firewallActionDrop
	f = &firewallFound{rules: []foundRule{{rule: v4, ipFamily: firewallFamilyIPv4}, {rule: other, ipFamily: firewallFamilyIPv6}}}
	if _, err := f.merged(); err == nil {
		t.Error("different rules merged")
	}
	// nftables has no inet pairs.
	f = &firewallFound{rules: []foundRule{{rule: v4}, {rule: v6}}}
	if _, err := f.merged(); err == nil {
		t.Error("nftables pair merged")
	}
	f = &firewallFound{rules: []foundRule{{rule: v4, err: errTest, text: "rule 3"}}}
	if _, err := f.merged(); err == nil || !strings.Contains(err.Error(), "rule 3") {
		t.Errorf("unparsed rule: %v", err)
	}
}

var errTest = errors.New("test")
