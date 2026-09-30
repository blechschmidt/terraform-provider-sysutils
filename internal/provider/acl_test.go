package provider

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

// parseTestACL parses the short text form of posixACL.String, with numeric
// qualifiers, such as "user::rw-,user:1000:r--,group::r--,mask::r--,other::---".
func parseTestACL(t *testing.T, s string) posixACL {
	t.Helper()
	var a posixACL
	for _, part := range strings.Split(s, ",") {
		fields := strings.Split(part, ":")
		if len(fields) != 3 {
			t.Fatalf("bad entry %q", part)
		}
		perm, err := parseACLPerm(fields[2])
		if err != nil {
			t.Fatal(err)
		}
		e := aclEntry{Perm: perm}
		named := fields[1] != ""
		switch {
		case fields[0] == "user" && !named:
			e.Tag = aclUserObj
		case fields[0] == "user":
			e.Tag = aclUser
		case fields[0] == "group" && !named:
			e.Tag = aclGroupObj
		case fields[0] == "group":
			e.Tag = aclGroup
		case fields[0] == "mask":
			e.Tag = aclMask
		case fields[0] == "other":
			e.Tag = aclOther
		default:
			t.Fatalf("bad entry %q", part)
		}
		if named {
			id, ok := parseID(fields[1])
			if !ok {
				t.Fatalf("bad ID in %q", part)
			}
			e.ID = uint32(id)
		}
		a = append(a, e)
	}
	return a
}

func TestACLPerm(t *testing.T) {
	for p := uint16(0); p <= aclPermAll; p++ {
		s := formatACLPerm(p)
		got, err := parseACLPerm(s)
		if err != nil || got != p {
			t.Errorf("parseACLPerm(formatACLPerm(%d) = %q) = %d, %v", p, s, got, err)
		}
	}
	for _, s := range []string{"", "rw", "rx", "rwxr", "xwr", "RWX", "r x", "7", "r-X"} {
		if _, err := parseACLPerm(s); err == nil {
			t.Errorf("parseACLPerm(%q) = nil error, want one", s)
		}
	}
}

// The attribute of "user::rw-,user:1000:r--,group::r-x,group:50:rw-,
// mask::rwx,other::---" as the kernel stores it (getfattr -e hex).
const testACLHex = "02000000" +
	"0100" + "0600" + "ffffffff" +
	"0200" + "0400" + "e8030000" +
	"0400" + "0500" + "ffffffff" +
	"0800" + "0600" + "32000000" +
	"1000" + "0700" + "ffffffff" +
	"2000" + "0000" + "ffffffff"

func TestEncodeDecodeACLXattr(t *testing.T) {
	want, err := hex.DecodeString(testACLHex)
	if err != nil {
		t.Fatal(err)
	}
	acl := parseTestACL(t, "user::rw-,user:1000:r--,group::r-x,group:50:rw-,mask::rwx,other::---")

	// Entries are written in canonical order, whatever order they are in.
	shuffled := posixACL{acl[5], acl[3], acl[1], acl[4], acl[0], acl[2]}
	got, err := encodeACLXattr(shuffled)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("encodeACLXattr = %x, want %x", got, want)
	}

	decoded, err := decodeACLXattr(want)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.String() != acl.String() {
		t.Errorf("decodeACLXattr = %s, want %s", decoded, acl)
	}
}

func TestEncodeACLXattrSortsNamedEntriesByID(t *testing.T) {
	acl := parseTestACL(t, "user::rwx,user:2000:r--,user:10:r--,group::---,group:7:--x,group:3:-w-,mask::rwx,other::---")
	b, err := encodeACLXattr(acl)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeACLXattr(b)
	if err != nil {
		t.Fatal(err)
	}
	const want = "user::rwx,user:10:r--,user:2000:r--,group::---,group:3:-w-,group:7:--x,mask::rwx,other::---"
	if decoded.String() != want {
		t.Errorf("round trip = %s, want %s", decoded, want)
	}
	// The unnamed entries carry ACL_UNDEFINED_ID, whatever their ID field.
	withID := parseTestACL(t, "user::rw-,group::r--,other::r--")
	withID[0].ID = 42
	b, err = encodeACLXattr(withID)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(b[8:12]); got != "ffffffff" {
		t.Errorf("owner entry ID = %s, want ffffffff", got)
	}
}

