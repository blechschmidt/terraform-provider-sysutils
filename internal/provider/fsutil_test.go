package provider

import (
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

func TestParseMode(t *testing.T) {
	cases := []struct {
		in   string
		want fs.FileMode
	}{
		{"0644", 0o644},
		{"644", 0o644},
		{"0755", 0o755},
		{"0", 0},
		{"0000", 0},
		{"0777", 0o777},
		{"00600", 0o600},
		{"4755", fs.ModeSetuid | 0o755},
		{"2755", fs.ModeSetgid | 0o755},
		{"1777", fs.ModeSticky | 0o777},
		{"01777", fs.ModeSticky | 0o777},
		{"6750", fs.ModeSetuid | fs.ModeSetgid | 0o750},
		{"7777", fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky | 0o777},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseMode(tc.in)
			if err != nil {
				t.Fatalf("parseMode(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("parseMode(%q) = %v (%#o), want %v (%#o)", tc.in, got, uint32(got), tc.want, uint32(tc.want))
			}
		})
	}
}

func TestParseMode_invalid(t *testing.T) {
	for _, in := range []string{
		"",
		" 0644",
		"0644 ",
		"rwxr-xr-x",
		"0o644",
		"0x1ff",
		"0648",
		"9",
		"-644",
		"+644",
		"0_644",
		"10000",
		"17777",
		"077777",
		"99999999999999999999",
	} {
		t.Run(in, func(t *testing.T) {
			if got, err := parseMode(in); err == nil {
				t.Fatalf("parseMode(%q) = %v, want error", in, got)
			}
		})
	}
}

func TestFormatMode(t *testing.T) {
	cases := []struct {
		in   fs.FileMode
		want string
	}{
		{0, "0000"},
		{0o644, "0644"},
		{0o755, "0755"},
		{0o7, "0007"},
		{fs.ModeSetuid | 0o755, "4755"},
		{fs.ModeSetgid | 0o755, "2755"},
		{fs.ModeSticky | 0o777, "1777"},
		{fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky | 0o777, "7777"},
		// File type bits are not part of the mode string.
		{fs.ModeDir | 0o755, "0755"},
		{fs.ModeDir | fs.ModeSticky | 0o1777, "1777"},
		{fs.ModeSymlink | 0o777, "0777"},
	}
	for _, tc := range cases {
		if got := formatMode(tc.in); got != tc.want {
			t.Errorf("formatMode(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseFormatMode_roundTrip(t *testing.T) {
	for v := uint64(0); v <= maxOctalMode; v++ {
		s := strconv.FormatUint(v, 8)
		for len(s) < 4 {
			s = "0" + s
		}
		m, err := parseMode(s)
		if err != nil {
			t.Fatalf("parseMode(%q): %v", s, err)
		}
		if got := formatMode(m); got != s {
			t.Fatalf("formatMode(parseMode(%q)) = %q", s, got)
		}
	}
}

// TestParseMode_chmod verifies parsed special bits survive a round trip
// through the filesystem. The sticky bit on a regular file and setgid/setuid
// are allowed for the file owner, so this does not require root.
func TestParseMode_chmod(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"0640", "4750", "1700"} {
		m, err := parseMode(s)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, m); err != nil {
			t.Fatalf("chmod %s: %v", s, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := formatMode(info.Mode()); got != s {
			t.Errorf("mode after chmod %s = %s", s, got)
		}
	}
}

func TestLookupIDs_numericFallback(t *testing.T) {
	// A numeric ID that is unlikely to name an account is used verbatim.
	if got, err := lookupUID("2000000001"); err != nil || got != 2000000001 {
		t.Errorf("lookupUID numeric = %d, %v", got, err)
	}
	if got, err := lookupGID("2000000001"); err != nil || got != 2000000001 {
		t.Errorf("lookupGID numeric = %d, %v", got, err)
	}

	for _, bad := range []string{"-1", "4294967296", "no-such-user-tfsysutils"} {
		if _, err := lookupUID(bad); err == nil {
			t.Errorf("lookupUID(%q) expected error", bad)
		}
		if _, err := lookupGID(bad); err == nil {
			t.Errorf("lookupGID(%q) expected error", bad)
		}
	}
}

func TestLookupIDs_byName(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Skipf("current user unavailable: %v", err)
	}
	wantUID, _ := strconv.Atoi(u.Uid)
	if got, err := lookupUID(u.Username); err != nil || got != wantUID {
		t.Errorf("lookupUID(%q) = %d, %v; want %d", u.Username, got, err, wantUID)
	}
	g, err := user.LookupGroupId(u.Gid)
	if err != nil {
		t.Skipf("primary group unavailable: %v", err)
	}
	wantGID, _ := strconv.Atoi(g.Gid)
	if got, err := lookupGID(g.Name); err != nil || got != wantGID {
		t.Errorf("lookupGID(%q) = %d, %v; want %d", g.Name, got, err, wantGID)
	}
}

func TestOwnerAndGroup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("syscall.Stat_t unavailable")
	}

	owner, group := ownerAndGroup(st)
	uid, err := lookupUID(owner)
	if err != nil || uint64(uid) != uint64(st.Uid) {
		t.Errorf("owner %q resolves to %d (%v), want %d", owner, uid, err, st.Uid)
	}
	gid, err := lookupGID(group)
	if err != nil || uint64(gid) != uint64(st.Gid) {
		t.Errorf("group %q resolves to %d (%v), want %d", group, gid, err, st.Gid)
	}

	// IDs without an account fall back to their decimal form.
	if got := uidName(2000000001); got != "2000000001" {
		t.Errorf("uidName(unknown) = %q", got)
	}
	if got := gidName(2000000001); got != "2000000001" {
		t.Errorf("gidName(unknown) = %q", got)
	}

	// A configured value that resolves to the actual ID is preserved, whether
	// given as a name or number; a mismatch reports the actual name.
	numericUID := strconv.FormatUint(uint64(st.Uid), 10)
	if got := reconcileOwner(numericUID, st.Uid); got != numericUID {
		t.Errorf("reconcileOwner(%q) = %q", numericUID, got)
	}
	if got := reconcileOwner(owner, st.Uid); got != owner {
		t.Errorf("reconcileOwner(%q) = %q", owner, got)
	}
	if got := reconcileOwner("2000000001", st.Uid); got != owner {
		t.Errorf("reconcileOwner(mismatch) = %q, want %q", got, owner)
	}
	numericGID := strconv.FormatUint(uint64(st.Gid), 10)
	if got := reconcileGroup(numericGID, st.Gid); got != numericGID {
		t.Errorf("reconcileGroup(%q) = %q", numericGID, got)
	}
	if got := reconcileGroup("2000000001", st.Gid); got != group {
		t.Errorf("reconcileGroup(mismatch) = %q, want %q", got, group)
	}
}
