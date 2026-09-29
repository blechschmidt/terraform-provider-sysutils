package provider

// Recursive ownership and mode enforcement for sysutils_directory
// (recursive_owner / recursive_mode), the equivalent of chown -R / chmod -R.
//
// Like removeAllNoFollow, the walk never follows a symlink and never leaves
// the tree:
//
//   - Every entry is opened relative to its parent's directory descriptor
//     with O_PATH|O_NOFOLLOW. Its type and attributes are read from that
//     descriptor, and it is modified through that descriptor, so replacing
//     an entry between the check and the change cannot redirect the change.
//     O_PATH also means that FIFOs and device nodes are never actually
//     opened.
//   - Symlinks are never followed. Their own ownership is changed (like
//     chown -R -P); their targets, inside or outside the tree, are untouched.
//   - Directories on another filesystem (mount points) are skipped entirely,
//     like find -xdev.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// treeReadBatch is the number of directory entries read at a time, so that
// huge directories do not have to be listed into memory at once.
const treeReadBatch = 256

// treeSpec describes the attributes every entry below a managed directory must
// have when recursion is enabled.
type treeSpec struct {
	// ownership requires every entry to have the UID and GID of the managed
	// directory itself.
	ownership bool
	// modes requires subdirectories to have dirMode and regular files to have
	// fileMode (both chmod(2) bits). Other entry types are left alone.
	modes    bool
	dirMode  uint32
	fileMode uint32
}

func (s treeSpec) enabled() bool { return s.ownership || s.modes }

// newTreeSpec builds a treeSpec from the resource settings. dirMode is the
// directory's mode; fileMode is the file_mode attribute, which defaults to
// dirMode when empty (matching chmod -R).
func newTreeSpec(recursiveOwner, recursiveMode bool, dirMode, fileMode string) (treeSpec, error) {
	spec := treeSpec{ownership: recursiveOwner, modes: recursiveMode}
	if !recursiveMode {
		return spec, nil
	}
	dm, err := parseMode(dirMode)
	if err != nil {
		return spec, err
	}
	spec.dirMode = unixModeBits(dm)
	spec.fileMode = spec.dirMode
	if fileMode != "" {
		fm, err := parseMode(fileMode)
		if err != nil {
			return spec, err
		}
		spec.fileMode = unixModeBits(fm)
	}
	return spec, nil
}

// conformTree walks everything below the directory root (but not root
// itself) and returns the number of entries that do not match spec. If fix is
// true, each such entry is corrected as it is found. root itself is refused
// if it is a symlink.
func conformTree(ctx context.Context, root string, spec treeSpec, fix bool) (int64, error) {
	if !spec.enabled() {
		return 0, nil
	}
	dir, err := openNoFollow(root, os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return 0, err
	}
	defer func() { _ = dir.Close() }()

	var st unix.Stat_t
	if err := unix.Fstat(int(dir.Fd()), &st); err != nil {
		return 0, &fs.PathError{Op: "stat", Path: root, Err: err}
	}
	w := treeWalker{ctx: ctx, spec: spec, fix: fix, uid: st.Uid, gid: st.Gid, dev: st.Dev}
	if err := w.walkDir(dir, root); err != nil {
		return w.count, err
	}
	return w.count, nil
}

type treeWalker struct {
	ctx  context.Context
	spec treeSpec
	fix  bool
	// uid, gid and dev are those of the managed (root) directory.
	uid, gid uint32
	dev      uint64
	count    int64
}

