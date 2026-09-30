package provider

// Whole-file drop-ins in a configuration directory, such as sudoers.d,
// logrotate.d or journald.conf.d: one file per resource, written atomically
// with a fixed mode and owner under the shared-file lock, optionally checked
// by an external tool before it is renamed into place, and removed on
// destroy.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// errDropInExists is returned by writeDropInFile when a new drop-in already
// exists.
var errDropInExists = errors.New("file already exists")

// dropInSpec describes the files of one kind of drop-in.
type dropInSpec struct {
	// mode is the mode of every file written.
	mode fs.FileMode
	// dirMode is the mode the directory is created with if it is missing;
	// its missing parents get 0755.
	dirMode fs.FileMode
	// maxSize bounds how much of a file is read.
	maxSize int64
	// uid and gid own every file written.
	uid, gid uint32
	// tmpSuffix ends the names of temporary files; see replaceAttrs.
	tmpSuffix string
}

// read reads the drop-in at p without following symlinks. A missing file
// reads as nil data with a nil snapshot.
func (s dropInSpec) read(p string) ([]byte, *fileSnapshot, error) {
	data, snap, err := readRegularFileNoFollow(p, s.maxSize)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	return data, snap, err
}

// write atomically makes the drop-in at p contain data, with s's mode and
// owner, whatever the mode and owner of an existing file were. check, if
// not nil, is called with the complete temporary file before it is renamed
// into place; if it fails, p is left untouched. Extended attributes such as
// the SELinux label are carried over, except an ACL. With create set, an
// existing file is an error wrapping errDropInExists. The shared-file lock
// of p is held throughout.
func (s dropInSpec) write(p string, data []byte, create bool, check func(tmp string) error) error {
	unlock, err := lockFileForEdit(p)
	if err != nil {
		return err
	}
	defer unlock()
	_, snap, err := s.read(p)
	if err != nil {
		return err
	}
	if create && snap != nil {
		return fmt.Errorf("%s: %w", p, errDropInExists)
	}
	attrs := replaceAttrs{mode: s.mode, chown: true, uid: s.uid, gid: s.gid, dropACL: true, check: check, tmpSuffix: s.tmpSuffix}
	if snap != nil {
		attrs.xattrs = make(map[string][]byte, len(snap.xattrs))
		for k, v := range snap.xattrs {
			if k != aclAccessXattr {
				attrs.xattrs[k] = v
			}
		}
	} else if err := ensureDropInDir(filepath.Dir(p), s.dirMode); err != nil {
		return err
	}
	return replaceFileAtomicWith(p, data, snap, attrs)
}

// ensureDropInDir creates dir with mode, and its parents with mode 0755, if
// it does not exist.
func ensureDropInDir(dir string, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(dir, mode); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}

// removeDropInFile removes the drop-in at p under the shared-file lock. A
// missing file is not an error; anything other than a regular file is left
// alone and reported. removed tells whether a file was removed.
func removeDropInFile(p string) (removed bool, err error) {
	unlock, err := lockFileForEdit(p)
	if err != nil {
		return false, err
	}
	defer unlock()
	info, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := checkRegularFile(p, info); err != nil {
		return false, err
	}
	snap, err := snapshotOf(p, info)
	if err != nil {
		return false, err
	}
	if err := removeManagedFile(p, snap); err != nil {
		return false, err
	}
	return true, nil
}

// processOwner returns the effective UID and GID of the provider, which own
// the drop-ins it writes: root in production. Tools such as logrotate
// accept configuration files owned by root or by the user running them.
func processOwner() (uid, gid uint32) {
	return uint32(os.Geteuid()), uint32(os.Getegid()) //nolint:gosec // IDs are non-negative.
}
