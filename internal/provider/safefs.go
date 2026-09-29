package provider

// Symlink-safe filesystem primitives.
//
// The provider usually runs as root and acts on paths that other local users
// may be able to influence (for example by pre-creating entries in a shared
// directory such as /tmp). The helpers in this file implement the following
// security decisions:
//
//   - The final component of a managed file or directory path is never
//     followed if it is a symlink. Mode and ownership are changed through a
//     file descriptor obtained with O_NOFOLLOW (fchmod/fchown), so there is
//     no window between checking what a path is and modifying it.
//   - Intermediate path components are resolved normally for ordinary
//     create/update operations. The trust assumption is that every ancestor
//     of a managed path is writable only by trusted users.
//   - Recursive deletion (force_destroy) is irreversible and unbounded, so it
//     is held to a stricter standard: no component of the path may be a
//     symlink, every step is performed relative to an already opened
//     directory descriptor, filesystem boundaries are never crossed, and a
//     fixed set of critical system directories is refused outright.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// protectedPaths are directories that force_destroy refuses to delete
// recursively, even though they pass path validation. Removing any of them
// would render a typical Linux host unusable or destroy unrelated data.
var protectedPaths = map[string]struct{}{
	"/bin": {}, "/boot": {}, "/dev": {}, "/etc": {}, "/home": {}, "/lib": {},
	"/lib32": {}, "/lib64": {}, "/libx32": {}, "/media": {}, "/mnt": {},
	"/opt": {}, "/proc": {}, "/root": {}, "/run": {}, "/sbin": {}, "/srv": {},
	"/sys": {}, "/tmp": {}, "/usr": {}, "/var": {},
	"/usr/bin": {}, "/usr/include": {}, "/usr/lib": {}, "/usr/lib32": {},
	"/usr/lib64": {}, "/usr/libexec": {}, "/usr/local": {}, "/usr/sbin": {},
	"/usr/share": {}, "/usr/src": {},
	"/var/cache": {}, "/var/lib": {}, "/var/log": {}, "/var/mail": {},
	"/var/spool": {}, "/var/tmp": {}, "/var/www": {},
}

// errNotRecursivelyRemovable is wrapped by removeAllNoFollow errors that are
// caused by a safety guard rather than an I/O failure.
var errNotRecursivelyRemovable = errors.New("refusing to delete recursively")

// validateRecursivelyRemovable reports why p must never be deleted
// recursively, based on the path string alone. It does not touch the
// filesystem, so it can be used at plan time.
func validateRecursivelyRemovable(p string) error {
	if err := validateAbsolutePath(p); err != nil {
		return err
	}
	if _, ok := protectedPaths[p]; ok {
		return fmt.Errorf("path %q is a protected system directory and cannot be deleted recursively", p)
	}
	return nil
}

// openNoFollow opens p with the given flags, refusing to follow p itself if
// it is a symlink. O_NOFOLLOW, O_NONBLOCK and O_CLOEXEC are always added;
// O_NONBLOCK keeps a FIFO planted at p from blocking the provider forever and
// has no effect on regular files or directories.
func openNoFollow(p string, flag int, perm fs.FileMode) (*os.File, error) {
	f, err := os.OpenFile(p, flag|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, perm)
	if err == nil {
		return f, nil
	}
	// Depending on the other flags, the kernel reports a trailing symlink as
	// ELOOP or ENOTDIR; distinguish it from a genuine loop or non-directory.
	if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) {
		if info, lerr := os.Lstat(p); lerr == nil && info.Mode()&fs.ModeSymlink != 0 {
			return nil, symlinkRefusedError(p)
		}
	}
	return nil, err
}

// symlinkRefusedError is the error returned when p is a symlink but the
// operation requires a real file or directory.
func symlinkRefusedError(p string) error {
	return fmt.Errorf("path %q is a symbolic link; refusing to follow it", p)
}

