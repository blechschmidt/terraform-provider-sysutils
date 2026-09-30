package provider

// POSIX access control lists, read and written as the Linux
// system.posix_acl_access and system.posix_acl_default extended attributes,
// so that no setfacl/getfacl binary is needed.
//
// The attribute value is the kernel's posix_acl_xattr format (see
// include/uapi/linux/posix_acl_xattr.h): a little-endian uint32 version (2)
// followed by one 8-byte entry per ACL entry, each a uint16 tag, a uint16
// permission set and a uint32 user or group ID, which is ACL_UNDEFINED_ID for
// the entries without a qualifier.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// aclDefaultXattr holds a directory's default ACL, the ACL that new entries
// created in it inherit. aclAccessXattr is defined in safefs.go.
const aclDefaultXattr = "system.posix_acl_default"

const (
	aclXattrVersion = 2
	aclHeaderSize   = 4
	aclEntrySize    = 8
	// aclUndefinedID is the qualifier of entries that have none.
	aclUndefinedID = 0xffffffff
	// aclMaxEntries bounds the entries decoded from an attribute. ext4 and
	// XFS store at most a few hundred; the bound only guards against
	// allocating for nonsense.
	aclMaxEntries = 1 << 16
)

// aclTag is the type of an ACL entry, with the kernel's values.
type aclTag uint16

const (
	aclUserObj  aclTag = 0x01 // The owner: "user::".
	aclUser     aclTag = 0x02 // A named user: "user:<uid>:".
	aclGroupObj aclTag = 0x04 // The owning group: "group::".
	aclGroup    aclTag = 0x08 // A named group: "group:<gid>:".
	aclMask     aclTag = 0x10 // The upper bound of the group class: "mask::".
	aclOther    aclTag = 0x20 // Everyone else: "other::".
)

// String returns the tag's name as setfacl writes it, with a "::" suffix for
// the entries without a qualifier.
func (t aclTag) String() string {
	switch t {
	case aclUserObj:
		return "user::"
	case aclUser:
		return "user"
	case aclGroupObj:
		return "group::"
	case aclGroup:
		return "group"
	case aclMask:
		return "mask::"
	case aclOther:
		return "other::"
	}
	return fmt.Sprintf("tag(0x%x)", uint16(t))
}

// named reports whether entries with tag t have a user or group ID.
func (t aclTag) named() bool { return t == aclUser || t == aclGroup }

// ACL permission bits, with the kernel's values.
const (
	aclRead    uint16 = 0x4
	aclWrite   uint16 = 0x2
	aclExecute uint16 = 0x1
	aclPermAll        = aclRead | aclWrite | aclExecute
)

// aclEntry is one entry of an ACL. ID is only meaningful for aclUser and
// aclGroup entries.
type aclEntry struct {
	Tag  aclTag
	ID   uint32
	Perm uint16
}

// key identifies the entry within its ACL: an ACL has at most one entry per
// tag, or per tag and ID for the named entries.
func (e aclEntry) key() aclKey {
	if e.Tag.named() {
		return aclKey{e.Tag, e.ID}
	}
	return aclKey{Tag: e.Tag}
}

// aclKey identifies an entry of an ACL; see aclEntry.key.
type aclKey struct {
	Tag aclTag
	ID  uint32
}

func (k aclKey) String() string {
	if k.Tag.named() {
		return fmt.Sprintf("%s:%d:", k.Tag, k.ID)
	}
	return k.Tag.String()
}

// posixACL is a POSIX ACL. Its entries are kept in the kernel's canonical
// order (see sortACL) by every function that returns one.
type posixACL []aclEntry

// formatACLPerm returns p in setfacl's fixed-width form, such as "r-x".
func formatACLPerm(p uint16) string {
	b := []byte("---")
	if p&aclRead != 0 {
		b[0] = 'r'
	}
	if p&aclWrite != 0 {
		b[1] = 'w'
	}
	if p&aclExecute != 0 {
		b[2] = 'x'
	}
	return string(b)
}

// parseACLPerm parses a permission set in the fixed-width form of
// formatACLPerm: exactly three characters, "r" or "-", "w" or "-", and "x"
// or "-". Only this form is accepted so that every set has a single
// spelling, and plans never show a difference between "rx" and "r-x".
func parseACLPerm(s string) (uint16, error) {
	if len(s) != 3 {
		return 0, fmt.Errorf("permissions %q must be three characters such as \"rwx\", \"r-x\" or \"---\"", s)
	}
	var p uint16
	for i, bit := range []struct {
		c byte
		v uint16
	}{{'r', aclRead}, {'w', aclWrite}, {'x', aclExecute}} {
		switch s[i] {
		case bit.c:
			p |= bit.v
		case '-':
		default:
			return 0, fmt.Errorf("permissions %q must be three characters such as \"rwx\", \"r-x\" or \"---\": character %d must be %q or \"-\"", s, i+1, bit.c)
		}
	}
	return p, nil
}

