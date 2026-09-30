package provider

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

const testPackageDataSource = "data.sysutils_package.test"

func packageDataHCL(name, extra string) string {
	return fmt.Sprintf(`
data "sysutils_package" "test" {
  name = %q
%s
}
`, name, extra)
}

func expectPackageData(attr string, v knownvalue.Check) statecheck.StateCheck {
	return statecheck.ExpectKnownValue(testPackageDataSource, tfjsonpath.New(attr), v)
}

// checkPackageCallsRepeat checks that the calls made to f so far, reads
// included, are one or more repetitions of want. Every Terraform command
// starts a new provider process, which reads the data sources again.
func checkPackageCallsRepeat(f *fakePackageManager, want ...string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		ok := len(f.calls) > 0 && len(f.calls)%len(want) == 0
		for i := 0; ok && i < len(f.calls); i += len(want) {
			ok = slices.Equal(f.calls[i:i+len(want)], want)
		}
		if !ok {
			return fmt.Errorf("package manager calls:\n  %s\nwant repetitions of:\n  %s", strings.Join(f.calls, "\n  "), strings.Join(want, "\n  "))
		}
		return nil
	}
}

// checkOnlyUpdates checks that the only changes f saw were index
// refreshes, at least one.
func checkOnlyUpdates(f *fakePackageManager) resource.TestCheckFunc {
	return func(*terraform.State) error {
		got := f.changes()
		if len(got) == 0 || slices.ContainsFunc(got, func(c string) bool { return c != "update" }) {
			return fmt.Errorf("package manager changes = %q, want only index refreshes", got)
		}
		return nil
	}
}

func TestPackageDataSource_installed(t *testing.T) {
	f := newFakePackageManager(packageManagerApt)
	f.available["hello"] = []string{"2.10-1", "2.10-2"}
	f.installed["hello"] = "2.10-1"

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: packageProviderFactories(f),
		Steps: []resource.TestStep{{
			Config: packageDataHCL("hello", ""),
			ConfigStateChecks: []statecheck.StateCheck{
				expectPackageData("id", knownvalue.StringExact("hello")),
				expectPackageData("package_manager", knownvalue.StringExact("apt")),
				expectPackageData("manager", knownvalue.Null()),
				expectPackageData("refresh_cache", knownvalue.Null()),
				expectPackageData("installed", knownvalue.Bool(true)),
				expectPackageData("version", knownvalue.StringExact("2.10-1")),
				expectPackageData("architecture", knownvalue.StringExact("amd64")),
				expectPackageData("available_version", knownvalue.StringExact("2.10-2")),
			},
			// Only reads, and never the package-index refresh.
			Check: checkPackageChanges(f),
		}},
	})
}

func TestPackageDataSource_notInstalled(t *testing.T) {
	f := newFakePackageManager(packageManagerApk)
	f.available["tree"] = []string{"2.1.1-r0"}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: packageProviderFactories(f),
		Steps: []resource.TestStep{
			{
				Config: packageDataHCL("tree", `  manager = "apk"`),
				ConfigStateChecks: []statecheck.StateCheck{
					expectPackageData("manager", knownvalue.StringExact("apk")),
					expectPackageData("package_manager", knownvalue.StringExact("apk")),
					expectPackageData("installed", knownvalue.Bool(false)),
					expectPackageData("version", knownvalue.Null()),
					expectPackageData("architecture", knownvalue.Null()),
					expectPackageData("available_version", knownvalue.StringExact("2.1.1-r0")),
				},
			},
			{
				// In no repository and not installed: not an error.
				Config: packageDataHCL("nosuch", ""),
				ConfigStateChecks: []statecheck.StateCheck{
					expectPackageData("installed", knownvalue.Bool(false)),
					expectPackageData("available_version", knownvalue.Null()),
				},
				Check: checkPackageChanges(f),
			},
		},
	})
}