// walkDir visits every entry of the open directory dir, whose full path is
// display.
func (w *treeWalker) walkDir(dir *os.File, display string) error {
	for {
		if err := w.ctx.Err(); err != nil {
			return err
		}
		names, err := dir.Readdirnames(treeReadBatch)
		for _, name := range names {
			if verr := w.visit(int(dir.Fd()), name, filepath.Join(display, name)); verr != nil {
				return verr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return &fs.PathError{Op: "readdir", Path: display, Err: err}
		}
	}
}

// visit checks (and, when fixing, corrects) the entry name in the directory
// dirfd, and descends into it if it is a directory on the same filesystem.
func (w *treeWalker) visit(dirfd int, name, display string) error {
	fd, err := unix.Openat(dirfd, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		// Removed concurrently; nothing to conform.
		return nil
	}
	if err != nil {
		return &fs.PathError{Op: "open", Path: display, Err: err}
	}
	closeFD := func() { _ = unix.Close(fd) }

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		closeFD()
		return &fs.PathError{Op: "stat", Path: display, Err: err}
	}
	typ := st.Mode & unix.S_IFMT
	if typ == unix.S_IFDIR && st.Dev != w.dev {
		// A mount point: another filesystem's root is not part of the tree.
		closeFD()
		return nil
	}

	if err := w.conformEntry(fd, &st, display); err != nil {
		closeFD()
		return err
	}
	if typ != unix.S_IFDIR {
		closeFD()
		return nil
	}

	// Reopen the directory itself for listing. "." relative to the O_PATH
	// descriptor refers to exactly the inode checked above.
	dfd, err := unix.Openat(fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	closeFD()
	if err != nil {
		return &fs.PathError{Op: "open", Path: display, Err: err}
	}
	sub := os.NewFile(uintptr(dfd), display)
	defer func() { _ = sub.Close() }()
	return w.walkDir(sub, display)
}

// conformEntry counts the entry behind the O_PATH descriptor fd if it does not
// match the spec and, when fixing, corrects it through fd.
func (w *treeWalker) conformEntry(fd int, st *unix.Stat_t, display string) error {
	uid, gid := -1, -1
	if w.spec.ownership {
		if st.Uid != w.uid {
			uid = int(w.uid)
		}
		if st.Gid != w.gid {
			gid = int(w.gid)
		}
	}
	needChown := uid != -1 || gid != -1

	var wantMode uint32
	hasMode := false
	if w.spec.modes {
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			wantMode, hasMode = w.spec.dirMode, true
		case unix.S_IFREG:
			wantMode, hasMode = w.spec.fileMode, true
		}
	}
	needChmod := hasMode && st.Mode&0o7777 != wantMode

	if !needChown && !needChmod {
		return nil
	}
	w.count++
	if !w.fix {
		return nil
	}

	if needChown {
		// AT_EMPTY_PATH acts on fd itself; for a symlink that is the link,
		// never its target.
		if err := unix.Fchownat(fd, "", uid, gid, unix.AT_EMPTY_PATH); err != nil {
			return &fs.PathError{Op: "chown", Path: display, Err: err}
		}
	}
	// chown clears the setuid/setgid bits of regular files, so the mode is
	// (re)applied after any ownership change.
	if hasMode && (needChmod || needChown) {
		if err := chmodPathFD(fd, wantMode); err != nil {
			return &fs.PathError{Op: "chmod", Path: display, Err: err}
		}
	}
	return nil
}

// chmodPathFD changes the mode of the file behind the O_PATH descriptor fd.
// fchmod(2) does not accept O_PATH descriptors, so fchmodat2 with
// AT_EMPTY_PATH (Linux 6.6+) is used, falling back to the descriptor's
// /proc/self/fd magic link on older kernels (as glibc does). Both act on the
// inode fd refers to and cannot be redirected by a path swap.
func chmodPathFD(fd int, mode uint32) error {
	err := unix.Fchmodat(fd, "", mode, unix.AT_EMPTY_PATH)
	if !errors.Is(err, unix.EOPNOTSUPP) && !errors.Is(err, unix.ENOSYS) {
		return err
	}
	return chmodViaProcFD(fd, mode)
}

// chmodViaProcFD changes the mode of the file behind fd through its
// /proc/self/fd magic link.
func chmodViaProcFD(fd int, mode uint32) error {
	procPath := "/proc/self/fd/" + strconv.Itoa(fd)
	if err := unix.Chmod(procPath, mode); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("kernel lacks fchmodat2 and /proc is not mounted: %w", err)
		}
		return err
	}
	return nil
}
