package provider

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSysctlComponents(t *testing.T) {
	valid := map[string][]string{
		"net.ipv4.ip_forward":                {"net", "ipv4", "ip_forward"},
		"fs.lease-break-time":                {"fs", "lease-break-time"},
		"net.ipv4.conf.eth0/100.forwarding":  {"net", "ipv4", "conf", "eth0.100", "forwarding"},
		"net.ipv6.conf.br-lan.disable_ipv6":  {"net", "ipv6", "conf", "br-lan", "disable_ipv6"},
		"net.ipv4.neigh.wg@home.gc_stale_ti": {"net", "ipv4", "neigh", "wg@home", "gc_stale_ti"},
	}
	for name, want := range valid {
		got, err := sysctlComponents(name)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("sysctlComponents(%q) = %q, %v; want %q", name, got, err, want)
		}
	}

	invalid := []string{
		"",
		"kernel",
		"net/ipv4/ip_forward", // Slash form: one component.
		".net.ipv4",
		"net.ipv4.",
		"net..ipv4",
		"net.//.passwd", // "//" stands for "..".
		"net./.x",       // "/" stands for ".".
		"net.ipv4.//",
		"../../etc.passwd",
		"net.ipv4.ip forward",
		"net.ipv4.*.rp_filter",
		"net.ipv4.ip_forward\x00",
		"net.ipv4.ip_forward\n",
		"net.ipv4.ip\\forward",
		"kernel." + strings.Repeat("a", maxSysctlNameLen),
	}
	for _, name := range invalid {
		if got, err := sysctlComponents(name); err == nil {
			t.Errorf("sysctlComponents(%q) = %q, want an error", name, got)
		}
	}
}

func TestValidateSysctlValue(t *testing.T) {
	for _, v := range []string{"1", "", "32768 60999", "|/usr/lib/systemd/systemd-coredump %P %u", "4096\t87380\t6291456"} {
		if err := validateSysctlValue(v); err != nil {
			t.Errorf("validateSysctlValue(%q) = %v", v, err)
		}
	}
	for _, v := range []string{"1\n", "1\r", "a\x00b", " 1", "1 ", strings.Repeat("1", maxSysctlValueLen+1)} {
		if err := validateSysctlValue(v); err == nil {
			t.Errorf("validateSysctlValue(%q) succeeded", v)
		}
	}
}

// newFakeProcSys returns a directory laid out like /proc/sys with the given
// parameters.
func newFakeProcSys(t *testing.T, params map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, v := range params {
		setFakeSysctl(t, root, name, v)
	}
	return root
}

func fakeSysctlPath(t *testing.T, root, name string) string {
	t.Helper()
	parts, err := sysctlComponents(name)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(append([]string{root}, parts...)...)
}

