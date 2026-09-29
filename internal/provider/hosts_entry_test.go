package provider

import (
	"context"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestParseHostsLine(t *testing.T) {
	tests := []struct {
		line    string
		ok      bool
		ip      string
		names   []string
		comment string
	}{
		{line: "127.0.0.1\tlocalhost", ok: true, ip: "127.0.0.1", names: []string{"localhost"}},
		{line: "127.0.1.1   host.example.com host", ok: true, ip: "127.0.1.1", names: []string{"host.example.com", "host"}},
		{line: "  10.0.0.5\tdb.internal\tdb  # database", ok: true, ip: "10.0.0.5", names: []string{"db.internal", "db"}, comment: "database"},
		{line: "10.0.0.5 db#no space", ok: true, ip: "10.0.0.5", names: []string{"db"}, comment: "no space"},
		{line: "::1     localhost ip6-localhost ip6-loopback", ok: true, ip: "::1", names: []string{"localhost", "ip6-localhost", "ip6-loopback"}},
		{line: "fe00::0 ip6-localnet", ok: true, ip: "fe00::", names: []string{"ip6-localnet"}},
		{line: "10.0.0.5 db\r", ok: true, ip: "10.0.0.5", names: []string{"db"}},
		{line: ""},
		{line: "   \t"},
		{line: "# 10.0.0.5 db"},
		{line: "10.0.0.5"},
		{line: "10.0.0.5 # only a comment"},
		{line: "not-an-ip host"},
		{line: "10.0.0 host"},
		{line: "fe80::1%eth0 host"},
	}
	for _, tt := range tests {
		hl, ok := parseHostsLine(tt.line)
		if ok != tt.ok {
			t.Errorf("parseHostsLine(%q) ok = %v, want %v", tt.line, ok, tt.ok)
			continue
		}
		if !ok {
			continue
		}
		if hl.addr != netip.MustParseAddr(tt.ip) || !slices.Equal(hl.names, tt.names) || hl.comment != tt.comment {
			t.Errorf("parseHostsLine(%q) = %s %q # %q, want %s %q # %q", tt.line, hl.addr, hl.names, hl.comment, tt.ip, tt.names, tt.comment)
		}
		// An unchanged line renders byte for byte, except for trailing
		// white space without a comment.
		if got := hl.render(); got != tt.line {
			t.Errorf("render(parseHostsLine(%q)) = %q", tt.line, got)
		}
	}
}

func TestHostsEntryLine(t *testing.T) {
	e := &hostsEntry{ipText: "10.0.0.5", addr: netip.MustParseAddr("10.0.0.5"), names: []string{"db.internal", "db"}}
	if got, want := e.line("\t"), "10.0.0.5\tdb.internal db"; got != want {
		t.Errorf("line = %q, want %q", got, want)
	}
	e.comment = "managed by Terraform"
	if got, want := e.line(" "), "10.0.0.5 db.internal db # managed by Terraform"; got != want {
		t.Errorf("line = %q, want %q", got, want)
	}
	v6 := &hostsEntry{ipText: "fd00::5", addr: netip.MustParseAddr("fd00::5"), names: []string{"db6"}}
	if got, want := v6.line("\t"), "fd00::5\tdb6"; got != want {
		t.Errorf("line = %q, want %q", got, want)
	}
}

func TestValidateHostname(t *testing.T) {
	valid := []string{"a", "localhost", "db.internal", "3com.com", "ip6-localhost", "Host-1.Example.COM",
		strings.Repeat("a", 63) + ".example", strings.Repeat("a.", 126) + "a"}
	for _, s := range valid {
		if err := validateHostname(s); err != nil {
			t.Errorf("validateHostname(%q) = %v, want nil", s, err)
		}
	}
	invalid := []string{"", ".", "a..b", ".a", "a.", "-a", "a-", "a.-b", "a_b", "a b", "a#b", "ä.example", "10.0.0.5", "::1",
		strings.Repeat("a", 64), strings.Repeat("a.", 127) + "a"}
	for _, s := range invalid {
		if err := validateHostname(s); err == nil {
			t.Errorf("validateHostname(%q) = nil, want an error", s)
		}
	}
}

func TestValidateHostsIPAndComment(t *testing.T) {
	for _, s := range []string{"10.0.0.5", "::1", "fd00::5", "2001:DB8::1", "::ffff:10.0.0.1"} {
		if err := validateHostsIP(s); err != nil {
			t.Errorf("validateHostsIP(%q) = %v", s, err)
		}
	}
	for _, s := range []string{"", "10.0.0", "010.0.0.5", "10.0.0.256", "10.0.0.5/32", "fe80::1%eth0", "db", " 10.0.0.5"} {
		if err := validateHostsIP(s); err == nil {
			t.Errorf("validateHostsIP(%q) = nil, want an error", s)
		}
	}
	for _, s := range []string{"x", "managed by Terraform", "a # b"} {
		if err := validateHostsComment(s); err != nil {
			t.Errorf("validateHostsComment(%q) = %v", s, err)
		}
	}
	for _, s := range []string{"", " x", "x ", "a\nb", "a\rb"} {
		if err := validateHostsComment(s); err == nil {
			t.Errorf("validateHostsComment(%q) = nil, want an error", s)
		}
	}
	if dup, ok := duplicateHostname([]string{"db", "web", "DB"}); !ok || dup != "DB" {
		t.Errorf("duplicateHostname = %q, %v", dup, ok)
	}
	if _, ok := duplicateHostname([]string{"db", "web"}); ok {
		t.Error("duplicateHostname found a duplicate in distinct names")
	}
}

// editHosts applies ensureHostsEntry for e, managing lines with the given
// identities (or e's own if none), to content and returns the result.
func editHosts(t *testing.T, content string, e *hostsEntry, ids ...hostsIdentity) (string, bool) {
	t.Helper()
	if len(ids) == 0 {
		ids = []hostsIdentity{{addr: e.addr, canonical: e.names[0]}}
	}
	text := parseTextFile([]byte(content))
	changed := ensureHostsEntry(text, findHostsEntries(text.lines, ids...), e)
	return string(text.bytes()), changed
}

func newTestHostsEntry(ip, comment string, names ...string) *hostsEntry {
	return &hostsEntry{ipText: ip, addr: netip.MustParseAddr(ip), names: names, comment: comment}
}

func TestEnsureHostsEntry(t *testing.T) {
	const base = "# The following lines are desirable\n127.0.0.1\tlocalhost\n\n::1\tlocalhost ip6-localhost\n"
	tests := []struct {
		name    string
		content string
		entry   *hostsEntry
		ids     []hostsIdentity
		want    string
		changed bool
	}{
		{
			name:    "append with the file's tab",
			content: base,
			entry:   newTestHostsEntry("10.0.0.5", "", "db.internal", "db"),
			want:    base + "10.0.0.5\tdb.internal db\n",
			changed: true,
		},
		{
			name:    "append with the file's spaces",
			content: "127.0.0.1   localhost\n",
			entry:   newTestHostsEntry("10.0.0.5", "note", "db"),
			want:    "127.0.0.1   localhost\n10.0.0.5   db # note\n",
			changed: true,
		},
		{
			name:    "empty file",
			content: "",
			entry:   newTestHostsEntry("fd00::5", "", "db6"),
			want:    "fd00::5\tdb6\n",
			changed: true,
		},
		{
			name:    "no final newline",
			content: "127.0.0.1 localhost",
			entry:   newTestHostsEntry("10.0.0.5", "", "db"),
			want:    "127.0.0.1 localhost\n10.0.0.5 db\n",
			changed: true,
		},
		{
			name:    "crlf",
			content: "127.0.0.1 localhost\r\n",
			entry:   newTestHostsEntry("10.0.0.5", "", "db"),
			want:    "127.0.0.1 localhost\r\n10.0.0.5 db\r\n",
			changed: true,
		},
		{
			name:    "same address, other canonical name is left alone",
			content: base,
			entry:   newTestHostsEntry("127.0.0.1", "", "app.local"),
			want:    base + "127.0.0.1\tapp.local\n",
			changed: true,
		},
		{
			name:    "unchanged entry keeps its formatting",
			content: "  10.0.0.5 \t db.internal\tdb   #   note \n",
			entry:   newTestHostsEntry("10.0.0.5", "note", "db.internal", "db"),
			want:    "  10.0.0.5 \t db.internal\tdb   #   note \n",
		},
		{
			name:    "alias added, separators kept",
			content: "a\n10.0.0.5\t\tdb.internal\tdb # note\nb\n",
			entry:   newTestHostsEntry("10.0.0.5", "note", "db.internal", "db", "postgres"),
			want:    "a\n10.0.0.5\t\tdb.internal\tdb\tpostgres # note\nb\n",
			changed: true,
		},
		{
			name:    "alias added to a single name",
			content: "10.0.0.5\tdb.internal\n",
			entry:   newTestHostsEntry("10.0.0.5", "", "db.internal", "db"),
			want:    "10.0.0.5\tdb.internal db\n",
			changed: true,
		},
		{
			name:    "comment changed",
			content: "10.0.0.5 db #old\n",
			entry:   newTestHostsEntry("10.0.0.5", "new", "db"),
			want:    "10.0.0.5 db # new\n",
			changed: true,
		},
		{
			name:    "comment removed",
			content: "10.0.0.5 db\t# old\n",
			entry:   newTestHostsEntry("10.0.0.5", "", "db"),
			want:    "10.0.0.5 db\n",
			changed: true,
		},
		{
			name:    "canonical name compared without case",
			content: "10.0.0.5 DB.internal\n",
			entry:   newTestHostsEntry("10.0.0.5", "", "db.internal"),
			want:    "10.0.0.5 db.internal\n",
			changed: true,
		},
		{
			name:    "ipv6 spelled differently keeps its spelling",
			content: "fd00:0:0:0:0:0:0:5 db6\n",
			entry:   newTestHostsEntry("fd00::5", "", "db6"),
			want:    "fd00:0:0:0:0:0:0:5 db6\n",
		},
		{
			name:    "duplicates of the entry are removed",
			content: "10.0.0.5 db\n127.0.0.1 localhost\n10.0.0.5 DB old\n",
			entry:   newTestHostsEntry("10.0.0.5", "", "db"),
			want:    "10.0.0.5 db\n127.0.0.1 localhost\n",
			changed: true,
		},
		{
			name:    "crlf line updated keeps its CR",
			content: "10.0.0.5 db\r\n",
			entry:   newTestHostsEntry("10.0.0.5", "", "db", "x"),
			want:    "10.0.0.5 db x\r\n",
			changed: true,
		},
		{
			name:    "address changed in place",
			content: "a\n10.0.0.5\tdb\nb\n",
			entry:   newTestHostsEntry("10.0.0.6", "", "db"),
			ids:     []hostsIdentity{{addr: netip.MustParseAddr("10.0.0.6"), canonical: "db"}, {addr: netip.MustParseAddr("10.0.0.5"), canonical: "db"}},
			want:    "a\n10.0.0.6\tdb\nb\n",
			changed: true,
		},
		{
			name:    "canonical name changed in place",
			content: "a\n10.0.0.5\tdb\nb\n",
			entry:   newTestHostsEntry("10.0.0.5", "", "database", "db"),
			ids:     []hostsIdentity{{addr: netip.MustParseAddr("10.0.0.5"), canonical: "database"}, {addr: netip.MustParseAddr("10.0.0.5"), canonical: "db"}},
			want:    "a\n10.0.0.5\tdatabase db\nb\n",
			changed: true,
		},
		{
			name:    "commented-out entry is not an entry",
			content: "#10.0.0.5 db\n",
			entry:   newTestHostsEntry("10.0.0.5", "", "db"),
			want:    "#10.0.0.5 db\n10.0.0.5\tdb\n",
			changed: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changed := editHosts(t, tt.content, tt.entry, tt.ids...)
			if got != tt.want || changed != tt.changed {
				t.Errorf("got %q (changed %v), want %q (changed %v)", got, changed, tt.want, tt.changed)
			}
		})
	}
}