// TestPackageDataSource_refreshCache checks that the index is only
// refreshed with refresh_cache, at most once per provider run, and before
// the package is read.
func TestPackageDataSource_refreshCache(t *testing.T) {
	f := newFakePackageManager(packageManagerDnf)
	f.available["tree"] = []string{"2.1.1-1.fc42"}
	f.stale["tree"] = []string{"2.2.1-1.fc42"}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: packageProviderFactories(f),
		Steps: []resource.TestStep{
			{
				Config: packageDataHCL("tree", ""),
				ConfigStateChecks: []statecheck.StateCheck{
					expectPackageData("available_version", knownvalue.StringExact("2.1.1-1.fc42")),
				},
				Check: checkPackageChanges(f),
			},
			{
				Config: packageDataHCL("tree", `  refresh_cache = true`),
				ConfigStateChecks: []statecheck.StateCheck{
					expectPackageData("refresh_cache", knownvalue.Bool(true)),
					expectPackageData("available_version", knownvalue.StringExact("2.2.1-1.fc42")),
				},
				Check: checkOnlyUpdates(f),
			},
		},
	})

	// Two data sources in one run refresh once.
	f = newFakePackageManager(packageManagerApt)
	f.stale["hello"] = []string{"2.10-3"}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: packageProviderFactories(f),
		Steps: []resource.TestStep{{
			Config: packageDataHCL("hello", `  refresh_cache = true`) + `
data "sysutils_package" "other" {
  name          = "hello"
  refresh_cache = true
}
`,
			ConfigStateChecks: []statecheck.StateCheck{
				expectPackageData("available_version", knownvalue.StringExact("2.10-3")),
				statecheck.ExpectKnownValue("data.sysutils_package.other", tfjsonpath.New("available_version"), knownvalue.StringExact("2.10-3")),
			},
			Check: checkPackageCallsRepeat(f, "update", "inspect hello", "inspect hello"),
		}},
	})
}

func TestPackageDataSource_errors(t *testing.T) {
	f := newFakePackageManager(packageManagerApt)
	f.updateErr = errors.New("apt-get update: exit status 100:\nE: Could not open lock file /var/lib/apt/lists/lock - open (13: Permission denied)")

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: packageProviderFactories(f),
		Steps: []resource.TestStep{
			{
				Config:      packageDataHCL("hello", `  manager = "dnf"`),
				ExpectError: regexp.MustCompile(`Package\s+manager\s+not\s+available`),
			},
			{
				// Rules of an explicit manager are checked at plan time.
				Config:      packageDataHCL("Hello", `  manager = "apt"`),
				ExpectError: regexp.MustCompile(`Invalid\s+package\s+name`),
			},
			{
				// ... and those of a detected one once it is known.
				Config:      packageDataHCL("Hello", ""),
				ExpectError: regexp.MustCompile(`Invalid\s+package\s+name`),
			},
			{
				Config:      packageDataHCL("hello*", ""),
				ExpectError: regexp.MustCompile(`Invalid\s+package\s+name(.|\n)*must\s+be\s+1\s+to\s+128`),
			},
			{
				Config:      packageDataHCL("hello", `  manager = "zypper"`),
				ExpectError: regexp.MustCompile(`Invalid\s+Attribute\s+Value\s+Match`),
			},
			{
				Config:      packageDataHCL("hello", `  refresh_cache = true`),
				ExpectError: regexp.MustCompile(`Updating\s+package\s+index`),
			},
		},
	})
	if err := checkOnlyUpdates(f)(nil); err != nil {
		t.Error(err)
	}

	f = newFakePackageManager(packageManagerApt)
	f.inspectErr = errors.New("dpkg-query: exit status 2:\ndpkg-query: error: parsing file '/var/lib/dpkg/status'")
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: packageProviderFactories(f),
		Steps: []resource.TestStep{{
			Config:      packageDataHCL("hello", ""),
			ExpectError: regexp.MustCompile(`(?s)Querying\s+package.*parsing\s+file`),
		}},
	})
}

func TestPackageDataSource_rootDirRefused(t *testing.T) {
	f := newFakePackageManager(packageManagerApt)
	f.installed["hello"] = "2.10-1"
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: packageProviderFactories(f),
		Steps: []resource.TestStep{{
			Config: fmt.Sprintf(`
provider "sysutils" {
  root_dir = %q
}
`, t.TempDir()) + packageDataHCL("hello", ""),
			ExpectError: regexp.MustCompile(`Not\s+supported\s+with\s+root_dir`),
		}},
	})
	if len(f.calls) != 0 {
		t.Errorf("package manager ran with root_dir set: %v", f.calls)
	}
}

