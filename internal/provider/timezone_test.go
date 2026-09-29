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

// fakeTZif returns the content of a fake compiled zone file for name. Only
// the magic number matters to the provider.
func fakeTZif(name string) string { return "TZif2\x00fake zone " + name + "\n" }

// writeZoneinfo creates fake zone files for names below root.
func writeZoneinfo(t *testing.T, root string, names ...string) {
	t.Helper()
	for _, n := range names {
		mustWrite(t, filepath.Join(root, zoneinfoDir, n), fakeTZif(n))
	}
}

// testTimezoneRoot returns a root filesystem tree with a few zone files,
// an /etc directory and the given distribution marker file (or none).
func testTimezoneRoot(t *testing.T, marker string) string {
	t.Helper()
	root := testRootDir(t)
	mustMkdir(t, filepath.Join(root, "etc"))
	writeZoneinfo(t, root, "Etc/UTC", "Europe/Berlin", "America/New_York", "Etc/GMT+5")
	// As in Debian, UTC is a symlink to Etc/UTC.
	mustSymlink(t, "Etc/UTC", filepath.Join(root, zoneinfoDir, "UTC"))
	mustWrite(t, filepath.Join(root, zoneinfoDir, "leapseconds"), "# not a zone file\n")
	if marker != "" {
		mustWrite(t, filepath.Join(root, marker), "12.5\n")
	}
	return root
}

