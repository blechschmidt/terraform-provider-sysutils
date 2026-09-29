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

// snapshot reads p and returns its snapshot, failing the test on error.
func snapshot(t *testing.T, p string) *fileSnapshot {
	t.Helper()
	_, snap, err := readRegularFileNoFollow(p, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// mustContain fails the test unless p is a file with exactly content.
func mustContain(t *testing.T, p, content string) {
	t.Helper()
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Fatalf("%s: content = %q, want %q", p, got, content)
	}
}

// mustHaveNoTempFiles fails the test if dir contains a leftover temporary
// file of replaceFileAtomicWith.
func mustHaveNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".sysutils-tmp-") {
			t.Errorf("temporary file %q left behind in %s", e.Name(), dir)
		}
	}
}

// setRenameHook installs hook as testHookBeforeRename for the duration of
// the test.
func setRenameHook(t *testing.T, hook func(target string)) {
	t.Helper()
	testHookBeforeRename = hook
	t.Cleanup(func() { testHookBeforeRename = nil })
}

func TestWriteManagedFile_createsFileAndParents(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a", "b", "x.conf")
	if err := writeManagedFile(p, []byte("x=1\n"), nil, 0o640); err != nil {
		t.Fatal(err)
	}
	mustContain(t, p, "x=1\n")
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Errorf("mode = %o, want 640", got)
	}
	parent, err := os.Stat(filepath.Dir(p))
	if err != nil {
		t.Fatal(err)
	}
	if !parent.IsDir() {
		t.Errorf("%s is not a directory", filepath.Dir(p))
	}
	mustHaveNoTempFiles(t, filepath.Dir(p))
}

// replaceFileAtomic itself never creates directories: a missing parent is an
// error and leaves nothing behind.
func TestReplaceFileAtomic_missingParent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "missing", "x.conf")
	err := replaceFileAtomic(p, []byte("x"), nil, 0o644)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
	if _, err := os.Lstat(filepath.Dir(p)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("parent directory was created: %v", err)
	}
}

