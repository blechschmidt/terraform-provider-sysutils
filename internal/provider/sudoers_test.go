package provider

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// requireVisudo skips the test if visudo is not installed, as on Alpine
// without sudo.
func requireVisudo(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("visudo")
	if err != nil {
		t.Skip("visudo is not installed (sudo is not installed on this host)")
	}
	return bin
}

// fakeVisudo is a commandRunner standing in for visudo -cf: it rejects
// files containing "SYNTAX ERROR" with a message naming the file, as visudo
// does, and records the files it checked.
type fakeVisudo struct {
	t       *testing.T
	mu      sync.Mutex
	checked []string
}

const fakeVisudoPath = "/fake/sbin/visudo"

func (f *fakeVisudo) run(_ context.Context, spec execSpec) (*execResult, error) {
	if spec.Timeout <= 0 {
		f.t.Errorf("fake visudo: %q run without a timeout", spec.Argv)
	}
	if len(spec.Argv) != 4 || spec.Argv[0] != fakeVisudoPath || spec.Argv[1] != "-c" || spec.Argv[2] != "-f" {
		return nil, errors.New("fake visudo: unexpected arguments")
	}
	tmp := spec.Argv[3]
	res := &execResult{Stdout: newCappedOutput(spec.MaxOutputBytes), Stderr: newCappedOutput(spec.MaxOutputBytes)}
	data, err := os.ReadFile(tmp)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(tmp)
	if err != nil {
		return nil, err
	}
	if info.Mode() != sudoersFileMode {
		f.t.Errorf("fake visudo: %s has mode %v before the rename, want %v", tmp, info.Mode(), sudoersFileMode)
	}
	f.mu.Lock()
	f.checked = append(f.checked, tmp)
	f.mu.Unlock()
	if strings.Contains(string(data), "SYNTAX ERROR") {
		res.ExitCode = 1
		_, _ = res.Stderr.Write([]byte(tmp + ":1:7: syntax error\n"))
		return res, nil
	}
	_, _ = res.Stdout.Write([]byte(tmp + ": parsed OK\n"))
	return res, nil
}

func (f *fakeVisudo) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.checked)
}

func TestValidateSudoersName(t *testing.T) {
	for _, name := range []string{"90-deploy", "admins", "a_b-c", "x~y", strings.Repeat("a", 255)} {
		if err := validateSudoersName(name); err != nil {
			t.Errorf("validateSudoersName(%q) = %v, want nil", name, err)
		}
	}
	for name, want := range map[string]string{
		"":                       "empty",
		"admins.conf":            `contain "."`,
		".hidden":                `contain "."`,
		"..":                     `contain "."`,
		"admins~":                `end with "~"`,
		"a/b":                    `contain "/"`,
		"../../etc/shadow":       `contain "/"`,
		"a b":                    "white space",
		"a\nb":                   "white space",
		strings.Repeat("a", 256): "at most 255",
	} {
		err := validateSudoersName(name)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("validateSudoersName(%q) = %v, want an error containing %q", name, err, want)
		}
	}
}

func TestSudoersRuleRender(t *testing.T) {
	for _, c := range []struct {
		rule sudoersRule
		want string
	}{
		{sudoersRule{users: []string{"alice"}, hosts: []string{"ALL"}, commands: []string{"ALL"}}, "alice ALL = ALL"},
		{
			sudoersRule{users: []string{"%deploy", "bob"}, hosts: []string{"web1", "web2"}, runas: "ALL:ALL", nopasswd: true, setenv: true,
				commands: []string{"/usr/bin/systemctl restart app", `/usr/bin/kill -s TERM\, 42`}},
			`%deploy, bob web1, web2 = (ALL:ALL) NOPASSWD: SETENV: /usr/bin/systemctl restart app, /usr/bin/kill -s TERM\, 42`,
		},
		{sudoersRule{users: []string{"#1000"}, hosts: []string{"ALL"}, runas: "postgres", commands: []string{"/usr/bin/psql"}}, "#1000 ALL = (postgres) /usr/bin/psql"},
	} {
		if err := c.rule.validate(); err != nil {
			t.Errorf("validate(%+v) = %v", c.rule, err)
		}
		if got := c.rule.render(); got != c.want {
			t.Errorf("render(%+v) = %q, want %q", c.rule, got, c.want)
		}
	}
	got := renderSudoersRules([]sudoersRule{{users: []string{"a"}, hosts: []string{"ALL"}, commands: []string{"ALL"}}})
	if want := sudoersFileHeader + "\na ALL = ALL\n"; got != want {
		t.Errorf("renderSudoersRules = %q, want %q", got, want)
	}
}