func TestDecodeACLXattrRejectsMalformed(t *testing.T) {
	valid, _ := hex.DecodeString(testACLHex)
	mutate := func(off int, b ...byte) []byte {
		v := append([]byte(nil), valid...)
		copy(v[off:], b)
		return v
	}
	cases := map[string][]byte{
		"empty":          {},
		"short header":   {2, 0, 0},
		"version 1":      mutate(0, 1),
		"partial entry":  valid[:len(valid)-3],
		"no entries":     valid[:4],
		"unknown tag":    mutate(4, 0x40, 0),
		"unknown perm":   mutate(6, 0x0e, 0),
		"no other":       valid[:len(valid)-8],
		"duplicate user": mutate(4+8*3, 0x02, 0x00, 0x06, 0x00, 0xe8, 0x03, 0, 0), // group:50 turned into user:1000
		"undefined ID":   mutate(4+8+4, 0xff, 0xff, 0xff, 0xff),
	}
	// Named entries without a mask.
	noMask, err := encodeACLXattr(parseTestACL(t, "user::rw-,group::r--,other::---"))
	if err != nil {
		t.Fatal(err)
	}
	cases["named without mask"] = append(noMask[:12:12], append([]byte{0x02, 0, 0x04, 0, 1, 0, 0, 0}, noMask[12:]...)...)
	for name, b := range cases {
		if acl, err := decodeACLXattr(b); err == nil {
			t.Errorf("%s: decodeACLXattr(%x) = %s, want an error", name, b, acl)
		}
	}
}

func TestEncodeACLXattrRejectsInvalid(t *testing.T) {
	for _, s := range []string{
		"user::rw-,group::r--",                       // No other.
		"user::rw-,user:5:r--,group::r--,other::---", // Named without mask.
		"user::rw-,user::r--,group::r--,other::---",  // Two owners.
		"user::rw-,group:5:r--,group:5:rw-,mask::rw-,other::---",
	} {
		if _, err := encodeACLXattr(parseTestACL(t, s)); err == nil {
			t.Errorf("encodeACLXattr(%s) = nil error, want one", s)
		}
	}
}

func TestACLFromMode(t *testing.T) {
	if got, want := aclFromMode(0o4751).String(), "user::rwx,group::r-x,other::--x"; got != want {
		t.Errorf("aclFromMode(04751) = %s, want %s", got, want)
	}
}

func TestApplyACLEntries(t *testing.T) {
	cases := []struct {
		name, cur, want string
		exclusive       bool
		result          string
	}{
		{
			name: "mask computed from the group class", cur: "user::rw-,group::r--,other::---",
			want: "user:1000:rw-", exclusive: true,
			result: "user::rw-,user:1000:rw-,group::r--,mask::rw-,other::---",
		},
		{
			name: "exclusive drops unlisted named entries and recomputes the mask",
			cur:  "user::rw-,user:1000:rwx,group::r--,group:50:rw-,mask::rwx,other::---",
			want: "group:60:r--", exclusive: true,
			result: "user::rw-,group::r--,group:60:r--,mask::r--,other::---",
		},
		{
			name: "merge keeps unlisted named entries",
			cur:  "user::rw-,user:1000:rwx,group::r--,mask::rwx,other::---",
			want: "group:60:--x", exclusive: false,
			result: "user::rw-,user:1000:rwx,group::r--,group:60:--x,mask::rwx,other::---",
		},
		{
			name: "explicit mask is kept",
			cur:  "user::rw-,group::r--,other::---",
			want: "user:1000:rwx,mask::r--", exclusive: true,
			result: "user::rw-,user:1000:rwx,group::r--,mask::r--,other::---",
		},
		{
			name: "exclusive without named entries is minimal",
			cur:  "user::rw-,user:1000:rwx,group::r--,mask::rwx,other::---",
			want: "other::r--", exclusive: true,
			result: "user::rw-,group::r--,other::r--",
		},
		{
			name: "merge keeps a lone mask, recomputed",
			cur:  "user::rw-,group::rw-,mask::r--,other::---",
			want: "user::rwx", exclusive: false,
			result: "user::rwx,group::rw-,mask::rw-,other::---",
		},
		{
			name: "base entries set",
			cur:  "user::rw-,group::r--,other::r--",
			want: "user::r--,group::---,other::---", exclusive: true,
			result: "user::r--,group::---,other::---",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := applyACLEntries(parseTestACL(t, c.cur), parseTestACL(t, "user::rw-,group::r--,other::---,"+c.want)[3:], c.exclusive)
			if got.String() != c.result {
				t.Errorf("applyACLEntries = %s, want %s", got, c.result)
			}
			// Applying the result again changes nothing, and reports no
			// drift.
			want := parseTestACL(t, "user::rw-,group::r--,other::---,"+c.want)[3:]
			if again := applyACLEntries(got, want, c.exclusive); !again.equal(got) {
				t.Errorf("second applyACLEntries = %s, want %s", again, got)
			}
			if _, unexpected := aclDrift(got, want, c.exclusive); len(unexpected) != 0 {
				t.Errorf("aclDrift(%s) reports %v", got, unexpected)
			}
		})
	}
}

