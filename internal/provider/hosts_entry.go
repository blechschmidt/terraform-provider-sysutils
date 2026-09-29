package provider

// Logic behind sysutils_hosts_entry: parsing and rendering lines of a
// hosts(5) file and editing the file's lines.
//
// A line of the file is an entry if, after an optional comment starting at
// the first "#" is cut off, it holds an IP address followed by at least one
// hostname, separated by blanks. Anything else (blank lines, comments, lines
// with an unparsable address, lines with an address but no hostname) is not
// an entry and is kept byte for byte, as are all entries the resource does
// not manage.
//
// The managed entry is the line that maps the resource's IP address, with
// its first hostname (the canonical name) equal to the resource's, ignoring
// case. Matching by the address alone would let a resource for 127.0.0.1
// take over the distribution's "127.0.0.1 localhost" line, which may well
// share the address. Addresses are compared as addresses, so "::1" in the
// configuration matches "0:0:0:0:0:0:0:1" in the file.

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

const (
	// hostsFileMode is the mode of a hosts file created because it did not
	// exist; an existing file keeps its own.
	hostsFileMode = 0o644

	// maxHostnameLength is the longest hostname RFC 1123 allows, without a
	// trailing dot.
	maxHostnameLength = 253
	maxLabelLength    = 63
)

// parseHostsIP parses the ip attribute: an IPv4 address in dotted-decimal
// form or an IPv6 address, without a zone, which hosts files cannot hold.
func parseHostsIP(s string) (netip.Addr, error) {
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("%q is not an IPv4 or IPv6 address, such as \"10.0.0.5\" or \"fd00::5\"", s)
	}
	if addr.Zone() != "" {
		return netip.Addr{}, fmt.Errorf("IP address %q must not have a zone (%%%s); hosts files cannot hold one", s, addr.Zone())
	}
	return addr, nil
}

// validateHostsIP reports why s is not a usable value of ip.
func validateHostsIP(s string) error {
	_, err := parseHostsIP(s)
	return err
}

// validateHostname reports why s is not a hostname as defined by RFC 1123:
// dot-separated labels of 1 to 63 letters, digits and hyphens that neither
// start nor end with a hyphen, at most 253 characters in all. A trailing
// dot, an underscore and anything that reads as an IP address are refused.
func validateHostname(s string) error {
	if s == "" {
		return errors.New("hostname must not be empty")
	}
	if len(s) > maxHostnameLength {
		return fmt.Errorf("hostname %q is longer than %d characters", s, maxHostnameLength)
	}
	if _, err := netip.ParseAddr(s); err == nil {
		return fmt.Errorf("hostname %q is an IP address", s)
	}
	for label := range strings.SplitSeq(s, ".") {
		if err := validateHostnameLabel(label); err != nil {
			return fmt.Errorf("hostname %q is not valid: %w", s, err)
		}
	}
	return nil
}

func validateHostnameLabel(label string) error {
	switch {
	case label == "":
		return errors.New("it has an empty label (it must not start or end with a dot or contain two dots in a row)")
	case len(label) > maxLabelLength:
		return fmt.Errorf("label %q is longer than %d characters", label, maxLabelLength)
	case label[0] == '-' || label[len(label)-1] == '-':
		return fmt.Errorf("label %q starts or ends with a hyphen", label)
	}
	for _, c := range []byte(label) {
		if !isHostnameByte(c) {
			return fmt.Errorf("label %q contains %q; only letters, digits and hyphens are allowed", label, c)
		}
	}
	return nil
}

// isHostnameByte reports whether c may appear in a hostname label: an ASCII
// letter or digit, or a hyphen.
func isHostnameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-'
}

// validateHostsComment reports why s cannot be written as the comment of a
// hosts entry and read back unchanged.
func validateHostsComment(s string) error {
	if s == "" {
		return errors.New("comment must not be empty; leave it unset for no comment")
	}
	if strings.ContainsAny(s, "\r\n\x00") {
		return errors.New("comment must be a single line")
	}
	if strings.TrimSpace(s) != s {
		return errors.New("comment must not start or end with white space")
	}
	return nil
}

// duplicateHostname returns the first hostname in names that occurs earlier
// in names, ignoring case, as resolvers do.
func duplicateHostname(names []string) (string, bool) {
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		k := strings.ToLower(n)
		if seen[k] {
			return n, true
		}
		seen[k] = true
	}
	return "", false
}

// isHostsBlank reports whether c separates the fields of a hosts entry.
func isHostsBlank(c byte) bool { return c == ' ' || c == '\t' }

