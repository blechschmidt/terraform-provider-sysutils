package provider

// System locale handling behind sysutils_locale. The locale is a set of
// shell-style variable assignments (LANG=..., LC_TIME=...) in
// /etc/locale.conf (systemd, RHEL, Fedora, Arch) or /etc/default/locale
// (Debian, Ubuntu). The file is edited line by line: the LANG and LC_*
// assignments are the resource's, everything else is kept. Missing locales
// are compiled with locale-gen where /etc/locale.gen lists the locales to
// build (Debian, Ubuntu, Arch), or with localedef otherwise.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	// localeConfPath is the locale file of systemd-based distributions.
	localeConfPath = "/etc/locale.conf"
	// debianLocalePath is the locale file of Debian-based distributions.
	debianLocalePath = "/etc/default/locale"
	// localeGenPath lists the locales that locale-gen compiles.
	localeGenPath = "/etc/locale.gen"
	// i18nSupportedPath lists the locales glibc can build, with their
	// character maps.
	i18nSupportedPath = "/usr/share/i18n/SUPPORTED"
	// maxLocaleFileSize bounds how much of a locale file, locale.gen or
	// SUPPORTED is read.
	maxLocaleFileSize = 4 << 20
	// maxLocaleNameLen bounds the length of a locale name.
	maxLocaleNameLen = 128
	// localeFileMode is the mode of a locale file created by the provider.
	localeFileMode fs.FileMode = 0o644
	// localeGenTimeout bounds locale-gen and localedef, which compile
	// every listed locale and can take minutes on slow machines.
	localeGenTimeout = 15 * time.Minute
	// localeListTimeout bounds "locale -a".
	localeListTimeout = 30 * time.Second
	// localeOutputLimit caps the output kept from locale commands.
	localeOutputLimit = 1 << 20
)

// localeLangVar is the variable that sets the default for every category.
const localeLangVar = "LANG"

// localeLCVariables are the LC_* variables that can be set in the locale
// file, in the order in which they are written. LC_ALL is not among them:
// it overrides everything and is meant for one-off use, and systemd refuses
// it in locale.conf.
var localeLCVariables = []string{
	"LC_CTYPE", "LC_NUMERIC", "LC_TIME", "LC_COLLATE", "LC_MONETARY", "LC_MESSAGES",
	"LC_PAPER", "LC_NAME", "LC_ADDRESS", "LC_TELEPHONE", "LC_MEASUREMENT", "LC_IDENTIFICATION",
}

