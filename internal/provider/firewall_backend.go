package provider

// The commands behind sysutils_firewall_rule. nftablesBackend keeps the
// provider's rules in its own table and changes it with nft scripts, which
// nft applies atomically. iptablesBackend adds rules to the built-in chains
// of the filter table of iptables and ip6tables. Both run their commands
// through a commandRunner, with an argument vector and never through a
// shell, so that unit tests can substitute a fake and acceptance tests can
// run the commands inside a network namespace.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	// firewallCommandTimeout bounds each nft or iptables invocation.
	firewallCommandTimeout = 2 * time.Minute
	// firewallOutputLimit caps the output kept of a listing. Hosts running
	// container engines can have thousands of rules.
	firewallOutputLimit = 32 << 20
)

// firewallConfig is the provider-level configuration of
// sysutils_firewall_rule. The zero value, or a nil pointer, selects the real
// nft, iptables and ip6tables.
type firewallConfig struct {
	// run runs the commands; nil selects runCommand.
	run commandRunner
	// lookPath finds the executables; nil selects exec.LookPath.
	lookPath func(string) (string, error)
}

func (c *firewallConfig) runner() commandRunner {
	if c == nil || c.run == nil {
		return runCommand
	}
	return c.run
}

func (c *firewallConfig) has(tool string) bool {
	lookPath := exec.LookPath
	if c != nil && c.lookPath != nil {
		lookPath = c.lookPath
	}
	_, err := lookPath(tool)
	return err == nil
}

// backend returns the backend of the given kind, which must not be auto.
func (c *firewallConfig) backend(kind string) (firewallBackend, error) {
	switch kind {
	case firewallBackendNftables:
		if !c.has("nft") {
			return nil, errors.New("the nftables backend requires the nft command, which was not found")
		}
		return &nftablesBackend{run: c.runner()}, nil
	case firewallBackendIptables:
		b := &iptablesBackend{run: c.runner(), v4: c.has("iptables"), v6: c.has("ip6tables")}
		if !b.v4 && !b.v6 {
			return nil, errors.New("the iptables backend requires the iptables or ip6tables command, neither of which was found")
		}
		return b, nil
	}
	return nil, fmt.Errorf("unknown firewall backend %q", kind)
}

// usable reports whether the backend kind can be used on this host: its
// commands exist and, for nftables, the kernel supports nf_tables.
func (c *firewallConfig) usable(ctx context.Context, kind string) bool {
	b, err := c.backend(kind)
	if err != nil {
		return false
	}
	if nb, ok := b.(*nftablesBackend); ok {
		_, err := nb.listTables(ctx)
		return err == nil
	}
	return true
}

// detect resolves backend = "auto": nftables if nft works, else iptables.
func (c *firewallConfig) detect(ctx context.Context) (string, error) {
	if c.usable(ctx, firewallBackendNftables) {
		return firewallBackendNftables, nil
	}
	if c.usable(ctx, firewallBackendIptables) {
		return firewallBackendIptables, nil
	}
	return "", errors.New("no firewall backend found: neither a working nft command nor iptables or ip6tables is available")
}

// firewallBackend finds, adds and removes the rules of one resource.
type firewallBackend interface {
	kind() string
	// find returns the rules tagged with name.
	find(ctx context.Context, name string) (*firewallFound, error)
	// apply makes the rules found by find equal to want, changing only
	// what differs. It reports whether it changed anything, even if it
	// failed part way.
	apply(ctx context.Context, want firewallRule, found *firewallFound) (bool, error)
	// remove deletes the rules found by find.
	remove(ctx context.Context, found *firewallFound) error
}

// firewallFound is the result of firewallBackend.find: one entry per rule
// tagged with the resource's name.
type firewallFound struct {
	rules []foundRule
	// otherRules counts the rules in the provider's nftables table that
	// belong to other resources or were added by hand.
	otherRules int
	// otherObjects counts the other objects in that table that the
	// provider did not create, such as sets or chains added by hand (see
	// nftForeignObjects).
	otherObjects int
}

// foundRule is a rule found by its tag.
type foundRule struct {
	// rule is the parsed rule; err is set if it could not be parsed.
	rule firewallRule
	err  error
	// text describes the rule for messages.
	text string
	// nftChain and nftHandle identify an nftables rule.
	nftChain  string
	nftHandle uint64
	// ipFamily and words identify an iptables rule: the words of its
	// "iptables -S" line, starting with "-A". ipPos is its 1-based position
	// in its chain when it was listed.
	ipFamily string
	words    []string
	ipPos    int
}

