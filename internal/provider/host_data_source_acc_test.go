package provider

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// shellOSRelease reads the host's os-release with the shell, which is what
// its syntax is defined by, independently of parseOSRelease.
func shellOSRelease(t *testing.T) (id, idLike, versionID, codename, pretty string) {
	t.Helper()
	script := `f=/etc/os-release; [ -e "$f" ] || f=/usr/lib/os-release; . "$f" && ` +
		`printf '%s\n' "${ID:-linux}" "$ID_LIKE" "$VERSION_ID" "${VERSION_CODENAME:-$UBUNTU_CODENAME}" "${PRETTY_NAME:-Linux}"`
	out, err := exec.Command("sh", "-c", script).Output()
	if err != nil {
		t.Skipf("cannot read os-release with sh: %v", err)
	}
	f := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(f) != 5 {
		t.Fatalf("unexpected output %q", out)
	}
	return f[0], f[1], f[2], f[3], f[4]
}

func commandLine(t *testing.T, argv ...string) string {
	t.Helper()
	out, err := exec.Command(argv[0], argv[1:]...).Output()
	if err != nil {
		t.Fatalf("%s: %v", strings.Join(argv, " "), err)
	}
	return strings.TrimSpace(string(out))
}

// checkOptionalAttr checks attr against want, where "" means null.
func checkOptionalAttr(name, attr, want string) resource.TestCheckFunc {
	if want == "" {
		return resource.TestCheckNoResourceAttr(name, attr)
	}
	return resource.TestCheckResourceAttr(name, attr, want)
}

// TestAccHostDataSource_live compares the facts with what the usual
// command-line tools report on this host. It needs no root.
func TestAccHostDataSource_live(t *testing.T) {
	id, idLike, versionID, codename, pretty := shellOSRelease(t)
	memTotal, err := (&hostConfig{}).memTotal()
	if err != nil {
		t.Fatal(err)
	}
	var memKB int64
	for _, line := range strings.Split(commandLine(t, "cat", "/proc/meminfo"), "\n") {
		if _, err := fmt.Sscanf(line, "MemTotal: %d kB", &memKB); err == nil {
			break
		}
	}
	if memKB*1024 != memTotal {
		t.Fatalf("memTotal() = %d, /proc/meminfo says %d kB", memTotal, memKB)
	}

	wantInit := ""
	if fi, err := os.Stat("/run/systemd/system"); err == nil && fi.IsDir() {
		wantInit = initSystemSystemd
	} else if _, err := os.Stat("/run/openrc/softlevel"); err == nil {
		wantInit = initSystemOpenRC
	} else if comm, _ := os.ReadFile("/proc/1/comm"); strings.TrimSpace(string(comm)) == "init" {
		wantInit = initSystemSysvinit
	}
	// The first manager whose tools the shell finds.
	out, err := exec.Command("sh", "-c", `
has() { for c in "$@"; do command -v "$c" >/dev/null 2>&1 || return 1; done; }
if has apt-get apt-cache dpkg-query; then echo apt
elif has dnf rpm; then echo dnf
elif has yum rpm; then echo yum
elif has apk; then echo apk
fi`).Output()
	if err != nil {
		t.Fatal(err)
	}
	wantPkg := strings.TrimSpace(string(out))
	wantFirewall, _ := (&firewallConfig{}).detect(context.Background())

	const ds = "data.sysutils_host.this"
	checks := []resource.TestCheckFunc{
		resource.TestCheckResourceAttr(ds, "id", "/"),
		resource.TestCheckResourceAttr(ds, "root_dir", "/"),
		resource.TestCheckResourceAttr(ds, "hostname", commandLine(t, "uname", "-n")),
		resource.TestCheckResourceAttrSet(ds, "fqdn"),
		resource.TestCheckResourceAttr(ds, "os_id", id),
		resource.TestCheckResourceAttr(ds, "os_id_like.#", fmt.Sprint(len(strings.Fields(idLike)))),
		checkOptionalAttr(ds, "os_version_id", versionID),
		checkOptionalAttr(ds, "os_version_codename", codename),
		resource.TestCheckResourceAttr(ds, "os_pretty_name", pretty),
		resource.TestCheckResourceAttr(ds, "kernel_release", commandLine(t, "uname", "-r")),
		resource.TestCheckResourceAttr(ds, "architecture", commandLine(t, "uname", "-m")),
		resource.TestCheckResourceAttr(ds, "cpu_count", commandLine(t, "nproc")),
		resource.TestCheckResourceAttr(ds, "memory_total_bytes", fmt.Sprint(memTotal)),
		checkOptionalAttr(ds, "init_system", wantInit),
		checkOptionalAttr(ds, "package_manager", wantPkg),
		checkOptionalAttr(ds, "firewall_backend", wantFirewall),
		resource.TestCheckResourceAttr(ds, "live_facts.#", fmt.Sprint(len(liveHostFacts))),
	}
	for i, like := range strings.Fields(idLike) {
		checks = append(checks, resource.TestCheckResourceAttr(ds, fmt.Sprintf("os_id_like.%d", i), like))
	}
	// hostname -f uses getaddrinfo, which may consult other name services
	// than the provider; compare only where it finds a qualified name.
	if out, err := exec.Command("hostname", "-f").Output(); err == nil {
		if fqdn := strings.TrimSpace(string(out)); strings.Contains(fqdn, ".") {
			checks = append(checks, resource.TestCheckResourceAttr(ds, "fqdn", fqdn))
		}
	}
	t.Logf("expecting os_id %q, init_system %q, package_manager %q, firewall_backend %q", id, wantInit, wantPkg, wantFirewall)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: `data "sysutils_host" "this" {}`,
			Check:  resource.ComposeAggregateTestCheckFunc(checks...),
		}},
	})
}

