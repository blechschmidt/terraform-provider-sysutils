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
	"slices"
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
	return sysctlComponentsMin(name, 2)
}

// sysctlComponentsMin is sysctlComponents for keys with at least min
// components; a prefix such as "vm" names a whole group with one.
func sysctlComponentsMin(name string, min int) ([]string, error) {
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
	if len(parts) < min {
		return nil, fmt.Errorf("name %q must be in dotted form with at least two components, such as \"net.ipv4.ip_forward\"", name)
	}
	if strings.Contains(parts[0], "/") {
		// sysctl(8) and systemd-sysctl read a key whose first separator is
		// "/" in the slash-separated form (see canonicalSysctlKey), so the
		// persisted entry would set a different parameter than the one
		// written to /proc/sys. No top-level directory has a "." anyway.
		return nil, fmt.Errorf("name %q must not contain \"/\" in its first component", name)
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

// validateSysctlPrefix reports why prefix does not name a group of kernel
// parameters, such as "vm" or "net.ipv4.conf.all".
func validateSysctlPrefix(prefix string) error {
	_, err := sysctlComponentsMin(prefix, 1)
	return err
}

// validateSysctlFile reports why p cannot be the sysctl.d file entries are
// persisted in. systemd-sysctl and sysctl --system only read files ending in
// ".conf", so any other file would never take effect; requiring the suffix
// also keeps the resource from appending entries to unrelated files such as
// /etc/passwd.
func validateSysctlFile(p string) error {
	if err := validateAbsolutePath(p); err != nil {
		return err
	}
	if !strings.HasSuffix(p, ".conf") || filepath.Base(p) == ".conf" {
		return fmt.Errorf("file %q must be a sysctl.d file with a name ending in \".conf\", such as %q", p, defaultSysctlFile)
	}
	return nil
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
	f, info, err := openSysctlPath(root, name, parts, flag)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		if info.IsDir() {
			return nil, fmt.Errorf("%s is a group of kernel parameters, not a single parameter", name)
		}
		return nil, fmt.Errorf("%s is not a regular file", f.Name())
	}
	return f, nil
}

// openSysctlPath opens the file or directory of the components parts of
// the key name below root, resolved with RESOLVE_BENEATH and without
// following symlinks where the kernel supports openat2(2).
func openSysctlPath(root, name string, parts []string, flag int) (*os.File, fs.FileInfo, error) {
	rel := filepath.Join(parts...)
	display := filepath.Join(root, rel)
	dirfd, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("opening %s: %w", root, err)
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
		return nil, nil, fmt.Errorf("%s: %w", name, errSysctlNotFound)
	case err != nil:
		return nil, nil, fmt.Errorf("opening %s: %w", display, err)
	}
	f := os.NewFile(uintptr(fd), display)
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return f, info, nil
}

// readSysctl returns the current value of the kernel parameter name, with
// the trailing newline removed and white space normalized.
func readSysctl(root, name string) (string, error) {
	f, err := openSysctl(root, name, unix.O_RDONLY)
	if err != nil {
		return "", sysctlAccessError(name, "reading", err)
	}
	defer func() { _ = f.Close() }()
	return readSysctlFD(f, name)
}

// readSysctlFD reads the value of the open parameter file f, as readSysctl
// does.
func readSysctlFD(f *os.File, name string) (string, error) {
	data, err := io.ReadAll(io.LimitReader(f, maxSysctlValueLen+1))
	if err != nil {
		return "", sysctlAccessError(name, "reading", err)
	}
	if len(data) > maxSysctlValueLen {
		return "", fmt.Errorf("value of %s is longer than %d bytes", name, maxSysctlValueLen)
	}
	return normalizeSysctlValue(string(data)), nil
}