// sortACL sorts a into the order the kernel requires: the owner, named users
// by UID, the owning group, named groups by GID, the mask, and other.
func sortACL(a posixACL) {
	sort.SliceStable(a, func(i, j int) bool {
		if a[i].Tag != a[j].Tag {
			return a[i].Tag < a[j].Tag
		}
		return a[i].Tag.named() && a[i].ID < a[j].ID
	})
}

// validate reports why a is not a valid ACL: it must have exactly one owner,
// owning group and other entry, at most one entry per named user or group,
// no unknown tags or permission bits, and a mask if it has named entries.
// This is what the kernel's posix_acl_valid enforces, checked here so that
// errors name the offending entry instead of just "invalid argument".
func (a posixACL) validate() error {
	seen := map[aclKey]bool{}
	named := false
	for _, e := range a {
		switch e.Tag {
		case aclUserObj, aclGroupObj, aclMask, aclOther:
		case aclUser, aclGroup:
			named = true
			if e.ID == aclUndefinedID {
				return fmt.Errorf("entry %s has the invalid ID %d", e.Tag, e.ID)
			}
		default:
			return fmt.Errorf("unknown ACL entry type 0x%x", uint16(e.Tag))
		}
		if e.Perm&^aclPermAll != 0 {
			return fmt.Errorf("entry %s has unknown permission bits 0x%x", e.key(), e.Perm)
		}
		if seen[e.key()] {
			return fmt.Errorf("duplicate entry %s", e.key())
		}
		seen[e.key()] = true
	}
	for _, t := range []aclTag{aclUserObj, aclGroupObj, aclOther} {
		if !seen[aclKey{Tag: t}] {
			return fmt.Errorf("ACL has no %s entry", t)
		}
	}
	if named && !seen[aclKey{Tag: aclMask}] {
		return errors.New("an ACL with named user or group entries needs a mask entry")
	}
	return nil
}

// get returns the entry with key k.
func (a posixACL) get(k aclKey) (aclEntry, bool) {
	for _, e := range a {
		if e.key() == k {
			return e, true
		}
	}
	return aclEntry{}, false
}

// hasNamed reports whether a has named user or group entries.
func (a posixACL) hasNamed() bool {
	for _, e := range a {
		if e.Tag.named() {
			return true
		}
	}
	return false
}

// isMinimal reports whether a consists of the owner, owning group and other
// entries only, and so is fully expressed by the file's mode bits.
func (a posixACL) isMinimal() bool {
	for _, e := range a {
		if e.Tag != aclUserObj && e.Tag != aclGroupObj && e.Tag != aclOther {
			return false
		}
	}
	return true
}

// groupClassPerm returns the union of the permissions of the entries in the
// group class: named users, the owning group and named groups. This is the
// mask setfacl computes when it is not given explicitly.
func (a posixACL) groupClassPerm() uint16 {
	var p uint16
	for _, e := range a {
		switch e.Tag {
		case aclUser, aclGroupObj, aclGroup:
			p |= e.Perm
		}
	}
	return p
}

// set returns a copy of a with e added, or replacing the entry with the same
// key.
func (a posixACL) set(e aclEntry) posixACL {
	out := make(posixACL, 0, len(a)+1)
	for _, o := range a {
		if o.key() != e.key() {
			out = append(out, o)
		}
	}
	out = append(out, e)
	sortACL(out)
	return out
}

// without returns a copy of a without the entries for which drop is true.
func (a posixACL) without(drop func(aclEntry) bool) posixACL {
	out := make(posixACL, 0, len(a))
	for _, e := range a {
		if !drop(e) {
			out = append(out, e)
		}
	}
	return out
}

// withRecomputedMask returns a copy of a whose mask is the union of the
// group class, as setfacl computes it. An ACL without named entries keeps
// a mask only if keepLoneMask is set and it already has one.
func (a posixACL) withRecomputedMask(keepLoneMask bool) posixACL {
	_, hasMask := a.get(aclKey{Tag: aclMask})
	if !a.hasNamed() && (!keepLoneMask || !hasMask) {
		return a.without(func(e aclEntry) bool { return e.Tag == aclMask })
	}
	return a.set(aclEntry{Tag: aclMask, Perm: a.groupClassPerm()})
}

