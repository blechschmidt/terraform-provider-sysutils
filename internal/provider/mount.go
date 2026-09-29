package provider

// Mount table and fstab handling behind sysutils_mount. Parsing and editing
// are pure functions; mounting and unmounting go through the mounter
// interface so that unit tests can substitute a fake.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// defaultFstabPath is the file sysutils_mount persists mounts in.
	defaultFstabPath = "/etc/fstab"
	// defaultMountInfoPath lists the mounts in the provider's mount namespace.
	defaultMountInfoPath = "/proc/self/mountinfo"
	// mountCommandTimeout bounds each mount and umount invocation, so that an
	// unreachable network file system cannot hang the apply forever.
	mountCommandTimeout = 2 * time.Minute
	// mountOutputLimit caps how much mount/umount output is kept per stream.
	mountOutputLimit = 64 << 10
	// maxFstabSize bounds how much of the fstab is read into memory.
	maxFstabSize = 4 << 20
	// fstabCreateMode is the mode of an fstab created by sysutils_mount.
	fstabCreateMode fs.FileMode = 0o644
)

// defaultMountOptions is the value of options when it is not configured.
var defaultMountOptions = []string{"defaults"}

// mountEntry is one line of /proc/self/mountinfo.
type mountEntry struct {
	mountPoint   string
	root         string
	major, minor uint32
	fstype       string
	source       string
	// options are the per-mount options, such as "rw,nosuid,relatime".
	options []string
	// superOptions are the per-superblock options, such as "rw,size=1024k".
	superOptions []string
}

// readOnly reports whether the mount is read-only, either on its own or
// because its superblock is.
func (e *mountEntry) readOnly() bool {
	return slices.Contains(e.options, "ro") || slices.Contains(e.superOptions, "ro")
}

// mounter mounts and unmounts file systems and lists the mount table.
// systemMounter is the production implementation.
type mounter interface {
	// mounts returns the current mount table in mount order, so that of
	// several mounts stacked on one mount point the topmost comes last.
	mounts() ([]mountEntry, error)
	mount(ctx context.Context, device, target, fstype string, options []string) error
	// remount applies options to the file system mounted at target.
	remount(ctx context.Context, device, target string, options []string) error
	// unmount unmounts the topmost file system mounted at target.
	unmount(ctx context.Context, target string) error
}

// mountConfig is the provider-level configuration of sysutils_mount. The
// zero value, or a nil pointer, selects /etc/fstab and the real mount(8).
type mountConfig struct {
	fstabPath string
	mounter   mounter
}

func (c *mountConfig) fstab() string {
	if c == nil || c.fstabPath == "" {
		return defaultFstabPath
	}
	return c.fstabPath
}

func (c *mountConfig) mnt() mounter {
	if c == nil || c.mounter == nil {
		return systemMounter{run: runCommand, mountInfo: defaultMountInfoPath, timeout: mountCommandTimeout}
	}
	return c.mounter
}

// systemMounter runs mount(8) and umount(8) and reads the mount table from
// mountinfo. mount(8) rather than mount(2) is used so that file system
// helpers (mount.nfs, mount.cifs, FUSE), device tags such as UUID= and
// userspace options such as _netdev work as they do in /etc/fstab.
type systemMounter struct {
	run       commandRunner
	mountInfo string
	timeout   time.Duration
}

func (m systemMounter) mounts() ([]mountEntry, error) {
	data, err := os.ReadFile(m.mountInfo)
	if err != nil {
		return nil, fmt.Errorf("reading mount table: %w", err)
	}
	return parseMountInfo(string(data))
}

func (m systemMounter) mount(ctx context.Context, device, target, fstype string, options []string) error {
	return m.do(ctx, "mount", "-t", fstype, "-o", strings.Join(options, ","), "--", device, target)
}

// remount passes both the device and the mount point, so that mount(8)
// replaces the mount options instead of merging them with /etc/fstab.
func (m systemMounter) remount(ctx context.Context, device, target string, options []string) error {
	return m.do(ctx, "mount", "-o", strings.Join(append([]string{"remount"}, options...), ","), "--", device, target)
}

func (m systemMounter) unmount(ctx context.Context, target string) error {
	return m.do(ctx, "umount", "--", target)
}

func (m systemMounter) do(ctx context.Context, argv ...string) error {
	return runSystemCommand(ctx, m.run, m.timeout, mountOutputLimit, argv...)
}

