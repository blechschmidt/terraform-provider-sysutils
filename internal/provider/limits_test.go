package provider

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateLimitsDomain(t *testing.T) {
	valid := []string{
		"postgres", "www-data", "svc.app", "_apt", "HOST$", "a1",
		"@developers", "@www-data",
		"*", "%", "%admins", "%:100",
		"1000:1999", "1000:", ":1000", "0:0",
		"@1000:1999", "@1000:", "@:100",
	}
	for _, d := range valid {
		if err := validateLimitsDomain(d); err != nil {
			t.Errorf("validateLimitsDomain(%q) = %v, want nil", d, err)
		}
	}
	invalid := map[string]string{
		"":                       "empty",
		"-rf":                    "must not start",
		"a b":                    "character",
		"al/ice":                 "character",
		"..":                     "not a valid name",
		"1000":                   "numeric",
		"@100":                   "numeric",
		"@":                      "empty",
		"%1000":                  "numeric",
		":":                      "minimum, a maximum or both",
		"@:":                     "minimum, a maximum or both",
		"2000:1000":              "greater than",
		"1000:x":                 "invalid maximum",
		"-1:5":                   "invalid minimum",
		"1:2:3":                  "invalid maximum",
		"4294967295:":            "out of range",
		"99999999999:":           "out of range",
		"%1:2":                   "\"%:<gid>\"",
		"%:":                     "invalid GID",
		"**":                     "whole domain",
		"*x":                     "whole domain",
		"al#ice":                 "character",
		"user\n":                 "character",
		strings.Repeat("a", 256): "at most",
	}
	for d, want := range invalid {
		err := validateLimitsDomain(d)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("validateLimitsDomain(%q) = %v, want error containing %q", d, err, want)
		}
	}
}

func TestValidateLimitsTypeAndItem(t *testing.T) {
	for _, typ := range []string{"soft", "hard", "-"} {
		if err := validateLimitsType(typ); err != nil {
			t.Errorf("validateLimitsType(%q) = %v", typ, err)
		}
	}
	for _, typ := range []string{"", "Soft", "both", "--"} {
		if validateLimitsType(typ) == nil {
			t.Errorf("validateLimitsType(%q) = nil, want error", typ)
		}
	}
	for _, item := range []string{"nofile", "nproc", "memlock", "core", "maxlogins", "nice", "nonewprivs", "rtprio"} {
		if err := validateLimitsItem(item); err != nil {
			t.Errorf("validateLimitsItem(%q) = %v", item, err)
		}
	}
	for _, item := range []string{"", "NOFILE", "chroot", "files"} {
		if err := validateLimitsItem(item); err == nil || !strings.Contains(err.Error(), "nofile") {
			t.Errorf("validateLimitsItem(%q) = %v, want error listing the items", item, err)
		}
	}
}

func TestValidateLimitsValue(t *testing.T) {
	tests := []struct {
		item, value string
		want        string // Substring of the error; empty for valid.
	}{
		{"nofile", "65536", ""},
		{"nofile", "0", ""},
		{"nofile", "unlimited", ""},
		{"memlock", "infinity", ""},
		{"core", "-1", ""},
		{"nproc", "18446744073709551615", ""},
		{"nproc", "18446744073709551616", "too large"},
		{"nofile", "-2", "non-negative"},
		{"nofile", "1k", "non-negative"},
		{"nofile", "+5", "non-negative"},
		{"nofile", "Unlimited", "non-negative"},
		{"nofile", "", "empty"},
		{"nofile", "1 2", "single field"},
		{"nofile", "1#2", "single field"},
		{"nofile", "1\t", "single field"},
		{"maxlogins", "4", ""},
		{"nice", "-20", ""},
		{"nice", "19", ""},
		{"priority", "0", ""},
		{"nice", "20", "-20 to 19"},
		{"nice", "-21", "-20 to 19"},
		{"nice", "unlimited", "-20 to 19"},
		{"nonewprivs", "1", ""},
		{"nonewprivs", "0", ""},
		{"nonewprivs", "2", "0 or 1"},
		{"nonewprivs", "unlimited", "0 or 1"},
	}
	for _, tc := range tests {
		err := validateLimitsValue(tc.item, tc.value)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("validateLimitsValue(%q, %q) = %v, want nil", tc.item, tc.value, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("validateLimitsValue(%q, %q) = %v, want error containing %q", tc.item, tc.value, err, tc.want)
		}
	}
}

