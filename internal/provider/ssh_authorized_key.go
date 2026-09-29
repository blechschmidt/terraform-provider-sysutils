package provider

// Logic behind sysutils_ssh_authorized_key: parsing and rendering
// authorized_keys lines, editing the file, and reaching it safely.
//
// Security model. The provider usually runs as root, but ~/.ssh and
// everything in it belong to the user, who may replace any entry at any
// time, for example with a symlink to /root/.ssh/authorized_keys or with a
// hard link to /etc/shadow. Path-based operations would follow such a
// replacement even if it was checked a moment earlier. Therefore:
//
//   - The home directory path from the user database is resolved normally:
//     its ancestors (such as /home) are root's, not the user's.
//   - Inside it, ~/.ssh is opened with O_NOFOLLOW relative to the home
//     directory's descriptor, and refused if it is a symlink or not a
//     directory.
//   - authorized_keys is then only ever reached through that descriptor,
//     as /proc/self/fd/<n>/authorized_keys. The kernel resolves the
//     /proc/self/fd/<n> magic link to the opened directory itself, so the
//     read, the temporary file and the rename all happen in the directory
//     that was checked, whatever the user renames in the meantime. This
//     lets the regular safefs helpers (O_NOFOLLOW reads, atomic replacement
//     with concurrent-modification checks) be used unchanged.
//   - A file with more than one hard link is refused, so that a link to a
//     file the user cannot read never has its contents copied into the
//     user's authorized_keys.

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

const (
	// maxAuthorizedKeysSize bounds how much of an authorized_keys file is
	// read. The file belongs to the user, who could otherwise make the
	// provider read an arbitrarily large file into memory.
	maxAuthorizedKeysSize = 8 << 20

	sshDirName             = ".ssh"
	authorizedKeysName     = "authorized_keys"
	sshDirMode             = 0o700
	authorizedKeysFileMode = 0o600

	// fingerprintPrefix starts every fingerprint returned by
	// ssh.FingerprintSHA256.
	fingerprintPrefix = "SHA256:"
)

// parseSSHPublicKey parses the value of the key attribute: a single OpenSSH
// public key "<type> <base64>", optionally followed by a comment, as found
// in a .pub file. Surrounding white space, such as the newline at the end of
// a .pub file, is ignored. Options are refused; they belong in options.
func parseSSHPublicKey(s string) (pub ssh.PublicKey, comment string, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, "", errors.New("key must not be empty")
	}
	if strings.ContainsAny(s, "\r\n") {
		return nil, "", errors.New("key must be a single public key on one line")
	}
	pub, comment, options, _, err := ssh.ParseAuthorizedKey([]byte(s))
	if err != nil {
		return nil, "", fmt.Errorf("key is not a valid OpenSSH public key such as \"ssh-ed25519 AAAA... comment\": %w", err)
	}
	if len(options) > 0 {
		return nil, "", fmt.Errorf("key must start with the key type, not with options (%s); set them in options instead", strings.Join(options, ","))
	}
	if _, ok := pub.(*ssh.Certificate); ok {
		return nil, "", errors.New("key must be a public key, not a certificate; to trust certificates, add the CA's public key with the cert-authority option")
	}
	return pub, comment, nil
}

// validateSSHPublicKey reports why s is not a usable value of key.
func validateSSHPublicKey(s string) error {
	_, _, err := parseSSHPublicKey(s)
	return err
}

// sshKeyOptionPattern matches one authorized_keys option: a name, optionally
// followed by "=" and a double-quoted value in which a double quote is
// escaped with a backslash, such as no-pty or from="10.0.0.0/8".
var sshKeyOptionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*(="(?:[^"\\\r\n]|\\[^\r\n])*")?$`)

// validateSSHKeyOption reports why s is not a single authorized_keys option.
func validateSSHKeyOption(s string) error {
	if !sshKeyOptionPattern.MatchString(s) {
		return fmt.Errorf("option %q must be a name such as no-pty, or a name, \"=\" and a double-quoted value such as from=\"10.0.0.0/8\", in which a double quote is escaped as \\\"", s)
	}
	return nil
}

// validateSSHKeyComment reports why s cannot be written as the comment of an
// authorized_keys line and read back unchanged.
func validateSSHKeyComment(s string) error {
	if strings.ContainsAny(s, "\r\n") {
		return errors.New("comment must be a single line")
	}
	if strings.TrimSpace(s) != s {
		return errors.New("comment must not start or end with white space")
	}
	return nil
}

