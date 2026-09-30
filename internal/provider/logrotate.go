package provider

// logrotate drop-in handling behind sysutils_logrotate: validation of file
// names, log paths, directives and scripts, rendering of the single block a
// drop-in holds, parsing such a block back for import, and the check of a
// rendered file with "logrotate -d". The directory and how logrotate is
// found and run are injectable so that tests can use a temporary directory
// and a fake logrotate.

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	// defaultLogrotateDir is the directory the stock logrotate.conf of
	// every major distribution includes.
	defaultLogrotateDir = "/etc/logrotate.d"
	// logrotateFileMode is the mode of every drop-in the resource writes.
	// logrotate ignores configuration files writable by group or others.
	logrotateFileMode = 0o644
	// maxLogrotateFileSize bounds how much of a drop-in is read or written.
	maxLogrotateFileSize = 1 << 20
	// maxLogrotateNameLen is NAME_MAX, the longest file name.
	maxLogrotateNameLen = 255
	// logrotateFileHeader starts every drop-in the resource writes.
	logrotateFileHeader = "# Managed by Terraform (sysutils_logrotate). Manual changes will be reverted."
	// logrotateIndent indents the directives of the block.
	logrotateIndent = "    "
	// logrotateTimeout bounds a "logrotate -d" run.
	logrotateTimeout = 30 * time.Second
	// logrotateOutputLimit caps the output kept from logrotate.
	logrotateOutputLimit = 64 << 10
	// maxLogrotateRotate bounds rotate, well above any sensible count.
	maxLogrotateRotate = 1000000
)

// logrotateFrequencies are the values of frequency, in the order the
// documentation lists them.
var logrotateFrequencies = []string{"hourly", "daily", "weekly", "monthly", "yearly"}

// logrotateTabooSuffixes are the default taboo extensions of logrotate
// 3.21: files in an included directory whose names end with one of them are
// silently skipped. ".rhn-cfg-tmp-" matches anywhere after the start,
// as its pattern ends with "*".
var logrotateTabooSuffixes = []string{
	",v", ".bak", ".cfsaved", ".disabled", ".dpkg-bak", ".dpkg-del", ".dpkg-dist", ".dpkg-new",
	".dpkg-old", ".dpkg-tmp", ".rpmnew", ".rpmorig", ".rpmsave", ".swp", ".ucf-dist",
	".ucf-new", ".ucf-old", "~",
}

