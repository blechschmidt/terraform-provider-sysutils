package provider

// Regression tests for the security review of the mount, sysctl,
// kernel_module and cron_job resources and of root_dir.

import (
	"slices"
	"strings"
	"testing"
)

// A mount point may contain Unicode white space, which the kernel does not
// escape in mountinfo. An unprivileged user can create such a mount point
// with FUSE; it must not be split into several fields.
func TestParseMountInfoUnicodeSpace(t *testing.T) {
	const nbsp = " "
	data := "22 1 8:1 / / rw - ext4 /dev/sda1 rw\n" +
		"40 22 0:50 / /tmp/data" + nbsp + "-" + nbsp + "x rw,nosuid shared:3 - fuse.evil src" + nbsp + "x rw\n"
	entries, err := parseMountInfo(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	e := entries[1]
	if e.mountPoint != "/tmp/data"+nbsp+"-"+nbsp+"x" || e.fstype != "fuse.evil" || e.source != "src"+nbsp+"x" {
		t.Errorf("entry = %+v", e)
	}
	if topMount(entries, "/tmp/data") != nil {
		t.Error("the FUSE mount was mistaken for a mount at /tmp/data")
	}
}

// Likewise, a device with Unicode white space must not shift the other
// fields of an fstab line, or the line would be taken for another mount
// point's entry and edited or removed by its resource.
func TestParseFstabLineUnicodeSpace(t *testing.T) {
	want := fstabEntry{device: "a /srv/other", mountPoint: "/srv/data", fstype: "ext4", options: []string{"defaults"}}
	got, ok := parseFstabLine(want.String())
	if !ok || !got.equal(want) {
		t.Fatalf("parseFstabLine(%q) = %+v, %v; want %+v", want.String(), got, ok, want)
	}
	if idx := findFstabEntries([]string{want.String()}, "/srv/other"); len(idx) != 0 {
		t.Errorf("entry for /srv/data matched /srv/other")
	}
}

// An unbalanced quote in one option would swallow the next ones when mount(8)
// and libmount split the joined option string.
func TestValidateMountOptionUnbalancedQuote(t *testing.T) {
	for _, bad := range []string{`context="a`, `x"`, `a"b"c"`} {
		if err := validateMountOption(bad); err == nil {
			t.Errorf("validateMountOption(%q) = nil", bad)
		}
	}
	opts := []string{`context="a`, `b"`}
	if got := splitMountOptions(strings.Join(opts, ",")); slices.Equal(got, opts) {
		t.Fatalf("test premise: %q splits back into %q", opts, got)
	}
}

// A "/" in the first component makes sysctl(8) and systemd-sysctl read the
// persisted key in the slash-separated form, which names another parameter
// than the one written to /proc/sys.
func TestSysctlNameSlashInFirstComponent(t *testing.T) {
	const name = "kernel/x.y"
	if err := validateSysctlName(name); err == nil {
		live, _ := sysctlComponents(name)
		t.Errorf("validateSysctlName(%q) = nil; it is set as %q but persisted as %q",
			name, strings.Join(live, "/"), canonicalSysctlKey(name))
	}
	if err := validateSysctlName("net.ipv4.conf.eth0/100.forwarding"); err != nil {
		t.Errorf("VLAN interface name refused: %v", err)
	}
}

// The persistence file must be a .conf file: nothing else is read at boot,
// and the resource must not append entries to arbitrary files.
func TestValidateSysctlFile(t *testing.T) {
	for _, ok := range []string{"/etc/sysctl.d/99-terraform.conf", "/etc/sysctl.conf", "/run/sysctl.d/a.conf"} {
		if err := validateSysctlFile(ok); err != nil {
			t.Errorf("validateSysctlFile(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"/etc/passwd", "/etc/sudoers.d/x", "/etc/sysctl.d/.conf", "/etc/sysctl.d/x.conf.bak", "etc/x.conf", "/"} {
		if err := validateSysctlFile(bad); err == nil {
			t.Errorf("validateSysctlFile(%q) = nil", bad)
		}
	}
}
