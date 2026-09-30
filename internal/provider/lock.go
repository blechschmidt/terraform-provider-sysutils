package provider

// Locks that serialise changes to shared state.
//
// Terraform applies independent resources in parallel (10 at a time by
// default), and several resource types edit the same file: many
// sysutils_file_line or sysutils_ini_value resources commonly target one
// configuration file, and /etc/hosts, /etc/fstab, authorized_keys,
// /etc/apk/repositories and friends are shared the same way. Each edit is a
// read-modify-write cycle; without serialisation two cycles that overlap
// either lose one of the updates or, since replaceFileAtomic detects the
// concurrent change, fail. Package managers are worse off: apt, dnf and apk
// hold an exclusive lock on their database and fail outright rather than
// wait for each other.
//
// Two kinds of locks are therefore provided:
//
//   - lockFileForEdit locks one file for a read-modify-write cycle. Locks are
//     keyed by the file's cleaned absolute host path (after root_dir has been
//     applied by the caller), with symlinks in its parent directories
//     resolved, so that different spellings of the same file share a lock.
//   - lockPackageManager is a single lock for all package manager commands
//     and for changes to the package manager's repository configuration.
//   - lockFirewall is a single lock for changes to the firewall rules of
//     sysutils_firewall_rule, each of which lists the rules and then adds
//     or deletes some.
//
// Each lock is an in-process mutex, which serialises the goroutines of this
// provider process, combined with flock(2) on a lock file in a private
// directory (see lockDir), which serialises this process with other
// processes of this provider, such as a second Terraform run on the same
// host. The lock files are kept out of the directories of the managed files
// on purpose: tools such as cron, apt or run-parts read every file in their
// configuration directories.
//
// Lock order: the package-manager lock is taken before any file lock, never
// the other way round, and no code holds two file locks at once. The
// firewall lock is never held together with another lock.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// lockDirName is the name of the directory holding the lock files.
	lockDirName = "terraform-provider-sysutils"
	// packageManagerLockName is the lock file of lockPackageManager.
	packageManagerLockName = "package-manager.lock"
	// firewallLockName is the lock file of lockFirewall.
	firewallLockName = "firewall.lock"
)

// fileLockTimeout bounds how long lockFileForEdit waits for another process
// to release a file lock. A single edit takes milliseconds, so running into
// it means the other process hangs. A variable so tests can shorten it.
var fileLockTimeout = 2 * time.Minute

// pathLock is an entry of the in-process lock registry.
type pathLock struct {
	// sem is a one-slot semaphore rather than a sync.Mutex, so that waiting
	// for it can be cancelled.
	sem chan struct{}
	// refs counts the holders and waiters; the entry is dropped from the
	// registry when it reaches zero.
	refs int
}

// lockRegistry maps lock keys to their in-process locks.
var lockRegistry = struct {
	sync.Mutex
	locks map[string]*pathLock
}{locks: map[string]*pathLock{}}

// Test hooks. disableEditLocks turns lockFileForEdit into a no-op, and
// testHookAfterRead, if set, is called by readRegularFileNoFollow after it
// read a file, so that tests can widen the window between the read and the
// write of an edit and show that the lock closes it.
var (
	disableEditLocks  atomic.Bool
	testHookAfterRead func(p string)
)

// lockFileForEdit locks the file p for a read-modify-write cycle and returns
// the function that unlocks it. p is the host path of the file. Callers take
// the lock before reading the file and release it after writing it, and
// must not take another file lock while holding it.
func lockFileForEdit(p string) (unlock func(), err error) {
	if disableEditLocks.Load() {
		return func() {}, nil
	}
	key, err := fileLockKey(p)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), fileLockTimeout)
	defer cancel()
	unlock, err = acquireLock(ctx, "file:"+key, lockFileName(key))
	if err != nil {
		return nil, fmt.Errorf("locking %q for editing: %w", p, err)
	}
	return unlock, nil
}

// lockPackageManager takes the lock that serialises package manager
// commands and changes to package repositories, waiting until ctx is done.
func lockPackageManager(ctx context.Context) (unlock func(), err error) {
	unlock, err = acquireLock(ctx, "package-manager", packageManagerLockName)
	if err != nil {
		return nil, fmt.Errorf("waiting for the package manager lock: %w", err)
	}
	return unlock, nil
}

// lockFirewall takes the lock that serialises changes to firewall rules,
// waiting until ctx is done.
func lockFirewall(ctx context.Context) (unlock func(), err error) {
	unlock, err = acquireLock(ctx, "firewall", firewallLockName)
	if err != nil {
		return nil, fmt.Errorf("waiting for the firewall lock: %w", err)
	}
	return unlock, nil
}

// fileLockKey returns the registry key of the file p: its cleaned absolute
// path, with symlinks in the parent directories resolved if they exist. The
// last component is kept as it is, since the resources never follow a
// symlink there.
func fileLockKey(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	dir, base := filepath.Split(abs)
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	return filepath.Join(dir, base), nil
}