// parseMountInfo parses the format of /proc/<pid>/mountinfo, described in
// proc(5):
//
//	36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw,errors=continue
func parseMountInfo(data string) ([]mountEntry, error) {
	var entries []mountEntry
	for n, line := range strings.Split(data, "\n") {
		if line == "" {
			continue
		}
		fields := asciiFields(line)
		sep := slices.Index(fields, "-")
		if sep < 6 || len(fields) < sep+3 {
			return nil, fmt.Errorf("malformed mount table line %d: %q", n+1, line)
		}
		majStr, minStr, ok := strings.Cut(fields[2], ":")
		major, err1 := strconv.ParseUint(majStr, 10, 32)
		minor, err2 := strconv.ParseUint(minStr, 10, 32)
		if !ok || err1 != nil || err2 != nil {
			return nil, fmt.Errorf("malformed device number on mount table line %d: %q", n+1, fields[2])
		}
		e := mountEntry{
			root:       unescapeOctal(fields[3]),
			mountPoint: unescapeOctal(fields[4]),
			major:      uint32(major),
			minor:      uint32(minor),
			options:    strings.Split(fields[5], ","),
			fstype:     unescapeOctal(fields[sep+1]),
			source:     unescapeOctal(fields[sep+2]),
		}
		if len(fields) > sep+3 {
			e.superOptions = strings.Split(fields[sep+3], ",")
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// topMount returns the topmost mount at mountPoint, or nil.
func topMount(entries []mountEntry, mountPoint string) *mountEntry {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].mountPoint == mountPoint {
			return &entries[i]
		}
	}
	return nil
}

// lookupMount returns the topmost mount at mountPoint, or nil.
func lookupMount(m mounter, mountPoint string) (*mountEntry, error) {
	entries, err := m.mounts()
	if err != nil {
		return nil, err
	}
	return topMount(entries, mountPoint), nil
}

// asciiFields splits s at runs of spaces and tabs, the only separators of
// mountinfo and fstab fields (libmount and the kernel escape them as \040
// and \011). strings.Fields would also split at Unicode spaces such as
// U+00A0, which may appear unescaped in a field: a FUSE mount point chosen
// by an unprivileged user could then shift the fields of its line and be
// mistaken for another mount.
func asciiFields(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == '\t' })
}

