package provider

// Extracting archives for sysutils_archive_extract.
//
// An archive is never extracted directly into its destination. It is first
// extracted in full into a new, private (0700) temporary directory, and its
// owner, group and modes are applied there. Only once walkArchive has
// validated the whole archive, including every symlink, is the result moved
// into place:
//
//   - If the destination does not exist, the temporary directory is created
//     next to it and renamed to it, so the whole tree appears at once.
//   - Otherwise the temporary directory is created inside the destination,
//     on the same filesystem, and every top-level entry (or every entry
//     that is new below a directory that exists already) is renamed into
//     place. Each file appears atomically, with its final contents, mode and
//     owner. Before anything is moved, every destination path is checked,
//     so that a conflict leaves the destination untouched.
//
// All operations on the destination tree go through directory descriptors
// opened with O_NOFOLLOW, component by component, so no symlink in the tree
// (whether shipped by an earlier archive or planted by someone else) is ever
// followed, and existing symlinks are replaced rather than written through.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

// extractSpec are the settings that determine what the extracted tree looks
// like.
type extractSpec struct {
	limits archiveLimits
	// uid and gid are given to every extracted entry; -1 leaves the owner
	// (the user running Terraform) unchanged.
	uid, gid int
	// fileMode and dirMode, if set, replace the modes recorded in the
	// archive for regular files and directories.
	fileMode, dirMode *fs.FileMode
}

// modeFor returns the mode that e gets when extracted.
func (s extractSpec) modeFor(e *archiveEntry) fs.FileMode {
	switch {
	case e.Kind == archiveDir && s.dirMode != nil:
		return *s.dirMode
	case e.Kind != archiveDir && s.fileMode != nil:
		return *s.fileMode
	default:
		return e.Mode
	}
}

// rootDirMode is the mode of a destination directory created by the
// extraction.
func (s extractSpec) rootDirMode() fs.FileMode {
	if s.dirMode != nil {
		return *s.dirMode
	}
	return 0o755
}

// splitEntryName splits a manifest name into its parent ("" for the
// destination itself) and base name.
func splitEntryName(name string) (dir, base string) {
	i := strings.LastIndexByte(name, '/')
	if i < 0 {
		return "", name
	}
	return name[:i], name[i+1:]
}

const dirOpenFlags = unix.O_PATH | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

// errSymlinkInTree is wrapped by openDirAt errors for a path component that
// is a symlink or not a directory.
var errSymlinkInTree = errors.New("is a symbolic link or not a directory")

// openDirAt opens the directory rel, a slash-separated path relative to
// dirfd, component by component without following symlinks. With create,
// missing components are created with mode 0700. The returned O_PATH
// descriptor is only suitable as the dirfd of *at calls. display is the path
// of dirfd for error messages.
func openDirAt(dirfd int, rel, display string, create bool) (int, error) {
	fd, err := unix.Openat(dirfd, ".", dirOpenFlags, 0)
	if err != nil {
		return -1, &fs.PathError{Op: "open", Path: display, Err: err}
	}
	if rel == "" {
		return fd, nil
	}
	cur := display
	for _, name := range strings.Split(rel, "/") {
		cur = filepath.Join(cur, name)
		next, err := unix.Openat(fd, name, dirOpenFlags, 0)
		if errors.Is(err, unix.ENOENT) && create {
			if err = unix.Mkdirat(fd, name, 0o700); err == nil || errors.Is(err, unix.EEXIST) {
				next, err = unix.Openat(fd, name, dirOpenFlags, 0)
			}
		}
		if err != nil {
			_ = unix.Close(fd)
			if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
				return -1, fmt.Errorf("path %q %w", cur, errSymlinkInTree)
			}
			return -1, &fs.PathError{Op: "open", Path: cur, Err: err}
		}
		_ = unix.Close(fd)
		fd = next
	}
	return fd, nil
}

