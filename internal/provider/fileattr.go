package provider

// Inode flags ("file attributes" in chattr(1) and lsattr(1)), read and
// written with the FS_IOC_GETFLAGS and FS_IOC_SETFLAGS ioctls, as chattr
// does, through a descriptor opened without following symlinks.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Inode flags from linux/fs.h; golang.org/x/sys/unix defines only a few of
// them.
const (
	fsSecrmFL        uint32 = 0x00000001 // s: secure deletion
	fsUnrmFL         uint32 = 0x00000002 // u: undeletable
	fsComprFL        uint32 = 0x00000004 // c: compressed
	fsSyncFL         uint32 = 0x00000008 // S: synchronous updates
	fsImmutableFL    uint32 = 0x00000010 // i: immutable
	fsAppendFL       uint32 = 0x00000020 // a: append only
	fsNodumpFL       uint32 = 0x00000040 // d: no dump
	fsNoatimeFL      uint32 = 0x00000080 // A: no atime updates
	fsNocompFL       uint32 = 0x00000400 // m: don't compress
	fsJournalDataFL  uint32 = 0x00004000 // j: data journalling
	fsNotailFL       uint32 = 0x00008000 // t: no tail merging
	fsDirsyncFL      uint32 = 0x00010000 // D: synchronous directory updates
	fsTopdirFL       uint32 = 0x00020000 // T: top of directory hierarchy
	fsNocowFL        uint32 = 0x00800000 // C: no copy on write
	fsDaxFL          uint32 = 0x02000000 // x: direct access
	fsProjinheritFL  uint32 = 0x20000000 // P: project hierarchy
	fileAttrAllFlags        = fsSecrmFL | fsUnrmFL | fsComprFL | fsSyncFL | fsImmutableFL | fsAppendFL |
		fsNodumpFL | fsNoatimeFL | fsNocompFL | fsJournalDataFL | fsNotailFL | fsDirsyncFL | fsTopdirFL |
		fsNocowFL | fsDaxFL | fsProjinheritFL
)

// fileAttrFlag is an inode flag that sysutils_file_attributes manages.
type fileAttrFlag struct {
	letter string
	flag   uint32
	name   string
}

// fileAttrFlags are the flags that chattr can set and clear, in the order
// lsattr prints them. Flags that the kernel sets itself or that can't be
// cleared again, such as e (extents), E (encrypted), I (indexed directory),
// N (inline data), V (verity) and F (casefold, only on empty directories),
// are not managed and never changed.
var fileAttrFlags = []fileAttrFlag{
	{"s", fsSecrmFL, "secure deletion"},
	{"u", fsUnrmFL, "undeletable"},
	{"S", fsSyncFL, "synchronous updates"},
	{"D", fsDirsyncFL, "synchronous directory updates"},
	{"i", fsImmutableFL, "immutable"},
	{"a", fsAppendFL, "append only"},
	{"d", fsNodumpFL, "no dump"},
	{"A", fsNoatimeFL, "no atime updates"},
	{"c", fsComprFL, "compressed"},
	{"j", fsJournalDataFL, "data journalling"},
	{"t", fsNotailFL, "no tail merging"},
	{"T", fsTopdirFL, "top of directory hierarchy"},
	{"C", fsNocowFL, "no copy on write"},
	{"x", fsDaxFL, "direct access"},
	{"P", fsProjinheritFL, "project hierarchy"},
	{"m", fsNocompFL, "don't compress"},
}

// fileAttrLetters lists the letters of fileAttrFlags, for messages.
func fileAttrLetters() string {
	var b strings.Builder
	for _, f := range fileAttrFlags {
		b.WriteString(f.letter)
	}
	return b.String()
}

// parseFileAttr returns the flag for the chattr letter s.
func parseFileAttr(s string) (uint32, error) {
	for _, f := range fileAttrFlags {
		if f.letter == s {
			return f.flag, nil
		}
	}
	if len(s) != 1 {
		return 0, fmt.Errorf("%q is not a single flag letter; use one of %s", s, fileAttrLetters())
	}
	return 0, fmt.Errorf("%q is not a supported flag; use one of %s", s, fileAttrLetters())
}

// parseFileAttrs returns the flags for the chattr letters in list.
func parseFileAttrs(list []string) (uint32, error) {
	var flags uint32
	for _, s := range list {
		f, err := parseFileAttr(s)
		if err != nil {
			return 0, err
		}
		flags |= f
	}
	return flags, nil
}

// formatFileAttrs returns the letters of the managed flags in flags, in
// lsattr's order.
func formatFileAttrs(flags uint32) []string {
	out := []string{}
	for _, f := range fileAttrFlags {
		if flags&f.flag != 0 {
			out = append(out, f.letter)
		}
	}
	return out
}

// describeFileAttrs returns the letters of flags for messages, such as
// "i" or "a, d".
func describeFileAttrs(flags uint32) string {
	letters := formatFileAttrs(flags)
	sort.Strings(letters)
	return strings.Join(letters, ", ")
}

// errFileAttrsNotSupported matches errors of file systems (or kernels) that
// don't implement the flag ioctls at all, or not the flags asked for.
var errFileAttrsNotSupported = errors.New("inode flags are not supported")

// fileAttrIoctlError describes a failed FS_IOC_GETFLAGS or FS_IOC_SETFLAGS.
type fileAttrIoctlError struct {
	op   string
	path string
	err  error
}