// TestAccHostDataSource_rootDir reads the distribution and package manager
// of an image tree below root_dir, while the live facts still describe the
// running host: they equal those read without root_dir. The two are read in
// separate steps because the test framework runs every configuration of the
// provider in one process, so provider aliases would share one root_dir.
func TestAccHostDataSource_rootDir(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"etc/os-release":     "->../usr/lib/os-release",
		"usr/lib/os-release": fixture(t, "alpine-3.21"),
		"bin":                "->usr/bin",
		"usr/bin/apk":        "x:#!/bin/sh\nexit 1\n",
	})
	const ds = "data.sysutils_host.this"
	live := map[string]string{}
	captureLive := func(s *terraform.State) error {
		attrs := s.RootModule().Resources[ds].Primary.Attributes
		for _, attr := range liveHostFacts {
			live[attr] = attrs[attr]
		}
		return nil
	}
	checks := []resource.TestCheckFunc{
		resource.TestCheckResourceAttr(ds, "root_dir", dir),
		resource.TestCheckResourceAttr(ds, "os_id", "alpine"),
		resource.TestCheckResourceAttr(ds, "os_version_id", "3.21.7"),
		resource.TestCheckResourceAttr(ds, "os_pretty_name", "Alpine Linux v3.21"),
		resource.TestCheckNoResourceAttr(ds, "os_version_codename"),
		resource.TestCheckResourceAttr(ds, "os_release.NAME", "Alpine Linux"),
		resource.TestCheckResourceAttr(ds, "package_manager", "apk"),
		func(s *terraform.State) error {
			attrs := s.RootModule().Resources[ds].Primary.Attributes
			for _, attr := range liveHostFacts {
				if attrs[attr] != live[attr] {
					return fmt.Errorf("%s = %q below root_dir, %q on the host", attr, attrs[attr], live[attr])
				}
			}
			return nil
		},
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `data "sysutils_host" "this" {}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(ds, "root_dir", "/"),
					resource.TestCheckResourceAttrSet(ds, "kernel_release"),
					captureLive,
				),
			},
			{
				Config: fmt.Sprintf(`
provider "sysutils" {
  root_dir = %q
}

data "sysutils_host" "this" {}
`, dir),
				Check: resource.ComposeAggregateTestCheckFunc(checks...),
			},
		},
	})
}