// setOwnershipAndMode applies owner, group and mode to the open file f.
// Ownership is changed first, because chown may clear the setuid and setgid
// bits. IDs that already match are skipped so that no privileges are needed
// when nothing changes. Empty owner or group values leave the corresponding
// attribute unchanged.
func setOwnershipAndMode(f *os.File, owner, group string, mode fs.FileMode) error {
	if owner != "" || group != "" {
		uid, gid, err := resolveOwnership(owner, group)
		if err != nil {
			return err
		}
		info, err := f.Stat()
		if err != nil {
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("unable to determine ownership of %q on this platform", f.Name())
		}
		if uid >= 0 && uint64(uid) == uint64(st.Uid) {
			uid = -1
		}
		if gid >= 0 && uint64(gid) == uint64(st.Gid) {
			gid = -1
		}
		if uid != -1 || gid != -1 {
			if err := f.Chown(uid, gid); err != nil {
				return fmt.Errorf("setting ownership: %w", err)
			}
		}
	}
	if err := f.Chmod(mode); err != nil {
		return fmt.Errorf("setting mode: %w", err)
	}
	return nil
}

// removeAllNoFollow recursively deletes the directory p. Unlike os.RemoveAll
// it refuses to delete anything if p or any of its ancestors is a symlink, if
// p is a protected system directory, or if p is not a directory. It never
// descends into another filesystem mounted below p. Every step is performed
// relative to an open directory descriptor, so swapping a path component for
// a symlink mid-way cannot redirect the deletion. A missing p is not an error.
func removeAllNoFollow(p string) error {
	if err := validateRecursivelyRemovable(p); err != nil {
		return fmt.Errorf("%w: %w", errNotRecursivelyRemovable, err)
	}

	parent, err := openDirPathNoSymlinks(filepath.Dir(p))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer func() { _ = unix.Close(parent) }()

	base := filepath.Base(p)
	var st unix.Stat_t
	if err := unix.Fstatat(parent, base, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return &fs.PathError{Op: "lstat", Path: p, Err: err}
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
	case unix.S_IFLNK:
		return fmt.Errorf("%w: %w", errNotRecursivelyRemovable, symlinkRefusedError(p))
	default:
		return fmt.Errorf("%w: path %q is not a directory", errNotRecursivelyRemovable, p)
	}

	if err := removeTreeAt(parent, base, p, st.Dev); err != nil {
		return err
	}
	if err := unix.Unlinkat(parent, base, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
		return &fs.PathError{Op: "rmdir", Path: p, Err: err}
	}
	return nil
}

// openDirPathNoSymlinks opens the absolute directory path dir component by
// component with O_NOFOLLOW, failing if any component is a symlink. The
// returned O_PATH descriptor is only suitable as the dirfd of *at calls.
func openDirPathNoSymlinks(dir string) (int, error) {
	const flags = unix.O_PATH | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fd, err := unix.Open("/", flags, 0)
	if err != nil {
		return -1, &fs.PathError{Op: "open", Path: "/", Err: err}
	}
	cur := "/"
	for _, name := range strings.Split(strings.TrimPrefix(dir, "/"), "/") {
		if name == "" {
			continue
		}
		cur = filepath.Join(cur, name)
		next, err := unix.Openat(fd, name, flags, 0)
		if err != nil {
			if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
				var st unix.Stat_t
				if unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Mode&unix.S_IFMT == unix.S_IFLNK {
					err = fmt.Errorf("%w: path component %q is a symbolic link; use the real path instead", errNotRecursivelyRemovable, cur)
				}
			}
			_ = unix.Close(fd)
			if errors.Is(err, errNotRecursivelyRemovable) {
				return -1, err
			}
			return -1, &fs.PathError{Op: "open", Path: cur, Err: err}
		}
		_ = unix.Close(fd)
		fd = next
	}
	return fd, nil
}

