package provider

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestValidateLogrotateName(t *testing.T) {
	for _, name := range []string{"myapp", "nginx", "app-1.2", "a_b+c@d", "0-early"} {
		if err := validateLogrotateName(name); err != nil {
			t.Errorf("validateLogrotateName(%q) = %v", name, err)
		}
	}
	for name, want := range map[string]string{
		"":                       "empty",
		".hidden":                "must not start",
		"-dash":                  "must not start",
		"a/b":                    "must consist",
		"../x":                   "must consist",
		"a b":                    "must consist",
		"app.bak":                `".bak"`,
		"app.dpkg-old":           `".dpkg-old"`,
		"app.rpmsave":            `".rpmsave"`,
		"app~":                   "must consist",
		"app,v":                  "must consist",
		"x.rhn-cfg-tmp-abc":      "rhn-cfg-tmp",
		strings.Repeat("a", 256): "at most 255",
	} {
		err := validateLogrotateName(name)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("validateLogrotateName(%q) = %v, want an error containing %q", name, err, want)
		}
	}
}

func TestValidateLogrotatePath(t *testing.T) {
	for _, p := range []string{"/var/log/app.log", "/var/log/app/*.log", "/var/log/a b/x.log", "/srv/[ab]?.log"} {
		if err := validateLogrotatePath(p); err != nil {
			t.Errorf("validateLogrotatePath(%q) = %v", p, err)
		}
	}
	for p, want := range map[string]string{
		"":                  "empty",
		"var/log/x":         "absolute",
		"/var/log/":         "not end",
		"/var//log/x":       `"//"`,
		"/var/log/../x":     `".."`,
		"/var/./log":        `"."`,
		"/var/log/\"x":      `"\""`,
		"/var/log/x{":       `"{"`,
		"/var/log/#x":       `"#"`,
		"/var/log/x\\y":     `"\\"`,
		"/var/log/x\ny.log": "control",
	} {
		err := validateLogrotatePath(p)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("validateLogrotatePath(%q) = %v, want an error containing %q", p, err, want)
		}
	}
}

func TestValidateLogrotateDirective(t *testing.T) {
	for _, d := range []string{"maxsize 100M", "dateext", "su root adm", "olddir /var/log/old", "create", "nocreate"} {
		if err := validateLogrotateDirective(d); err != nil {
			t.Errorf("validateLogrotateDirective(%q) = %v", d, err)
		}
	}
	for d, want := range map[string]string{
		"":                  "empty",
		" dateext":          "white space",
		"# comment":         "comment",
		"postrotate":        "scripts",
		"prerotate":         "scripts",
		"endscript":         "scripts",
		"lastaction":        "scripts",
		"include /etc/x":    "include",
		"} /var/log/x {":    `"{"`,
		"dateext\nweekly":   "single line",
		"maxsize 1\x00":     "single line",
		"firstaction   foo": "scripts",
	} {
		err := validateLogrotateDirective(d)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("validateLogrotateDirective(%q) = %v, want an error containing %q", d, err, want)
		}
	}
}