func TestValidateLimitsDomainItem(t *testing.T) {
	if err := validateLimitsDomainItem("%admins", "maxlogins"); err != nil {
		t.Error(err)
	}
	if err := validateLimitsDomainItem("%", "maxsyslogins"); err != nil {
		t.Error(err)
	}
	if err := validateLimitsDomainItem("%:100", "nofile"); err == nil {
		t.Error("\"%:100\" with nofile accepted")
	}
	if err := validateLimitsDomainItem("@admins", "nofile"); err != nil {
		t.Error(err)
	}
}

func TestLimitsValuesEqual(t *testing.T) {
	equal := [][2]string{
		{"unlimited", "infinity"}, {"-1", "unlimited"}, {"0100", "100"}, {"-05", "-5"}, {"18446744073709551615", "018446744073709551615"},
	}
	for _, p := range equal {
		if !limitsValuesEqual(p[0], p[1]) {
			t.Errorf("limitsValuesEqual(%q, %q) = false", p[0], p[1])
		}
	}
	for _, p := range [][2]string{{"1", "10"}, {"0", "unlimited"}, {"abc", "abd"}} {
		if limitsValuesEqual(p[0], p[1]) {
			t.Errorf("limitsValuesEqual(%q, %q) = true", p[0], p[1])
		}
	}
}

func TestValidateLimitsFileName(t *testing.T) {
	for _, n := range []string{"90-terraform.conf", "a.conf", "x_y+z@1.conf", "20-nproc.conf"} {
		if err := validateLimitsFileName(n); err != nil {
			t.Errorf("validateLimitsFileName(%q) = %v", n, err)
		}
	}
	invalid := map[string]string{
		"":                                 "empty",
		".conf":                            "must not start",
		".hidden.conf":                     "must not start",
		"-x.conf":                          "must not start",
		"limits":                           "must end in",
		"limits.conf.bak":                  "must end in",
		"../limits.conf":                   "without \"/\"",
		"../../../etc/shadow":              "without \"/\"",
		"sub/x.conf":                       "without \"/\"",
		"a b.conf":                         "character",
		"a\x00.conf":                       "character",
		strings.Repeat("a", 251) + ".conf": "at most",
	}
	for n, want := range invalid {
		err := validateLimitsFileName(n)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("validateLimitsFileName(%q) = %v, want error containing %q", n, err, want)
		}
	}
}

func TestLimitsFileNameFromPath(t *testing.T) {
	name, err := limitsFileNameFromPath("/etc/security/limits.d/90-x.conf")
	if err != nil || name != "90-x.conf" {
		t.Errorf("got %q, %v", name, err)
	}
	for _, p := range []string{
		"/etc/security/limits.conf",
		"/etc/security/limits.d/../limits.conf",
		"/etc/security/limits.d//x.conf",
		"/etc/security/limits.d/sub/x.conf",
		"etc/security/limits.d/x.conf",
		"/etc/security/limits.d/",
		"/etc/security/limits.d/x",
	} {
		if _, err := limitsFileNameFromPath(p); err == nil {
			t.Errorf("limitsFileNameFromPath(%q) accepted", p)
		}
	}
}

