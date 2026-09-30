package provider

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"golang.org/x/sys/unix"
)

// The os-release fixtures in testdata/os-release are copies of the files
// shipped by the distributions' container images.
func TestOSReleaseFixtures(t *testing.T) {
	for _, tc := range []struct {
		file                               string
		id                                 string
		idLike                             []string
		versionID, codename, prettyName    string
		extraKey, extraValue, missingField string
	}{
		{"debian-12", "debian", []string{}, "12", "bookworm", "Debian GNU/Linux 12 (bookworm)", "VERSION", "12 (bookworm)", "ID_LIKE"},
		{"ubuntu-24.04", "ubuntu", []string{"debian"}, "24.04", "noble", "Ubuntu 24.04.4 LTS", "UBUNTU_CODENAME", "noble", "VARIANT_ID"},
		{"alpine-3.21", "alpine", []string{}, "3.21.7", "", "Alpine Linux v3.21", "NAME", "Alpine Linux", "VERSION_CODENAME"},
		{"fedora-40", "fedora", []string{}, "40", "", "Fedora Linux 40 (Container Image)", "VARIANT", "Container Image", "ID_LIKE"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", "os-release", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			r := parseOSRelease(data)
			if got := r.id(); got != tc.id {
				t.Errorf("id = %q, want %q", got, tc.id)
			}
			if got := r.idLike(); !reflect.DeepEqual(got, tc.idLike) {
				t.Errorf("idLike = %q, want %q", got, tc.idLike)
			}
			if got := r["VERSION_ID"]; got != tc.versionID {
				t.Errorf("VERSION_ID = %q, want %q", got, tc.versionID)
			}
			if got := r.versionCodename(); got != tc.codename {
				t.Errorf("versionCodename = %q, want %q", got, tc.codename)
			}
			if got := r.prettyName(); got != tc.prettyName {
				t.Errorf("prettyName = %q, want %q", got, tc.prettyName)
			}
			if got := r[tc.extraKey]; got != tc.extraValue {
				t.Errorf("%s = %q, want %q", tc.extraKey, got, tc.extraValue)
			}
			if _, ok := r[tc.missingField]; ok {
				t.Errorf("%s is set, want it absent", tc.missingField)
			}
			for k, v := range r {
				if strings.ContainsAny(v, `"'`) {
					t.Errorf("%s = %q still has quotes", k, v)
				}
			}
		})
	}
}

func TestParseOSReleaseSyntax(t *testing.T) {
	r := parseOSRelease([]byte(strings.Join([]string{
		"# comment",
		"",
		`  ID=Custom  `,
		`ID_LIKE="rhel  centos fedora"`,
		`PRETTY_NAME="A \"quoted\" \$name with \\ and \` + "`" + `tick\x"`,
		`SINGLE='it''s $literal \n'`,
		`BARE=a\ b`,
		`CONCAT="a"'b'c`,
		`EMPTY=`,
		`UNTERMINATED="abc`,
		`SPACED=a b`,
		`lower_case=ok`,
		`1BAD=x`,
		`BAD-KEY=x`,
		`not an assignment`,
		`VERSION_ID=1`,
		`VERSION_ID=2`,
	}, "\n")))
	want := osRelease{
		"ID":          "Custom",
		"ID_LIKE":     "rhel  centos fedora",
		"PRETTY_NAME": "A \"quoted\" $name with \\ and `tick\\x",
		"SINGLE":      `its $literal \n`,
		"BARE":        "a b",
		"CONCAT":      "abc",
		"EMPTY":       "",
		"lower_case":  "ok",
		"VERSION_ID":  "2",
	}
	if !reflect.DeepEqual(r, want) {
		t.Errorf("parseOSRelease =\n%#v\nwant\n%#v", r, want)
	}
	if got := r.idLike(); !reflect.DeepEqual(got, []string{"rhel", "centos", "fedora"}) {
		t.Errorf("idLike = %q", got)
	}

	// Defaults of os-release(5) for an empty file.
	empty := parseOSRelease(nil)
	if empty.id() != "linux" || empty.prettyName() != "Linux" || empty.versionCodename() != "" || len(empty.idLike()) != 0 {
		t.Errorf("defaults: id %q, pretty %q, codename %q, idLike %q", empty.id(), empty.prettyName(), empty.versionCodename(), empty.idLike())
	}
	// UBUNTU_CODENAME is only a fallback.
	if got := parseOSRelease([]byte("VERSION_CODENAME=a\nUBUNTU_CODENAME=b\n")).versionCodename(); got != "a" {
		t.Errorf("versionCodename = %q, want a", got)
	}
	if got := parseOSRelease([]byte("UBUNTU_CODENAME=xenial\n")).versionCodename(); got != "xenial" {
		t.Errorf("versionCodename = %q, want xenial", got)
	}
}

