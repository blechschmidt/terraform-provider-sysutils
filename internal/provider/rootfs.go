package provider

// Confinement of managed paths to the provider's root_dir.
//
// With root_dir set to something other than "/", every path in the
// configuration is interpreted relative to root_dir, as if the provider were
// running in a chroot there: path "/etc/hosts" with root_dir "/srv/rootfs"
// refers to the host path "/srv/rootfs/etc/hosts". This is how a container
// image or OS image is built from an unpacked root filesystem tree.
//
// Such a tree is not trusted: it may come from an image and contain
// arbitrary symlinks. Letting the kernel resolve paths inside it would
// follow a symlink such as "etc -> /etc" out of the tree and onto the host.
// fsRoot.resolve therefore resolves every path component itself, one at a
// time, with the following rules:
//
//   - A symlink with an absolute target is resolved relative to root_dir,
//     which is what the link means inside the tree (chroot semantics).
//   - A ".." that would climb above root_dir, whether it comes from a
//     relative symlink target or not, is refused with errEscapesRoot.
//   - At most maxSymlinkHops symlinks are followed, like the kernel's limit.
//
// The result is a host path whose ancestors were real directories inside
// root_dir at the time of resolution. As for the host root (see safefs.go),
// the provider assumes that nobody modifies the tree concurrently with a
// Terraform run; the final component is still never followed by the
// resources, and recursive deletion still refuses any symlinked component.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
)

// maxSymlinkHops bounds the number of symlinks followed while resolving one
// path, like the kernel's MAXSYMLINKS.
const maxSymlinkHops = 40

// errEscapesRoot is wrapped by errors for paths or symlinks that lead outside
// root_dir.
var errEscapesRoot = errors.New("escapes root_dir")

// fsRoot is the directory that managed paths are confined to. The zero value
// and a nil *fsRoot both mean the host's root directory, "/".
type fsRoot struct {
	// dir is the absolute, canonical host path of the root directory.
	dir string
}

// hostRoot is the default root: paths are used as they are.
var hostRoot = &fsRoot{dir: "/"}

// newFSRoot returns the root for the root_dir value dir, which must be an
// absolute path in canonical form. It does not need to exist yet; that is
// checked whenever a path is resolved.
func newFSRoot(dir string) (*fsRoot, error) {
	if err := validateCanonicalPath(dir); err != nil {
		return nil, err
	}
	return &fsRoot{dir: dir}, nil
}

// isHost reports whether r is the host's root directory, in which case paths
// are used unchanged.
func (r *fsRoot) isHost() bool {
	return r == nil || r.dir == "" || r.dir == "/"
}

// String returns the root directory's path.
func (r *fsRoot) String() string {
	if r.isHost() {
		return "/"
	}
	return r.dir
}

// resolve maps the managed path p to the host path to operate on. Every
// component except the last is resolved inside the root, following symlinks
// as described in the file comment; the last component is not followed, so
// callers still see (and refuse, or manage) a symlink at p itself. p must be
// absolute and canonical. Components that do not exist are kept as they are,
// so that the result can be created.
func (r *fsRoot) resolve(p string) (string, error) {
	return r.resolvePath(p, false)
}

// resolveFollow is like resolve, but also follows a symlink at p itself, as
// the data sources do. The result contains no symlinks at all.
func (r *fsRoot) resolveFollow(p string) (string, error) {
	return r.resolvePath(p, true)
}

