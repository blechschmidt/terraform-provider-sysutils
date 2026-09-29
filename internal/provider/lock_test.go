package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// holdFlock takes the flock of the lock file name the way another provider
// process would, through a separate open file description, and returns the
// function that releases it.
func holdFlock(t *testing.T, name string) func() {
	t.Helper()
	dir, err := lockDir()
	if err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(filepath.Join(dir, name), unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(func() { _ = unix.Close(fd) }) }
	t.Cleanup(release)
	return release
}

func registrySize() int {
	lockRegistry.Lock()
	defer lockRegistry.Unlock()
	return len(lockRegistry.locks)
}

func TestFileLockKey(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	// A symlink at the file itself is not followed: the resources refuse
	// to edit through it rather than editing its target.
	if err := os.Symlink("other", filepath.Join(real, "file-link")); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(real, "file")
	for _, p := range []string{
		want,
		filepath.Join(dir, "link", "file"),
		filepath.Join(dir, "link", ".", "file"),
		filepath.Join(real, "..", "real", "file"),
		dir + "/real//file",
	} {
		got, err := fileLockKey(p)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("fileLockKey(%q) = %q, want %q", p, got, want)
		}
	}
	if got, _ := fileLockKey(filepath.Join(real, "file-link")); got != filepath.Join(real, "file-link") {
		t.Errorf("fileLockKey followed the symlink at the file: %q", got)
	}
	// Missing directories are kept as they are.
	missing := filepath.Join(dir, "missing", "dir", "file")
	if got, _ := fileLockKey(missing); got != missing {
		t.Errorf("fileLockKey(%q) = %q", missing, got)
	}
	// Relative paths are made absolute.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	wd, _ = filepath.EvalSymlinks(wd)
	if got, _ := fileLockKey("x/../y"); got != filepath.Join(wd, "y") {
		t.Errorf("fileLockKey(x/../y) = %q, want %q", got, filepath.Join(wd, "y"))
	}
}

// TestLockFileForEdit_serialisesEdits runs read-modify-write cycles on one
// file from many goroutines, with the race window widened, and checks that
// none of the updates is lost or fails.
func TestLockFileForEdit_serialisesEdits(t *testing.T) {
	dir := t.TempDir()
	// Two spellings of the same file must share a lock.
	if err := os.Symlink(".", filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "counter")
	mustWrite(t, p, "")
	testHookAfterRead = func(string) { time.Sleep(2 * time.Millisecond) }
	t.Cleanup(func() { testHookAfterRead = nil })

	const workers = 20
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			target := p
			if i%2 == 1 {
				target = filepath.Join(dir, "alias", "counter")
			}
			unlock, err := lockFileForEdit(target)
			if err != nil {
				errs <- err
				return
			}
			defer unlock()
			data, snap, err := readRegularFileNoFollow(target, 1<<20)
			if err != nil {
				errs <- err
				return
			}
			errs <- replaceFileAtomic(target, append(data, "line "+strconv.Itoa(i)+"\n"...), snap, 0o644)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "\n"); n != workers {
		t.Errorf("file has %d lines, want %d:\n%s", n, workers, data)
	}
	if n := registrySize(); n != 0 {
		t.Errorf("%d locks left in the registry", n)
	}
}