// sshKeyFingerprint returns the SHA-256 fingerprint of pub, as printed by
// ssh-keygen -l: "SHA256:" followed by unpadded base64.
func sshKeyFingerprint(pub ssh.PublicKey) string {
	return ssh.FingerprintSHA256(pub)
}

// validateSSHKeyFingerprint reports why s is not a SHA-256 fingerprint.
func validateSSHKeyFingerprint(s string) error {
	rest, ok := strings.CutPrefix(s, fingerprintPrefix)
	// 32 bytes are 43 characters of unpadded base64.
	if !ok || len(rest) != 43 || strings.ContainsAny(rest, "=\r\n \t") {
		return fmt.Errorf("fingerprint %q must be a SHA-256 fingerprint as printed by ssh-keygen -l, such as \"SHA256:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU\"", s)
	}
	return nil
}

// renderAuthorizedKey returns the authorized_keys line for pub with the
// given options and comment.
func renderAuthorizedKey(pub ssh.PublicKey, options []string, comment string) string {
	var b strings.Builder
	if len(options) > 0 {
		b.WriteString(strings.Join(options, ","))
		b.WriteByte(' ')
	}
	b.Write(bytes.TrimSuffix(ssh.MarshalAuthorizedKey(pub), []byte("\n")))
	if comment != "" {
		b.WriteByte(' ')
		b.WriteString(comment)
	}
	return b.String()
}

// authorizedKeyLine is a line of an authorized_keys file that holds a key.
type authorizedKeyLine struct {
	pub     ssh.PublicKey
	options []string
	comment string
}

// parseAuthorizedKeyLine parses one line of an authorized_keys file. It
// reports ok=false for blank lines, comments and lines sshd would not
// accept either.
func parseAuthorizedKeyLine(line string) (authorizedKeyLine, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || trimmed[0] == '#' {
		return authorizedKeyLine{}, false
	}
	pub, comment, options, _, err := ssh.ParseAuthorizedKey([]byte(trimmed))
	if err != nil {
		return authorizedKeyLine{}, false
	}
	return authorizedKeyLine{pub: pub, options: options, comment: comment}, true
}

// sshKeyMatcher decides whether a line holds the managed key.
type sshKeyMatcher func(ssh.PublicKey) bool

// matchSSHKey matches lines with the same key type and key data as pub,
// whatever their options and comment.
func matchSSHKey(pub ssh.PublicKey) sshKeyMatcher {
	want := pub.Marshal()
	return func(p ssh.PublicKey) bool { return bytes.Equal(p.Marshal(), want) }
}

// matchSSHKeyFingerprint matches lines whose key has the SHA-256
// fingerprint fp. Used on import, when only the fingerprint is known.
func matchSSHKeyFingerprint(fp string) sshKeyMatcher {
	return func(p ssh.PublicKey) bool { return sshKeyFingerprint(p) == fp }
}

// findAuthorizedKey returns the indices of the lines holding the matched
// key, and the parsed first one.
func findAuthorizedKey(lines []string, match sshKeyMatcher) (indices []int, first authorizedKeyLine) {
	for i, l := range lines {
		k, ok := parseAuthorizedKeyLine(l)
		if !ok || !match(k.pub) {
			continue
		}
		if indices == nil {
			first = k
		}
		indices = append(indices, i)
	}
	return indices, first
}

// removeAuthorizedKey deletes every line holding the matched key from t. It
// reports whether t changed and the index of the first removed line.
func removeAuthorizedKey(t *textFile, match sshKeyMatcher) (changed bool, at int) {
	indices, _ := findAuthorizedKey(t.lines, match)
	if len(indices) == 0 {
		return false, -1
	}
	for i := len(indices) - 1; i >= 0; i-- {
		t.replace(indices[i], indices[i]+1, nil)
	}
	return true, indices[0]
}