// removeTreeAt deletes everything inside the directory name, which is
// resolved relative to the directory descriptor parent, without removing the
// directory itself. display is the full path, used in error messages only.
// Directories on a device other than dev are refused rather than entered.
func removeTreeAt(parent int, name, display string, dev uint64) error {
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return &fs.PathError{Op: "open", Path: display, Err: err}
	}
	dir := os.NewFile(uintptr(fd), display)
	defer func() { _ = dir.Close() }()

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return &fs.PathError{Op: "stat", Path: display, Err: err}
	}
	if st.Dev != dev {
		return fmt.Errorf("%w: %q is on a different filesystem (mount point)", errNotRecursivelyRemovable, display)
	}

	names, err := dir.Readdirnames(-1)
	if err != nil {
		return &fs.PathError{Op: "readdir", Path: display, Err: err}
	}
	for _, entry := range names {
		child := filepath.Join(display, entry)
		err := unix.Unlinkat(fd, entry, 0)
		switch {
		case err == nil, errors.Is(err, unix.ENOENT):
			continue
		case errors.Is(err, unix.EISDIR):
			if err := removeTreeAt(fd, entry, child, dev); err != nil {
				return err
			}
			if err := unix.Unlinkat(fd, entry, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
				return &fs.PathError{Op: "rmdir", Path: child, Err: err}
			}
		default:
			return &fs.PathError{Op: "unlink", Path: child, Err: err}
		}
	}
	return nil
}

// fileSnapshot records the identity and attributes of a regular file at the
// time it was read, so that a later in-place edit can detect whether the file
// was replaced or modified in between and can preserve its mode and owner.
type fileSnapshot struct {
	dev, ino uint64
	size     int64
	mtime    int64 // Nanoseconds since the epoch.
	// ctime changes with chmod, chown and extended attribute (ACL) changes,
	// so comparing it detects a concurrent change of permissions that the
	// replacement would otherwise silently revert.
	ctime    int64
	uid, gid uint32
	mode     fs.FileMode // Permission and special bits only.
	// xattrs are the extended attributes to carry over to a replacement
	// file; see readXattrs. Only set by readRegularFileNoFollow.
	xattrs map[string][]byte
}

func snapshotOf(p string, info fs.FileInfo) (*fileSnapshot, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("unable to determine ownership of %q on this platform", p)
	}
	return &fileSnapshot{
		dev:   uint64(st.Dev), //nolint:unconvert // Dev is not uint64 on every platform.
		ino:   st.Ino,
		size:  info.Size(),
		mtime: info.ModTime().UnixNano(),
		ctime: time.Unix(int64(st.Ctim.Sec), int64(st.Ctim.Nsec)).UnixNano(), //nolint:unconvert // Not int64 on every platform.
		uid:   st.Uid,
		gid:   st.Gid,
		mode:  info.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky),
	}, nil
}

// unchangedSince reports whether s and o describe the same, unmodified file.
func (s *fileSnapshot) unchangedSince(o *fileSnapshot) bool {
	// Mode and ownership are compared directly as well, because timestamps
	// may be too coarse to tell apart changes made in quick succession.
	return s.dev == o.dev && s.ino == o.ino && s.size == o.size && s.mtime == o.mtime && s.ctime == o.ctime &&
		s.mode == o.mode && s.uid == o.uid && s.gid == o.gid
}

// errFileChangedConcurrently is returned by replaceFileAtomic when the target
// was modified, replaced or created by someone else after it was read.
var errFileChangedConcurrently = errors.New("file was modified by another process while it was being edited; try again")

// readRegularFileNoFollow reads the regular file p without following a
// symlink at p. Files larger than limit bytes are refused. Errors for a
// missing file wrap fs.ErrNotExist.
func readRegularFileNoFollow(p string, limit int64) ([]byte, *fileSnapshot, error) {
	f, err := openNoFollow(p, os.O_RDONLY, 0)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if err := checkRegularFile(p, info); err != nil {
		return nil, nil, err
	}
	if info.Size() > limit {
		return nil, nil, fmt.Errorf("file %q is larger than %d bytes", p, limit)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(data)) > limit {
		return nil, nil, fmt.Errorf("file %q is larger than %d bytes", p, limit)
	}
	snap, err := snapshotOf(p, info)
	if err != nil {
		return nil, nil, err
	}
	if snap.xattrs, err = readXattrs(f); err != nil {
		return nil, nil, fmt.Errorf("reading extended attributes of %q: %w", p, err)
	}
	return data, snap, nil
}