func TestLogrotateSpecValidate(t *testing.T) {
	valid := func() logrotateSpec { return logrotateSpec{paths: []string{"/var/log/app.log"}} }
	for _, tc := range []struct {
		name     string
		modify   func(*logrotateSpec)
		attr     string
		contains string
	}{
		{"no paths", func(s *logrotateSpec) { s.paths = nil }, "paths", "at least one"},
		{"duplicate path", func(s *logrotateSpec) { s.paths = append(s.paths, s.paths[0]) }, "paths", "twice"},
		{"frequency", func(s *logrotateSpec) { s.frequency = "fortnightly" }, "frequency", "must be one of"},
		{"rotate", func(s *logrotateSpec) { s.rotate = ptr(int64(-2)) }, "rotate", "between -1"},
		{"create mode", func(s *logrotateSpec) { s.createMode = "644x" }, "create_mode", "octal"},
		{"owner without mode", func(s *logrotateSpec) { s.createOwner = "root" }, "create_owner", "requires create_mode"},
		{"group without owner", func(s *logrotateSpec) { s.createMode, s.createGroup = "0640", "adm" }, "create_owner", "requires create_owner"},
		{"owner without group", func(s *logrotateSpec) { s.createMode, s.createOwner = "0640", "root" }, "create_group", "requires create_group"},
		{"bad owner", func(s *logrotateSpec) { s.createMode, s.createOwner, s.createGroup = "0640", "0", "adm" }, "create_owner", "numeric"},
		{"script", func(s *logrotateSpec) { s.postrotate = "kill -HUP 1\n  endscript\n" }, "postrotate", "endscript"},
		{"conflicting extra", func(s *logrotateSpec) { s.compress = ptr(true); s.extra = []string{"nocompress"} }, "extra_directives", "conflicts with compress"},
		{"conflicting create", func(s *logrotateSpec) { s.createMode = "0600"; s.extra = []string{"nocreate"} }, "extra_directives", "conflicts with create_mode"},
		{"conflicting frequency", func(s *logrotateSpec) { s.frequency = "daily"; s.extra = []string{"weekly 1"} }, "extra_directives", "conflicts with frequency"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := valid()
			tc.modify(&s)
			attr, err := s.validate()
			if err == nil || attr != tc.attr || !strings.Contains(err.Error(), tc.contains) {
				t.Errorf("validate() = %q, %v; want %q and an error containing %q", attr, err, tc.attr, tc.contains)
			}
		})
	}
	// Extra directives may use the keywords of typed fields that are unset.
	s := valid()
	s.extra = []string{"weekly 1", "nocompress", "create", "ifempty"}
	if attr, err := s.validate(); err != nil {
		t.Errorf("validate() = %q, %v", attr, err)
	}
}

func fullLogrotateSpec() *logrotateSpec {
	return &logrotateSpec{
		paths:         []string{"/var/log/app/*.log", "/var/log/my app/out.log"},
		frequency:     "daily",
		rotate:        ptr(int64(14)),
		compress:      ptr(true),
		delaycompress: ptr(true),
		missingok:     ptr(true),
		notifempty:    ptr(false),
		sharedscripts: ptr(true),
		createMode:    "0640",
		createOwner:   "root",
		createGroup:   "adm",
		postrotate:    "systemctl kill -s HUP app.service\n  [ -f /run/x ] && kill -USR1 \"$(cat /run/x)\"\n",
		extra:         []string{"maxsize 100M", "dateext", "su root adm"},
	}
}

func TestLogrotateRender(t *testing.T) {
	want := logrotateFileHeader + `
/var/log/app/*.log "/var/log/my app/out.log" {
    daily
    rotate 14
    compress
    delaycompress
    missingok
    ifempty
    create 0640 root adm
    sharedscripts
    maxsize 100M
    dateext
    su root adm
    postrotate
systemctl kill -s HUP app.service
  [ -f /run/x ] && kill -USR1 "$(cat /run/x)"
    endscript
}
`
	if got := fullLogrotateSpec().render(); got != want {
		t.Errorf("render() =\n%s\nwant\n%s", got, want)
	}
	minimal := &logrotateSpec{paths: []string{"/var/log/x.log"}, createMode: "600", postrotate: "true"}
	wantMinimal := logrotateFileHeader + "\n/var/log/x.log {\n    create 600\n    postrotate\ntrue\n    endscript\n}\n"
	if got := minimal.render(); got != wantMinimal {
		t.Errorf("render() =\n%s\nwant\n%s", got, wantMinimal)
	}
}

func TestParseLogrotateFile_roundTrip(t *testing.T) {
	for _, s := range []*logrotateSpec{
		fullLogrotateSpec(),
		{paths: []string{"/var/log/x.log"}},
		{paths: []string{"/var/log/x.log"}, rotate: ptr(int64(0)), compress: ptr(false), notifempty: ptr(true), createMode: "0600", createOwner: "syslog", createGroup: "adm"},
		{paths: []string{"/var/log/x.log"}, extra: []string{"weekly 0", "create", "su root adm"}},
	} {
		got, err := parseLogrotateFile(s.render())
		if err != nil {
			t.Fatalf("parseLogrotateFile(%q) = %v", s.render(), err)
		}
		if !reflect.DeepEqual(got, s) {
			t.Errorf("parseLogrotateFile(render(%+v)) = %+v", s, got)
		}
	}
}