// TestSudoersRuleValidate checks that no field can smuggle in another
// command, rule or line.
func TestSudoersRuleValidate(t *testing.T) {
	ok := func() sudoersRule {
		return sudoersRule{users: []string{"alice"}, hosts: []string{"ALL"}, commands: []string{"/bin/true"}}
	}
	for what, c := range map[string]struct {
		mutate func(*sudoersRule)
		want   string
	}{
		"no users":             {func(r *sudoersRule) { r.users = nil }, "at least one"},
		"user list":            {func(r *sudoersRule) { r.users = []string{"alice,bob"} }, `","`},
		"user with space":      {func(r *sudoersRule) { r.users = []string{"alice ALL=ALL"} }, `" "`},
		"user newline":         {func(r *sudoersRule) { r.users = []string{"alice\nbob"} }, "single line"},
		"host equals":          {func(r *sudoersRule) { r.hosts = []string{"ALL=ALL"} }, `"="`},
		"host comment":         {func(r *sudoersRule) { r.hosts = []string{"#x"} }, "comment"},
		"runas parenthesis":    {func(r *sudoersRule) { r.runas = "root) ALL" }, `")"`},
		"runas two colons":     {func(r *sudoersRule) { r.runas = "a:b:c" }, "at most one"},
		"command comma":        {func(r *sudoersRule) { r.commands = []string{"/bin/ls, ALL"} }, `unescaped ","`},
		"command colon":        {func(r *sudoersRule) { r.commands = []string{"/bin/ls : ALL = ALL"} }, `unescaped ":"`},
		"command equals":       {func(r *sudoersRule) { r.commands = []string{"/bin/env A=B"} }, `unescaped "="`},
		"command continuation": {func(r *sudoersRule) { r.commands = []string{`/bin/ls \`} }, "backslash"},
		"command newline":      {func(r *sudoersRule) { r.commands = []string{"/bin/ls\nALL ALL=ALL"} }, "single line"},
		"command empty":        {func(r *sudoersRule) { r.commands = []string{""} }, "empty"},
		"command padded":       {func(r *sudoersRule) { r.commands = []string{" /bin/ls"} }, "white space"},
	} {
		r := ok()
		c.mutate(&r)
		err := r.validate()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: validate() = %v, want an error containing %q", what, err, c.want)
		}
	}
	r := ok()
	r.commands = []string{`/bin/env A\=B`, `/bin/echo a\\`, `/bin/echo a\,b\:c`}
	if err := r.validate(); err != nil {
		t.Errorf("escaped command: validate() = %v", err)
	}
}

func TestWriteSudoersFile_checkFailureLeavesFileUntouched(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sudoers.d")
	p := filepath.Join(dir, "deploy")
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid()) //nolint:gosec // Test IDs fit.
	f := &fakeVisudo{t: t}
	check := func(tmp string) error {
		return checkSudoersFile(context.Background(), f.run, fakeVisudoPath, tmp, "/etc/sudoers.d/deploy")
	}

	if err := writeSudoersFile(p, []byte("alice ALL = ALL\n"), uid, gid, true, check); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != sudoersDirMode {
		t.Errorf("created %s with mode %v, want %v", dir, info.Mode().Perm(), sudoersDirMode)
	}

	err = writeSudoersFile(p, []byte("SYNTAX ERROR\n"), uid, gid, false, check)
	var verr *visudoError
	if !errors.As(err, &verr) {
		t.Fatalf("writeSudoersFile = %v, want a visudoError", err)
	}
	// The output names the file being written, not the temporary file.
	if want := "/etc/sudoers.d/deploy:1:7: syntax error"; verr.output != want {
		t.Errorf("visudo output = %q, want %q", verr.output, want)
	}
	if data, err := os.ReadFile(p); err != nil || string(data) != "alice ALL = ALL\n" {
		t.Errorf("%s = %q, %v after a rejected write; want the previous contents", p, data, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("%s contains %d entries after a rejected write, want only the drop-in", dir, len(entries))
	}
	if f.count() != 2 {
		t.Errorf("visudo checked %d files, want 2", f.count())
	}

	// A rejected new file is not created at all.
	q := filepath.Join(dir, "other")
	if err := writeSudoersFile(q, []byte("SYNTAX ERROR\n"), uid, gid, true, check); !errors.As(err, &verr) {
		t.Fatalf("writeSudoersFile = %v, want a visudoError", err)
	}
	if _, err := os.Lstat(q); !os.IsNotExist(err) {
		t.Errorf("%s exists after a rejected create (err = %v)", q, err)
	}
}

func TestCheckSudoersFile_realVisudo(t *testing.T) {
	bin := requireVisudo(t)
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(good, []byte("alice ALL = (ALL) NOPASSWD: /bin/true\n"), 0o440); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("alice ALL = (ALL NOPASSWD: /bin/true\n"), 0o440); err != nil {
		t.Fatal(err)
	}
	if err := checkSudoersFile(context.Background(), runCommand, bin, good, "/etc/sudoers.d/good"); err != nil {
		t.Errorf("checkSudoersFile(good) = %v", err)
	}
	err := checkSudoersFile(context.Background(), runCommand, bin, bad, "/etc/sudoers.d/bad")
	var verr *visudoError
	if !errors.As(err, &verr) {
		t.Fatalf("checkSudoersFile(bad) = %v, want a visudoError", err)
	}
	if !strings.Contains(verr.output, "/etc/sudoers.d/bad") || strings.Contains(verr.output, bad) {
		t.Errorf("visudo output %q should name /etc/sudoers.d/bad instead of %s", verr.output, bad)
	}
}

func TestSudoersConfigVisudo(t *testing.T) {
	found := func(string) (string, error) { return fakeVisudoPath, nil }
	missing := func(string) (string, error) { return "", exec.ErrNotFound }
	inRoot := func(look func(string) (string, error)) func(*fsRoot) func(string) (string, error) {
		return func(*fsRoot) func(string) (string, error) { return look }
	}
	tree := &fsRoot{dir: "/srv/rootfs"}

	// On the host, visudo is required.
	if bin, skipped, err := (&sudoersConfig{lookPath: found}).visudo(hostRoot); bin != fakeVisudoPath || skipped != "" || err != nil {
		t.Errorf("host with visudo: %q, %q, %v", bin, skipped, err)
	}
	if _, _, err := (&sudoersConfig{lookPath: missing}).visudo(hostRoot); !errors.Is(err, errVisudoMissing) {
		t.Errorf("host without visudo: err = %v, want errVisudoMissing", err)
	}
	// Below root_dir, a tree without visudo, or a host without one, skips
	// the check with a reason; the host's visudo is run otherwise.
	if bin, skipped, err := (&sudoersConfig{lookPath: found, lookPathInRoot: inRoot(missing)}).visudo(tree); bin != "" || !strings.Contains(skipped, "not installed below root_dir") || err != nil {
		t.Errorf("tree without visudo: %q, %q, %v", bin, skipped, err)
	}
	if bin, skipped, err := (&sudoersConfig{lookPath: missing, lookPathInRoot: inRoot(found)}).visudo(tree); bin != "" || !strings.Contains(skipped, "never runs programs from the tree") || err != nil {
		t.Errorf("host without visudo, tree with one: %q, %q, %v", bin, skipped, err)
	}
	if bin, skipped, err := (&sudoersConfig{lookPath: found, lookPathInRoot: inRoot(found)}).visudo(tree); bin != fakeVisudoPath || skipped != "" || err != nil {
		t.Errorf("both with visudo: %q, %q, %v", bin, skipped, err)
	}
}
