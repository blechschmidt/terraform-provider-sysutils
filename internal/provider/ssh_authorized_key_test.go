package provider

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/crypto/ssh"
)

// testSSHKey returns a deterministic ed25519 public key derived from seed.
func testSSHKey(t *testing.T, seed byte) ssh.PublicKey {
	t.Helper()
	priv := ed25519.NewKeyFromSeed([]byte(strings.Repeat(string(rune('a'+seed%26)), ed25519.SeedSize)))
	pub, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

// testSSHKeyText returns the authorized_keys form "<type> <base64>" of pub.
func testSSHKeyText(pub ssh.PublicKey) string {
	return strings.TrimSuffix(string(ssh.MarshalAuthorizedKey(pub)), "\n")
}

func TestParseSSHPublicKey(t *testing.T) {
	pub := testSSHKey(t, 0)
	text := testSSHKeyText(pub)

	for _, tc := range []struct {
		in, comment string
	}{
		{text, ""},
		{text + " alice@laptop", "alice@laptop"},
		{text + " two words\n", "two words"},
		{"  " + text + "  \n", ""},
	} {
		got, comment, err := parseSSHPublicKey(tc.in)
		if err != nil {
			t.Errorf("parseSSHPublicKey(%q): %v", tc.in, err)
			continue
		}
		if !matchSSHKey(pub)(got) || comment != tc.comment {
			t.Errorf("parseSSHPublicKey(%q) = %s, %q; want the key and %q", tc.in, testSSHKeyText(got), comment, tc.comment)
		}
	}

	signer, err := ssh.NewSignerFromKey(ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)))
	if err != nil {
		t.Fatal(err)
	}
	cert := &ssh.Certificate{Key: pub, CertType: ssh.UserCert, ValidBefore: ssh.CertTimeInfinity}
	if err := cert.SignCert(rand.Reader, signer); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ in, want string }{
		{"", "must not be empty"},
		{"  \n", "must not be empty"},
		{"ssh-ed25519", "not a valid OpenSSH public key"},
		{"ssh-ed25519 bm90IGEga2V5", "not a valid OpenSSH public key"},
		{"not a key at all", "not a valid OpenSSH public key"},
		{text + "\n" + text, "single public key on one line"},
		{text + " comment\rmore", "single public key on one line"},
		{`no-pty ` + text, "not with options (no-pty)"},
		{`from="10.0.0.1",no-pty ` + text, "not with options"},
		{testSSHKeyText(cert), "not a certificate"},
	} {
		_, _, err := parseSSHPublicKey(tc.in)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parseSSHPublicKey(%q) error = %v, want it to contain %q", tc.in, err, tc.want)
		}
	}
}

