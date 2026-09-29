package provider

// Kernel parameter handling behind sysutils_sysctl. Keys are validated and
// mapped to files below /proc/sys, which is injectable so that unit tests can
// use a temporary directory instead; sysctl.d files are parsed and edited as
// text, keeping comments and all other entries.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	// defaultProcSys is where the kernel exposes its parameters.
	defaultProcSys = "/proc/sys"
	// defaultSysctlFile is the sysctl.d file sysutils_sysctl persists
	// parameters in. The 99- prefix sorts it after distribution defaults.
	defaultSysctlFile = "/etc/sysctl.d/99-terraform.conf"
	// maxSysctlNameLen bounds the length of a key. The longest keys the
	// kernel has, with a 15-character interface name, are about 70 bytes.
	maxSysctlNameLen = 256
	// maxSysctlValueLen bounds the value read from or written to /proc/sys.
	maxSysctlValueLen = 64 << 10
	// maxSysctlFileSize bounds how much of a sysctl.d file is read.
	maxSysctlFileSize = 4 << 20
	// sysctlFileCreateMode is the mode of a sysctl.d file created by
	// sysutils_sysctl.
	sysctlFileCreateMode fs.FileMode = 0o644
)

// sysctlConfig is the provider-level configuration of sysutils_sysctl. The
// zero value, or a nil pointer, selects the real /proc/sys.
type sysctlConfig struct {
	procSys string
}

func (c *sysctlConfig) root() string {
	if c == nil || c.procSys == "" {
		return defaultProcSys
	}
	return c.procSys
}

// errSysctlNotFound is returned when the kernel has no parameter of the
// given name, for example because the module providing it is not loaded or
// the network interface it belongs to does not exist.
var errSysctlNotFound = errors.New("no such kernel parameter")

// sysctlNameChar reports whether c may appear in a sysctl key. Besides the
// characters of kernel parameter names, this covers those of network
// interface names that appear as path components, such as "eth0.100"
// (written "eth0/100", see sysctlComponents) or "br-lan".
func sysctlNameChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '_' || c == '-' || c == '.' || c == '/' || c == '@' || c == '+'
}

// sysctlComponents splits a key in the dotted form used by sysctl(8), such as
// "net.ipv4.conf.all.forwarding", into the path components below /proc/sys.
// As in sysctl(8), a "/" inside a component stands for a literal ".", so the
// VLAN interface "eth0.100" is written "net.ipv4.conf.eth0/100.forwarding".
//
// The result can only name a file below /proc/sys: components are never
// empty, ".", or "..", and consist of a small set of characters that
// excludes the path separator.
func sysctlComponents(name string) ([]string, error) {
	if name == "" {
		return nil, errors.New("name must not be empty")
	}
	if len(name) > maxSysctlNameLen {
		return nil, fmt.Errorf("name must be at most %d bytes long", maxSysctlNameLen)
	}
	for i := 0; i < len(name); i++ {
		if !sysctlNameChar(name[i]) {
			return nil, fmt.Errorf("name %q contains the character %q; only letters, digits and \"_-.@+/\" are allowed", name, name[i])
		}
	}
	parts := strings.Split(name, ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("name %q must be in dotted form with at least two components, such as \"net.ipv4.ip_forward\"", name)
	}
	for i, p := range parts {
		p = strings.ReplaceAll(p, "/", ".")
		switch p {
		case "":
			return nil, fmt.Errorf("name %q has an empty component", name)
		case ".", "..":
			return nil, fmt.Errorf("name %q has a component %q that would leave /proc/sys", name, strings.ReplaceAll(p, ".", "/"))
		}
		parts[i] = p
	}
	return parts, nil
}

// validateSysctlName reports why name is not an acceptable sysctl key.
func validateSysctlName(name string) error {
	_, err := sysctlComponents(name)
	return err
}

// validateSysctlValue reports why v cannot be written to /proc/sys and to a
// sysctl.d file as a single line.
func validateSysctlValue(v string) error {
	if strings.ContainsAny(v, "\n\r\x00") {
		return errors.New("value must not contain line breaks or NUL bytes")
	}
	if len(v) > maxSysctlValueLen {
		return fmt.Errorf("value must be at most %d bytes long", maxSysctlValueLen)
	}
	if strings.TrimSpace(v) != v {
		return errors.New("value must not start or end with white space, which sysctl.d files cannot represent")
	}
	return nil
}