var (
	// localeNamePattern matches locale names such as "C", "POSIX",
	// "C.UTF-8", "en_US.UTF-8", "de_DE@euro" or "sr_RS.UTF-8@latin".
	localeNamePattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.@+-]*$`)
	// localeVarPattern matches the variable names of assignments.
	localeVarPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// validateLocaleName reports why name is not an acceptable locale name.
func validateLocaleName(name string) error {
	if name == "" {
		return errors.New("locale must not be empty")
	}
	if len(name) > maxLocaleNameLen {
		return fmt.Errorf("locale must be at most %d bytes long", maxLocaleNameLen)
	}
	if !localeNamePattern.MatchString(name) {
		return fmt.Errorf("locale %q must be a locale name such as \"en_US.UTF-8\" or \"C.UTF-8\": letters, digits, \"_\", \".\", \"@\", \"+\" and \"-\", starting with a letter, digit or \"_\"", name)
	}
	return nil
}

// validateLCVariable reports why name is not one of the LC_* variables the
// locale file can set.
func validateLCVariable(name string) error {
	if slices.Contains(localeLCVariables, name) {
		return nil
	}
	return fmt.Errorf("%q is not a locale category variable; use one of %s", name, strings.Join(localeLCVariables, ", "))
}

// isManagedLocaleVar reports whether name is one of the variables the
// resource owns in the locale file: LANG and every LC_* variable,
// including LC_ALL and unknown ones, which are removed.
func isManagedLocaleVar(name string) bool {
	return name == localeLangVar || strings.HasPrefix(name, "LC_")
}

// normalizeLocaleName returns name in the form glibc stores and lists
// locales in: the codeset lower-cased with everything but letters and
// digits removed, prefixed with "iso" if it is all digits, as
// _nl_normalize_codeset does. "en_US.UTF-8" becomes "en_US.utf8".
func normalizeLocaleName(name string) string {
	base, modifier, hasModifier := strings.Cut(name, "@")
	lang, codeset, hasCodeset := strings.Cut(base, ".")
	if hasCodeset {
		var b strings.Builder
		digitsOnly := true
		for _, c := range codeset {
			switch {
			case c >= 'A' && c <= 'Z':
				b.WriteRune(c - 'A' + 'a')
				digitsOnly = false
			case c >= 'a' && c <= 'z':
				b.WriteRune(c)
				digitsOnly = false
			case c >= '0' && c <= '9':
				b.WriteRune(c)
			}
		}
		codeset = b.String()
		if digitsOnly {
			codeset = "iso" + codeset
		}
		lang += "." + codeset
	}
	if hasModifier {
		lang += "@" + modifier
	}
	return lang
}

// parseLocaleAssignment parses one line of a locale file. It accepts
// "NAME=value", optionally preceded by "export" and with the value in single
// or double quotes, and reports ok=false for blank lines, comments and
// anything else.
func parseLocaleAssignment(line string) (name, value string, ok bool) {
	s := strings.TrimSpace(line)
	if s == "" || s[0] == '#' {
		return "", "", false
	}
	if rest, found := strings.CutPrefix(s, "export"); found && rest != "" && (rest[0] == ' ' || rest[0] == '\t') {
		s = strings.TrimSpace(rest)
	}
	name, value, found := strings.Cut(s, "=")
	if !found || !localeVarPattern.MatchString(name) {
		return "", "", false
	}
	switch {
	case len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && strings.IndexByte(value[1:], value[0]) >= 0:
		value = value[1 : 1+strings.IndexByte(value[1:], value[0])]
	default:
		if i := strings.IndexAny(value, " \t#"); i >= 0 {
			value = value[:i]
		}
	}
	return name, value, true
}

// readLocaleVars returns the managed variables that the lines assign. The
// last assignment of a variable wins, as when the file is sourced.
func readLocaleVars(lines []string) map[string]string {
	vars := map[string]string{}
	for _, line := range lines {
		if name, value, ok := parseLocaleAssignment(line); ok && isManagedLocaleVar(name) {
			vars[name] = value
		}
	}
	return vars
}

// setLocaleVars makes t assign exactly the managed variables in vars. The
// first assignment of a variable in vars is rewritten in place if its value
// differs; further assignments of it, and assignments of managed variables
// not in vars, are removed; missing ones are appended, LANG first. Other
// lines are kept. It reports whether t changed.
func setLocaleVars(t *textFile, vars map[string]string) bool {
	seen := map[string]bool{}
	changed := false
	kept := t.lines[:0:0]
	for _, line := range t.lines {
		name, value, ok := parseLocaleAssignment(line)
		if !ok || !isManagedLocaleVar(name) {
			kept = append(kept, line)
			continue
		}
		want, wanted := vars[name]
		if !wanted || seen[name] {
			changed = true
			continue
		}
		seen[name] = true
		if value != want {
			line = name + "=" + want
			changed = true
		}
		kept = append(kept, line)
	}
	order := append([]string{localeLangVar}, localeLCVariables...)
	for _, name := range slices.Sorted(maps.Keys(vars)) {
		if !slices.Contains(order, name) {
			order = append(order, name)
		}
	}
	for _, name := range order {
		if want, wanted := vars[name]; wanted && !seen[name] {
			kept = append(kept, name+"="+want)
			changed = true
		}
	}
	t.lines = kept
	if changed && len(t.lines) > 0 {
		t.trailingNewline = true
	}
	return changed
}

// localeFileEmpty reports whether t holds nothing but blank lines and
// comments.
func localeFileEmpty(t *textFile) bool {
	for _, line := range t.lines {
		if s := strings.TrimSpace(line); s != "" && s[0] != '#' {
			return false
		}
	}
	return true
}

// detectLocalePath returns the locale file of the distribution in root:
// /etc/default/locale if it is a regular file, else /etc/locale.conf if it
// exists, else /etc/default/locale on Debian-based distributions and
// /etc/locale.conf everywhere else. On Ubuntu, /etc/default/locale can be a
// symlink to /etc/locale.conf, which is then the file to edit.
func detectLocalePath(root *fsRoot) (string, error) {
	lstat := func(p string) (fs.FileInfo, error) {
		host, err := root.resolve(p)
		if err != nil {
			return nil, err
		}
		return os.Lstat(host)
	}
	if info, err := lstat(debianLocalePath); err == nil && info.Mode().IsRegular() {
		return debianLocalePath, nil
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if _, err := lstat(localeConfPath); err == nil {
		return localeConfPath, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if _, err := lstat("/etc/debian_version"); err == nil {
		return debianLocalePath, nil
	}
	return localeConfPath, nil
}

// localeFileState is what the locale file assigns. It is recorded in
// private state before the first apply, so that destroy can put it back.
type localeFileState struct {
	Exists bool              `json:"exists"`
	Vars   map[string]string `json:"vars,omitempty"`
}

// readLocaleFile reads the locale file at the host path p. A missing file
// assigns nothing.
func readLocaleFile(p string) (*textFile, *fileSnapshot, error) {
	data, snap, err := readRegularFileNoFollow(p, maxLocaleFileSize)
	if errors.Is(err, fs.ErrNotExist) {
		return &textFile{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return parseTextFile(data), snap, nil
}

// readLocaleFileState returns what the locale file at the host path p
// assigns.
func readLocaleFileState(p string) (*localeFileState, error) {
	t, snap, err := readLocaleFile(p)
	if err != nil {
		return nil, err
	}
	return &localeFileState{Exists: snap != nil, Vars: readLocaleVars(t.lines)}, nil
}

// writeLocaleVars makes the locale file at the host path p assign exactly
// the managed variables in vars. With removeIfEmpty set, a file left with
// nothing but comments and blank lines is removed instead.
func writeLocaleVars(p string, vars map[string]string, removeIfEmpty bool) error {
	unlock, err := lockFileForEdit(p)
	if err != nil {
		return err
	}
	defer unlock()
	t, snap, err := readLocaleFile(p)
	if err != nil {
		return err
	}
	changed := setLocaleVars(t, vars)
	if removeIfEmpty && localeFileEmpty(t) {
		if snap == nil {
			return nil
		}
		return removeManagedFile(p, snap)
	}
	if !changed && snap != nil {
		return nil
	}
	return writeManagedFile(p, t.bytes(), snap, localeFileMode)
}

// localeConfig is the provider-level configuration of sysutils_locale. The
// zero value, or a nil pointer, selects the real locale tools.
type localeConfig struct {
	// run runs locale, locale-gen and localedef; nil selects runCommand.
	run commandRunner
	// lookPath finds them; nil selects exec.LookPath.
	lookPath func(string) (string, error)
	// etcDir and i18nDir replace /etc and /usr/share/i18n when looking for
	// locale.gen and SUPPORTED.
	etcDir, i18nDir string
}

func (c *localeConfig) runner() commandRunner {
	if c == nil || c.run == nil {
		return runCommand
	}
	return c.run
}

func (c *localeConfig) look(name string) error {
	lookPath := exec.LookPath
	if c != nil && c.lookPath != nil {
		lookPath = c.lookPath
	}
	_, err := lookPath(name)
	return err
}

func (c *localeConfig) localeGenFile() string {
	if c != nil && c.etcDir != "" {
		return filepath.Join(c.etcDir, "locale.gen")
	}
	return localeGenPath
}

func (c *localeConfig) supportedFile() string {
	if c != nil && c.i18nDir != "" {
		return filepath.Join(c.i18nDir, "SUPPORTED")
	}
	return i18nSupportedPath
}

// command runs argv with the C locale and fails on a non-zero exit code.
func (c *localeConfig) command(ctx context.Context, timeout time.Duration, argv ...string) (string, error) {
	res, err := c.runner()(ctx, execSpec{
		Argv:           argv,
		Env:            append(os.Environ(), "LC_ALL=C"),
		Timeout:        timeout,
		MaxOutputBytes: localeOutputLimit,
	})
	if err != nil {
		return "", fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
	}
	if res.TimedOut {
		return "", fmt.Errorf("%s: timed out after %s", strings.Join(argv, " "), timeout)
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(res.Stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(res.Stdout.String())
		}
		return "", fmt.Errorf("%s: exit status %d: %s", strings.Join(argv, " "), res.ExitCode, msg)
	}
	return res.Stdout.String(), nil
}

// available returns the normalized names of the locales installed on the
// host, as listed by "locale -a". C and POSIX are always available.
func (c *localeConfig) available(ctx context.Context) (map[string]bool, error) {
	if err := c.look("locale"); err != nil {
		return nil, errors.New("cannot list the installed locales: the locale command was not found")
	}
	out, err := c.command(ctx, localeListTimeout, "locale", "-a")
	if err != nil {
		return nil, err
	}
	set := map[string]bool{"C": true, "POSIX": true}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		if name := strings.TrimSpace(sc.Text()); name != "" {
			set[normalizeLocaleName(name)] = true
		}
	}
	return set, nil
}

// missingLocales returns those of names that are not installed, in order and
// without duplicates.
func missingLocales(installed map[string]bool, names []string) []string {
	var missing []string
	for _, n := range names {
		if !installed[normalizeLocaleName(n)] && !slices.Contains(missing, n) {
			missing = append(missing, n)
		}
	}
	return missing
}

// supportedLocale is a line of SUPPORTED or locale.gen: a locale name and
// its character map, such as "en_US.UTF-8 UTF-8".
type supportedLocale struct {
	name, charmap string
}

// parseSupportedLine parses a line of SUPPORTED or an uncommented line of
// locale.gen.
func parseSupportedLine(line string) (supportedLocale, bool) {
	fields := strings.Fields(line)
	if len(fields) != 2 || strings.HasPrefix(fields[0], "#") {
		return supportedLocale{}, false
	}
	return supportedLocale{name: fields[0], charmap: fields[1]}, true
}

// lookupSupported finds name in the SUPPORTED file, comparing normalized
// names. It returns found=false if the file or the entry does not exist.
func (c *localeConfig) lookupSupported(name string) (supportedLocale, bool, error) {
	data, _, err := readRegularFileNoFollow(c.supportedFile(), maxLocaleFileSize)
	if errors.Is(err, fs.ErrNotExist) {
		return supportedLocale{}, false, nil
	}
	if err != nil {
		return supportedLocale{}, false, err
	}
	want := normalizeLocaleName(name)
	for _, line := range strings.Split(string(data), "\n") {
		if e, ok := parseSupportedLine(line); ok && normalizeLocaleName(e.name) == want {
			return e, true, nil
		}
	}
	return supportedLocale{}, false, nil
}

// enableInLocaleGen makes the locale.gen lines t list e: an existing
// uncommented line for it is kept, a commented-out one ("# en_US.UTF-8
// UTF-8") is uncommented, and otherwise a line is appended. It reports
// whether t changed.
func enableInLocaleGen(t *textFile, e supportedLocale) bool {
	want := normalizeLocaleName(e.name)
	commented := -1
	for i, line := range t.lines {
		s := strings.TrimSpace(line)
		if entry, ok := parseSupportedLine(s); ok && normalizeLocaleName(entry.name) == want {
			return false
		}
		if rest, ok := strings.CutPrefix(s, "#"); ok && commented < 0 {
			if entry, ok := parseSupportedLine(strings.TrimSpace(rest)); ok && entry.name == e.name && entry.charmap == e.charmap {
				commented = i
			}
		}
	}
	line := e.name + " " + e.charmap
	if commented >= 0 {
		t.lines[commented] = line
	} else {
		t.lines = append(t.lines, line)
	}
	t.trailingNewline = true
	return true
}

// generate compiles the locales in names on the host: with locale-gen after
// listing them in /etc/locale.gen where that file and locale-gen exist, as
// on Debian, Ubuntu and Arch, whose locale-gen rebuilds exactly the locales
// listed there; otherwise with localedef, as on Fedora and RHEL.
func (c *localeConfig) generate(ctx context.Context, names []string) error {
	localeGen := c.localeGenFile()
	_, lgErr := os.Lstat(localeGen)
	if lgErr == nil && c.look("locale-gen") == nil {
		return c.generateWithLocaleGen(ctx, localeGen, names)
	}
	if c.look("localedef") != nil {
		return errors.New("cannot generate locales: neither locale-gen with " + localeGenPath + " nor localedef was found; install the distribution's locale package (such as \"locales\" on Debian or \"glibc-locale-source\" on Fedora), or generate the locale otherwise and set generate = false")
	}
	for _, name := range names {
		input, charmap, err := c.localedefArgs(name)
		if err != nil {
			return err
		}
		if _, err := c.command(ctx, localeGenTimeout, "localedef", "-i", input, "-f", charmap, "--", name); err != nil {
			return fmt.Errorf("generating locale %s: %w; the locale sources may be missing (on Fedora and RHEL, install glibc-locale-source, or the glibc-langpack package of the language instead of using generate)", name, err)
		}
	}
	return nil
}

func (c *localeConfig) generateWithLocaleGen(ctx context.Context, localeGen string, names []string) error {
	entries := make([]supportedLocale, 0, len(names))
	for _, name := range names {
		e, found, err := c.lookupSupported(name)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("cannot generate locale %s: it is not listed in %s", name, c.supportedFile())
		}
		entries = append(entries, e)
	}
	err := func() error {
		unlock, err := lockFileForEdit(localeGen)
		if err != nil {
			return err
		}
		defer unlock()
		data, snap, err := readRegularFileNoFollow(localeGen, maxLocaleFileSize)
		if err != nil {
			return err
		}
		t := parseTextFile(data)
		changed := false
		for _, e := range entries {
			if enableInLocaleGen(t, e) {
				changed = true
			}
		}
		if !changed {
			return nil
		}
		return replaceFileAtomic(localeGen, t.bytes(), snap, localeFileMode)
	}()
	if err != nil {
		return fmt.Errorf("editing %s: %w", localeGen, err)
	}
	if _, err := c.command(ctx, localeGenTimeout, "locale-gen"); err != nil {
		return err
	}
	return nil
}

// localedefArgs returns the input and character map arguments of localedef
// for name: "de_DE.UTF-8@euro" is compiled from the input "de_DE@euro" with
// the character map "UTF-8". A name without a codeset takes the character
// map from SUPPORTED.
func (c *localeConfig) localedefArgs(name string) (input, charmap string, err error) {
	base, modifier, hasModifier := strings.Cut(name, "@")
	lang, codeset, hasCodeset := strings.Cut(base, ".")
	input = lang
	if hasModifier {
		input += "@" + modifier
	}
	switch {
	case hasCodeset && normalizeLocaleName("x."+codeset) == "x.utf8":
		charmap = "UTF-8"
	case hasCodeset:
		charmap = codeset
	default:
		e, found, err := c.lookupSupported(name)
		if err != nil {
			return "", "", err
		}
		if !found {
			return "", "", fmt.Errorf("cannot generate locale %s: it has no codeset, such as %s.UTF-8, and is not listed in %s", name, name, c.supportedFile())
		}
		charmap = e.charmap
	}
	return input, charmap, nil
}