// extractToDir extracts the archive f into the empty directory tmpfd and
// gives every entry the owner, group and mode of spec. display is the path
// of tmpfd for error messages.
func extractToDir(ctx context.Context, f *os.File, tmpfd int, display string, spec extractSpec) (*archiveManifest, error) {
	buf := make([]byte, 128<<10)
	m, err := walkArchive(f, spec.limits, func(e *archiveEntry, content io.Reader) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		dir, base := splitEntryName(e.Name)
		pfd, err := openDirAt(tmpfd, dir, display, true)
		if err != nil {
			return err
		}
		defer func() { _ = unix.Close(pfd) }()
		p := filepath.Join(display, e.Name)
		switch e.Kind {
		case archiveDir:
			err := unix.Mkdirat(pfd, base, 0o700)
			if errors.Is(err, unix.EEXIST) {
				// Created implicitly for an earlier entry below it.
				var st unix.Stat_t
				if err = unix.Fstatat(pfd, base, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil && st.Mode&unix.S_IFMT != unix.S_IFDIR {
					err = unix.EEXIST
				}
			}
			if err != nil {
				return &fs.PathError{Op: "mkdir", Path: p, Err: err}
			}
		case archiveFile:
			fd, err := unix.Openat(pfd, base, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
			if err != nil {
				return &fs.PathError{Op: "create", Path: p, Err: err}
			}
			out := os.NewFile(uintptr(fd), p)
			_, err = io.CopyBuffer(out, content, buf)
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return fmt.Errorf("extracting %q: %w", e.Name, err)
			}
		case archiveSymlink:
			if err := unix.Symlinkat(e.Link, pfd, base); err != nil {
				return &fs.PathError{Op: "symlink", Path: p, Err: err}
			}
		case archiveHardlink:
			tdir, tbase := splitEntryName(e.Link)
			tfd, err := openDirAt(tmpfd, tdir, display, false)
			if err != nil {
				return err
			}
			// Without AT_SYMLINK_FOLLOW, linkat never follows tbase.
			err = unix.Linkat(tfd, tbase, pfd, base, 0)
			_ = unix.Close(tfd)
			if err != nil {
				return &os.LinkError{Op: "link", Old: filepath.Join(display, e.Link), New: p, Err: err}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := applyExtractAttrs(tmpfd, display, m, spec); err != nil {
		return nil, err
	}
	return m, nil
}

// applyExtractAttrs gives every entry of m below dirfd its owner, group,
// mode and modification time. Entries are handled children first, so that a
// directory mode without search permission applies only after its contents
// are done.
func applyExtractAttrs(dirfd int, display string, m *archiveManifest, spec extractSpec) error {
	names := m.names()
	for i := len(names) - 1; i >= 0; i-- {
		e := m.entries[names[i]]
		if e.Kind == archiveHardlink {
			continue // Shares the inode, and the attributes, of its target.
		}
		dir, base := splitEntryName(e.Name)
		pfd, err := openDirAt(dirfd, dir, display, false)
		if err != nil {
			return err
		}
		err = applyEntryAttrs(pfd, base, e, spec)
		_ = unix.Close(pfd)
		if err != nil {
			return fmt.Errorf("%q: %w", filepath.Join(display, e.Name), err)
		}
	}
	return nil
}

// applyEntryAttrs gives the entry base below pfd the owner, group, mode and
// modification time of e. The entry is pinned with O_PATH and O_NOFOLLOW and
// must still have e's type, and ownership and mode are changed through that
// descriptor: a directory of the destination that the archive is merged into
// may have been swapped since it was checked, for a symlink or for a hard
// link to another file, and neither may be changed in its place.
func applyEntryAttrs(pfd int, base string, e *archiveEntry, spec extractSpec) error {
	fd, err := unix.Openat(pfd, base, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != archiveKindType(e.Kind) {
		return fmt.Errorf("is no longer a %s; refusing to change it", e.Kind)
	}
	if spec.uid >= 0 || spec.gid >= 0 {
		// Before chmod: chown clears the setuid and setgid bits. With
		// AT_EMPTY_PATH, a symlink's own ownership is changed.
		if err := unix.Fchownat(fd, "", spec.uid, spec.gid, unix.AT_EMPTY_PATH); err != nil {
			return fmt.Errorf("setting ownership: %w", err)
		}
	}
	if e.Kind != archiveSymlink {
		if err := chmodPathFD(fd, unixModeBits(spec.modeFor(e))); err != nil {
			return fmt.Errorf("setting mode: %w", err)
		}
	}
	if !e.Implicit && !e.ModTime.IsZero() {
		ts := []unix.Timespec{unix.NsecToTimespec(e.ModTime.UnixNano()), unix.NsecToTimespec(e.ModTime.UnixNano())}
		if err := unix.UtimesNanoAt(pfd, base, ts, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("setting modification time: %w", err)
		}
	}
	return nil
}

// archiveKindType returns the S_IFMT file type that an entry of kind k has
// once extracted.
func archiveKindType(k archiveKind) uint32 {
	switch k {
	case archiveDir:
		return unix.S_IFDIR
	case archiveSymlink:
		return unix.S_IFLNK
	default:
		return unix.S_IFREG
	}
}

// fchmodatNoFollow changes the mode of name below dirfd, failing with ELOOP
// if it is a symlink. fchmodat(2) always follows a symlink at name, so a
// directory of the destination that someone swapped for a symlink since it
// was checked would have the mode of the link's target changed instead.
// The entry is pinned with O_PATH and O_NOFOLLOW and changed through that
// descriptor.
func fchmodatNoFollow(dirfd int, name string, mode uint32) error {
	fd, err := unix.Openat(dirfd, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT == unix.S_IFLNK {
		return unix.ELOOP
	}
	return chmodPathFD(fd, mode)
}

// installResult is what installArchive did.
type installResult struct {
	manifest *archiveManifest
	// created is set if the destination did not exist and was created.
	created bool
	// placed is set once entries may have been moved into the destination,
	// even if an error occurred afterwards.
	placed bool
	// existingDirs are the directories of the manifest that existed in the
	// destination before, and that the archive was merged into.
	existingDirs []string
}

// installArchive extracts the archive f into the directory dest, a host
// path. owned holds the names that the previous extraction of this resource
// created: existing files with these names are replaced, and with overwrite
// any existing file is. Otherwise an existing file at a path the archive
// contains is a conflict, and nothing is changed.
func installArchive(ctx context.Context, f *os.File, dest string, spec extractSpec, owned map[string]bool, overwrite bool) (installResult, error) {
	info, err := os.Lstat(dest)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return installNew(ctx, f, dest, spec)
	case err != nil:
		return installResult{}, err
	case info.Mode()&fs.ModeSymlink != 0:
		return installResult{}, symlinkRefusedError(dest)
	case !info.IsDir():
		return installResult{}, fmt.Errorf("destination %q exists but is not a directory", dest)
	}

	destfd, err := unix.Open(dest, dirOpenFlags, 0)
	if err != nil {
		return installResult{}, &fs.PathError{Op: "open", Path: dest, Err: err}
	}
	defer func() { _ = unix.Close(destfd) }()

	tmpName := ".sysutils-extract-" + randomID()
	tmpDisplay := filepath.Join(dest, tmpName)
	if err := unix.Mkdirat(destfd, tmpName, 0o700); err != nil {
		return installResult{}, &fs.PathError{Op: "mkdir", Path: tmpDisplay, Err: err}
	}
	defer removeTempTree(destfd, tmpName, tmpDisplay)
	tmpfd, err := openDirAt(destfd, tmpName, dest, false)
	if err != nil {
		return installResult{}, err
	}
	defer func() { _ = unix.Close(tmpfd) }()

	m, err := extractToDir(ctx, f, tmpfd, tmpDisplay, spec)
	if err != nil {
		return installResult{}, err
	}
	res := installResult{manifest: m}
	actions, err := planMerge(destfd, dest, m, owned, overwrite)
	if err != nil {
		return res, err
	}
	for _, name := range m.names() {
		if actions[name] == mergeExistingDir {
			res.existingDirs = append(res.existingDirs, name)
		}
	}
	syncFS(tmpfd)
	res.placed = true
	if err := applyMerge(destfd, dest, tmpfd, tmpDisplay, m, actions, spec); err != nil {
		return res, err
	}
	syncFS(destfd)
	return res, nil
}

// installNew extracts f into a temporary directory next to dest, which does
// not exist, and renames it to dest.
func installNew(ctx context.Context, f *os.File, dest string, spec extractSpec) (installResult, error) {
	parent, base := filepath.Dir(dest), filepath.Base(dest)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return installResult{}, err
	}
	// Symlinks in the parent's path are followed, as for every other
	// resource; the destination itself and everything below it are not.
	pfd, err := unix.Open(parent, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return installResult{}, &fs.PathError{Op: "open", Path: parent, Err: err}
	}
	defer func() { _ = unix.Close(pfd) }()

	tmpName := "." + base + ".sysutils-tmp-" + randomID()
	tmpDisplay := filepath.Join(parent, tmpName)
	if err := unix.Mkdirat(pfd, tmpName, 0o700); err != nil {
		return installResult{}, &fs.PathError{Op: "mkdir", Path: tmpDisplay, Err: err}
	}
	renamed := false
	defer func() {
		if !renamed {
			removeTempTree(pfd, tmpName, tmpDisplay)
		}
	}()
	tmpfd, err := openDirAt(pfd, tmpName, parent, false)
	if err != nil {
		return installResult{}, err
	}
	defer func() { _ = unix.Close(tmpfd) }()

	m, err := extractToDir(ctx, f, tmpfd, tmpDisplay, spec)
	if err != nil {
		return installResult{}, err
	}
	root := &archiveEntry{Kind: archiveDir, Implicit: true}
	rootSpec := spec
	mode := spec.rootDirMode()
	rootSpec.dirMode = &mode
	if err := applyEntryAttrs(pfd, tmpName, root, rootSpec); err != nil {
		return installResult{}, fmt.Errorf("%q: %w", dest, err)
	}
	syncFS(tmpfd)
	if err := renameAtNoReplace(pfd, tmpName, pfd, base); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return installResult{}, fmt.Errorf("destination %q was created by another process during the extraction; try again", dest)
		}
		return installResult{}, &os.LinkError{Op: "rename", Old: tmpDisplay, New: dest, Err: err}
	}
	renamed = true
	syncDir(parent)
	return installResult{manifest: m, created: true, placed: true}, nil
}

// mergeAction is what applyMerge does with one entry of the manifest.
type mergeAction int

const (
	// mergeSkip: the entry is moved along with a new directory above it.
	mergeSkip mergeAction = iota
	// mergePlace: the path does not exist; the entry, and everything below
	// it, is renamed into place.
	mergePlace
	// mergeReplace: a file, symlink or special file at the path is
	// atomically replaced by the entry, which is not a directory.
	mergeReplace
	// mergeReplaceWithDir: a file at the path is removed, and the entry,
	// a directory, is renamed into place.
	mergeReplaceWithDir
	// mergeExistingDir: a directory exists at the path; the entry, a
	// directory as well, is merged into it.
	mergeExistingDir
)

// planMerge decides, before anything is changed, what applyMerge does with
// every entry of m, and fails on the first conflict.
func planMerge(destfd int, dest string, m *archiveManifest, owned map[string]bool, overwrite bool) (map[string]mergeAction, error) {
	actions := make(map[string]mergeAction, len(m.entries))
	for _, name := range m.names() {
		dir, base := splitEntryName(name)
		if dir != "" {
			if a := actions[dir]; a != mergeExistingDir {
				actions[name] = mergeSkip // Below a directory that is moved as a whole.
				continue
			}
		}
		e := m.entries[name]
		p := filepath.Join(dest, name)
		pfd, err := openDirAt(destfd, dir, dest, false)
		if err != nil {
			return nil, err
		}
		var st unix.Stat_t
		err = unix.Fstatat(pfd, base, &st, unix.AT_SYMLINK_NOFOLLOW)
		_ = unix.Close(pfd)
		switch {
		case errors.Is(err, unix.ENOENT):
			actions[name] = mergePlace
			continue
		case err != nil:
			return nil, &fs.PathError{Op: "lstat", Path: p, Err: err}
		}
		isDir := st.Mode&unix.S_IFMT == unix.S_IFDIR
		switch {
		case isDir && e.Kind == archiveDir:
			actions[name] = mergeExistingDir
		case isDir:
			return nil, fmt.Errorf("cannot extract the %s %q: a directory exists at %q; remove it first", e.Kind, name, p)
		case !owned[name] && !overwrite:
			return nil, fmt.Errorf("%q already exists and was not extracted by this resource; remove it, or set overwrite = true to replace it", p)
		case e.Kind == archiveDir:
			actions[name] = mergeReplaceWithDir
		default:
			actions[name] = mergeReplace
		}
	}
	return actions, nil
}

// applyMerge moves the entries of m from the temporary directory tmpfd into
// the destination destfd as planned by planMerge.
func applyMerge(destfd int, dest string, tmpfd int, tmpDisplay string, m *archiveManifest, actions map[string]mergeAction, spec extractSpec) error {
	for _, name := range m.names() {
		action := actions[name]
		if action == mergeSkip {
			continue
		}
		e := m.entries[name]
		dir, base := splitEntryName(name)
		p := filepath.Join(dest, name)
		if err := func() error {
			dfd, err := openDirAt(destfd, dir, dest, false)
			if err != nil {
				return err
			}
			defer func() { _ = unix.Close(dfd) }()
			if action == mergeExistingDir {
				// Implicit directories are the destination's own, such
				// as /usr for an archive of usr/local/bin/tool: leave them
				// alone. Explicit ones get the archive's attributes, as
				// tar gives them.
				if e.Implicit {
					return nil
				}
				if err := applyEntryAttrs(dfd, base, e, spec); err != nil {
					return fmt.Errorf("%q: %w", p, err)
				}
				return nil
			}
			tfd, err := openDirAt(tmpfd, dir, tmpDisplay, false)
			if err != nil {
				return err
			}
			defer func() { _ = unix.Close(tfd) }()
			switch action {
			case mergeReplace:
				// rename(2) replaces the entry itself; a symlink at p is
				// not followed.
				err = unix.Renameat(tfd, base, dfd, base)
			case mergeReplaceWithDir:
				if err = unix.Unlinkat(dfd, base, 0); err == nil || errors.Is(err, unix.ENOENT) {
					err = renameAtNoReplace(tfd, base, dfd, base)
				}
			default:
				err = renameAtNoReplace(tfd, base, dfd, base)
			}
			if err != nil {
				return &os.LinkError{Op: "rename", Old: filepath.Join(tmpDisplay, name), New: p, Err: err}
			}
			return nil
		}(); err != nil {
			return err
		}
	}
	return nil
}

// renameAtNoReplace renames oldname below olddirfd to newname below
// newdirfd, failing with EEXIST if newname exists. Filesystems without
// RENAME_NOREPLACE fall back to link-free checking: the rename is refused if
// newname exists right before it.
func renameAtNoReplace(olddirfd int, oldname string, newdirfd int, newname string) error {
	err := unix.Renameat2(olddirfd, oldname, newdirfd, newname, unix.RENAME_NOREPLACE)
	if !errors.Is(err, unix.ENOSYS) && !errors.Is(err, unix.EINVAL) {
		return err
	}
	var st unix.Stat_t
	if err := unix.Fstatat(newdirfd, newname, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		return unix.EEXIST
	}
	return unix.Renameat(olddirfd, oldname, newdirfd, newname)
}

// removeTempTree removes the temporary directory name below dirfd and
// everything in it. Failures are ignored: it only cleans up after an error
// or after a merge, and the directory's name marks it as disposable.
func removeTempTree(dirfd int, name, display string) {
	var st unix.Stat_t
	if unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW) != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return
	}
	// Extracted directories may lack write or search permission for the
	// user running Terraform; restore it before descending.
	_ = fchmodatNoFollow(dirfd, name, 0o700)
	if fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0); err == nil {
		d := os.NewFile(uintptr(fd), display)
		names, _ := d.Readdirnames(-1)
		for _, child := range names {
			if unix.Unlinkat(fd, child, 0) != nil {
				removeTempTree(fd, child, filepath.Join(display, child))
			}
		}
		_ = d.Close()
	}
	_ = unix.Unlinkat(dirfd, name, unix.AT_REMOVEDIR)
}

