package provider

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

const testSysctlDataSource = "data.sysutils_sysctl.test"

func expectSysctlData(attr string, v knownvalue.Check) statecheck.StateCheck {
	return statecheck.ExpectKnownValue(testSysctlDataSource, tfjsonpath.New(attr), v)
}

// writeTestTree creates the files of tree below dir. Values starting with
// "->" create a symlink to the rest of the value instead.
func writeTestTree(t *testing.T, dir string, tree map[string]string) {
	t.Helper()
	for rel, content := range tree {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		var err error
		if target, ok := strings.CutPrefix(content, "->"); ok {
			err = os.Symlink(target, p)
		} else {
			err = os.WriteFile(p, []byte(content), 0o644)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

// newTestProcSys returns a fake /proc/sys.
func newTestProcSys(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeTestTree(t, dir, map[string]string{
		"net/ipv4/ip_forward":                 "1\n",
		"net/ipv4/ip_local_port_range":        "32768\t60999\n",
		"net/ipv4/conf/all/rp_filter":         "2\n",
		"net/ipv4/conf/all_squash/x":          "0\n", // Not below "net.ipv4.conf.all".
		"net/ipv4/conf/eth0.100/forwarding":   "0\n",
		"net/ipv4/conf/all/link":              "->../all_squash/x",
		"kernel/ostype":                       "Linux\n",
		"vm/swappiness":                       "60\n",
		"net/ipv4/conf/all/weird name":        "1\n", // Not a valid key component.
		"net/ipv4/conf/all/nested/deep/value": "deep\n",
	})
	if err := syscall.Mkfifo(filepath.Join(dir, "net/ipv4/conf/all/fifo"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestValidateSysctlPrefix(t *testing.T) {
	for _, ok := range []string{"vm", "net", "net.ipv4.conf.all", "net.ipv4.conf.eth0/100", "net.ipv4.ip_forward"} {
		if err := validateSysctlPrefix(ok); err != nil {
			t.Errorf("validateSysctlPrefix(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", ".", "..", "net.", ".net", "net..ipv4", "net/ipv4", "net.//", "net.*", "vm swappiness"} {
		if err := validateSysctlPrefix(bad); err == nil {
			t.Errorf("validateSysctlPrefix(%q) succeeded", bad)
		}
	}
	// Names still need two components.
	if err := validateSysctlName("vm"); err == nil {
		t.Error("validateSysctlName(\"vm\") succeeded")
	}
}

func TestSysctlKeyComponent(t *testing.T) {
	for name, want := range map[string]string{"eth0.100": "eth0/100", "ip_forward": "ip_forward", "br-lan": "br-lan"} {
		if got, ok := sysctlKeyComponent(name); !ok || got != want {
			t.Errorf("sysctlKeyComponent(%q) = %q, %v; want %q", name, got, ok, want)
		}
	}
	for _, name := range []string{"", ".", "..", "a b", "a*", "a\nb"} {
		if got, ok := sysctlKeyComponent(name); ok {
			t.Errorf("sysctlKeyComponent(%q) = %q, want rejection", name, got)
		}
	}
}

func TestListSysctl(t *testing.T) {
	procSys := newTestProcSys(t)
	got, err := listSysctl(procSys, "net.ipv4.conf.all")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"net.ipv4.conf.all.rp_filter":         "2",
		"net.ipv4.conf.all.nested.deep.value": "deep",
	}
	if !maps.Equal(got, want) {
		t.Errorf("listSysctl(net.ipv4.conf.all) = %v, want %v (no symlinks, FIFOs or invalid names)", got, want)
	}

	got, err = listSysctl(procSys, "net")
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"net.ipv4.ip_local_port_range":      "32768 60999",
		"net.ipv4.conf.eth0/100.forwarding": "0",
		"net.ipv4.conf.all_squash.x":        "0",
	} {
		if got[key] != value {
			t.Errorf("listSysctl(net)[%q] = %q, want %q", key, got[key], value)
		}
	}
	if _, ok := got["kernel.ostype"]; ok {
		t.Error("listSysctl(net) includes kernel.ostype")
	}

	// A single parameter.
	got, err = listSysctl(procSys, "vm.swappiness")
	if err != nil || !maps.Equal(got, map[string]string{"vm.swappiness": "60"}) {
		t.Errorf("listSysctl(vm.swappiness) = %v, %v", got, err)
	}

	for _, missing := range []string{"nosuch", "net.ipv4.nosuch", "vm.swappiness.x"} {
		if _, err := listSysctl(procSys, missing); !errors.Is(err, errSysctlNotFound) {
			t.Errorf("listSysctl(%q) = %v, want errSysctlNotFound", missing, err)
		}
	}
	// The symlink itself cannot be used as a prefix either.
	if _, err := listSysctl(procSys, "net.ipv4.conf.all.link"); err == nil {
		t.Error("listSysctl followed a symlink")
	}
}

func TestListSysctlRealProcSys(t *testing.T) {
	if _, err := os.Stat("/proc/sys/kernel/ostype"); err != nil {
		t.Skip("no /proc/sys")
	}
	got, err := listSysctl(defaultProcSys, "kernel")
	if err != nil {
		t.Fatal(err)
	}
	if got["kernel.ostype"] != "Linux" {
		t.Errorf("kernel.ostype = %q", got["kernel.ostype"])
	}
	for key := range got {
		if err := validateSysctlName(key); err != nil {
			t.Errorf("listed key %q is invalid: %v", key, err)
		}
	}
	// binfmt_misc is a separate file system mounted inside /proc/sys.
	all, err := listSysctl(defaultProcSys, "fs")
	if err != nil {
		t.Fatal(err)
	}
	for key := range all {
		if strings.HasPrefix(key, "fs.binfmt_misc.") {
			t.Errorf("listSysctl(fs) descended into binfmt_misc: %q", key)
		}
	}
}

// sysctlConfTree is a root filesystem tree with sysctl configuration that
// exercises the precedence rules of systemd-sysctl and sysctl --system.
var sysctlConfTree = map[string]string{
	// Hides /usr/lib/sysctl.d/10-base.conf.
	"etc/sysctl.d/10-base.conf":     "net.ipv4.ip_forward = 1\n",
	"usr/lib/sysctl.d/10-base.conf": "net.ipv4.ip_forward = 0\nvm.overcommit_memory = 1\n",
	"usr/lib/sysctl.d/20-net.conf": "# comment\n; also a comment\n" +
		"net/ipv4/conf/all/rp_filter = 2\n" + // Slash form.
		"-net.ipv4.tcp_syncookies=1\n" + // Ignore errors.
		"net.ipv4.ip_local_port_range = 1024\t65000\n" +
		"net.ipv4.conf.*.accept_redirects = 0\n",
	// Masked: the /usr/lib file of the same name is not read either.
	"run/sysctl.d/30-masked.conf":     "->/dev/null",
	"usr/lib/sysctl.d/30-masked.conf": "vm.swappiness = 1\n",
	"lib/sysctl.d/40-late.conf":       "vm.swappiness = 10\nkernel.pid_max = 65536\n",
	"etc/sysctl.d/.hidden.conf":       "vm.swappiness = 99\n",
	"etc/sysctl.d/50-ignored.txt":     "vm.swappiness = 98\n",
	// Debian's symlink to /etc/sysctl.conf, followed inside the tree.
	"etc/sysctl.d/99-sysctl.conf": "->../sysctl.conf",
	"etc/sysctl.conf":             "kernel.pid_max = 4194304\n",
}

func TestReadPersistedSysctl(t *testing.T) {
	dir := t.TempDir()
	writeTestTree(t, dir, sysctlConfTree)
	root, err := newFSRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	files, err := sysctlConfFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	wantFiles := []string{
		"/etc/sysctl.d/10-base.conf",
		"/usr/lib/sysctl.d/20-net.conf",
		"/run/sysctl.d/30-masked.conf",
		"/lib/sysctl.d/40-late.conf",
		"/etc/sysctl.d/99-sysctl.conf",
		"/etc/sysctl.conf",
	}
	if !slices.Equal(files, wantFiles) {
		t.Errorf("sysctlConfFiles = %q, want %q", files, wantFiles)
	}

	got, err := readPersistedSysctl(root)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]persistedSysctl{
		"net.ipv4.ip_forward":              {"1", "/etc/sysctl.d/10-base.conf"},
		"net.ipv4.conf.all.rp_filter":      {"2", "/usr/lib/sysctl.d/20-net.conf"},
		"net.ipv4.tcp_syncookies":          {"1", "/usr/lib/sysctl.d/20-net.conf"},
		"net.ipv4.ip_local_port_range":     {"1024 65000", "/usr/lib/sysctl.d/20-net.conf"},
		"net.ipv4.conf.*.accept_redirects": {"0", "/usr/lib/sysctl.d/20-net.conf"},
		"vm.swappiness":                    {"10", "/lib/sysctl.d/40-late.conf"},
		"kernel.pid_max":                   {"4194304", "/etc/sysctl.conf"},
	}
	if !maps.Equal(got, want) {
		t.Errorf("readPersistedSysctl =\n%v\nwant\n%v", got, want)
	}
}

func TestReadPersistedSysctlEmptyTree(t *testing.T) {
	root, err := newFSRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got, err := readPersistedSysctl(root)
	if err != nil || len(got) != 0 {
		t.Errorf("readPersistedSysctl(empty) = %v, %v", got, err)
	}
}

// A symlink in the tree must not lead the data source to read host files.
func TestReadPersistedSysctlEscape(t *testing.T) {
	dir := t.TempDir()
	writeTestTree(t, dir, map[string]string{"etc/sysctl.d/10-x.conf": "->../../../../../../../../etc/hostname"})
	root, err := newFSRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readPersistedSysctl(root); !errors.Is(err, errEscapesRoot) {
		t.Errorf("readPersistedSysctl = %v, want errEscapesRoot", err)
	}
}

func sysctlDataHCL(attr, value string) string {
	return fmt.Sprintf(`
data "sysutils_sysctl" "test" {
  %s = %q
}
`, attr, value)
}

func TestSysctlDataSource_name(t *testing.T) {
	procSys := newTestProcSys(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: sysctlProviderFactories(procSys),
		Steps: []resource.TestStep{
			{
				Config: sysctlDataHCL("name", "net.ipv4.ip_local_port_range"),
				ConfigStateChecks: []statecheck.StateCheck{
					expectSysctlData("id", knownvalue.StringExact("net.ipv4.ip_local_port_range")),
					expectSysctlData("exists", knownvalue.Bool(true)),
					expectSysctlData("value", knownvalue.StringExact("32768 60999")),
					expectSysctlData("values", knownvalue.Null()),
					expectSysctlData("persisted_values", knownvalue.Null()),
				},
			},
			{
				Config: sysctlDataHCL("name", "net.ipv4.conf.eth0/100.forwarding"),
				ConfigStateChecks: []statecheck.StateCheck{
					expectSysctlData("value", knownvalue.StringExact("0")),
				},
			},
			{
				// The module providing it is not loaded, say.
				Config: sysctlDataHCL("name", "net.netfilter.nf_conntrack_max"),
				ConfigStateChecks: []statecheck.StateCheck{
					expectSysctlData("exists", knownvalue.Bool(false)),
					expectSysctlData("value", knownvalue.Null()),
				},
			},
			{
				Config:      sysctlDataHCL("name", "net.ipv4.conf.all"),
				ExpectError: regexp.MustCompile(`group\s+of\s+kernel\s+parameters`),
			},
		},
	})
}

func TestSysctlDataSource_prefix(t *testing.T) {
	procSys := newTestProcSys(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: sysctlProviderFactories(procSys),
		Steps: []resource.TestStep{
			{
				Config: sysctlDataHCL("prefix", "net.ipv4.conf.all"),
				ConfigStateChecks: []statecheck.StateCheck{
					expectSysctlData("id", knownvalue.StringExact("net.ipv4.conf.all")),
					expectSysctlData("values", knownvalue.MapExact(map[string]knownvalue.Check{
						"net.ipv4.conf.all.rp_filter":         knownvalue.StringExact("2"),
						"net.ipv4.conf.all.nested.deep.value": knownvalue.StringExact("deep"),
					})),
					expectSysctlData("persisted_values", knownvalue.NotNull()),
					expectSysctlData("exists", knownvalue.Null()),
					expectSysctlData("value", knownvalue.Null()),
					expectSysctlData("persisted_value", knownvalue.Null()),
				},
			},
			{
				Config: sysctlDataHCL("prefix", "nosuch"),
				ConfigStateChecks: []statecheck.StateCheck{
					expectSysctlData("values", knownvalue.MapExact(map[string]knownvalue.Check{})),
				},
			},
		},
	})
}

func TestSysctlDataSource_rootDir(t *testing.T) {
	dir := t.TempDir()
	writeTestTree(t, dir, sysctlConfTree)
	provider := fmt.Sprintf("provider \"sysutils\" {\n  root_dir = %q\n}\n", dir)
	resource.UnitTest(t, resource.TestCase{
		// The fake /proc/sys would have values; with root_dir they are
		// not read at all.
		ProtoV6ProviderFactories: sysctlProviderFactories(newTestProcSys(t)),
		Steps: []resource.TestStep{
			{
				Config: provider + sysctlDataHCL("name", "net.ipv4.ip_forward"),
				ConfigStateChecks: []statecheck.StateCheck{
					expectSysctlData("exists", knownvalue.Null()),
					expectSysctlData("value", knownvalue.Null()),
					expectSysctlData("persisted_value", knownvalue.StringExact("1")),
					expectSysctlData("persisted_file", knownvalue.StringExact("/etc/sysctl.d/10-base.conf")),
				},
			},
			{
				Config: provider + sysctlDataHCL("name", "vm.dirty_ratio"),
				ConfigStateChecks: []statecheck.StateCheck{
					expectSysctlData("persisted_value", knownvalue.Null()),
					expectSysctlData("persisted_file", knownvalue.Null()),
				},
			},
			{
				Config: provider + sysctlDataHCL("prefix", "net.ipv4"),
				ConfigStateChecks: []statecheck.StateCheck{
					expectSysctlData("values", knownvalue.Null()),
					expectSysctlData("persisted_values", knownvalue.MapExact(map[string]knownvalue.Check{
						"net.ipv4.ip_forward":              knownvalue.StringExact("1"),
						"net.ipv4.conf.all.rp_filter":      knownvalue.StringExact("2"),
						"net.ipv4.tcp_syncookies":          knownvalue.StringExact("1"),
						"net.ipv4.ip_local_port_range":     knownvalue.StringExact("1024 65000"),
						"net.ipv4.conf.*.accept_redirects": knownvalue.StringExact("0"),
					})),
				},
			},
		},
	})
}

func TestSysctlDataSource_validation(t *testing.T) {
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      `data "sysutils_sysctl" "test" {}`,
				ExpectError: regexp.MustCompile(`(?s)No\s+attribute\s+specified`),
			},
			{
				Config: `
data "sysutils_sysctl" "test" {
  name   = "vm.swappiness"
  prefix = "vm"
}`,
				ExpectError: regexp.MustCompile(`(?s)Invalid\s+Attribute\s+Combination`),
			},
			{
				Config:      sysctlDataHCL("name", "vm"),
				ExpectError: regexp.MustCompile(`at\s+least\s+two\s+components`),
			},
			{
				Config:      sysctlDataHCL("prefix", "../etc"),
				ExpectError: regexp.MustCompile(`Invalid`),
			},
		},
	})
}

// TestAccSysctlDataSource_realKernel reads the real /proc/sys and sysctl
// configuration, which needs no privileges.
func TestAccSysctlDataSource_realKernel(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: sysctlDataHCL("name", "kernel.ostype"),
				ConfigStateChecks: []statecheck.StateCheck{
					expectSysctlData("exists", knownvalue.Bool(true)),
					expectSysctlData("value", knownvalue.StringExact("Linux")),
				},
			},
			{
				Config: sysctlDataHCL("prefix", "kernel"),
				ConfigStateChecks: []statecheck.StateCheck{
					expectSysctlData("values", knownvalue.MapPartial(map[string]knownvalue.Check{
						"kernel.ostype": knownvalue.StringExact("Linux"),
					})),
					expectSysctlData("persisted_values", knownvalue.NotNull()),
				},
			},
		},
	})
}
