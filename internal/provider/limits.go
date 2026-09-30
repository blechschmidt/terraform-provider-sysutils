package provider

// Logic behind sysutils_limits: validating pam_limits(8) entries, parsing
// and rendering lines of limits.conf(5) files, and editing drop-in files in
// /etc/security/limits.d.
//
// A line of such a file is "<domain> <type> <item> <value>", with fields
// separated by white space. As in pam_limits, a "#" anywhere starts a
// comment, and lines with fewer than four fields are ignored. An entry is
// identified by its domain, type and item; several resources can manage
// different entries of one file, and every other line is kept as written.

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
)

const (
	// defaultLimitsDir is the directory pam_limits reads drop-in files
	// from, below the provider's root_dir.
	defaultLimitsDir = "/etc/security/limits.d"
	// limitsFileMode is the mode of a drop-in file created by
	// sysutils_limits.
	limitsFileMode fs.FileMode = 0o644
	// maxLimitsFileSize bounds how much of a drop-in file is read.
	maxLimitsFileSize = 4 << 20
	// maxLimitsNameLen bounds user and group names in a domain, and the
	// file name.
	maxLimitsNameLen = 255
	// limitsFileHeader is the first line of a drop-in file created by
	// sysutils_limits. A file that holds nothing but this line and blank
	// lines after an entry is removed is deleted; a file without it is
	// never deleted, because the provider did not create it.
	limitsFileHeader = "# Managed by Terraform (sysutils_limits). Lines not managed by Terraform are kept."
)

// limitsTypes are the values of the type field. "-" sets both limits.
var limitsTypes = []string{"soft", "hard", "-"}

// limitsItemKind says which values an item accepts.
type limitsItemKind int

const (
	// limitsRlimit items are resource limits and logins counts: a
	// non-negative integer or unlimited.
	limitsRlimit limitsItemKind = iota
	// limitsPriority items are nice values, -20 to 19. They have no
	// unlimited.
	limitsPriority
	// limitsBoolean items are 0 or 1.
	limitsBoolean
)

// limitsItems are the items pam_limits understands, with the kind of value
// each accepts.
var limitsItems = map[string]limitsItemKind{
	"as":           limitsRlimit,
	"core":         limitsRlimit,
	"cpu":          limitsRlimit,
	"data":         limitsRlimit,
	"fsize":        limitsRlimit,
	"locks":        limitsRlimit,
	"maxlogins":    limitsRlimit,
	"maxsyslogins": limitsRlimit,
	"memlock":      limitsRlimit,
	"msgqueue":     limitsRlimit,
	"nofile":       limitsRlimit,
	"nproc":        limitsRlimit,
	"rss":          limitsRlimit,
	"rtprio":       limitsRlimit,
	"sigpending":   limitsRlimit,
	"stack":        limitsRlimit,
	"nice":         limitsPriority,
	"priority":     limitsPriority,
	"nonewprivs":   limitsBoolean,
}

