package provider

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestValidateLocaleName(t *testing.T) {
	for _, ok := range []string{"C", "POSIX", "C.UTF-8", "en_US.UTF-8", "en_US.utf8", "de_DE@euro", "sr_RS.UTF-8@latin", "de_DE.ISO-8859-15@euro", "ks_IN.UTF-8@devanagari"} {
		if err := validateLocaleName(ok); err != nil {
			t.Errorf("validateLocaleName(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "-C", ".UTF-8", "en US", "en_US.UTF-8\nLC_ALL=C", "en_US.UTF-8;id", "$(id)", "`id`", "en_US/UTF-8", "\"C\"", strings.Repeat("a", 129)} {
		if err := validateLocaleName(bad); err == nil {
			t.Errorf("validateLocaleName(%q) = nil, want an error", bad)
		}
	}
	if err := validateLCVariable("LC_TIME"); err != nil {
		t.Error(err)
	}
	for _, bad := range []string{"LC_ALL", "LANG", "LANGUAGE", "lc_time", "LC_FOO"} {
		if err := validateLCVariable(bad); err == nil {
			t.Errorf("validateLCVariable(%q) = nil, want an error", bad)
		}
	}
}

func TestNormalizeLocaleName(t *testing.T) {
	for in, want := range map[string]string{
		"C":                      "C",
		"C.UTF-8":                "C.utf8",
		"en_US.UTF-8":            "en_US.utf8",
		"en_US.utf8":             "en_US.utf8",
		"de_DE.ISO-8859-15@euro": "de_DE.iso885915@euro",
		"sr_RS.UTF-8@latin":      "sr_RS.utf8@latin",
		"de_DE@euro":             "de_DE@euro",
		"ja_JP.eucJP":            "ja_JP.eucjp",
		"xx_XX.8859-1":           "xx_XX.iso88591",
	} {
		if got := normalizeLocaleName(in); got != want {
			t.Errorf("normalizeLocaleName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseLocaleAssignment(t *testing.T) {
	for _, tc := range []struct {
		line, name, value string
		ok                bool
	}{
		{"LANG=en_US.UTF-8", "LANG", "en_US.UTF-8", true},
		{`LANG="en_US.UTF-8"`, "LANG", "en_US.UTF-8", true},
		{"  LC_TIME='en_GB.UTF-8'  ", "LC_TIME", "en_GB.UTF-8", true},
		{"export LANG=C.UTF-8", "LANG", "C.UTF-8", true},
		{"LANG=C.UTF-8 # comment", "LANG", "C.UTF-8", true},
		{"LANGUAGE=en_US:en", "LANGUAGE", "en_US:en", true},
		{"LANG=", "LANG", "", true},
		{"# LANG=de_DE.UTF-8", "", "", false},
		{"", "", "", false},
		{"exportLANG=C", "exportLANG", "C", true},
		{"not an assignment", "", "", false},
		{"1LANG=C", "", "", false},
	} {
		name, value, ok := parseLocaleAssignment(tc.line)
		if name != tc.name || value != tc.value || ok != tc.ok {
			t.Errorf("parseLocaleAssignment(%q) = %q, %q, %v; want %q, %q, %v", tc.line, name, value, ok, tc.name, tc.value, tc.ok)
		}
	}
}

func TestSetLocaleVars(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		vars     map[string]string
		want     string
		changed  bool
	}{
		{"empty file", "", map[string]string{"LANG": "en_US.UTF-8"}, "LANG=en_US.UTF-8\n", true},
		{"unchanged keeps quoting", "# locale\nLANG=\"en_US.UTF-8\"\n", map[string]string{"LANG": "en_US.UTF-8"}, "# locale\nLANG=\"en_US.UTF-8\"\n", false},
		{
			"rewrite in place, drop others, keep LANGUAGE",
			"# comment\nLANGUAGE=en_US:en\nLANG=C\nLC_ALL=C\nLC_TIME=C\nLANG=de_DE.UTF-8\nLC_PAPER=C\n",
			map[string]string{"LANG": "en_US.UTF-8", "LC_PAPER": "de_DE.UTF-8", "LC_MEASUREMENT": "de_DE.UTF-8"},
			"# comment\nLANGUAGE=en_US:en\nLANG=en_US.UTF-8\nLC_PAPER=de_DE.UTF-8\nLC_MEASUREMENT=de_DE.UTF-8\n",
			true,
		},
		{"append in canonical order", "LANG=C.UTF-8", map[string]string{"LANG": "C.UTF-8", "LC_TIME": "en_GB.UTF-8", "LC_CTYPE": "C.UTF-8"}, "LANG=C.UTF-8\nLC_CTYPE=C.UTF-8\nLC_TIME=en_GB.UTF-8\n", true},
		{"no trailing newline unchanged", "LANG=C.UTF-8", map[string]string{"LANG": "C.UTF-8"}, "LANG=C.UTF-8", false},
		{"remove all", "LANG=C\n# keep\nLC_TIME=C\n", map[string]string{}, "# keep\n", true},
		{"restore unknown LC variable", "LANG=C\n", map[string]string{"LANG": "C", "LC_ALL": "C"}, "LANG=C\nLC_ALL=C\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := parseTextFile([]byte(tc.in))
			changed := setLocaleVars(f, tc.vars)
			if got := string(f.bytes()); got != tc.want || changed != tc.changed {
				t.Errorf("setLocaleVars = %q, %v; want %q, %v", got, changed, tc.want, tc.changed)
			}
			if got := readLocaleVars(f.lines); !maps.Equal(got, tc.vars) {
				t.Errorf("readLocaleVars after set = %v, want %v", got, tc.vars)
			}
		})
	}
}

func TestDetectLocalePath(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, root string)
		want  string
	}{
		{"empty", func(*testing.T, string) {}, localeConfPath},
		{"debian marker", func(t *testing.T, root string) { mustWrite(t, filepath.Join(root, "etc/debian_version"), "12\n") }, debianLocalePath},
		{"default/locale exists", func(t *testing.T, root string) { mustWrite(t, filepath.Join(root, "etc/default/locale"), "LANG=C\n") }, debianLocalePath},
		{"locale.conf exists", func(t *testing.T, root string) {
			mustWrite(t, filepath.Join(root, "etc/debian_version"), "12\n")
			mustWrite(t, filepath.Join(root, "etc/locale.conf"), "LANG=C\n")
		}, localeConfPath},
		{"ubuntu symlink", func(t *testing.T, root string) {
			mustWrite(t, filepath.Join(root, "etc/debian_version"), "trixie/sid\n")
			mustWrite(t, filepath.Join(root, "etc/locale.conf"), "LANG=C\n")
			mustSymlink(t, "../locale.conf", filepath.Join(root, "etc/default/locale"))
		}, localeConfPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := testRootDir(t)
			mustMkdir(t, filepath.Join(root, "etc/default"))
			tc.setup(t, root)
			r, _ := newFSRoot(root)
			if got, err := detectLocalePath(r); err != nil || got != tc.want {
				t.Errorf("detectLocalePath = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestWriteLocaleVars(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "locale.conf")
	// A missing file and its directory are created.
	if err := writeLocaleVars(p, map[string]string{"LANG": "C.UTF-8"}, false); err != nil {
		t.Fatal(err)
	}
	if err := checkFileContent(p, "LANG=C.UTF-8\n")(nil); err != nil {
		t.Error(err)
	}
	if err := checkFileMode(p, 0o644)(nil); err != nil {
		t.Error(err)
	}
	// Restoring the state of a file that did not exist removes it again.
	if err := writeLocaleVars(p, map[string]string{}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		t.Errorf("file still exists: %v", err)
	}
	// ... unless something else was added meanwhile.
	mustWrite(t, p, "LANG=C\nLANGUAGE=en\n")
	if err := writeLocaleVars(p, map[string]string{}, true); err != nil {
		t.Fatal(err)
	}
	if err := checkFileContent(p, "LANGUAGE=en\n")(nil); err != nil {
		t.Error(err)
	}
	// A symlink at the path is refused.
	link := filepath.Join(dir, "link")
	mustSymlink(t, p, link)
	if err := writeLocaleVars(link, map[string]string{"LANG": "C"}, false); err == nil {
		t.Error("writeLocaleVars wrote through a symlink")
	}
	if err := checkFileContent(p, "LANGUAGE=en\n")(nil); err != nil {
		t.Error(err)
	}
	checkNoTempFilesT(t, filepath.Dir(p))
}

func TestEnableInLocaleGen(t *testing.T) {
	const gen = "# This file lists locales that you wish to have built.\n# de_DE.UTF-8 UTF-8\n# en_US ISO-8859-1\n# en_US.UTF-8 UTF-8\nC.UTF-8 UTF-8\n"
	f := parseTextFile([]byte(gen))
	if !enableInLocaleGen(f, supportedLocale{"en_US.UTF-8", "UTF-8"}) {
		t.Fatal("enableInLocaleGen reported no change")
	}
	want := strings.Replace(gen, "# en_US.UTF-8 UTF-8", "en_US.UTF-8 UTF-8", 1)
	if got := string(f.bytes()); got != want {
		t.Errorf("uncommented: %q, want %q", got, want)
	}
	if enableInLocaleGen(f, supportedLocale{"en_US.UTF-8", "UTF-8"}) {
		t.Error("an enabled locale was enabled again")
	}
	if !enableInLocaleGen(f, supportedLocale{"fr_FR.UTF-8", "UTF-8"}) || !strings.HasSuffix(string(f.bytes()), "\nfr_FR.UTF-8 UTF-8\n") {
		t.Errorf("not appended: %q", f.bytes())
	}
}

// fakeLocaleTools simulates locale -a, locale-gen and localedef.
type fakeLocaleTools struct {
	t         *testing.T
	etcDir    string
	i18nDir   string
	tools     []string // Tools that are "installed".
	mu        sync.Mutex
	installed []string
	calls     [][]string
	failDef   bool
}

func newFakeLocaleTools(t *testing.T, tools ...string) *fakeLocaleTools {
	f := &fakeLocaleTools{t: t, etcDir: t.TempDir(), i18nDir: t.TempDir(), tools: tools, installed: []string{"C", "C.utf8", "POSIX"}}
	mustWrite(t, filepath.Join(f.i18nDir, "SUPPORTED"), "de_DE.UTF-8 UTF-8\nde_DE ISO-8859-1\nde_DE@euro ISO-8859-15\nen_GB.UTF-8 UTF-8\nen_US.UTF-8 UTF-8\n")
	return f
}

func (f *fakeLocaleTools) config() *localeConfig {
	return &localeConfig{
		run: f.run,
		lookPath: func(name string) (string, error) {
			if slices.Contains(f.tools, name) {
				return "/usr/bin/" + name, nil
			}
			return "", errors.New("not found")
		},
		etcDir:  f.etcDir,
		i18nDir: f.i18nDir,
	}
}

func (f *fakeLocaleTools) run(_ context.Context, spec execSpec) (*execResult, error) {
	if spec.Timeout <= 0 {
		f.t.Errorf("%q run without a timeout", spec.Argv)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, spec.Argv)
	res := &execResult{Stdout: newCappedOutput(spec.MaxOutputBytes), Stderr: newCappedOutput(spec.MaxOutputBytes)}
	switch spec.Argv[0] {
	case "locale":
		_, _ = res.Stdout.Write([]byte(strings.Join(f.installed, "\n") + "\n"))
	case "locale-gen":
		data, err := os.ReadFile(filepath.Join(f.etcDir, "locale.gen"))
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(data), "\n") {
			if e, ok := parseSupportedLine(line); ok {
				f.installed = append(f.installed, normalizeLocaleName(e.name))
			}
		}
	case "localedef":
		if f.failDef {
			res.ExitCode = 4
			_, _ = res.Stderr.Write([]byte("localedef: cannot open locale definition file `xx_XX': No such file or directory\n"))
			return res, nil
		}
		f.installed = append(f.installed, normalizeLocaleName(spec.Argv[len(spec.Argv)-1]))
	default:
		return nil, fmt.Errorf("unexpected command %q", spec.Argv)
	}
	return res, nil
}

func TestLocaleGenerate_localeGen(t *testing.T) {
	f := newFakeLocaleTools(t, "locale", "locale-gen", "localedef")
	gen := filepath.Join(f.etcDir, "locale.gen")
	mustWrite(t, gen, "# en_US.UTF-8 UTF-8\n# de_DE@euro ISO-8859-15\n")
	cfg := f.config()
	ctx := context.Background()
	installed, err := cfg.available(ctx)
	if err != nil {
		t.Fatal(err)
	}
	missing := missingLocales(installed, []string{"en_US.UTF-8", "C.UTF-8", "de_DE@euro", "en_US.utf8"})
	if want := []string{"en_US.UTF-8", "de_DE@euro", "en_US.utf8"}; !slices.Equal(missing, want) {
		t.Fatalf("missing = %q, want %q", missing, want)
	}
	if err := cfg.generate(ctx, missing); err != nil {
		t.Fatal(err)
	}
	if err := checkFileContent(gen, "en_US.UTF-8 UTF-8\nde_DE@euro ISO-8859-15\n")(nil); err != nil {
		t.Error(err)
	}
	if installed, _ = cfg.available(ctx); len(missingLocales(installed, missing)) > 0 {
		t.Errorf("still missing after locale-gen: %v", installed)
	}
	// A locale glibc does not know is refused before anything changes.
	if err := cfg.generate(ctx, []string{"xx_XX.UTF-8"}); err == nil || !strings.Contains(err.Error(), "not listed in") {
		t.Errorf("generate(xx_XX) = %v", err)
	}
}

func TestLocaleGenerate_localedef(t *testing.T) {
	f := newFakeLocaleTools(t, "locale", "localedef")
	cfg := f.config()
	if err := cfg.generate(context.Background(), []string{"de_DE.UTF-8@euro", "de_DE"}); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"localedef", "-i", "de_DE@euro", "-f", "UTF-8", "--", "de_DE.UTF-8@euro"},
		{"localedef", "-i", "de_DE", "-f", "ISO-8859-1", "--", "de_DE"},
	}
	if !slices.EqualFunc(f.calls, want, slices.Equal) {
		t.Errorf("calls = %q, want %q", f.calls, want)
	}
	if err := cfg.generate(context.Background(), []string{"xx_XX"}); err == nil || !strings.Contains(err.Error(), "has no codeset") {
		t.Errorf("generate(xx_XX) = %v", err)
	}
	f.failDef = true
	if err := cfg.generate(context.Background(), []string{"xx_XX.UTF-8"}); err == nil || !strings.Contains(err.Error(), "glibc-locale-source") {
		t.Errorf("failing localedef: %v", err)
	}
}

func TestLocaleGenerate_noTools(t *testing.T) {
	f := newFakeLocaleTools(t)
	cfg := f.config()
	if _, err := cfg.available(context.Background()); err == nil {
		t.Error("available without the locale command")
	}
	if err := cfg.generate(context.Background(), []string{"en_US.UTF-8"}); err == nil || !strings.Contains(err.Error(), "neither locale-gen") {
		t.Errorf("generate = %v", err)
	}
	if len(f.calls) > 0 {
		t.Errorf("commands run: %q", f.calls)
	}
}
