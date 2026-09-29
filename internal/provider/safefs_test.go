package provider

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestValidateRecursivelyRemovable(t *testing.T) {
	cases := []struct {
		path    string
		wantErr string
	}{
		{"/srv/app", ""},
		{"/var/lib/app", ""},
		{"/data", ""},
		{"", "must be absolute"},
		{"relative/dir", "must be absolute"},
		{"/", "filesystem root"},
		{"//", "canonical form"},
		{"/srv/app/", "canonical form"},
		{"/srv/../etc", "canonical form"},
		{"/etc", "protected system directory"},
		{"/usr", "protected system directory"},
		{"/usr/lib", "protected system directory"},
		{"/var/lib", "protected system directory"},
		{"/home", "protected system directory"},
		{"/tmp", "protected system directory"},
	}
	for _, tc := range cases {
		err := validateRecursivelyRemovable(tc.path)
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("validateRecursivelyRemovable(%q) = %v, want nil", tc.path, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("validateRecursivelyRemovable(%q) = %v, want error containing %q", tc.path, err, tc.wantErr)
		}
	}
}

// mustWrite creates a file with the given content, failing the test on error.
func mustWrite(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// mustExist fails the test unless p exists (without following symlinks).
func mustExist(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Lstat(p); err != nil {
		t.Errorf("expected %q to survive: %v", p, err)
	}
}

func TestRemoveAllNoFollow_removesTreeButNotSymlinkTargets(t *testing.T) {
	root := t.TempDir()
	victim := filepath.Join(root, "victim")
	mustWrite(t, filepath.Join(victim, "precious.txt"), "keep")

	dir := filepath.Join(root, "managed")
	mustWrite(t, filepath.Join(dir, "a.txt"), "a")
	mustWrite(t, filepath.Join(dir, "sub", "deeper", "b.txt"), "b")
	if err := os.Mkdir(filepath.Join(dir, "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Symlinks inside the tree must be unlinked, never followed.
	if err := os.Symlink(victim, filepath.Join(dir, "sub", "to-victim-dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(victim, "precious.txt"), filepath.Join(dir, "to-victim-file")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := removeAllNoFollow(dir); err != nil {
		t.Fatalf("removeAllNoFollow: %v", err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("directory still exists after removal: %v", err)
	}
	mustExist(t, filepath.Join(victim, "precious.txt"))
}

func TestRemoveAllNoFollow_missingIsNotAnError(t *testing.T) {
	root := t.TempDir()
	if err := removeAllNoFollow(filepath.Join(root, "missing")); err != nil {
		t.Errorf("missing directory: %v", err)
	}
	if err := removeAllNoFollow(filepath.Join(root, "missing-parent", "child")); err != nil {
		t.Errorf("missing parent: %v", err)
	}
}

func TestRemoveAllNoFollow_refusesFinalSymlink(t *testing.T) {
	root := t.TempDir()
	victim := filepath.Join(root, "victim")
	mustWrite(t, filepath.Join(victim, "precious.txt"), "keep")
	link := filepath.Join(root, "link")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}

	err := removeAllNoFollow(link)
	if !errors.Is(err, errNotRecursivelyRemovable) || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("removeAllNoFollow(symlink) = %v, want symlink refusal", err)
	}
	mustExist(t, link)
	mustExist(t, filepath.Join(victim, "precious.txt"))
}

func TestRemoveAllNoFollow_refusesIntermediateSymlink(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "real", "data", "precious.txt"), "keep")
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	err := removeAllNoFollow(filepath.Join(root, "link", "data"))
	if !errors.Is(err, errNotRecursivelyRemovable) || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("removeAllNoFollow(through symlink) = %v, want symlink refusal", err)
	}
	if !strings.Contains(err.Error(), filepath.Join(root, "link")) {
		t.Errorf("error %q does not name the offending component", err)
	}
	mustExist(t, filepath.Join(root, "real", "data", "precious.txt"))
}

func TestRemoveAllNoFollow_refusesNonDirectory(t *testing.T) {
	p := filepath.Join(t.TempDir(), "file")
	mustWrite(t, p, "keep")
	err := removeAllNoFollow(p)
	if !errors.Is(err, errNotRecursivelyRemovable) || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("removeAllNoFollow(file) = %v, want not-a-directory refusal", err)
	}
	mustExist(t, p)
}

func TestRemoveAllNoFollow_refusesDangerousPaths(t *testing.T) {
	// None of these may touch the filesystem; the guards run first.
	for _, p := range []string{"", "/", "//", "relative", "/etc", "/usr", "/var/lib", "/home"} {
		err := removeAllNoFollow(p)
		if !errors.Is(err, errNotRecursivelyRemovable) {
			t.Errorf("removeAllNoFollow(%q) = %v, want refusal", p, err)
		}
	}
}

func TestRemoveAllNoFollow_doesNotCrossMountPoints(t *testing.T) {
	requireRoot(t)

	dir := filepath.Join(t.TempDir(), "managed")
	mnt := filepath.Join(dir, "mnt")
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mount("tmpfs", mnt, "tmpfs", 0, "size=1m"); err != nil {
		t.Skipf("cannot mount tmpfs (unprivileged container?): %v", err)
	}
	t.Cleanup(func() { _ = syscall.Unmount(mnt, syscall.MNT_DETACH) })
	mustWrite(t, filepath.Join(mnt, "precious.txt"), "keep")

	err := removeAllNoFollow(dir)
	if !errors.Is(err, errNotRecursivelyRemovable) || !strings.Contains(err.Error(), "different filesystem") {
		t.Fatalf("removeAllNoFollow(with mount) = %v, want mount-point refusal", err)
	}
	mustExist(t, filepath.Join(mnt, "precious.txt"))
}

func TestOpenNoFollow_refusesSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	for name, flag := range map[string]int{
		"directory": os.O_RDONLY | syscall.O_DIRECTORY,
		"read":      os.O_RDONLY,
		"write":     os.O_WRONLY | os.O_CREATE,
	} {
		f, err := openNoFollow(link, flag, 0o600)
		if err == nil {
			_ = f.Close()
			t.Errorf("%s: openNoFollow(symlink) succeeded", name)
			continue
		}
		if !strings.Contains(err.Error(), "symbolic link") {
			t.Errorf("%s: openNoFollow(symlink) = %v, want symlink refusal", name, err)
		}
	}
}

func TestSetOwnershipAndMode_keepsSetuidAcrossChown(t *testing.T) {
	requireRoot(t)

	p := filepath.Join(t.TempDir(), "bin")
	mustWrite(t, p, "#!/bin/sh\n")
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	// chown clears setuid/setgid, so the mode must be applied afterwards.
	mode := fs.FileMode(0o755) | fs.ModeSetuid | fs.ModeSetgid
	if err := setOwnershipAndMode(f, "65534", "65534", mode); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := formatMode(info.Mode()); got != "6755" {
		t.Errorf("mode = %s, want 6755", got)
	}
	st := info.Sys().(*syscall.Stat_t)
	if st.Uid != 65534 || st.Gid != 65534 {
		t.Errorf("ownership = %d:%d, want 65534:65534", st.Uid, st.Gid)
	}
}