// limitsItemNames returns the item names, sorted, for error messages and
// documentation.
func limitsItemNames() []string {
	names := make([]string, 0, len(limitsItems))
	for n := range limitsItems {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// limitsUnlimited are the spellings of "no limit" pam_limits accepts. They
// are all the same value.
var limitsUnlimited = []string{"unlimited", "infinity", "-1"}

func isLimitsUnlimited(v string) bool {
	for _, u := range limitsUnlimited {
		if v == u {
			return true
		}
	}
	return false
}

// limitsNameChar reports whether c may appear in a user or group name of a
// domain: the portable character set of POSIX user names.
func limitsNameChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '_' || c == '.' || c == '-'
}

// validateLimitsName reports why s cannot be the user or group name of a
// domain. what is "user" or "group".
func validateLimitsName(s, what string) error {
	name := strings.TrimSuffix(s, "$") // Samba machine accounts.
	switch {
	case name == "":
		return fmt.Errorf("%s name must not be empty", what)
	case len(s) > maxLimitsNameLen:
		return fmt.Errorf("%s name must be at most %d bytes long", what, maxLimitsNameLen)
	case name[0] == '-':
		return fmt.Errorf("%s name %q must not start with \"-\"", what, s)
	case name == "." || name == "..":
		return fmt.Errorf("%s name %q is not a valid name", what, s)
	}
	for i := 0; i < len(name); i++ {
		if !limitsNameChar(name[i]) {
			return fmt.Errorf("%s name %q contains the character %q; only letters, digits and \"_.-\" are allowed, and a final \"$\"", what, s, name[i])
		}
	}
	if _, err := strconv.ParseUint(name, 10, 64); err == nil {
		if what == "user" {
			return fmt.Errorf("user name %q is numeric; pam_limits compares it with user names, not UIDs. Use \":%s\" to match the UID %s", s, name, name)
		}
		return fmt.Errorf("group name %q is numeric; pam_limits compares it with group names, not GIDs. Use \"@:%s\" to match the GID %s", s, name, name)
	}
	return nil
}

// parseLimitsID parses a UID or GID of a range.
func parseLimitsID(s string) (uint64, error) {
	if s == "" || strings.TrimLeft(s, "0123456789") != "" {
		return 0, fmt.Errorf("%q is not a decimal number", s)
	}
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil || n == 1<<32-1 {
		return 0, fmt.Errorf("%s is out of range for an ID", s)
	}
	return n, nil
}

// validateLimitsRange reports why r, the part of a domain after an optional
// "@" or "%", is not a range as pam_limits parses it: "<min>:<max>",
// "<min>:" (min and above) or ":<id>" (exactly id).
func validateLimitsRange(r string) error {
	lo, hi, _ := strings.Cut(r, ":")
	if lo == "" && hi == "" {
		return errors.New("a range needs a minimum, a maximum or both")
	}
	var min, max uint64
	var err error
	if lo != "" {
		if min, err = parseLimitsID(lo); err != nil {
			return fmt.Errorf("invalid minimum: %w", err)
		}
	}
	if hi != "" {
		if max, err = parseLimitsID(hi); err != nil {
			return fmt.Errorf("invalid maximum: %w", err)
		}
	}
	if lo != "" && hi != "" && min > max {
		return fmt.Errorf("the minimum %d is greater than the maximum %d", min, max)
	}
	return nil
}

// validateLimitsDomain reports why d is not a domain of pam_limits: a user
// name, "@group", "*", "%", "%group", a UID range "<min>:<max>", a GID
// range "@<min>:<max>" or a GID "%:<gid>".
func validateLimitsDomain(d string) error {
	if d == "" {
		return errors.New("domain must not be empty")
	}
	if d == "*" || d == "%" {
		return nil
	}
	wrap := func(err error) error {
		if err == nil {
			return nil
		}
		return fmt.Errorf("domain %q: %w", d, err)
	}
	switch d[0] {
	case '@':
		if strings.Contains(d, ":") {
			return wrap(validateLimitsRange(d[1:]))
		}
		return wrap(validateLimitsName(d[1:], "group"))
	case '%':
		if strings.Contains(d, ":") {
			if !strings.HasPrefix(d, "%:") {
				return wrap(errors.New("a GID after \"%\" must be written \"%:<gid>\"; ranges are not supported there"))
			}
			if _, err := parseLimitsID(d[2:]); err != nil {
				return wrap(fmt.Errorf("invalid GID: %w", err))
			}
			return nil
		}
		return wrap(validateLimitsName(d[1:], "group"))
	case '*':
		return wrap(errors.New("\"*\" must be the whole domain"))
	}
	if strings.Contains(d, ":") {
		return wrap(validateLimitsRange(d))
	}
	return wrap(validateLimitsName(d, "user"))
}

// validateLimitsType reports why t is not a limit type.
func validateLimitsType(t string) error {
	for _, v := range limitsTypes {
		if t == v {
			return nil
		}
	}
	return fmt.Errorf("type %q must be \"soft\", \"hard\" or \"-\" (both)", t)
}

// validateLimitsItem reports why item is not an item pam_limits knows.
func validateLimitsItem(item string) error {
	if _, ok := limitsItems[item]; !ok {
		return fmt.Errorf("item %q is not one of %s", item, strings.Join(limitsItemNames(), ", "))
	}
	return nil
}

// validateLimitsValueSyntax reports why v cannot be the value of any item:
// it must be one field of printable characters.
func validateLimitsValueSyntax(v string) error {
	if v == "" {
		return errors.New("value must not be empty")
	}
	for i := 0; i < len(v); i++ {
		if c := v[i]; c <= ' ' || c == 0x7f || c == '#' {
			return fmt.Errorf("value %q must be a single field without white space, control characters or \"#\"", v)
		}
	}
	return nil
}

// validateLimitsValue reports why v is not a valid value of item, which
// must be valid.
func validateLimitsValue(item, v string) error {
	if err := validateLimitsValueSyntax(v); err != nil {
		return err
	}
	switch limitsItems[item] {
	case limitsPriority:
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < -20 || n > 19 {
			return fmt.Errorf("value %q of %s must be a nice value from -20 to 19", v, item)
		}
	case limitsBoolean:
		if v != "0" && v != "1" {
			return fmt.Errorf("value %q of %s must be 0 or 1", v, item)
		}
	default:
		if isLimitsUnlimited(v) {
			return nil
		}
		if strings.TrimLeft(v, "0123456789") != "" {
			return fmt.Errorf("value %q of %s must be a non-negative integer, or \"unlimited\", \"infinity\" or \"-1\" for no limit", v, item)
		}
		if _, err := strconv.ParseUint(v, 10, 64); err != nil {
			return fmt.Errorf("value %q of %s is too large", v, item)
		}
	}
	return nil
}

// validateLimitsDomainItem reports why an entry for domain cannot set item:
// pam_limits only applies logins limits to "%" domains.
func validateLimitsDomainItem(domain, item string) error {
	if strings.HasPrefix(domain, "%") && item != "maxlogins" && item != "maxsyslogins" {
		return fmt.Errorf("domain %q only applies to the maxlogins and maxsyslogins items, not to %s", domain, item)
	}
	return nil
}

// normalizeLimitsValue returns the canonical form of v, so that values
// pam_limits treats the same compare equal: all spellings of unlimited are
// "unlimited", and numbers lose leading zeros and a "+".
func normalizeLimitsValue(v string) string {
	if isLimitsUnlimited(v) {
		return "unlimited"
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return strconv.FormatInt(n, 10)
	}
	if n, err := strconv.ParseUint(v, 10, 64); err == nil {
		return strconv.FormatUint(n, 10)
	}
	return v
}

// limitsValuesEqual reports whether a and b are the same limit.
func limitsValuesEqual(a, b string) bool {
	return normalizeLimitsValue(a) == normalizeLimitsValue(b)
}

// limitsFileNameChar reports whether c may appear in a drop-in file name.
func limitsFileNameChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '_' || c == '.' || c == '-' || c == '+' || c == '@'
}

