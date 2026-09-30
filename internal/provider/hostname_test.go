package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestValidateHostname_rfc1123(t *testing.T) {
	label63 := strings.Repeat("a", 63)
	// 4 labels of 63 plus 3 dots is 255; trim to exactly 253.
	name253 := label63 + "." + label63 + "." + label63 + "." + strings.Repeat("b", 61)
	for _, ok := range []string{"web1", "web-1", "Web1.Example.COM", "1host", "a", label63, name253, "xn--bcher-kva.example"} {
		if err := validateHostname(ok); err != nil {
			t.Errorf("validateHostname(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{
		"", "-web", "web-", "web.-a", "web.a-", "web_1", "web 1", "web.", ".web", "web..example",
		strings.Repeat("a", 64), name253 + "c", "10.0.0.5", "::1", "wéb", "web\n1", "web/1",
	} {
		if err := validateHostname(bad); err == nil {
			t.Errorf("validateHostname(%q) = nil, want an error", bad)
		}
	}
}

func TestValidateKernelHostname(t *testing.T) {
	if err := validateKernelHostname(strings.Repeat("a", 64)); err != nil {
		t.Errorf("64 characters: %v", err)
	}
	if err := validateKernelHostname("a." + strings.Repeat("a", 63)); err == nil || !strings.Contains(err.Error(), "at most 64") {
		t.Errorf("65 characters: %v, want an error about the kernel's limit", err)
	}
}

func TestValidatePrettyHostname(t *testing.T) {
	for _, ok := range []string{"Web server 1", "Lennart's Laptop", "Büro-PC", `"quoted" $x`, strings.Repeat("x", 255)} {
		if err := validatePrettyHostname(ok); err != nil {
			t.Errorf("validatePrettyHostname(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", " lead", "trail ", "two\nlines", "tab\there", "bell\a", "\xff\xfe", "c1\u0085", strings.Repeat("x", 256)} {
		if err := validatePrettyHostname(bad); err == nil {
			t.Errorf("validatePrettyHostname(%q) = nil, want an error", bad)
		}
	}
}

func TestStaticHostname(t *testing.T) {
	for in, want := range map[string]string{
		"web1\n":                         "web1",
		"web1":                           "web1",
		"  web1  \n":                     "web1",
		"# comment\n\nweb1\nignored\n":   "web1",
		"":                               "",
		"\n\n":                           "",
		"# only a comment\n":             "",
		"web1\r\n":                       "web1",
		"web1.example.com\nsecond\n":     "web1.example.com",
		"\t# indented comment\nhost-a\n": "host-a",
	} {
		if got := staticHostname(in); got != want {
			t.Errorf("staticHostname(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEnvValueQuoting(t *testing.T) {
	for _, tc := range []struct{ value, rendered string }{
		{"web1", "web1"},
		{"Web server 1", `"Web server 1"`},
		{`say "hi"`, `"say \"hi\""`},
		{`back\slash`, `"back\\slash"`},
		{"$HOME `cmd`", "\"\\$HOME \\`cmd\\`\""},
		{"it's", `"it's"`},
		{"Büro", "Büro"},
		{"", `""`},
	} {
		if got := quoteEnvValue(tc.value); got != tc.rendered {
			t.Errorf("quoteEnvValue(%q) = %s, want %s", tc.value, got, tc.rendered)
		}
		if got := parseEnvValue(tc.rendered); got != tc.value {
			t.Errorf("parseEnvValue(%s) = %q, want %q", tc.rendered, got, tc.value)
		}
	}
	for raw, want := range map[string]string{
		`'single $quoted'`: "single $quoted",
		`  bare  `:         "bare",
		`bare\ space`:      "bare space",
		`"unterminated`:    "unterminated",
		`"keep \n as is"`:  `keep \n as is`,
	} {
		if got := parseEnvValue(raw); got != want {
			t.Errorf("parseEnvValue(%s) = %q, want %q", raw, got, want)
		}
	}
}

func TestMachineInfoValue(t *testing.T) {
	content := "# managed elsewhere\nICON_NAME=computer-vm\nPRETTY_HOSTNAME=\"First\"\n  PRETTY_HOSTNAME = 'Second one'\nCHASSIS=vm\n"
	if v, ok := machineInfoValue(content, prettyHostnameKey); !ok || v != "Second one" {
		t.Errorf("machineInfoValue = %q, %v; want the last assignment", v, ok)
	}
	if v, ok := machineInfoValue("ICON_NAME=x\n# PRETTY_HOSTNAME=commented\n", prettyHostnameKey); ok {
		t.Errorf("machineInfoValue found %q in a comment", v)
	}
}

func TestSetMachineInfoValue(t *testing.T) {
	str := func(s string) *string { return &s }
	for _, tc := range []struct {
		name, in string
		value    *string
		want     string
	}{
		{"append to empty", "", str("Web 1"), "PRETTY_HOSTNAME=\"Web 1\"\n"},
		{"append keeps others", "ICON_NAME=computer\nCHASSIS=vm", str("web"), "ICON_NAME=computer\nCHASSIS=vm\nPRETTY_HOSTNAME=web\n"},
		{"replace in place, drop duplicates", "PRETTY_HOSTNAME=a\nCHASSIS=vm\nPRETTY_HOSTNAME=b\n", str("c d"), "PRETTY_HOSTNAME=\"c d\"\nCHASSIS=vm\n"},
		{"keep CRLF", "PRETTY_HOSTNAME=a\r\nCHASSIS=vm\r\n", str("b"), "PRETTY_HOSTNAME=b\r\nCHASSIS=vm\r\n"},
		{"remove", "ICON_NAME=x\nPRETTY_HOSTNAME=a\n# note\nPRETTY_HOSTNAME=b\n", nil, "ICON_NAME=x\n# note\n"},
		{"remove absent", "ICON_NAME=x\n", nil, "ICON_NAME=x\n"},
		{"comment untouched", "# PRETTY_HOSTNAME=old\n", str("new"), "# PRETTY_HOSTNAME=old\nPRETTY_HOSTNAME=new\n"},
	} {
		got := setMachineInfoValue(tc.in, prettyHostnameKey, tc.value)
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
		if tc.value != nil {
			if v, ok := machineInfoValue(got, prettyHostnameKey); !ok || v != *tc.value {
				t.Errorf("%s: reads back as %q, %v", tc.name, v, ok)
			}
		}
	}
}

func TestHostnameHostsNames(t *testing.T) {
	for name, want := range map[string][]string{
		"web1":                  {"web1"},
		"web1.example.com":      {"web1.example.com", "web1"},
		"Web1.Example.com":      {"Web1.Example.com", "Web1"},
		"a.b":                   {"a.b", "a"},
		"localhost.localdomain": {"localhost.localdomain", "localhost"},
	} {
		if got := hostnameHostsNames(name); !slices.Equal(got, want) {
			t.Errorf("hostnameHostsNames(%q) = %q, want %q", name, got, want)
		}
	}
}

// fakeProcDir returns a /proc with namespace links for PID 1 and self.
func fakeProcDir(t *testing.T, initUTS, selfUTS string) string {
	t.Helper()
	d := t.TempDir()
	for pid, uts := range map[string]string{"1": initUTS, "self": selfUTS} {
		mustMkdir(t, filepath.Join(d, pid, "ns"))
		mustSymlink(t, "uts:["+uts+"]", filepath.Join(d, pid, "ns", "uts"))
		mustSymlink(t, "mnt:[4026531841]", filepath.Join(d, pid, "ns", "mnt"))
	}
	return d
}

func TestHostnameUseHostnamectl(t *testing.T) {
	found := func(string) (string, error) { return "/usr/bin/hostnamectl", nil }
	notFound := func(string) (string, error) { return "", errors.New("not found") }
	same := fakeProcDir(t, "4026531838", "4026531838")
	other := fakeProcDir(t, "4026531838", "4026532999")
	rooted, _ := newFSRoot(t.TempDir())
	for _, tc := range []struct {
		name string
		root *fsRoot
		cfg  *hostnameConfig
		want bool
	}{
		{"systemd", hostRoot, &hostnameConfig{runDir: fakeSystemdRunDir(t), procDir: same, lookPath: found}, true},
		{"unreadable /proc/1/ns", hostRoot, &hostnameConfig{runDir: fakeSystemdRunDir(t), procDir: t.TempDir(), lookPath: found}, true},
		{"no hostnamectl", hostRoot, &hostnameConfig{runDir: fakeSystemdRunDir(t), procDir: same, lookPath: notFound}, false},
		{"not booted with systemd", hostRoot, &hostnameConfig{runDir: t.TempDir(), procDir: same, lookPath: found}, false},
		{"own UTS namespace", hostRoot, &hostnameConfig{runDir: fakeSystemdRunDir(t), procDir: other, lookPath: found}, false},
		{"root_dir", rooted, &hostnameConfig{runDir: fakeSystemdRunDir(t), procDir: same, lookPath: found}, false},
	} {
		if got := tc.cfg.useHostnamectl(tc.root); got != tc.want {
			t.Errorf("%s: useHostnamectl = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSharesNamespacesWithInit_real(t *testing.T) {
	// The test process normally shares PID 1's namespaces; in the
	// hostname sandbox it does not. Either way this must not panic, and
	// self compared with self must be equal.
	d := t.TempDir()
	mustMkdir(t, filepath.Join(d, "1"))
	mustSymlink(t, "/proc/self/ns", filepath.Join(d, "1", "ns"))
	mustSymlink(t, "/proc/self", filepath.Join(d, "self"))
	if !sharesNamespacesWithInit(d, "uts", "mnt") {
		t.Error("the process does not share its own namespaces")
	}
}

// fakeHostnamectl records hostnamectl calls and writes the files as
// systemd-hostnamed would, below root.
type fakeHostnamectl struct {
	t      *testing.T
	root   string
	calls  [][]string
	fail   bool
	kernel string
}

func (f *fakeHostnamectl) config(t *testing.T) *hostnameConfig {
	return &hostnameConfig{
		runDir:     fakeSystemdRunDir(t),
		procDir:    fakeProcDir(t, "1", "1"),
		lookPath:   func(string) (string, error) { return "/usr/bin/hostnamectl", nil },
		rootIsHost: true,
		getKernel:  func() (string, error) { return f.kernel, nil },
		setKernel:  func(s string) error { f.kernel = s; return nil },
		run: func(_ context.Context, spec execSpec) (*execResult, error) {
			if spec.Timeout <= 0 {
				f.t.Error("hostnamectl run without a timeout")
			}
			f.calls = append(f.calls, spec.Argv)
			res := &execResult{Stdout: newCappedOutput(1024), Stderr: newCappedOutput(1024)}
			if f.fail {
				res.ExitCode = 1
				_, _ = res.Stderr.Write([]byte("Could not set property: Access denied\n"))
				return res, nil
			}
			value := spec.Argv[len(spec.Argv)-1]
			switch spec.Argv[2] {
			case "--static":
				mustWrite(f.t, filepath.Join(f.root, "etc", "hostname"), value+"\n")
				f.kernel = value
			case "--pretty":
				mustWrite(f.t, filepath.Join(f.root, "etc", "machine-info"), "PRETTY_HOSTNAME="+quoteEnvValue(value)+"\n")
			}
			return res, nil
		},
	}
}

func TestSetHostname_rootDirWritesFilesOnly(t *testing.T) {
	ctx := context.Background()
	root := testRootDir(t)
	mustMkdir(t, filepath.Join(root, "etc"))
	mustWrite(t, filepath.Join(root, "etc", "hostname"), "debian\n")
	mustWrite(t, filepath.Join(root, "etc", "hosts"), "127.0.0.1\tlocalhost\n127.0.1.1\tdebian\n\n::1 localhost ip6-localhost\n")
	mustWrite(t, filepath.Join(root, "etc", "machine-info"), "ICON_NAME=computer-vm\n")
	r, _ := newFSRoot(root)
	// Neither hostnamectl nor the kernel are touched below a root_dir,
	// even if the fakes say systemd is running.
	cfg := &hostnameConfig{
		runDir:   fakeSystemdRunDir(t),
		lookPath: func(string) (string, error) { return "/usr/bin/hostnamectl", nil },
		getKernel: func() (string, error) {
			t.Error("kernel hostname read for a root_dir")
			return "", errors.New("unexpected")
		},
		setKernel: func(string) error { t.Error("kernel hostname set for a root_dir"); return errors.New("unexpected") },
		run: func(context.Context, execSpec) (*execResult, error) {
			t.Error("hostnamectl run for a root_dir")
			return nil, errors.New("unexpected")
		},
	}
	before, err := readHostnameSnapshot(cfg, r)
	if err != nil {
		t.Fatal(err)
	}
	if before.static() != "debian" || before.HostsLine == nil || *before.HostsLine != "127.0.1.1\tdebian" || before.Pretty != nil || before.Kernel != "" {
		t.Fatalf("snapshot = %+v", before)
	}

	pretty := "Web server 1"
	// A name longer than the kernel's limit is fine for a tree.
	long := strings.Repeat("a", 60) + ".example.com"
	for _, name := range []string{"web1.example.com", long} {
		warnings, err := setHostname(ctx, cfg, r, hostnameSpec{Static: name, Pretty: &pretty, HostsLine: true, PreviousNames: []string{"debian", "web1.example.com"}})
		if err != nil || len(warnings) > 0 {
			t.Fatalf("setHostname(%q) = %v, %v", name, warnings, err)
		}
		checkFileContentT(t, filepath.Join(root, "etc", "hostname"), name+"\n")
		short, _, _ := strings.Cut(name, ".")
		checkFileContentT(t, filepath.Join(root, "etc", "hosts"), "127.0.0.1\tlocalhost\n127.0.1.1\t"+name+" "+short+"\n\n::1 localhost ip6-localhost\n")
		checkFileContentT(t, filepath.Join(root, "etc", "machine-info"), "ICON_NAME=computer-vm\nPRETTY_HOSTNAME=\"Web server 1\"\n")
		if ok, err := hostnameHostsLineCurrent(filepath.Join(root, "etc", "hosts"), name); !ok || err != nil {
			t.Errorf("hostnameHostsLineCurrent = %v, %v", ok, err)
		}
	}

	// Setting the same again changes nothing.
	info1, _ := os.Lstat(filepath.Join(root, "etc", "hostname"))
	if _, err := setHostname(ctx, cfg, r, hostnameSpec{Static: long, Pretty: &pretty, HostsLine: true}); err != nil {
		t.Fatal(err)
	}
	if info2, _ := os.Lstat(filepath.Join(root, "etc", "hostname")); !os.SameFile(info1, info2) {
		t.Error("an unchanged hostname replaced /etc/hostname")
	}

	if _, err := restoreHostname(ctx, cfg, r, before, long, true, true); err != nil {
		t.Fatal(err)
	}
	checkFileContentT(t, filepath.Join(root, "etc", "hostname"), "debian\n")
	checkFileContentT(t, filepath.Join(root, "etc", "hosts"), "127.0.0.1\tlocalhost\n127.0.1.1\tdebian\n\n::1 localhost ip6-localhost\n")
	checkFileContentT(t, filepath.Join(root, "etc", "machine-info"), "ICON_NAME=computer-vm\n")
	checkNoTempFilesT(t, filepath.Join(root, "etc"))
}

func TestSetHostname_restoreRemovesWhatDidNotExist(t *testing.T) {
	ctx := context.Background()
	root := testRootDir(t)
	mustMkdir(t, filepath.Join(root, "etc"))
	r, _ := newFSRoot(root)
	before, err := readHostnameSnapshot(nil, r)
	if err != nil {
		t.Fatal(err)
	}
	pretty := "x"
	if _, err := setHostname(ctx, nil, r, hostnameSpec{Static: "web1", Pretty: &pretty, HostsLine: true}); err != nil {
		t.Fatal(err)
	}
	checkFileContentT(t, filepath.Join(root, "etc", "hosts"), "127.0.1.1\tweb1\n")
	if _, err := restoreHostname(ctx, nil, r, before, "web1", true, true); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"hostname", "machine-info"} {
		if _, err := os.Lstat(filepath.Join(root, "etc", f)); !os.IsNotExist(err) {
			t.Errorf("/etc/%s exists after restore (%v)", f, err)
		}
	}
	// The hosts file existed by then; only the line goes.
	checkFileContentT(t, filepath.Join(root, "etc", "hosts"), "")
}

func TestSetHostname_hostsLineKeepsOtherLines(t *testing.T) {
	root := testRootDir(t)
	mustMkdir(t, filepath.Join(root, "etc"))
	hosts := filepath.Join(root, "etc", "hosts")
	// Another 127.0.1.1 line that is not the hostname's stays, and so does
	// a mapping of the name to another address, which only warns.
	mustWrite(t, hosts, "127.0.1.1 other-service\n10.0.0.5 web1.example.com\n")
	r, _ := newFSRoot(root)
	warnings, err := setHostname(context.Background(), nil, r, hostnameSpec{Static: "web1.example.com", HostsLine: true, PreviousNames: []string{"old"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0].Detail, `"web1.example.com" is mapped to 10.0.0.5 on line 2`) {
		t.Errorf("warnings = %+v, want one about line 2", warnings)
	}
	checkFileContentT(t, hosts, "127.0.1.1 other-service\n10.0.0.5 web1.example.com\n127.0.1.1 web1.example.com web1\n")
}

func TestSetHostname_refusesSymlinks(t *testing.T) {
	root := testRootDir(t)
	mustMkdir(t, filepath.Join(root, "etc"))
	outside := filepath.Join(t.TempDir(), "target")
	mustWrite(t, outside, "keep\n")
	mustSymlink(t, outside, filepath.Join(root, "etc", "hostname"))
	r, _ := newFSRoot(root)
	if _, err := setHostname(context.Background(), nil, r, hostnameSpec{Static: "web1"}); err == nil {
		t.Error("setHostname wrote through a symlink at /etc/hostname")
	}
	checkFileContentT(t, outside, "keep\n")
}

func TestSetHostname_hostnamectl(t *testing.T) {
	ctx := context.Background()
	root := testRootDir(t)
	mustMkdir(t, filepath.Join(root, "etc"))
	mustWrite(t, filepath.Join(root, "etc", "hostname"), "old\n")
	f := &fakeHostnamectl{t: t, root: root, kernel: "old"}
	cfg := f.config(t)
	r, _ := newFSRoot(root)
	before, err := readHostnameSnapshot(cfg, r)
	if err != nil || before.Kernel != "old" {
		t.Fatalf("snapshot = %+v, %v", before, err)
	}
	pretty := "Web $1"
	warnings, err := setHostname(ctx, cfg, r, hostnameSpec{Static: "web1", Pretty: &pretty})
	if err != nil || len(warnings) > 0 {
		t.Fatalf("setHostname = %v, %v", warnings, err)
	}
	want := [][]string{
		{"hostnamectl", "--no-ask-password", "--static", "set-hostname", "--", "web1"},
		{"hostnamectl", "--no-ask-password", "--pretty", "set-hostname", "--", "Web $1"},
	}
	if !slices.EqualFunc(f.calls, want, slices.Equal) {
		t.Errorf("calls = %q, want %q", f.calls, want)
	}
	checkFileContentT(t, filepath.Join(root, "etc", "hostname"), "web1\n")
	if f.kernel != "web1" {
		t.Errorf("kernel = %q", f.kernel)
	}

	// Nothing to do: no calls.
	f.calls = nil
	if _, err := setHostname(ctx, cfg, r, hostnameSpec{Static: "web1", Pretty: &pretty}); err != nil || len(f.calls) != 0 {
		t.Errorf("unchanged: calls = %q, err = %v", f.calls, err)
	}

	// The kernel hostname drifted: set directly, no hostnamectl needed.
	f.kernel = "dhcp-name"
	if _, err := setHostname(ctx, cfg, r, hostnameSpec{Static: "web1"}); err != nil || len(f.calls) != 0 || f.kernel != "web1" {
		t.Errorf("kernel drift: calls = %q, kernel = %q, err = %v", f.calls, f.kernel, err)
	}

	// hostnamectl fails: warned about, and the files and the kernel are
	// set directly.
	f.fail = true
	warnings, err = setHostname(ctx, cfg, r, hostnameSpec{Static: "web2", Pretty: &pretty})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0].Detail, "Access denied") || !strings.Contains(warnings[0].Detail, "written directly") {
		t.Errorf("warnings = %+v", warnings)
	}
	checkFileContentT(t, filepath.Join(root, "etc", "hostname"), "web2\n")
	if f.kernel != "web2" {
		t.Errorf("kernel = %q after failed hostnamectl", f.kernel)
	}

	// Restore uses hostnamectl for the old static name and puts back the
	// recorded kernel hostname and files.
	f.fail, f.calls = false, nil
	if _, err := restoreHostname(ctx, cfg, r, before, "web2", true, false); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || f.calls[0][5] != "old" {
		t.Errorf("restore calls = %q", f.calls)
	}
	checkFileContentT(t, filepath.Join(root, "etc", "hostname"), "old\n")
	// It did not exist before.
	if _, err := os.Lstat(filepath.Join(root, "etc", "machine-info")); !os.IsNotExist(err) {
		t.Errorf("/etc/machine-info exists after restore (%v)", err)
	}
	if f.kernel != "old" {
		t.Errorf("kernel = %q after restore", f.kernel)
	}
}

func TestSetHostname_kernelLimit(t *testing.T) {
	root := testRootDir(t)
	mustMkdir(t, filepath.Join(root, "etc"))
	f := &fakeHostnamectl{t: t, root: root, kernel: "old"}
	r, _ := newFSRoot(root)
	long := strings.Repeat("a", 60) + ".example.com"
	if _, err := setHostname(context.Background(), f.config(t), r, hostnameSpec{Static: long}); err == nil || !strings.Contains(err.Error(), "at most 64") {
		t.Errorf("err = %v, want the kernel's limit", err)
	}
	if len(f.calls) > 0 || f.kernel != "old" {
		t.Errorf("a refused name changed something: calls %q, kernel %q", f.calls, f.kernel)
	}
	if _, err := os.Lstat(filepath.Join(root, "etc", "hostname")); !os.IsNotExist(err) {
		t.Errorf("/etc/hostname written for a refused name (%v)", err)
	}
}

func checkFileContentT(t *testing.T, p, want string) {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Errorf("reading %s: %v", p, err)
		return
	}
	if string(data) != want {
		t.Errorf("%s = %q, want %q", p, data, want)
	}
}

func TestRestoreHostnameHostsLine_refusesMultiLineRecord(t *testing.T) {
	hosts := filepath.Join(t.TempDir(), "hosts")
	mustWrite(t, hosts, "127.0.1.1 web1\n")
	bad := "127.0.1.1 old\n10.6.6.6 bank.example"
	if err := restoreHostnameHostsLine(hosts, "web1", &bad); err == nil {
		t.Error("a recorded line with a newline was written back")
	}
	checkFileContentT(t, hosts, "127.0.1.1 web1\n")
}