// readXattrs returns the extended attributes of f that a replacement file
// must carry over: POSIX ACLs (system.posix_acl_access), the SELinux label
// (security.selinux) and any other attribute the caller can read.
//
// Dropping them is not merely lossy. With an ACL, the group permission bits
// of the mode hold the ACL mask rather than the owning group's permissions,
// so a replacement with the same mode but without the ACL would grant the
// owning group whatever the mask allows (typically more than before), and
// a lost SELinux label can move a file out of its confinement domain.
//
// security.capability is left out on purpose: the kernel drops file
// capabilities whenever file content is modified, and a text edit must not
// be able to keep them.
func readXattrs(f *os.File) (map[string][]byte, error) {
	rc, err := f.SyscallConn()
	if err != nil {
		return nil, err
	}
	var attrs map[string][]byte
	var opErr error
	err = rc.Control(func(fd uintptr) {
		names, err := xattrCall(func(buf []byte) (int, error) { return unix.Flistxattr(int(fd), buf) })
		if err != nil {
			if errors.Is(err, unix.ENOTSUP) {
				return // The filesystem has no extended attributes.
			}
			opErr = err
			return
		}
		for _, name := range strings.Split(strings.TrimRight(string(names), "\x00"), "\x00") {
			if name == "" || name == "security.capability" {
				continue
			}
			val, err := xattrCall(func(buf []byte) (int, error) { return unix.Fgetxattr(int(fd), name, buf) })
			switch {
			case errors.Is(err, unix.ENODATA):
				continue // Removed since it was listed.
			case err != nil:
				opErr = fmt.Errorf("%s: %w", name, err)
				return
			}
			if attrs == nil {
				attrs = map[string][]byte{}
			}
			attrs[name] = val
		}
	})
	if err != nil {
		return nil, err
	}
	return attrs, opErr
}

// xattrCall calls fn, which follows the listxattr/getxattr size protocol,
// with a buffer large enough for the result.
func xattrCall(fn func(buf []byte) (int, error)) ([]byte, error) {
	for {
		n, err := fn(nil)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, nil
		}
		buf := make([]byte, n)
		n, err = fn(buf)
		if errors.Is(err, unix.ERANGE) {
			continue // Grew between the two calls.
		}
		if err != nil {
			return nil, err
		}
		return buf[:n], nil
	}
}

// aclAccessXattr holds a file's POSIX access ACL.
const aclAccessXattr = "system.posix_acl_access"

// removeXattr removes the extended attribute name from f. It is not an error
// if f does not have it, or the filesystem has no extended attributes.
func removeXattr(f *os.File, name string) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var opErr error
	err = rc.Control(func(fd uintptr) {
		opErr = unix.Fremovexattr(int(fd), name)
	})
	if err != nil {
		return err
	}
	if errors.Is(opErr, unix.ENODATA) || errors.Is(opErr, unix.ENOTSUP) {
		return nil
	}
	return opErr
}

// writeXattrs sets attrs on f. Failing to set any of them is an error, since
// silently dropping an ACL can widen access to the file.
func writeXattrs(f *os.File, attrs map[string][]byte) error {
	if len(attrs) == 0 {
		return nil
	}
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	// Sorted, so that errors are deterministic.
	names := make([]string, 0, len(attrs))
	for name := range attrs {
		names = append(names, name)
	}
	sort.Strings(names)
	var opErr error
	err = rc.Control(func(fd uintptr) {
		for _, name := range names {
			if err := unix.Fsetxattr(int(fd), name, attrs[name], 0); err != nil {
				opErr = fmt.Errorf("%s: %w", name, err)
				return
			}
		}
	})
	if err != nil {
		return err
	}
	return opErr
}