// validateLimitsFileName reports why name cannot be the name of a drop-in
// file in limits.d. pam_limits only reads files matching "*.conf" there,
// and the glob skips names starting with ".". Names are plain file names,
// so they can never refer to a file outside the directory.
func validateLimitsFileName(name string) error {
	if name == "" {
		return errors.New("file name must not be empty")
	}
	if len(name) > maxLimitsNameLen {
		return fmt.Errorf("file name must be at most %d bytes long", maxLimitsNameLen)
	}
	if strings.Contains(name, "/") {
		return fmt.Errorf("file name %q must be a plain name inside %s, without \"/\"", name, defaultLimitsDir)
	}
	for i := 0; i < len(name); i++ {
		if !limitsFileNameChar(name[i]) {
			return fmt.Errorf("file name %q contains the character %q; only letters, digits and \"_.-+@\" are allowed", name, name[i])
		}
	}
	if name[0] == '.' || name[0] == '-' {
		return fmt.Errorf("file name %q must not start with %q", name, name[0])
	}
	if !strings.HasSuffix(name, ".conf") || name == ".conf" {
		return fmt.Errorf("file name %q must end in \".conf\"; pam_limits ignores other files in %s", name, defaultLimitsDir)
	}
	return nil
}

// limitsFilePath returns the managed path of the drop-in file name, which
// must be valid.
func limitsFilePath(name string) string {
	return path.Join(defaultLimitsDir, name)
}