// logrotateNamePattern is the set of file names accepted for drop-ins.
var logrotateNamePattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.+@-]*$`)

// validateLogrotateName reports why name cannot name a drop-in in
// logrotate.d.
func validateLogrotateName(name string) error {
	switch {
	case name == "":
		return errors.New("name must not be empty")
	case len(name) > maxLogrotateNameLen:
		return fmt.Errorf("name must be at most %d bytes long", maxLogrotateNameLen)
	case !logrotateNamePattern.MatchString(name):
		return fmt.Errorf("name %q must consist of letters, digits and \"_.+@-\", and must not start with \".\", \"+\", \"@\" or \"-\"; it names a file directly in the logrotate.d directory", name)
	case strings.Contains(name, ".rhn-cfg-tmp-"):
		return fmt.Errorf("name %q must not contain \".rhn-cfg-tmp-\": logrotate skips such files in logrotate.d", name)
	}
	for _, s := range logrotateTabooSuffixes {
		if strings.HasSuffix(name, s) {
			return fmt.Errorf("name %q must not end with %q: logrotate skips files in logrotate.d with that extension", name, s)
		}
	}
	return nil
}

// validateLogrotatePath reports why p cannot be one of the log paths of a
// drop-in. Paths are absolute and may contain the glob characters "*", "?"
// and "[...]". A path with white space is written in double quotes.
func validateLogrotatePath(p string) error {
	switch {
	case p == "":
		return errors.New("log path must not be empty")
	case !strings.HasPrefix(p, "/"):
		return fmt.Errorf("log path %q must be absolute", p)
	case strings.HasSuffix(p, "/"):
		return fmt.Errorf("log path %q must name files, not end with \"/\"", p)
	}
	for _, c := range p {
		if c < ' ' || c == 0x7f {
			return fmt.Errorf("log path %q must not contain control characters", p)
		}
		if strings.ContainsRune("\"'\\{}#", c) {
			return fmt.Errorf("log path %q must not contain %q", p, string(c))
		}
	}
	for _, part := range strings.Split(p[1:], "/") {
		switch part {
		case "":
			return fmt.Errorf("log path %q must not contain \"//\"", p)
		case ".", "..":
			return fmt.Errorf("log path %q must not contain %q components", p, part)
		}
	}
	return nil
}

// logrotateScriptKeywords start or end a script; they can't be extra
// directives, which are single lines.
var logrotateScriptKeywords = []string{"postrotate", "prerotate", "firstaction", "lastaction", "preremove", "endscript"}

// logrotateKeywordAttrs maps the directives that the typed attributes
// render to the attribute. An extra directive may use one of them only if
// the attribute is not set.
var logrotateKeywordAttrs = map[string]string{
	"hourly": "frequency", "daily": "frequency", "weekly": "frequency", "monthly": "frequency", "yearly": "frequency",
	"rotate":   "rotate",
	"compress": "compress", "nocompress": "compress",
	"delaycompress": "delaycompress", "nodelaycompress": "delaycompress",
	"missingok": "missingok", "nomissingok": "missingok",
	"notifempty": "notifempty", "ifempty": "notifempty",
	"create": "create_mode", "nocreate": "create_mode",
	"sharedscripts": "sharedscripts", "nosharedscripts": "sharedscripts",
}

// logrotateOffKeywords are the directives that turn off what the flag
// attributes turn on.
var logrotateOffKeywords = map[string]bool{
	"nocompress": true, "nodelaycompress": true, "nomissingok": true, "ifempty": true, "nosharedscripts": true,
}

// logrotateKeyword returns the directive name of line: its first field.
func logrotateKeyword(line string) string {
	if f := strings.Fields(line); len(f) > 0 {
		return f[0]
	}
	return ""
}

// validateLogrotateDirective reports why d cannot be an extra directive:
// one line inside the block, such as "maxsize 100M" or "su root adm".
func validateLogrotateDirective(d string) error {
	switch {
	case d == "":
		return errors.New("directive must not be empty")
	case strings.TrimSpace(d) != d:
		return fmt.Errorf("directive %q must not start or end with white space", d)
	case strings.HasPrefix(d, "#"):
		return fmt.Errorf("directive %q must not start with \"#\", which starts a comment", d)
	case strings.ContainsAny(d, "{}"):
		return fmt.Errorf("directive %q must not contain \"{\" or \"}\", which start and end blocks", d)
	}
	for _, c := range d {
		if (c < ' ' && c != '\t') || c == 0x7f {
			return fmt.Errorf("directive %q must be a single line without control characters", d)
		}
	}
	switch kw := logrotateKeyword(d); {
	case kw == "include":
		return fmt.Errorf("directive %q is not allowed: include is not valid inside a block", d)
	case slices.Contains(logrotateScriptKeywords, kw):
		return fmt.Errorf("directive %q is not allowed: scripts span several lines; use the postrotate attribute, and manage files with other scripts with sysutils_file", d)
	}
	return nil
}

// validateLogrotateScript reports why s cannot be the postrotate script.
func validateLogrotateScript(s string) error {
	switch {
	case strings.TrimSpace(s) == "":
		return errors.New("script must not be empty")
	case strings.ContainsRune(s, 0):
		return errors.New("script must not contain NUL bytes")
	case len(s) > maxLogrotateFileSize/2:
		return fmt.Errorf("script must be at most %d bytes long", maxLogrotateFileSize/2)
	}
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) == "endscript" {
			return errors.New("script must not contain a line \"endscript\", which would end it early")
		}
	}
	return nil
}

// validateLogrotateFrequency reports why f is not a frequency.
func validateLogrotateFrequency(f string) error {
	if !slices.Contains(logrotateFrequencies, f) {
		return fmt.Errorf("frequency %q must be one of %q", f, logrotateFrequencies)
	}
	return nil
}

// logrotateSpec is the block of a drop-in. Unset optional values (nil or
// "") are not written, so logrotate uses its global setting.
type logrotateSpec struct {
	paths     []string
	frequency string
	rotate    *int64

	compress, delaycompress, missingok, notifempty, sharedscripts *bool

	createMode, createOwner, createGroup string
	postrotate                           string
	extra                                []string
}

// validate checks every field of s, so that the rendered file cannot say
// more than s does. The errors name the attribute at fault.
func (s *logrotateSpec) validate() (attr string, err error) {
	if len(s.paths) == 0 {
		return "paths", errors.New("at least one log path is required")
	}
	seen := map[string]bool{}
	for _, p := range s.paths {
		if err := validateLogrotatePath(p); err != nil {
			return "paths", err
		}
		if seen[p] {
			return "paths", fmt.Errorf("log path %q is listed twice; logrotate refuses duplicate log entries", p)
		}
		seen[p] = true
	}
	if s.frequency != "" {
		if err := validateLogrotateFrequency(s.frequency); err != nil {
			return "frequency", err
		}
	}
	if s.rotate != nil && (*s.rotate < -1 || *s.rotate > maxLogrotateRotate) {
		return "rotate", fmt.Errorf("rotate must be between -1 and %d, got %d", maxLogrotateRotate, *s.rotate)
	}
	if s.createMode != "" {
		if err := validateOctalMode(s.createMode); err != nil {
			return "create_mode", err
		}
	}
	for _, f := range []struct{ attr, v string }{{"create_owner", s.createOwner}, {"create_group", s.createGroup}} {
		if f.v != "" {
			if err := validateAccountName(f.v); err != nil {
				return f.attr, err
			}
		}
	}
	if s.postrotate != "" {
		if err := validateLogrotateScript(s.postrotate); err != nil {
			return "postrotate", err
		}
	}
	for _, d := range s.extra {
		if err := validateLogrotateDirective(d); err != nil {
			return "extra_directives", err
		}
	}
	return s.validateCombination()
}

// validateCombination checks the fields of s against each other: the
// parts of create, and the extra directives against the typed fields.
func (s *logrotateSpec) validateCombination() (attr string, err error) {
	if s.createOwner != "" && s.createMode == "" {
		return "create_owner", errors.New("setting create_owner requires create_mode: logrotate's create directive takes the mode first")
	}
	// logrotate 3.22 and later read "create <x> <y>" as an owner and a
	// group, so the owner can't be given without the group.
	if s.createOwner != "" && s.createGroup == "" {
		return "create_group", errors.New("setting create_owner requires create_group: logrotate 3.22 and later read \"create <mode> <owner>\" as an owner and a group")
	}
	if s.createGroup != "" && s.createOwner == "" {
		return "create_owner", errors.New("setting create_group requires create_owner: logrotate's create directive takes the owner before the group")
	}
	set := map[string]bool{
		"frequency": s.frequency != "", "rotate": s.rotate != nil, "compress": s.compress != nil,
		"delaycompress": s.delaycompress != nil, "missingok": s.missingok != nil, "notifempty": s.notifempty != nil,
		"create_mode": s.createMode != "", "sharedscripts": s.sharedscripts != nil,
	}
	for _, d := range s.extra {
		if a, ok := logrotateKeywordAttrs[logrotateKeyword(d)]; ok && set[a] {
			return "extra_directives", fmt.Errorf("directive %q conflicts with %s; set only one of them", d, a)
		}
	}
	return "", nil
}

// quoteLogrotatePath quotes p if it contains white space, which would
// otherwise separate paths.
func quoteLogrotatePath(p string) string {
	if strings.ContainsAny(p, " \t") {
		return `"` + p + `"`
	}
	return p
}

// render returns the drop-in for s, which must be valid.
func (s *logrotateSpec) render() string {
	var b strings.Builder
	b.WriteString(logrotateFileHeader + "\n")
	quoted := make([]string, len(s.paths))
	for i, p := range s.paths {
		quoted[i] = quoteLogrotatePath(p)
	}
	b.WriteString(strings.Join(quoted, " ") + " {\n")
	line := func(d string) { b.WriteString(logrotateIndent + d + "\n") }
	flag := func(v *bool, on, off string) {
		switch {
		case v == nil:
		case *v:
			line(on)
		default:
			line(off)
		}
	}
	if s.frequency != "" {
		line(s.frequency)
	}
	if s.rotate != nil {
		line("rotate " + strconv.FormatInt(*s.rotate, 10))
	}
	flag(s.compress, "compress", "nocompress")
	flag(s.delaycompress, "delaycompress", "nodelaycompress")
	flag(s.missingok, "missingok", "nomissingok")
	flag(s.notifempty, "notifempty", "ifempty")
	if s.createMode != "" {
		line(strings.TrimSpace(strings.Join([]string{"create", s.createMode, s.createOwner, s.createGroup}, " ")))
	}
	flag(s.sharedscripts, "sharedscripts", "nosharedscripts")
	for _, d := range s.extra {
		line(d)
	}
	if s.postrotate != "" {
		line("postrotate")
		b.WriteString(s.postrotate)
		if !strings.HasSuffix(s.postrotate, "\n") {
			b.WriteString("\n")
		}
		line("endscript")
	}
	b.WriteString("}\n")
	return b.String()
}

// splitLogrotatePaths splits the part of a line before "{" into paths,
// honouring double and single quotes.
func splitLogrotatePaths(s string) ([]string, error) {
	var out []string
	for {
		s = strings.TrimLeft(s, " \t")
		if s == "" {
			return out, nil
		}
		if q := s[0]; q == '"' || q == '\'' {
			end := strings.IndexByte(s[1:], q)
			if end < 0 {
				return nil, fmt.Errorf("unterminated quote in %q", s)
			}
			out = append(out, s[1:1+end])
			s = s[end+2:]
			continue
		}
		end := strings.IndexAny(s, " \t")
		if end < 0 {
			end = len(s)
		}
		out = append(out, s[:end])
		s = s[end:]
	}
}

// parseLogrotateFile parses a drop-in with a single block back into a
// spec, for import. Comments and blank lines are skipped. Directives the
// typed attributes do not cover become extra directives. Scripts other
// than postrotate, several blocks and global directives are refused.
func parseLogrotateFile(content string) (*logrotateSpec, error) {
	s := &logrotateSpec{}
	lines := strings.Split(content, "\n")
	i := 0
	// The paths, up to the "{" that opens the block.
	opened := false
	for ; i < len(lines) && !opened; i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		before, after, found := strings.Cut(line, "{")
		if found && strings.TrimSpace(after) != "" {
			return nil, fmt.Errorf("line %d: unexpected text after \"{\"", i+1)
		}
		if !found && !strings.HasPrefix(line, "/") && !strings.HasPrefix(line, "\"") && !strings.HasPrefix(line, "'") {
			return nil, fmt.Errorf("line %d: %q is a global directive; sysutils_logrotate manages files with a single block of log paths only", i+1, line)
		}
		paths, err := splitLogrotatePaths(before)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		s.paths = append(s.paths, paths...)
		opened = found
	}
	if !opened {
		return nil, errors.New("no block of log paths found")
	}
	closed := false
	for ; i < len(lines) && !closed; i++ {
		line := strings.TrimSpace(lines[i])
		kw := logrotateKeyword(line)
		fields := strings.Fields(line)
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case line == "}":
			closed = true
		case line == "postrotate":
			if s.postrotate != "" {
				return nil, fmt.Errorf("line %d: a second postrotate script", i+1)
			}
			var script strings.Builder
			for i++; i < len(lines) && strings.TrimSpace(lines[i]) != "endscript"; i++ {
				script.WriteString(lines[i] + "\n")
			}
			if i == len(lines) {
				return nil, errors.New("postrotate script without endscript")
			}
			s.postrotate = script.String()
		case slices.Contains(logrotateScriptKeywords, kw):
			return nil, fmt.Errorf("line %d: %s scripts are not supported by sysutils_logrotate, only postrotate", i+1, kw)
		case len(fields) == 1 && slices.Contains(logrotateFrequencies, kw):
			s.frequency = kw
		case len(fields) == 2 && kw == "rotate":
			n, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("line %d: invalid rotate count %q", i+1, fields[1])
			}
			s.rotate = &n
		case len(fields) >= 2 && len(fields) <= 4 && kw == "create":
			s.createMode = fields[1]
			if len(fields) > 2 {
				s.createOwner = fields[2]
			}
			if len(fields) > 3 {
				s.createGroup = fields[3]
			}
		case len(fields) == 1 && logrotateKeywordAttrs[kw] != "" && kw != "create" && kw != "nocreate":
			v := !logrotateOffKeywords[kw]
			switch logrotateKeywordAttrs[kw] {
			case "compress":
				s.compress = &v
			case "delaycompress":
				s.delaycompress = &v
			case "missingok":
				s.missingok = &v
			case "notifempty":
				s.notifempty = &v
			case "sharedscripts":
				s.sharedscripts = &v
			}
		default:
			s.extra = append(s.extra, line)
		}
	}
	if !closed {
		return nil, errors.New("the block is not closed with \"}\"")
	}
	for ; i < len(lines); i++ {
		if line := strings.TrimSpace(lines[i]); line != "" && !strings.HasPrefix(line, "#") {
			return nil, fmt.Errorf("line %d: content after the block; sysutils_logrotate manages files with a single block only", i+1)
		}
	}
	if attr, err := s.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", attr, err)
	}
	return s, nil
}

// logrotateConfig is the provider-level configuration of
// sysutils_logrotate. The zero value, or a nil pointer, selects
// /etc/logrotate.d and the host's logrotate; tests set a temporary directory
// or a fake logrotate.
type logrotateConfig struct {
	dir string
	// run runs logrotate; nil selects runCommand.
	run commandRunner
	// lookPath finds logrotate; nil selects exec.LookPath.
	lookPath func(string) (string, error)
}

func (c *logrotateConfig) directory() string {
	if c == nil || c.dir == "" {
		return defaultLogrotateDir
	}
	return c.dir
}

// filePath returns the path of the drop-in name, which must be valid.
func (c *logrotateConfig) filePath(name string) string {
	return filepath.Join(c.directory(), name)
}

func (c *logrotateConfig) runner() commandRunner {
	if c == nil || c.run == nil {
		return runCommand
	}
	return c.run
}

// logrotate returns the logrotate binary on the host, or "" if there is
// none.
func (c *logrotateConfig) logrotate() string {
	lookPath := exec.LookPath
	if c != nil && c.lookPath != nil {
		lookPath = c.lookPath
	}
	bin, err := lookPath("logrotate")
	if err != nil {
		return ""
	}
	return bin
}

// logrotateError is returned by checkLogrotateFile when logrotate rejects
// a file.
type logrotateError struct {
	output string
}

func (e *logrotateError) Error() string {
	return "logrotate rejected the file:\n" + e.output
}

// checkLogrotateFile runs "logrotate -d" on tmp, which only parses the file
// and prints what rotation would do, with a state file that is never
// written. logrotate also reports log files it can't rotate, such as
// missing ones, and exits with status 1 for those too, so the exit status
// is ignored; only errors about the configuration file itself, which name
// it or say that a block was skipped, and warnings about one of its lines,
// such as an ignored unknown directive, reject it. They are returned as a *logrotateError, with display
// in place of tmp.
func checkLogrotateFile(ctx context.Context, run commandRunner, bin, tmp, display string) error {
	argv := []string{bin, "-d", "-s", "/dev/null", tmp}
	res, err := run(ctx, execSpec{
		Argv:           argv,
		Env:            []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"},
		Timeout:        logrotateTimeout,
		MaxOutputBytes: logrotateOutputLimit,
	})
	if err != nil {
		return fmt.Errorf("running %s: %w", strings.Join(argv, " "), err)
	}
	if res.TimedOut {
		return fmt.Errorf("%s timed out after %s", strings.Join(argv, " "), logrotateTimeout)
	}
	var errs []string
	for _, line := range strings.Split(res.Stdout.String()+"\n"+res.Stderr.String(), "\n") {
		line = strings.TrimSpace(line)
		if msg, ok := strings.CutPrefix(line, "error: "); ok {
			if strings.Contains(msg, tmp) || strings.HasPrefix(msg, "found error in ") {
				errs = append(errs, "error: "+strings.ReplaceAll(msg, tmp, display))
			}
			continue
		}
		// logrotate 3.22 and later only warn about an unknown directive,
		// and ignore the line: a typo all the same.
		if msg, ok := strings.CutPrefix(line, "warning: "); ok && strings.HasPrefix(msg, tmp+":") {
			errs = append(errs, "warning: "+strings.ReplaceAll(msg, tmp, display))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return &logrotateError{output: strings.ToValidUTF8(strings.Join(errs, "\n"), "�")}
}