func TestDefaultLimitsFileName(t *testing.T) {
	tests := map[string]string{
		"postgres":    "90-terraform-user-postgres.conf",
		"HOST$":       "90-terraform-user-HOST.conf",
		"@developers": "90-terraform-group-developers.conf",
		"*":           "90-terraform-default.conf",
		"%":           "90-terraform-logins.conf",
		"%admins":     "90-terraform-logins-group-admins.conf",
		"%:100":       "90-terraform-logins-gid-100.conf",
		"1000:1999":   "90-terraform-uid-1000-1999.conf",
		"1000:":       "90-terraform-uid-1000.conf",
		":1000":       "90-terraform-uid-1000.conf",
		"@100:200":    "90-terraform-gid-100-200.conf",
	}
	for domain, want := range tests {
		got := defaultLimitsFileName(domain)
		if got != want {
			t.Errorf("defaultLimitsFileName(%q) = %q, want %q", domain, got, want)
		}
		if err := validateLimitsFileName(got); err != nil {
			t.Errorf("defaultLimitsFileName(%q) = %q is invalid: %v", domain, got, err)
		}
	}
	long := strings.Repeat("a", maxLimitsNameLen)
	if got := defaultLimitsFileName(long); validateLimitsFileName(got) != nil {
		t.Errorf("default file name for a long user name is invalid: %q", got)
	}
}

func TestParseLimitsLine(t *testing.T) {
	tests := []struct {
		line string
		want limitsEntry
		ok   bool
	}{
		{"postgres soft nofile 65536", limitsEntry{"postgres", "soft", "nofile", "65536"}, true},
		{"  @dev\t-\tnproc   unlimited  ", limitsEntry{"@dev", "-", "nproc", "unlimited"}, true},
		{"* hard core 0 # no core dumps", limitsEntry{"*", "hard", "core", "0"}, true},
		{"* hard core 0 extra", limitsEntry{"*", "hard", "core", "0"}, true},
		{"*#hard core 0", limitsEntry{}, false},
		{"# postgres soft nofile 65536", limitsEntry{}, false},
		{"postgres soft nofile", limitsEntry{}, false},
		{"", limitsEntry{}, false},
	}
	for _, tc := range tests {
		got, ok := parseLimitsLine(tc.line)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseLimitsLine(%q) = %+v, %v, want %+v, %v", tc.line, got, ok, tc.want, tc.ok)
		}
	}
}

const testLimitsFile = "# Limits for the database.\n" +
	"postgres   soft   nofile   4096\n" +
	"\n" +
	"postgres   hard   nofile   8192 # ceiling\n" +
	"*          soft   core     0\n"