// hostsLine is a parsed entry. The white space around the fields is kept so
// that an entry can be changed without reformatting it.
type hostsLine struct {
	// indent is the white space before the address.
	indent string
	// ipText is the address as written; addr is its value.
	ipText string
	addr   netip.Addr
	// ipSep is the white space after the address.
	ipSep string
	names []string
	// nameSep is the white space between the first two names; empty if
	// there is only one.
	nameSep string
	// hasComment is set if the line has a "#". commentSep is the white
	// space before it and commentRaw everything after it; comment is
	// commentRaw without surrounding white space.
	hasComment bool
	commentSep string
	commentRaw string
	comment    string
	// cr is set if the line ends with a carriage return (CRLF line
	// endings).
	cr bool
}

// parseHostsLine parses one line of a hosts file. It reports ok=false for
// lines that are not entries.
func parseHostsLine(line string) (hl hostsLine, ok bool) {
	if s, found := strings.CutSuffix(line, "\r"); found {
		line, hl.cr = s, true
	}
	content := line
	if i := strings.IndexByte(line, '#'); i >= 0 {
		content = line[:i]
		hl.hasComment = true
		hl.commentRaw = line[i+1:]
		hl.comment = strings.TrimSpace(hl.commentRaw)
	}
	fields, seps := splitHostsFields(content)
	if len(fields) < 2 {
		return hostsLine{}, false
	}
	addr, err := netip.ParseAddr(fields[0])
	if err != nil || addr.Zone() != "" {
		return hostsLine{}, false
	}
	hl.indent, hl.ipText, hl.addr, hl.ipSep = seps[0], fields[0], addr, seps[1]
	hl.names = fields[1:]
	if len(fields) > 2 {
		hl.nameSep = seps[2]
	}
	if hl.hasComment {
		hl.commentSep = seps[len(seps)-1]
	}
	return hl, true
}

// splitHostsFields splits s into its blank-separated fields. seps[i] is the
// white space before fields[i], and seps[len(fields)] the white space after
// the last field.
func splitHostsFields(s string) (fields, seps []string) {
	i := 0
	for {
		start := i
		for i < len(s) && isHostsBlank(s[i]) {
			i++
		}
		seps = append(seps, s[start:i])
		if i == len(s) {
			return fields, seps
		}
		start = i
		for i < len(s) && !isHostsBlank(s[i]) {
			i++
		}
		fields = append(fields, s[start:i])
	}
}

// canonical returns the entry's canonical name, its first hostname.
func (hl *hostsLine) canonical() string { return hl.names[0] }

// render returns the line for hl, including a trailing carriage return if
// it had one.
func (hl *hostsLine) render() string {
	var b strings.Builder
	b.WriteString(hl.indent)
	b.WriteString(hl.ipText)
	b.WriteString(hl.ipSep)
	nameSep := hl.nameSep
	if nameSep == "" {
		nameSep = " "
	}
	b.WriteString(strings.Join(hl.names, nameSep))
	if hl.hasComment {
		b.WriteString(hl.commentSep)
		b.WriteByte('#')
		b.WriteString(hl.commentRaw)
	}
	if hl.cr {
		b.WriteByte('\r')
	}
	return b.String()
}

// hostsEntry is the desired state of a managed entry.
type hostsEntry struct {
	ipText string
	addr   netip.Addr
	names  []string
	// comment is empty for none.
	comment string
}

// line returns the entry as a new line, with ipSep between the address and
// the first hostname.
func (e *hostsEntry) line(ipSep string) string {
	hl := hostsLine{ipText: e.ipText, addr: e.addr, ipSep: ipSep, names: e.names}
	e.setComment(&hl)
	return hl.render()
}

// setComment gives hl the entry's comment, keeping hl's own spelling of it
// if the comment is unchanged.
func (e *hostsEntry) setComment(hl *hostsLine) {
	switch {
	case e.comment == "":
		hl.hasComment, hl.commentSep, hl.commentRaw, hl.comment = false, "", "", ""
	case !hl.hasComment || hl.comment != e.comment:
		hl.hasComment, hl.commentRaw, hl.comment = true, " "+e.comment, e.comment
		if hl.commentSep == "" {
			hl.commentSep = " "
		}
	}
}

// hostsIdentity identifies a managed entry: its address and canonical name.
type hostsIdentity struct {
	addr      netip.Addr
	canonical string
}

func (id hostsIdentity) matches(hl *hostsLine) bool {
	return hl.addr == id.addr && strings.EqualFold(hl.canonical(), id.canonical)
}