// replaceFileAtomic replaces the contents of target with data. orig is the
// snapshot taken when target was read, or nil if target did not exist and
// should be created with mode newMode (owned by the current user).
//
// The data is written to a temporary file in the same directory, which gets
// orig's owner, group, mode and extended attributes (including ACLs and the
// SELinux label, see readXattrs) before any data is written to it, and is then
// renamed over target. Readers therefore see either the old or the new
// contents, never a partial file. rename(2) replaces the directory entry
// without following it, so a symlink swapped in at target is replaced rather
// than written through. Immediately before the rename, target is checked
// against orig and the edit is abandoned with errFileChangedConcurrently if
// it was modified in the meantime.
//
// If target is a mount point (for example a bind-mounted /etc/hosts in a
// container) rename fails with EBUSY. In that case, and only then, the file
// is rewritten in place through an O_NOFOLLOW descriptor after re-checking
// its identity; this is not atomic.
func replaceFileAtomic(target string, data []byte, orig *fileSnapshot, newMode fs.FileMode) error {
	attrs := replaceAttrs{mode: newMode}
	if orig != nil {
		attrs = replaceAttrs{
			mode:    orig.mode,
			chown:   true,
			uid:     orig.uid,
			gid:     orig.gid,
			xattrs:  orig.xattrs,
			dropACL: orig.xattrs[aclAccessXattr] == nil,
		}
	}
	return replaceFileAtomicWith(target, data, orig, attrs)
}

// replaceAttrs are the attributes replaceFileAtomicWith gives the
// replacement file before any data is written to it.
type replaceAttrs struct {
	mode fs.FileMode
	// chown selects whether the file is given uid and gid. Otherwise it is
	// owned by the current user, like any newly created file.
	chown    bool
	uid, gid uint32
	// xattrs are set on the file after its mode; see readXattrs.
	xattrs map[string][]byte
	// dropACL removes an access ACL that the file inherited from a default
	// ACL on the directory and that xattrs does not set.
	dropACL bool
}

// replaceFileAtomicWith is replaceFileAtomic with explicitly given
// attributes for the replacement file, whatever those of orig were. orig is
// used only to detect concurrent modification.
//
// The attributes are applied before the rename, so the new contents never
// appear with a mode or owner other than attrs, except on the non-atomic
// in-place fallback for mount points, where the existing file is given the
// attributes before its contents are replaced.
func replaceFileAtomicWith(target string, data []byte, orig *fileSnapshot, attrs replaceAttrs) (err error) {
	dir, base := filepath.Dir(target), filepath.Base(target)
	tmp := filepath.Join(dir, "."+base+".sysutils-tmp-"+randomID())
	f, err := openNoFollow(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("creating temporary file: %w", err)
	}
	renamed := false
	defer func() {
		_ = f.Close()
		if !renamed {
			_ = os.Remove(tmp)
		}
	}()

	if err := applyReplaceAttrs(f, attrs); err != nil {
		return fmt.Errorf("%q: %w", target, err)
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	if err := checkUnchanged(target, orig); err != nil {
		return err
	}
	if orig == nil {
		err = renameNoReplace(tmp, target)
	} else {
		err = os.Rename(tmp, target)
		if errors.Is(err, syscall.EBUSY) {
			if err := rewriteInPlace(target, data, orig, attrs); err != nil {
				return err
			}
			return nil // The deferred function removes tmp.
		}
	}
	if err != nil {
		return err
	}
	renamed = true
	syncDir(dir)
	return nil
}

// applyReplaceAttrs gives f the ownership, mode and extended attributes in
// attrs.
func applyReplaceAttrs(f *os.File, attrs replaceAttrs) error {
	if attrs.chown {
		if err := chownToMatch(f, attrs.uid, attrs.gid); err != nil {
			return fmt.Errorf("setting ownership: %w", err)
		}
	}
	// A default ACL on the directory gives a new file an access ACL.
	if attrs.dropACL && attrs.xattrs[aclAccessXattr] == nil {
		if err := removeXattr(f, aclAccessXattr); err != nil {
			return fmt.Errorf("removing inherited ACL: %w", err)
		}
	}
	// After chown, which may clear the setuid and setgid bits.
	if err := f.Chmod(attrs.mode); err != nil {
		return fmt.Errorf("setting mode: %w", err)
	}
	// After chmod: setting an ACL also sets the group bits to its mask.
	if err := writeXattrs(f, attrs.xattrs); err != nil {
		return fmt.Errorf("setting extended attributes: %w", err)
	}
	return nil
}

// chownToMatch changes the ownership of f to uid:gid, skipping the call when
// it already matches so no privileges are needed in the common case.
func chownToMatch(f *os.File, uid, gid uint32) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("unable to determine ownership of %q on this platform", f.Name())
	}
	if st.Uid == uid && st.Gid == gid {
		return nil
	}
	return f.Chown(int(uid), int(gid))
}