// String returns a in setfacl's short text form, such as
// "user::rw-,user:1000:r--,group::r--,mask::r--,other::---".
func (a posixACL) String() string {
	parts := make([]string, len(a))
	for i, e := range a {
		parts[i] = e.key().String() + formatACLPerm(e.Perm)
	}
	return strings.Join(parts, ",")
}

// encodeACLXattr returns the extended attribute value for a, which must be
// valid. The entries are written in canonical order.
func encodeACLXattr(a posixACL) ([]byte, error) {
	sorted := append(posixACL(nil), a...)
	sortACL(sorted)
	if err := sorted.validate(); err != nil {
		return nil, err
	}
	buf := make([]byte, aclHeaderSize+aclEntrySize*len(sorted))
	binary.LittleEndian.PutUint32(buf, aclXattrVersion)
	for i, e := range sorted {
		off := aclHeaderSize + aclEntrySize*i
		id := uint32(aclUndefinedID)
		if e.Tag.named() {
			id = e.ID
		}
		binary.LittleEndian.PutUint16(buf[off:], uint16(e.Tag))
		binary.LittleEndian.PutUint16(buf[off+2:], e.Perm)
		binary.LittleEndian.PutUint32(buf[off+4:], id)
	}
	return buf, nil
}

// decodeACLXattr parses an extended attribute value written by the kernel
// or encodeACLXattr. It rejects malformed values and invalid ACLs rather than
// guessing, since the result is written back.
func decodeACLXattr(b []byte) (posixACL, error) {
	if len(b) < aclHeaderSize {
		return nil, fmt.Errorf("ACL attribute of %d bytes is too short", len(b))
	}
	if v := binary.LittleEndian.Uint32(b); v != aclXattrVersion {
		return nil, fmt.Errorf("unsupported ACL attribute version %d", v)
	}
	body := b[aclHeaderSize:]
	if len(body)%aclEntrySize != 0 {
		return nil, fmt.Errorf("ACL attribute of %d bytes is not a whole number of entries", len(b))
	}
	n := len(body) / aclEntrySize
	if n > aclMaxEntries {
		return nil, fmt.Errorf("ACL attribute has %d entries, more than the %d supported", n, aclMaxEntries)
	}
	a := make(posixACL, n)
	for i := range a {
		off := aclEntrySize * i
		a[i] = aclEntry{
			Tag:  aclTag(binary.LittleEndian.Uint16(body[off:])),
			Perm: binary.LittleEndian.Uint16(body[off+2:]),
		}
		if a[i].Tag.named() {
			a[i].ID = binary.LittleEndian.Uint32(body[off+4:])
		}
	}
	sortACL(a)
	if err := a.validate(); err != nil {
		return nil, err
	}
	return a, nil
}

// aclFromMode returns the minimal ACL equivalent to the permission bits of
// mode, which is the access ACL of a file without an ACL attribute.
func aclFromMode(mode fs.FileMode) posixACL {
	m := uint16(mode.Perm())
	return posixACL{
		{Tag: aclUserObj, Perm: m >> 6 & aclPermAll},
		{Tag: aclGroupObj, Perm: m >> 3 & aclPermAll},
		{Tag: aclOther, Perm: m & aclPermAll},
	}
}

// errACLNotSupported is wrapped by the errors of the ACL functions when the
// file system (or the kernel) has no POSIX ACL support.
var errACLNotSupported = errors.New("the file system does not support POSIX ACLs")

// errNotACLTarget matches the errors of openACLTarget for paths that are not
// a regular file or directory, such as a symlink (see notACLTargetError).
var errNotACLTarget = errors.New("not a regular file or directory")

// notACLTargetError is an error that matches errNotACLTarget without adding
// its text to the message.
type notACLTargetError struct{ err error }

func (e notACLTargetError) Error() string        { return e.err.Error() }
func (e notACLTargetError) Unwrap() error        { return e.err }
func (e notACLTargetError) Is(target error) bool { return target == errNotACLTarget }

// aclFile is an open file or directory whose ACLs are read or written.
type aclFile struct {
	f     *os.File
	isDir bool
	mode  fs.FileMode
}