func (r *fsRoot) resolvePath(p string, followLast bool) (string, error) {
	if err := validateCanonicalPath(p); err != nil {
		return "", err
	}
	if r.isHost() {
		return p, nil
	}
	// root_dir itself is trusted configuration, so symlinks in it are
	// followed normally. The walk below then works on a symlink-free path.
	base, err := filepath.EvalSymlinks(r.dir)
	if err != nil {
		return "", fmt.Errorf("cannot use %q as root_dir: %w", r.dir, err)
	}
	if info, err := os.Stat(base); err != nil {
		return "", fmt.Errorf("cannot use %q as root_dir: %w", r.dir, err)
	} else if !info.IsDir() {
		return "", fmt.Errorf("cannot use %q as root_dir: not a directory", r.dir)
	}

	var (
		resolved []string       // Components below base, all verified.
		todo     = splitPath(p) // Components still to resolve.
		hops     int            // Symlinks followed so far.
		host     = func() string { return filepath.Join(append([]string{base}, resolved...)...) }
	)
	for len(todo) > 0 {
		name := todo[0]
		todo = todo[1:]
		switch name {
		case "", ".":
			continue
		case "..":
			if len(resolved) == 0 {
				return "", fmt.Errorf("path %q %w %q: a symbolic link leads above it", p, errEscapesRoot, r.dir)
			}
			resolved = resolved[:len(resolved)-1]
			continue
		}

		next := filepath.Join(host(), name)
		info, err := os.Lstat(next)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// Nothing below a missing directory exists either, so the rest
			// of the path cannot contain symlinks. A ".." after it can only
			// come from a symlink target; the kernel would fail such a
			// lookup, and so does this.
			for _, rest := range todo {
				if rest == ".." {
					return "", &fs.PathError{Op: "resolve", Path: p, Err: syscall.ENOENT}
				}
			}
			resolved = append(resolved, name)
			for _, rest := range todo {
				if rest != "" && rest != "." {
					resolved = append(resolved, rest)
				}
			}
			return host(), nil
		case err != nil:
			return "", err
		}

		last := len(todo) == 0
		if info.Mode()&fs.ModeSymlink == 0 || (last && !followLast) {
			if !last && !info.IsDir() {
				return "", &fs.PathError{Op: "resolve", Path: next, Err: syscall.ENOTDIR}
			}
			resolved = append(resolved, name)
			continue
		}

		hops++
		if hops > maxSymlinkHops {
			return "", &fs.PathError{Op: "resolve", Path: p, Err: syscall.ELOOP}
		}
		target, err := os.Readlink(next)
		if err != nil {
			return "", err
		}
		if strings.HasPrefix(target, "/") {
			// Absolute targets are relative to the root, not to the host.
			resolved = resolved[:0]
		}
		todo = append(splitPath(target), todo...)
	}
	return host(), nil
}

// inRoot returns the path inside the root of host, a path returned by
// resolve or resolveFollow. It differs from the managed path that was
// resolved if symlinks in the tree redirected it: with "/a" a symlink to
// "/", the managed path "/a/etc" is "/etc" inside the root. Checks that
// depend on where a path really is, such as the list of protected
// directories, must use this path rather than the configured one.
func (r *fsRoot) inRoot(host string) (string, error) {
	if r.isHost() {
		return host, nil
	}
	base, err := filepath.EvalSymlinks(r.dir)
	if err != nil {
		return "", fmt.Errorf("cannot use %q as root_dir: %w", r.dir, err)
	}
	rel, err := filepath.Rel(base, host)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("path %q %w %q", host, errEscapesRoot, r.dir)
	}
	return filepath.Join("/", rel), nil
}

// splitPath splits p into its components. Leading, trailing and duplicate
// slashes produce empty components, which callers skip.
func splitPath(p string) []string {
	return strings.Split(strings.Trim(p, "/"), "/")
}

// checkSymlinkTargetInRoot reports an error if the symlink target, stored in
// a link at the managed path link, would lead above the root when resolved
// inside it. Absolute targets always stay inside the root (they are
// resolved relative to it); relative targets are checked lexically against
// the link's directory. With the host root every target is accepted.
func (r *fsRoot) checkSymlinkTargetInRoot(link, target string) error {
	if r.isHost() || strings.HasPrefix(target, "/") {
		return nil
	}
	depth := len(splitPath(filepath.Dir(link)))
	if filepath.Dir(link) == "/" {
		depth = 0
	}
	for _, name := range splitPath(target) {
		switch name {
		case "", ".":
		case "..":
			depth--
			if depth < 0 {
				return fmt.Errorf("symlink target %q of %q %w (%q)", target, link, errEscapesRoot, r.dir)
			}
		default:
			depth++
		}
	}
	return nil
}

// resolvePathAttr resolves the managed path p inside root, following a
// symlink at p itself only if follow is set, and reports a failure as an
// error on the path attribute.
func resolvePathAttr(root *fsRoot, p string, follow bool) (string, diag.Diagnostics) {
	var diags diag.Diagnostics
	resolve := root.resolve
	if follow {
		resolve = root.resolveFollow
	}
	host, err := resolve(p)
	if err != nil {
		diags.AddAttributeError(path.Root("path"), "Unable to resolve path", capitalize(err.Error())+".")
		return "", diags
	}
	return host, diags
}
