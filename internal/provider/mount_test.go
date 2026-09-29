package provider

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseMountInfo(t *testing.T) {
	data := `22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw,errors=remount-ro
36 22 0:32 / /mnt/with\040space rw,nosuid shared:2 master:1 - tmpfs my\040tmpfs rw,size=1024k
37 36 0:33 /sub /mnt/with\040space ro,relatime - nfs4 server:/export ro,vers=4.2
`
	entries, err := parseMountInfo(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}
	e := entries[1]
	if e.mountPoint != "/mnt/with space" || e.source != "my tmpfs" || e.fstype != "tmpfs" || e.major != 0 || e.minor != 32 || e.root != "/" {
		t.Errorf("entry 1 = %+v", e)
	}
	if !slices.Equal(e.options, []string{"rw", "nosuid"}) || !slices.Equal(e.superOptions, []string{"rw", "size=1024k"}) {
		t.Errorf("entry 1 options = %v / %v", e.options, e.superOptions)
	}
	if e.readOnly() || !entries[2].readOnly() {
		t.Errorf("readOnly: entry 1 %v, entry 2 %v", e.readOnly(), entries[2].readOnly())
	}
	// Of stacked mounts, the most recent is on top.
	if top := topMount(entries, "/mnt/with space"); top == nil || top.fstype != "nfs4" {
		t.Errorf("topMount = %+v, want the nfs4 mount", top)
	}
	if topMount(entries, "/mnt") != nil {
		t.Error("topMount(/mnt) != nil")
	}

	for _, bad := range []string{"1 2 3", "22 1 8:1 / / rw - ext4", "22 1 x / / rw - ext4 /dev/sda1 rw"} {
		if _, err := parseMountInfo(bad); err == nil {
			t.Errorf("parseMountInfo(%q) succeeded", bad)
		}
	}
}

