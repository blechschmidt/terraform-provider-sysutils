package provider

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// newTestRoot returns a root in a fresh temporary directory, with symlinks
// in the temporary directory's own path resolved so that expected host
// paths can be compared literally.
func newTestRoot(t *testing.T) (*fsRoot, string) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, err := newFSRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	return root, dir
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func TestFSRoot_hostRootIsIdentity(t *testing.T) {
	for _, root := range []*fsRoot{nil, {}, hostRoot} {
		for _, p := range []string{"/", "/etc/hosts", "/var/run/x"} {
			for _, follow := range []bool{false, true} {
				got, err := root.resolvePath(p, follow)
				if err != nil || got != p {
					t.Errorf("resolvePath(%q, %v) with host root = %q, %v; want %q", p, follow, got, err, p)
				}
			}
		}
	}
}

func TestFSRoot_rejectsInvalidPaths(t *testing.T) {
	root, _ := newTestRoot(t)
	for _, p := range []string{"", "etc", "/etc/../x", "/etc/", "//etc"} {
		if _, err := root.resolve(p); err == nil {
			t.Errorf("resolve(%q) succeeded; want an error", p)
		}
	}
	for _, dir := range []string{"", "relative", "/a/../b", "/a/"} {
		if _, err := newFSRoot(dir); err == nil {
			t.Errorf("newFSRoot(%q) succeeded; want an error", dir)
		}
	}
}