// normalizeSysctlValue returns v with runs of white space collapsed to one
// space. The kernel separates the fields of vector values such as
// net.ipv4.ip_local_port_range with tabs; configurations use spaces.
func normalizeSysctlValue(v string) string {
	return strings.Join(strings.Fields(v), " ")
}

// sysctlValuesEqual reports whether two values are the same setting.
func sysctlValuesEqual(a, b string) bool {
	return normalizeSysctlValue(a) == normalizeSysctlValue(b)
}

// openSysctl opens the file of the kernel parameter name below root. Besides
// the lexical checks of sysctlComponents, it is resolved with RESOLVE_BENEATH
// and without following symlinks where the kernel supports openat2(2), and
// must be a regular file.
func openSysctl(root, name string, flag int) (*os.File, error) {
	parts, err := sysctlComponents(name)
	if err != nil {
		return nil, err
	}
	rel := filepath.Join(parts...)
	display := filepath.Join(root, rel)
	dirfd, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", root, err)
	}
	defer func() { _ = unix.Close(dirfd) }()

	fd, err := unix.Openat2(dirfd, rel, &unix.OpenHow{
		Flags:   uint64(flag | unix.O_CLOEXEC | unix.O_NOFOLLOW),
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
	if errors.Is(err, unix.ENOSYS) {
		// Before Linux 5.6. The components contain no "..", and there are
		// no symlinks in /proc/sys; O_NOFOLLOW guards the last component.
		fd, err = unix.Openat(dirfd, rel, flag|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	}
	switch {
	case errors.Is(err, unix.ENOENT), errors.Is(err, unix.ENOTDIR):
		return nil, fmt.Errorf("%s: %w", name, errSysctlNotFound)
	case err != nil:
		return nil, fmt.Errorf("opening %s: %w", display, err)
	}
	f := os.NewFile(uintptr(fd), display)
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		if info.IsDir() {
			return nil, fmt.Errorf("%s is a group of kernel parameters, not a single parameter", name)
		}
		return nil, fmt.Errorf("%s is not a regular file", display)
	}
	return f, nil
}

// readSysctl returns the current value of the kernel parameter name, with
// the trailing newline removed and white space normalized.
func readSysctl(root, name string) (string, error) {
	f, err := openSysctl(root, name, unix.O_RDONLY)
	if err != nil {
		return "", sysctlAccessError(name, "reading", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxSysctlValueLen+1))
	if err != nil {
		return "", sysctlAccessError(name, "reading", err)
	}
	if len(data) > maxSysctlValueLen {
		return "", fmt.Errorf("value of %s is longer than %d bytes", name, maxSysctlValueLen)
	}
	return normalizeSysctlValue(string(data)), nil
}

// writeSysctl sets the kernel parameter name to value with a single write,
// as sysctl(8) does.
func writeSysctl(root, name, value string) error {
	f, err := openSysctl(root, name, unix.O_WRONLY|unix.O_TRUNC)
	if err != nil {
		return sysctlAccessError(name, "writing", err)
	}
	if _, err := f.Write([]byte(value + "\n")); err != nil {
		_ = f.Close()
		return sysctlAccessError(name, "writing", err)
	}
	return sysctlAccessError(name, "writing", f.Close())
}

// sysctlAccessError explains the errors the kernel returns for /proc/sys.
func sysctlAccessError(name, op string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errSysctlNotFound):
		return err
	case errors.Is(err, syscall.EROFS):
		return fmt.Errorf("%s %s: /proc/sys is mounted read-only, as is usual in containers: %w", op, name, err)
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		if op == "reading" {
			return fmt.Errorf("%s %s: permission denied; the parameter may be write-only, which sysutils_sysctl does not support: %w", op, name, err)
		}
		return fmt.Errorf("%s %s: permission denied; setting kernel parameters requires root, and in a container only some parameters can be set: %w", op, name, err)
	case errors.Is(err, syscall.EINVAL):
		return fmt.Errorf("%s %s: the kernel rejected the value as invalid: %w", op, name, err)
	default:
		return fmt.Errorf("%s %s: %w", op, name, err)
	}
}

// sysctlFileEntry is an assignment in a sysctl.d file.
type sysctlFileEntry struct {
	key, value string
}

