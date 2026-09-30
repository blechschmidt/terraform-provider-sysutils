package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// debianEditorQuery is real update-alternatives --query output (dpkg
// 1.22, Ubuntu 24.04), shortened.
const debianEditorQuery = `Name: editor
Link: /usr/bin/editor
Slaves:
 editor.1.gz /usr/share/man/man1/editor.1.gz
 editor.da.1.gz /usr/share/man/da/man1/editor.1.gz
Status: auto
Best: /bin/nano
Value: /bin/nano

Alternative: /bin/ed
Priority: -100
Slaves:
 editor.1.gz /usr/share/man/man1/ed.1.gz

Alternative: /bin/nano
Priority: 40
Slaves:
 editor.1.gz /usr/share/man/man1/nano.1.gz

Alternative: /usr/bin/vim.basic
Priority: 30
Slaves:
 editor.1.gz /usr/share/man/man1/vim.1.gz
`

// rhelDisplay is real alternatives --display output (chkconfig 1.33,
// Fedora), with a family and a follower added.
const rhelDisplay = `tst - status is manual.
 link currently points to /opt/a/x
/opt/a/x - priority 10
 follower tst.1.gz: /usr/share/man/man1/x.1.gz
/opt/a/y - family fam priority 20
Current ` + "`best'" + ` version is /opt/a/y.
`

