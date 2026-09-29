package provider

// Regression tests for the security review of the package resource: names
// and versions that the package managers would take for something else, and
// the environment they run in.

import (
	"context"
	"strings"
	"testing"
)

// apt-get takes a trailing "-" as an instruction to remove the package:
// "apt-get install -- nginx-" removes nginx and everything that depends on
// it. Such a name must be refused before it reaches apt-get.
func TestValidatePackageNameAptTrailingDash(t *testing.T) {
	for _, name := range []string{"nginx-", "zstd-", "g--"} {
		if err := validatePackageNameFor(packageManagerApt, name); err == nil {
			t.Errorf("validatePackageNameFor(apt, %q) = nil", name)
		}
	}
	for _, name := range []string{"g++", "libstdc++6", "nginx-core"} {
		if err := validatePackageNameFor(packageManagerApt, name); err != nil {
			t.Errorf("validatePackageNameFor(apt, %q) = %v", name, err)
		}
	}
}

// dnf, yum and apk install an argument ending in ".rpm" or ".apk" from a
// local file of that name, even after "--". dnf does not check signatures
// of local packages by default.
func TestValidatePackageLocalFileNames(t *testing.T) {
	for _, tc := range []struct{ kind, name string }{
		{packageManagerDnf, "evil.rpm"},
		{packageManagerYum, "evil.RPM"},
		{packageManagerApk, "evil.apk"},
	} {
		if err := validatePackageNameFor(tc.kind, tc.name); err == nil {
			t.Errorf("validatePackageNameFor(%s, %q) = nil", tc.kind, tc.name)
		}
	}
	// The version ends the name-version argument of dnf and yum.
	if err := validatePackageVersionFor(packageManagerDnf, "1.0-1.rpm"); err == nil {
		t.Error(`validatePackageVersionFor(dnf, "1.0-1.rpm") = nil`)
	}
	// Names that merely contain the suffix are fine.
	for _, kind := range []string{packageManagerDnf, packageManagerApk} {
		if err := validatePackageNameFor(kind, "python3-rpm"); err != nil {
			t.Errorf("validatePackageNameFor(%s, python3-rpm) = %v", kind, err)
		}
	}
}

// apt takes a name that no package has for a pattern: "lib.+" and "zstd+"
// are regular expressions, and a trailing "+" or "-" is an install or
// remove instruction. None of that can be turned off with "--", so apt-get
// must only ever see names that apt-cache resolves to exactly that package.
func TestAptRefusesNamesThatAreNotPackages(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, policy, wantErr string
	}{
		// What apt-cache policy prints on Ubuntu 24.04 for these names.
		{"libzstd.+", "libzstd-jni1:\n  Installed: (none)\n  Candidate: 1.5.2-5+ds-3build1\n", "act on other packages"},
		{"zstd+", "librust-zstd-sys-2.0.9-dev:\n  Installed: (none)\n  Candidate: 2.0.9-1\n", "act on other packages"},
		{"hello", "hello:\n  Installed: (none)\n  Candidate: 2.10-3\nhello-traditional:\n  Installed: (none)\n", "act on other packages"},
		{"zstd-", "", "not in the package index"},
		{"nosuch", "", "not in the package index"},
	} {
		for op, call := range map[string]func(m packageManager) error{
			"install": func(m packageManager) error { return m.Install(ctx, tc.name, "") },
			"pin":     func(m packageManager) error { return m.Install(ctx, tc.name, "1.0-1") },
			"upgrade": func(m packageManager) error { return m.Upgrade(ctx, tc.name) },
			"remove":  func(m packageManager) error { return m.Remove(ctx, tc.name) },
		} {
			s := &scriptedRunner{rules: []scriptedRule{
				{prefix: "apt-cache policy", stdout: tc.policy},
				{prefix: "apt-get"},
			}}
			err := call(newPackageBackend(packageManagerApt, s.run))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s %q: got %v, want error containing %q", op, tc.name, err, tc.wantErr)
			}
			for _, c := range s.commands() {
				if strings.HasPrefix(c, "apt-get") {
					t.Errorf("%s %q ran %s", op, tc.name, c)
				}
			}
		}
	}
}