func (f *firewallFound) empty() bool { return f == nil || len(f.rules) == 0 }

// merged returns the single firewallRule the found rules implement. For
// iptables, a rule of family inet is an iptables and an ip6tables rule that
// are equal apart from the family. It fails if a rule could not be parsed or
// the rules do not form one firewallRule, for example because a copy was
// added by hand.
func (f *firewallFound) merged() (firewallRule, error) {
	var texts []string
	for _, fr := range f.rules {
		if fr.err != nil {
			return firewallRule{}, fmt.Errorf("the rule %s cannot be managed: %w", fr.text, fr.err)
		}
		texts = append(texts, fr.text)
	}
	switch len(f.rules) {
	case 1:
		return f.rules[0].rule, nil
	case 2:
		a, b := f.rules[0].rule, f.rules[1].rule
		if f.rules[0].ipFamily != "" && a.family != b.family && a.withFamily(firewallFamilyInet).equal(b.withFamily(firewallFamilyInet)) {
			if err := a.withFamily(firewallFamilyInet).validate(); err == nil {
				return a.withFamily(firewallFamilyInet), nil
			}
		}
	}
	return firewallRule{}, fmt.Errorf("%d rules carry the tag, which do not form a single rule: %s", len(f.rules), strings.Join(texts, "; "))
}

// runFirewallCommand runs argv in the C locale. It returns the result even
// for a non-zero exit status, and an error if the command could not be run,
// timed out or produced more output than is kept.
func runFirewallCommand(ctx context.Context, run commandRunner, stdin string, argv ...string) (*execResult, error) {
	res, err := run(ctx, execSpec{
		Argv:           argv,
		Env:            append(os.Environ(), "LC_ALL=C"),
		Stdin:          stdin,
		Timeout:        firewallCommandTimeout,
		MaxOutputBytes: firewallOutputLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
	}
	if res.TimedOut {
		return nil, fmt.Errorf("%s: timed out after %s", strings.Join(argv, " "), firewallCommandTimeout)
	}
	if res.Stdout.Truncated() {
		return nil, fmt.Errorf("%s: output exceeds %d bytes", strings.Join(argv, " "), firewallOutputLimit)
	}
	return res, nil
}

// firewallCommand runs argv and fails unless it exits with status 0.
func firewallCommand(ctx context.Context, run commandRunner, stdin string, argv ...string) (string, error) {
	res, err := runFirewallCommand(ctx, run, stdin, argv...)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", serviceCommandError(argv, res)
	}
	return res.Stdout.String(), nil
}

// ---------------------------------------------------------------------------
// nftables

type nftablesBackend struct {
	run commandRunner
}

func (b *nftablesBackend) kind() string { return firewallBackendNftables }

func (b *nftablesBackend) listTables(ctx context.Context) (*nftListing, error) {
	out, err := firewallCommand(ctx, b.run, "", "nft", "-j", "list", "tables")
	if err != nil {
		return nil, err
	}
	return parseNftListing([]byte(out))
}

func (b *nftablesBackend) find(ctx context.Context, name string) (*firewallFound, error) {
	tables, err := b.listTables(ctx)
	if err != nil {
		return nil, err
	}
	found := &firewallFound{}
	if !tables.hasNftTable() {
		return found, nil
	}
	out, err := firewallCommand(ctx, b.run, "", "nft", "-j", "list", "table", "inet", nftTable)
	if err != nil {
		return nil, err
	}
	listing, err := parseNftListing([]byte(out))
	if err != nil {
		return nil, err
	}
	if found.otherObjects, err = nftForeignObjects([]byte(out)); err != nil {
		return nil, err
	}
	for _, lr := range listing.rules() {
		if n, _, ok := parseFirewallTag(lr.Comment); !ok || n != name {
			found.otherRules++
			continue
		}
		fr := foundRule{nftChain: lr.Chain, nftHandle: lr.Handle,
			text: fmt.Sprintf("in chain %s of table inet %s with handle %d", lr.Chain, nftTable, lr.Handle)}
		fr.rule, fr.err = parseNftRule(lr)
		found.rules = append(found.rules, fr)
	}
	return found, nil
}

