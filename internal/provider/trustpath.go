package provider

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// checkTrustedDir checks that the directory dir exists and that nobody but
// root or the provider's user can change what it resolves to. It is for
// paths handed to external tools that run as root and follow symlinks
// (mkswap, swapon, update-alternatives): if another user could replace a
// directory on the way, or a symlink on the way, with a symlink of their
// own, the tool could be made to act on any file of the host.
//
// Every directory that resolving dir passes through, including the targets
// of symlinks, must therefore belong to root or the provider's user and not
// be writable by others, unless it is sticky like /tmp; in a sticky
// directory the next entry must belong to root or the provider's user as
// well. open reports whether dir itself is such a sticky directory that
// others can create entries in. what names the file the directory is for
// in messages, such as "swap file".
func checkTrustedDir(dir, what string) (open bool, err error) {
	euid := uint32(os.Geteuid()) //nolint:gosec // UIDs fit in 32 bits.
	trusted := func(st *syscall.Stat_t) bool { return st.Uid == 0 || st.Uid == euid }
	// check checks the directory at p and reports whether it is sticky and
	// writable by others.
	check := func(p string) (bool, error) {
		info, err := os.Stat(p)
		if err != nil {
			return false, fmt.Errorf("directory for %s: %w", what, err)
		}
		if !info.IsDir() {
			return false, fmt.Errorf("%q is not a directory", p)
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return false, nil
		}
		if !trusted(st) {
			return false, fmt.Errorf("directory %q belongs to user %d; %ss must be in a directory owned by root", p, st.Uid, what)
		}
		open := info.Mode()&0o022 != 0
		if open && info.Mode()&fs.ModeSticky == 0 {
			return false, fmt.Errorf("directory %q is writable by other users; %ss must be in a directory only root can write to", p, what)
		}
		return open, nil
	}

	pending := strings.Split(filepath.Clean(dir), "/")
	cur := "/"
	sticky, err := check(cur)
	if err != nil {
		return false, err
	}
	for links := 0; len(pending) > 0; {
		name := pending[0]
		pending = pending[1:]
		switch name {
		case "", ".":
			continue
		case "..":
			// cur has no symlinks, so its parent is the lexical one.
			cur = filepath.Dir(cur)
			if sticky, err = check(cur); err != nil {
				return false, err
			}
			continue
		}
		next := filepath.Join(cur, name)
		info, err := os.Lstat(next)
		if err != nil {
			return false, fmt.Errorf("directory for %s: %w", what, err)
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok && sticky && !trusted(st) {
			return false, fmt.Errorf("%q belongs to user %d, who can replace it in the sticky directory %q", next, st.Uid, cur)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			if links++; links > 40 {
				return false, fmt.Errorf("directory for %s %q: too many levels of symbolic links", what, dir)
			}
			target, err := os.Readlink(next)
			if err != nil {
				return false, fmt.Errorf("directory for %s: %w", what, err)
			}
			if filepath.IsAbs(target) {
				cur = "/"
				if sticky, err = check(cur); err != nil {
					return false, err
				}
			}
			pending = append(strings.Split(target, "/"), pending...)
			continue
		}
		if sticky, err = check(next); err != nil {
			return false, err
		}
		cur = next
	}
	return sticky, nil
}