// openACLTarget opens the host path p for reading and writing its ACLs. It
// refuses a symlink at p (the open uses O_NOFOLLOW) and anything but a
// regular file or directory: opening a device node can have side effects,
// and other special files can't have ACLs set by setfacl either. The ACL
// attributes are read and written through the returned descriptor, so p can't
// be swapped for another file in between.
func openACLTarget(p string) (*aclFile, error) {
	// Rejected before opening; the check after it catches a swap.
	info, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, notACLTargetError{symlinkRefusedError(p)}
	}
	if !info.Mode().IsRegular() && !info.IsDir() {
		return nil, notACLTargetError{fmt.Errorf("path %q is neither a regular file nor a directory", p)}
	}
	f, err := openNoFollow(p, os.O_RDONLY, 0)
	if err != nil {
		if info, lerr := os.Lstat(p); lerr == nil && info.Mode()&fs.ModeSymlink != 0 {
			return nil, notACLTargetError{err}
		}
		return nil, err
	}
	af, err := newACLFile(f)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return af, nil
}

func newACLFile(f *os.File) (*aclFile, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() && !info.IsDir() {
		return nil, notACLTargetError{fmt.Errorf("path %q is neither a regular file nor a directory", f.Name())}
	}
	return &aclFile{f: f, isDir: info.IsDir(), mode: info.Mode()}, nil
}

func (a *aclFile) Close() error { return a.f.Close() }

// getXattr returns the value of the extended attribute name, or nil if the
// file does not have it. A file system without ACL support has no ACL
// attributes, so that is not an error here; writing one fails.
func (a *aclFile) getXattr(name string) ([]byte, error) {
	rc, err := a.f.SyscallConn()
	if err != nil {
		return nil, err
	}
	var val []byte
	var opErr error
	err = rc.Control(func(fd uintptr) {
		val, opErr = xattrCall(func(buf []byte) (int, error) { return unix.Fgetxattr(int(fd), name, buf) })
	})
	if err != nil {
		return nil, err
	}
	switch {
	case errors.Is(opErr, unix.ENODATA), errors.Is(opErr, unix.ENOTSUP):
		return nil, nil
	case opErr != nil:
		return nil, fmt.Errorf("reading %s of %q: %w", name, a.f.Name(), opErr)
	}
	if val == nil {
		// An empty attribute, which the kernel never stores for ACLs.
		return []byte{}, nil
	}
	return val, nil
}

// readAccessACL returns the access ACL: the attribute if the file has one,
// or else the minimal ACL of its mode.
func (a *aclFile) readAccessACL() (posixACL, error) {
	val, err := a.getXattr(aclAccessXattr)
	if err != nil {
		return nil, err
	}
	if val == nil {
		return aclFromMode(a.mode), nil
	}
	acl, err := decodeACLXattr(val)
	if err != nil {
		return nil, fmt.Errorf("decoding %s of %q: %w", aclAccessXattr, a.f.Name(), err)
	}
	return acl, nil
}

// readDefaultACL returns the default ACL, or nil if the file has none. Only
// directories have default ACLs.
func (a *aclFile) readDefaultACL() (posixACL, error) {
	if !a.isDir {
		return nil, nil
	}
	val, err := a.getXattr(aclDefaultXattr)
	if err != nil || val == nil {
		return nil, err
	}
	acl, err := decodeACLXattr(val)
	if err != nil {
		return nil, fmt.Errorf("decoding %s of %q: %w", aclDefaultXattr, a.f.Name(), err)
	}
	return acl, nil
}

// writeAccessACL sets the access ACL. The kernel updates the mode to match
// (the group bits hold the mask if there is one, else the owning group's
// permissions) and stores no attribute for a minimal ACL, so writing a
// minimal ACL removes the extended entries and keeps the base entries.
func (a *aclFile) writeAccessACL(acl posixACL) error {
	return a.setACLXattr(aclAccessXattr, acl)
}

// writeDefaultACL sets the default ACL of a directory, or removes it if acl
// is nil.
func (a *aclFile) writeDefaultACL(acl posixACL) error {
	if !a.isDir {
		return fmt.Errorf("path %q is not a directory; only directories have default ACLs", a.f.Name())
	}
	if acl == nil {
		if err := removeXattr(a.f, aclDefaultXattr); err != nil {
			return fmt.Errorf("removing %s of %q: %w", aclDefaultXattr, a.f.Name(), err)
		}
		return nil
	}
	return a.setACLXattr(aclDefaultXattr, acl)
}

func (a *aclFile) setACLXattr(name string, acl posixACL) error {
	val, err := encodeACLXattr(acl)
	if err != nil {
		return fmt.Errorf("invalid ACL %s: %w", acl, err)
	}
	err = writeXattrs(a.f, map[string][]byte{name: val})
	switch {
	case errors.Is(err, unix.ENOTSUP):
		return fmt.Errorf("setting %s of %q: %w", name, a.f.Name(), errACLNotSupported)
	case err != nil:
		return fmt.Errorf("setting %s of %q to %s: %w", name, a.f.Name(), acl, err)
	}
	// The mode may have changed; refresh it for readAccessACL.
	info, err := a.f.Stat()
	if err != nil {
		return err
	}
	a.mode = info.Mode()
	return nil
}