// deleteStatement returns the nft command that deletes fr.
func (fr foundRule) nftDeleteStatement() (string, error) {
	if !nftIdentPattern.MatchString(fr.nftChain) {
		return "", fmt.Errorf("unexpected chain name %q", fr.nftChain)
	}
	return fmt.Sprintf("delete rule inet %s %s handle %d", nftTable, fr.nftChain, fr.nftHandle), nil
}

func (b *nftablesBackend) apply(ctx context.Context, want firewallRule, found *firewallFound) (bool, error) {
	if err := want.validate(); err != nil {
		return false, err
	}
	if want.family != firewallFamilyInet && want.family != firewallFamilyIPv4 && want.family != firewallFamilyIPv6 {
		return false, fmt.Errorf("unsupported family %q", want.family)
	}
	if !found.empty() && len(found.rules) == 1 && found.rules[0].err == nil && found.rules[0].rule.equal(want) {
		return false, nil
	}
	script := []string{"add table inet " + nftTable, nftChainDecl(want.chain)}
	replaced := false
	if !found.empty() {
		for i, fr := range found.rules {
			// Replace a rule in the same chain in place, so that it keeps
			// its position; delete the others.
			if i == 0 && fr.nftChain == want.chain {
				script = append(script, fmt.Sprintf("replace rule inet %s %s handle %d %s", nftTable, want.chain, fr.nftHandle, want.nftExpr()))
				replaced = true
				continue
			}
			stmt, err := fr.nftDeleteStatement()
			if err != nil {
				return false, err
			}
			script = append(script, stmt)
		}
	}
	if !replaced {
		script = append(script, fmt.Sprintf("add rule inet %s %s %s", nftTable, want.chain, want.nftExpr()))
	}
	// nft applies a script as one transaction: all of it or nothing.
	if _, err := firewallCommand(ctx, b.run, strings.Join(script, "\n")+"\n", "nft", "-f", "-"); err != nil {
		return false, err
	}
	return true, nil
}

func (b *nftablesBackend) remove(ctx context.Context, found *firewallFound) error {
	if found.empty() {
		return nil
	}
	var script []string
	if found.otherRules == 0 && found.otherObjects == 0 {
		// The table holds nothing else: remove it, and its chains, as well.
		script = []string{"delete table inet " + nftTable}
	} else {
		for _, fr := range found.rules {
			stmt, err := fr.nftDeleteStatement()
			if err != nil {
				return err
			}
			script = append(script, stmt)
		}
	}
	_, err := firewallCommand(ctx, b.run, strings.Join(script, "\n")+"\n", "nft", "-f", "-")
	return err
}

// ---------------------------------------------------------------------------
// iptables

type iptablesBackend struct {
	run commandRunner
	// v4 and v6 record whether iptables and ip6tables exist.
	v4, v6 bool
}

func (b *iptablesBackend) kind() string { return firewallBackendIptables }

// command returns the command for family.
func iptablesCommand(family string) string {
	if family == firewallFamilyIPv6 {
		return "ip6tables"
	}
	return "iptables"
}

func (b *iptablesBackend) families() []string {
	var f []string
	if b.v4 {
		f = append(f, firewallFamilyIPv4)
	}
	if b.v6 {
		f = append(f, firewallFamilyIPv6)
	}
	return f
}

func (b *iptablesBackend) find(ctx context.Context, name string) (*firewallFound, error) {
	found := &firewallFound{}
	tag := firewallTagPrefix + name
	for _, family := range b.families() {
		// -w waits for the xtables lock held by another iptables command
		// rather than failing.
		out, err := firewallCommand(ctx, b.run, "", iptablesCommand(family), "-w", "-S")
		if err != nil {
			return nil, err
		}
		positions := map[string]int{}
		for _, line := range strings.Split(out, "\n") {
			if !strings.HasPrefix(line, "-A ") {
				continue
			}
			chain, _, _ := strings.Cut(strings.TrimPrefix(line, "-A "), " ")
			positions[chain]++
			if !strings.Contains(line, tag) {
				continue
			}
			fr := foundRule{ipFamily: family, ipPos: positions[chain], text: fmt.Sprintf("%q of %s", line, iptablesCommand(family))}
			words, err := splitIptablesLine(line)
			if err != nil {
				// A line that cannot be split cannot be deleted by
				// specification either.
				return nil, fmt.Errorf("parsing %s: %w", fr.text, err)
			}
			if c, ok := iptablesComment(words); !ok {
				continue
			} else if n, _, ok := parseFirewallTag(c); !ok || n != name {
				continue
			}
			fr.words = words
			fr.rule, fr.err = parseIptablesRule(family, words)
			found.rules = append(found.rules, fr)
		}
	}
	return found, nil
}

