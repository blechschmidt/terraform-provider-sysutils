package provider

// Alternatives handling behind sysutils_alternatives. Two tools exist:
// dpkg's update-alternatives (Debian, Ubuntu, SUSE), which reports a link
// group with --query in a stable, machine-readable format, and the
// alternatives command of chkconfig (Fedora, RHEL), which only has the
// human-readable --display. Both accept the same --install, --set, --auto
// and --remove commands. The commands run through a commandRunner, always
// with an argument vector and never through a shell, so that unit tests can
// substitute a fake.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/sys/unix"
)

// Kinds of alternatives tools.
const (
	// alternativesDebian is dpkg's update-alternatives.
	alternativesDebian = "update-alternatives"
	// alternativesRHEL is chkconfig's alternatives.
	alternativesRHEL = "alternatives"
)

// Values of the mode attribute, as both tools name them.
const (
	alternativesModeAuto   = "auto"
	alternativesModeManual = "manual"
)

const (
	// defaultRHELAlternativesAdminDir holds chkconfig's administrative
	// files, one per link group, which record the group's master link.
	defaultRHELAlternativesAdminDir = "/var/lib/alternatives"
	// alternativesTimeout bounds each command. They only rewrite a few
	// symlinks and small files.
	alternativesTimeout = 2 * time.Minute
	// alternativesOutputLimit caps how much output is kept per stream.
	// Link groups with many alternatives and followers, such as java,
	// print tens of kilobytes.
	alternativesOutputLimit = 1 << 20
	// maxAlternativesNameLength bounds link group names, which are file
	// names in the administrative directory.
	maxAlternativesNameLength = 255
	// maxAlternativesPathLength is PATH_MAX.
	maxAlternativesPathLength = 4096
	// maxAlternativesAdminFileSize bounds how much of a chkconfig
	// administrative file is read.
	maxAlternativesAdminFileSize = 1 << 20
)

