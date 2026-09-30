package provider

// Sudoers drop-in handling behind sysutils_sudoers: validation of drop-in
// names and rule fields, rendering of structured rules, and writing a
// drop-in file through replaceFileAtomicWith with a visudo check of the
// temporary file before it is renamed into place. The directory, the owner
// of the files and how visudo is found and run are injectable so that tests
// can use a temporary directory and a fake visudo.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// defaultSudoersDir is the directory that the stock sudoers of every
	// major distribution includes with @includedir (or #includedir).
	defaultSudoersDir = "/etc/sudoers.d"
	// sudoersFileMode is the mode of every drop-in the resource writes, as
	// visudo and the distributions' packages install them. sudo refuses
	// files writable by anyone but root.
	sudoersFileMode fs.FileMode = 0o440
	// sudoersDirMode is the mode a missing sudoers.d is created with.
	sudoersDirMode fs.FileMode = 0o750
	// maxSudoersFileSize bounds how much of a drop-in is read or written.
	maxSudoersFileSize = 1 << 20
	// maxSudoersNameLen is NAME_MAX, the longest file name.
	maxSudoersNameLen = 255
	// sudoersFileHeader starts every drop-in rendered from rules.
	sudoersFileHeader = "# Managed by Terraform (sysutils_sudoers). Manual changes will be reverted."
	// visudoTimeout bounds a visudo run.
	visudoTimeout = 30 * time.Second
	// visudoOutputLimit caps the output kept from visudo.
	visudoOutputLimit = 64 << 10
)

// validateSudoersName reports why name cannot name a drop-in in
// sudoers.d. sudo's @includedir silently skips files whose names contain a
// "." or end in "~", so such a drop-in would never take effect.
func validateSudoersName(name string) error {
	switch {
	case name == "":
		return errors.New("name must not be empty")
	case len(name) > maxSudoersNameLen:
		return fmt.Errorf("name must be at most %d bytes long", maxSudoersNameLen)
	case strings.ContainsRune(name, '/'):
		return fmt.Errorf("name %q must not contain \"/\"; it names a file directly in the sudoers.d directory", name)
	case strings.ContainsRune(name, '.'):
		return fmt.Errorf("name %q must not contain \".\"; sudo ignores files in sudoers.d whose names contain a \".\", such as \"admins.conf\"", name)
	case strings.HasSuffix(name, "~"):
		return fmt.Errorf("name %q must not end with \"~\"; sudo ignores files in sudoers.d whose names end with \"~\"", name)
	}
	for _, c := range name {
		if c <= ' ' || c == 0x7f {
			return fmt.Errorf("name %q must not contain white space or control characters", name)
		}
	}
	return nil
}

// validateSudoersContent reports why content cannot be written to a drop-in.
func validateSudoersContent(content string) error {
	switch {
	case strings.ContainsRune(content, 0):
		return errors.New("content must not contain NUL bytes")
	case len(content) > maxSudoersFileSize:
		return fmt.Errorf("content must be at most %d bytes long", maxSudoersFileSize)
	}
	return nil
}

// validateSudoersToken reports why s cannot be one of the users or hosts of
// a rule: a user, %group, #uid, alias or ALL, and a host name, address,
// network or alias. what is "user" or "host".
func validateSudoersToken(what string) func(string) error {
	return func(s string) error {
		if err := validateSudoersLine(what, s); err != nil {
			return err
		}
		if i := strings.IndexAny(s, " \t,=:()"); i >= 0 {
			return fmt.Errorf("%s %q must not contain %q; list several %ss as separate elements", what, s, string(s[i]), what)
		}
		return nil
	}
}

// validateSudoersRunas reports why s cannot be the Runas_Spec of a rule,
// written in parentheses: "user", "user:group" or ":group", where either
// part may be a comma-separated list.
func validateSudoersRunas(s string) error {
	if err := validateSudoersLine("runas", s); err != nil {
		return err
	}
	if i := strings.IndexAny(s, "()=#"); i >= 0 {
		return fmt.Errorf("runas %q must not contain %q; give what goes inside the parentheses, such as \"ALL:ALL\"", s, string(s[i]))
	}
	if strings.Count(s, ":") > 1 {
		return fmt.Errorf("runas %q must contain at most one \":\", between the users and the groups", s)
	}
	return nil
}

// validateSudoersCommand reports why s cannot be one of the commands of a
// rule. sudoers ends a command at an unescaped ",", ":" or "=", which would
// turn the rest into another command or rule, so they must be escaped with
// a backslash, as in "/usr/bin/kill -s TERM\\, 42".
func validateSudoersCommand(s string) error {
	if err := validateSudoersLine("command", s); err != nil {
		return err
	}
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++ // Skip the escaped character.
		case ',', ':', '=':
			return fmt.Errorf("command %q contains an unescaped %q, which sudoers would read as the start of another command or rule; escape it with a backslash (\"\\\\%c\" in HCL)", s, string(s[i]), s[i])
		}
	}
	return nil
}