// findHostsEntries returns the indices of the lines that hold an entry with
// any of the given identities, in file order.
func findHostsEntries(lines []string, ids ...hostsIdentity) []int {
	var indices []int
	for i, l := range lines {
		hl, ok := parseHostsLine(l)
		if !ok {
			continue
		}
		if slices.ContainsFunc(ids, func(id hostsIdentity) bool { return id.matches(&hl) }) {
			indices = append(indices, i)
		}
	}
	return indices
}

// findHostsAddr returns the indices of the entries for addr, whatever their
// hostnames. Used on import, when only the address may be known.
func findHostsAddr(lines []string, addr netip.Addr) []int {
	var indices []int
	for i, l := range lines {
		if hl, ok := parseHostsLine(l); ok && hl.addr == addr {
			indices = append(indices, i)
		}
	}
	return indices
}

// hostsConflict is a hostname that another entry maps to another address.
type hostsConflict struct {
	// line is 1-based.
	line   int
	name   string
	ipText string
}

func (c hostsConflict) String() string {
	return fmt.Sprintf("%q is mapped to %s on line %d", c.name, c.ipText, c.line)
}

// hostsConflicts returns the hostnames in names, compared without regard to
// case, that an entry other than the lines at skip maps to an address other
// than addr of the same family. A name with both an IPv4 and an IPv6
// address, such as localhost with 127.0.0.1 and ::1, is the normal dual-stack
// case and not a conflict.
func hostsConflicts(lines []string, addr netip.Addr, names []string, skip []int) []hostsConflict {
	var out []hostsConflict
	for i, l := range lines {
		if slices.Contains(skip, i) {
			continue
		}
		hl, ok := parseHostsLine(l)
		if !ok || hl.addr == addr || hl.addr.Is4() != addr.Is4() {
			continue
		}
		for _, n := range hl.names {
			if slices.ContainsFunc(names, func(want string) bool { return strings.EqualFold(want, n) }) {
				out = append(out, hostsConflict{line: i + 1, name: n, ipText: hl.ipText})
			}
		}
	}
	return out
}

// hostsFieldSeparator returns the white space to put between the address
// and the first hostname of a new entry: the same as in the file's first
// entry, or a tab, as in most distributions' default hosts files.
func hostsFieldSeparator(lines []string) string {
	for _, l := range lines {
		if hl, ok := parseHostsLine(l); ok {
			return hl.ipSep
		}
	}
	return "\t"
}

// ensureHostsEntry makes the lines at indices, which hold the managed entry
// (see findHostsEntries), a single line for e, and reports whether t
// changed. The first of them is updated in place, keeping its indentation,
// spacing, the spelling of an unchanged address and of an unchanged comment;
// the others are removed, so that no stale copy of the entry remains. If
// indices is empty, e is appended to the end of the file.
func ensureHostsEntry(t *textFile, indices []int, e *hostsEntry) bool {
	if len(indices) == 0 {
		line := e.line(hostsFieldSeparator(t.lines))
		if usesCRLF(t.lines) {
			line += "\r"
			// The old last line gets a newline; it needs the CR in front
			// of it.
			if last := len(t.lines) - 1; !t.trailingNewline && !strings.HasSuffix(t.lines[last], "\r") {
				t.lines[last] += "\r"
			}
		}
		t.insert(len(t.lines), []string{line})
		return true
	}
	changed := false
	for i := len(indices) - 1; i >= 1; i-- {
		t.replace(indices[i], indices[i]+1, nil)
		changed = true
	}
	idx := indices[0]
	hl, ok := parseHostsLine(t.lines[idx])
	if !ok {
		// Callers pass indices of entries only.
		panic(fmt.Sprintf("line %d is not a hosts entry", idx+1))
	}
	if hl.addr != e.addr {
		hl.ipText, hl.addr = e.ipText, e.addr
	}
	hl.names = e.names
	if len(e.names) > 1 && hl.nameSep == "" {
		hl.nameSep = " "
	}
	e.setComment(&hl)
	if line := hl.render(); line != t.lines[idx] {
		t.lines[idx] = line
		changed = true
	}
	return changed
}

// removeHostsEntries deletes the lines at indices from t and reports whether
// t changed.
func removeHostsEntries(t *textFile, indices []int) bool {
	for i := len(indices) - 1; i >= 0; i-- {
		t.replace(indices[i], indices[i]+1, nil)
	}
	return len(indices) > 0
}