func TestValidateSSHKeyOption(t *testing.T) {
	for _, ok := range []string{
		"no-pty", "restrict", "no-port-forwarding", "pty",
		`from="10.0.0.0/8,192.168.1.*"`,
		`command="/usr/bin/rsync --server -e.LsfxC . /backup"`,
		`command="echo \"hi\""`,
		`environment="LANG=C"`,
		`permitopen="localhost:22"`,
		`from=""`,
	} {
		if err := validateSSHKeyOption(ok); err != nil {
			t.Errorf("validateSSHKeyOption(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{
		"", "no-pty,no-agent-forwarding", "-no-pty", "no pty", "from=10.0.0.1",
		`from="10.0.0.1`, `command="a"b"`, `command="a" `, `command="a\"`,
		"no-pty\n", `command="a` + "\n" + `b"`, `command="a\` + "\n" + `"`,
	} {
		if err := validateSSHKeyOption(bad); err == nil {
			t.Errorf("validateSSHKeyOption(%q) = nil, want an error", bad)
		}
	}
}

func TestValidateSSHKeyComment(t *testing.T) {
	for _, ok := range []string{"", "alice@laptop", "two words", "a  b"} {
		if err := validateSSHKeyComment(ok); err != nil {
			t.Errorf("validateSSHKeyComment(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{" a", "a ", "a\nb", "a\rb", "\t"} {
		if err := validateSSHKeyComment(bad); err == nil {
			t.Errorf("validateSSHKeyComment(%q) = nil, want an error", bad)
		}
	}
}

func TestValidateSSHKeyFingerprint(t *testing.T) {
	fp := sshKeyFingerprint(testSSHKey(t, 0))
	if err := validateSSHKeyFingerprint(fp); err != nil {
		t.Errorf("validateSSHKeyFingerprint(%q): %v", fp, err)
	}
	for _, bad := range []string{"", "SHA256:", "MD5:" + fp[7:], fp + "=", fp[:len(fp)-1], "sha256:" + fp[7:], fp[:10] + " " + fp[11:]} {
		if err := validateSSHKeyFingerprint(bad); err == nil {
			t.Errorf("validateSSHKeyFingerprint(%q) = nil, want an error", bad)
		}
	}
}

func TestRenderAuthorizedKeyRoundTrip(t *testing.T) {
	pub := testSSHKey(t, 1)
	for _, tc := range []struct {
		options []string
		comment string
		want    string
	}{
		{nil, "", testSSHKeyText(pub)},
		{nil, "alice@laptop", testSSHKeyText(pub) + " alice@laptop"},
		{
			[]string{`from="10.0.0.0/8,192.168.0.0/16"`, "no-pty", `command="echo \"a, b\""`}, "two words",
			`from="10.0.0.0/8,192.168.0.0/16",no-pty,command="echo \"a, b\"" ` + testSSHKeyText(pub) + " two words",
		},
	} {
		line := renderAuthorizedKey(pub, tc.options, tc.comment)
		if line != tc.want {
			t.Errorf("renderAuthorizedKey = %q, want %q", line, tc.want)
		}
		k, ok := parseAuthorizedKeyLine(line)
		if !ok {
			t.Fatalf("parseAuthorizedKeyLine(%q) failed", line)
		}
		if !matchSSHKey(pub)(k.pub) || k.comment != tc.comment || strings.Join(k.options, "\x00") != strings.Join(tc.options, "\x00") {
			t.Errorf("parseAuthorizedKeyLine(%q) = %q, %q; want %q, %q", line, k.options, k.comment, tc.options, tc.comment)
		}
	}
}

func TestParseAuthorizedKeyLineSkipsNonKeys(t *testing.T) {
	text := testSSHKeyText(testSSHKey(t, 0))
	for _, l := range []string{"", "   ", "# " + text, "   #" + text, "garbage", "ssh-ed25519 !!!"} {
		if _, ok := parseAuthorizedKeyLine(l); ok {
			t.Errorf("parseAuthorizedKeyLine(%q) = ok, want not a key", l)
		}
	}
}

func TestEnsureAuthorizedKey(t *testing.T) {
	a, b := testSSHKey(t, 0), testSSHKey(t, 1)
	at, bt := testSSHKeyText(a), testSSHKeyText(b)
	want := `no-pty ` + at + " managed"

	for _, tc := range []struct {
		name, in, out string
		changed       bool
	}{
		{"empty file", "", want + "\n", true},
		{"appended", "# keys\n" + bt + " bob\n", "# keys\n" + bt + " bob\n" + want + "\n", true},
		{"no trailing newline", bt, bt + "\n" + want + "\n", true},
		{"present", bt + "\n" + want + "\n", bt + "\n" + want + "\n", false},
		// The comment does not make a line a different key.
		{"other comment replaced in place", "# a\n" + at + " old\n" + bt + "\n", "# a\n" + want + "\n" + bt + "\n", true},
		{"other options replaced", `from="1.2.3.4" ` + at + " managed\n", want + "\n", true},
		{"duplicates removed", at + "\n" + bt + "\n" + want + "\n" + `command="x" ` + at + "\n", want + "\n" + bt + "\n", true},
		{"commented out copy ignored", "# " + at + "\n", "# " + at + "\n" + want + "\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := parseTextFile([]byte(tc.in))
			changed := ensureAuthorizedKey(text, matchSSHKey(a), want, -1)
			if got := string(text.bytes()); got != tc.out || changed != tc.changed {
				t.Errorf("got %q (changed %v), want %q (changed %v)", got, changed, tc.out, tc.changed)
			}
		})
	}
}

func TestRemoveAndReplaceAuthorizedKey(t *testing.T) {
	a, b, c := testSSHKey(t, 0), testSSHKey(t, 1), testSSHKey(t, 2)
	at, bt, ct := testSSHKeyText(a), testSSHKeyText(b), testSSHKeyText(c)

	text := parseTextFile([]byte(bt + "\n" + at + " x\n" + ct + "\n" + at + " y\n"))
	changed, hint := removeAuthorizedKey(text, matchSSHKey(a))
	if !changed || hint != 1 {
		t.Fatalf("removeAuthorizedKey = %v, %d; want true, 1", changed, hint)
	}
	if got := string(text.bytes()); got != bt+"\n"+ct+"\n" {
		t.Fatalf("after remove: %q", got)
	}
	// A key change puts the new key where the old one was.
	ensureAuthorizedKey(text, matchSSHKey(testSSHKey(t, 3)), "NEW", hint)
	if got := string(text.bytes()); got != bt+"\nNEW\n"+ct+"\n" {
		t.Fatalf("after replace: %q", got)
	}
	if changed, _ := removeAuthorizedKey(text, matchSSHKey(a)); changed {
		t.Error("removing an absent key changed the file")
	}
	// Lookup by fingerprint, as after import.
	if idx, first := findAuthorizedKey(text.lines, matchSSHKeyFingerprint(sshKeyFingerprint(c))); len(idx) != 1 || idx[0] == 0 || !matchSSHKey(c)(first.pub) {
		t.Errorf("findAuthorizedKey by fingerprint = %v", idx)
	}
}

// testSSHAccount returns an account of the current user with a new
// temporary home directory.
func testSSHAccount(t *testing.T) *sshAccount {
	t.Helper()
	return &sshAccount{name: "tester", uid: uint32(os.Getuid()), gid: uint32(os.Getgid()), home: t.TempDir()}
}

func TestOpenSSHDirCreates(t *testing.T) {
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)
	a := testSSHAccount(t)

	if _, err := openSSHDir(a, false); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("openSSHDir without create = %v, want fs.ErrNotExist", err)
	}
	d, err := openSSHDir(a, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	info, err := os.Lstat(filepath.Join(a.home, ".ssh"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Errorf("~/.ssh has mode %v, want a directory with 0700", info.Mode())
	}

	data, snap, err := d.readAuthorizedKeys()
	if err != nil || data != nil || snap != nil {
		t.Fatalf("readAuthorizedKeys of a missing file = %q, %v, %v", data, snap, err)
	}
	if err := d.writeAuthorizedKeys(a, []byte("x\n"), nil); err != nil {
		t.Fatal(err)
	}
	p := a.authorizedKeysPath()
	info, err = os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode() != 0o600 {
		t.Errorf("authorized_keys has mode %v, want 0600", info.Mode())
	}

	// An existing file with a wider mode is narrowed on the next write.
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	_, snap, err = d.readAuthorizedKeys()
	if err != nil {
		t.Fatal(err)
	}
	if err := d.writeAuthorizedKeys(a, []byte("y\n"), snap); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Lstat(p); info.Mode() != 0o600 {
		t.Errorf("authorized_keys has mode %v after rewrite, want 0600", info.Mode())
	}
	if got, _ := os.ReadFile(p); string(got) != "y\n" {
		t.Errorf("authorized_keys = %q", got)
	}
	// Existing ~/.ssh keeps its mode.
	if err := os.Chmod(filepath.Join(a.home, ".ssh"), 0o750); err != nil {
		t.Fatal(err)
	}
	d2, err := openSSHDir(a, true)
	if err != nil {
		t.Fatal(err)
	}
	_ = d2.Close()
	if info, _ := os.Lstat(filepath.Join(a.home, ".ssh")); info.Mode().Perm() != 0o750 {
		t.Errorf("existing ~/.ssh changed to %v", info.Mode())
	}
}

func TestOpenSSHDirRefusesSymlinks(t *testing.T) {
	a := testSSHAccount(t)
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(a.home, ".ssh")); err != nil {
		t.Fatal(err)
	}
	for _, create := range []bool{false, true} {
		_, err := openSSHDir(a, create)
		if err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Errorf("openSSHDir(create=%v) with ~/.ssh a symlink = %v, want a symlink error", create, err)
		}
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Errorf("symlink target was modified: %v", entries)
	}

	// Not a directory.
	b := testSSHAccount(t)
	if err := os.WriteFile(filepath.Join(b.home, ".ssh"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openSSHDir(b, true); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("openSSHDir with ~/.ssh a file = %v", err)
	}
}

func TestReadAuthorizedKeysRefusesSymlinkAndHardLink(t *testing.T) {
	a := testSSHAccount(t)
	d, err := openSSHDir(a, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("root:$6$hash:::\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	p := a.authorizedKeysPath()
	if err := os.Symlink(secret, p); err != nil {
		t.Fatal(err)
	}
	_, _, err = d.readAuthorizedKeys()
	if err == nil || !strings.Contains(err.Error(), "symbolic link") || !strings.Contains(err.Error(), p) {
		t.Errorf("readAuthorizedKeys through a symlink = %v, want a symlink error naming %s", err, p)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}

	// A hard link must be on the same file system.
	target := filepath.Join(a.home, "other")
	if err := os.WriteFile(target, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, p); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.readAuthorizedKeys(); err == nil || !strings.Contains(err.Error(), "hard links") {
		t.Errorf("readAuthorizedKeys of a hard link = %v, want a hard link error", err)
	}

	// A FIFO does not block.
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.readAuthorizedKeys(); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("readAuthorizedKeys of a FIFO = %v", err)
	}
}

// TestSSHDirSurvivesSwap checks that once ~/.ssh is open, swapping it for a
// symlink cannot redirect the write: the file is written into the directory
// that was opened and checked.
func TestSSHDirSurvivesSwap(t *testing.T) {
	a := testSSHAccount(t)
	d, err := openSSHDir(a, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()

	victim := t.TempDir()
	sshPath := filepath.Join(a.home, ".ssh")
	moved := filepath.Join(a.home, "moved")
	if err := os.Rename(sshPath, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, sshPath); err != nil {
		t.Fatal(err)
	}

	if err := d.writeAuthorizedKeys(a, []byte("key\n"), nil); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(victim); len(entries) != 0 {
		t.Errorf("write followed the swapped-in symlink: %v", entries)
	}
	if got, err := os.ReadFile(filepath.Join(moved, "authorized_keys")); err != nil || string(got) != "key\n" {
		t.Errorf("opened directory has %q, %v", got, err)
	}
}

func TestOpenSSHDirHome(t *testing.T) {
	for _, home := range []string{"/", "", "relative", "/home/../etc"} {
		a := &sshAccount{name: "x", home: home}
		if _, err := openSSHDir(a, true); err == nil || errors.Is(err, fs.ErrNotExist) {
			t.Errorf("openSSHDir with home %q = %v, want a validation error", home, err)
		}
	}
	a := &sshAccount{name: "x", home: filepath.Join(t.TempDir(), "missing")}
	if _, err := openSSHDir(a, true); !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("openSSHDir with a missing home = %v, want fs.ErrNotExist", err)
	}
}

func TestLookupSSHAccount(t *testing.T) {
	a, err := lookupSSHAccount("root")
	if err != nil {
		t.Fatal(err)
	}
	if a.uid != 0 || a.home == "" {
		t.Errorf("lookupSSHAccount(root) = %+v", a)
	}
	if _, err := lookupSSHAccount("no-such-user-sysutils"); !errors.Is(err, errSSHUserNotFound) {
		t.Errorf("lookupSSHAccount of a missing user = %v, want errSSHUserNotFound", err)
	}
}