func TestParseDebianAlternativesQuery(t *testing.T) {
	got, err := parseDebianAlternativesQuery(debianEditorQuery, "editor")
	if err != nil {
		t.Fatal(err)
	}
	want := alternativesStatus{
		Found: true,
		Link:  "/usr/bin/editor",
		Mode:  "auto",
		Value: "/bin/nano",
		Alternatives: []alternativeEntry{
			{Path: "/bin/ed", Priority: -100},
			{Path: "/bin/nano", Priority: 40},
			{Path: "/usr/bin/vim.basic", Priority: 30},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
	if got.selected() != "/bin/nano" {
		t.Errorf("selected() = %q", got.selected())
	}

	// A manual group pointing to nothing, and one pointing to a deleted
	// alternative, select nothing.
	none := strings.Replace(strings.Replace(debianEditorQuery, "Value: /bin/nano", "Value: none", 1), "Status: auto", "Status: manual", 1)
	got, err = parseDebianAlternativesQuery(none, "editor")
	if err != nil || got.Value != "" || got.Mode != "manual" || got.selected() != "" {
		t.Errorf("Value: none: %+v, %v", got, err)
	}
	gone := strings.Replace(debianEditorQuery, "Value: /bin/nano", "Value: /usr/bin/emacs", 1)
	if got, err = parseDebianAlternativesQuery(gone, "editor"); err != nil || got.selected() != "" {
		t.Errorf("unregistered value: %+v, %v", got, err)
	}

	for name, out := range map[string]string{
		"other group":    strings.Replace(debianEditorQuery, "Name: editor", "Name: vi", 1),
		"no name":        "Link: /usr/bin/editor\nStatus: auto\n",
		"bad status":     strings.Replace(debianEditorQuery, "Status: auto", "Status: broken", 1),
		"bad priority":   strings.Replace(debianEditorQuery, "Priority: 40", "Priority: high", 1),
		"stray priority": "Name: editor\nStatus: auto\nPriority: 3\n",
		"garbage":        "Name: editor\nStatus: auto\nthis is not a field\n",
	} {
		if _, err := parseDebianAlternativesQuery(out, "editor"); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestParseRHELAlternativesDisplay(t *testing.T) {
	got, err := parseRHELAlternativesDisplay(rhelDisplay, "tst")
	if err != nil {
		t.Fatal(err)
	}
	want := alternativesStatus{
		Found: true,
		Mode:  "manual",
		Value: "/opt/a/x",
		Alternatives: []alternativeEntry{
			{Path: "/opt/a/x", Priority: 10},
			{Path: "/opt/a/y", Priority: 20},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
	absent := strings.Replace(rhelDisplay, " link currently points to /opt/a/x", " link currently absent", 1)
	if got, err = parseRHELAlternativesDisplay(absent, "tst"); err != nil || got.Value != "" {
		t.Errorf("absent link: %+v, %v", got, err)
	}
	for name, out := range map[string]string{
		"other group": strings.Replace(rhelDisplay, "tst - status", "vi - status", 1),
		"empty":       "",
		"garbage":     "no status line\n",
		"bad status":  strings.Replace(rhelDisplay, "status is manual.", "status is broken.", 1),
	} {
		if _, err := parseRHELAlternativesDisplay(out, "tst"); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestReadRHELAlternativesLink(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "tst")
	if _, found, err := readRHELAlternativesLink(p); found || err != nil {
		t.Errorf("missing file: found=%t err=%v", found, err)
	}
	mustWrite(t, p, "manual\n/usr/local/bin/tst\n\n/opt/a/x\n10\n")
	if link, found, err := readRHELAlternativesLink(p); link != "/usr/local/bin/tst" || !found || err != nil {
		t.Errorf("got %q, %t, %v", link, found, err)
	}
	mustWrite(t, p, "manual\n")
	if _, _, err := readRHELAlternativesLink(p); err == nil {
		t.Error("file without link: no error")
	}
	// Never followed through a symlink.
	link := filepath.Join(dir, "link")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readRHELAlternativesLink(link); err == nil {
		t.Error("symlink: no error")
	}
}

func TestValidateAlternatives(t *testing.T) {
	for _, name := range []string{"editor", "x-www-browser", "c++", "libblas.so.3-x86_64-linux-gnu", "libnssckbi.so.x86_64", "_x", "a@b:c~d"} {
		if err := validateAlternativesName(name); err != nil {
			t.Errorf("%q: %v", name, err)
		}
	}
	for _, name := range []string{"", "-editor", "--auto", ".hidden", "a/b", "..", "a b", "a\nb", strings.Repeat("a", 256), "é"} {
		if err := validateAlternativesName(name); err == nil {
			t.Errorf("%q: no error", name)
		}
	}
	for _, p := range []string{"/usr/bin/vim.basic", "/opt/jdk-21/bin/java", "/usr/lib/x86_64-linux-gnu/libblas.so.3"} {
		if err := validateAlternativesPath(p); err != nil {
			t.Errorf("%q: %v", p, err)
		}
	}
	for _, p := range []string{"", "vim", "-x", "--set", "/", "/a/../b", "/a/", "/a b", "/a\tb", "/a\nb", "/a\x00b", "/a b", "/a\xffb", "/" + strings.Repeat("a", 4096)} {
		if err := validateAlternativesPath(p); err == nil {
			t.Errorf("%q: no error", p)
		}
	}
}

func TestAlternativesToolDetection(t *testing.T) {
	dir := t.TempDir()
	// Fedora: update-alternatives is a symlink to alternatives.
	rhel := filepath.Join(dir, "alternatives")
	mustWrite(t, rhel, "")
	fedoraUA := filepath.Join(dir, "update-alternatives")
	if err := os.Symlink("alternatives", fedoraUA); err != nil {
		t.Fatal(err)
	}
	debianDir := t.TempDir()
	debianUA := filepath.Join(debianDir, "update-alternatives")
	mustWrite(t, debianUA, "")

	cases := []struct {
		name     string
		paths    map[string]string
		wantKind string
		wantBin  string
	}{
		{"debian", map[string]string{alternativesDebian: debianUA}, alternativesDebian, debianUA},
		{"fedora", map[string]string{alternativesDebian: fedoraUA, alternativesRHEL: rhel}, alternativesRHEL, rhel},
		{"rhel without update-alternatives", map[string]string{alternativesRHEL: rhel}, alternativesRHEL, rhel},
		{"none", nil, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &alternativesConfig{lookPath: func(n string) (string, error) {
				if p, ok := c.paths[n]; ok {
					return p, nil
				}
				return "", errors.New("not found")
			}}
			tool, err := cfg.tool()
			if c.wantKind == "" {
				if err == nil {
					t.Fatalf("got %+v, want an error", tool)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tool.kind != c.wantKind || tool.bin != c.wantBin || tool.adminDir != defaultRHELAlternativesAdminDir {
				t.Errorf("got kind=%q bin=%q adminDir=%q", tool.kind, tool.bin, tool.adminDir)
			}
		})
	}
}

// TestAlternativesFakeBinary runs a fake update-alternatives script through
// the real command runner, to check the argument vector, the C locale and
// the handling of exit statuses end to end.
func TestAlternativesFakeBinary(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	bin := filepath.Join(dir, "update-alternatives")
	query := filepath.Join(dir, "query")
	mustWrite(t, query, debianEditorQuery)
	script := `#!/bin/sh
printf '%s|' "$LC_ALL" "$@" >>` + log + `
echo >>` + log + `
case "$1" in
--query)
	if [ "$2" = editor ]; then cat ` + query + `; exit 0; fi
	echo "update-alternatives: error: no alternatives for $2" >&2; exit 2 ;;
--set)
	if [ "$3" = /bin/ed ]; then exit 0; fi
	echo "update-alternatives: error: alternative $3 for $2 not registered; not setting" >&2; exit 2 ;;
esac
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &alternativesConfig{lookPath: func(n string) (string, error) {
		if n == alternativesDebian {
			return bin, nil
		}
		return "", errors.New("not found")
	}}
	tool, err := cfg.tool()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	st, err := tool.Query(ctx, "editor")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Found || st.Value != "/bin/nano" || st.Link != "/usr/bin/editor" || len(st.Alternatives) != 3 {
		t.Errorf("query editor: %+v", st)
	}
	if st, err = tool.Query(ctx, "nosuch"); err != nil || st.Found {
		t.Errorf("query nosuch: %+v, %v", st, err)
	}
	if err := tool.Set(ctx, "editor", "/bin/ed"); err != nil {
		t.Error(err)
	}
	err = tool.Set(ctx, "editor", "/usr/bin/emacs")
	if err == nil || !strings.Contains(err.Error(), "exit status 2: update-alternatives: error: alternative /usr/bin/emacs for editor not registered") {
		t.Errorf("set unregistered: %v", err)
	}
	if err := tool.Install(ctx, "/usr/bin/editor", "editor", "/opt/ed", -3); err != nil {
		t.Error(err)
	}
	if err := tool.Auto(ctx, "editor"); err != nil {
		t.Error(err)
	}
	if err := tool.Remove(ctx, "editor", "/opt/ed"); err != nil {
		t.Error(err)
	}

	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := `C|--query|editor|
C|--query|nosuch|
C|--set|editor|/bin/ed|
C|--set|editor|/usr/bin/emacs|
C|--install|/usr/bin/editor|editor|/opt/ed|-3|
C|--auto|editor|
C|--remove|editor|/opt/ed|
`
	if string(data) != want {
		t.Errorf("invocations:\n%s\nwant:\n%s", data, want)
	}
}