// A package of exactly the configured name is installed, including for a
// foreign architecture.
func TestAptPolicyAcceptsExactPackage(t *testing.T) {
	ctx := context.Background()
	for _, policy := range []string{
		"g++:\n  Installed: (none)\n  Candidate: 4:13.2.0-7ubuntu1\n  Version table:\n     4:13.2.0-7ubuntu1 500\n",
		"hello:\n  Installed: 2.10-3\n  Candidate: 2.10-3\nhello:i386:\n  Installed: (none)\n  Candidate: 2.10-3\n",
	} {
		name := policy[:strings.IndexByte(policy, ':')]
		s := &scriptedRunner{rules: []scriptedRule{{prefix: "apt-cache policy", stdout: policy}, {prefix: "apt-get"}}}
		if err := newPackageBackend(packageManagerApt, s.run).Install(ctx, name, ""); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := s.commands(); len(got) != 2 || !strings.HasSuffix(got[1], " -- "+name) {
			t.Errorf("%s: commands %q", name, got)
		}
	}
	// Only the native block decides whether the package is up to date.
	s := &scriptedRunner{rules: []scriptedRule{{prefix: "apt-cache policy", stdout: "hello:\n  Installed: 2.10-3\n  Candidate: 2.10-3\nhello:i386:\n  Installed: (none)\n  Candidate: 2.10-4\n"}}}
	if ok, err := newPackageBackend(packageManagerApt, s.run).UpToDate(ctx, "hello"); err != nil || !ok {
		t.Errorf("UpToDate = %v, %v; want true", ok, err)
	}
}