// checkUnchanged verifies that target still matches orig, or still does not
// exist if orig is nil.
func checkUnchanged(target string, orig *fileSnapshot) error {
	info, err := os.Lstat(target)
	if orig == nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("%q: %w", target, errFileChangedConcurrently)
	}
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%q: %w", target, errFileChangedConcurrently)
		}
		return err
	}
	if err := checkRegularFile(target, info); err != nil {
		return err
	}
	cur, err := snapshotOf(target, info)
	if err != nil {
		return err
	}
	if !cur.unchangedSince(orig) {
		return fmt.Errorf("%q: %w", target, errFileChangedConcurrently)
	}
	return nil
}

// renameNoReplace renames oldpath to newpath, failing if newpath exists.
// Filesystems without RENAME_NOREPLACE support fall back to a plain rename;
// checkUnchanged has verified immediately before that newpath is absent.
func renameNoReplace(oldpath, newpath string) error {
	err := unix.Renameat2(unix.AT_FDCWD, oldpath, unix.AT_FDCWD, newpath, unix.RENAME_NOREPLACE)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EEXIST):
		return fmt.Errorf("%q: %w", newpath, errFileChangedConcurrently)
	case errors.Is(err, unix.ENOSYS), errors.Is(err, unix.EINVAL):
		return os.Rename(oldpath, newpath)
	default:
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: err}
	}
}

// rewriteInPlace overwrites target with data through an O_NOFOLLOW
// descriptor, provided it is still the file described by orig.
func rewriteInPlace(target string, data []byte, orig *fileSnapshot, attrs replaceAttrs) error {
	f, err := openNoFollow(target, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if err := checkRegularFile(target, info); err != nil {
		return err
	}
	cur, err := snapshotOf(target, info)
	if err != nil {
		return err
	}
	if !cur.unchangedSince(orig) {
		return fmt.Errorf("%q: %w", target, errFileChangedConcurrently)
	}
	// attrs are orig's own unless the caller forces other ones. Only what
	// differs is changed; extended attributes other than the ACL stay.
	if attrs.chown {
		if err := chownToMatch(f, attrs.uid, attrs.gid); err != nil {
			return fmt.Errorf("%q: setting ownership: %w", target, err)
		}
	}
	if attrs.dropACL && attrs.xattrs[aclAccessXattr] == nil && orig.xattrs[aclAccessXattr] != nil {
		if err := removeXattr(f, aclAccessXattr); err != nil {
			return fmt.Errorf("%q: removing ACL: %w", target, err)
		}
	}
	if cur.mode != attrs.mode || attrs.chown && (cur.uid != attrs.uid || cur.gid != attrs.gid) {
		if err := f.Chmod(attrs.mode); err != nil {
			return fmt.Errorf("%q: setting mode: %w", target, err)
		}
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return f.Close()
}

// syncDir flushes the directory entry changes in dir to disk. Failures are
// ignored: the rename has already taken effect and some filesystems do not
// support fsync on directories.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