// syncFS flushes the filesystem that fd is on, so that renamed files are
// on disk with their contents. Failures are ignored, as for syncDir.
func syncFS(fd int) {
	rfd, err := unix.Openat(fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return
	}
	_ = unix.Syncfs(rfd)
	_ = unix.Close(rfd)
}

// removeExtracted removes the entries names (manifest names, with a trailing
// slash for directories) from the destination directory dest, children
// first. Directories are removed only if they are empty, since they may
// hold files that were not extracted. Entries that are gone already are
// skipped, and so are entries whose type changed or whose path now contains
// a symlink; those are returned as warnings, since they are not what was
// extracted.
func removeExtracted(dest string, names []string) (warnings []string, err error) {
	destfd, err := unix.Open(dest, dirOpenFlags, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, nil
		}
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return []string{fmt.Sprintf("Destination %q is no longer a directory and was left untouched.", dest)}, nil
		}
		return nil, &fs.PathError{Op: "open", Path: dest, Err: err}
	}
	defer func() { _ = unix.Close(destfd) }()

	sorted := slices.Clone(names)
	slices.Sort(sorted)
	for i := len(sorted) - 1; i >= 0; i-- {
		name, isDir := strings.CutSuffix(sorted[i], "/")
		if name == "" || !validManifestName(name) {
			continue
		}
		dir, base := splitEntryName(name)
		p := filepath.Join(dest, name)
		pfd, err := openDirAt(destfd, dir, dest, false)
		if err != nil {
			if errors.Is(err, errSymlinkInTree) {
				warnings = append(warnings, fmt.Sprintf("%s; %q was left untouched.", capitalize(err.Error()), p))
				continue
			}
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return warnings, err
		}
		var st unix.Stat_t
		err = unix.Fstatat(pfd, base, &st, unix.AT_SYMLINK_NOFOLLOW)
		switch {
		case errors.Is(err, unix.ENOENT):
		case err != nil:
			err = &fs.PathError{Op: "lstat", Path: p, Err: err}
		case isDir != (st.Mode&unix.S_IFMT == unix.S_IFDIR):
			warnings = append(warnings, fmt.Sprintf("%q is no longer a %s and was left untouched.", p, map[bool]string{true: "directory", false: "file"}[isDir]))
		case isDir:
			if err = unix.Unlinkat(pfd, base, unix.AT_REMOVEDIR); errors.Is(err, unix.ENOTEMPTY) || errors.Is(err, unix.EEXIST) || errors.Is(err, unix.ENOENT) {
				err = nil // Holds files that were not extracted.
			} else if err != nil {
				err = &fs.PathError{Op: "rmdir", Path: p, Err: err}
			}
		default:
			if err = unix.Unlinkat(pfd, base, 0); errors.Is(err, unix.ENOENT) {
				err = nil
			} else if err != nil {
				err = &fs.PathError{Op: "unlink", Path: p, Err: err}
			}
		}
		_ = unix.Close(pfd)
		if err != nil {
			return warnings, err
		}
	}
	syncFS(destfd)
	return warnings, nil
}