func TestEnsureHostsEntryCRLFNoFinalNewline(t *testing.T) {
	got, _ := editHosts(t, "127.0.0.1 localhost\r\n::1 localhost", newTestHostsEntry("10.0.0.5", "", "db"))
	if want := "127.0.0.1 localhost\r\n::1 localhost\r\n10.0.0.5 db\r\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRemoveHostsEntries(t *testing.T) {
	const content = "# header\n127.0.0.1 localhost\n\n10.0.0.5\tdb # x\n10.0.0.5 other\n# trailer\n10.0.0.5 DB\n"
	text := parseTextFile([]byte(content))
	id := hostsIdentity{addr: netip.MustParseAddr("10.0.0.5"), canonical: "db"}
	if !removeHostsEntries(text, findHostsEntries(text.lines, id)) {
		t.Fatal("removeHostsEntries reported no change")
	}
	if got, want := string(text.bytes()), "# header\n127.0.0.1 localhost\n\n10.0.0.5 other\n# trailer\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if removeHostsEntries(text, findHostsEntries(text.lines, id)) {
		t.Error("second removeHostsEntries reported a change")
	}
}

func TestHostsConflicts(t *testing.T) {
	lines := parseTextFile([]byte("127.0.0.1 localhost\n::1 localhost\n10.0.0.9 db.internal DB\n10.0.0.5 db\n# 10.0.0.8 db\nfd00::9 db\n")).lines
	addr := netip.MustParseAddr("10.0.0.5")
	got := hostsConflicts(lines, addr, []string{"db", "localhost"}, nil)
	// 127.0.0.1 localhost is a conflict (same family); ::1 localhost and
	// fd00::9 db are not, nor is the line with the entry's own address.
	want := []hostsConflict{{line: 1, name: "localhost", ipText: "127.0.0.1"}, {line: 3, name: "DB", ipText: "10.0.0.9"}}
	if !slices.Equal(got, want) {
		t.Errorf("hostsConflicts = %v, want %v", got, want)
	}
	if got := hostsConflicts(lines, addr, []string{"db"}, []int{2}); len(got) != 0 {
		t.Errorf("hostsConflicts with skipped line = %v, want none", got)
	}
	v6 := hostsConflicts(lines, netip.MustParseAddr("fd00::5"), []string{"db", "localhost"}, nil)
	want6 := []hostsConflict{{line: 2, name: "localhost", ipText: "::1"}, {line: 6, name: "db", ipText: "fd00::9"}}
	if !slices.Equal(v6, want6) {
		t.Errorf("hostsConflicts(v6) = %v, want %v", v6, want6)
	}
}