func setFakeSysctl(t *testing.T, root, name, value string) {
	t.Helper()
	p := fakeSysctlPath(t, root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(value+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadWriteSysctl(t *testing.T) {
	root := newFakeProcSys(t, map[string]string{
		"net.ipv4.ip_local_port_range":      "32768\t60999",
		"net.ipv4.conf.eth0/100.forwarding": "0",
	})
	if v, err := readSysctl(root, "net.ipv4.ip_local_port_range"); err != nil || v != "32768 60999" {
		t.Errorf("readSysctl = %q, %v; want normalized \"32768 60999\"", v, err)
	}
	if err := writeSysctl(root, "net.ipv4.conf.eth0/100.forwarding", "1"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "net", "ipv4", "conf", "eth0.100", "forwarding"))
	if err != nil || string(data) != "1\n" {
		t.Errorf("forwarding = %q, %v; want \"1\\n\"", data, err)
	}

	if _, err := readSysctl(root, "net.ipv4.missing"); !errors.Is(err, errSysctlNotFound) {
		t.Errorf("reading a missing parameter: %v, want errSysctlNotFound", err)
	}
	if _, err := readSysctl(root, "net.ipv4.ip_local_port_range.x"); !errors.Is(err, errSysctlNotFound) {
		t.Errorf("reading below a file: %v, want errSysctlNotFound", err)
	}
	if _, err := readSysctl(root, "net.ipv4"); err == nil || !strings.Contains(err.Error(), "group of kernel parameters") {
		t.Errorf("reading a directory: %v", err)
	}
}

// TestOpenSysctl_confined checks that neither the name nor symlinks in the
// tree can make the provider read or write outside the /proc/sys root.
func TestOpenSysctl_confined(t *testing.T) {
	root := newFakeProcSys(t, map[string]string{"kernel.hostname": "host"})
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "kernel", "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(root, "dirlink")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"kernel.link", "dirlink.secret"} {
		if v, err := readSysctl(root, name); err == nil {
			t.Errorf("readSysctl(%q) = %q through a symlink", name, v)
		}
		if err := writeSysctl(root, name, "x"); err == nil {
			t.Errorf("writeSysctl(%q) wrote through a symlink", name)
		}
	}
	for _, name := range []string{"kernel.//.//.etc.passwd", "kernel..hostname"} {
		if _, err := readSysctl(root, name); err == nil || errors.Is(err, errSysctlNotFound) {
			t.Errorf("readSysctl(%q) = %v, want a validation error", name, err)
		}
	}
	if data, _ := os.ReadFile(outside); string(data) != "secret\n" {
		t.Errorf("file outside the root was modified: %q", data)
	}
}

func TestParseSysctlLine(t *testing.T) {
	cases := map[string]*sysctlFileEntry{
		"net.ipv4.ip_forward = 1":                 {"net.ipv4.ip_forward", "1"},
		"  net.ipv4.ip_forward=1  ":               {"net.ipv4.ip_forward", "1"},
		"-net.ipv4.ip_forward = 1":                {"net.ipv4.ip_forward", "1"},
		"net/ipv4/conf/eth0.100/forwarding = 1":   {"net.ipv4.conf.eth0/100.forwarding", "1"},
		"net.ipv4.ip_local_port_range = 1024 999": {"net.ipv4.ip_local_port_range", "1024 999"},
		"kernel.core_pattern = |/bin/x %p = y":    {"kernel.core_pattern", "|/bin/x %p = y"},
		"# net.ipv4.ip_forward = 1":               nil,
		"; net.ipv4.ip_forward = 1":               nil,
		"":                                        nil,
		"net.ipv4.ip_forward":                     nil,
	}
	for line, want := range cases {
		got, ok := parseSysctlLine(line)
		switch {
		case want == nil && ok:
			t.Errorf("parseSysctlLine(%q) = %+v, want no entry", line, got)
		case want != nil && (!ok || got != *want):
			t.Errorf("parseSysctlLine(%q) = %+v, %v; want %+v", line, got, ok, *want)
		}
	}
}

func TestSysctlFileEditing(t *testing.T) {
	const orig = "# Local settings\nnet.ipv4.ip_forward=1\nvm.swappiness = 10\n-net.ipv4.ip_forward = 0\n"
	tf := parseTextFile([]byte(orig))
	if v, n := lookupSysctlEntry(tf, "net.ipv4.ip_forward"); v != "0" || n != 2 {
		t.Errorf("lookup = %q, %d; want the last of 2 entries, \"0\"", v, n)
	}

	// Setting the value of the first entry keeps its formatting but drops
	// the duplicate.
	if !setSysctlEntry(tf, "net.ipv4.ip_forward", "1") {
		t.Error("setSysctlEntry reported no change despite a duplicate")
	}
	if got, want := string(tf.bytes()), "# Local settings\nnet.ipv4.ip_forward=1\nvm.swappiness = 10\n"; got != want {
		t.Errorf("after set:\n%s\nwant\n%s", got, want)
	}
	if setSysctlEntry(tf, "net.ipv4.ip_forward", "1") {
		t.Error("setting the same value again reported a change")
	}
	if !setSysctlEntry(tf, "vm.swappiness", "60") || !setSysctlEntry(tf, "net.ipv4.ip_local_port_range", "1024\t65535") {
		t.Error("setSysctlEntry reported no change")
	}
	if setSysctlEntry(tf, "net.ipv4.ip_local_port_range", "1024 65535") {
		t.Error("a value differing only in white space was rewritten")
	}
	want := "# Local settings\nnet.ipv4.ip_forward=1\nvm.swappiness = 60\nnet.ipv4.ip_local_port_range = 1024\t65535\n"
	if got := string(tf.bytes()); got != want {
		t.Errorf("after edits:\n%s\nwant\n%s", got, want)
	}
	if !removeSysctlEntries(tf, "vm.swappiness") || removeSysctlEntries(tf, "vm.swappiness") {
		t.Error("removeSysctlEntries reported the wrong change")
	}

	// A file without a trailing newline gets one before the appended line.
	tf = parseTextFile([]byte("a.b = 1"))
	setSysctlEntry(tf, "c.d", "2")
	if got := string(tf.bytes()); got != "a.b = 1\nc.d = 2\n" {
		t.Errorf("append without trailing newline: %q", got)
	}
}

func TestEditSysctlFile_createAndRemove(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sysctl.d", "99-test.conf")
	if err := editSysctlFile(p, func(t *textFile) bool { return removeSysctlEntries(t, "a.b") }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(p)); !os.IsNotExist(err) {
		t.Errorf("removing from a missing file created its directory: %v", err)
	}
	if err := editSysctlFile(p, func(t *textFile) bool { return setSysctlEntry(t, "a.b", "1") }); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil || info.Mode().Perm() != sysctlFileCreateMode {
		t.Fatalf("created file: %v, %v", info, err)
	}
	if err := editSysctlFile(p, func(t *textFile) bool { return removeSysctlEntries(t, "a.b") }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("file left without lines was not removed: %v", err)
	}
}