// listSysctl returns the values of every kernel parameter at or below the
// key prefix, which may name a group such as "net.ipv4.conf.all" or a single
// parameter, like "sysctl -a" restricted to it. Parameters that cannot be
// read, such as write-only ones or net.ipv6.conf.*.stable_secret while it
// is unset, are left out, as are other file systems mounted inside /proc/sys
// (binfmt_misc). So are parameters that only their owner (root) may read
// (see sysctlOwnerOnly): those include secrets such as
// net.ipv4.tcp_fastopen_key and net.ipv6.conf.*.stable_secret, which would
// otherwise end up in the Terraform state. A prefix that does not exist
// yields errSysctlNotFound.
func listSysctl(root, prefix string) (map[string]string, error) {
	parts, err := sysctlComponentsMin(prefix, 1)
	if err != nil {
		return nil, err
	}
	f, info, err := openSysctlPath(root, prefix, parts, unix.O_RDONLY)
	if err != nil {
		return nil, sysctlAccessError(prefix, "reading", err)
	}
	defer func() { _ = f.Close() }()
	values := map[string]string{}
	if info.Mode().IsRegular() {
		if sysctlOwnerOnly(uint32(info.Mode().Perm())) {
			return values, nil
		}
		if v, err := readSysctlFD(f, prefix); err == nil {
			values[prefix] = v
		}
		return values, nil
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is neither a kernel parameter nor a group of them", f.Name())
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("cannot determine the file system of %s", f.Name())
	}
	w := sysctlWalker{dev: uint64(st.Dev), values: values} //nolint:unconvert // Dev is not uint64 on every platform.
	if err := w.walk(f, prefix, 0); err != nil {
		return nil, err
	}
	return values, nil
}

const (
	// maxSysctlDepth bounds how deep listSysctl descends below /proc/sys.
	// The deepest keys, such as net.ipv4.conf.<if>.<param> or
	// net.netfilter.nf_log.<n>, are about six levels deep.
	maxSysctlDepth = 16
	// maxSysctlEntries bounds how many parameters listSysctl returns. A host
	// with many network interfaces has tens of thousands.
	maxSysctlEntries = 1 << 20
)

// sysctlWalker collects parameter values below a directory of /proc/sys.
type sysctlWalker struct {
	dev    uint64
	values map[string]string
}