func TestSystemMounterReadsMountInfo(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(p, []byte("36 22 0:32 / /srv rw - tmpfs tmpfs rw\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e, err := lookupMount(systemMounter{mountInfo: p}, "/srv")
	if err != nil || e == nil || e.fstype != "tmpfs" {
		t.Fatalf("lookupMount = %+v, %v", e, err)
	}
}

func TestSystemMounterCommands(t *testing.T) {
	var last execSpec
	m := systemMounter{run: cannedRunner(&last, 0, "", "", false), timeout: time.Minute}
	ctx := context.Background()

	if err := m.mount(ctx, "tmpfs", "/srv/x", "tmpfs", []string{"size=1m", "mode=0755"}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"mount", "-t", "tmpfs", "-o", "size=1m,mode=0755", "--", "tmpfs", "/srv/x"}; !slices.Equal(last.Argv, want) {
		t.Errorf("mount argv = %q, want %q", last.Argv, want)
	}
	if last.Timeout != time.Minute || last.MaxOutputBytes == 0 {
		t.Errorf("mount spec: timeout %s, max output %d", last.Timeout, last.MaxOutputBytes)
	}
	if err := m.remount(ctx, "tmpfs", "/srv/x", []string{"size=2m"}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"mount", "-o", "remount,size=2m", "--", "tmpfs", "/srv/x"}; !slices.Equal(last.Argv, want) {
		t.Errorf("remount argv = %q, want %q", last.Argv, want)
	}
	if err := m.unmount(ctx, "/srv/x"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"umount", "--", "/srv/x"}; !slices.Equal(last.Argv, want) {
		t.Errorf("umount argv = %q, want %q", last.Argv, want)
	}

	m.run = cannedRunner(&last, 32, "", "umount: /srv/x: target is busy.\n", false)
	if err := m.unmount(ctx, "/srv/x"); err == nil || !strings.Contains(err.Error(), "target is busy") || !strings.Contains(err.Error(), "exit status 32") {
		t.Errorf("unmount error = %v", err)
	}
	m.run = cannedRunner(&last, 0, "", "", true)
	if err := m.mount(ctx, "srv:/x", "/srv/x", "nfs", []string{"defaults"}); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("mount error = %v", err)
	}
}

func TestParseFstabLine(t *testing.T) {
	tests := []struct {
		line string
		want *fstabEntry
	}{
		{"", nil},
		{"   ", nil},
		{"# UUID=x / ext4 defaults 0 1", nil},
		{"  #tmpfs /tmp tmpfs defaults 0 0", nil},
		{"/dev/sdb1\t/data  ext4 noatime,nodev 0 2", &fstabEntry{device: "/dev/sdb1", mountPoint: "/data", fstype: "ext4", options: []string{"noatime", "nodev"}, pass: 2}},
		{"tmpfs /tmp tmpfs", &fstabEntry{device: "tmpfs", mountPoint: "/tmp", fstype: "tmpfs", options: []string{"defaults"}}},
		{`//srv/my\040share /mnt/my\040share cifs credentials=/root/c 0 0`, &fstabEntry{device: "//srv/my share", mountPoint: "/mnt/my share", fstype: "cifs", options: []string{"credentials=/root/c"}}},
		{"a /b c d x 0", &fstabEntry{device: "a", mountPoint: "/b", fstype: "c", options: []string{"d"}, dump: -1}},
	}
	for _, tt := range tests {
		got, ok := parseFstabLine(tt.line)
		if tt.want == nil {
			if ok {
				t.Errorf("parseFstabLine(%q) = %+v, want no entry", tt.line, got)
			}
			continue
		}
		if !ok || !got.equal(*tt.want) {
			t.Errorf("parseFstabLine(%q) = %+v, %v; want %+v", tt.line, got, ok, *tt.want)
		}
	}
}

func TestFstabEntryStringRoundTrip(t *testing.T) {
	e := fstabEntry{device: "LABEL=my data", mountPoint: "/mnt/a\tb#c\\d", fstype: "ext4",
		options: []string{"noatime", `context="system_u:object_r:tmp_t:s0:c127,c456"`}, dump: 1, pass: 2}
	s := e.String()
	if want := `LABEL=my\040data /mnt/a\011b\043c\134d ext4 noatime,context="system_u:object_r:tmp_t:s0:c127,c456" 1 2`; s != want {
		t.Errorf("String() = %q, want %q", s, want)
	}
	got, ok := parseFstabLine(s)
	if !ok || !got.equal(e) {
		t.Errorf("round trip = %+v, want %+v", got, e)
	}
}

func TestSetAndRemoveFstabEntries(t *testing.T) {
	const orig = "# comment\nUUID=abc / ext4 defaults 0 1\n\n#tmpfs /srv tmpfs defaults 0 0\nproc /proc proc defaults 0 0\n"
	e := fstabEntry{device: "tmpfs", mountPoint: "/srv", fstype: "tmpfs", options: []string{"size=1m"}}

	text := parseTextFile([]byte(orig))
	if !setFstabEntry(text, e) {
		t.Fatal("setFstabEntry on a file without the entry reported no change")
	}
	want := orig + "tmpfs /srv tmpfs size=1m 0 0\n"
	if got := string(text.bytes()); got != want {
		t.Fatalf("after add:\n%s\nwant:\n%s", got, want)
	}
	if setFstabEntry(text, e) {
		t.Error("setFstabEntry with an equal entry reported a change")
	}

	// An equivalent line with different spacing is left alone.
	text = parseTextFile([]byte("tmpfs\t/srv/\ttmpfs\tsize=1m\n"))
	if setFstabEntry(text, e) {
		t.Errorf("setFstabEntry rewrote an equivalent line: %q", text.bytes())
	}

	// A changed entry is replaced in place; duplicates are removed.
	text = parseTextFile([]byte("# head\ntmpfs /srv tmpfs size=2m 0 0\nproc /proc proc defaults 0 0\ntmpfs /srv tmpfs size=3m 0 0\n# tail"))
	if !setFstabEntry(text, e) {
		t.Fatal("setFstabEntry reported no change")
	}
	if got, want := string(text.bytes()), "# head\ntmpfs /srv tmpfs size=1m 0 0\nproc /proc proc defaults 0 0\n# tail"; got != want {
		t.Errorf("after replace = %q, want %q", got, want)
	}

	text = parseTextFile([]byte(want))
	if !removeFstabEntries(text, "/srv") {
		t.Fatal("removeFstabEntries reported no change")
	}
	if got := string(text.bytes()); got != orig {
		t.Errorf("after remove = %q, want %q", got, orig)
	}
	if removeFstabEntries(text, "/srv") {
		t.Error("removeFstabEntries on a file without the entry reported a change")
	}

	// Only whole mount points match, not prefixes.
	text = parseTextFile([]byte("tmpfs /srv2 tmpfs defaults 0 0\n"))
	if e, n := lookupFstabEntry(text, "/srv"); e != nil || n != 0 {
		t.Errorf("lookupFstabEntry(/srv) = %+v, %d", e, n)
	}
}

func TestEditFstabKeepsModeAndCreatesMissingFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "fstab")
	add := func(t *textFile) bool {
		return setFstabEntry(t, fstabEntry{device: "tmpfs", mountPoint: "/srv", fstype: "tmpfs", options: []string{"defaults"}})
	}
	if err := editFstab(p, add); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil || info.Mode().Perm() != fstabCreateMode {
		t.Fatalf("created fstab: %v, %v", info, err)
	}
	if err := os.Chmod(p, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := editFstab(p, func(t *textFile) bool { return removeFstabEntries(t, "/srv") }); err != nil {
		t.Fatal(err)
	}
	info, _ = os.Stat(p)
	data, _ := os.ReadFile(p)
	if info.Mode().Perm() != 0o640 || len(data) != 0 {
		t.Errorf("after remove: mode %o, content %q", info.Mode().Perm(), data)
	}

	// A symlinked fstab is refused rather than followed.
	link := filepath.Join(dir, "link")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	if err := editFstab(link, add); err == nil {
		t.Error("editFstab followed a symlink")
	}
}