func TestPackageBackendInspect(t *testing.T) {
	ctx := context.Background()
	dpkgQuery := "dpkg-query --show --showformat=${Package}\\t${db:Status-Abbrev}\\t${Version}\\t${Architecture}\\n -- "

	t.Run("apt", func(t *testing.T) {
		for _, tc := range []struct {
			desc, dpkg, policy string
			dpkgExit           int
			want               packageDetails
		}{
			{
				desc:   "installed, upgrade available",
				dpkg:   "hello\tii \t2.10-2\tamd64\n",
				policy: "hello:\n  Installed: 2.10-2\n  Candidate: 2.10-3\n  Version table:\n",
				want:   packageDetails{packageInfo{true, "2.10-2", "amd64"}, "2.10-3"},
			},
			{
				desc:     "not installed",
				dpkg:     "",
				dpkgExit: 1,
				policy:   helloPolicy,
				want:     packageDetails{Candidate: "2.10-3"},
			},
			{
				desc:   "installed, in no repository",
				dpkg:   "hello\tii \t2.10-2\tall\n",
				policy: "hello:\n  Installed: 2.10-2\n  Candidate: (none)\n  Version table:\n",
				want:   packageDetails{packageInfo{true, "2.10-2", "all"}, ""},
			},
			{
				desc:     "not in the index",
				dpkgExit: 1,
				want:     packageDetails{},
			},
			{
				// apt took the name for a pattern that selects other
				// packages; they are not this package's candidate.
				desc:     "pattern",
				dpkgExit: 1,
				policy:   "hello-traditional:\n  Installed: (none)\n  Candidate: 2.10-3\n",
				want:     packageDetails{},
			},
		} {
			t.Run(tc.desc, func(t *testing.T) {
				s := &scriptedRunner{rules: []scriptedRule{
					{prefix: "dpkg-query", exit: tc.dpkgExit, stdout: tc.dpkg, stderr: map[bool]string{true: "dpkg-query: no packages found matching hello\n"}[tc.dpkgExit == 1]},
					{prefix: "apt-cache policy -- hello", stdout: tc.policy},
				}}
				got, err := newPackageBackend(packageManagerApt, s.run).Inspect(ctx, "hello")
				if err != nil || got != tc.want {
					t.Fatalf("got %+v, %v; want %+v", got, err, tc.want)
				}
				checkCommands(t, s, dpkgQuery+"hello", "apt-cache policy -- hello")
				for _, spec := range s.specs {
					if spec.Timeout != packageQueryTimeout {
						t.Errorf("%q: timeout %s", spec.Argv, spec.Timeout)
					}
				}
			})
		}
		s := &scriptedRunner{rules: []scriptedRule{{prefix: "dpkg-query", exit: 1, stderr: "dpkg-query: no packages found matching hello\n"}, {prefix: "apt-cache", exit: 100, stderr: "E: The package cache file is corrupted\n"}}}
		if _, err := newPackageBackend(packageManagerApt, s.run).Inspect(ctx, "hello"); err == nil || !strings.Contains(err.Error(), "corrupted") {
			t.Errorf("failed apt-cache: got %v", err)
		}
	})

	t.Run("rpm", func(t *testing.T) {
		repoquery := "dnf repoquery -C -q --available --latest-limit=1 --queryformat " + rpmRepoQueryFormat + " -- "
		native := rpmArch(runtime.GOARCH)
		for _, tc := range []struct {
			desc, rpm, repo, stderr string
			rpmExit, repoExit       int
			want                    packageDetails
		}{
			{
				desc: "installed",
				rpm:  "glibc\t2.41-5.fc42\ti686\n",
				repo: "glibc\t" + native + "\t2.41-9.fc42\nglibc\ti686\t2.41-8.fc42\n",
				want: packageDetails{packageInfo{true, "2.41-5.fc42", "i686"}, "2.41-8.fc42"},
			},
			{
				desc:    "not installed, multilib",
				rpm:     "package glibc is not installed\n",
				rpmExit: 1,
				repo:    "glibc\ti686\t2.41-8.fc42\nglibc\t" + native + "\t2.41-9.fc42\nglibc-devel\t" + native + "\t2.41-9.fc42\n",
				want:    packageDetails{Candidate: "2.41-9.fc42"},
			},
			{
				desc:     "no metadata downloaded",
				rpm:      "glibc\t2.41-5.fc42\tx86_64\n",
				repoExit: 1,
				stderr:   "Cache-only enabled but no cache for repository \"fedora\"\n",
				want:     packageDetails{packageInfo: packageInfo{true, "2.41-5.fc42", "x86_64"}},
			},
		} {
			t.Run(tc.desc, func(t *testing.T) {
				s := &scriptedRunner{rules: []scriptedRule{
					{prefix: "rpm", exit: tc.rpmExit, stdout: tc.rpm},
					{prefix: "dnf repoquery", exit: tc.repoExit, stdout: tc.repo, stderr: tc.stderr},
				}}
				got, err := newPackageBackend(packageManagerDnf, s.run).Inspect(ctx, "glibc")
				if err != nil || got != tc.want {
					t.Fatalf("got %+v, %v; want %+v", got, err, tc.want)
				}
				checkCommands(t, s, "rpm --query --queryformat "+rpmQueryFormat+" -- glibc", repoquery+"glibc")
			})
		}
		s := &scriptedRunner{rules: []scriptedRule{{prefix: "rpm", exit: 1, stdout: "package x is not installed\n"}, {prefix: "yum repoquery", exit: 1, stderr: "Error: Failed to download metadata for repo 'updates'\n"}}}
		if _, err := newPackageBackend(packageManagerYum, s.run).Inspect(ctx, "x"); err == nil || !strings.Contains(err.Error(), "Failed to download metadata") {
			t.Errorf("failed repoquery: got %v", err)
		}
	})

	t.Run("apk", func(t *testing.T) {
		policy := "busybox policy:\n  1.37.0-r14:\n    @old https://dl-cdn.alpinelinux.org/alpine/v3.21/main\n  1.37.0-r31:\n    lib/apk/db/installed\n    https://dl-cdn.alpinelinux.org/alpine/v3.24/main\n  1.38.0-r7:\n    @edge https://dl-cdn.alpinelinux.org/alpine/edge/main\n"
		s := &scriptedRunner{rules: []scriptedRule{
			{prefix: "apk info", stdout: "busybox-1.37.0-r31\n"},
			{prefix: "apk list", stdout: "busybox-1.37.0-r31 x86_64 {busybox} (GPL-2.0-only) [installed]\n"},
			{prefix: "apk policy", stdout: policy},
		}}
		got, err := newPackageBackend(packageManagerApk, s.run).Inspect(ctx, "busybox")
		if want := (packageDetails{packageInfo{true, "1.37.0-r31", "x86_64"}, "1.37.0-r31"}); err != nil || got != want {
			t.Fatalf("got %+v, %v; want %+v", got, err, want)
		}
		checkCommands(t, s, "apk info --installed --verbose -- busybox", "apk list --installed -- busybox", "apk policy -- busybox")

		// Not installed: apk list is not needed.
		s = &scriptedRunner{rules: []scriptedRule{
			{prefix: "apk info", exit: 1},
			{prefix: "apk policy", stdout: "curl policy:\n  8.22.0-r0:\n    https://dl-cdn.alpinelinux.org/alpine/v3.24/main\n"},
		}}
		got, err = newPackageBackend(packageManagerApk, s.run).Inspect(ctx, "curl")
		if want := (packageDetails{Candidate: "8.22.0-r0"}); err != nil || got != want {
			t.Fatalf("got %+v, %v; want %+v", got, err, want)
		}
		checkCommands(t, s, "apk info --installed --verbose -- curl", "apk policy -- curl")
	})
}