func TestFSRoot_resolve(t *testing.T) {
	root, dir := newTestRoot(t)
	outside := t.TempDir()

	mustMkdir(t, filepath.Join(dir, "usr", "lib"))
	mustMkdir(t, filepath.Join(dir, "etc", "app"))
	if err := os.WriteFile(filepath.Join(dir, "etc", "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Typical links of a root filesystem tree.
	mustSymlink(t, "usr/lib", filepath.Join(dir, "lib"))
	mustSymlink(t, "/usr/lib", filepath.Join(dir, "abslib"))
	mustSymlink(t, "../usr/lib", filepath.Join(dir, "etc", "rel"))
	mustSymlink(t, "app", filepath.Join(dir, "etc", "current"))
	// A link to the host: inside the root it means <root>/<outside>.
	mustSymlink(t, outside, filepath.Join(dir, "hostlink"))
	// A dangling link.
	mustSymlink(t, "/nonexistent/target", filepath.Join(dir, "dangling"))

	tests := []struct {
		p            string
		want         string // Relative to dir.
		wantFollowed string // Relative to dir; defaults to want.
	}{
		{p: "/", want: ""},
		{p: "/etc/app/config", want: "etc/app/config"},
		{p: "/missing/a/b", want: "missing/a/b"},
		{p: "/lib/libc.so", want: "usr/lib/libc.so"},
		{p: "/abslib/libc.so", want: "usr/lib/libc.so"},
		{p: "/etc/rel/x", want: "usr/lib/x"},
		{p: "/etc/current/x", want: "etc/app/x"},
		{p: "/hostlink/x", want: strings.TrimPrefix(outside, "/") + "/x"},
		// The last component is followed only by resolveFollow.
		{p: "/lib", want: "lib", wantFollowed: "usr/lib"},
		{p: "/etc/current", want: "etc/current", wantFollowed: "etc/app"},
		{p: "/dangling", want: "dangling", wantFollowed: "nonexistent/target"},
	}
	for _, tt := range tests {
		wantFollowed := tt.wantFollowed
		if wantFollowed == "" {
			wantFollowed = tt.want
		}
		for _, c := range []struct {
			follow bool
			want   string
		}{{false, tt.want}, {true, wantFollowed}} {
			got, err := root.resolvePath(tt.p, c.follow)
			if want := filepath.Join(dir, c.want); err != nil || got != want {
				t.Errorf("resolvePath(%q, %v) = %q, %v; want %q", tt.p, c.follow, got, err, want)
			}
		}
	}
}

// TestFSRoot_escapeAttempts checks that no symlink inside the root can make
// a path resolve to a location outside it.
func TestFSRoot_escapeAttempts(t *testing.T) {
	root, dir := newTestRoot(t)
	depth := strings.Count(dir, "/")

	mustMkdir(t, filepath.Join(dir, "etc"))
	// Enough ".." to reach the host's / from anywhere in the root.
	up := strings.Repeat("../", depth+2)
	mustSymlink(t, up+"etc", filepath.Join(dir, "etc", "escape"))
	mustSymlink(t, "..", filepath.Join(dir, "dotdot"))
	mustSymlink(t, "../..", filepath.Join(dir, "etc", "dotdot2"))
	mustSymlink(t, "/etc/../../x", filepath.Join(dir, "absup"))
	// A chain whose second hop escapes.
	mustSymlink(t, "/dotdot", filepath.Join(dir, "chain"))
	// Escapes that are only reached through an otherwise harmless link.
	mustSymlink(t, "etc", filepath.Join(dir, "via"))

	for _, p := range []string{
		"/etc/escape/passwd",
		"/dotdot/x",
		"/etc/dotdot2/x",
		"/absup/y",
		"/chain/x",
		"/via/escape/shadow",
	} {
		for _, follow := range []bool{false, true} {
			got, err := root.resolvePath(p, follow)
			if !errors.Is(err, errEscapesRoot) {
				t.Errorf("resolvePath(%q, %v) = %q, %v; want an error wrapping errEscapesRoot", p, follow, got, err)
			}
		}
	}

	// As the last component, an escaping link is only refused when it is
	// followed; the resources then manage (or refuse) the link itself.
	for _, p := range []string{"/etc/escape", "/dotdot", "/chain"} {
		got, err := root.resolve(p)
		if want := filepath.Join(dir, p); err != nil || got != want {
			t.Errorf("resolve(%q) = %q, %v; want %q", p, got, err, want)
		}
		if got, err := root.resolveFollow(p); !errors.Is(err, errEscapesRoot) {
			t.Errorf("resolveFollow(%q) = %q, %v; want an error wrapping errEscapesRoot", p, got, err)
		}
	}
}

func TestFSRoot_symlinkLoop(t *testing.T) {
	root, dir := newTestRoot(t)
	mustSymlink(t, "b", filepath.Join(dir, "a"))
	mustSymlink(t, "a", filepath.Join(dir, "b"))
	mustSymlink(t, "self/x", filepath.Join(dir, "self"))

	for _, p := range []string{"/a/x", "/self/y"} {
		if _, err := root.resolve(p); !errors.Is(err, syscall.ELOOP) {
			t.Errorf("resolve(%q) error = %v; want ELOOP", p, err)
		}
	}
	if _, err := root.resolveFollow("/a"); !errors.Is(err, syscall.ELOOP) {
		t.Errorf("resolveFollow(/a) error = %v; want ELOOP", err)
	}
}

func TestFSRoot_nonDirectoryAndMissingComponents(t *testing.T) {
	root, dir := newTestRoot(t)
	if err := os.WriteFile(filepath.Join(dir, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := root.resolve("/file/x"); !errors.Is(err, syscall.ENOTDIR) {
		t.Errorf("resolve(/file/x) error = %v; want ENOTDIR", err)
	}
	// ".." after a missing component cannot be resolved, just like in the
	// kernel; it must not be applied lexically either.
	mustSymlink(t, "missing/../../x", filepath.Join(dir, "weird"))
	if got, err := root.resolve("/weird/y"); err == nil {
		t.Errorf("resolve(/weird/y) = %q; want an error", got)
	}
}

func TestFSRoot_rootDirItself(t *testing.T) {
	_, dir := newTestRoot(t)
	realDir := filepath.Join(dir, "real")
	mustMkdir(t, realDir)
	mustSymlink(t, "real", filepath.Join(dir, "alias"))

	// Symlinks in root_dir itself are trusted configuration and followed.
	root, err := newFSRoot(filepath.Join(dir, "alias"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := root.resolve("/etc/x"); err != nil || got != filepath.Join(realDir, "etc", "x") {
		t.Errorf("resolve(/etc/x) = %q, %v; want %q", got, err, filepath.Join(realDir, "etc", "x"))
	}

	missing, err := newFSRoot(filepath.Join(dir, "missing"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := missing.resolve("/etc/x"); err == nil || !strings.Contains(err.Error(), "root_dir") {
		t.Errorf("resolve with missing root_dir: error = %v; want one mentioning root_dir", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "notdir"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	notDir, err := newFSRoot(filepath.Join(dir, "notdir"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := notDir.resolve("/etc/x"); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("resolve with a file as root_dir: error = %v; want 'not a directory'", err)
	}
}

func TestFSRoot_checkSymlinkTargetInRoot(t *testing.T) {
	root, _ := newTestRoot(t)
	tests := []struct {
		link, target string
		ok           bool
	}{
		{"/etc/localtime", "/usr/share/zoneinfo/UTC", true},
		{"/lib", "usr/lib", true},
		{"/etc/alt/x", "../../usr/bin/x", true},
		{"/etc/alt/x", "../../../usr/bin/x", false},
		{"/x", "..", false},
		{"/x", "../y", false},
		{"/a/b", "c/../../..", false},
		{"/a/b", "./c/./../..", true},
		{"/a/b", "c//d", true},
	}
	for _, tt := range tests {
		err := root.checkSymlinkTargetInRoot(tt.link, tt.target)
		if tt.ok && err != nil {
			t.Errorf("checkSymlinkTargetInRoot(%q, %q) = %v; want nil", tt.link, tt.target, err)
		}
		if !tt.ok && !errors.Is(err, errEscapesRoot) {
			t.Errorf("checkSymlinkTargetInRoot(%q, %q) = %v; want errEscapesRoot", tt.link, tt.target, err)
		}
	}
	// The host root accepts anything; the kernel clamps ".." at /.
	if err := hostRoot.checkSymlinkTargetInRoot("/x", "../../y"); err != nil {
		t.Errorf("host root: %v", err)
	}
}
