package provider

// Firewall rules behind sysutils_firewall_rule: the rule model, its
// validation, and rendering and parsing for the nftables and iptables
// backends (see firewall_backend.go for the commands that apply them).
//
// Every rule the provider adds carries a comment that starts with
// firewallTagPrefix and the resource's name, which is how a rule is found
// again on refresh, update and destroy. Rules are parsed back into
// firewallRule, so changes made outside Terraform show up as drift of the
// individual attributes. Everything that ends up in an nft script or on an
// iptables command line is validated first; commands are always run with an
// argument vector, never through a shell.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const (
	firewallFamilyIPv4 = "ipv4"
	firewallFamilyIPv6 = "ipv6"
	firewallFamilyInet = "inet"

	firewallChainInput   = "input"
	firewallChainOutput  = "output"
	firewallChainForward = "forward"

	firewallProtoAll    = "all"
	firewallProtoTCP    = "tcp"
	firewallProtoUDP    = "udp"
	firewallProtoSCTP   = "sctp"
	firewallProtoICMP   = "icmp"
	firewallProtoICMPv6 = "icmpv6"

	firewallActionAccept = "accept"
	firewallActionDrop   = "drop"
	firewallActionReject = "reject"

	firewallBackendAuto     = "auto"
	firewallBackendNftables = "nftables"
	firewallBackendIptables = "iptables"

	// firewallTagPrefix starts the comment of every rule the provider adds;
	// the resource's name follows, then optionally a space and the comment
	// attribute.
	firewallTagPrefix = "tf-sysutils:"
	// nftTable is the provider's own nftables table, in the inet family so
	// that one table holds IPv4 and IPv6 rules.
	nftTable = "terraform_sysutils"

	// maxFirewallNameLen and maxFirewallCommentLen keep the tag within the
	// 128 bytes nftables allows for a rule comment (iptables allows 256).
	maxFirewallNameLen    = 48
	maxFirewallCommentLen = 64
	// maxMultiportEntries is the limit of iptables' multiport match, in
	// which a range counts as two ports.
	maxMultiportEntries = 15
)