func TestParseApkPolicy(t *testing.T) {
	for _, tc := range []struct{ out, want string }{
		{"", ""},
		// Only in the installed database: no repository has it.
		{"x policy:\n  1.0-r0:\n    lib/apk/db/installed\n", ""},
		{"x policy:\n  1.0-r0:\n    lib/apk/db/installed\n    https://a/main\n  1.1-r0:\n    https://a/edge\n", "1.1-r0"},
		// Tagged repositories only count with name@tag.
		{"x policy:\n  1.0-r0:\n    https://a/main\n  1.1-r0:\n    @edge https://a/edge\n", "1.0-r0"},
		// A local repository directory counts.
		{"x policy:\n  1.0-r0:\n    /srv/repo\n", "1.0-r0"},
		// Other packages' blocks are ignored.
		{"x-doc policy:\n  2.0-r0:\n    https://a/main\nx policy:\n  1.0-r0:\n    https://a/main\n", "1.0-r0"},
		{"x-doc policy:\n  2.0-r0:\n    https://a/main\n", ""},
	} {
		if got := parseApkPolicy(tc.out, "x"); got != tc.want {
			t.Errorf("parseApkPolicy(%q) = %q, want %q", tc.out, got, tc.want)
		}
	}
}

func TestParseQueriesWithArchitecture(t *testing.T) {
	if got := parseDpkgQuery("libc6\tii \t2.39-0ubuntu8\ti386\nlibc6\tii \t2.39-0ubuntu8\tamd64\n", "libc6"); got != (packageInfo{true, "2.39-0ubuntu8", "i386"}) {
		t.Errorf("parseDpkgQuery = %+v", got)
	}
	if got := parseDpkgQuery("hello\tii \t2.10\tamd64\textra\n", "hello"); got.Installed {
		t.Errorf("parseDpkgQuery accepted a line with 5 fields: %+v", got)
	}
	if got := parseRPMQuery("bash\t5.3.9-3.fc44\tx86_64\n", "bash"); got != (packageInfo{true, "5.3.9-3.fc44", "x86_64"}) {
		t.Errorf("parseRPMQuery = %+v", got)
	}
	if got := parseApkListArch("py3-six-1.17.0-r0 noarch {py3-six} (MIT) [installed]\n", "py3-six-1.17.0-r0"); got != "noarch" {
		t.Errorf("parseApkListArch = %q", got)
	}
	if got := pickRPMCandidate("tzdata\tnoarch\t2026a-1.fc44\n", "tzdata", ""); got != "2026a-1.fc44" {
		t.Errorf("pickRPMCandidate(noarch) = %q", got)
	}
	if got := pickRPMCandidate("other\tx86_64\t1-1\n", "tzdata", ""); got != "" {
		t.Errorf("pickRPMCandidate(other package) = %q", got)
	}
}