// limitsFileNameFromPath returns the file name of the drop-in at p, a path
// as in an import ID, and reports why p is not a drop-in in limits.d.
func limitsFileNameFromPath(p string) (string, error) {
	dir, name := path.Split(p)
	if path.Clean(dir) != defaultLimitsDir || p != limitsFilePath(name) {
		return "", fmt.Errorf("path %q must be a file directly in %s", p, defaultLimitsDir)
	}
	if err := validateLimitsFileName(name); err != nil {
		return "", err
	}
	return name, nil
}

// defaultLimitsFileName returns the drop-in file name for entries of
// domain, which must be valid: "90-terraform-<domain>.conf", with the domain
// spelled so that it is a valid file name. The "90-" sorts it after the
// files distributions ship, such as RHEL's 20-nproc.conf, so its entries
// take precedence over theirs for the same domain.
func defaultLimitsFileName(domain string) string {
	var slug string
	switch {
	case domain == "*":
		slug = "default"
	case domain == "%":
		slug = "logins"
	case strings.HasPrefix(domain, "%:"):
		slug = "logins-gid-" + domain[2:]
	case strings.HasPrefix(domain, "%"):
		slug = "logins-group-" + domain[1:]
	case strings.HasPrefix(domain, "@") && strings.Contains(domain, ":"):
		slug = "gid-" + domain[1:]
	case strings.HasPrefix(domain, "@"):
		slug = "group-" + domain[1:]
	case strings.Contains(domain, ":"):
		slug = "uid-" + domain
	default:
		slug = "user-" + domain
	}
	slug = strings.Map(func(r rune) rune {
		if r < 0x80 && limitsFileNameChar(byte(r)) {
			return r
		}
		return '-'
	}, slug)
	for strings.Contains(slug, "--") {
		slug = strings.ReplaceAll(slug, "--", "-")
	}
	slug = strings.Trim(slug, "-")
	name := "90-terraform-" + slug + ".conf"
	if len(name) > maxLimitsNameLen {
		name = name[:maxLimitsNameLen-len(".conf")] + ".conf"
	}
	return name
}

// limitsEntry is an entry of a limits file.
type limitsEntry struct {
	domain, typ, item, value string
}

// limitsKey identifies an entry.
type limitsKey struct {
	domain, typ, item string
}

func (e limitsEntry) key() limitsKey { return limitsKey{e.domain, e.typ, e.item} }

// line renders e as a line of a limits file, aligned like the examples in
// limits.conf.
func (e limitsEntry) line() string {
	return fmt.Sprintf("%-15s %-5s %-15s %s", e.domain, e.typ, e.item, e.value)
}

// parseLimitsLine parses one line of a limits file as pam_limits does and
// reports whether it is an entry.
func parseLimitsLine(line string) (limitsEntry, bool) {
	if i := strings.IndexByte(line, '#'); i >= 0 {
		line = line[:i]
	}
	f := strings.Fields(line)
	if len(f) < 4 {
		return limitsEntry{}, false
	}
	return limitsEntry{domain: f[0], typ: f[1], item: f[2], value: f[3]}, true
}