// validateSudoersLine checks what every rule field has in common: it is a
// non-empty part of a single line.
func validateSudoersLine(what, s string) error {
	switch {
	case s == "":
		return fmt.Errorf("%s must not be empty", what)
	case strings.ContainsAny(s, "\n\r\x00"):
		return fmt.Errorf("%s %q must be a single line without NUL bytes", what, s)
	case strings.TrimSpace(s) != s:
		return fmt.Errorf("%s %q must not start or end with white space", what, s)
	case (len(s)-len(strings.TrimRight(s, "\\")))%2 == 1:
		return fmt.Errorf("%s %q must not end with a backslash, which continues the line", what, s)
	case strings.HasPrefix(s, "#") && what != "user":
		return fmt.Errorf("%s %q must not start with \"#\", which starts a comment", what, s)
	}
	return nil
}

// sudoersRule is one user specification of a drop-in rendered from rules.
type sudoersRule struct {
	users, hosts, commands []string
	// runas is the Runas_Spec without the parentheses, or "" for none.
	runas            string
	nopasswd, setenv bool
}

// validate checks all fields of r, so that the rendered line cannot say
// more than r does.
func (r *sudoersRule) validate() error {
	if len(r.users) == 0 || len(r.hosts) == 0 || len(r.commands) == 0 {
		return errors.New("a rule needs at least one user, host and command")
	}
	for _, u := range r.users {
		if err := validateSudoersToken("user")(u); err != nil {
			return err
		}
	}
	for _, h := range r.hosts {
		if err := validateSudoersToken("host")(h); err != nil {
			return err
		}
	}
	if r.runas != "" {
		if err := validateSudoersRunas(r.runas); err != nil {
			return err
		}
	}
	for _, c := range r.commands {
		if err := validateSudoersCommand(c); err != nil {
			return err
		}
	}
	return nil
}

// render returns the line of r:
//
//	users hosts = (runas) NOPASSWD: SETENV: command, command
func (r *sudoersRule) render() string {
	var b strings.Builder
	b.WriteString(strings.Join(r.users, ", "))
	b.WriteString(" ")
	b.WriteString(strings.Join(r.hosts, ", "))
	b.WriteString(" = ")
	if r.runas != "" {
		b.WriteString("(" + r.runas + ") ")
	}
	if r.nopasswd {
		b.WriteString("NOPASSWD: ")
	}
	if r.setenv {
		b.WriteString("SETENV: ")
	}
	b.WriteString(strings.Join(r.commands, ", "))
	return b.String()
}

// renderSudoersRules returns the drop-in for rules: the header and one line
// per rule.
func renderSudoersRules(rules []sudoersRule) string {
	var b strings.Builder
	b.WriteString(sudoersFileHeader + "\n")
	for i := range rules {
		b.WriteString(rules[i].render() + "\n")
	}
	return b.String()
}

// sudoersConfig is the provider-level configuration of sysutils_sudoers.
// The zero value, or a nil pointer, selects /etc/sudoers.d, files owned by
// root, and the host's visudo; tests set a temporary directory, their own
// user and a fake visudo.
type sudoersConfig struct {
	dir      string
	uid, gid uint32
	// run runs visudo; nil selects runCommand.
	run commandRunner
	// lookPath finds visudo on the host; nil selects exec.LookPath.
	lookPath func(string) (string, error)
	// lookPathInRoot finds visudo below root_dir; nil selects lookPathIn.
	lookPathInRoot func(*fsRoot) func(string) (string, error)
}

func (c *sudoersConfig) directory() string {
	if c == nil || c.dir == "" {
		return defaultSudoersDir
	}
	return c.dir
}

func (c *sudoersConfig) owner() (uid, gid uint32) {
	if c == nil {
		return 0, 0
	}
	return c.uid, c.gid
}

// filePath returns the path of the drop-in name, which must be valid.
func (c *sudoersConfig) filePath(name string) string {
	return filepath.Join(c.directory(), name)
}

func (c *sudoersConfig) runner() commandRunner {
	if c == nil || c.run == nil {
		return runCommand
	}
	return c.run
}

// errVisudoMissing is returned by sudoersConfig.visudo when visudo cannot
// be run.
var errVisudoMissing = errors.New("visudo not found")