// unescapeOctal decodes the \ooo escapes used for white space and
// backslashes in mountinfo and fstab fields.
func unescapeOctal(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && isOctal(s[i+1]) && isOctal(s[i+2]) && isOctal(s[i+3]) {
			v := (s[i+1]-'0')<<6 | (s[i+2]-'0')<<3 | (s[i+3] - '0')
			b.WriteByte(v)
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }

// escapeFstabField escapes the characters that would otherwise end or
// comment out an fstab field.
func escapeFstabField(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= ' ' || c == '\\' || c == '#' || c == 0x7f {
			fmt.Fprintf(&b, `\%03o`, c)
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// fstabEntry is one entry of /etc/fstab, as described in fstab(5).
type fstabEntry struct {
	device     string
	mountPoint string
	fstype     string
	options    []string
	dump, pass int64
}

// String formats e as an fstab line with fields separated by single spaces.
func (e fstabEntry) String() string {
	return strings.Join([]string{
		escapeFstabField(e.device),
		escapeFstabField(e.mountPoint),
		escapeFstabField(e.fstype),
		escapeFstabField(strings.Join(e.options, ",")),
		strconv.FormatInt(e.dump, 10),
		strconv.FormatInt(e.pass, 10),
	}, " ")
}

func (e fstabEntry) equal(o fstabEntry) bool {
	return e.device == o.device && e.mountPoint == o.mountPoint && e.fstype == o.fstype &&
		slices.Equal(e.options, o.options) && e.dump == o.dump && e.pass == o.pass
}

// parseFstabLine parses an fstab line. Blank lines and comments yield
// ok=false. Missing trailing fields get their fstab(5) defaults; a dump or
// pass field that is not a number is reported as -1, so that it never
// equals a configured value and is rewritten by the next apply. Fields after
// the sixth are ignored by mount(8) and here.
func parseFstabLine(line string) (e fstabEntry, ok bool) {
	fields := asciiFields(line)
	if len(fields) < 2 || strings.HasPrefix(fields[0], "#") {
		return fstabEntry{}, false
	}
	e = fstabEntry{
		device:     unescapeOctal(fields[0]),
		mountPoint: unescapeOctal(fields[1]),
		options:    defaultMountOptions,
	}
	if len(fields) > 2 {
		e.fstype = unescapeOctal(fields[2])
	}
	if len(fields) > 3 {
		e.options = splitMountOptions(unescapeOctal(fields[3]))
	}
	parseNum := func(s string) int64 {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil || v < 0 {
			return -1
		}
		return v
	}
	if len(fields) > 4 {
		e.dump = parseNum(fields[4])
	}
	if len(fields) > 5 {
		e.pass = parseNum(fields[5])
	}
	return e, true
}

// splitMountOptions splits a comma-separated option string. Commas inside
// double quotes, as in context="system_u:object_r:tmp_t:s0:c127,c456", do
// not separate options.
func splitMountOptions(s string) []string {
	var out []string
	start, quoted := 0, false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			quoted = !quoted
		case ',':
			if !quoted {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// fstabMountPointMatches reports whether an fstab mount point field refers
// to mountPoint. A trailing slash in the file is tolerated.
func fstabMountPointMatches(field, mountPoint string) bool {
	if field == mountPoint {
		return true
	}
	return strings.HasPrefix(field, "/") && filepath.Clean(field) == mountPoint
}

// findFstabEntries returns the indexes of the entries for mountPoint.
func findFstabEntries(lines []string, mountPoint string) []int {
	var idx []int
	for i, l := range lines {
		if e, ok := parseFstabLine(l); ok && fstabMountPointMatches(e.mountPoint, mountPoint) {
			idx = append(idx, i)
		}
	}
	return idx
}

// lookupFstabEntry returns the first entry for mountPoint, which is the one
// mount(8) and systemd use, and how many entries there are for it.
func lookupFstabEntry(t *textFile, mountPoint string) (*fstabEntry, int) {
	idx := findFstabEntries(t.lines, mountPoint)
	if len(idx) == 0 {
		return nil, 0
	}
	e, _ := parseFstabLine(t.lines[idx[0]])
	return &e, len(idx)
}

// setFstabEntry makes e the only entry for its mount point. The first
// existing entry is replaced in place unless it already equals e, further
// entries for the same mount point are removed, and if there is none, e is
// appended. Comments and all other lines are kept. It reports whether t
// changed.
func setFstabEntry(t *textFile, e fstabEntry) bool {
	idx := findFstabEntries(t.lines, e.mountPoint)
	if len(idx) == 0 {
		t.insert(len(t.lines), []string{e.String()})
		return true
	}
	changed := false
	for i := len(idx) - 1; i > 0; i-- {
		t.replace(idx[i], idx[i]+1, nil)
		changed = true
	}
	cur, _ := parseFstabLine(t.lines[idx[0]])
	cur.mountPoint = e.mountPoint // Matched already; may differ by a trailing slash.
	if !cur.equal(e) {
		t.replace(idx[0], idx[0]+1, []string{e.String()})
		changed = true
	}
	return changed
}

// removeFstabEntries removes every entry for mountPoint and reports whether
// t changed.
func removeFstabEntries(t *textFile, mountPoint string) bool {
	idx := findFstabEntries(t.lines, mountPoint)
	for i := len(idx) - 1; i >= 0; i-- {
		t.replace(idx[i], idx[i]+1, nil)
	}
	return len(idx) > 0
}

// readFstab reads and parses the fstab. A missing file reads as empty with
// a nil snapshot.
func readFstab(p string) (*textFile, *fileSnapshot, error) {
	data, snap, err := readRegularFileNoFollow(p, maxFstabSize)
	if errors.Is(err, fs.ErrNotExist) {
		return &textFile{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return parseTextFile(data), snap, nil
}

// editFstab applies edit to the fstab at p and writes the result atomically
// if it changed, keeping the file's mode, ownership and extended attributes.
// Concurrent edits by other sysutils resources are serialized.
func editFstab(p string, edit func(t *textFile) bool) error {
	unlock, err := lockFileForEdit(p)
	if err != nil {
		return err
	}
	defer unlock()
	t, snap, err := readFstab(p)
	if err != nil {
		return err
	}
	if !edit(t) {
		return nil
	}
	return replaceFileAtomic(p, t.bytes(), snap, fstabCreateMode)
}

// isBindMount reports whether options make a bind mount, whose source is a
// directory or file rather than a device.
func isBindMount(options []string) bool {
	return slices.Contains(options, "bind") || slices.Contains(options, "rbind")
}

// fstypeMatches reports whether a mount the kernel reports as live could
// have been made with the configured type want. Kernel aliases (nfs mounted
// as nfs4, smb3 as cifs, FUSE subtypes) and types that mount(8) resolves
// itself ("auto", "none", lists, and negations) are accepted.
func fstypeMatches(want, live string) bool {
	for _, w := range strings.Split(want, ",") {
		switch {
		case w == live, w == "auto", w == "none", strings.HasPrefix(w, "no"):
			return true
		case w == "nfs" && live == "nfs4", w == "nfs4" && live == "nfs":
			return true
		case (w == "smb3" || w == "cifs") && (live == "smb3" || live == "cifs"):
			return true
		case strings.HasPrefix(w, "fuse") && strings.HasPrefix(live, "fuse"):
			return true
		}
	}
	return false
}

// deviceTagDirs maps the device tags accepted by mount(8) to the udev
// directories that resolve them.
var deviceTagDirs = map[string]string{
	"UUID":      "/dev/disk/by-uuid",
	"LABEL":     "/dev/disk/by-label",
	"PARTUUID":  "/dev/disk/by-partuuid",
	"PARTLABEL": "/dev/disk/by-partlabel",
}

// deviceDiffers reports whether the mount live was certainly not made from
// the configured device want. It errs on the side of "same": the kernel
// reports sources in forms that often differ from what was passed to mount,
// so only comparisons that are reliable count.
//
//   - Block devices, by path, symlink or tag such as UUID=, are compared by
//     device number, so /dev/disk/by-uuid/... and /dev/sda1 are the same.
//   - Paths that are not block devices (loop images, directories) and bind
//     mounts are never reported as different, and neither are FUSE mounts.
//   - Other sources, such as "tmpfs" or "server:/export", are compared as
//     text, ignoring a trailing slash.
func deviceDiffers(want, fstype string, options []string, live *mountEntry) bool {
	if isBindMount(options) || strings.HasPrefix(fstype, "fuse") || strings.HasPrefix(live.fstype, "fuse") {
		return false
	}
	p := want
	if tag, value, ok := strings.Cut(want, "="); ok {
		dir, isTag := deviceTagDirs[tag]
		if isTag {
			value = strings.Trim(value, `"`)
			if value == "" || strings.Contains(value, "/") {
				return false
			}
			p = filepath.Join(dir, value)
		}
	}
	if strings.HasPrefix(p, "/") {
		info, err := os.Stat(p)
		if err != nil || info.Mode()&fs.ModeDevice == 0 || info.Mode()&fs.ModeCharDevice != 0 {
			return false // Can't tell.
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		// File systems such as btrfs report an anonymous device number
		// (major 0) instead of the block device's.
		if !ok || live.major == 0 {
			return false
		}
		rdev := uint64(st.Rdev) //nolint:unconvert // Rdev is not uint64 on every platform.
		return unix.Major(rdev) != live.major || unix.Minor(rdev) != live.minor
	}
	return strings.TrimSuffix(want, "/") != strings.TrimSuffix(live.source, "/")
}

// checkMountPoint checks that p is a directory, or a file for bind mounts,
// and that no component of p is a symlink. The kernel reports mount points
// by their resolved path, so a mount through a symlink would never be found
// again.
func checkMountPoint(p string) error {
	info, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("mount point %q does not exist; create it first, for example with sysutils_directory", p)
	}
	if err != nil {
		return err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return symlinkRefusedError(p)
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return fmt.Errorf("mount point %q is neither a directory nor a regular file", p)
	}
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return err
	}
	if resolved != p {
		return fmt.Errorf("mount point %q contains a symlink and resolves to %q; use the resolved path", p, resolved)
	}
	return nil
}

// validateMountOption reports why s cannot be used as one element of the
// options list.
func validateMountOption(s string) error {
	if s == "" {
		return errors.New("mount options must not be empty strings")
	}
	for _, c := range s {
		if c <= ' ' || c == 0x7f {
			return fmt.Errorf("mount option %q contains white space or a control character", s)
		}
	}
	if strings.Count(s, `"`)%2 != 0 {
		// Joined with the other options, an unbalanced quote would make
		// mount(8) read the following options as part of this one.
		return fmt.Errorf("mount option %q contains an unbalanced double quote", s)
	}
	if parts := splitMountOptions(s); len(parts) != 1 {
		return fmt.Errorf("mount option %q contains a comma; use one list element per option", s)
	}
	if s == "remount" || s == "move" {
		return fmt.Errorf("mount option %q is not supported; the provider remounts on its own when options change", s)
	}
	return nil
}

// validateFSType reports why s is not a usable file system type.
func validateFSType(s string) error {
	if s == "" {
		return errors.New("file system type must not be empty")
	}
	for _, c := range s {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '.' || c == '_' || c == '-' || c == '+' || c == ','
		if !ok {
			return fmt.Errorf("file system type %q contains the invalid character %q", s, c)
		}
	}
	return nil
}

// validateMountDevice reports why s is not a usable mount source.
func validateMountDevice(s string) error {
	if s == "" {
		return errors.New("device must not be empty")
	}
	for _, c := range s {
		if c < ' ' || c == 0x7f {
			return fmt.Errorf("device %q contains a control character", s)
		}
	}
	return nil
}