func readLinkOrFail(t *testing.T, p string) string {
	t.Helper()
	target, err := os.Readlink(p)
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func TestValidateTimezoneName(t *testing.T) {
	for _, ok := range []string{"UTC", "Europe/Berlin", "America/Argentina/Buenos_Aires", "America/Port-au-Prince", "Etc/GMT+5", "Etc/GMT-14", "posix/Europe/Berlin", "EST5EDT"} {
		if err := validateTimezoneName(ok); err != nil {
			t.Errorf("validateTimezoneName(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "/UTC", "UTC/", "Europe//Berlin", "../etc/passwd", "Europe/../../etc/shadow", ".", "Europe/.hidden", "-UTC", "Europe/Berlin ", "Europe/Berl\nin", "Europe\\Berlin", strings.Repeat("A", 256)} {
		if err := validateTimezoneName(bad); err == nil {
			t.Errorf("validateTimezoneName(%q) = nil, want an error", bad)
		}
	}
}

func TestZoneFromLinkTarget(t *testing.T) {
	for target, want := range map[string]string{
		"/usr/share/zoneinfo/Europe/Berlin":         "Europe/Berlin",
		"../usr/share/zoneinfo/Etc/UTC":             "Etc/UTC",
		"/usr/share/zoneinfo//America/New_York":     "America/New_York",
		"../../usr/share/zoneinfo/UTC":              "UTC",
		"/usr/share/zoneinfo/../zoneinfo/Etc/GMT+5": "Etc/GMT+5",
	} {
		if got, ok := zoneFromLinkTarget(target); !ok || got != want {
			t.Errorf("zoneFromLinkTarget(%q) = %q, %v; want %q", target, got, ok, want)
		}
	}
	for _, target := range []string{"/etc/zoneinfo/Europe/Berlin", "/usr/share/zoneinfo", "/usr/share/zoneinfo/", "zoneinfo/UTC", "/var/db/timezone/zoneinfo/UTC"} {
		if got, ok := zoneFromLinkTarget(target); ok {
			t.Errorf("zoneFromLinkTarget(%q) = %q, true; want false", target, got)
		}
	}
}

func TestReadZoneinfo(t *testing.T) {
	root := testTimezoneRoot(t, "")
	r, err := newFSRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := readZoneinfo(r, "UTC"); err != nil || string(data) != fakeTZif("Etc/UTC") {
		t.Errorf("readZoneinfo(UTC) = %q, %v; want the file of Etc/UTC", data, err)
	}
	for name, want := range map[string]string{
		"Mars/Olympus_Mons": "does not exist",
		"leapseconds":       "not a compiled zone file",
		"zone.tab":          "must be an IANA time zone name",
		"Europe":            "is not a regular file",
		"../etc":            "must be an IANA time zone name",
	} {
		if _, err := readZoneinfo(r, name); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("readZoneinfo(%q) = %v, want an error containing %q", name, err, want)
		}
	}
	// A symlink in the zoneinfo tree is resolved inside the root: an
	// absolute target that exists on the host but not in the root fails.
	mustSymlink(t, "/etc/hostname", filepath.Join(root, zoneinfoDir, "Evil"))
	if _, err := readZoneinfo(r, "Evil"); err == nil {
		t.Error("readZoneinfo followed a symlink out of the root")
	}
	// A relative symlink that leads above the root is refused.
	mustSymlink(t, "../../../../../../../etc/hostname", filepath.Join(root, zoneinfoDir, "Escape"))
	if _, err := readZoneinfo(r, "Escape"); err == nil || !errors.Is(err, errEscapesRoot) {
		t.Errorf("readZoneinfo(Escape) = %v, want errEscapesRoot", err)
	}
}

func TestSetTimezone_rootDir(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		marker     string
		wantTZFile bool
	}{
		{"etc/debian_version", true},
		{"etc/alpine-release", true},
		{"etc/fedora-release", false},
	} {
		t.Run(filepath.Base(tc.marker), func(t *testing.T) {
			root := testTimezoneRoot(t, tc.marker)
			r, err := newFSRoot(root)
			if err != nil {
				t.Fatal(err)
			}
			lt := filepath.Join(root, "etc", "localtime")
			tz := filepath.Join(root, "etc", "timezone")
			mustSymlink(t, "/usr/share/zoneinfo/Etc/UTC", lt)

			before, err := readTimezoneSnapshot(r)
			if err != nil {
				t.Fatal(err)
			}
			if z, ok := before.zone(r, ""); !ok || z != "Etc/UTC" {
				t.Errorf("zone before = %q, %v; want Etc/UTC", z, ok)
			}

			// timedatectl is never used below a root_dir, even if the
			// fake says systemd is running.
			cfg := &timezoneConfig{
				runDir:   fakeSystemdRunDir(t),
				lookPath: func(string) (string, error) { return "/bin/timedatectl", nil },
				run: func(context.Context, execSpec) (*execResult, error) {
					t.Error("timedatectl run for a root_dir")
					return nil, errors.New("unexpected")
				},
			}
			warnings, err := setTimezone(ctx, cfg, r, "Europe/Berlin")
			if err != nil || len(warnings) > 0 {
				t.Fatalf("setTimezone = %v, %v", warnings, err)
			}
			if got := readLinkOrFail(t, lt); got != "../usr/share/zoneinfo/Europe/Berlin" {
				t.Errorf("localtime -> %q", got)
			}
			data, err := os.ReadFile(tz)
			switch {
			case tc.wantTZFile && (err != nil || string(data) != "Europe/Berlin\n"):
				t.Errorf("/etc/timezone = %q, %v; want Europe/Berlin", data, err)
			case !tc.wantTZFile && !os.IsNotExist(err):
				t.Errorf("/etc/timezone was created (%v) where the distribution does not use it", err)
			}
			after, err := readTimezoneSnapshot(r)
			if err != nil {
				t.Fatal(err)
			}
			if z, ok := after.zone(r, "Europe/Berlin"); !ok || z != "Europe/Berlin" {
				t.Errorf("zone after = %q, %v", z, ok)
			}

			// Setting the same zone again changes nothing.
			info1, _ := os.Lstat(lt)
			if _, err := setTimezone(ctx, cfg, r, "Europe/Berlin"); err != nil {
				t.Fatal(err)
			}
			if info2, _ := os.Lstat(lt); !os.SameFile(info1, info2) {
				t.Error("an unchanged zone replaced /etc/localtime")
			}

			// Restoring puts back the symlink as it was and removes the
			// /etc/timezone that did not exist.
			if _, err := restoreTimezone(context.Background(), nil, r, before); err != nil {
				t.Fatal(err)
			}
			if got := readLinkOrFail(t, lt); got != "/usr/share/zoneinfo/Etc/UTC" {
				t.Errorf("restored localtime -> %q", got)
			}
			if _, err := os.Lstat(tz); !os.IsNotExist(err) {
				t.Errorf("restored /etc/timezone exists (%v)", err)
			}
			checkNoTempFilesT(t, filepath.Join(root, "etc"))
		})
	}
}

func checkNoTempFilesT(t *testing.T, dir string) {
	t.Helper()
	if err := checkNoTempFiles(dir)(nil); err != nil {
		t.Error(err)
	}
}

func fakeSystemdRunDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	mustMkdir(t, filepath.Join(d, "systemd", "system"))
	return d
}