func TestParseLogrotateFile_foreign(t *testing.T) {
	// As Debian's rsyslog package ships it, apart from the second block.
	const content = `# comment
/var/log/syslog
/var/log/mail.log
{
	rotate 4
	weekly
	missingok
	notifempty
	compress
	delaycompress
	sharedscripts
	postrotate
		/usr/lib/rsyslog/rsyslog-rotate
	endscript
}
`
	got, err := parseLogrotateFile(content)
	if err != nil {
		t.Fatal(err)
	}
	want := &logrotateSpec{
		paths: []string{"/var/log/syslog", "/var/log/mail.log"}, frequency: "weekly", rotate: ptr(int64(4)),
		missingok: ptr(true), notifempty: ptr(true), compress: ptr(true), delaycompress: ptr(true), sharedscripts: ptr(true),
		postrotate: "\t\t/usr/lib/rsyslog/rsyslog-rotate\n",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseLogrotateFile = %+v, want %+v", got, want)
	}
	quoted, err := parseLogrotateFile("'/var/log/a b.log' \"/var/log/c d.log\" /var/log/e.log {\n}\n")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/var/log/a b.log", "/var/log/c d.log", "/var/log/e.log"}; !reflect.DeepEqual(quoted.paths, want) {
		t.Errorf("paths = %q, want %q", quoted.paths, want)
	}
}

func TestParseLogrotateFile_errors(t *testing.T) {
	for content, want := range map[string]string{
		"":                                   "no block",
		"# only a comment\n":                 "no block",
		"weekly\n/var/log/x {\n}\n":          "global directive",
		"/var/log/x {\n":                     "not closed",
		"/var/log/x { daily\n}\n":            "after \"{\"",
		"/var/log/x {\n}\n/var/log/y {\n}\n": "single block",
		"/var/log/x {\n  prerotate\n  true\n  endscript\n}\n": "prerotate scripts are not supported",
		"/var/log/x {\n  postrotate\n  true\n}\n":             "without endscript",
		"/var/log/x {\n  rotate many\n}\n":                    "invalid rotate",
		"/var/log/x {\n  compress\n  nocompress 1\n}\n":       "conflicts with compress",
		"relative {\n}\n":     "must be absolute",
		"\"/var/log/x {\n}\n": "unterminated quote",
		"/var/log/x {\n  postrotate\n  a\n  endscript\n  postrotate\n  b\n  endscript\n}\n": "second postrotate",
	} {
		_, err := parseLogrotateFile(content)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("parseLogrotateFile(%q) = %v, want an error containing %q", content, err, want)
		}
	}
}

// fakeLogrotate is a commandRunner standing in for logrotate -d. It prints
// output like logrotate's for the file it is given.
type fakeLogrotate struct {
	checked []string
	// output returns the exit code and output for the file's contents.
	output func(content, file string) (int, string)
}

func (f *fakeLogrotate) run(_ context.Context, spec execSpec) (*execResult, error) {
	if len(spec.Argv) != 5 || spec.Argv[1] != "-d" || spec.Argv[2] != "-s" || spec.Argv[3] != "/dev/null" {
		return nil, errors.New("fake logrotate: unexpected arguments " + strings.Join(spec.Argv, " "))
	}
	file := spec.Argv[4]
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	f.checked = append(f.checked, file)
	res := &execResult{Stdout: newCappedOutput(spec.MaxOutputBytes), Stderr: newCappedOutput(spec.MaxOutputBytes)}
	code, out := f.output(string(data), file)
	res.ExitCode = code
	_, _ = res.Stderr.Write([]byte(out))
	return res, nil
}