// lockFileName returns the name of the lock file for the registry key key.
// Paths are hashed, since they can be longer than a file name may be.
func lockFileName(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "file-" + hex.EncodeToString(sum[:16]) + ".lock"
}

// acquireLock takes the in-process lock key, then flock(2) on the lock file
// name in lockDir, and returns the function that releases both.
func acquireLock(ctx context.Context, key, name string) (func(), error) {
	l := registerLock(key)
	select {
	case l.sem <- struct{}{}:
	case <-ctx.Done():
		unregisterLock(key)
		return nil, lockWaitError(ctx)
	}
	release := func() {
		<-l.sem
		unregisterLock(key)
	}
	fd, err := flockFile(ctx, name)
	if err != nil {
		release()
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			// Closing the descriptor releases the flock.
			_ = unix.Close(fd)
			release()
		})
	}, nil
}

func registerLock(key string) *pathLock {
	lockRegistry.Lock()
	defer lockRegistry.Unlock()
	l := lockRegistry.locks[key]
	if l == nil {
		l = &pathLock{sem: make(chan struct{}, 1)}
		lockRegistry.locks[key] = l
	}
	l.refs++
	return l
}

func unregisterLock(key string) {
	lockRegistry.Lock()
	defer lockRegistry.Unlock()
	l := lockRegistry.locks[key]
	l.refs--
	if l.refs == 0 {
		delete(lockRegistry.locks, key)
	}
}

func lockWaitError(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errors.New("timed out: another process holds the lock")
	}
	return ctx.Err()
}

// flockFile opens the lock file name in lockDir and takes an exclusive
// flock on it, polling until ctx is done. It returns the open descriptor,
// which holds the lock until it is closed.
func flockFile(ctx context.Context, name string) (int, error) {
	dir, err := lockDir()
	if err != nil {
		return -1, err
	}
	p := filepath.Join(dir, name)
	fd, err := unix.Open(p, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return -1, fmt.Errorf("opening lock file %s: %w", p, err)
	}
	delay := time.Millisecond
	for {
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		switch {
		case err == nil:
			return fd, nil
		case errors.Is(err, unix.EINTR):
			continue
		case !errors.Is(err, unix.EWOULDBLOCK):
			_ = unix.Close(fd)
			return -1, fmt.Errorf("locking %s: %w", p, err)
		}
		t := time.NewTimer(delay)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			_ = unix.Close(fd)
			return -1, fmt.Errorf("%w (lock file %s)", lockWaitError(ctx), p)
		}
		delay = min(2*delay, 100*time.Millisecond)
	}
}

var lockDirOnce struct {
	sync.Once
	dir string
	err error
}

// lockDir returns the directory for lock files, creating it if needed. It
// is private to the user the provider runs as: another user who could
// create or hold the lock files there could otherwise block the provider.
// root uses /run/terraform-provider-sysutils, since /run is writable only
// by root; other users use $XDG_RUNTIME_DIR, which belongs to them. If
// neither is usable, a directory in os.TempDir() that includes the user ID
// in its name is used, and refused unless it is private to the user.
func lockDir() (string, error) {
	lockDirOnce.Do(func() {
		uid := os.Geteuid()
		var candidates []string
		if uid == 0 {
			candidates = append(candidates, filepath.Join("/run", lockDirName))
		} else if xdg := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(xdg) {
			candidates = append(candidates, filepath.Join(xdg, lockDirName))
		}
		candidates = append(candidates, filepath.Join(os.TempDir(), lockDirName+"-"+strconv.Itoa(uid)))
		var errs []error
		for _, d := range candidates {
			err := ensurePrivateDir(d)
			if err == nil {
				lockDirOnce.dir = d
				return
			}
			errs = append(errs, err)
		}
		lockDirOnce.err = fmt.Errorf("no usable directory for lock files: %w", errors.Join(errs...))
	})
	return lockDirOnce.dir, lockDirOnce.err
}

// ensurePrivateDir creates the directory d with mode 0700 if it does not
// exist, and checks that it is a real directory owned by the current user
// that nobody else can write to.
func ensurePrivateDir(d string) error {
	if err := os.Mkdir(d, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	var st unix.Stat_t
	if err := unix.Lstat(d, &st); err != nil {
		return &os.PathError{Op: "lstat", Path: d, Err: err}
	}
	switch {
	case st.Mode&unix.S_IFMT != unix.S_IFDIR:
		return fmt.Errorf("%s is not a directory", d)
	case int(st.Uid) != os.Geteuid():
		return fmt.Errorf("%s is owned by user ID %d, not by the current user", d, st.Uid)
	case st.Mode&0o022 != 0:
		return fmt.Errorf("%s is writable by other users", d)
	}
	return nil
}