// ensureAuthorizedKey makes line the only line holding the matched key in
// t and reports whether t changed. An existing line is replaced in place
// and further lines with the same key are removed: sshd uses the first
// line whose key and options allow the login, so a duplicate without, say,
// a from= restriction would defeat the restriction on the managed line. A
// missing key is inserted at hint if hint >= 0, and appended otherwise.
func ensureAuthorizedKey(t *textFile, match sshKeyMatcher, line string, hint int) bool {
	indices, _ := findAuthorizedKey(t.lines, match)
	if len(indices) == 0 {
		idx := hint
		if idx < 0 {
			idx = len(t.lines)
		}
		t.insert(idx, []string{line})
		return true
	}
	changed := false
	for i := len(indices) - 1; i >= 1; i-- {
		t.replace(indices[i], indices[i]+1, nil)
		changed = true
	}
	if t.lines[indices[0]] != line {
		t.lines[indices[0]] = line
		changed = true
	}
	return changed
}

// sshAccount is the user whose authorized_keys file is managed.
type sshAccount struct {
	name     string
	uid, gid uint32
	home     string
}

// errSSHUserNotFound is wrapped by lookup errors for users that do not
// exist.
var errSSHUserNotFound = errors.New("user does not exist")

// sshUserNotFound returns the error for the missing user name.
func sshUserNotFound(name string) error {
	return &rewordedError{msg: fmt.Sprintf("user %q does not exist", name), err: errSSHUserNotFound}
}

// sshKeyConfig overrides how sysutils_ssh_authorized_key finds users. It is
// nil in production and set by unit tests to map user names to their own
// account and a temporary home directory.
type sshKeyConfig struct {
	lookup func(name string) (*sshAccount, error)
}

func (c *sshKeyConfig) lookupAccount(name string) (*sshAccount, error) {
	if c != nil && c.lookup != nil {
		return c.lookup(name)
	}
	return lookupSSHAccount(name)
}

// lookupSSHAccount looks name up in the user database.
func lookupSSHAccount(name string) (*sshAccount, error) {
	u, err := user.Lookup(name)
	if err != nil {
		var unknown user.UnknownUserError
		if errors.As(err, &unknown) {
			return nil, sshUserNotFound(name)
		}
		return nil, fmt.Errorf("looking up user %q: %w", name, err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("user %q has non-numeric uid %q", name, u.Uid)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("user %q has non-numeric gid %q", name, u.Gid)
	}
	return &sshAccount{name: name, uid: uint32(uid), gid: uint32(gid), home: u.HomeDir}, nil
}

// authorizedKeysPath returns the path of a's authorized_keys file.
func (a *sshAccount) authorizedKeysPath() string {
	return filepath.Join(a.home, sshDirName, authorizedKeysName)
}

// validateHome reports why a's home directory cannot hold an
// authorized_keys file managed by the provider.
func (a *sshAccount) validateHome() error {
	if a.home == "/" {
		return fmt.Errorf("the home directory of user %q is /; refusing to manage /%s", a.name, sshDirName)
	}
	if err := validateAbsolutePath(a.home); err != nil {
		return fmt.Errorf("the home directory of user %q is unusable: %w", a.name, err)
	}
	return nil
}

// sshDir is an open ~/.ssh directory.
type sshDir struct {
	dir *os.File
	// display is the directory's real path, for messages; the files in it
	// are only accessed through dir.
	display string
}

func (d *sshDir) Close() error { return d.dir.Close() }

// fdPath is the path through which the directory is accessed, which the
// kernel resolves to the open directory, not to whatever is at display now.
func (d *sshDir) fdPath() string {
	return "/proc/self/fd/" + strconv.Itoa(int(d.dir.Fd()))
}

// keysPath returns the path through which authorized_keys is accessed.
func (d *sshDir) keysPath() string {
	return d.fdPath() + "/" + authorizedKeysName
}

// displayError replaces the descriptor path in err's message with the real
// path. The result still wraps err.
func (d *sshDir) displayError(err error) error {
	if err == nil {
		return nil
	}
	return &rewordedError{msg: strings.ReplaceAll(err.Error(), d.fdPath(), d.display), err: err}
}

// rewordedError is err with another message.
type rewordedError struct {
	msg string
	err error
}

func (e *rewordedError) Error() string { return e.msg }
func (e *rewordedError) Unwrap() error { return e.err }

// openSSHDir opens a's ~/.ssh directory without following a symlink at it.
// With create set, a missing directory is created with mode 0700 and owned
// by a; otherwise a missing home or ~/.ssh directory yields an error
// wrapping fs.ErrNotExist.
func openSSHDir(a *sshAccount, create bool) (*sshDir, error) {
	if err := a.validateHome(); err != nil {
		return nil, err
	}
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		return nil, fmt.Errorf("/proc must be mounted to manage authorized_keys safely: %w", err)
	}
	display := filepath.Join(a.home, sshDirName)

	home, err := unix.Open(a.home, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, fmt.Errorf("the home directory %q of user %q does not exist: %w", a.home, a.name, fs.ErrNotExist)
		}
		return nil, &fs.PathError{Op: "open", Path: a.home, Err: err}
	}
	defer func() { _ = unix.Close(home) }()

	const flags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fd, err := unix.Openat(home, sshDirName, flags, 0)
	created := false
	if errors.Is(err, unix.ENOENT) {
		if !create {
			return nil, &fs.PathError{Op: "open", Path: display, Err: fs.ErrNotExist}
		}
		switch err := unix.Mkdirat(home, sshDirName, sshDirMode); {
		case err == nil:
			created = true
		case errors.Is(err, unix.EEXIST):
			// Created concurrently; open whatever is there now.
		default:
			return nil, &fs.PathError{Op: "mkdir", Path: display, Err: err}
		}
		fd, err = unix.Openat(home, sshDirName, flags, 0)
	}
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			var st unix.Stat_t
			if unix.Fstatat(home, sshDirName, &st, unix.AT_SYMLINK_NOFOLLOW) == nil {
				if st.Mode&unix.S_IFMT == unix.S_IFLNK {
					return nil, symlinkRefusedError(display)
				}
				return nil, fmt.Errorf("path %q exists but is not a directory", display)
			}
		}
		return nil, &fs.PathError{Op: "open", Path: display, Err: err}
	}
	d := &sshDir{dir: os.NewFile(uintptr(fd), display), display: display}

	if created {
		if err := d.initCreated(a); err != nil {
			_ = d.Close()
			return nil, err
		}
	}
	return d, nil
}