// writeTree creates files below dir: a value starting with "->" makes a
// symlink to the rest, "x:" an executable with the rest as content.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		host := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(host), 0o755); err != nil {
			t.Fatal(err)
		}
		var err error
		switch {
		case strings.HasPrefix(content, "->"):
			err = os.Symlink(strings.TrimPrefix(content, "->"), host)
		case strings.HasPrefix(content, "x:"):
			err = os.WriteFile(host, []byte(strings.TrimPrefix(content, "x:")), 0o755)
		default:
			err = os.WriteFile(host, []byte(content), 0o644)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "os-release", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestReadOSReleaseInRoot(t *testing.T) {
	debian, alpine := fixture(t, "debian-12"), fixture(t, "alpine-3.21")
	for _, tc := range []struct {
		name     string
		files    map[string]string
		wantID   string
		wantPath string
		wantErr  string
	}{
		// Debian's layout: a relative link into /usr/lib.
		{"relative symlink", map[string]string{"etc/os-release": "->../usr/lib/os-release", "usr/lib/os-release": debian}, "debian", "/etc/os-release", ""},
		// An absolute link is relative to root_dir, as in a chroot.
		{"absolute symlink", map[string]string{"etc/os-release": "->/usr/lib/os-release", "usr/lib/os-release": alpine}, "alpine", "/etc/os-release", ""},
		{"etc only", map[string]string{"etc/os-release": alpine}, "alpine", "/etc/os-release", ""},
		{"usr/lib fallback", map[string]string{"usr/lib/os-release": debian}, "debian", "/usr/lib/os-release", ""},
		{"dangling etc falls back", map[string]string{"etc/os-release": "->/nowhere", "usr/lib/os-release": debian}, "debian", "/usr/lib/os-release", ""},
		{"etc wins", map[string]string{"etc/os-release": alpine, "usr/lib/os-release": debian}, "alpine", "/etc/os-release", ""},
		{"none", map[string]string{"etc/hostname": "x"}, "", "", "neither /etc/os-release nor /usr/lib/os-release exists"},
		{"escaping symlink", map[string]string{"etc/os-release": "->../../../../../../etc/os-release"}, "", "", "escapes root_dir"},
		{"directory", map[string]string{"etc/os-release/x": ""}, "", "", "not a regular file"},
		{"too large", map[string]string{"etc/os-release": "ID=x\n" + strings.Repeat("#", maxOSReleaseSize)}, "", "", "larger than"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTree(t, dir, tc.files)
			root, err := newFSRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			r, p, err := readOSRelease(root)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("readOSRelease error = %v, want %q", err, tc.wantErr)
				}
				if tc.name == "none" && !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("error %v does not wrap fs.ErrNotExist", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if r.id() != tc.wantID || p != tc.wantPath {
				t.Errorf("readOSRelease = %q from %s, want %q from %s", r.id(), p, tc.wantID, tc.wantPath)
			}
		})
	}
}