// findLimitsEntries returns the indexes of the lines that are entries with
// key k.
func findLimitsEntries(lines []string, k limitsKey) []int {
	var idx []int
	for i, l := range lines {
		if e, ok := parseLimitsLine(l); ok && e.key() == k {
			idx = append(idx, i)
		}
	}
	return idx
}

// lookupLimitsEntry returns the value that t sets for k and the number of
// entries with key k. As in pam_limits, the last one wins.
func lookupLimitsEntry(t *textFile, k limitsKey) (value string, count int) {
	idx := findLimitsEntries(t.lines, k)
	if len(idx) == 0 {
		return "", 0
	}
	e, _ := parseLimitsLine(t.lines[idx[len(idx)-1]])
	return e.value, len(idx)
}

// setLimitsEntry makes t hold e exactly once, replacing the first entry with
// its key in place and removing the others, or appending it. An entry of an
// equal value is left as written. It reports whether t changed.
func setLimitsEntry(t *textFile, e limitsEntry) bool {
	idx := findLimitsEntries(t.lines, e.key())
	if len(idx) == 0 {
		t.insert(len(t.lines), []string{e.line()})
		return true
	}
	changed := false
	for i := len(idx) - 1; i > 0; i-- {
		t.replace(idx[i], idx[i]+1, nil)
		changed = true
	}
	if cur, _ := parseLimitsLine(t.lines[idx[0]]); !limitsValuesEqual(cur.value, e.value) {
		t.lines[idx[0]] = e.line()
		changed = true
	}
	return changed
}

// removeLimitsEntries removes every entry with key k from t and reports
// whether t changed.
func removeLimitsEntries(t *textFile, k limitsKey) bool {
	idx := findLimitsEntries(t.lines, k)
	for i := len(idx) - 1; i >= 0; i-- {
		t.replace(idx[i], idx[i]+1, nil)
	}
	return len(idx) > 0
}

// limitsFileOnlyHeader reports whether t is a file sysutils_limits created
// that holds nothing else anymore: its first line is limitsFileHeader, and
// every other line is blank.
func limitsFileOnlyHeader(t *textFile) bool {
	if len(t.lines) == 0 || t.lines[0] != limitsFileHeader {
		return false
	}
	for _, l := range t.lines[1:] {
		if strings.TrimSpace(l) != "" {
			return false
		}
	}
	return true
}

// readLimitsFile reads and parses the drop-in file at the host path p. A
// missing file reads as empty with a nil snapshot.
func readLimitsFile(p string) (*textFile, *fileSnapshot, error) {
	data, snap, err := readRegularFileNoFollow(p, maxLimitsFileSize)
	if errors.Is(err, fs.ErrNotExist) {
		return &textFile{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return parseTextFile(data), snap, nil
}

// editLimitsFile applies edit to the drop-in file at the host path p and
// writes the result atomically if it changed, keeping the file's mode,
// ownership and extended attributes. A missing file is created, along with
// its parent directories, only if edit adds something, and then starts with
// limitsFileHeader. A file that starts with that header and holds nothing
// else after the edit is removed. Concurrent edits by other sysutils
// resources are serialized.
func editLimitsFile(p string, edit func(t *textFile) bool) error {
	unlock, err := lockFileForEdit(p)
	if err != nil {
		return err
	}
	defer unlock()
	t, snap, err := readLimitsFile(p)
	if err != nil {
		return err
	}
	if !edit(t) {
		return nil
	}
	if snap == nil {
		t.lines = append([]string{limitsFileHeader}, t.lines...)
		t.trailingNewline = true
	}
	if limitsFileOnlyHeader(t) {
		if snap == nil {
			return nil
		}
		return removeManagedFile(p, snap)
	}
	return writeManagedFile(p, t.bytes(), snap, limitsFileMode)
}