// TestLockFileForEdit_otherProcess checks that the lock also waits for
// another process that holds the lock file.
func TestLockFileForEdit_otherProcess(t *testing.T) {
	p := filepath.Join(t.TempDir(), "file")
	key, err := fileLockKey(p)
	if err != nil {
		t.Fatal(err)
	}
	release := holdFlock(t, lockFileName(key))

	var acquired atomic.Bool
	done := make(chan error, 1)
	go func() {
		unlock, err := lockFileForEdit(p)
		if err == nil {
			acquired.Store(true)
			unlock()
		}
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	if acquired.Load() {
		t.Fatal("lock acquired while another process held it")
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("lock not acquired after the other process released it")
	}
}

func TestLockFileForEdit_timeout(t *testing.T) {
	p := filepath.Join(t.TempDir(), "file")
	key, err := fileLockKey(p)
	if err != nil {
		t.Fatal(err)
	}
	holdFlock(t, lockFileName(key))
	old := fileLockTimeout
	fileLockTimeout = 100 * time.Millisecond
	t.Cleanup(func() { fileLockTimeout = old })

	_, err = lockFileForEdit(p)
	if err == nil || !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), p) {
		t.Fatalf("err = %v, want a timeout naming %s", err, p)
	}
	if n := registrySize(); n != 0 {
		t.Errorf("%d locks left in the registry", n)
	}
}

func TestLockFileForEdit_differentFilesDoNotBlock(t *testing.T) {
	dir := t.TempDir()
	unlockA, err := lockFileForEdit(filepath.Join(dir, "a"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlockA()
	done := make(chan error, 1)
	go func() {
		unlockB, err := lockFileForEdit(filepath.Join(dir, "b"))
		if err == nil {
			unlockB()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("locking another file in the same directory blocked")
	}
}

func TestLockFileForEdit_unlockTwice(t *testing.T) {
	p := filepath.Join(t.TempDir(), "file")
	unlock, err := lockFileForEdit(p)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	unlock() // Must not release a lock taken by someone else in between.
	unlock2, err := lockFileForEdit(p)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock2()
	if n := registrySize(); n != 1 {
		t.Errorf("registry has %d locks, want 1", n)
	}
}

func TestLockPackageManager(t *testing.T) {
	unlock, err := lockPackageManager(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// A second holder in this process waits, and gives up when its
	// context is done.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := lockPackageManager(ctx); err == nil {
		t.Fatal("package manager locked twice")
	} else if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %v, want a timeout", err)
	}
	unlock()

	// Another process holding the lock file blocks it too.
	release := holdFlock(t, packageManagerLockName)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	if _, err := lockPackageManager(ctx2); err == nil {
		t.Fatal("package manager locked while another process held it")
	}
	release()
	unlock, err = lockPackageManager(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	unlock()

	// Cancellation is reported as such.
	unlock, err = lockPackageManager(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx3, cancel3 := context.WithCancel(context.Background())
	cancel3()
	if _, err := lockPackageManager(ctx3); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestLockDirIsPrivate(t *testing.T) {
	dir, err := lockDir()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Errorf("%s: mode %v, want a directory with mode 0700", dir, info.Mode())
	}
	if os.Geteuid() == 0 && dir != "/run/"+lockDirName {
		t.Errorf("lock directory of root is %s", dir)
	}
}

func TestEnsurePrivateDir(t *testing.T) {
	base := t.TempDir()

	d := filepath.Join(base, "new")
	if err := ensurePrivateDir(d); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDir(d); err != nil {
		t.Fatalf("existing directory: %v", err)
	}

	writable := filepath.Join(base, "writable")
	if err := os.Mkdir(writable, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(writable, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDir(writable); err == nil || !strings.Contains(err.Error(), "writable by other users") {
		t.Errorf("world-writable directory: err = %v", err)
	}

	link := filepath.Join(base, "link")
	if err := os.Symlink(d, link); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDir(link); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("symlink: err = %v", err)
	}

	file := filepath.Join(base, "file")
	mustWrite(t, file, "")
	if err := ensurePrivateDir(file); err == nil {
		t.Error("regular file accepted")
	}

	if os.Geteuid() == 0 {
		foreign := filepath.Join(base, "foreign")
		if err := os.Mkdir(foreign, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(foreign, 65534, 65534); err != nil {
			t.Fatal(err)
		}
		if err := ensurePrivateDir(foreign); err == nil || !strings.Contains(err.Error(), "owned by user ID 65534") {
			t.Errorf("directory of another user: err = %v", err)
		}
	}
}
