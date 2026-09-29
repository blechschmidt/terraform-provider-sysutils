package provider

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func TestValidateCronSchedule(t *testing.T) {
	valid := []string{
		"* * * * *",
		"0 0 * * *",
		"30 2 * * 1-5",
		"*/15 * * * *",
		"0 9-17/2 * * mon-fri",
		"0,15,30,45 0-23 1,15 jan-jun,dec 0,7",
		"59 23 31 12 7",
		"0 0 1 JAN SUN",
		"5 4 * Feb Sat",
		"0-59/59 */23 */31 */12 */7",
		"@reboot", "@yearly", "@annually", "@monthly", "@weekly", "@daily", "@midnight", "@hourly",
	}
	for _, s := range valid {
		if err := validateCronSchedule(s); err != nil {
			t.Errorf("validateCronSchedule(%q) = %v, want nil", s, err)
		}
	}
	invalid := map[string]string{
		"":                   "five fields",
		"* * * *":            "five fields",
		"* * * * * *":        "five fields",
		"*  * * * *":         "five fields",
		" * * * * *":         "five fields",
		"* * * * * ":         "five fields",
		"*\t* * * *":         "five fields",
		"60 * * * *":         "out of range",
		"* 24 * * *":         "out of range",
		"* * 0 * *":          "out of range",
		"* * 32 * *":         "out of range",
		"* * * 0 *":          "out of range",
		"* * * 13 *":         "out of range",
		"* * * * 8":          "out of range",
		"-1 * * * *":         "not a number",
		"+1 * * * *":         "not a number",
		"a * * * *":          "not a number",
		"* * mon * *":        "not a number",
		"* * * * foo":        "three-letter name",
		"* * * * monday":     "three-letter name",
		"5-1 * * * *":        "must not end before it starts",
		"1-2-3 * * * *":      "not a number",
		"*/0 * * * *":        "step",
		"*/60 * * * *":       "step",
		"*/-1 * * * *":       "step",
		"*/+5 * * * *":       "step",
		"*/ * * * *":         "step",
		"5/10 * * * *":       "only allowed after",
		"1,,2 * * * *":       "empty list element",
		"1, * * * *":         "empty list element",
		"? * * * *":          "not a number",
		"L * * * *":          "not a number",
		"@every":             "not one of",
		"@DAILY":             "not one of",
		"@daily 5":           "not one of",
		"* * * * 1#2":        "not a number",
		"0 0 * * *\n0 * * *": "",
	}
	for s, want := range invalid {
		err := validateCronSchedule(s)
		if err == nil {
			t.Errorf("validateCronSchedule(%q) = nil, want an error", s)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("validateCronSchedule(%q) = %q, want it to contain %q", s, err, want)
		}
	}
}

func TestValidateCronName(t *testing.T) {
	for _, s := range []string{"backup", "a", "db-backup_2", "-x", "A_B", strings.Repeat("a", 255)} {
		if err := validateCronName(s); err != nil {
			t.Errorf("validateCronName(%q) = %v, want nil", s, err)
		}
	}
	for _, s := range []string{"", "backup.cron", ".hidden", "a/b", "..", "a b", "a~", "ä", strings.Repeat("a", 256), "a\n"} {
		if err := validateCronName(s); err == nil {
			t.Errorf("validateCronName(%q) = nil, want an error", s)
		}
	}
}

func TestValidateCronCommand(t *testing.T) {
	for _, s := range []string{"true", "/usr/bin/backup --all > /dev/null 2>&1", `date +\%F`, strings.Repeat("x", maxCronLineLen)} {
		if err := validateCronCommand(s); err != nil {
			t.Errorf("validateCronCommand(%q) = %v, want nil", s, err)
		}
	}
	for _, s := range []string{"", " true", "true ", "true\n", "a\nb", "a\rb", "a\x00b", strings.Repeat("x", maxCronLineLen+1)} {
		if err := validateCronCommand(s); err == nil {
			t.Errorf("validateCronCommand(%q) = nil, want an error", s)
		}
	}
}