func TestSetLimitsEntry(t *testing.T) {
	soft := limitsKey{"postgres", "soft", "nofile"}
	tests := []struct {
		name    string
		in      string
		entry   limitsEntry
		want    string
		changed bool
	}{
		{
			name:    "replace in place",
			in:      testLimitsFile,
			entry:   limitsEntry{"postgres", "soft", "nofile", "65536"},
			want:    strings.Replace(testLimitsFile, "postgres   soft   nofile   4096", "postgres        soft  nofile          65536", 1),
			changed: true,
		},
		{
			name:    "equal value kept as written",
			in:      testLimitsFile,
			entry:   limitsEntry{"postgres", "soft", "nofile", "04096"},
			want:    testLimitsFile,
			changed: false,
		},
		{
			name:    "append",
			in:      testLimitsFile,
			entry:   limitsEntry{"@dev", "-", "nproc", "unlimited"},
			want:    testLimitsFile + "@dev            -     nproc           unlimited\n",
			changed: true,
		},
		{
			name:    "append after unterminated last line",
			in:      "* soft core 0",
			entry:   limitsEntry{"*", "hard", "core", "0"},
			want:    "* soft core 0\n*               hard  core            0\n",
			changed: true,
		},
		{
			name:    "duplicates removed",
			in:      "postgres soft nofile 1\n# x\npostgres soft nofile 2\npostgres soft nofile 3\n",
			entry:   limitsEntry{"postgres", "soft", "nofile", "1"},
			want:    "postgres soft nofile 1\n# x\n",
			changed: true,
		},
		{
			name:    "commented entry is not an entry",
			in:      "#postgres soft nofile 1\n",
			entry:   limitsEntry{"postgres", "soft", "nofile", "1"},
			want:    "#postgres soft nofile 1\npostgres        soft  nofile          1\n",
			changed: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tf := parseTextFile([]byte(tc.in))
			if changed := setLimitsEntry(tf, tc.entry); changed != tc.changed {
				t.Errorf("changed = %v, want %v", changed, tc.changed)
			}
			if got := string(tf.bytes()); got != tc.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}

	tf := parseTextFile([]byte(testLimitsFile))
	if v, n := lookupLimitsEntry(tf, soft); v != "4096" || n != 1 {
		t.Errorf("lookupLimitsEntry = %q, %d", v, n)
	}
	if !removeLimitsEntries(tf, soft) {
		t.Error("removeLimitsEntries reported no change")
	}
	want := strings.Replace(testLimitsFile, "postgres   soft   nofile   4096\n", "", 1)
	if got := string(tf.bytes()); got != want {
		t.Errorf("after removal got:\n%s\nwant:\n%s", got, want)
	}
	if removeLimitsEntries(tf, soft) {
		t.Error("second removeLimitsEntries reported a change")
	}
	// The last entry wins, as in pam_limits.
	tf = parseTextFile([]byte("a soft nofile 1\na soft nofile 2\n"))
	if v, n := lookupLimitsEntry(tf, limitsKey{"a", "soft", "nofile"}); v != "2" || n != 2 {
		t.Errorf("lookupLimitsEntry with duplicates = %q, %d", v, n)
	}
}

func TestEditLimitsFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "limits.d", "90-test.conf")
	a := limitsEntry{"alice", "soft", "nofile", "1024"}
	b := limitsEntry{"alice", "hard", "nofile", "2048"}
	set := func(e limitsEntry) func(*textFile) bool {
		return func(t *textFile) bool { return setLimitsEntry(t, e) }
	}
	remove := func(e limitsEntry) func(*textFile) bool {
		return func(t *textFile) bool { return removeLimitsEntries(t, e.key()) }
	}
	read := func() string {
		t.Helper()
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	// Removing from a missing file creates nothing.
	if err := editLimitsFile(p, remove(a)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Dir(p)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("directory created for a removal: %v", err)
	}

	// A new file, and its directory, are created with the header.
	if err := editLimitsFile(p, set(a)); err != nil {
		t.Fatal(err)
	}
	if got, want := read(), limitsFileHeader+"\n"+a.line()+"\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != limitsFileMode {
		t.Errorf("mode %v, want %v", info.Mode().Perm(), limitsFileMode)
	}
	if err := editLimitsFile(p, set(b)); err != nil {
		t.Fatal(err)
	}
	if got, want := read(), limitsFileHeader+"\n"+a.line()+"\n"+b.line()+"\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	// The file stays while an entry is left, and is removed with the last.
	if err := editLimitsFile(p, remove(a)); err != nil {
		t.Fatal(err)
	}
	if got, want := read(), limitsFileHeader+"\n"+b.line()+"\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if err := editLimitsFile(p, remove(b)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("file created by the provider not removed with its last entry: %v", err)
	}

	// A comment someone added keeps the file.
	if err := editLimitsFile(p, set(a)); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, p, read()+"# keep me\n")
	if err := editLimitsFile(p, remove(a)); err != nil {
		t.Fatal(err)
	}
	if got, want := read(), limitsFileHeader+"\n# keep me\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	// A file the provider did not create is never removed, even if empty,
	// and keeps its mode.
	q := filepath.Join(dir, "limits.d", "10-admin.conf")
	mustWrite(t, q, "alice soft nofile 1024\n")
	mustChmod(t, q, 0o600)
	if err := editLimitsFile(q, remove(a)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(q)
	if err != nil || len(data) != 0 {
		t.Fatalf("pre-existing file: %q, %v; want empty file", data, err)
	}
	if info, err := os.Stat(q); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("pre-existing file mode not kept: %v, %v", info, err)
	}

	// Symlinks are refused rather than written through.
	target := filepath.Join(dir, "target")
	mustWrite(t, target, "")
	link := filepath.Join(dir, "limits.d", "20-link.conf")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := editLimitsFile(link, set(a)); err == nil {
		t.Fatal("wrote through a symlink")
	}
	if data, _ := os.ReadFile(target); len(data) != 0 {
		t.Fatalf("symlink target modified: %q", data)
	}
}