func TestACLDrift(t *testing.T) {
	want := parseTestACL(t, "user::rw-,user:1000:rw-,group::r--,other::---")[1:2]
	cases := []struct {
		name, actual string
		exclusive    bool
		managed      string // Permissions of user:1000, or "" if missing.
		unexpected   string
	}{
		{"in sync", "user::rw-,user:1000:rw-,group::r--,mask::rw-,other::---", true, "rw-", ""},
		{"permissions changed", "user::rw-,user:1000:r--,group::r--,mask::r--,other::---", true, "r--", ""},
		{"entry removed", "user::rw-,group::r--,other::---", true, "", ""},
		{
			"extra entry, exclusive", "user::rw-,user:1000:rw-,user:2000:r--,group::r--,mask::rw-,other::---", true, "rw-",
			"user:2000:r--",
		},
		{"extra entry, merged", "user::rw-,user:1000:rw-,user:2000:r--,group::r--,mask::rw-,other::---", false, "rw-", ""},
		{"mask narrowed", "user::rw-,user:1000:rw-,group::r--,mask::r--,other::---", true, "rw-", "mask::r--"},
		{"lone mask, exclusive", "user::rw-,group::r--,mask::r--,other::---", true, "", "mask::r--"},
		{"lone mask, merged", "user::rw-,group::r--,mask::r--,other::---", false, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			managed, unexpected := aclDrift(parseTestACL(t, c.actual), want, c.exclusive)
			e, ok := managed[aclKey{aclUser, 1000}]
			switch {
			case c.managed == "" && ok:
				t.Errorf("managed entry = %v, want none", e)
			case c.managed != "" && (!ok || formatACLPerm(e.Perm) != c.managed):
				t.Errorf("managed entry = %v, %v, want %s", e, ok, c.managed)
			}
			if got := posixACL(unexpected).String(); got != c.unexpected {
				t.Errorf("unexpected = %q, want %q", got, c.unexpected)
			}
		})
	}
}

func TestStripACLEntries(t *testing.T) {
	cur := parseTestACL(t, "user::rw-,user:1000:rwx,user:2000:r--,group::r--,group:50:-w-,mask::rwx,other::---")
	managed := parseTestACL(t, "user::rw-,user:1000:rwx,group::r--,mask::rwx,other::---")
	if got, want := stripACLEntries(cur, managed, true).String(), "user::rw-,group::r--,other::---"; got != want {
		t.Errorf("exclusive strip = %s, want %s", got, want)
	}
	if got, want := stripACLEntries(cur, managed, false).String(), "user::rw-,user:2000:r--,group::r--,group:50:-w-,mask::rw-,other::---"; got != want {
		t.Errorf("merged strip = %s, want %s", got, want)
	}
	// Removing the last named entry drops the mask; base entries stay.
	only := parseTestACL(t, "user::r-x,user:1000:rwx,group::---,mask::rwx,other::--x")
	if got, want := stripACLEntries(only, managed, false).String(), "user::r-x,group::---,other::--x"; got != want {
		t.Errorf("strip = %s, want %s", got, want)
	}
}

func TestObservedACLEntries(t *testing.T) {
	managed := []fileACLEntryModel{
		testACLEntryModel("user", "", "rw-"),
		testACLEntryModel("user", "0", "rwx"),
		testACLEntryModel("group", "0", "r--"),
	}
	actual := parseTestACL(t, "user::rw-,user:0:rwx,group::r--,group:0:r--,mask::rwx,other::---")
	got := observedACLEntries(actual, managed, true)
	if len(got) != 3 {
		t.Fatalf("observedACLEntries = %v, want the 3 managed entries", got)
	}
	for _, m := range got {
		if m.Name.ValueString() != "" && m.Name.ValueString() != "0" {
			t.Errorf("entry %v: the configured spelling of the name was not kept", m)
		}
	}
	// A permission change and an extra entry show up.
	actual = parseTestACL(t, "user::rw-,user:0:r--,user:65534:r--,group::r--,group:0:r--,mask::r--,other::---")
	var texts []string
	for _, m := range observedACLEntries(actual, managed, true) {
		texts = append(texts, describeACLEntry(m)+"="+m.Permissions.ValueString())
	}
	if got, want := strings.Join(texts, " "), "group:0=r-- user=rw- user:0=r-- user:"+uidName(65534)+"=r--"; got != want {
		t.Errorf("observedACLEntries = %s, want %s", got, want)
	}
}

