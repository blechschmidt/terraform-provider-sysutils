package provider

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// scriptedRunner is a commandRunner that answers commands from a script
// and records them. Each answer is chosen by the first rule whose prefix
// matches the command line.
type scriptedRunner struct {
	rules []scriptedRule
	specs []execSpec
}

type scriptedRule struct {
	prefix string // space-joined argv prefix
	exit   int
	stdout string
	stderr string
}

func (s *scriptedRunner) run(_ context.Context, spec execSpec) (*execResult, error) {
	s.specs = append(s.specs, spec)
	line := strings.Join(spec.Argv, " ")
	for _, r := range s.rules {
		if strings.HasPrefix(line, r.prefix) {
			res := &execResult{ExitCode: r.exit, Stdout: newCappedOutput(spec.MaxOutputBytes), Stderr: newCappedOutput(spec.MaxOutputBytes)}
			_, _ = res.Stdout.Write([]byte(r.stdout))
			_, _ = res.Stderr.Write([]byte(r.stderr))
			return res, nil
		}
	}
	return nil, errors.New("scriptedRunner: unexpected command " + line)
}

func (s *scriptedRunner) commands() []string {
	var out []string
	for _, spec := range s.specs {
		out = append(out, strings.Join(spec.Argv, " "))
	}
	return out
}