// parseSysctlLine parses one line of a sysctl.d file as systemd-sysctl and
// sysctl(8) do. Comments start with "#" or ";", and a leading "-" (ignore
// errors) is not part of the key. Keys in the slash-separated form, such as
// "net/ipv4/ip_forward", are converted to the dotted form.
func parseSysctlLine(line string) (sysctlFileEntry, bool) {
	s := strings.TrimSpace(line)
	if s == "" || s[0] == '#' || s[0] == ';' {
		return sysctlFileEntry{}, false
	}
	key, value, ok := strings.Cut(s, "=")
	if !ok {
		return sysctlFileEntry{}, false
	}
	key = strings.TrimPrefix(strings.TrimSpace(key), "-")
	return sysctlFileEntry{key: canonicalSysctlKey(key), value: strings.TrimSpace(value)}, true
}

// canonicalSysctlKey converts a key in the slash-separated form, recognized
// by a "/" before the first ".", to the dotted form by swapping the two.
func canonicalSysctlKey(key string) string {
	i := strings.IndexAny(key, "./")
	if i < 0 || key[i] == '.' {
		return key
	}
	return strings.Map(func(r rune) rune {
		switch r {
		case '/':
			return '.'
		case '.':
			return '/'
		}
		return r
	}, key)
}

// findSysctlEntries returns the indexes of the lines of lines that assign
// the parameter name.
func findSysctlEntries(lines []string, name string) []int {
	var idx []int
	for i, l := range lines {
		if e, ok := parseSysctlLine(l); ok && e.key == name {
			idx = append(idx, i)
		}
	}
	return idx
}

// lookupSysctlEntry returns the value that t assigns to name and the number
// of assignments. As with systemd-sysctl and sysctl(8), the last one wins.
func lookupSysctlEntry(t *textFile, name string) (value string, count int) {
	idx := findSysctlEntries(t.lines, name)
	if len(idx) == 0 {
		return "", 0
	}
	e, _ := parseSysctlLine(t.lines[idx[len(idx)-1]])
	return e.value, len(idx)
}

// setSysctlEntry makes t assign value to name exactly once, replacing the
// first existing assignment in place and removing the others, or appending
// one. An assignment of an equal value is left as written. It reports
// whether t changed.
func setSysctlEntry(t *textFile, name, value string) bool {
	idx := findSysctlEntries(t.lines, name)
	if len(idx) == 0 {
		t.insert(len(t.lines), []string{name + " = " + value})
		return true
	}
	changed := false
	for i := len(idx) - 1; i > 0; i-- {
		t.replace(idx[i], idx[i]+1, nil)
		changed = true
	}
	if e, _ := parseSysctlLine(t.lines[idx[0]]); !sysctlValuesEqual(e.value, value) {
		t.lines[idx[0]] = name + " = " + value
		changed = true
	}
	return changed
}

// removeSysctlEntries removes every assignment of name from t and reports
// whether t changed.
func removeSysctlEntries(t *textFile, name string) bool {
	idx := findSysctlEntries(t.lines, name)
	for i := len(idx) - 1; i >= 0; i-- {
		t.replace(idx[i], idx[i]+1, nil)
	}
	return len(idx) > 0
}

// readSysctlFile reads and parses a sysctl.d file. A missing file reads as
// empty with a nil snapshot.
func readSysctlFile(p string) (*textFile, *fileSnapshot, error) {
	data, snap, err := readRegularFileNoFollow(p, maxSysctlFileSize)
	if errors.Is(err, fs.ErrNotExist) {
		return &textFile{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return parseTextFile(data), snap, nil
}

// editSysctlFile applies edit to the sysctl.d file at p and writes the result
// atomically if it changed, keeping the file's mode, ownership and extended
// attributes. A missing file is created, along with its parent directory,
// only if edit adds something to it, and a file left without any lines is
// removed. Concurrent edits by other sysutils resources are serialized.
func editSysctlFile(p string, edit func(t *textFile) bool) error {
	unlock := lockFileForEdit(p)
	defer unlock()
	t, snap, err := readSysctlFile(p)
	if err != nil {
		return err
	}
	if !edit(t) {
		return nil
	}
	if len(t.lines) == 0 {
		if err := checkUnchanged(p, snap); err != nil {
			return err
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		syncDir(filepath.Dir(p))
		return nil
	}
	if snap == nil {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
	}
	return replaceFileAtomic(p, t.bytes(), snap, sysctlFileCreateMode)
}
