package provider

import (
	"slices"
	"strings"
	"testing"
)

func mustIniSpec(t *testing.T, section, key, separator string) *iniSpec {
	t.Helper()
	s, err := newIniSpec(section, key, separator)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestParseIniFileRoundTrip(t *testing.T) {
	for _, in := range []string{
		"",
		"a=1",
		"a=1\n",
		"a=1\r\n[s]\r\nb = 2\r\n",
		"a=1\r\nb=2\nc=3",
		"\n\n",
		"\r\n",
		"a=1\r", // A lone "\r" at the end is content, not a terminator.
	} {
		if got := string(parseIniFile([]byte(in)).bytes()); got != in {
			t.Errorf("round trip of %q = %q", in, got)
		}
	}
}

func TestIniParseLine(t *testing.T) {
	eq := newIniSyntax(" = ")
	ws := newIniSyntax(" ")
	colon := newIniSyntax(": ")
	for _, tc := range []struct {
		syntax  iniSyntax
		text    string
		want    iniParsedLine
		comment string
	}{
		{eq, "", iniParsedLine{kind: iniBlank}, ""},
		{eq, " \t", iniParsedLine{kind: iniBlank}, ""},
		{eq, "key = value", iniParsedLine{kind: iniKey, key: "key", value: "value"}, ""},
		{eq, "\tkey=value  ", iniParsedLine{kind: iniKey, key: "key", value: "value"}, "indented, no spaces"},
		{eq, "key = a = b", iniParsedLine{kind: iniKey, key: "key", value: "a = b"}, "split at first delimiter"},
		{eq, "key =", iniParsedLine{kind: iniKey, key: "key", value: ""}, "empty value"},
		{eq, "skip-name-resolve", iniParsedLine{kind: iniKey, key: "skip-name-resolve"}, "bare key"},
		{eq, `name = "x" ; note`, iniParsedLine{kind: iniKey, key: "name", value: `"x" ; note`}, "values are literal"},
		{eq, "Some Key = v", iniParsedLine{kind: iniKey, key: "Some Key", value: "v"}, "space in key"},
		{eq, "# key = old", iniParsedLine{kind: iniComment, key: "key"}, "commented-out key"},
		{eq, ";extension=gd", iniParsedLine{kind: iniComment, key: "extension"}, "semicolon comment"},
		{eq, "#", iniParsedLine{kind: iniComment}, ""},
		{eq, "# [section]", iniParsedLine{kind: iniComment}, "commented header is no key"},
		{eq, "[Service]", iniParsedLine{kind: iniSection, section: "Service"}, ""},
		{eq, ` [ remote "origin" ] # comment`, iniParsedLine{kind: iniSection, section: `remote "origin"`}, ""},
		{eq, "[]", iniParsedLine{kind: iniSection, section: ""}, ""},
		{eq, "[bad", iniParsedLine{kind: iniOther}, "unterminated header"},
		{eq, "[a] junk", iniParsedLine{kind: iniOther}, "junk after header"},
		{ws, "Port 22", iniParsedLine{kind: iniKey, key: "Port", value: "22"}, ""},
		{ws, "  AllowUsers\talice bob", iniParsedLine{kind: iniKey, key: "AllowUsers", value: "alice bob"}, ""},
		{ws, "#Port 22", iniParsedLine{kind: iniComment, key: "Port"}, ""},
		{ws, "UsePAM", iniParsedLine{kind: iniKey, key: "UsePAM"}, ""},
		{colon, "host: example.com:80", iniParsedLine{kind: iniKey, key: "host", value: "example.com:80"}, ""},
	} {
		if got := tc.syntax.parseLine(tc.text); got != tc.want {
			t.Errorf("parseLine(%q) with delim %q (%s) = %+v, want %+v", tc.text, tc.syntax.delim, tc.comment, got, tc.want)
		}
	}
}

func TestIniEnsure(t *testing.T) {
	for _, tc := range []struct {
		name              string
		section, key, sep string
		value             string
		in, want          string
		wantChanged       bool
	}{
		{
			name: "update in place keeps everything else", section: "Service", key: "User", value: "app",
			in:   "# unit\n[Unit]\nDescription=x\n\n[Service]\n; who\nUser=root\nExecStart=/bin/true\n",
			want: "# unit\n[Unit]\nDescription=x\n\n[Service]\n; who\nUser = app\nExecStart=/bin/true\n", wantChanged: true,
		},
		{
			name: "already set with other formatting", section: "Service", key: "User", value: "app",
			in: "[Service]\nUser=app\n", want: "[Service]\nUser=app\n",
		},
		{
			name: "insert after last key of section", section: "a", key: "new", value: "1",
			in:   "[a]\nx = 1\ny = 2\n\n# about b\n[b]\nz = 3\n",
			want: "[a]\nx = 1\ny = 2\nnew = 1\n\n# about b\n[b]\nz = 3\n", wantChanged: true,
		},
		{
			name: "insert into empty section", section: "b", key: "k", value: "v",
			in:   "[a]\nx = 1\n[b]\n\n[c]\n",
			want: "[a]\nx = 1\n[b]\nk = v\n\n[c]\n", wantChanged: true,
		},
		{
			name: "insert keeps indentation", section: `remote "origin"`, key: "fetch", sep: " = ", value: "+refs/*:refs/*",
			in:   "[remote \"origin\"]\n\turl = git@example.com:x.git\n[branch \"main\"]\n\tremote = origin\n",
			want: "[remote \"origin\"]\n\turl = git@example.com:x.git\n\tfetch = +refs/*:refs/*\n[branch \"main\"]\n\tremote = origin\n", wantChanged: true,
		},
		{
			name: "missing section is appended", section: "new", key: "k", value: "v",
			in:   "[a]\nx = 1\n",
			want: "[a]\nx = 1\n\n[new]\nk = v\n", wantChanged: true,
		},
		{
			name: "missing section after trailing blank line", section: "new", key: "k", value: "v",
			in:   "[a]\nx = 1\n\n",
			want: "[a]\nx = 1\n\n[new]\nk = v\n", wantChanged: true,
		},
		{
			name: "empty file, section", section: "s", key: "k", value: "v",
			in: "", want: "[s]\nk = v\n", wantChanged: true,
		},
		{
			name: "empty file, global", key: "k", value: "v",
			in: "", want: "k = v\n", wantChanged: true,
		},
		{
			name: "global without sections appends", key: "PermitRootLogin", sep: " ", value: "no",
			in:   "Port 22\nUsePAM yes\n",
			want: "Port 22\nUsePAM yes\nPermitRootLogin no\n", wantChanged: true,
		},
		{
			name: "global key goes before first section and its comments", key: "root", value: "true",
			in:   "# top\n\n# all files\n[*]\nindent_style = space\n",
			want: "# top\n\nroot = true\n\n# all files\n[*]\nindent_style = space\n", wantChanged: true,
		},
		{
			name: "global key after existing global keys", key: "b", value: "2",
			in:   "a = 1\n\n[s]\nb = 9\n",
			want: "a = 1\nb = 2\n\n[s]\nb = 9\n", wantChanged: true,
		},
		{
			name: "same key in other section is untouched", section: "b", key: "x", value: "new",
			in:   "x = g\n[a]\nx = 1\n[b]\nx = 2\n[c]\nx = 3\n",
			want: "x = g\n[a]\nx = 1\n[b]\nx = new\n[c]\nx = 3\n", wantChanged: true,
		},
		{
			name: "global key does not match section keys", key: "x", value: "g",
			in:   "[a]\nx = 1\n",
			want: "x = g\n\n[a]\nx = 1\n", wantChanged: true,
		},
		{
			name: "duplicates are collapsed onto the first", section: "s", key: "k", value: "v",
			in:   "[s]\nk = 1\nother = x\nk = 2\n[t]\nk = 3\n[s]\nk = 4\n",
			want: "[s]\nk = v\nother = x\n[t]\nk = 3\n[s]\n", wantChanged: true,
		},
		{
			name: "duplicates with the right value are still collapsed", section: "s", key: "k", value: "v",
			in:   "[s]\nk = v\nk = v\n",
			want: "[s]\nk = v\n", wantChanged: true,
		},
		{
			name: "repeated section: insert into last occurrence", section: "s", key: "n", value: "1",
			in:   "[s]\na = 1\n[t]\n[s]\nb = 2\n",
			want: "[s]\na = 1\n[t]\n[s]\nb = 2\nn = 1\n", wantChanged: true,
		},
		{
			name: "commented-out key: insert below it and keep the comment", key: "PermitRootLogin", sep: " ", value: "no",
			in:   "Port 22\n#PermitRootLogin prohibit-password\nUsePAM yes\n",
			want: "Port 22\n#PermitRootLogin prohibit-password\nPermitRootLogin no\nUsePAM yes\n", wantChanged: true,
		},
		{
			name: "commented-out key in another section is ignored", section: "b", key: "k", value: "v",
			in:   "[a]\n;k = x\n[b]\ny = 1\n",
			want: "[a]\n;k = x\n[b]\ny = 1\nk = v\n", wantChanged: true,
		},
		{
			name: "commented-out key keeps indentation", section: "PHP", key: "memory_limit", value: "512M",
			in:   "[PHP]\n  ; memory_limit = 128M\n  display_errors = Off\n",
			want: "[PHP]\n  ; memory_limit = 128M\n  memory_limit = 512M\n  display_errors = Off\n", wantChanged: true,
		},
		{
			name: "CRLF update", section: "s", key: "k", value: "v",
			in:   "[s]\r\nk = old\r\nz = 1\r\n",
			want: "[s]\r\nk = v\r\nz = 1\r\n", wantChanged: true,
		},
		{
			name: "CRLF insert uses CRLF", section: "s", key: "k", value: "v",
			in:   "[s]\r\nz = 1\r\n",
			want: "[s]\r\nz = 1\r\nk = v\r\n", wantChanged: true,
		},
		{
			name: "CRLF new section uses CRLF", section: "n", key: "k", value: "v",
			in:   "[s]\r\nz = 1\r\n",
			want: "[s]\r\nz = 1\r\n\r\n[n]\r\nk = v\r\n", wantChanged: true,
		},
		{
			name: "no trailing newline: update keeps it missing", section: "s", key: "k", value: "v",
			in:   "[s]\nk = old",
			want: "[s]\nk = v", wantChanged: true,
		},
		{
			name: "no trailing newline: append terminates previous line", section: "s", key: "k", value: "v",
			in:   "[s]\nz = 1",
			want: "[s]\nz = 1\nk = v\n", wantChanged: true,
		},
		{
			name: "no trailing newline: new section", section: "n", key: "k", value: "v",
			in:   "[s]\r\nz = 1",
			want: "[s]\r\nz = 1\r\n\r\n[n]\r\nk = v\r\n", wantChanged: true,
		},
		{
			name: "no trailing newline: removing the last duplicate", section: "s", key: "k", value: "v",
			in:   "[s]\nk = v\nk = w",
			want: "[s]\nk = v", wantChanged: true,
		},
		{
			name: "empty value", section: "s", key: "k", value: "",
			in:   "[s]\nk = x\n",
			want: "[s]\nk =\n", wantChanged: true,
		},
		{
			name: "empty value with whitespace separator", key: "UseDNS", sep: " ", value: "",
			in:   "",
			want: "UseDNS\n", wantChanged: true,
		},
		{
			name: "colon separator", section: "s", key: "host", sep: ": ", value: "example.com:80",
			in:   "[s]\nhost:old\n",
			want: "[s]\nhost: example.com:80\n", wantChanged: true,
		},
		{
			name: "key is case-sensitive", section: "s", key: "Key", value: "v",
			in:   "[s]\nkey = x\n",
			want: "[s]\nkey = x\nKey = v\n", wantChanged: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sep := tc.sep
			if sep == "" {
				sep = defaultIniSeparator
			}
			s := mustIniSpec(t, tc.section, tc.key, sep)
			f := parseIniFile([]byte(tc.in))
			changed, err := s.ensure(f, tc.value)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(f.bytes()); got != tc.want {
				t.Errorf("ensure:\n got %q\nwant %q", got, tc.want)
			}
			if changed != tc.wantChanged {
				t.Errorf("changed = %v, want %v", changed, tc.wantChanged)
			}
			// Idempotency: a second ensure changes nothing, and the value
			// reads back.
			f2 := parseIniFile([]byte(tc.want))
			if changed, err := s.ensure(f2, tc.value); err != nil || changed {
				t.Errorf("second ensure: changed = %v, err = %v", changed, err)
			}
			if got := s.values(f2); !slices.Equal(got, []string{tc.value}) {
				t.Errorf("values after ensure = %q, want [%q]", got, tc.value)
			}
		})
	}
}

