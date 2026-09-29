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
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

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