func TestParseMemTotal(t *testing.T) {
	got, err := parseMemTotal([]byte("MemFree:  100 kB\nMemTotal:       16318280 kB\nSwapTotal: 0 kB\n"))
	if err != nil || got != 16318280*1024 {
		t.Errorf("parseMemTotal = %d, %v", got, err)
	}
	for _, bad := range []string{"MemFree: 1 kB\n", "MemTotal: 12\n", "MemTotal: x kB\n", "MemTotal: -1 kB\n", "MemTotal: 1 MB\n"} {
		if _, err := parseMemTotal([]byte(bad)); err == nil {
			t.Errorf("parseMemTotal(%q) succeeded", bad)
		}
	}
}

func TestHostFQDN(t *testing.T) {
	hosts := "127.0.0.1 localhost\n# 10.0.0.1 web.commented.example web\n127.0.1.1 db.example.org db  # the host\n10.0.0.2 app app.example.com\n"
	dir := t.TempDir()
	hostsFile := filepath.Join(dir, "hosts")
	if err := os.WriteFile(hostsFile, []byte(hosts), 0o644); err != nil {
		t.Fatal(err)
	}
	lookups := 0
	cfg := &hostConfig{hostsFile: hostsFile, lookupCNAME: func(_ context.Context, host string) (string, error) {
		lookups++
		switch host {
		case "web", "app":
			return host + ".dns.example.", nil
		case "short":
			return "short.", nil
		}
		return "", fmt.Errorf("lookup %s: no such host", host)
	}}
	for _, tc := range []struct{ host, want string }{
		{"already.qualified", "already.qualified"},
		{"db", "db.example.org"},
		{"DB", "db.example.org"},
		// Commented out in hosts, so DNS decides.
		{"web", "web.dns.example"},
		// Its canonical name in hosts is not qualified, so DNS decides.
		{"app", "app.dns.example"},
		{"short", "short"},
		{"unknown", "unknown"},
		{"", ""},
	} {
		if got := cfg.fqdn(context.Background(), tc.host); got != tc.want {
			t.Errorf("fqdn(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
	if lookups != 4 {
		t.Errorf("%d DNS lookups, want 4 (web, app, short, unknown)", lookups)
	}
}

func TestProbeInitSystem(t *testing.T) {
	for _, tc := range []struct {
		name, pid1 string
		files      map[string]string
		want       string
	}{
		{"systemd", "systemd", map[string]string{"run/systemd/system/x": ""}, initSystemSystemd},
		{"openrc", "init", map[string]string{"run/openrc/softlevel": "default"}, initSystemOpenRC},
		{"systemd wins", "systemd", map[string]string{"run/systemd/system/x": "", "run/openrc/softlevel": ""}, initSystemSystemd},
		{"sysvinit", "init", nil, initSystemSysvinit},
		{"container", "sh", nil, ""},
		{"no procfs", "", nil, ""},
		// A file is not the directory that sd_booted() checks for.
		{"systemd marker is a file", "bash", map[string]string{"run/systemd/system": ""}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTree(t, dir, tc.files)
			if tc.pid1 != "" {
				writeTree(t, dir, map[string]string{"proc/1/comm": tc.pid1 + "\n"})
			}
			p := probeInitSystem(filepath.Join(dir, "run"), filepath.Join(dir, "proc"))
			if p.kind != tc.want || p.pid1 != tc.pid1 {
				t.Errorf("probeInitSystem = %q (pid1 %q), want %q (pid1 %q)", p.kind, p.pid1, tc.want, tc.pid1)
			}
		})
	}

	// sysutils_service still refuses SysV init, and says what PID 1 is.
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"proc/1/comm": "init\n"})
	cfg := &serviceConfig{runDir: filepath.Join(dir, "run"), procDir: filepath.Join(dir, "proc"),
		lookPath: func(n string) (string, error) { return "", errors.New("not found") }}
	if _, err := cfg.detect(); err == nil || !strings.Contains(err.Error(), `no supported init system found`) || !strings.Contains(err.Error(), `PID 1 is "init"`) {
		t.Errorf("detect() error = %v", err)
	}
}