func TestValidateCronEnv(t *testing.T) {
	for _, s := range []string{"PATH", "_x", "MAILTO", "a1"} {
		if err := validateCronEnvName(s); err != nil {
			t.Errorf("validateCronEnvName(%q) = %v, want nil", s, err)
		}
	}
	for _, s := range []string{"", "1A", "A-B", "A B", "A=B", "Ä"} {
		if err := validateCronEnvName(s); err == nil {
			t.Errorf("validateCronEnvName(%q) = nil, want an error", s)
		}
	}
	for _, s := range []string{"", "x", "a b", `"q"`, `'q'`, `say "hi"`, `it's`, `a"b'c`} {
		if err := validateCronEnvValue(s); err != nil {
			t.Errorf("validateCronEnvValue(%q) = %v, want nil", s, err)
		}
	}
	for _, s := range []string{"a\nb", "a\rb", "a\x00", `say "it's"`, `"it's"`} {
		if err := validateCronEnvValue(s); err == nil {
			t.Errorf("validateCronEnvValue(%q) = nil, want an error", s)
		}
	}
}

func TestCronJobRender(t *testing.T) {
	j := cronJob{
		schedule: "30 2 * * 1-5",
		user:     "backup",
		command:  "/usr/local/bin/backup --quiet",
		env: map[string]string{
			"PATH":   "/usr/local/bin:/usr/bin:/bin",
			"MAILTO": "",
			"GREET":  "hello world",
			"QUOTED": `say "hi"`,
		},
		comment:    "Nightly backup.\n\n  Owner: ops",
		hasComment: true,
	}
	want := `# Managed by Terraform (sysutils_cron_job). Manual changes will be reverted.
# Nightly backup.
#
#   Owner: ops
GREET="hello world"
MAILTO=""
PATH=/usr/local/bin:/usr/bin:/bin
QUOTED='say "hi"'
30 2 * * 1-5 backup /usr/local/bin/backup --quiet
`
	if got := string(j.render()); got != want {
		t.Errorf("render() =\n%s\nwant\n%s", got, want)
	}

	minimal := cronJob{schedule: "@daily", user: "root", command: "true"}
	if got, want := string(minimal.render()), cronFileHeader+"\n@daily root true\n"; got != want {
		t.Errorf("render() = %q, want %q", got, want)
	}
}

// TestCronJobRoundTrip checks that every valid job parses back from its
// rendered file unchanged, which drift detection relies on.
func TestCronJobRoundTrip(t *testing.T) {
	jobs := []cronJob{
		{schedule: "@daily", user: "root", command: "true"},
		{schedule: "*/5 * * * *", user: "www-data", command: `php /srv/cron.php >/dev/null 2>&1 # not a comment`},
		{schedule: "0 0 1 jan *", user: "root", command: "echo a=b", env: map[string]string{"A": "1", "B": "  padded  ", "C": "", "D": `"quoted"`, "E": `'single'`, "F": `a"b`, "G": "tab\there"}},
		{schedule: "@reboot", user: "root", command: "x", comment: "one", hasComment: true},
		{schedule: "@reboot", user: "root", command: "x", comment: "", hasComment: true},
		{schedule: "@reboot", user: "root", command: "x", comment: "a\n", hasComment: true},
		{schedule: "@reboot", user: "root", command: "x", comment: "#nested\n # indented", hasComment: true},
		{schedule: "@reboot", user: "root", command: "x", comment: "FOO=bar", hasComment: true},
	}
	for _, j := range jobs {
		if err := j.validate(); err != nil {
			t.Fatalf("validate(%+v) = %v", j, err)
		}
		p := parseCronFile(j.render())
		if !p.hasJob || p.jobs != 1 {
			t.Errorf("parse(render(%+v)): hasJob = %v, jobs = %d", j, p.hasJob, p.jobs)
		}
		want := j
		if want.env == nil {
			want.env = map[string]string{}
		}
		if !reflect.DeepEqual(p.job, want) {
			t.Errorf("parse(render(%+v)) = %+v", j, p.job)
		}
	}
}