func (e *fileAttrIoctlError) Error() string {
	msg := fmt.Sprintf("%s %s: %v", e.op, e.path, e.err)
	if e.Is(errFileAttrsNotSupported) {
		msg += fmt.Sprintf(" (the file system of %q does not support these inode flags; "+
			"tmpfs supports only a, A, d and i, and overlayfs, as in Docker containers, only what its upper file system supports)", e.path)
	}
	return msg
}

func (e *fileAttrIoctlError) Unwrap() error { return e.err }

func (e *fileAttrIoctlError) Is(target error) bool {
	return target == errFileAttrsNotSupported && isFileAttrsNotSupported(e.err)
}

// isFileAttrsNotSupported reports whether err is how a file system says it
// has no inode flags (ENOTTY, or ENOSYS from some FUSE file systems) or
// doesn't support a particular one (EOPNOTSUPP).
func isFileAttrsNotSupported(err error) bool {
	return errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOSYS)
}

// getFileAttrs returns the inode flags of the open file f.
func getFileAttrs(f *os.File) (uint32, error) {
	var flags uint32
	err := controlFile(f, func(fd int) error {
		var err error
		flags, err = unix.IoctlGetUint32(fd, unix.FS_IOC_GETFLAGS)
		return err
	})
	if err != nil {
		return 0, &fileAttrIoctlError{op: "reading inode flags of", path: f.Name(), err: err}
	}
	return flags, nil
}

// setFileAttrs sets the inode flags of the open file f to flags.
func setFileAttrs(f *os.File, flags uint32) error {
	err := controlFile(f, func(fd int) error {
		// The kernel reads an int, whatever the size in the request
		// number says.
		return unix.IoctlSetPointerInt(fd, unix.FS_IOC_SETFLAGS, int(int32(flags))) //nolint:gosec // Bit pattern, not a number.
	})
	if err != nil {
		return &fileAttrIoctlError{op: "setting inode flags of", path: f.Name(), err: err}
	}
	return nil
}

// controlFile calls fn with the descriptor of f.
func controlFile(f *os.File, fn func(fd int) error) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var opErr error
	if err := rc.Control(func(fd uintptr) { opErr = fn(int(fd)) }); err != nil { //nolint:gosec // Descriptors fit in an int.
		return err
	}
	return opErr
}

// fileAttrsOf returns the inode flags of the regular file or directory at
// p. It never follows a symlink at p and never opens anything else, since
// opening a device can have side effects.
func fileAttrsOf(p string) (uint32, error) {
	info, err := os.Lstat(p)
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() && !info.IsDir() {
		return 0, fmt.Errorf("path %q is neither a regular file nor a directory", p)
	}
	f, err := openNoFollow(p, os.O_RDONLY, 0)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	if info, err := f.Stat(); err != nil {
		return 0, err
	} else if !info.Mode().IsRegular() && !info.IsDir() {
		return 0, fmt.Errorf("path %q is neither a regular file nor a directory", p)
	}
	return getFileAttrs(f)
}

// immutableFileError is an EPERM error explained by the immutable or
// append-only flag of path.
type immutableFileError struct {
	err   error
	path  string
	isDir bool
	flag  uint32
}

func (e *immutableFileError) Error() string {
	what := "file"
	if e.isDir {
		what = "directory"
	}
	if e.flag == fsImmutableFL {
		consequence := "nobody, not even root, can modify, replace or remove it"
		if e.isDir {
			consequence = "nobody, not even root, can create, rename or remove entries in it"
		}
		return fmt.Sprintf("%v: the %s %q has the immutable attribute (i, see lsattr) set, so %s; "+
			"clear it first, with a sysutils_file_attributes resource or chattr -i", e.err, what, e.path, consequence)
	}
	consequence := "it can only be appended to, not rewritten, replaced or removed"
	if e.isDir {
		consequence = "entries can only be added to it, not renamed or removed"
	}
	return fmt.Sprintf("%v: the %s %q has the append-only attribute (a, see lsattr) set, so %s; "+
		"clear it first, with a sysutils_file_attributes resource or chattr -a", e.err, what, e.path, consequence)
}

func (e *immutableFileError) Unwrap() error { return e.err }

// explainImmutable returns err with an explanation added if it is an EPERM
// caused by the immutable or append-only flag of one of paths, or of the
// file named in err or its directory, and err unchanged otherwise. The
// flags are only looked at after an EPERM, so this costs nothing on
// success.
func explainImmutable(err error, paths ...string) error {
	if err == nil || !errors.Is(err, syscall.EPERM) {
		return err
	}
	var already *immutableFileError
	if errors.As(err, &already) {
		return err
	}
	// The paths in the error come first: they name the file that failed.
	var candidates []string
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		candidates = append(candidates, pathErr.Path, filepath.Dir(pathErr.Path))
	}
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		candidates = append(candidates, linkErr.New, filepath.Dir(linkErr.New), filepath.Dir(linkErr.Old))
	}
	candidates = append(candidates, paths...)
	seen := map[string]bool{}
	for _, p := range candidates {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		flags, ferr := fileAttrsOf(p)
		if ferr != nil {
			continue
		}
		for _, flag := range []uint32{fsImmutableFL, fsAppendFL} {
			if flags&flag != 0 {
				info, _ := os.Lstat(p)
				return &immutableFileError{err: err, path: p, isDir: info != nil && info.IsDir(), flag: flag}
			}
		}
	}
	return err
}