// validManifestName reports whether name, read from state, is a name that
// walkArchive could have produced. State is not validated by the schema, so
// names from it are checked before they are used as paths.
func validManifestName(name string) bool {
	n, ok, err := normalizeArchiveName(name, 0)
	return err == nil && ok && n == name
}

// countDrift returns the number of entries of the extracted tree below dest
// that no longer look as extracted. names is the manifest from state. If
// archive is not nil it must be the archive that was extracted, and every
// entry is compared with it in full: type, contents, symlink target, mode
// and (if spec sets them) owner and group. Otherwise only the existence and
// type (directory or not) of each entry is checked.
func countDrift(ctx context.Context, dest string, names []string, archive *os.File, spec extractSpec) (int64, error) {
	destfd, err := unix.Open(dest, dirOpenFlags, 0)
	if err != nil {
		return 0, &fs.PathError{Op: "open", Path: dest, Err: err}
	}
	defer func() { _ = unix.Close(destfd) }()

	var drift int64
	seen := map[string]bool{}
	if archive != nil {
		hashes := map[string][]byte{} // Content checksums of regular files, for hard links.
		m, err := walkArchive(archive, spec.limits, func(e *archiveEntry, content io.Reader) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			var want []byte
			switch e.Kind {
			case archiveFile:
				h := sha256.New()
				if _, err := io.Copy(h, content); err != nil {
					return fmt.Errorf("reading %q from the archive: %w", e.Name, err)
				}
				want = h.Sum(nil)
				hashes[e.Name] = want
			case archiveHardlink:
				want = hashes[e.Link]
			}
			ok, err := entryConforms(destfd, dest, e, want, spec)
			if err != nil {
				return err
			}
			seen[e.Name] = true
			if !ok {
				drift++
			}
			return nil
		})
		if err != nil {
			return 0, err
		}
		// Implicit directories only have to exist.
		for name, e := range m.entries {
			if e.Implicit && !seen[name] {
				seen[name] = true
				if ok, err := entryExists(destfd, dest, name, true); err != nil {
					return 0, err
				} else if !ok {
					drift++
				}
			}
		}
	}
	for _, n := range names {
		name, isDir := strings.CutSuffix(n, "/")
		if seen[name] || !validManifestName(name) {
			continue
		}
		if ok, err := entryExists(destfd, dest, name, isDir); err != nil {
			return 0, err
		} else if !ok {
			drift++
		}
	}
	return drift, nil
}