func TestParseCronFile(t *testing.T) {
	t.Run("hand-written", func(t *testing.T) {
		data := "# m h dom mon dow user command\nSHELL = /bin/bash\nMAILTO=\"ops@example.com\"\n\n 17  *\t* * *  root   cd / && run-parts --report /etc/cron.hourly\n"
		p := parseCronFile([]byte(data))
		want := cronJob{
			schedule: "17 * * * *", user: "root", command: "cd / && run-parts --report /etc/cron.hourly",
			env:     map[string]string{"SHELL": "/bin/bash", "MAILTO": "ops@example.com"},
			comment: "m h dom mon dow user command", hasComment: true,
		}
		if !p.hasJob || p.jobs != 1 || !reflect.DeepEqual(p.job, want) {
			t.Errorf("parseCronFile = %+v, want job %+v", p, want)
		}
	})
	t.Run("several jobs", func(t *testing.T) {
		data := cronFileHeader + "\nA=1\n@hourly root first\nB=2\n# later comment\n@daily root second\n"
		p := parseCronFile([]byte(data))
		if !p.hasJob || p.jobs != 2 || p.job.command != "first" || !reflect.DeepEqual(p.job.env, map[string]string{"A": "1"}) {
			t.Errorf("parseCronFile = %+v", p)
		}
	})
	t.Run("no job", func(t *testing.T) {
		for _, data := range []string{"", "\n", cronFileHeader + "\n", "# only a comment\nA=1\n", "garbage\n", "* * * * * root\n"} {
			p := parseCronFile([]byte(data))
			if p.hasJob {
				t.Errorf("parseCronFile(%q) found job %+v", data, p.job)
			}
		}
	})
	t.Run("CRLF", func(t *testing.T) {
		p := parseCronFile([]byte("@daily root true\r\n"))
		if !p.hasJob || p.job.command != "true" {
			t.Errorf("parseCronFile = %+v", p)
		}
	})
}

func TestWriteCronFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cron.d")
	p := filepath.Join(dir, "job")
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())

	if err := writeCronFile(p, []byte("one\n"), uid, gid, true); err != nil {
		t.Fatal(err)
	}
	checkCronFile(t, p, "one\n")
	if err := writeCronFile(p, []byte("two\n"), uid, gid, true); !errors.Is(err, errCronFileExists) {
		t.Fatalf("second create: err = %v, want errCronFileExists", err)
	}

	// A group- and world-writable file is replaced with a 0644 one.
	if err := os.Chmod(p, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := writeCronFile(p, []byte("two\n"), uid, gid, false); err != nil {
		t.Fatal(err)
	}
	checkCronFile(t, p, "two\n")

	// A symlink is neither followed nor replaced.
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := writeCronFile(link, []byte("evil\n"), uid, gid, false); err == nil {
		t.Error("writing through a symlink succeeded")
	}
	if err := removeCronFile(link); err == nil {
		t.Error("removing a symlink succeeded")
	}
	if data, _ := os.ReadFile(target); string(data) != "keep\n" {
		t.Errorf("symlink target was modified: %q", data)
	}

	if err := removeCronFile(p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file still exists after remove: %v", err)
	}
	if err := removeCronFile(p); err != nil {
		t.Errorf("removing a missing file: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("leftover files in %s: %v", dir, entries)
	}
}

// checkCronFile checks that p contains want and has mode 0644.
func checkCronFile(t *testing.T, p, want string) {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Errorf("%s = %q, want %q", p, data, want)
	}
	info, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode() != cronFileMode {
		t.Errorf("%s has mode %v, want %v", p, info.Mode(), cronFileMode)
	}
	if st := info.Sys().(*syscall.Stat_t); st.Uid != uint32(os.Getuid()) {
		t.Errorf("%s is owned by %d, want %d", p, st.Uid, os.Getuid())
	}
}

// TestWriteCronFileChownsToRoot checks, as root, that a file owned by
// another user is replaced with one owned by root.
func TestWriteCronFileChownsToRoot(t *testing.T) {
	requireRoot(t)
	p := filepath.Join(t.TempDir(), "job")
	if err := os.WriteFile(p, []byte("old\n"), 0o664); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(p, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	if err := writeCronFile(p, []byte("new\n"), 0, 0, false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st := info.Sys().(*syscall.Stat_t); st.Uid != 0 || st.Gid != 0 || info.Mode() != cronFileMode {
		t.Errorf("file has owner %d:%d and mode %v, want 0:0 and %v", st.Uid, st.Gid, info.Mode(), cronFileMode)
	}
}