func TestLookPathIn(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		// Merged /usr: /sbin is a link to usr/sbin.
		"sbin":            "->usr/sbin",
		"usr/sbin/apk":    "x:",
		"usr/bin/notexec": "",
		"usr/bin/escape":  "->../../../../../../bin/sh",
		"usr/bin/abs":     "->/usr/sbin/apk",
	})
	root, err := newFSRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	lookPath := lookPathIn(root)
	for _, tc := range []struct{ name, want string }{
		{"apk", "/usr/sbin/apk"},
		{"abs", "/usr/bin/abs"},
		{"notexec", ""},
		{"escape", ""},
		{"missing", ""},
		{"../sbin/apk", ""},
		{"", ""},
	} {
		got, err := lookPath(tc.name)
		if got != tc.want || (tc.want == "") != (err != nil) {
			t.Errorf("lookPath(%q) = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
	if kind, err := detectPackageManager(lookPath); kind != packageManagerApk || err != nil {
		t.Errorf("detectPackageManager = %q, %v", kind, err)
	}
}

// fakeHost is a host for the sysutils_host data source: a root_dir tree,
// procfs, /run and hosts file in temporary directories.
type fakeHost struct {
	dir  string
	root string
}

func newFakeHost(t *testing.T, rootFiles map[string]string) *fakeHost {
	t.Helper()
	h := &fakeHost{dir: t.TempDir()}
	h.root = filepath.Join(h.dir, "root")
	if err := os.Mkdir(h.root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTree(t, h.root, rootFiles)
	writeTree(t, h.dir, map[string]string{
		"proc/meminfo":         "MemTotal:        2048 kB\nMemFree:  1 kB\n",
		"proc/1/comm":          "openrc-init\n",
		"run/openrc/softlevel": "default",
		"hosts":                "127.0.1.1 buildhost.example.net buildhost\n",
	})
	return h
}

func (h *fakeHost) providerFactories() map[string]func() (tfprotov6.ProviderServer, error) {
	notFound := func(name string) (string, error) { return "", fmt.Errorf("%s: not found", name) }
	return map[string]func() (tfprotov6.ProviderServer, error){
		"sysutils": providerserver.NewProtocol6WithError(&sysutilsProvider{
			version: "test",
			host: &hostConfig{
				procDir:   filepath.Join(h.dir, "proc"),
				hostsFile: filepath.Join(h.dir, "hosts"),
				unameFn: func(u *unix.Utsname) error {
					copy(u.Nodename[:], "buildhost")
					copy(u.Release[:], "6.1.0-99-fake")
					copy(u.Machine[:], "riscv64")
					return nil
				},
				lookupCNAME: func(context.Context, string) (string, error) { return "", errors.New("no DNS in tests") },
			},
			service:  &serviceConfig{runDir: filepath.Join(h.dir, "run"), procDir: filepath.Join(h.dir, "proc"), lookPath: notFound},
			firewall: &firewallConfig{lookPath: notFound},
		}),
	}
}

func TestHostDataSource_fakeRoot(t *testing.T) {
	h := newFakeHost(t, map[string]string{
		"etc/os-release":     "->../usr/lib/os-release",
		"usr/lib/os-release": fixture(t, "fedora-40"),
		"usr/bin/dnf":        "x:",
		"usr/bin/rpm":        "x:",
		// Found first on the host, but not below root_dir: apt needs three
		// tools.
		"usr/bin/apt-get": "x:",
	})
	const ds = "data.sysutils_host.this"
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: h.providerFactories(),
		Steps: []resource.TestStep{{
			Config: fmt.Sprintf(`
provider "sysutils" {
  root_dir = %q
}

data "sysutils_host" "this" {}
`, h.root),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(ds, "id", h.root),
				resource.TestCheckResourceAttr(ds, "root_dir", h.root),
				resource.TestCheckResourceAttr(ds, "hostname", "buildhost"),
				resource.TestCheckResourceAttr(ds, "fqdn", "buildhost.example.net"),
				resource.TestCheckResourceAttr(ds, "os_id", "fedora"),
				resource.TestCheckResourceAttr(ds, "os_id_like.#", "0"),
				resource.TestCheckResourceAttr(ds, "os_version_id", "40"),
				resource.TestCheckNoResourceAttr(ds, "os_version_codename"),
				resource.TestCheckResourceAttr(ds, "os_pretty_name", "Fedora Linux 40 (Container Image)"),
				resource.TestCheckResourceAttr(ds, "os_release.VARIANT_ID", "container"),
				resource.TestCheckResourceAttr(ds, "os_release.VERSION_CODENAME", ""),
				resource.TestCheckResourceAttr(ds, "kernel_release", "6.1.0-99-fake"),
				resource.TestCheckResourceAttr(ds, "architecture", "riscv64"),
				resource.TestCheckResourceAttrSet(ds, "cpu_count"),
				resource.TestCheckResourceAttr(ds, "memory_total_bytes", "2097152"),
				resource.TestCheckResourceAttr(ds, "init_system", "openrc"),
				resource.TestCheckResourceAttr(ds, "package_manager", "dnf"),
				resource.TestCheckNoResourceAttr(ds, "firewall_backend"),
				resource.TestCheckResourceAttr(ds, "live_facts.#", fmt.Sprint(len(liveHostFacts))),
				resource.TestCheckTypeSetElemAttr(ds, "live_facts.*", "kernel_release"),
				resource.TestCheckTypeSetElemAttr(ds, "live_facts.*", "init_system"),
			),
		}},
	})
}