// walk reads the parameters in the open directory dir, whose key is key,
// and descends into its subdirectories.
func (w *sysctlWalker) walk(dir *os.File, key string, depth int) error {
	if depth > maxSysctlDepth {
		return fmt.Errorf("%s: groups of kernel parameters nested more than %d levels deep", dir.Name(), maxSysctlDepth)
	}
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return fmt.Errorf("listing %s: %w", dir.Name(), err)
	}
	slices.Sort(names)
	for _, name := range names {
		child, ok := sysctlKeyComponent(name)
		if !ok {
			continue // Cannot be named by a key; never the case in /proc/sys.
		}
		childKey := key + "." + child
		// Check the type first, without triggering an automount: binfmt_misc
		// is often an autofs mount point inside /proc/sys.
		var st unix.Stat_t
		if err := unix.Fstatat(int(dir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW|unix.AT_NO_AUTOMOUNT); err != nil {
			continue
		}
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFREG:
			if sysctlOwnerOnly(st.Mode) {
				continue // Possibly a secret; see listSysctl.
			}
		case unix.S_IFDIR:
			if uint64(st.Dev) != w.dev { //nolint:unconvert // Dev is not uint64 on every platform.
				continue // Another file system, such as binfmt_misc.
			}
		default:
			continue // A symlink, FIFO or device; never the case in /proc/sys.
		}
		fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != nil {
			continue // Write-only, gone, or a symlink.
		}
		f := os.NewFile(uintptr(fd), filepath.Join(dir.Name(), name))
		err = w.visit(f, childKey, depth)
		_ = f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func (w *sysctlWalker) visit(f *os.File, key string, depth int) error {
	info, err := f.Stat()
	if err != nil {
		return nil
	}
	switch {
	case info.Mode().IsRegular():
		if len(w.values) >= maxSysctlEntries {
			return fmt.Errorf("more than %d kernel parameters below the prefix; use a longer one", maxSysctlEntries)
		}
		if v, err := readSysctlFD(f, key); err == nil {
			w.values[key] = v
		}
	case info.IsDir():
		if st, ok := info.Sys().(*syscall.Stat_t); !ok || uint64(st.Dev) != w.dev { //nolint:unconvert // Dev is not uint64 on every platform.
			return nil // Another file system, such as binfmt_misc.
		}
		return w.walk(f, key, depth+1)
	}
	return nil
}

// sysctlOwnerOnly reports whether a /proc/sys file with the permission
// bits mode can be read by its owner only. The kernel makes secrets such as
// net.ipv4.tcp_fastopen_key and stable_secret readable only by root (mode
// 0600), unlike ordinary parameters (0644).
func sysctlOwnerOnly(mode uint32) bool {
	return mode&0o044 == 0
}

// sysctlKeyComponent returns the key component for the file name name
// below /proc/sys: a "." in it, as in the interface name "eth0.100", is
// written "/" in keys (see sysctlComponents).
func sysctlKeyComponent(name string) (string, bool) {
	if name == "" || name == "." || name == ".." {
		return "", false
	}
	for i := 0; i < len(name); i++ {
		if !sysctlNameChar(name[i]) || name[i] == '/' {
			return "", false
		}
	}
	return strings.ReplaceAll(name, ".", "/"), true
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
	unlock, err := lockFileForEdit(p)
	if err != nil {
		return err
	}
	defer unlock()
	t, snap, err := readSysctlFile(p)
	if err != nil {
		return err
	}
	if !edit(t) {
		return nil
	}
	if len(t.lines) == 0 {
		return removeManagedFile(p, snap)
	}
	return writeManagedFile(p, t.bytes(), snap, sysctlFileCreateMode)
}

// sysctlConfDirs are the directories that systemd-sysctl and
// "sysctl --system" read *.conf files from at boot, in order of precedence:
// of several files with the same name, only the one in the earliest
// directory is read.
var sysctlConfDirs = []string{"/etc/sysctl.d", "/run/sysctl.d", "/usr/local/lib/sysctl.d", "/usr/lib/sysctl.d", "/lib/sysctl.d"}

// sysctlConfFile is read after the sysctl.d files by "sysctl --system".
const sysctlConfFile = "/etc/sysctl.conf"

// persistedSysctl is the value a key is set to at boot, and the file of the
// assignment that takes effect.
type persistedSysctl struct {
	value, file string
}

// sysctlConfFiles returns the configuration files below root that set
// kernel parameters at boot, in the order they are applied: the *.conf
// files of sysctlConfDirs sorted by name, each name taken from the earliest
// directory that has it, and then sysctlConfFile.
func sysctlConfFiles(root *fsRoot) ([]string, error) {
	byName := map[string]string{}
	for _, dir := range sysctlConfDirs {
		host, err := root.resolveFollow(dir)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			continue
		}
		if err != nil {
			return nil, err
		}
		entries, err := os.ReadDir(host)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".conf") {
				continue
			}
			if _, ok := byName[name]; !ok {
				byName[name] = filepath.Join(dir, name)
			}
		}
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	slices.Sort(names)
	files := make([]string, 0, len(names)+1)
	for _, name := range names {
		files = append(files, byName[name])
	}
	return append(files, sysctlConfFile), nil
}

// readPersistedSysctl returns, for every key the configuration files below
// root assign, the value it is set to at boot. As with systemd-sysctl and
// sysctl(8), the last assignment wins. Values have their white space
// normalized. A file that is not a regular file, such as a sysctl.d file
// masked by a symlink to /dev/null, sets nothing.
func readPersistedSysctl(root *fsRoot) (map[string]persistedSysctl, error) {
	files, err := sysctlConfFiles(root)
	if err != nil {
		return nil, err
	}
	out := map[string]persistedSysctl{}
	for _, p := range files {
		host, err := root.resolveFollow(p)
		if err == nil {
			var info fs.FileInfo
			if info, err = os.Stat(host); err == nil && !info.Mode().IsRegular() {
				continue
			}
		}
		var data []byte
		if err == nil {
			data, err = readRootedFile(root, p, maxSysctlFileSize)
		}
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", p, err)
		}
		for _, line := range parseTextFile(data).lines {
			if e, ok := parseSysctlLine(line); ok {
				out[e.key] = persistedSysctl{value: normalizeSysctlValue(e.value), file: p}
			}
		}
	}
	return out, nil
}