func TestIniRemove(t *testing.T) {
	for _, tc := range []struct {
		name         string
		section, key string
		in, want     string
		wantChanged  bool
	}{
		{
			name: "removes every occurrence in the section only", section: "s", key: "k",
			in:   "k = g\n[s]\n# k = commented\nk = 1\na = 1\n[t]\nk = 2\n[s]\nk = 3\n",
			want: "k = g\n[s]\n# k = commented\na = 1\n[t]\nk = 2\n[s]\n", wantChanged: true,
		},
		{
			name: "global", key: "k",
			in:   "k = 1\n\n[s]\nk = 2\n",
			want: "\n[s]\nk = 2\n", wantChanged: true,
		},
		{
			name: "missing key", section: "s", key: "k",
			in: "[s]\na = 1\n", want: "[s]\na = 1\n",
		},
		{
			name: "missing section", section: "x", key: "k",
			in: "[s]\nk = 1\n", want: "[s]\nk = 1\n",
		},
		{
			name: "CRLF", section: "s", key: "k",
			in: "[s]\r\nk = 1\r\na = 2\r\n", want: "[s]\r\na = 2\r\n", wantChanged: true,
		},
		{
			name: "no trailing newline stays missing", section: "s", key: "k",
			in: "[s]\na = 2\nk = 1", want: "[s]\na = 2", wantChanged: true,
		},
		{
			name: "only line", key: "k",
			in: "k = 1", want: "", wantChanged: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := mustIniSpec(t, tc.section, tc.key, defaultIniSeparator)
			f := parseIniFile([]byte(tc.in))
			if changed := s.remove(f); changed != tc.wantChanged {
				t.Errorf("changed = %v, want %v", changed, tc.wantChanged)
			}
			if got := string(f.bytes()); got != tc.want {
				t.Errorf("remove:\n got %q\nwant %q", got, tc.want)
			}
			if got := s.values(f); len(got) != 0 {
				t.Errorf("values after remove = %q", got)
			}
		})
	}
}