// An existing file keeps its mode and ownership, whatever newMode is.
func TestReplaceFileAtomic_preservesModeAndOwnership(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.conf")
	mustWrite(t, p, "old\n")
	if err := os.Chmod(p, 0o604); err != nil {
		t.Fatal(err)
	}
	uid, gid := os.Getuid(), os.Getgid()
	if os.Geteuid() == 0 {
		uid, gid = 1234, 5678
		if err := os.Chown(p, uid, gid); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeManagedFile(p, []byte("new\n"), snapshot(t, p), 0o600); err != nil {
		t.Fatal(err)
	}
	mustContain(t, p, "new\n")
	info, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o604 {
		t.Errorf("mode = %o, want 604", got)
	}
	st := info.Sys().(*syscall.Stat_t)
	if int(st.Uid) != uid || int(st.Gid) != gid {
		t.Errorf("owner = %d:%d, want %d:%d", st.Uid, st.Gid, uid, gid)
	}
	mustHaveNoTempFiles(t, dir)
}

// A symlink at the target is replaced, never written through, and is refused
// outright if it appeared after the file was read.
func TestReplaceFileAtomic_symlinkedTarget(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	mustWrite(t, victim, "secret\n")
	p := filepath.Join(dir, "x.conf")

	t.Run("absent when read", func(t *testing.T) {
		mustSymlink(t, victim, p)
		t.Cleanup(func() { _ = os.Remove(p) })
		err := writeManagedFile(p, []byte("evil\n"), nil, 0o644)
		if !errors.Is(err, errFileChangedConcurrently) {
			t.Fatalf("err = %v, want errFileChangedConcurrently", err)
		}
		mustContain(t, victim, "secret\n")
		mustHaveNoTempFiles(t, dir)
	})

	t.Run("regular file when read", func(t *testing.T) {
		mustWrite(t, p, "old\n")
		snap := snapshot(t, p)
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		mustSymlink(t, victim, p)
		t.Cleanup(func() { _ = os.Remove(p) })
		err := writeManagedFile(p, []byte("evil\n"), snap, 0o644)
		if err == nil {
			t.Fatal("writing through a swapped-in symlink succeeded")
		}
		mustContain(t, victim, "secret\n")
		mustHaveNoTempFiles(t, dir)
	})

	t.Run("symlink swapped in before rename", func(t *testing.T) {
		mustWrite(t, p, "old\n")
		t.Cleanup(func() { _ = os.Remove(p) })
		setRenameHook(t, func(target string) {
			_ = os.Remove(target)
			_ = os.Symlink(victim, target)
		})
		// The race is lost after the last check; rename(2) replaces the
		// symlink itself rather than following it.
		_ = writeManagedFile(p, []byte("new\n"), snapshot(t, p), 0o644)
		mustContain(t, victim, "secret\n")
		mustHaveNoTempFiles(t, dir)
	})
}

// Under root_dir, a symlinked parent directory is resolved inside the root,
// so the managed file is written there and never at the host path the
// symlink names.
func TestWriteManagedFile_symlinkedParentUnderRootDir(t *testing.T) {
	root, rootDir := newTestRoot(t)
	outside := t.TempDir()
	mustMkdir(t, filepath.Join(rootDir, "etc"))
	// /etc/conf.d inside the root points at an absolute path that exists
	// on the host.
	mustSymlink(t, outside, filepath.Join(rootDir, "etc", "conf.d"))

	p, err := root.resolve("/etc/conf.d/x.conf")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p, rootDir+"/") {
		t.Fatalf("resolved %q outside the root %q", p, rootDir)
	}
	if err := writeManagedFile(p, []byte("x=1\n"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	mustContain(t, filepath.Join(rootDir, strings.TrimPrefix(outside, "/"), "x.conf"), "x=1\n")
	if _, err := os.Lstat(filepath.Join(outside, "x.conf")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("file written outside the root: %v", err)
	}
}

// A failed rename removes the temporary file and leaves the target as it
// was.
func TestReplaceFileAtomic_failedRenameRemovesTempFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.conf")

	t.Run("new file", func(t *testing.T) {
		setRenameHook(t, func(target string) { mustWrite(t, target, "raced\n") })
		t.Cleanup(func() { _ = os.Remove(p) })
		err := writeManagedFile(p, []byte("new\n"), nil, 0o644)
		if !errors.Is(err, errFileChangedConcurrently) {
			t.Fatalf("err = %v, want errFileChangedConcurrently", err)
		}
		mustContain(t, p, "raced\n")
		mustHaveNoTempFiles(t, dir)
	})

	t.Run("existing file", func(t *testing.T) {
		mustWrite(t, p, "old\n")
		snap := snapshot(t, p)
		// A non-empty directory cannot be renamed over.
		setRenameHook(t, func(target string) {
			_ = os.Remove(target)
			mustWrite(t, filepath.Join(target, "child"), "")
		})
		t.Cleanup(func() { _ = os.RemoveAll(p) })
		if err := writeManagedFile(p, []byte("new\n"), snap, 0o644); err == nil {
			t.Fatal("rename over a non-empty directory succeeded")
		}
		mustContain(t, filepath.Join(p, "child"), "")
		mustHaveNoTempFiles(t, dir)
	})
}

func TestRemoveManagedFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.conf")

	t.Run("unchanged file is removed", func(t *testing.T) {
		mustWrite(t, p, "x\n")
		if err := removeManagedFile(p, snapshot(t, p)); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("file still exists: %v", err)
		}
	})

	t.Run("missing file is not an error", func(t *testing.T) {
		if err := removeManagedFile(p, nil); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("modified file is kept", func(t *testing.T) {
		mustWrite(t, p, "x\n")
		snap := snapshot(t, p)
		mustWrite(t, p, "changed by someone else\n")
		if err := removeManagedFile(p, snap); !errors.Is(err, errFileChangedConcurrently) {
			t.Fatalf("err = %v, want errFileChangedConcurrently", err)
		}
		mustContain(t, p, "changed by someone else\n")
	})

	t.Run("symlink is kept", func(t *testing.T) {
		_ = os.Remove(p)
		victim := filepath.Join(dir, "victim")
		mustWrite(t, victim, "secret\n")
		mustSymlink(t, victim, p)
		if err := removeManagedFile(p, nil); !errors.Is(err, errFileChangedConcurrently) {
			t.Fatalf("err = %v, want errFileChangedConcurrently", err)
		}
		mustContain(t, victim, "secret\n")
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("symlink removed: %v", err)
		}
	})
}