func TestSetTimezone_unknownZoneChangesNothing(t *testing.T) {
	root := testTimezoneRoot(t, "etc/debian_version")
	r, _ := newFSRoot(root)
	lt := filepath.Join(root, "etc", "localtime")
	mustSymlink(t, "/usr/share/zoneinfo/Etc/UTC", lt)
	if _, err := setTimezone(context.Background(), nil, r, "Mars/Olympus_Mons"); err == nil {
		t.Fatal("setTimezone accepted a zone without a zone file")
	}
	if got := readLinkOrFail(t, lt); got != "/usr/share/zoneinfo/Etc/UTC" {
		t.Errorf("localtime -> %q after a failed apply", got)
	}
	if _, err := os.Lstat(filepath.Join(root, "etc", "timezone")); !os.IsNotExist(err) {
		t.Errorf("/etc/timezone was written by a failed apply (%v)", err)
	}
}

func TestSetTimezone_replacesCopyAndRestoresIt(t *testing.T) {
	root := testTimezoneRoot(t, "etc/alpine-release")
	r, _ := newFSRoot(root)
	lt := filepath.Join(root, "etc", "localtime")
	tz := filepath.Join(root, "etc", "timezone")
	// Older installers copy the zone file instead of linking it.
	mustWrite(t, lt, fakeTZif("America/New_York"))
	mustWrite(t, tz, "America/New_York\n")
	mustChmod(t, tz, 0o640)

	before, err := readTimezoneSnapshot(r)
	if err != nil {
		t.Fatal(err)
	}
	if z, ok := before.zone(r, ""); !ok || z != "America/New_York" {
		t.Errorf("zone of a copied zone file = %q, %v; want America/New_York", z, ok)
	}
	if _, err := setTimezone(context.Background(), nil, r, "UTC"); err != nil {
		t.Fatal(err)
	}
	if got := readLinkOrFail(t, lt); got != "../usr/share/zoneinfo/UTC" {
		t.Errorf("localtime -> %q", got)
	}
	if err := checkFileContent(tz, "UTC\n")(nil); err != nil {
		t.Error(err)
	}
	if err := checkFileMode(tz, 0o640)(nil); err != nil {
		t.Error(err)
	}
	if _, err := restoreTimezone(context.Background(), nil, r, before); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(lt)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("restored localtime is not a regular file: %v, %v", info, err)
	}
	if err := checkFileContent(lt, fakeTZif("America/New_York"))(nil); err != nil {
		t.Error(err)
	}
	if err := checkFileContent(tz, "America/New_York\n")(nil); err != nil {
		t.Error(err)
	}
}