func checkCommands(t *testing.T, s *scriptedRunner, want ...string) {
	t.Helper()
	if got := s.commands(); !slices.Equal(got, want) {
		t.Errorf("commands:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func TestPackageEnvIsNoninteractive(t *testing.T) {
	s := &scriptedRunner{rules: []scriptedRule{{prefix: "apt-cache policy", stdout: helloPolicy}, {prefix: "apt-get"}}}
	a := newPackageBackend(packageManagerApt, s.run)
	if err := a.Install(context.Background(), "hello", ""); err != nil {
		t.Fatal(err)
	}
	env := s.specs[1].Env
	for _, want := range []string{"DEBIAN_FRONTEND=noninteractive", "LC_ALL=C", "NEEDRESTART_MODE=l", "APT_LISTCHANGES_FRONTEND=none"} {
		if !slices.Contains(env, want) {
			t.Errorf("environment lacks %s", want)
		}
	}
	if s.specs[1].Timeout != packageChangeTimeout || s.specs[1].MaxOutputBytes != packageOutputLimit {
		t.Errorf("timeout %s, output limit %d", s.specs[1].Timeout, s.specs[1].MaxOutputBytes)
	}
}

// helloPolicy is "apt-cache policy hello" output for a package in the index.
const helloPolicy = "hello:\n  Installed: (none)\n  Candidate: 2.10-3\n  Version table:\n     2.10-3 500\n"

func TestAptBackend(t *testing.T) {
	ctx := context.Background()
	aptGet := "-y -q -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold -o DPkg::Lock::Timeout=300"
	dpkgQuery := "dpkg-query --show --showformat=${Package}\\t${db:Status-Abbrev}\\t${Version}\\n -- "

	t.Run("query installed", func(t *testing.T) {
		s := &scriptedRunner{rules: []scriptedRule{{prefix: "dpkg-query", stdout: "hello\tii \t2.10-3build1\n"}}}
		info, err := newPackageBackend(packageManagerApt, s.run).Query(ctx, "hello")
		if err != nil || info != (packageInfo{Installed: true, Version: "2.10-3build1"}) {
			t.Fatalf("got %+v, %v", info, err)
		}
		checkCommands(t, s, dpkgQuery+"hello")
	})
	t.Run("query unknown", func(t *testing.T) {
		s := &scriptedRunner{rules: []scriptedRule{{prefix: "dpkg-query", exit: 1, stderr: "dpkg-query: no packages found matching hello\n"}}}
		info, err := newPackageBackend(packageManagerApt, s.run).Query(ctx, "hello")
		if err != nil || info.Installed {
			t.Fatalf("got %+v, %v", info, err)
		}
	})
	t.Run("query failure", func(t *testing.T) {
		s := &scriptedRunner{rules: []scriptedRule{{prefix: "dpkg-query", exit: 2, stderr: "dpkg-query: error: parsing file '/var/lib/dpkg/status'\n"}}}
		if _, err := newPackageBackend(packageManagerApt, s.run).Query(ctx, "hello"); err == nil || !strings.Contains(err.Error(), "parsing file") {
			t.Fatalf("got %v", err)
		}
	})
	policy := "apt-cache policy -- hello"
	t.Run("install", func(t *testing.T) {
		s := &scriptedRunner{rules: []scriptedRule{{prefix: policy, stdout: helloPolicy}, {prefix: "apt-get"}}}
		a := newPackageBackend(packageManagerApt, s.run)
		if err := a.Install(ctx, "hello", ""); err != nil {
			t.Fatal(err)
		}
		if err := a.Install(ctx, "hello", "1:2.10-3"); err != nil {
			t.Fatal(err)
		}
		if err := a.Upgrade(ctx, "hello"); err != nil {
			t.Fatal(err)
		}
		if err := a.UpdateCache(ctx); err != nil {
			t.Fatal(err)
		}
		checkCommands(t, s,
			policy,
			"apt-get install "+aptGet+" -- hello",
			policy,
			"apt-get install "+aptGet+" --allow-downgrades -- hello=1:2.10-3",
			policy,
			"apt-get install "+aptGet+" -- hello",
			"apt-get update -q -o DPkg::Lock::Timeout=300",
		)
	})
	t.Run("install failure", func(t *testing.T) {
		var out strings.Builder
		for i := range 30 {
			out.WriteString("noise line " + string(rune('a'+i%26)) + "\n")
		}
		out.WriteString("E: Unable to locate package hello\n")
		s := &scriptedRunner{rules: []scriptedRule{{prefix: policy, stdout: helloPolicy}, {prefix: "apt-get", exit: 100, stderr: out.String()}}}
		err := newPackageBackend(packageManagerApt, s.run).Install(ctx, "hello", "")
		if err == nil || !strings.Contains(err.Error(), "exit status 100") || !strings.HasSuffix(err.Error(), "E: Unable to locate package hello") {
			t.Fatalf("got %v", err)
		}
		if n := strings.Count(err.Error(), "\n"); n != packageErrorLines {
			t.Errorf("error quotes %d lines, want %d", n, packageErrorLines)
		}
	})
	t.Run("remove", func(t *testing.T) {
		s := &scriptedRunner{rules: []scriptedRule{{prefix: policy, stdout: helloPolicy}, {prefix: "apt-get remove"}}}
		if err := newPackageBackend(packageManagerApt, s.run).Remove(ctx, "hello"); err != nil {
			t.Fatal(err)
		}
		checkCommands(t, s, policy, "apt-get remove "+aptGet+" -- hello")
	})
	t.Run("up to date", func(t *testing.T) {
		for policy, want := range map[string]bool{
			"hello:\n  Installed: 2.10-3\n  Candidate: 2.10-3\n  Version table:\n":         true,
			"hello:\n  Installed: 2.10-2\n  Candidate: 2.10-3\n  Version table:\n":         false,
			"hello:\n  Installed: 2.10-2\n  Candidate: (none)\n  Version table:\n":         true,
			"hello:\n  Installed: 1:2.10-2\n  Candidate: 1:2.10-2\n  Version table:\n":     true,
			"hello:\n  Installed: 2.10-2\n  Candidate: 2.10-3\n  Version table:\n 2:1 x\n": false,
		} {
			s := &scriptedRunner{rules: []scriptedRule{{prefix: "apt-cache policy -- hello", stdout: policy}}}
			got, err := newPackageBackend(packageManagerApt, s.run).UpToDate(ctx, "hello")
			if err != nil || got != want {
				t.Errorf("%q: got %v, %v; want %v", policy, got, err, want)
			}
		}
	})
}

func TestParseDpkgQuery(t *testing.T) {
	for out, want := range map[string]packageInfo{
		"":                                {},
		"hello\tii \t2.10\n":              {Installed: true, Version: "2.10"},
		"hello\tiW \t2.10\n":              {Installed: true, Version: "2.10"},
		"hello\tit \t2.10\n":              {Installed: true, Version: "2.10"},
		"hello\trc \t2.10\n":              {},
		"hello\tun \t\n":                  {},
		"hello\tiF \t2.10\n":              {},
		"hello\tiU \t2.10\n":              {},
		"hello\tiHR\t2.10\n":              {},
		"hellox\tii \t2.10\n":             {},
		"hello\tun \t\nhello\tii \t3.0\n": {Installed: true, Version: "3.0"},
	} {
		if got := parseDpkgQuery(out, "hello"); got != want {
			t.Errorf("parseDpkgQuery(%q) = %+v, want %+v", out, got, want)
		}
	}
}

func TestRPMBackend(t *testing.T) {
	ctx := context.Background()
	rpmQuery := "rpm --query --queryformat " + rpmQueryFormat + " -- "

	t.Run("query", func(t *testing.T) {
		s := &scriptedRunner{rules: []scriptedRule{{prefix: "rpm", stdout: "vim-minimal\t2:9.2.280-1.fc42\n"}}}
		info, err := newPackageBackend(packageManagerDnf, s.run).Query(ctx, "vim-minimal")
		if err != nil || info != (packageInfo{Installed: true, Version: "2:9.2.280-1.fc42"}) {
			t.Fatalf("got %+v, %v", info, err)
		}
		checkCommands(t, s, rpmQuery+"vim-minimal")
	})
	t.Run("query not installed", func(t *testing.T) {
		s := &scriptedRunner{rules: []scriptedRule{{prefix: "rpm", exit: 1, stdout: "package tree is not installed\n"}}}
		info, err := newPackageBackend(packageManagerYum, s.run).Query(ctx, "tree")
		if err != nil || info.Installed {
			t.Fatalf("got %+v, %v", info, err)
		}
	})
	t.Run("query ignores other packages", func(t *testing.T) {
		// rpm -q foo-1 also matches package foo, version 1.
		s := &scriptedRunner{rules: []scriptedRule{{prefix: "rpm", stdout: "foo\t1-1\n"}}}
		info, err := newPackageBackend(packageManagerDnf, s.run).Query(ctx, "foo-1")
		if err != nil || info.Installed {
			t.Fatalf("got %+v, %v", info, err)
		}
	})
	t.Run("install", func(t *testing.T) {
		s := &scriptedRunner{rules: []scriptedRule{{prefix: "yum"}}}
		r := newPackageBackend(packageManagerYum, s.run)
		if err := r.Install(ctx, "tree", ""); err != nil {
			t.Fatal(err)
		}
		if err := r.UpdateCache(ctx); err != nil {
			t.Fatal(err)
		}
		if err := r.Remove(ctx, "tree"); err != nil {
			t.Fatal(err)
		}
		checkCommands(t, s, "yum install -y -q -- tree", "yum makecache -q", "yum remove -y -q -- tree")
	})
	t.Run("install version", func(t *testing.T) {
		s := &scriptedRunner{rules: []scriptedRule{
			{prefix: "dnf install"},
			{prefix: "rpm", stdout: "tree\t2.2.1-1.fc42\n"},
		}}
		if err := newPackageBackend(packageManagerDnf, s.run).Install(ctx, "tree", "0:2.2.1-1.fc42"); err != nil {
			t.Fatal(err)
		}
		checkCommands(t, s, "dnf install -y -q -- tree-0:2.2.1-1.fc42", rpmQuery+"tree")
	})
	t.Run("install older version downgrades", func(t *testing.T) {
		s := &scriptedRunner{rules: []scriptedRule{
			{prefix: "dnf install"},
			{prefix: "dnf downgrade"},
			{prefix: "rpm", stdout: "tree\t2.2.2-1.fc42\n"},
		}}
		if err := newPackageBackend(packageManagerDnf, s.run).Install(ctx, "tree", "2.2.1-1.fc42"); err != nil {
			t.Fatal(err)
		}
		checkCommands(t, s, "dnf install -y -q -- tree-2.2.1-1.fc42", rpmQuery+"tree", "dnf downgrade -y -q -- tree-2.2.1-1.fc42")
	})
	t.Run("upgrade", func(t *testing.T) {
		s := &scriptedRunner{rules: []scriptedRule{
			{prefix: "rpm", stdout: "tree\t2.2.1-1.fc42\n"},
			{prefix: "dnf upgrade"},
		}}
		if err := newPackageBackend(packageManagerDnf, s.run).Upgrade(ctx, "tree"); err != nil {
			t.Fatal(err)
		}
		checkCommands(t, s, rpmQuery+"tree", "dnf upgrade -y -q -- tree")

		s = &scriptedRunner{rules: []scriptedRule{
			{prefix: "rpm", exit: 1, stdout: "package tree is not installed\n"},
			{prefix: "dnf install"},
		}}
		if err := newPackageBackend(packageManagerDnf, s.run).Upgrade(ctx, "tree"); err != nil {
			t.Fatal(err)
		}
		checkCommands(t, s, rpmQuery+"tree", "dnf install -y -q -- tree")
	})
	t.Run("up to date", func(t *testing.T) {
		for exit, want := range map[int]bool{0: true, 100: false} {
			s := &scriptedRunner{rules: []scriptedRule{{prefix: "dnf check-update -q -- tree", exit: exit}}}
			got, err := newPackageBackend(packageManagerDnf, s.run).UpToDate(ctx, "tree")
			if err != nil || got != want {
				t.Errorf("exit %d: got %v, %v; want %v", exit, got, err, want)
			}
		}
		s := &scriptedRunner{rules: []scriptedRule{{prefix: "dnf check-update", exit: 1, stderr: "Error: Failed to download metadata for repo 'fedora'\n"}}}
		if _, err := newPackageBackend(packageManagerDnf, s.run).UpToDate(ctx, "tree"); err == nil || !strings.Contains(err.Error(), "Failed to download metadata") {
			t.Errorf("got %v", err)
		}
	})
}

func TestApkBackend(t *testing.T) {
	ctx := context.Background()

	t.Run("query", func(t *testing.T) {
		s := &scriptedRunner{rules: []scriptedRule{{prefix: "apk info", stdout: "tree-2.2.1-r0\n"}}}
		info, err := newPackageBackend(packageManagerApk, s.run).Query(ctx, "tree")
		if err != nil || info != (packageInfo{Installed: true, Version: "2.2.1-r0"}) {
			t.Fatalf("got %+v, %v", info, err)
		}
		checkCommands(t, s, "apk info --installed --verbose -- tree")
	})
	t.Run("query not installed", func(t *testing.T) {
		s := &scriptedRunner{rules: []scriptedRule{{prefix: "apk info", exit: 1}}}
		info, err := newPackageBackend(packageManagerApk, s.run).Query(ctx, "tree")
		if err != nil || info.Installed {
			t.Fatalf("got %+v, %v", info, err)
		}
	})
	t.Run("changes", func(t *testing.T) {
		s := &scriptedRunner{rules: []scriptedRule{{prefix: "apk"}}}
		a := newPackageBackend(packageManagerApk, s.run)
		for _, err := range []error{
			a.Install(ctx, "tree", ""),
			a.Install(ctx, "tree", "2.2.1-r0"),
			a.Upgrade(ctx, "tree"),
			a.Remove(ctx, "tree"),
			a.UpdateCache(ctx),
		} {
			if err != nil {
				t.Fatal(err)
			}
		}
		checkCommands(t, s,
			"apk add --quiet --no-progress --wait 300 -- tree",
			"apk add --quiet --no-progress --wait 300 -- tree=2.2.1-r0",
			"apk add --quiet --no-progress --wait 300 --upgrade -- tree",
			"apk del --quiet --no-progress --wait 300 -- tree",
			"apk update --quiet --no-progress",
		)
	})
	t.Run("up to date", func(t *testing.T) {
		header := "Installed:                                Available:\n"
		for out, want := range map[string]bool{
			header + "tree-2.2.1-r0                           = 2.2.1-r0 \n": true,
			header + "tree-2.2.1-r0                           < 2.2.2-r0 \n": false,
			header: true,
		} {
			s := &scriptedRunner{rules: []scriptedRule{
				{prefix: "apk info", stdout: "tree-2.2.1-r0\n"},
				{prefix: "apk version -- tree", stdout: out},
			}}
			got, err := newPackageBackend(packageManagerApk, s.run).UpToDate(ctx, "tree")
			if err != nil || got != want {
				t.Errorf("%q: got %v, %v; want %v", out, got, err, want)
			}
		}
	})
}

func TestParseApkInfo(t *testing.T) {
	for out, want := range map[string]packageInfo{
		"":                           {},
		"tree-2.2.1-r0\n":            {Installed: true, Version: "2.2.1-r0"},
		"tree-doc-2.2.1-r0\n":        {},
		"py3-tree-1.0-r0\n":          {},
		"tree-doc-1-r0\ntree-2-r1\n": {Installed: true, Version: "2-r1"},
	} {
		if got := parseApkInfo(out, "tree"); got != want {
			t.Errorf("parseApkInfo(%q) = %+v, want %+v", out, got, want)
		}
	}
}

func TestPackageManagerDetection(t *testing.T) {
	lookPath := func(present ...string) func(string) (string, error) {
		return func(name string) (string, error) {
			if slices.Contains(present, name) {
				return "/usr/bin/" + name, nil
			}
			return "", errors.New("not found")
		}
	}
	for _, tc := range []struct {
		present []string
		kind    string
		want    string
		err     string
	}{
		{[]string{"apt-get", "apt-cache", "dpkg-query", "rpm"}, "auto", "apt", ""},
		// rpm alone, as on Debian with the rpm package, is not dnf.
		{[]string{"apt-get", "apt-cache", "dpkg-query", "rpm"}, "", "apt", ""},
		{[]string{"dnf", "yum", "rpm"}, "auto", "dnf", ""},
		{[]string{"yum", "rpm"}, "auto", "yum", ""},
		{[]string{"apk"}, "auto", "apk", ""},
		{[]string{"dnf"}, "auto", "", "no supported package manager found"},
		{nil, "auto", "", "no supported package manager found"},
		{[]string{"dnf", "rpm", "apk"}, "apk", "apk", ""},
		{[]string{"apt-get", "dpkg-query"}, "apt", "", "package manager apt is not available: apt-cache not found in PATH"},
		{[]string{"apk"}, "zypper", "", "unsupported package manager"},
	} {
		cfg := &packageConfig{lookPath: lookPath(tc.present...)}
		m, err := cfg.resolve(tc.kind)
		switch {
		case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Errorf("%v %s: got error %v, want %q", tc.present, tc.kind, err, tc.err)
		case tc.err == "" && err != nil:
			t.Errorf("%v %s: %v", tc.present, tc.kind, err)
		case tc.err == "" && m.Kind() != tc.want:
			t.Errorf("%v %s: got %s, want %s", tc.present, tc.kind, m.Kind(), tc.want)
		}
	}
}

func TestPackageUpdateCacheOnce(t *testing.T) {
	s := &scriptedRunner{rules: []scriptedRule{{prefix: "apt-get update"}, {prefix: "apk update"}}}
	cfg := &packageConfig{}
	apt, apk := newPackageBackend(packageManagerApt, s.run), newPackageBackend(packageManagerApk, s.run)
	for _, m := range []packageManager{apt, apt, apk, apt, apk} {
		if err := cfg.updateCacheOnce(context.Background(), m); err != nil {
			t.Fatal(err)
		}
	}
	checkCommands(t, s, "apt-get update -q -o DPkg::Lock::Timeout=300", "apk update --quiet --no-progress")

	// A failed refresh is retried by the next resource.
	s = &scriptedRunner{rules: []scriptedRule{{prefix: "apt-get update", exit: 100, stderr: "E: Could not get lock\n"}}}
	cfg = &packageConfig{}
	apt = newPackageBackend(packageManagerApt, s.run)
	for range 2 {
		if err := cfg.updateCacheOnce(context.Background(), apt); err == nil {
			t.Fatal("no error")
		}
	}
	if n := len(s.specs); n != 2 {
		t.Errorf("ran %d commands, want 2", n)
	}
}

func TestValidatePackageNameAndVersion(t *testing.T) {
	for _, tc := range []struct {
		kind, name string
		ok         bool
	}{
		{"auto", "hello", true},
		{"auto", "g++", true},
		{"auto", "libstdc++6", true},
		{"auto", "python3.12", true},
		{"auto", "NetworkManager", true},
		{"auto", "perl_5", true},
		{"apt", "NetworkManager", false},
		{"apt", "x", false},
		{"apt", "perl_5", false},
		{"dnf", "NetworkManager", true},
		{"auto", "", false},
		{"auto", "-y", false},
		{"auto", ".hidden", false},
		{"auto", "a b", false},
		{"auto", "a;b", false},
		{"auto", "a$(id)", false},
		{"auto", "a*", false},
		{"auto", "a/b", false},
		{"auto", "a:amd64", false},
		{"auto", "a=1", false},
		{"auto", "a>1", false},
		{"auto", "python3dist(foo)", false},
		{"auto", "a\nb", false},
		{"auto", strings.Repeat("a", 128), true},
		{"auto", strings.Repeat("a", 129), false},
	} {
		if err := validatePackageNameFor(tc.kind, tc.name); (err == nil) != tc.ok {
			t.Errorf("validatePackageNameFor(%s, %q) = %v, want ok=%v", tc.kind, tc.name, err, tc.ok)
		}
	}
	for _, tc := range []struct {
		kind, version string
		ok            bool
	}{
		{"apt", "2.10-3build1", true},
		{"apt", "1:2.10-3", true},
		{"apt", "1.0~rc1+dfsg-2ubuntu0.1", true},
		{"apt", "v1", false},
		{"apt", "1.0_1", false},
		{"dnf", "2.2.1-1.fc42", true},
		{"dnf", "2:9.2.280-1.fc42", true},
		{"yum", "1.8.0-10.el9", true},
		{"dnf", "1.0^20230101git-1", true},
		{"dnf", "2.2.1", false},
		{"dnf", "1:2", false},
		{"apk", "2.2.1-r0", true},
		{"apk", "1.0_alpha1-r12", true},
		{"apk", "2.2.1", false},
		{"auto", "", false},
		{"auto", "-1", false},
		{"auto", "1 2", false},
		{"auto", "1;2", false},
		{"auto", "=1", false},
		{"auto", "1*", false},
		{"auto", ">=1", false},
	} {
		if err := validatePackageVersionFor(tc.kind, tc.version); (err == nil) != tc.ok {
			t.Errorf("validatePackageVersionFor(%s, %q) = %v, want ok=%v", tc.kind, tc.version, err, tc.ok)
		}
	}
}

func TestSamePackageVersion(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"1.0-1", "1.0-1", true},
		{"0:1.0-1", "1.0-1", true},
		{"1.0-1", "0:1.0-1", true},
		{"1:1.0-1", "1.0-1", false},
		{"1.0-1", "1.0-2", false},
		{"10:1.0", "0:1.0", false},
	} {
		if got := samePackageVersion(tc.a, tc.b); got != tc.want {
			t.Errorf("samePackageVersion(%q, %q) = %v", tc.a, tc.b, got)
		}
	}
}