// initCreated gives a newly created ~/.ssh its owner and mode; mkdir applied
// the umask to the mode. The directory opened after mkdir is only changed if
// it is owned by the provider's user, as the one it created is: something
// the user swapped in meanwhile is theirs already, and anything else is left
// alone.
func (d *sshDir) initCreated(a *sshAccount) error {
	var st unix.Stat_t
	if err := unix.Fstat(int(d.dir.Fd()), &st); err != nil {
		return &fs.PathError{Op: "stat", Path: d.display, Err: err}
	}
	if st.Uid != uint32(os.Geteuid()) { //nolint:gosec // Effective UIDs are never negative.
		return nil
	}
	if err := chownToMatch(d.dir, a.uid, a.gid); err != nil {
		return fmt.Errorf("setting ownership of %q: %w", d.display, err)
	}
	if err := d.dir.Chmod(sshDirMode); err != nil {
		return fmt.Errorf("setting mode of %q: %w", d.display, err)
	}
	return nil
}

// readAuthorizedKeys reads the authorized_keys file in d. A missing file
// reads as nil data with a nil snapshot.
func (d *sshDir) readAuthorizedKeys() ([]byte, *fileSnapshot, error) {
	data, snap, err := readRegularFileNoFollow(d.keysPath(), maxAuthorizedKeysSize)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, d.displayError(err)
	}
	if snap.nlink > 1 {
		return nil, nil, fmt.Errorf("%q has %d hard links; refusing to edit it, since it may be a link to another file", filepath.Join(d.display, authorizedKeysName), snap.nlink)
	}
	return data, snap, nil
}

// writeAuthorizedKeys atomically replaces the authorized_keys file in d,
// last read as orig (nil if it did not exist), with data. The file gets mode
// 0600 and is owned by a, whatever it was before, as sshd's StrictModes
// requires; this is set before the rename, so the new contents never
// appear with other permissions. Extended attributes such as the SELinux
// label are carried over, except an ACL.
func (d *sshDir) writeAuthorizedKeys(a *sshAccount, data []byte, orig *fileSnapshot) error {
	attrs := replaceAttrs{mode: authorizedKeysFileMode, chown: true, uid: a.uid, gid: a.gid, dropACL: true}
	if orig != nil {
		attrs.xattrs = make(map[string][]byte, len(orig.xattrs))
		for k, v := range orig.xattrs {
			if k != aclAccessXattr {
				attrs.xattrs[k] = v
			}
		}
	}
	return d.displayError(replaceFileAtomicWith(d.keysPath(), data, orig, attrs))
}