func TestResolveACLEntriesRejectsAliases(t *testing.T) {
	_, err := resolveACLEntries([]fileACLEntryModel{
		testACLEntryModel("user", "root", "r--"),
		testACLEntryModel("user", "0", "rw-"),
	})
	if err == nil || !strings.Contains(err.Error(), "both refer to") {
		t.Errorf("resolveACLEntries = %v, want a duplicate error", err)
	}
	if _, err := resolveACLEntries([]fileACLEntryModel{testACLEntryModel("mask", "x", "r--")}); err == nil {
		t.Error("a named mask entry was accepted")
	}
}

func TestValidateACLEntryName(t *testing.T) {
	for _, s := range []string{"alice", "www-data", "0", "65534", "4294967294"} {
		if err := validateACLEntryName(s); err != nil {
			t.Errorf("validateACLEntryName(%q) = %v", s, err)
		}
	}
	for _, s := range []string{"", "-x", "a:b", "a b", "a,b", "99999999999"} {
		if err := validateACLEntryName(s); err == nil {
			t.Errorf("validateACLEntryName(%q) = nil, want an error", s)
		}
	}
}

// TestACLXattrMatchesSetfacl checks the codec against the ACLs setfacl and
// getfacl read and write, on a real file system.
func TestACLXattrMatchesSetfacl(t *testing.T) {
	dir := t.TempDir()
	requireACLs(t, dir)
	p := filepath.Join(dir, "f")
	mustWrite(t, p, "")
	runTool(t, "setfacl", "--set", "u::rw-,u:65534:r--,g::r-x,g:65534:-w-,m::rwx,o::---", p)

	f, err := openACLTarget(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	got, err := f.readAccessACL()
	if err != nil {
		t.Fatal(err)
	}
	if want := "user::rw-,user:65534:r--,group::r-x,group:65534:-w-,mask::rwx,other::---"; got.String() != want {
		t.Errorf("readAccessACL = %s, want %s", got, want)
	}

	next := applyACLEntries(got, parseTestACL(t, "user::rw-,group::r--,other::---,user:65534:rwx")[3:], false)
	if err := f.writeAccessACL(next); err != nil {
		t.Fatal(err)
	}
	const want = "user::rw-\nuser:65534:rwx\ngroup::r-x\ngroup:65534:-w-\nmask::rwx\nother::---"
	if out := strings.TrimSpace(runTool(t, "getfacl", "-cnpE", p)); out != want {
		t.Errorf("getfacl after write:\n%s\nwant:\n%s", out, want)
	}
	// The kernel keeps the mode's group bits in sync with the mask.
	if info, err := os.Stat(p); err != nil || info.Mode().Perm() != 0o670 {
		t.Errorf("mode = %v, %v; want 0670", info.Mode(), err)
	}

	// A minimal ACL removes the attribute and sets the mode.
	if err := f.writeAccessACL(stripACLEntries(next, nil, true)); err != nil {
		t.Fatal(err)
	}
	if val, err := f.getXattr(aclAccessXattr); err != nil || val != nil {
		t.Errorf("access ACL attribute after strip = %x, %v; want none", val, err)
	}
	if info, err := os.Stat(p); err != nil || info.Mode().Perm() != 0o650 {
		t.Errorf("mode = %v, %v; want 0650", info.Mode(), err)
	}

	// Default ACLs of directories.
	d := filepath.Join(dir, "d")
	if err := os.Mkdir(d, 0o750); err != nil {
		t.Fatal(err)
	}
	runTool(t, "setfacl", "-d", "--set", "u::rwx,g::r-x,g:65534:rwx,m::rwx,o::---", d)
	df, err := openACLTarget(d)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = df.Close() }()
	def, err := df.readDefaultACL()
	if err != nil {
		t.Fatal(err)
	}
	if want := "user::rwx,group::r-x,group:65534:rwx,mask::rwx,other::---"; def.String() != want {
		t.Errorf("readDefaultACL = %s, want %s", def, want)
	}
	if err := df.writeDefaultACL(nil); err != nil {
		t.Fatal(err)
	}
	if out := strings.TrimSpace(runTool(t, "getfacl", "-cnpd", d)); out != "" {
		t.Errorf("default ACL after removal:\n%s", out)
	}
}

func TestOpenACLTargetRefusesSymlinksAndSpecialFiles(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	mustWrite(t, target, "")
	link := filepath.Join(dir, "link")
	mustSymlink(t, target, link)
	if _, err := openACLTarget(link); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("openACLTarget(symlink) = %v, want a symlink error", err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openACLTarget(fifo); err == nil || !strings.Contains(err.Error(), "neither a regular file nor a directory") {
		t.Errorf("openACLTarget(fifo) = %v, want an error", err)
	}
}

func testACLEntryModel(typ, name, perm string) fileACLEntryModel {
	m := fileACLEntryModel{Type: types.StringValue(typ), Permissions: types.StringValue(perm), Name: types.StringNull()}
	if name != "" {
		m.Name = types.StringValue(name)
	}
	return m
}