var (
	firewallFamilies  = []string{firewallFamilyIPv4, firewallFamilyIPv6, firewallFamilyInet}
	firewallChains    = []string{firewallChainInput, firewallChainOutput, firewallChainForward}
	firewallProtocols = []string{firewallProtoAll, firewallProtoTCP, firewallProtoUDP, firewallProtoSCTP, firewallProtoICMP, firewallProtoICMPv6}
	firewallActions   = []string{firewallActionAccept, firewallActionDrop, firewallActionReject}
	firewallBackends  = []string{firewallBackendAuto, firewallBackendNftables, firewallBackendIptables}

	// firewallNamePattern matches rule names. The first character must not
	// be "-", and there is no white space, since the name ends at the first
	// space of the tag.
	firewallNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	// firewallCommentPattern matches comments: words of a conservative
	// character set separated by single spaces. Quotes, backslashes and
	// shell or nft metacharacters are excluded, so the comment can be
	// embedded in double quotes by both backends without escaping.
	firewallCommentPattern = regexp.MustCompile(`^[A-Za-z0-9_.,:/@+=()-]+( [A-Za-z0-9_.,:/@+=()-]+)*$`)
	// interfaceNamePattern matches network interface names: at most 15
	// bytes (IFNAMSIZ - 1) without "/", white space or quotes, not starting
	// with "-". Wildcards are not supported.
	interfaceNamePattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.:@-]{0,14}$`)
	// nftIdentPattern matches the chain names the provider puts into nft
	// scripts when it deletes rules.
	nftIdentPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)
)

// portRange is a port or an inclusive range of ports; lo == hi for a single
// port.
type portRange struct{ lo, hi uint16 }

func (p portRange) String() string {
	if p.lo == p.hi {
		return strconv.Itoa(int(p.lo))
	}
	return fmt.Sprintf("%d-%d", p.lo, p.hi)
}

// firewallRule is one rule as managed by sysutils_firewall_rule. The zero
// value of source and destination (an invalid prefix) means any address.
type firewallRule struct {
	name        string
	family      string
	chain       string
	protocol    string
	source      netip.Prefix
	destination netip.Prefix
	sourcePorts []portRange
	destPorts   []portRange
	inIface     string
	outIface    string
	action      string
	comment     string
}

func validateFirewallName(s string) error {
	if len(s) == 0 || len(s) > maxFirewallNameLen || !firewallNamePattern.MatchString(s) {
		return fmt.Errorf("name %q must be 1 to %d letters, digits, \"_\", \".\" or \"-\", starting with a letter or digit", s, maxFirewallNameLen)
	}
	return nil
}

func validateFirewallComment(s string) error {
	if len(s) > maxFirewallCommentLen || !firewallCommentPattern.MatchString(s) {
		return fmt.Errorf("comment %q must be 1 to %d letters, digits, single spaces or characters of \"_.,:/@+=()-\", without leading or trailing space", s, maxFirewallCommentLen)
	}
	return nil
}

func validateInterfaceName(s string) error {
	if !interfaceNamePattern.MatchString(s) || s == "." || s == ".." {
		return fmt.Errorf("interface name %q must be 1 to 15 letters, digits or characters of \"_.:@-\", not starting with \"-\" or \".\"", s)
	}
	return nil
}

// parseFirewallAddress parses an address or a CIDR prefix. The prefix must
// not have host bits set, so that there is only one way to write it; a
// plain address stands for a prefix of its full length.
func parseFirewallAddress(s string) (netip.Prefix, error) {
	var p netip.Prefix
	if strings.Contains(s, "/") {
		var err error
		if p, err = netip.ParsePrefix(s); err != nil {
			return netip.Prefix{}, fmt.Errorf("%q is not an IP address or CIDR prefix", s)
		}
	} else {
		a, err := netip.ParseAddr(s)
		if err != nil || a.Zone() != "" {
			return netip.Prefix{}, fmt.Errorf("%q is not an IP address or CIDR prefix", s)
		}
		p = netip.PrefixFrom(a, a.BitLen())
	}
	if p.Addr().Is4In6() {
		return netip.Prefix{}, fmt.Errorf("%q is an IPv4-mapped IPv6 address; write the IPv4 address instead", s)
	}
	if m := p.Masked(); m != p {
		return netip.Prefix{}, fmt.Errorf("%q has host bits set; write the network address, %s", s, m)
	}
	return p, nil
}

func validateFirewallAddress(s string) error {
	_, err := parseFirewallAddress(s)
	return err
}

// formatFirewallAddress is the inverse of parseFirewallAddress: a prefix of
// full length is written as a plain address.
func formatFirewallAddress(p netip.Prefix) string {
	if p.Bits() == p.Addr().BitLen() {
		return p.Addr().String()
	}
	return p.String()
}

// parsePort parses a port or a range "lo-hi" with lo < hi, both without
// leading zeros.
func parsePort(s string) (portRange, error) {
	num := func(t string) (uint16, bool) {
		if t == "" || (len(t) > 1 && t[0] == '0') {
			return 0, false
		}
		n, err := strconv.ParseUint(t, 10, 16)
		return uint16(n), err == nil && n > 0
	}
	lo, hi, isRange := strings.Cut(s, "-")
	a, ok := num(lo)
	if !ok {
		return portRange{}, fmt.Errorf("port %q must be a number from 1 to 65535, or a range such as \"8000-8100\"", s)
	}
	if !isRange {
		return portRange{a, a}, nil
	}
	b, ok := num(hi)
	if !ok || b <= a {
		return portRange{}, fmt.Errorf("port range %q must be two numbers from 1 to 65535, the first lower than the second", s)
	}
	return portRange{a, b}, nil
}

func validatePort(s string) error {
	_, err := parsePort(s)
	return err
}

// parsePorts parses a list of ports and ranges, sorted by their first port,
// and checks that no two overlap and that iptables' multiport match can
// hold them.
func parsePorts(list []string) ([]portRange, error) {
	ports := make([]portRange, 0, len(list))
	entries := 0
	for _, s := range list {
		p, err := parsePort(s)
		if err != nil {
			return nil, err
		}
		ports = append(ports, p)
		entries++
		if p.lo != p.hi {
			entries++
		}
	}
	sortPorts(ports)
	for i := 1; i < len(ports); i++ {
		if ports[i].lo <= ports[i-1].hi {
			return nil, fmt.Errorf("ports %s and %s overlap", ports[i-1], ports[i])
		}
	}
	if entries > maxMultiportEntries {
		return nil, fmt.Errorf("at most %d ports are supported, where a range counts as two, as by iptables' multiport match; split the rule", maxMultiportEntries)
	}
	return ports, nil
}

func sortPorts(ports []portRange) {
	slices.SortFunc(ports, func(a, b portRange) int { return int(a.lo) - int(b.lo) })
}

func portStrings(ports []portRange) []string {
	s := make([]string, len(ports))
	for i, p := range ports {
		s[i] = p.String()
	}
	return s
}

// validate checks the rule, including the combinations of attributes that
// no backend supports. Every rule is validated before it is rendered.
func (r firewallRule) validate() error {
	if err := validateFirewallName(r.name); err != nil {
		return err
	}
	if !slices.Contains(firewallFamilies, r.family) {
		return fmt.Errorf("family %q must be one of %s", r.family, strings.Join(firewallFamilies, ", "))
	}
	if !slices.Contains(firewallChains, r.chain) {
		return fmt.Errorf("chain %q must be one of %s", r.chain, strings.Join(firewallChains, ", "))
	}
	if !slices.Contains(firewallProtocols, r.protocol) {
		return fmt.Errorf("protocol %q must be one of %s", r.protocol, strings.Join(firewallProtocols, ", "))
	}
	if !slices.Contains(firewallActions, r.action) {
		return fmt.Errorf("action %q must be one of %s", r.action, strings.Join(firewallActions, ", "))
	}
	for _, a := range []struct {
		what string
		p    netip.Prefix
	}{{"source", r.source}, {"destination", r.destination}} {
		if !a.p.IsValid() {
			continue
		}
		switch {
		case r.family == firewallFamilyInet:
			return fmt.Errorf("a %s address requires family \"ipv4\" or \"ipv6\", not \"inet\"", a.what)
		case a.p.Addr().Is4() != (r.family == firewallFamilyIPv4):
			return fmt.Errorf("%s %s is not an address of family %q", a.what, formatFirewallAddress(a.p), r.family)
		case a.p.Masked() != a.p:
			return fmt.Errorf("%s %s has host bits set", a.what, a.p)
		}
	}
	switch {
	case r.protocol == firewallProtoICMP && r.family != firewallFamilyIPv4:
		return errors.New("protocol \"icmp\" requires family \"ipv4\"; use \"icmpv6\" for IPv6")
	case r.protocol == firewallProtoICMPv6 && r.family != firewallFamilyIPv6:
		return errors.New("protocol \"icmpv6\" requires family \"ipv6\"; use \"icmp\" for IPv4")
	}
	if len(r.sourcePorts)+len(r.destPorts) > 0 && !hasPorts(r.protocol) {
		return fmt.Errorf("ports require protocol \"tcp\", \"udp\" or \"sctp\", not %q", r.protocol)
	}
	for _, ports := range [][]portRange{r.sourcePorts, r.destPorts} {
		strs := portStrings(ports)
		if _, err := parsePorts(strs); err != nil {
			return err
		}
	}
	if r.inIface != "" {
		if err := validateInterfaceName(r.inIface); err != nil {
			return err
		}
		if r.chain == firewallChainOutput {
			return errors.New("the output chain sees no incoming interface, so in_interface cannot be used there")
		}
	}
	if r.outIface != "" {
		if err := validateInterfaceName(r.outIface); err != nil {
			return err
		}
		if r.chain == firewallChainInput {
			return errors.New("the input chain sees no outgoing interface, so out_interface cannot be used there")
		}
	}
	if r.comment != "" {
		if err := validateFirewallComment(r.comment); err != nil {
			return err
		}
	}
	return nil
}

func hasPorts(protocol string) bool {
	return protocol == firewallProtoTCP || protocol == firewallProtoUDP || protocol == firewallProtoSCTP
}

// equal reports whether r and o describe the same rule. Ports are compared
// as sets.
func (r firewallRule) equal(o firewallRule) bool {
	portsEqual := func(a, b []portRange) bool {
		a, b = slices.Clone(a), slices.Clone(b)
		sortPorts(a)
		sortPorts(b)
		return slices.Equal(a, b)
	}
	return r.name == o.name && r.family == o.family && r.chain == o.chain && r.protocol == o.protocol &&
		r.source == o.source && r.destination == o.destination &&
		portsEqual(r.sourcePorts, o.sourcePorts) && portsEqual(r.destPorts, o.destPorts) &&
		r.inIface == o.inIface && r.outIface == o.outIface && r.action == o.action && r.comment == o.comment
}

// tag returns the comment the rule is added with.
func (r firewallRule) tag() string {
	if r.comment == "" {
		return firewallTagPrefix + r.name
	}
	return firewallTagPrefix + r.name + " " + r.comment
}

// parseFirewallTag splits a rule comment into the name and the comment
// attribute. ok is false if the comment is not a tag of this provider.
func parseFirewallTag(s string) (name, comment string, ok bool) {
	rest, ok := strings.CutPrefix(s, firewallTagPrefix)
	if !ok {
		return "", "", false
	}
	name, comment, _ = strings.Cut(rest, " ")
	return name, comment, name != ""
}

// withFamily returns r for a single address family, as added to iptables
// or ip6tables.
func (r firewallRule) withFamily(family string) firewallRule {
	r.family = family
	return r
}

// ---------------------------------------------------------------------------
// nftables

// nftExpr renders the rule as the statement of an nft "add rule" command,
// without the table and chain. The rule must be valid.
func (r firewallRule) nftExpr() string {
	var parts []string
	switch r.family {
	case firewallFamilyIPv4:
		parts = append(parts, "meta nfproto ipv4")
	case firewallFamilyIPv6:
		parts = append(parts, "meta nfproto ipv6")
	}
	if r.inIface != "" {
		parts = append(parts, fmt.Sprintf("iifname %q", r.inIface))
	}
	if r.outIface != "" {
		parts = append(parts, fmt.Sprintf("oifname %q", r.outIface))
	}
	ipProto := "ip"
	if r.family == firewallFamilyIPv6 {
		ipProto = "ip6"
	}
	if r.source.IsValid() {
		parts = append(parts, ipProto+" saddr "+formatFirewallAddress(r.source))
	}
	if r.destination.IsValid() {
		parts = append(parts, ipProto+" daddr "+formatFirewallAddress(r.destination))
	}
	switch r.protocol {
	case firewallProtoAll:
	case firewallProtoICMPv6:
		parts = append(parts, "meta l4proto ipv6-icmp")
	default:
		parts = append(parts, "meta l4proto "+r.protocol)
	}
	if len(r.sourcePorts) > 0 {
		parts = append(parts, r.protocol+" sport { "+strings.Join(portStrings(r.sourcePorts), ", ")+" }")
	}
	if len(r.destPorts) > 0 {
		parts = append(parts, r.protocol+" dport { "+strings.Join(portStrings(r.destPorts), ", ")+" }")
	}
	parts = append(parts, r.action, fmt.Sprintf("comment %q", r.tag()))
	return strings.Join(parts, " ")
}

// nftChainDecl returns the nft command that creates the provider's base
// chain for chain if it does not exist.
func nftChainDecl(chain string) string {
	return fmt.Sprintf("add chain inet %s %s { type filter hook %s priority 0; policy accept; }", nftTable, chain, chain)
}

// nftListedRule is a rule of "nft -j list table".
type nftListedRule struct {
	Family  string            `json:"family"`
	Table   string            `json:"table"`
	Chain   string            `json:"chain"`
	Handle  uint64            `json:"handle"`
	Comment string            `json:"comment"`
	Expr    []json.RawMessage `json:"expr"`
}

// nftListing is the output of "nft -j list ...".
type nftListing struct {
	Nftables []struct {
		Table *struct {
			Family string `json:"family"`
			Name   string `json:"name"`
		} `json:"table"`
		Rule *nftListedRule `json:"rule"`
	} `json:"nftables"`
}

func parseNftListing(data []byte) (*nftListing, error) {
	var l nftListing
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("parsing the JSON output of nft: %w", err)
	}
	return &l, nil
}

// hasNftTable reports whether the listing of "nft -j list tables" includes
// the provider's table.
func (l *nftListing) hasNftTable() bool {
	for _, o := range l.Nftables {
		if o.Table != nil && o.Table.Family == "inet" && o.Table.Name == nftTable {
			return true
		}
	}
	return false
}

// rules returns the rules of the listing.
func (l *nftListing) rules() []nftListedRule {
	var rules []nftListedRule
	for _, o := range l.Nftables {
		if o.Rule != nil {
			rules = append(rules, *o.Rule)
		}
	}
	return rules
}

// parseNftRule converts a rule listed by "nft -j" back into a firewallRule.
// It fails for expressions the provider does not add, such as counters or
// negated matches, so that such rules are reported rather than silently
// misread.
func parseNftRule(lr nftListedRule) (firewallRule, error) {
	r := firewallRule{chain: lr.Chain, protocol: firewallProtoAll}
	name, comment, ok := parseFirewallTag(lr.Comment)
	if !ok {
		return r, fmt.Errorf("rule comment %q is not a tag of this provider", lr.Comment)
	}
	r.name, r.comment = name, comment
	if !slices.Contains(firewallChains, lr.Chain) {
		return r, fmt.Errorf("unexpected chain %q", lr.Chain)
	}

	var nfproto, impliedFamily string
	imply := func(f string) error {
		if impliedFamily != "" && impliedFamily != f {
			return errors.New("the rule matches both IPv4 and IPv6 headers")
		}
		impliedFamily = f
		return nil
	}
	setProto := func(p string) error {
		if r.protocol != firewallProtoAll && r.protocol != p {
			return fmt.Errorf("the rule matches both protocol %s and %s", r.protocol, p)
		}
		r.protocol = p
		return nil
	}
	verdicts := 0
	for _, raw := range lr.Expr {
		var e map[string]json.RawMessage
		if err := json.Unmarshal(raw, &e); err != nil || len(e) != 1 {
			return r, fmt.Errorf("unsupported expression %s", raw)
		}
		var key string
		var val json.RawMessage
		for key, val = range e {
		}
		switch key {
		case "accept", "drop":
			if string(val) != "null" {
				return r, fmt.Errorf("unsupported expression %s", raw)
			}
			r.action = key
			verdicts++
		case "reject":
			if !nftDefaultReject(val) {
				return r, fmt.Errorf("unsupported reject type %s", val)
			}
			r.action = firewallActionReject
			verdicts++
		case "match":
			var m struct {
				Op    string          `json:"op"`
				Left  json.RawMessage `json:"left"`
				Right json.RawMessage `json:"right"`
			}
			if err := json.Unmarshal(val, &m); err != nil {
				return r, fmt.Errorf("unsupported expression %s", raw)
			}
			if m.Op != "==" && m.Op != "in" {
				return r, fmt.Errorf("unsupported match operator %q in %s", m.Op, raw)
			}
			var left struct {
				Meta *struct {
					Key string `json:"key"`
				} `json:"meta"`
				Payload *struct {
					Protocol string `json:"protocol"`
					Field    string `json:"field"`
				} `json:"payload"`
			}
			if err := json.Unmarshal(m.Left, &left); err != nil {
				return r, fmt.Errorf("unsupported expression %s", raw)
			}
			switch {
			case left.Meta != nil:
				var s string
				switch left.Meta.Key {
				case "nfproto":
					if json.Unmarshal(m.Right, &s) != nil || (s != "ipv4" && s != "ipv6") {
						return r, fmt.Errorf("unsupported match %s", raw)
					}
					nfproto = s
				case "l4proto":
					p, fam, err := nftProtocol(m.Right)
					if err != nil {
						return r, err
					}
					if err := setProto(p); err != nil {
						return r, err
					}
					if fam != "" {
						if err := imply(fam); err != nil {
							return r, err
						}
					}
				case "iifname", "oifname":
					if json.Unmarshal(m.Right, &s) != nil || validateInterfaceName(s) != nil {
						return r, fmt.Errorf("unsupported match %s", raw)
					}
					if left.Meta.Key == "iifname" {
						r.inIface = s
					} else {
						r.outIface = s
					}
				default:
					return r, fmt.Errorf("unsupported match %s", raw)
				}
			case left.Payload != nil:
				proto, field := left.Payload.Protocol, left.Payload.Field
				switch {
				case (proto == "ip" || proto == "ip6") && (field == "saddr" || field == "daddr"):
					p, err := nftPrefix(m.Right)
					if err != nil {
						return r, fmt.Errorf("unsupported match %s: %w", raw, err)
					}
					if p.Addr().Is4() != (proto == "ip") {
						return r, fmt.Errorf("unsupported match %s", raw)
					}
					if err := imply(map[string]string{"ip": firewallFamilyIPv4, "ip6": firewallFamilyIPv6}[proto]); err != nil {
						return r, err
					}
					if field == "saddr" {
						r.source = p
					} else {
						r.destination = p
					}
				case hasPorts(proto) && (field == "sport" || field == "dport"):
					ports, err := nftPorts(m.Right)
					if err != nil {
						return r, fmt.Errorf("unsupported match %s: %w", raw, err)
					}
					if err := setProto(proto); err != nil {
						return r, err
					}
					if field == "sport" {
						r.sourcePorts = ports
					} else {
						r.destPorts = ports
					}
				default:
					return r, fmt.Errorf("unsupported match %s", raw)
				}
			default:
				return r, fmt.Errorf("unsupported match %s", raw)
			}
		default:
			return r, fmt.Errorf("unsupported expression %s", raw)
		}
	}
	if verdicts != 1 {
		return r, errors.New("the rule does not end in exactly one accept, drop or reject")
	}
	switch {
	case nfproto != "" && impliedFamily != "" && nfproto != impliedFamily:
		return r, fmt.Errorf("the rule matches family %s and %s", nfproto, impliedFamily)
	case nfproto != "":
		r.family = nfproto
	case impliedFamily != "":
		// nft omits "meta nfproto" from listings when another match implies
		// it, such as "ip saddr" or "meta l4proto icmp".
		r.family = impliedFamily
	default:
		r.family = firewallFamilyInet
	}
	sortPorts(r.sourcePorts)
	sortPorts(r.destPorts)
	if err := r.validate(); err != nil {
		return r, err
	}
	return r, nil
}

// nftDefaultReject reports whether val is the argument of a plain "reject":
// null, or the port-unreachable ICMP type nft fills in.
func nftDefaultReject(val json.RawMessage) bool {
	if string(val) == "null" {
		return true
	}
	var rj struct {
		Type string `json:"type"`
		Expr string `json:"expr"`
	}
	if json.Unmarshal(val, &rj) != nil {
		return false
	}
	return (rj.Type == "icmpx" || rj.Type == "icmp" || rj.Type == "icmpv6") && (rj.Expr == "" || rj.Expr == "port-unreachable")
}

// nftProtocol parses the right-hand side of a "meta l4proto" match, which
// nft prints as a name or, for protocols without one, a number. It returns
// the family ICMP and ICMPv6 imply.
func nftProtocol(right json.RawMessage) (proto, family string, err error) {
	var s string
	if json.Unmarshal(right, &s) != nil {
		var n int
		if json.Unmarshal(right, &n) != nil {
			return "", "", fmt.Errorf("unsupported protocol %s", right)
		}
		s = strconv.Itoa(n)
	}
	switch s {
	case "tcp", "6":
		return firewallProtoTCP, "", nil
	case "udp", "17":
		return firewallProtoUDP, "", nil
	case "sctp", "132":
		return firewallProtoSCTP, "", nil
	case "icmp", "1":
		return firewallProtoICMP, firewallFamilyIPv4, nil
	case "ipv6-icmp", "icmpv6", "58":
		return firewallProtoICMPv6, firewallFamilyIPv6, nil
	}
	return "", "", fmt.Errorf("unsupported protocol %s", right)
}

// nftPrefix parses an address match: "10.0.0.1" or
// {"prefix": {"addr": "10.0.0.0", "len": 8}}.
func nftPrefix(right json.RawMessage) (netip.Prefix, error) {
	var s string
	if json.Unmarshal(right, &s) == nil {
		return parseFirewallAddress(s)
	}
	var p struct {
		Prefix *struct {
			Addr string `json:"addr"`
			Len  int    `json:"len"`
		} `json:"prefix"`
	}
	if json.Unmarshal(right, &p) != nil || p.Prefix == nil {
		return netip.Prefix{}, errors.New("not an address or prefix")
	}
	return parseFirewallAddress(p.Prefix.Addr + "/" + strconv.Itoa(p.Prefix.Len))
}

// nftPorts parses a port match: a number, {"range": [lo, hi]}, or
// {"set": [...]} of those.
func nftPorts(right json.RawMessage) ([]portRange, error) {
	one := func(raw json.RawMessage) (portRange, error) {
		var n uint16
		if json.Unmarshal(raw, &n) == nil && n > 0 {
			return portRange{n, n}, nil
		}
		var rg struct {
			Range []uint16 `json:"range"`
		}
		if json.Unmarshal(raw, &rg) == nil && len(rg.Range) == 2 && rg.Range[0] > 0 && rg.Range[0] <= rg.Range[1] {
			return portRange{rg.Range[0], rg.Range[1]}, nil
		}
		return portRange{}, fmt.Errorf("unsupported port %s", raw)
	}
	var set struct {
		Set []json.RawMessage `json:"set"`
	}
	if json.Unmarshal(right, &set) == nil && set.Set != nil {
		ports := make([]portRange, 0, len(set.Set))
		for _, e := range set.Set {
			p, err := one(e)
			if err != nil {
				return nil, err
			}
			ports = append(ports, p)
		}
		return ports, nil
	}
	p, err := one(right)
	if err != nil {
		return nil, err
	}
	return []portRange{p}, nil
}

// ---------------------------------------------------------------------------
// iptables

// iptablesChain returns the name of the built-in chain of iptables.
func iptablesChain(chain string) string { return strings.ToUpper(chain) }

// iptablesArgs renders the rule, which must be valid and not of family
// inet, as the arguments that follow "-A <chain>" (or -C or -D), in the
// order in which "iptables -S" prints them.
func (r firewallRule) iptablesArgs() []string {
	var args []string
	if r.source.IsValid() {
		args = append(args, "-s", r.source.String())
	}
	if r.destination.IsValid() {
		args = append(args, "-d", r.destination.String())
	}
	if r.inIface != "" {
		args = append(args, "-i", r.inIface)
	}
	if r.outIface != "" {
		args = append(args, "-o", r.outIface)
	}
	switch r.protocol {
	case firewallProtoAll:
	case firewallProtoICMPv6:
		args = append(args, "-p", "ipv6-icmp")
	default:
		args = append(args, "-p", r.protocol)
	}
	if len(r.sourcePorts) > 0 {
		args = append(args, "-m", "multiport", "--sports", iptablesPorts(r.sourcePorts))
	}
	if len(r.destPorts) > 0 {
		args = append(args, "-m", "multiport", "--dports", iptablesPorts(r.destPorts))
	}
	args = append(args, "-m", "comment", "--comment", r.tag(), "-j", strings.ToUpper(r.action))
	return args
}

func iptablesPorts(ports []portRange) string {
	s := make([]string, len(ports))
	for i, p := range ports {
		s[i] = strings.Replace(p.String(), "-", ":", 1)
	}
	return strings.Join(s, ",")
}

// splitIptablesLine splits a line of "iptables -S" into words. Words that
// contain white space or quotes are enclosed in double quotes, inside
// which a backslash escapes the next character.
func splitIptablesLine(line string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord, inQuote := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inQuote && c == '\\':
			if i+1 == len(line) {
				return nil, errors.New("unterminated escape")
			}
			i++
			cur.WriteByte(line[i])
		case c == '"':
			inQuote = !inQuote
			inWord = true
		case !inQuote && (c == ' ' || c == '\t'):
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inQuote {
		return nil, errors.New("unterminated quote")
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}

// iptablesComment returns the value of the comment match of a rule's words.
func iptablesComment(words []string) (string, bool) {
	for i := 0; i+1 < len(words); i++ {
		if words[i] == "--comment" {
			return words[i+1], true
		}
	}
	return "", false
}

// parseIptablesRule converts the words of an "iptables -S" line, starting
// with "-A", back into a firewallRule of the given family. Like
// parseNftRule, it fails for anything the provider does not add.
func parseIptablesRule(family string, words []string) (firewallRule, error) {
	r := firewallRule{family: family, protocol: firewallProtoAll}
	if len(words) < 2 || words[0] != "-A" {
		return r, errors.New("not a rule")
	}
	r.chain = strings.ToLower(words[1])
	if !slices.Contains(firewallChains, r.chain) {
		return r, fmt.Errorf("unexpected chain %q", words[1])
	}
	arg := func(i int) (string, error) {
		if i+1 >= len(words) {
			return "", fmt.Errorf("%s without a value", words[i])
		}
		return words[i+1], nil
	}
	tagged := false
	for i := 2; i < len(words); i += 2 {
		v, err := arg(i)
		if err != nil {
			return r, err
		}
		switch words[i] {
		case "-s", "-d":
			p, err := netip.ParsePrefix(v)
			if err != nil {
				return r, fmt.Errorf("unsupported address %q", v)
			}
			if p, err = parseFirewallAddress(formatFirewallAddress(p)); err != nil {
				return r, err
			}
			if words[i] == "-s" {
				r.source = p
			} else {
				r.destination = p
			}
		case "-i":
			r.inIface = v
		case "-o":
			r.outIface = v
		case "-p":
			switch v {
			case "tcp", "6":
				r.protocol = firewallProtoTCP
			case "udp", "17":
				r.protocol = firewallProtoUDP
			case "sctp", "132":
				r.protocol = firewallProtoSCTP
			case "icmp", "1":
				r.protocol = firewallProtoICMP
			case "ipv6-icmp", "icmpv6", "58":
				r.protocol = firewallProtoICMPv6
			default:
				return r, fmt.Errorf("unsupported protocol %q", v)
			}
		case "-m":
			switch v {
			case "multiport", "comment", "tcp", "udp", "sctp":
			default:
				return r, fmt.Errorf("unsupported match %q", v)
			}
		case "--sports", "--dports", "--sport", "--dport", "--source-ports", "--destination-ports":
			var ports []portRange
			for _, s := range strings.Split(v, ",") {
				p, err := parsePort(strings.Replace(s, ":", "-", 1))
				if err != nil {
					return r, err
				}
				ports = append(ports, p)
			}
			sortPorts(ports)
			if strings.HasPrefix(words[i], "--s") {
				r.sourcePorts = ports
			} else {
				r.destPorts = ports
			}
		case "--comment":
			name, comment, ok := parseFirewallTag(v)
			if !ok {
				return r, fmt.Errorf("rule comment %q is not a tag of this provider", v)
			}
			r.name, r.comment, tagged = name, comment, true
		case "-j":
			switch v {
			case "ACCEPT", "DROP", "REJECT":
				r.action = strings.ToLower(v)
			default:
				return r, fmt.Errorf("unsupported target %q", v)
			}
		case "--reject-with":
			if v != "icmp-port-unreachable" && v != "icmp6-port-unreachable" {
				return r, fmt.Errorf("unsupported reject type %q", v)
			}
		default:
			return r, fmt.Errorf("unsupported option %q", words[i])
		}
	}
	if !tagged {
		return r, errors.New("the rule has no comment")
	}
	if r.action == "" {
		return r, errors.New("the rule has no target")
	}
	if err := r.validate(); err != nil {
		return r, err
	}
	return r, nil
}
