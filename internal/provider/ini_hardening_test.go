package provider

// Regression tests for the security review of the ini_value editor.

import (
	"slices"
	"strings"
	"testing"
)

// A UTF-8 byte order mark, which Windows editors write, must not hide the
// first line: otherwise "[Service]" on the first line is taken for a key
// of the global section, the section is appended a second time, and a
// global key is inserted into [Service].
func TestIniByteOrderMark(t *testing.T) {
	const bom = "\xef\xbb\xbf"
	for _, tc := range []struct {
		name, section, key, value, in, want string
	}{
		{"existing section", "Service", "User", "b", bom + "[Service]\r\nUser=a\r\n", bom + "[Service]\r\nUser = b\r\n"},
		{"new key in section", "Service", "Group", "b", bom + "[Service]\nUser=a", bom + "[Service]\nUser=a\nGroup = b\n"},
		{"global key before first header", "", "root", "true", bom + "[*]\nindent = 2\n", bom + "root = true\n\n[*]\nindent = 2\n"},
		{"global key on first line", "", "root", "false", bom + "root = true\n[*]\n", bom + "root = false\n[*]\n"},
		{"only a BOM", "s", "k", "v", bom, bom + "[s]\nk = v\n"},
	} {
		spec, err := newIniSpec(tc.section, tc.key, defaultIniSeparator)
		if err != nil {
			t.Fatal(err)
		}
		f := parseIniFile([]byte(tc.in))
		if _, err := spec.ensure(f, tc.value); err != nil {
			t.Fatal(err)
		}
		if got := string(f.bytes()); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
		if got := spec.values(parseIniFile(f.bytes())); !slices.Equal(got, []string{tc.value}) {
			t.Errorf("%s: values %q", tc.name, got)
		}
	}

	// Removing the first line keeps the mark at the start of the file.
	spec, _ := newIniSpec("", "root", defaultIniSeparator)
	f := parseIniFile([]byte(bom + "root = true\r\n[*]\r\nindent = 2"))
	if got := spec.values(f); !slices.Equal(got, []string{"true"}) {
		t.Fatalf("values %q", got)
	}
	if !spec.remove(f) {
		t.Fatal("key not removed")
	}
	if got, want := string(f.bytes()), bom+"[*]\r\nindent = 2"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Edits leave everything but the managed key byte for byte intact:
// CRLF line endings, a missing final newline, duplicate sections and keys,
// comments and odd spacing.
func TestIniEditKeepsRestOfFile(t *testing.T) {
	in := "; c\r\n[a]\r\nk = 1\r\nx=  y  \r\n[b]\r\nk=2\r\n[a]\r\nk = 3\r\n#k = 4\r\nz = \"q\" ; c"
	spec, _ := newIniSpec("a", "k", defaultIniSeparator)
	f := parseIniFile([]byte(in))
	if got := spec.values(f); !slices.Equal(got, []string{"1", "3"}) {
		t.Fatalf("values %q", got)
	}
	if _, err := spec.ensure(f, "5"); err != nil {
		t.Fatal(err)
	}
	want := "; c\r\n[a]\r\nk = 5\r\nx=  y  \r\n[b]\r\nk=2\r\n[a]\r\n#k = 4\r\nz = \"q\" ; c"
	if got := string(f.bytes()); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Section names, keys and values that would add lines or sections, or be
// read back as something else, are refused.
func TestIniRefusesInjection(t *testing.T) {
	for _, s := range []string{"a]\n[b", "a\r", "a]", "[a", " a"} {
		if validateIniSection(s) == nil {
			t.Errorf("section %q accepted", s)
		}
	}
	for _, k := range []string{"k\nx", "k\r", "[k", "#k", ";k", "k = v", " k"} {
		if validateIniKey(k, defaultIniSeparator) == nil {
			t.Errorf("key %q accepted", k)
		}
	}
	for _, v := range []string{"v\n[evil]", "v\r\nx = 1", "v\r", " v"} {
		if validateIniValue(v) == nil {
			t.Errorf("value %q accepted", v)
		}
	}
	// Values that look like comments, sections or assignments are written
	// literally and read back unchanged.
	spec, _ := newIniSpec("s", "k", defaultIniSeparator)
	for _, v := range []string{"[evil]", "#x", "; y", "a = b", "x]"} {
		f := parseIniFile([]byte("[s]\n"))
		if _, err := spec.ensure(f, v); err != nil {
			t.Fatal(err)
		}
		if got := spec.values(parseIniFile(f.bytes())); !slices.Equal(got, []string{v}) {
			t.Errorf("value %q read back as %q", v, got)
		}
		if n := strings.Count(string(f.bytes()), "\n"); n != 2 {
			t.Errorf("value %q: %q", v, f.bytes())
		}
	}
}