func TestCheckLogrotateFile_classifiesErrors(t *testing.T) {
	const tmp = "/etc/logrotate.d/.app.sysutils-tmp-1"
	for _, tc := range []struct {
		name   string
		code   int
		out    string
		reject string
	}{
		{"clean", 0, "reading config file x\n", ""},
		{"missing log file", 1, "considering log /var/log/x\nerror: stat of /var/log/x failed: No such file or directory\n", ""},
		{"insecure parent", 1, "error: skipping \"/var/log/x\" because parent directory has insecure permissions\n", ""},
		{"syntax", 1, "reading config file " + tmp + "\nerror: " + tmp + ":3 unknown option 'bogus' -- ignoring line\n", "error: /etc/logrotate.d/app:3 unknown option 'bogus'"},
		{"skipped block", 1, "error: " + tmp + ":4 unknown user 'nobody2'\nerror: found error in /var/log/x , skipping\n", "found error in /var/log/x"},
		{"unknown directive warning", 0, "warning: " + tmp + ":3 unknown option 'bogus' -- ignoring line\n", "warning: /etc/logrotate.d/app:3 unknown option 'bogus'"},
		{"other warning", 0, "warning: logrotate in debug mode does nothing except printing debug messages!\n", ""},
		{"ignored file", 1, "error: Ignoring " + tmp + " because the file owner is wrong\n", "Ignoring /etc/logrotate.d/app because"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := func(_ context.Context, spec execSpec) (*execResult, error) {
				res := &execResult{ExitCode: tc.code, Stdout: newCappedOutput(1 << 10), Stderr: newCappedOutput(1 << 10)}
				_, _ = res.Stderr.Write([]byte(tc.out))
				return res, nil
			}
			err := checkLogrotateFile(context.Background(), run, "/usr/sbin/logrotate", tmp, "/etc/logrotate.d/app")
			var lerr *logrotateError
			switch {
			case tc.reject == "" && err != nil:
				t.Errorf("checkLogrotateFile = %v, want nil", err)
			case tc.reject != "" && (!errors.As(err, &lerr) || !strings.Contains(lerr.output, tc.reject)):
				t.Errorf("checkLogrotateFile = %v, want a logrotateError containing %q", err, tc.reject)
			case tc.reject != "" && strings.Contains(lerr.output, tmp):
				t.Errorf("output %q names the temporary file", lerr.output)
			}
		})
	}
}

// TestCheckLogrotateFile_real checks the classification against the real
// logrotate, if it is installed.
func TestCheckLogrotateFile_real(t *testing.T) {
	bin, err := exec.LookPath("logrotate")
	if err != nil {
		t.Skip("logrotate is not installed")
	}
	dir := t.TempDir()
	check := func(content string) error {
		p := filepath.Join(dir, "conf")
		mustWrite(t, p, content)
		mustChmod(t, p, 0o644)
		return checkLogrotateFile(context.Background(), runCommand, bin, p, "/etc/logrotate.d/app")
	}
	missing := filepath.Join(dir, "missing", "*.log")
	good := (&logrotateSpec{paths: []string{missing}, frequency: "daily", rotate: ptr(int64(3)), createMode: "0640",
		postrotate: "true\n", extra: []string{"maxsize 10M"}}).render()
	if err := check(good); err != nil {
		t.Errorf("valid file with a missing log file: %v", err)
	}
	for _, bad := range []string{
		(&logrotateSpec{paths: []string{missing}, extra: []string{"bogusdirective"}}).render(),
		(&logrotateSpec{paths: []string{missing}, extra: []string{"rotate many"}}).render(),
		(&logrotateSpec{paths: []string{missing}, createMode: "0640", createOwner: "sysutils-no-such-user", createGroup: "root"}).render(),
	} {
		var lerr *logrotateError
		if err := check(bad); !errors.As(err, &lerr) || !strings.Contains(lerr.output, "/etc/logrotate.d/app") {
			t.Errorf("check(%q) = %v, want a logrotateError naming the drop-in", bad, err)
		}
	}
}

// TestLogrotateTempFileSkipped checks that the temporary file of a drop-in
// has a name logrotate skips when it reads logrotate.d, so that a
// concurrent run can't take it for a second block for the same logs.
func TestLogrotateTempFileSkipped(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logrotate.d")
	uid, gid := processOwner()
	spec := dropInSpec{mode: logrotateFileMode, dirMode: 0o755, maxSize: maxLogrotateFileSize, uid: uid, gid: gid, tmpSuffix: "~"}
	var tmps []string
	check := func(tmp string) error {
		tmps = append(tmps, tmp)
		return nil
	}
	p := filepath.Join(dir, "app")
	if err := spec.write(p, []byte("/var/log/x {\n}\n"), true, check); err != nil {
		t.Fatal(err)
	}
	if len(tmps) != 1 || filepath.Dir(tmps[0]) != dir {
		t.Fatalf("checked %q, want one file in %s", tmps, dir)
	}
	name := filepath.Base(tmps[0])
	if !strings.HasSuffix(name, "~") {
		t.Errorf("temporary file %q does not end with \"~\"", name)
	}
	if validateLogrotateName(name) == nil {
		t.Errorf("logrotate would read the temporary file %q", name)
	}
	assertFileContent(t, p, "/var/log/x {\n}\n")
}