// alternativesNamePattern matches link group names such as "editor",
// "x-www-browser", "c++", "libblas.so.3-x86_64-linux-gnu" or
// "libnssckbi.so.x86_64". A name never starts with "-", so it can never be
// taken for an option, and never contains "/", so it can never name a file
// outside the administrative directory.
var alternativesNamePattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.+@:~-]*$`)

// validateAlternativesName reports why name is not an acceptable link group
// name.
func validateAlternativesName(name string) error {
	if len(name) > maxAlternativesNameLength {
		return fmt.Errorf("alternatives name %q is longer than %d characters", name, maxAlternativesNameLength)
	}
	if !alternativesNamePattern.MatchString(name) {
		return fmt.Errorf("alternatives name %q must consist of ASCII letters, digits, \"_\", \".\", \"+\", \"@\", \":\", \"~\" or \"-\", starting with a letter, digit or \"_\"", name)
	}
	return nil
}

// validateAlternativesPath reports why p is not an acceptable alternative
// or link path. Besides being absolute and canonical, it must not contain
// white space or control characters: chkconfig's --display separates the
// fields of its output with spaces, and both tools write paths into
// line-based administrative files.
func validateAlternativesPath(p string) error {
	if err := validateAbsolutePath(p); err != nil {
		return err
	}
	if len(p) > maxAlternativesPathLength {
		return fmt.Errorf("path %q is longer than %d bytes", p, maxAlternativesPathLength)
	}
	for _, r := range p {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == unicode.ReplacementChar {
			return fmt.Errorf("path %q must not contain white space, control characters or invalid UTF-8", p)
		}
	}
	return nil
}

// alternativeEntry is one alternative of a link group.
type alternativeEntry struct {
	Path     string
	Priority int64
}

// alternativesStatus is what the tool reports about a link group.
type alternativesStatus struct {
	// Found is false if the tool knows no link group of that name.
	Found bool
	// Link is the group's master link, such as "/usr/bin/editor".
	Link string
	// Mode is "auto" or "manual".
	Mode string
	// Value is the alternative the link points to; empty if it points to
	// none.
	Value string
	// Alternatives lists the registered alternatives in the tool's order.
	Alternatives []alternativeEntry
}

// entry returns the registered alternative p.
func (s *alternativesStatus) entry(p string) (alternativeEntry, bool) {
	for _, e := range s.Alternatives {
		if e.Path == p {
			return e, true
		}
	}
	return alternativeEntry{}, false
}

// selected returns the alternative the link group points to, if it is one
// of the registered alternatives. dpkg keeps reporting an alternative that
// was deleted from disk as the value until the group is next changed.
func (s *alternativesStatus) selected() string {
	if _, ok := s.entry(s.Value); ok {
		return s.Value
	}
	return ""
}

// alternativesConfig is the provider-level configuration of
// sysutils_alternatives. The zero value, or a nil pointer, selects the real
// tools.
type alternativesConfig struct {
	// run runs the commands; nil selects runCommand.
	run commandRunner
	// lookPath finds the tools; nil selects exec.LookPath.
	lookPath func(string) (string, error)
	// rhelAdminDir replaces /var/lib/alternatives.
	rhelAdminDir string
}

func (c *alternativesConfig) runner() commandRunner {
	if c == nil || c.run == nil {
		return runCommand
	}
	return c.run
}

// tool detects the alternatives tool of this host. Fedora and RHEL ship
// update-alternatives as a symlink to alternatives, so a command of that
// name only counts as dpkg's if it does not resolve to alternatives.
func (c *alternativesConfig) tool() (*alternativesTool, error) {
	lookPath := exec.LookPath
	adminDir := defaultRHELAlternativesAdminDir
	if c != nil && c.lookPath != nil {
		lookPath = c.lookPath
	}
	if c != nil && c.rhelAdminDir != "" {
		adminDir = c.rhelAdminDir
	}
	t := &alternativesTool{run: c.runner(), adminDir: adminDir}
	if p, err := lookPath(alternativesDebian); err == nil {
		resolved, err := filepath.EvalSymlinks(p)
		if err != nil {
			resolved = p
		}
		if filepath.Base(resolved) != alternativesRHEL {
			t.kind, t.bin = alternativesDebian, p
			return t, nil
		}
	}
	if p, err := lookPath(alternativesRHEL); err == nil {
		t.kind, t.bin = alternativesRHEL, p
		return t, nil
	}
	return nil, errors.New("neither update-alternatives (Debian, Ubuntu, SUSE) nor alternatives (Fedora, RHEL) was found in PATH")
}

// alternativesTool runs one of the two tools.
type alternativesTool struct {
	// kind is alternativesDebian or alternativesRHEL.
	kind string
	// bin is the path of the command.
	bin string
	run commandRunner
	// adminDir is chkconfig's administrative directory.
	adminDir string
}

// command runs the tool with args and returns its standard output.
func (t *alternativesTool) command(ctx context.Context, args ...string) (*execResult, error) {
	argv := append([]string{t.bin}, args...)
	res, err := t.run(ctx, execSpec{
		Argv:           argv,
		Env:            append(os.Environ(), "LC_ALL=C"),
		Timeout:        alternativesTimeout,
		MaxOutputBytes: alternativesOutputLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
	}
	if res.TimedOut {
		return nil, fmt.Errorf("%s: timed out after %s", strings.Join(argv, " "), alternativesTimeout)
	}
	return res, nil
}

// change runs a command that changes a link group and fails unless it
// exits with status 0.
func (t *alternativesTool) change(ctx context.Context, args ...string) error {
	res, err := t.command(ctx, args...)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return alternativesFailure(append([]string{t.bin}, args...), res)
	}
	return nil
}

// alternativesFailure describes a command that exited with a non-zero status,
// quoting its own message.
func alternativesFailure(argv []string, res *execResult) error {
	msg := strings.TrimSpace(res.Stderr.String())
	if msg == "" {
		msg = strings.TrimSpace(res.Stdout.String())
	}
	if msg == "" {
		return fmt.Errorf("%s: exit status %d", strings.Join(argv, " "), res.ExitCode)
	}
	return fmt.Errorf("%s: exit status %d: %s", strings.Join(argv, " "), res.ExitCode, msg)
}

// Install registers path as an alternative of the link group name with
// the master link link, creating the group if needed. For an alternative
// that is registered already, it updates the priority and link.
func (t *alternativesTool) Install(ctx context.Context, link, name, path string, priority int64) error {
	return t.change(ctx, "--install", link, name, path, strconv.FormatInt(priority, 10))
}

// Set selects path and puts the link group into manual mode.
func (t *alternativesTool) Set(ctx context.Context, name, path string) error {
	return t.change(ctx, "--set", name, path)
}

// Auto puts the link group into automatic mode, which selects the
// alternative with the highest priority.
func (t *alternativesTool) Auto(ctx context.Context, name string) error {
	return t.change(ctx, "--auto", name)
}

// Remove unregisters path. Removing the last alternative removes the link
// group and its links.
func (t *alternativesTool) Remove(ctx context.Context, name, path string) error {
	return t.change(ctx, "--remove", name, path)
}

// Query reads the state of a link group. It never changes the host.
func (t *alternativesTool) Query(ctx context.Context, name string) (alternativesStatus, error) {
	if t.kind == alternativesRHEL {
		return t.queryRHEL(ctx, name)
	}
	return t.queryDebian(ctx, name)
}

// debianUnknownGroup matches update-alternatives' message for an unknown
// link group, "update-alternatives: error: no alternatives for <name>".
var debianUnknownGroup = regexp.MustCompile(`(?m)error: no alternatives for `)

func (t *alternativesTool) queryDebian(ctx context.Context, name string) (alternativesStatus, error) {
	args := []string{"--query", name}
	res, err := t.command(ctx, args...)
	if err != nil {
		return alternativesStatus{}, err
	}
	if res.ExitCode != 0 {
		if debianUnknownGroup.MatchString(res.Stderr.String()) {
			return alternativesStatus{}, nil
		}
		return alternativesStatus{}, alternativesFailure(append([]string{t.bin}, args...), res)
	}
	if res.Truncated() {
		return alternativesStatus{}, fmt.Errorf("%s --query %s: output exceeds %d bytes", t.bin, name, alternativesOutputLimit)
	}
	return parseDebianAlternativesQuery(res.Stdout.String(), name)
}

// parseDebianAlternativesQuery parses the output of
// "update-alternatives --query <name>": a stanza describing the link group
// ("Name:", "Link:", "Status:", "Best:", "Value:" and "Slaves:" followed by
// indented lines), then one stanza per alternative ("Alternative:",
// "Priority:", "Slaves:"), separated by blank lines.
func parseDebianAlternativesQuery(out, name string) (alternativesStatus, error) {
	st := alternativesStatus{Found: true}
	var cur *alternativeEntry
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 0, 64<<10), alternativesOutputLimit)
	sawName := false
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			// Stanza separators and follower lines.
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return alternativesStatus{}, fmt.Errorf("parsing update-alternatives --query output, line %d: %q is not a \"Field: value\" line", n, line)
		}
		value = strings.TrimSpace(value)
		switch key {
		case "Name":
			if value != name {
				return alternativesStatus{}, fmt.Errorf("update-alternatives --query %s reported the link group %q", name, value)
			}
			sawName = true
		case "Link":
			st.Link = value
		case "Status":
			st.Mode = value
		case "Value":
			if value != "none" {
				st.Value = value
			}
		case "Alternative":
			st.Alternatives = append(st.Alternatives, alternativeEntry{Path: value})
			cur = &st.Alternatives[len(st.Alternatives)-1]
		case "Priority":
			if cur == nil {
				return alternativesStatus{}, fmt.Errorf("parsing update-alternatives --query output, line %d: priority outside an alternative", n)
			}
			p, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return alternativesStatus{}, fmt.Errorf("parsing update-alternatives --query output, line %d: invalid priority %q", n, value)
			}
			cur.Priority = p
		}
	}
	if err := sc.Err(); err != nil {
		return alternativesStatus{}, fmt.Errorf("parsing update-alternatives --query output: %w", err)
	}
	if !sawName {
		return alternativesStatus{}, fmt.Errorf("update-alternatives --query %s printed no link group", name)
	}
	if st.Mode != alternativesModeAuto && st.Mode != alternativesModeManual {
		return alternativesStatus{}, fmt.Errorf("update-alternatives --query %s reported the unknown status %q", name, st.Mode)
	}
	return st, nil
}

func (t *alternativesTool) queryRHEL(ctx context.Context, name string) (alternativesStatus, error) {
	// alternatives --display exits with status 2 and no message for an
	// unknown link group, as it does for other failures, so the
	// administrative file decides whether the group exists. It also
	// records the master link, which --display does not show.
	link, found, err := readRHELAlternativesLink(filepath.Join(t.adminDir, name))
	if err != nil || !found {
		return alternativesStatus{}, err
	}
	args := []string{"--display", name}
	res, err := t.command(ctx, args...)
	if err != nil {
		return alternativesStatus{}, err
	}
	if res.ExitCode != 0 {
		return alternativesStatus{}, alternativesFailure(append([]string{t.bin}, args...), res)
	}
	if res.Truncated() {
		return alternativesStatus{}, fmt.Errorf("%s --display %s: output exceeds %d bytes", t.bin, name, alternativesOutputLimit)
	}
	st, err := parseRHELAlternativesDisplay(res.Stdout.String(), name)
	if err != nil {
		return alternativesStatus{}, err
	}
	st.Link = link
	return st, nil
}

// readRHELAlternativesLink returns the master link recorded in a chkconfig
// administrative file, whose first line is the mode and whose second line
// is the master link. found is false if the file does not exist.
func readRHELAlternativesLink(p string) (link string, found bool, err error) {
	f, err := os.OpenFile(p, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading %s: %w", p, err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return "", false, fmt.Errorf("reading %s: %w", p, err)
	}
	if !fi.Mode().IsRegular() {
		return "", false, fmt.Errorf("%s is not a regular file", p)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxAlternativesAdminFileSize))
	if err != nil {
		return "", false, fmt.Errorf("reading %s: %w", p, err)
	}
	lines := strings.SplitN(string(data), "\n", 3)
	if len(lines) < 2 || strings.TrimSpace(lines[1]) == "" {
		return "", false, fmt.Errorf("%s does not record a master link on its second line", p)
	}
	return strings.TrimSpace(lines[1]), true, nil
}

var (
	// rhelStatusLine is the first line of alternatives --display,
	// "<name> - status is auto.".
	rhelStatusLine = regexp.MustCompile(`^(\S+) - status is (\S+?)\.?$`)
	// rhelEntryLine describes an alternative, "<path> - priority <n>" or
	// "<path> - family <family> priority <n>".
	rhelEntryLine = regexp.MustCompile(`^(/\S*) - (?:family \S+ )?priority (-?\d+)$`)
)

// parseRHELAlternativesDisplay parses the output of
// "alternatives --display <name>":
//
//	editor - status is auto.
//	 link currently points to /usr/bin/vim
//	/usr/bin/nano - priority 40
//	 follower editor.1.gz: /usr/share/man/man1/nano.1.gz
//	/usr/bin/vim - family vim priority 50
//	Current `best' version is /usr/bin/vim.
//
// Older versions say "slave" instead of "follower".
func parseRHELAlternativesDisplay(out, name string) (alternativesStatus, error) {
	st := alternativesStatus{Found: true}
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 0, 64<<10), alternativesOutputLimit)
	sawStatus := false
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimRight(sc.Text(), " \t")
		if !sawStatus {
			if line == "" {
				continue
			}
			m := rhelStatusLine.FindStringSubmatch(line)
			if m == nil {
				return alternativesStatus{}, fmt.Errorf("parsing alternatives --display output, line %d: expected %q, got %q", n, name+" - status is ...", line)
			}
			if m[1] != name {
				return alternativesStatus{}, fmt.Errorf("alternatives --display %s reported the link group %q", name, m[1])
			}
			st.Mode = m[2]
			sawStatus = true
			continue
		}
		if v, ok := strings.CutPrefix(line, " link currently points to "); ok {
			st.Value = strings.TrimSpace(v)
			continue
		}
		if m := rhelEntryLine.FindStringSubmatch(line); m != nil {
			p, err := strconv.ParseInt(m[2], 10, 64)
			if err != nil {
				return alternativesStatus{}, fmt.Errorf("parsing alternatives --display output, line %d: invalid priority %q", n, m[2])
			}
			st.Alternatives = append(st.Alternatives, alternativeEntry{Path: m[1], Priority: p})
		}
		// Follower lines, " link currently absent", the "best" line and
		// anything newer versions add are not needed.
	}
	if err := sc.Err(); err != nil {
		return alternativesStatus{}, fmt.Errorf("parsing alternatives --display output: %w", err)
	}
	if !sawStatus {
		return alternativesStatus{}, fmt.Errorf("alternatives --display %s printed no link group", name)
	}
	if st.Mode != alternativesModeAuto && st.Mode != alternativesModeManual {
		return alternativesStatus{}, fmt.Errorf("alternatives --display %s reported the unknown status %q", name, st.Mode)
	}
	return st, nil
}