func TestTimezoneSnapshotZone(t *testing.T) {
	root := testTimezoneRoot(t, "")
	r, _ := newFSRoot(root)
	str := func(s string) *string { return &s }
	for _, tc := range []struct {
		name    string
		snap    timezoneSnapshot
		applied string
		want    string
		wantOK  bool
	}{
		{"absent", timezoneSnapshot{Localtime: localtimeEntry{Kind: localtimeAbsent}}, "", "", false},
		{"link", timezoneSnapshot{Localtime: localtimeEntry{Kind: localtimeSymlink, Target: "/usr/share/zoneinfo/Europe/Berlin"}}, "", "Europe/Berlin", true},
		{"foreign link", timezoneSnapshot{Localtime: localtimeEntry{Kind: localtimeSymlink, Target: "/etc/zoneinfo/Europe/Berlin"}}, "Europe/Berlin", "/etc/zoneinfo/Europe/Berlin", false},
		{"copy", timezoneSnapshot{Localtime: localtimeEntry{Kind: localtimeFile, Content: []byte(fakeTZif("Europe/Berlin"))}, TimezoneFile: str("Europe/Berlin\n")}, "", "Europe/Berlin", true},
		{"copy of another zone", timezoneSnapshot{Localtime: localtimeEntry{Kind: localtimeFile, Content: []byte(fakeTZif("Etc/UTC"))}, TimezoneFile: str("Europe/Berlin\n")}, "", "", false},
		{"copy without timezone file", timezoneSnapshot{Localtime: localtimeEntry{Kind: localtimeFile, Content: []byte(fakeTZif("Etc/UTC"))}}, "", "", false},
		// /etc/timezone disagreeing with the link is drift.
		{"timezone file drift", timezoneSnapshot{Localtime: localtimeEntry{Kind: localtimeSymlink, Target: "../usr/share/zoneinfo/Europe/Berlin"}, TimezoneFile: str("Etc/UTC\n")}, "Europe/Berlin", "Etc/UTC", true},
		{"link drift wins", timezoneSnapshot{Localtime: localtimeEntry{Kind: localtimeSymlink, Target: "../usr/share/zoneinfo/Etc/UTC"}, TimezoneFile: str("Etc/UTC\n")}, "Europe/Berlin", "Etc/UTC", true},
		{"timezone file garbage", timezoneSnapshot{Localtime: localtimeEntry{Kind: localtimeSymlink, Target: "../usr/share/zoneinfo/Europe/Berlin"}, TimezoneFile: str("")}, "Europe/Berlin", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, ok := tc.snap.zone(r, tc.applied); got != tc.want || ok != tc.wantOK {
				t.Errorf("zone = %q, %v; want %q, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestReplaceWithSymlink_refusesDirectory(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "localtime")
	mustMkdir(t, p)
	if err := replaceWithSymlink(p, "../usr/share/zoneinfo/UTC"); err == nil {
		t.Fatal("replaceWithSymlink replaced a directory")
	}
	if info, err := os.Lstat(p); err != nil || !info.IsDir() {
		t.Errorf("directory changed: %v, %v", info, err)
	}
	checkNoTempFilesT(t, dir)
}

func TestTimezoneUseTimedatectl(t *testing.T) {
	found := func(string) (string, error) { return "/usr/bin/timedatectl", nil }
	notFound := func(string) (string, error) { return "", errors.New("not found") }
	rooted, _ := newFSRoot(t.TempDir())
	for _, tc := range []struct {
		name string
		root *fsRoot
		cfg  *timezoneConfig
		want bool
	}{
		{"systemd", hostRoot, &timezoneConfig{runDir: fakeSystemdRunDir(t), lookPath: found}, true},
		{"no timedatectl", hostRoot, &timezoneConfig{runDir: fakeSystemdRunDir(t), lookPath: notFound}, false},
		{"not booted with systemd", hostRoot, &timezoneConfig{runDir: t.TempDir(), lookPath: found}, false},
		{"root_dir", rooted, &timezoneConfig{runDir: fakeSystemdRunDir(t), lookPath: found}, false},
	} {
		if got := tc.cfg.useTimedatectl(tc.root); got != tc.want {
			t.Errorf("%s: useTimedatectl = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestTimezoneSetWithTimedatectl(t *testing.T) {
	var calls [][]string
	cfg := &timezoneConfig{run: func(_ context.Context, spec execSpec) (*execResult, error) {
		if spec.Timeout <= 0 {
			t.Error("timedatectl run without a timeout")
		}
		calls = append(calls, spec.Argv)
		res := &execResult{Stdout: newCappedOutput(1024), Stderr: newCappedOutput(1024)}
		if spec.Argv[len(spec.Argv)-1] == "Bad/Zone" {
			res.ExitCode = 1
			_, _ = res.Stderr.Write([]byte("Failed to set time zone: Invalid or not installed time zone 'Bad/Zone'\n"))
		}
		return res, nil
	}}
	if err := cfg.setWithTimedatectl(context.Background(), "Europe/Berlin"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"timedatectl", "--no-ask-password", "set-timezone", "--", "Europe/Berlin"}; len(calls) != 1 || !slices.Equal(calls[0], want) {
		t.Errorf("calls = %q, want %q", calls, want)
	}
	if err := cfg.setWithTimedatectl(context.Background(), "Bad/Zone"); err == nil || !strings.Contains(err.Error(), "Invalid or not installed") {
		t.Errorf("error = %v, want timedatectl's message", err)
	}
}