// TestHostDataSource_distroConditional evaluates the cookbook's choice of
// package names by distribution (examples/guides/bootstrap-host/distro.tf,
// up to its resources) against each fixture.
func TestHostDataSource_distroConditional(t *testing.T) {
	guide, err := os.ReadFile(filepath.Join("..", "..", "examples", "guides", "bootstrap-host", "distro.tf"))
	if err != nil {
		t.Fatal(err)
	}
	snippet, _, ok := strings.Cut(string(guide), "\nresource \"")
	if !ok {
		t.Fatal("distro.tf has no resource")
	}
	for _, tc := range []struct{ fixture, family, packages string }{
		{"debian-12", "debian", "bind9-dnsutils,cron"},
		{"ubuntu-24.04", "debian", "bind9-dnsutils,cron"},
		{"alpine-3.21", "alpine", "bind-tools,cronie"},
		{"fedora-40", "redhat", "bind-utils,cronie"},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			h := newFakeHost(t, map[string]string{"etc/os-release": fixture(t, tc.fixture)})
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: h.providerFactories(),
				Steps: []resource.TestStep{{
					Config: fmt.Sprintf(`
provider "sysutils" {
  root_dir = %q
}
%s
output "family" {
  value = local.distro_family
}

output "packages" {
  value = join(",", local.distro_packages[local.distro_family])
}
`, h.root, snippet),
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckOutput("family", tc.family),
						resource.TestCheckOutput("packages", tc.packages),
					),
				}},
			})
		})
	}
}

// TestHostDataSource_noOSRelease checks that a tree without os-release is
// a warning, with the os_* attributes null, not an error.
func TestHostDataSource_noOSRelease(t *testing.T) {
	h := newFakeHost(t, map[string]string{"etc/hostname": "x\n"})
	const ds = "data.sysutils_host.this"
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: h.providerFactories(),
		Steps: []resource.TestStep{{
			Config: fmt.Sprintf(`
provider "sysutils" {
  root_dir = %q
}

data "sysutils_host" "this" {}
`, h.root),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckNoResourceAttr(ds, "os_id"),
				resource.TestCheckNoResourceAttr(ds, "os_id_like.#"),
				resource.TestCheckNoResourceAttr(ds, "os_release.%"),
				resource.TestCheckNoResourceAttr(ds, "package_manager"),
				resource.TestCheckResourceAttr(ds, "kernel_release", "6.1.0-99-fake"),
			),
		}},
	})
}