func TestIniValues(t *testing.T) {
	const in = "k = g\n[s]\nk = 1\n;k = c\n  k=2\n[t]\nk = 3\n[s]\nk = 4\n"
	for _, tc := range []struct {
		section string
		want    []string
	}{
		{"", []string{"g"}},
		{"s", []string{"1", "2", "4"}},
		{"t", []string{"3"}},
		{"u", []string{}},
	} {
		got := mustIniSpec(t, tc.section, "k", defaultIniSeparator).values(parseIniFile([]byte(in)))
		if !slices.Equal(got, tc.want) {
			t.Errorf("values in section %q = %q, want %q", tc.section, got, tc.want)
		}
	}
}

func TestIniValidation(t *testing.T) {
	for _, tc := range []struct {
		section, key, sep string
		wantErr           string
	}{
		{"", "k", " = ", ""},
		{`remote "origin"`, "url", "=", ""},
		{"Service", "Environment", " ", ""},
		{"", "a.b-c_d", ": ", ""},
		{"a]", "k", " = ", "section must not contain"},
		{"[a", "k", " = ", "section must not contain"},
		{" a", "k", " = ", "whitespace"},
		{"a\nb", "k", " = ", "line breaks"},
		{"", "", " = ", "key must not be empty"},
		{"", " k", " = ", "whitespace"},
		{"", "#k", " = ", "must not start with"},
		{"", ";k", " = ", "must not start with"},
		{"", "[k", " = ", "must not start with"},
		{"", "a=b", " = ", "must not contain the separator"},
		{"", "a b", " ", "must not contain whitespace"},
		{"", "a b", " = ", ""},
		{"", "k\r", " = ", "line breaks"},
		{"", "k", "", "separator must not be empty"},
		{"", "k", " : = ", "whitespace other than"},
		{"", "k", " # ", "must not start with"},
		{"", "k", "=\n", "line breaks"},
	} {
		_, err := newIniSpec(tc.section, tc.key, tc.sep)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("newIniSpec(%q, %q, %q) = %v, want no error", tc.section, tc.key, tc.sep, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("newIniSpec(%q, %q, %q) = %v, want error containing %q", tc.section, tc.key, tc.sep, err, tc.wantErr)
		}
	}

	for _, v := range []string{"", "x", "a = b", `"quoted"`, "#not a comment"} {
		if err := validateIniValue(v); err != nil {
			t.Errorf("validateIniValue(%q) = %v", v, err)
		}
	}
	for _, v := range []string{" x", "x ", "a\nb", "a\rb", "\t"} {
		if err := validateIniValue(v); err == nil {
			t.Errorf("validateIniValue(%q) = nil, want error", v)
		}
	}
}

// TestIniEnsureWrittenValueReadsBack checks, for every valid separator
// style, that whatever ensure writes is parsed back as the same key and
// value, so that the resource converges.
func TestIniEnsureWrittenValueReadsBack(t *testing.T) {
	for _, sep := range []string{" = ", "=", " =", ": ", ":", " ", "\t", "  ", " => "} {
		for _, value := range []string{"", "v", "a b", "a = b", "x:y", "#h", ";s", "[x]"} {
			s := mustIniSpec(t, "s", "key", sep)
			f := parseIniFile([]byte("[s]\nother = 1\n"))
			if _, err := s.ensure(f, value); err != nil {
				t.Fatal(err)
			}
			if got := s.values(f); !slices.Equal(got, []string{value}) {
				t.Errorf("separator %q, value %q: wrote %q, read back %q", sep, value, f.bytes(), got)
			}
		}
	}
}