// lstatEntry returns the attributes of the manifest entry name below destfd,
// without following symlinks anywhere in its path. found is false if it, or
// a directory above it, does not exist, or if a symlink or file has taken
// the place of such a directory.
func lstatEntry(destfd int, dest, name string) (st unix.Stat_t, pfd int, found bool, err error) {
	dir, base := splitEntryName(name)
	pfd, err = openDirAt(destfd, dir, dest, false)
	if err != nil {
		if errors.Is(err, errSymlinkInTree) || errors.Is(err, fs.ErrNotExist) {
			return st, -1, false, nil
		}
		return st, -1, false, err
	}
	err = unix.Fstatat(pfd, base, &st, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		_ = unix.Close(pfd)
		return st, -1, false, nil
	}
	if err != nil {
		_ = unix.Close(pfd)
		return st, -1, false, &fs.PathError{Op: "lstat", Path: filepath.Join(dest, name), Err: err}
	}
	return st, pfd, true, nil
}

// entryExists reports whether the manifest entry name exists below destfd
// as a directory (isDir) or as anything else.
func entryExists(destfd int, dest, name string, isDir bool) (bool, error) {
	st, pfd, found, err := lstatEntry(destfd, dest, name)
	if !found || err != nil {
		return false, err
	}
	_ = unix.Close(pfd)
	return isDir == (st.Mode&unix.S_IFMT == unix.S_IFDIR), nil
}