func TestSplitMountOptions(t *testing.T) {
	for in, want := range map[string][]string{
		"defaults":                    {"defaults"},
		"rw,noatime":                  {"rw", "noatime"},
		`context="a:b:c,d",nodev`:     {`context="a:b:c,d"`, "nodev"},
		"a,,b":                        {"a", "", "b"},
		`x-systemd.requires=a.mount`:  {"x-systemd.requires=a.mount"},
		`uid=1000,gid=1000,umask=022`: {"uid=1000", "gid=1000", "umask=022"},
	} {
		if got := splitMountOptions(in); !slices.Equal(got, want) {
			t.Errorf("splitMountOptions(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMountValidators(t *testing.T) {
	for _, ok := range []string{"defaults", "size=1m", "x-systemd.automount", `context="a,b"`, "_netdev"} {
		if err := validateMountOption(ok); err != nil {
			t.Errorf("validateMountOption(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "a,b", "a b", "a\tb", "remount", "move", "a\n"} {
		if err := validateMountOption(bad); err == nil {
			t.Errorf("validateMountOption(%q) = nil", bad)
		}
	}
	for _, ok := range []string{"ext4", "fuse.sshfs", "nfs4", "ext4,xfs", "none"} {
		if err := validateFSType(ok); err != nil {
			t.Errorf("validateFSType(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "ext 4", "a/b", "x\n"} {
		if err := validateFSType(bad); err == nil {
			t.Errorf("validateFSType(%q) = nil", bad)
		}
	}
	for _, ok := range []string{"/dev/sda1", "UUID=abc", "//srv/a share", "tmpfs"} {
		if err := validateMountDevice(ok); err != nil {
			t.Errorf("validateMountDevice(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "a\nb", "a\x00"} {
		if err := validateMountDevice(bad); err == nil {
			t.Errorf("validateMountDevice(%q) = nil", bad)
		}
	}
}

func TestFstypeMatches(t *testing.T) {
	for _, c := range []struct {
		want, live string
		match      bool
	}{
		{"ext4", "ext4", true},
		{"ext4", "xfs", false},
		{"auto", "xfs", true},
		{"ext4,xfs", "xfs", true},
		{"nfs", "nfs4", true},
		{"cifs", "smb3", true},
		{"fuse.sshfs", "fuse.sshfs", true},
		{"fuse", "fuse.sshfs", true},
		{"tmpfs", "ramfs", false},
	} {
		if got := fstypeMatches(c.want, c.live); got != c.match {
			t.Errorf("fstypeMatches(%q, %q) = %v, want %v", c.want, c.live, got, c.match)
		}
	}
}

func TestDeviceDiffers(t *testing.T) {
	live := &mountEntry{source: "server:/export", fstype: "nfs4", major: 0, minor: 50}
	if deviceDiffers("server:/export/", "nfs", nil, live) {
		t.Error("trailing slash counted as a difference")
	}
	if !deviceDiffers("other:/export", "nfs", nil, live) {
		t.Error("different share not detected")
	}
	// Bind mounts and FUSE report a source that differs from the configured one.
	if deviceDiffers("/srv/data", "none", []string{"bind"}, &mountEntry{source: "/dev/sda1", fstype: "ext4", major: 8, minor: 1}) {
		t.Error("bind mount reported as different")
	}
	if deviceDiffers("user@host:", "fuse.sshfs", nil, &mountEntry{source: "user@host:/", fstype: "fuse.sshfs"}) {
		t.Error("FUSE mount reported as different")
	}
	// Paths that are not block devices, and unresolvable tags, can't be compared.
	if deviceDiffers(t.TempDir(), "ext4", nil, &mountEntry{source: "/dev/loop0", fstype: "ext4", major: 7}) {
		t.Error("loop image reported as different")
	}
	if deviceDiffers("UUID=does-not-exist", "ext4", nil, &mountEntry{source: "/dev/sda1", fstype: "ext4", major: 8, minor: 1}) {
		t.Error("unresolvable UUID reported as different")
	}
	if deviceDiffers("tmpfs", "tmpfs", nil, &mountEntry{source: "tmpfs", fstype: "tmpfs"}) {
		t.Error("equal tmpfs reported as different")
	}
	if !deviceDiffers("tmpfs", "tmpfs", nil, &mountEntry{source: "other", fstype: "tmpfs"}) {
		t.Error("different tmpfs source not detected")
	}
}

func TestCheckMountPoint(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mnt := filepath.Join(dir, "mnt")
	if err := checkMountPoint(mnt); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("missing mount point: %v", err)
	}
	if err := os.Mkdir(mnt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkMountPoint(mnt); err != nil {
		t.Errorf("directory: %v", err)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkMountPoint(file); err != nil {
		t.Errorf("file: %v", err)
	}
	if err := os.Symlink(mnt, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := checkMountPoint(filepath.Join(dir, "link")); err == nil {
		t.Error("symlink accepted")
	}
	if err := os.Mkdir(filepath.Join(mnt, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkMountPoint(filepath.Join(dir, "link", "sub")); err == nil || !strings.Contains(err.Error(), "resolves to") {
		t.Errorf("path through a symlink: %v", err)
	}
}