func TestFindHostsAddr(t *testing.T) {
	lines := parseTextFile([]byte("# 10.0.0.5 x\n127.0.0.1 localhost\nfd00:0::5 a\n10.0.0.5 b\n10.0.0.5 c\n")).lines
	for _, tt := range []struct {
		addr string
		want []int
	}{
		{"fd00::5", []int{2}},
		{"10.0.0.5", []int{3, 4}},
		{"10.0.0.6", nil},
	} {
		if got := findHostsAddr(lines, netip.MustParseAddr(tt.addr)); !slices.Equal(got, tt.want) {
			t.Errorf("findHostsAddr(%s) = %v, want %v", tt.addr, got, tt.want)
		}
	}
}

// TestHostsEntryRefreshImportWarning checks that importing an address that
// several lines map warns and picks the first line.
func TestHostsEntryRefreshImportWarning(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hosts")
	mustWrite(t, p, "127.0.0.1 localhost\n127.0.0.1 app.local # app\n")
	r := &hostsEntryResource{}
	m := hostsEntryModel{
		Path:           types.StringValue(p),
		IP:             types.StringValue("127.0.0.1"),
		Hostnames:      types.ListNull(types.StringType),
		Comment:        types.StringNull(),
		AllowDuplicate: types.BoolValue(false),
	}
	found, diags := r.refresh(context.Background(), &m)
	if !found || diags.HasError() {
		t.Fatalf("refresh = %v, %v", found, diags)
	}
	if diags.WarningsCount() != 1 || !strings.Contains(diags.Warnings()[0].Detail(), ":127.0.0.1,<hostname>") {
		t.Errorf("warnings = %v, want one naming the <path>:<ip>,<hostname> form", diags.Warnings())
	}
	if got := m.Hostnames.String(); got != `["localhost"]` {
		t.Errorf("hostnames = %s, want the first line's", got)
	}

	// With a canonical name, the matching line is found without a warning.
	m.Hostnames = stringList([]string{"APP.local"})
	found, diags = r.refresh(context.Background(), &m)
	if !found || len(diags) != 0 {
		t.Fatalf("refresh = %v, %v", found, diags)
	}
	if got := m.Hostnames.String(); got != `["app.local"]` || m.Comment.ValueString() != "app" {
		t.Errorf("hostnames = %s, comment = %s", got, m.Comment)
	}
}