// entryConforms reports whether the extracted entry e below destfd still
// looks as extracted. sum is the SHA-256 checksum of the contents of a
// regular file or hard link.
func entryConforms(destfd int, dest string, e *archiveEntry, sum []byte, spec extractSpec) (bool, error) {
	st, pfd, found, err := lstatEntry(destfd, dest, e.Name)
	if !found || err != nil {
		return false, err
	}
	defer func() { _ = unix.Close(pfd) }()
	_, base := splitEntryName(e.Name)

	if st.Mode&unix.S_IFMT != archiveKindType(e.Kind) {
		return false, nil
	}
	if (spec.uid >= 0 && st.Uid != uint32(spec.uid)) || (spec.gid >= 0 && st.Gid != uint32(spec.gid)) {
		return false, nil
	}
	switch e.Kind {
	case archiveSymlink:
		buf := make([]byte, maxSymlinkTargetLen+1)
		n, err := unix.Readlinkat(pfd, base, buf)
		if err != nil {
			return false, &fs.PathError{Op: "readlink", Path: filepath.Join(dest, e.Name), Err: err}
		}
		return string(buf[:n]) == e.Link, nil
	case archiveDir:
		return st.Mode&0o7777 == unixModeBits(spec.modeFor(e)), nil
	}
	if st.Mode&0o7777 != unixModeBits(spec.modeFor(e)) || st.Size != e.Size {
		return false, nil
	}
	fd, err := unix.Openat(pfd, base, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, &fs.PathError{Op: "open", Path: filepath.Join(dest, e.Name), Err: err}
	}
	f := os.NewFile(uintptr(fd), filepath.Join(dest, e.Name))
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, e.Size+1)); err != nil {
		return false, err
	}
	return string(h.Sum(nil)) == string(sum), nil
}

// manifestNamesFrom converts the files attribute into the set of entry names
// it holds, without trailing slashes.
func manifestNamesFrom(files []string) map[string]bool {
	set := make(map[string]bool, len(files))
	for _, f := range files {
		name := strings.TrimSuffix(f, "/")
		if validManifestName(name) {
			set[name] = true
		}
	}
	return set
}

// staleEntries returns the entries of old (files attribute values) whose
// names are not in the new manifest m. An entry whose type changed has been
// replaced by the extraction already.
func staleEntries(old []string, m *archiveManifest) []string {
	var stale []string
	for _, f := range old {
		if m.entries[strings.TrimSuffix(f, "/")] == nil {
			stale = append(stale, f)
		}
	}
	return stale
}

// joinManifest returns the union of the files attribute values a and b.
func joinManifest(a, b []string) []string {
	out := slices.Concat(a, b)
	slices.Sort(out)
	return slices.Compact(out)
}