// isACLNotSupported reports whether err means that the file system has no
// ACL support.
func isACLNotSupported(err error) bool {
	return errors.Is(err, errACLNotSupported) || errors.Is(err, syscall.EOPNOTSUPP)
}

// isBaseTag reports whether t is one of the entries every ACL has, which
// correspond to the owner, group and other permission bits of the mode.
func isBaseTag(t aclTag) bool {
	return t == aclUserObj || t == aclGroupObj || t == aclOther
}

// baseEntries returns the owner, owning group and other entries of a.
func (a posixACL) baseEntries() posixACL {
	return a.without(func(e aclEntry) bool { return !isBaseTag(e.Tag) })
}

// hasTag reports whether want has an entry with tag t.
func hasTag(want []aclEntry, t aclTag) bool {
	for _, e := range want {
		if e.Tag == t {
			return true
		}
	}
	return false
}

// applyACLEntries returns the ACL that results from setting the entries want
// on cur, like setfacl:
//
//   - With exclusive, every named entry and the mask of cur are dropped
//     first, like setfacl --set, but base entries not in want keep their
//     current permissions.
//   - Otherwise want is merged into cur, like setfacl -m.
//
// Unless want has a mask entry, the mask is then recomputed as the union of
// the group class, as setfacl does without -n. Without named entries, an
// exclusive ACL has no mask, and a merged one keeps its mask only if it had
// one. want must not have two entries with the same key.
func applyACLEntries(cur posixACL, want []aclEntry, exclusive bool) posixACL {
	next := append(posixACL(nil), cur...)
	if exclusive {
		next = next.baseEntries()
	}
	for _, e := range want {
		next = next.set(e)
	}
	if !hasTag(want, aclMask) {
		next = next.withRecomputedMask(!exclusive)
	}
	sortACL(next)
	return next
}

// aclDrift compares actual with the managed entries want, as applied by
// applyACLEntries with the same exclusive. It returns the actual entries with
// the keys of want (missing ones are absent) and the actual entries that
// applyACLEntries would remove or change although want does not list them:
// named entries of an exclusive ACL, and a mask that is not the one
// applyACLEntries would compute.
func aclDrift(actual posixACL, want []aclEntry, exclusive bool) (managed map[aclKey]aclEntry, unexpected []aclEntry) {
	wanted := map[aclKey]bool{}
	for _, e := range want {
		wanted[e.key()] = true
	}
	managed = map[aclKey]aclEntry{}
	for _, e := range actual {
		switch {
		case wanted[e.key()]:
			managed[e.key()] = e
		case e.Tag.named() && exclusive:
			unexpected = append(unexpected, e)
		case e.Tag == aclMask:
			expected, ok := actual.withRecomputedMask(!exclusive).get(aclKey{Tag: aclMask})
			if !ok || expected.Perm != e.Perm {
				unexpected = append(unexpected, e)
			}
		}
	}
	return managed, unexpected
}

// stripACLEntries returns cur without the extended entries a resource
// managed: every named entry if exclusive, or else the named entries whose
// keys are in managed. The mask is recomputed, and dropped if no named
// entries remain, so that the result has no extended entries left over from
// the resource. Base entries are left alone.
func stripACLEntries(cur posixACL, managed []aclEntry, exclusive bool) posixACL {
	drop := map[aclKey]bool{}
	for _, e := range managed {
		drop[e.key()] = true
	}
	next := cur.without(func(e aclEntry) bool {
		return e.Tag.named() && (exclusive || drop[e.key()])
	})
	return next.withRecomputedMask(false)
}

// removedACLEntries returns the named entries and mask of before whose keys
// are not in after: entries a resource managed but no longer does.
func removedACLEntries(before, after []aclEntry) []aclEntry {
	keep := map[aclKey]bool{}
	for _, e := range after {
		keep[e.key()] = true
	}
	var out []aclEntry
	for _, e := range before {
		if e.Tag.named() && !keep[e.key()] {
			out = append(out, e)
		}
	}
	return out
}

// equal reports whether a and b have the same entries, in any order.
func (a posixACL) equal(b posixACL) bool {
	if len(a) != len(b) {
		return false
	}
	x := append(posixACL(nil), a...)
	y := append(posixACL(nil), b...)
	sortACL(x)
	sortACL(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}