// visudo returns the visudo to check drop-ins for root with. skipped is a
// reason not to check them, to be reported as a warning, when root is a
// root_dir tree without sudo, or the host lacks the visudo to check a
// tree's drop-ins with. Only the host's visudo is ever run: a binary in the
// tree could be anything, and running it as root would hand the host to
// whoever built the tree. Without visudo on the host itself, err wraps
// errVisudoMissing.
func (c *sudoersConfig) visudo(root *fsRoot) (bin, skipped string, err error) {
	lookPath := exec.LookPath
	if c != nil && c.lookPath != nil {
		lookPath = c.lookPath
	}
	if !root.isHost() {
		inRoot := lookPathIn
		if c != nil && c.lookPathInRoot != nil {
			inRoot = c.lookPathInRoot
		}
		if _, err := inRoot(root)("visudo"); err != nil {
			return "", fmt.Sprintf("visudo is not installed below root_dir %s, so sudo is presumably not installed in the tree either", root), nil
		}
		bin, err := lookPath("visudo")
		if err != nil {
			return "", fmt.Sprintf("visudo is installed below root_dir %s but not on the host running Terraform; the provider never runs programs from the tree", root), nil
		}
		return bin, "", nil
	}
	bin, err = lookPath("visudo")
	if err != nil {
		return "", "", fmt.Errorf("%w in PATH: %w", errVisudoMissing, err)
	}
	return bin, "", nil
}

// visudoError is returned by checkSudoersFile when visudo rejects a file.
type visudoError struct {
	output string
}

func (e *visudoError) Error() string {
	return "visudo rejected the file:\n" + e.output
}

// checkSudoersFile runs "visudo -cf tmp" and returns a *visudoError with its
// output if it rejects the file. display replaces tmp in the output, so
// that messages refer to the file being written rather than to the
// temporary file.
func checkSudoersFile(ctx context.Context, run commandRunner, bin, tmp, display string) error {
	argv := []string{bin, "-c", "-f", tmp}
	res, err := run(ctx, execSpec{
		Argv:           argv,
		Env:            []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"},
		Timeout:        visudoTimeout,
		MaxOutputBytes: visudoOutputLimit,
	})
	if err != nil {
		return fmt.Errorf("running %s: %w", strings.Join(argv, " "), err)
	}
	if res.TimedOut {
		return fmt.Errorf("%s timed out after %s", strings.Join(argv, " "), visudoTimeout)
	}
	if res.ExitCode == 0 {
		return nil
	}
	out := strings.TrimSpace(res.Stdout.String() + "\n" + res.Stderr.String())
	out = strings.ReplaceAll(out, tmp, display)
	if out == "" {
		out = fmt.Sprintf("visudo exited with status %d", res.ExitCode)
	}
	return &visudoError{output: strings.ToValidUTF8(out, "�")}
}

// readSudoersFile reads a drop-in without following symlinks. A missing file
// reads as nil data with a nil snapshot.
func readSudoersFile(p string) ([]byte, *fileSnapshot, error) {
	data, snap, err := readRegularFileNoFollow(p, maxSudoersFileSize)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	return data, snap, err
}

// errSudoersFileExists is returned by writeSudoersFile when a new drop-in
// already exists.
var errSudoersFileExists = errors.New("file already exists")

// writeSudoersFile atomically makes the drop-in at p contain data, with
// mode 0440 and owned by uid:gid, whatever the mode and owner of an existing
// file were. check, if not nil, is called with the complete temporary file
// before it is renamed into place; if it fails, p is left untouched. The
// temporary file's name starts with ".", so sudo never reads it. Extended
// attributes such as the SELinux label are carried over, except an ACL.
// With create set, an existing file is an error. A missing sudoers.d is
// created with mode 0750.
func writeSudoersFile(p string, data []byte, uid, gid uint32, create bool, check func(tmp string) error) error {
	unlock, err := lockFileForEdit(p)
	if err != nil {
		return err
	}
	defer unlock()
	_, snap, err := readSudoersFile(p)
	if err != nil {
		return err
	}
	if create && snap != nil {
		return fmt.Errorf("%s: %w", p, errSudoersFileExists)
	}
	attrs := replaceAttrs{mode: sudoersFileMode, chown: true, uid: uid, gid: gid, dropACL: true, check: check}
	if snap != nil {
		attrs.xattrs = make(map[string][]byte, len(snap.xattrs))
		for k, v := range snap.xattrs {
			if k != aclAccessXattr {
				attrs.xattrs[k] = v
			}
		}
	} else if err := ensureSudoersDir(filepath.Dir(p)); err != nil {
		return err
	}
	return replaceFileAtomicWith(p, data, snap, attrs)
}

// ensureSudoersDir creates dir with mode 0750, and its parents with mode
// 0755, if it does not exist.
func ensureSudoersDir(dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(dir, sudoersDirMode); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}

// removeSudoersFile removes the drop-in at p. A missing file is not an
// error; anything other than a regular file is left alone.
func removeSudoersFile(p string) error {
	unlock, err := lockFileForEdit(p)
	if err != nil {
		return err
	}
	defer unlock()
	info, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := checkRegularFile(p, info); err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	syncDir(filepath.Dir(p))
	return nil
}