// wantFamilies returns the families of the rules that implement want.
func (b *iptablesBackend) wantFamilies(want firewallRule) ([]string, error) {
	switch want.family {
	case firewallFamilyIPv4, firewallFamilyIPv6:
		if (want.family == firewallFamilyIPv4 && !b.v4) || (want.family == firewallFamilyIPv6 && !b.v6) {
			return nil, fmt.Errorf("family %q requires the %s command, which was not found", want.family, iptablesCommand(want.family))
		}
		return []string{want.family}, nil
	case firewallFamilyInet:
		if !b.v4 || !b.v6 {
			return nil, errors.New("family \"inet\" requires both the iptables and the ip6tables command")
		}
		return []string{firewallFamilyIPv4, firewallFamilyIPv6}, nil
	}
	return nil, fmt.Errorf("unsupported family %q", want.family)
}

func (b *iptablesBackend) apply(ctx context.Context, want firewallRule, found *firewallFound) (bool, error) {
	if err := want.validate(); err != nil {
		return false, err
	}
	families, err := b.wantFamilies(want)
	if err != nil {
		return false, err
	}
	chain := iptablesChain(want.chain)
	have := map[string]bool{}
	// insertAt records, per family, the position of a rule that is replaced
	// in the same chain, so that its replacement takes its place.
	insertAt := map[string]int{}
	var stale []foundRule
	if !found.empty() {
		for _, fr := range found.rules {
			if fr.err == nil && !have[fr.ipFamily] && fr.rule.equal(want.withFamily(fr.ipFamily)) && (want.family == firewallFamilyInet || want.family == fr.ipFamily) {
				have[fr.ipFamily] = true
				continue
			}
			stale = append(stale, fr)
			if len(fr.words) > 1 && fr.words[1] == chain && insertAt[fr.ipFamily] == 0 {
				insertAt[fr.ipFamily] = fr.ipPos
			}
		}
	}
	changed := false
	for _, family := range families {
		if have[family] {
			continue
		}
		cmd, args := iptablesCommand(family), want.withFamily(family).iptablesArgs()
		// -C checks whether the rule exists already, as it does when an
		// earlier apply was interrupted.
		res, err := runFirewallCommand(ctx, b.run, "", append([]string{cmd, "-w", "-C", chain}, args...)...)
		if err != nil {
			return changed, err
		}
		if res.ExitCode == 0 {
			continue
		}
		// A replacement is inserted before the rule it replaces, which is
		// deleted below. Should another tool have changed the chain since
		// it was listed, the new rule merely lands at another position:
		// rules are only ever deleted by their full specification.
		add := []string{cmd, "-w", "-A", chain}
		if pos := insertAt[family]; pos > 0 {
			add = []string{cmd, "-w", "-I", chain, strconv.Itoa(pos)}
		}
		if _, err := firewallCommand(ctx, b.run, "", append(add, args...)...); err != nil {
			return changed, err
		}
		changed = true
	}
	// Delete what differs, by the specification iptables printed.
	for _, fr := range stale {
		if err := b.delete(ctx, fr); err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}

// delete deletes fr, passing the words iptables printed for it to -D.
func (b *iptablesBackend) delete(ctx context.Context, fr foundRule) error {
	if len(fr.words) < 2 || fr.words[0] != "-A" {
		return fmt.Errorf("cannot delete %s", fr.text)
	}
	argv := append([]string{iptablesCommand(fr.ipFamily), "-w", "-D"}, fr.words[1:]...)
	_, err := firewallCommand(ctx, b.run, "", argv...)
	return err
}

func (b *iptablesBackend) remove(ctx context.Context, found *firewallFound) error {
	if found.empty() {
		return nil
	}
	for _, fr := range found.rules {
		if err := b.delete(ctx, fr); err != nil {
			return err
		}
	}
	return nil
}