// Every package argument of dnf, yum and apk follows "--", and apk waits
// for its database lock like apt does.
func TestPackageArgumentsFollowDoubleDash(t *testing.T) {
	ctx := context.Background()
	s := &scriptedRunner{rules: []scriptedRule{
		{prefix: "rpm", stdout: "tree\t2.2.1-1.fc42\n"},
		{prefix: "dnf check-update", exit: 100},
		{prefix: "dnf"},
		{prefix: "apk info", stdout: "tree-2.2.1-r0\n"},
		{prefix: "apk version"},
		{prefix: "apk"},
	}}
	dnf, apk := newPackageBackend(packageManagerDnf, s.run), newPackageBackend(packageManagerApk, s.run)
	for _, err := range []error{
		dnf.Install(ctx, "tree", "2.2.2-1.fc42"),
		dnf.Upgrade(ctx, "tree"),
		dnf.Remove(ctx, "tree"),
		func() error { _, err := dnf.UpToDate(ctx, "tree"); return err }(),
		apk.Install(ctx, "tree", ""),
		apk.Install(ctx, "tree", "2.2.1-r0"),
		apk.Upgrade(ctx, "tree"),
		apk.Remove(ctx, "tree"),
		func() error { _, err := apk.UpToDate(ctx, "tree"); return err }(),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	checkCommands(t, s,
		"dnf install -y -q -- tree-2.2.2-1.fc42",
		"rpm --query --queryformat "+rpmQueryFormat+" -- tree",
		"dnf downgrade -y -q -- tree-2.2.2-1.fc42",
		"rpm --query --queryformat "+rpmQueryFormat+" -- tree",
		"dnf upgrade -y -q -- tree",
		"dnf remove -y -q -- tree",
		"dnf check-update -q -- tree",
		"apk add --quiet --no-progress --wait 300 -- tree",
		"apk add --quiet --no-progress --wait 300 -- tree=2.2.1-r0",
		"apk add --quiet --no-progress --wait 300 --upgrade -- tree",
		"apk del --quiet --no-progress --wait 300 -- tree",
		"apk info --installed --verbose -- tree",
		"apk version -- tree",
	)
}

// Package managers read configuration from the environment. The provider's
// environment must not redirect apt, dpkg, dnf or rpm, change the locale
// their output is parsed in, or let rpm load another user's macros.
func TestPackageEnvDropsInheritedOverrides(t *testing.T) {
	for k, v := range map[string]string{
		"APT_CONFIG":              "/tmp/evil/apt.conf",
		"DPKG_ADMINDIR":           "/tmp/evil/dpkg",
		"DPKG_ROOT":               "/tmp/evil",
		"DPKG_FRONTEND_LOCKED":    "1",
		"DPKG_FORCE":              "all",
		"DEBCONF_DB_OVERRIDE":     "File{/tmp/evil/debconf.dat}",
		"DNF_VAR_releasever":      "evil",
		"YUM0":                    "evil",
		"LANGUAGE":                "de",
		"LC_MESSAGES":             "de_DE.UTF-8",
		"LANG":                    "de_DE.UTF-8",
		"HOME":                    "/tmp/evil-home",
		"UCF_FORCE_CONFFNEW":      "1",
		"http_proxy":              "http://proxy.example:3128",
		"SYSUTILS_REVIEW5_MARKER": "x",
	} {
		t.Setenv(k, v)
	}
	s := &scriptedRunner{rules: []scriptedRule{{prefix: "dpkg-query", exit: 1, stderr: "dpkg-query: no packages found matching hello\n"}}}
	if _, err := newPackageBackend(packageManagerApt, s.run).Query(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	env := s.specs[0].Env
	vars := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if _, dup := vars[k]; dup {
			t.Errorf("%s is set twice", k)
		}
		vars[k] = v
	}
	for _, k := range []string{"APT_CONFIG", "DPKG_ADMINDIR", "DPKG_ROOT", "DPKG_FRONTEND_LOCKED", "DPKG_FORCE",
		"DEBCONF_DB_OVERRIDE", "DNF_VAR_releasever", "YUM0", "LANGUAGE", "LC_MESSAGES", "UCF_FORCE_CONFFNEW", "SYSUTILS_REVIEW5_MARKER"} {
		if _, ok := vars[k]; ok {
			t.Errorf("environment inherits %s", k)
		}
	}
	for k, want := range map[string]string{
		"LC_ALL":          "C",
		"LANG":            "C",
		"DEBIAN_FRONTEND": "noninteractive",
		"http_proxy":      "http://proxy.example:3128",
	} {
		if vars[k] != want {
			t.Errorf("%s = %q, want %q", k, vars[k], want)
		}
	}
	if vars["PATH"] == "" {
		t.Error("PATH is not set")
	}
	if h := vars["HOME"]; h == "" || h == "/tmp/evil-home" {
		t.Errorf("HOME = %q, want the home directory of the effective user", h)
	}
}

// Versions with epochs, revisions and the characters Debian and RPM allow
// are parsed as reported.
func TestPackageQueryParsesEpochsAndRevisions(t *testing.T) {
	if got := parseDpkgQuery("libc6\tii \t2.39-0ubuntu8.4\nvim\thi \t2:9.1.0016-1ubuntu7.8\n", "vim"); got != (packageInfo{Installed: true, Version: "2:9.1.0016-1ubuntu7.8"}) {
		t.Errorf("dpkg: %+v", got)
	}
	if got := parseDpkgQuery("x\tii \t1.0~rc1+dfsg-2+deb12u1\n", "x"); got.Version != "1.0~rc1+dfsg-2+deb12u1" {
		t.Errorf("dpkg: %+v", got)
	}
	if got := parseRPMQuery("vim-enhanced\t2:9.1.1000-1.fc42\n", "vim-enhanced"); got.Version != "2:9.1.1000-1.fc42" {
		t.Errorf("rpm: %+v", got)
	}
	if got := parseApkInfo("py3-foo-1.0_alpha1-r12\n", "py3-foo"); got.Version != "1.0_alpha1-r12" {
		t.Errorf("apk: %+v", got)
	}
	for _, v := range []string{"2:9.1.0016-1ubuntu7.8", "1.0~rc1+dfsg-2+deb12u1"} {
		if err := validatePackageVersionFor(packageManagerApt, v); err != nil {
			t.Errorf("apt %q: %v", v, err)
		}
	}
}
