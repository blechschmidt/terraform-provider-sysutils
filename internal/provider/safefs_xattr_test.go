package provider

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// requireACLs skips the test unless setfacl/getfacl are installed and dir's
// filesystem supports ACLs.
func requireACLs(t *testing.T, dir string) {
	t.Helper()
	for _, tool := range []string{"setfacl", "getfacl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	probe := filepath.Join(dir, ".acl-probe")
	mustWrite(t, probe, "")
	defer func() { _ = os.Remove(probe) }()
	if out, err := exec.Command("setfacl", "-m", "u:65534:r", probe).CombinedOutput(); err != nil {
		t.Skipf("filesystem does not support ACLs: %v: %s", err, out)
	}
}

func runTool(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
	return string(out)
}

func editAtomically(t *testing.T, p, content string) {
	t.Helper()
	_, snap, err := readRegularFileNoFollow(p, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := replaceFileAtomic(p, []byte(content), snap, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); string(got) != content {
		t.Fatalf("content = %q, want %q", got, content)
	}
}

// Regression test: replacing a file used to drop its ACL while copying its
// mode. With an ACL, the group bits of the mode are the ACL mask, so the
// owning group, which the ACL denied any access, ended up with read-write
// access, and the named user lost theirs.
func TestReplaceFileAtomicPreservesACL(t *testing.T) {
	requireRoot(t)
	dir := t.TempDir()
	requireACLs(t, dir)
	p := filepath.Join(dir, "f")
	mustWrite(t, p, "old\n")
	if err := os.Chown(p, 0, 65534); err != nil {
		t.Fatal(err)
	}
	runTool(t, "setfacl", "--set", "u::rw-,u:65534:rw-,g::---,m::rw-,o::---", p)
	before := runTool(t, "getfacl", "-cn", p)

	editAtomically(t, p, "new\n")

	if after := runTool(t, "getfacl", "-cn", p); after != before {
		t.Errorf("ACL changed by the edit:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// Regression test: a default ACL on the directory must not add an ACL to a
// file that had none.
func TestReplaceFileAtomicDoesNotInheritDefaultACL(t *testing.T) {
	dir := t.TempDir()
	requireACLs(t, dir)
	p := filepath.Join(dir, "f")
	mustWrite(t, p, "old\n")
	runTool(t, "setfacl", "-d", "-m", "u:65534:rwx", dir)
	runTool(t, "setfacl", "-b", p)
	mustChmod(t, p, 0o600)

	editAtomically(t, p, "new\n")

	if _, err := unix.Getxattr(p, aclAccessXattr, nil); !errors.Is(err, unix.ENODATA) {
		t.Errorf("edited file has an access ACL (err = %v):\n%s", err, runTool(t, "getfacl", "-cn", p))
	}
	checkMode(t, p, 0o600)
}

func TestReplaceFileAtomicXattrs(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	mustWrite(t, p, "old\n")
	if err := unix.Setxattr(p, "user.origin", []byte("kept"), 0); err != nil {
		t.Skipf("user extended attributes not supported: %v", err)
	}
	// A valid VFS_CAP_REVISION_2 blob granting CAP_NET_BIND_SERVICE.
	capBlob := make([]byte, 20)
	binary.LittleEndian.PutUint32(capBlob[0:], 0x02000000)
	binary.LittleEndian.PutUint32(capBlob[4:], 1<<unix.CAP_NET_BIND_SERVICE)
	haveCap := unix.Setxattr(p, "security.capability", capBlob, 0) == nil

	editAtomically(t, p, "new\n")

	buf := make([]byte, 64)
	if n, err := unix.Getxattr(p, "user.origin", buf); err != nil || !bytes.Equal(buf[:n], []byte("kept")) {
		t.Errorf("user.origin = %q, %v; want %q", buf[:max(n, 0)], err, "kept")
	}
	if haveCap {
		// File capabilities must not survive a content change.
		if _, err := unix.Getxattr(p, "security.capability", buf); !errors.Is(err, unix.ENODATA) {
			t.Errorf("security.capability carried over to the edited file (err = %v)", err)
		}
	}
}

// Regression test: a permission change made after the file was read, such as
// an administrator tightening the mode, used to be reverted silently by the
// replacement, which copied the mode from the stale snapshot.
func TestReplaceFileAtomicDetectsConcurrentChmod(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	mustWrite(t, p, "old\n")
	mustChmod(t, p, 0o644)
	_, snap, err := readRegularFileNoFollow(p, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	mustChmod(t, p, 0o600)
	if err := replaceFileAtomic(p, []byte("new\n"), snap, 0o644); !errors.Is(err, errFileChangedConcurrently) {
		t.Errorf("err = %v, want errFileChangedConcurrently", err)
	}
	checkMode(t, p, 0o600)
	if got, _ := os.ReadFile(p); string(got) != "old\n" {
		t.Errorf("content = %q, want the file untouched", got)
	}
}

func checkMode(t *testing.T, p string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("mode of %s = %v, want %v", p, got, want)
	}
}
