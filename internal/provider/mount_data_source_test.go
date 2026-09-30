package provider

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

const testMountDataSource = "data.sysutils_mount.test"

func expectMountData(attr string, v knownvalue.Check) statecheck.StateCheck {
	return statecheck.ExpectKnownValue(testMountDataSource, tfjsonpath.New(attr), v)
}

func stringsExact(s ...string) knownvalue.Check {
	checks := make([]knownvalue.Check, len(s))
	for i, v := range s {
		checks[i] = knownvalue.StringExact(v)
	}
	return knownvalue.ListExact(checks)
}

// mountListEntry is the expected value of an element of mounts.
func mountListEntry(path, source, fstype string, readOnly bool, inFstab knownvalue.Check) knownvalue.Check {
	return knownvalue.ObjectPartial(map[string]knownvalue.Check{
		"path":      knownvalue.StringExact(path),
		"source":    knownvalue.StringExact(source),
		"fstype":    knownvalue.StringExact(fstype),
		"read_only": knownvalue.Bool(readOnly),
		"in_fstab":  inFstab,
	})
}

func TestMountsInRoot(t *testing.T) {
	dir := t.TempDir()
	base, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	entries := []mountEntry{
		{mountPoint: "/", fstype: "ext4"},
		{mountPoint: base, fstype: "overlay"},
		{mountPoint: base + "/proc", fstype: "proc"},
		{mountPoint: base + "2/proc", fstype: "proc"}, // A sibling of the root.
		{mountPoint: base + "/dev/pts", fstype: "devpts"},
		{mountPoint: "/proc", fstype: "proc"},
	}
	host, err := mountsInRoot(hostRoot, entries)
	if err != nil || len(host) != len(entries) {
		t.Fatalf("mountsInRoot(host) = %v, %v", host, err)
	}
	root, err := newFSRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := mountsInRoot(root, entries)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, e := range got {
		paths = append(paths, e.mountPoint+" "+e.fstype)
	}
	if want := []string{"/ overlay", "/proc proc", "/dev/pts devpts"}; !slices.Equal(paths, want) {
		t.Errorf("mountsInRoot = %q, want %q", paths, want)
	}
	if entries[1].mountPoint != base {
		t.Error("mountsInRoot modified its argument")
	}
}

func TestReadFstabInRoot(t *testing.T) {
	dir := t.TempDir()
	writeTestTree(t, dir, map[string]string{
		"etc/fstab.real": "tmpfs /tmp tmpfs defaults 0 0\n",
		"etc/fstab":      "->/etc/fstab.real", // Resolved inside the tree.
	})
	root, err := newFSRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	f, err := readFstabInRoot(root, defaultFstabPath)
	if err != nil {
		t.Fatal(err)
	}
	if e, n := lookupFstabEntry(f, "/tmp"); e == nil || n != 1 || e.fstype != "tmpfs" {
		t.Errorf("lookupFstabEntry = %v, %d", e, n)
	}

	empty, err := newFSRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if f, err := readFstabInRoot(empty, defaultFstabPath); err != nil || len(f.lines) != 0 {
		t.Errorf("missing fstab: %v, %v", f, err)
	}

	escape := t.TempDir()
	writeTestTree(t, escape, map[string]string{"etc/fstab": "->../../../../../../../../../etc/fstab"})
	if root, err = newFSRoot(escape); err != nil {
		t.Fatal(err)
	}
	if _, err := readFstabInRoot(root, defaultFstabPath); err == nil {
		t.Error("readFstabInRoot followed a symlink out of the tree")
	}
}

func mountDataHCL(body string) string {
	return fmt.Sprintf("data \"sysutils_mount\" \"test\" {\n%s\n}\n", body)
}

func TestMountDataSource_path(t *testing.T) {
	env := newMountTestEnv(t)
	other := filepath.Join(env.dir, "other")
	fstab := testFstabPrelude +
		"tmpfs " + env.mountPoint + "/ tmpfs size=1m,noatime 0 0\n" + // Trailing slash.
		"tmpfs " + env.mountPoint + " tmpfs defaults 0 0\n" + // mount(8) uses the first.
		"server:/export " + other + " nfs\n" // No options field.
	if err := os.WriteFile(env.fstab, []byte(fstab), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newFakeMounter()
	m.table = []mountEntry{
		{mountPoint: "/", root: "/", fstype: "ext4", source: "/dev/sda1", options: []string{"rw", "relatime"}, superOptions: []string{"rw"}},
		{mountPoint: env.mountPoint, root: "/", fstype: "tmpfs", source: "under", options: []string{"rw"}, superOptions: []string{"rw"}},
		// Stacked on top; the visible one.
		{mountPoint: env.mountPoint, root: "/", fstype: "tmpfs", source: "tmpfs", options: []string{"rw", "noatime"}, superOptions: []string{"ro", "size=1024k"}},
	}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: mountProviderFactories(env.fstab, m),
		Steps: []resource.TestStep{
			{
				Config: mountDataHCL(fmt.Sprintf("path = %q", env.mountPoint)),
				ConfigStateChecks: []statecheck.StateCheck{
					expectMountData("id", knownvalue.StringExact(env.mountPoint)),
					expectMountData("mounted", knownvalue.Bool(true)),
					expectMountData("source", knownvalue.StringExact("tmpfs")),
					expectMountData("fstype", knownvalue.StringExact("tmpfs")),
					expectMountData("options", stringsExact("rw", "noatime")),
					expectMountData("super_options", stringsExact("ro", "size=1024k")),
					expectMountData("read_only", knownvalue.Bool(true)),
					expectMountData("in_fstab", knownvalue.Bool(true)),
					expectMountData("fstab_device", knownvalue.StringExact("tmpfs")),
					expectMountData("fstab_fstype", knownvalue.StringExact("tmpfs")),
					expectMountData("fstab_options", stringsExact("size=1m", "noatime")),
					expectMountData("mounts", knownvalue.Null()),
				},
			},
			{
				// In fstab, not mounted.
				Config: mountDataHCL(fmt.Sprintf("path = %q", other)),
				ConfigStateChecks: []statecheck.StateCheck{
					expectMountData("mounted", knownvalue.Bool(false)),
					expectMountData("source", knownvalue.Null()),
					expectMountData("options", knownvalue.Null()),
					expectMountData("read_only", knownvalue.Null()),
					expectMountData("in_fstab", knownvalue.Bool(true)),
					expectMountData("fstab_device", knownvalue.StringExact("server:/export")),
					expectMountData("fstab_fstype", knownvalue.StringExact("nfs")),
					expectMountData("fstab_options", stringsExact("defaults")),
				},
			},
			{
				// Mounted, not in fstab; the root may be looked up.
				Config: mountDataHCL(`path = "/"`),
				ConfigStateChecks: []statecheck.StateCheck{
					expectMountData("mounted", knownvalue.Bool(true)),
					expectMountData("source", knownvalue.StringExact("/dev/sda1")),
					expectMountData("read_only", knownvalue.Bool(false)),
					// The prelude has "UUID=0a1b2c3d / ext4".
					expectMountData("in_fstab", knownvalue.Bool(true)),
					expectMountData("fstab_device", knownvalue.StringExact("UUID=0a1b2c3d")),
				},
			},
			{
				Config: mountDataHCL(fmt.Sprintf("path = %q\nfstab = false", filepath.Join(env.dir, "nothing"))),
				ConfigStateChecks: []statecheck.StateCheck{
					expectMountData("mounted", knownvalue.Bool(false)),
					expectMountData("in_fstab", knownvalue.Null()),
					expectMountData("fstab_options", knownvalue.Null()),
				},
			},
		},
	})
}

func TestMountDataSource_list(t *testing.T) {
	env := newMountTestEnv(t)
	if err := os.WriteFile(env.fstab, []byte(testFstabPrelude+"tmpfs "+env.mountPoint+"/ tmpfs defaults 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newFakeMounter()
	m.table = []mountEntry{
		{mountPoint: "/", fstype: "ext4", source: "/dev/sda1", options: []string{"rw"}},
		{mountPoint: "/proc", fstype: "proc", source: "proc", options: []string{"rw"}},
		{mountPoint: env.mountPoint, fstype: "tmpfs", source: "tmpfs", options: []string{"ro"}},
		{mountPoint: "/srv/nfs", fstype: "nfs4", source: "server:/export", options: []string{"rw"}},
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: mountProviderFactories(env.fstab, m),
		Steps: []resource.TestStep{
			{
				Config: mountDataHCL(""),
				ConfigStateChecks: []statecheck.StateCheck{
					expectMountData("id", knownvalue.StringExact("*")),
					expectMountData("mounted", knownvalue.Null()),
					expectMountData("in_fstab", knownvalue.Null()),
					expectMountData("mounts", knownvalue.ListExact([]knownvalue.Check{
						mountListEntry("/", "/dev/sda1", "ext4", false, knownvalue.Bool(true)),
						mountListEntry("/proc", "proc", "proc", false, knownvalue.Bool(false)),
						mountListEntry(env.mountPoint, "tmpfs", "tmpfs", true, knownvalue.Bool(true)),
						mountListEntry("/srv/nfs", "server:/export", "nfs4", false, knownvalue.Bool(false)),
					})),
				},
			},
			{
				Config: mountDataHCL(`fstypes = ["nfs", "nfs4", "tmpfs"]` + "\nfstab = false"),
				ConfigStateChecks: []statecheck.StateCheck{
					expectMountData("mounts", knownvalue.ListExact([]knownvalue.Check{
						mountListEntry(env.mountPoint, "tmpfs", "tmpfs", true, knownvalue.Null()),
						mountListEntry("/srv/nfs", "server:/export", "nfs4", false, knownvalue.Null()),
					})),
				},
			},
			{
				Config: mountDataHCL(`fstypes = ["xfs"]`),
				ConfigStateChecks: []statecheck.StateCheck{
					expectMountData("mounts", knownvalue.ListExact([]knownvalue.Check{})),
				},
			},
		},
	})
}

func TestMountDataSource_rootDir(t *testing.T) {
	dir := t.TempDir()
	base, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	writeTestTree(t, dir, map[string]string{
		"etc/fstab": "proc /proc proc defaults 0 0\nUUID=abc /data xfs noatime 0 2\n",
	})
	m := newFakeMounter()
	m.table = []mountEntry{
		{mountPoint: "/", fstype: "ext4", source: "/dev/sda1", options: []string{"rw"}},
		{mountPoint: "/proc", fstype: "proc", source: "proc", options: []string{"rw"}},
		{mountPoint: base + "/proc", fstype: "proc", source: "proc", options: []string{"rw"}},
		{mountPoint: base + "/data", fstype: "xfs", source: "/dev/sdb1", options: []string{"rw", "noatime"}},
	}
	provider := fmt.Sprintf("provider \"sysutils\" {\n  root_dir = %q\n}\n", dir)
	resource.UnitTest(t, resource.TestCase{
		// An empty fstab path selects the default, /etc/fstab below root_dir.
		ProtoV6ProviderFactories: mountProviderFactories("", m),
		Steps: []resource.TestStep{
			{
				Config: provider + mountDataHCL(""),
				ConfigStateChecks: []statecheck.StateCheck{
					expectMountData("mounts", knownvalue.ListExact([]knownvalue.Check{
						mountListEntry("/proc", "proc", "proc", false, knownvalue.Bool(true)),
						mountListEntry("/data", "/dev/sdb1", "xfs", false, knownvalue.Bool(true)),
					})),
				},
			},
			{
				Config: provider + mountDataHCL(`path = "/data"`),
				ConfigStateChecks: []statecheck.StateCheck{
					expectMountData("mounted", knownvalue.Bool(true)),
					expectMountData("source", knownvalue.StringExact("/dev/sdb1")),
					expectMountData("fstab_device", knownvalue.StringExact("UUID=abc")),
					expectMountData("fstab_options", stringsExact("noatime")),
				},
			},
			{
				// Mounted on the host, but not in the tree.
				Config: provider + mountDataHCL(`path = "/"`),
				ConfigStateChecks: []statecheck.StateCheck{
					expectMountData("mounted", knownvalue.Bool(false)),
					expectMountData("in_fstab", knownvalue.Bool(false)),
				},
			},
		},
	})
}

func TestMountDataSource_validation(t *testing.T) {
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      mountDataHCL(`path = "/srv"` + "\n" + `fstypes = ["ext4"]`),
				ExpectError: regexp.MustCompile(`(?s)Invalid\s+Attribute\s+Combination`),
			},
			{
				Config:      mountDataHCL(`path = "srv/data"`),
				ExpectError: regexp.MustCompile(`must\s+be\s+absolute`),
			},
			{
				Config:      mountDataHCL(`path = "/srv/data/"`),
				ExpectError: regexp.MustCompile(`canonical\s+form`),
			},
			{
				Config:      mountDataHCL(`fstypes = ["ext 4"]`),
				ExpectError: regexp.MustCompile(`invalid\s+character`),
			},
			{
				Config:      mountDataHCL(`fstypes = []`),
				ExpectError: regexp.MustCompile(`(?s)at\s+least\s+1`),
			},
		},
	})
}

// TestAccMountDataSource_realMountTable reads the real mount table, which
// needs no privileges: every Linux host and container has /proc mounted.
func TestAccMountDataSource_realMountTable(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: mountDataHCL(`path = "/proc"`),
				ConfigStateChecks: []statecheck.StateCheck{
					expectMountData("mounted", knownvalue.Bool(true)),
					expectMountData("fstype", knownvalue.StringExact("proc")),
				},
			},
			{
				Config: mountDataHCL(`fstypes = ["proc"]` + "\nfstab = false"),
				ConfigStateChecks: []statecheck.StateCheck{
					expectMountData("mounts", knownvalue.ListPartial(map[int]knownvalue.Check{
						0: knownvalue.ObjectPartial(map[string]knownvalue.Check{"fstype": knownvalue.StringExact("proc")}),
					})),
				},
			},
		},
	})
}

// TestAccMountDataSource_tmpfs mounts a real tmpfs with mount(8) and reads
// it back, with the fstab lookup in a temporary fstab. Requires root.
func TestAccMountDataSource_tmpfs(t *testing.T) {
	env := newMountAccEnv(t)
	if err := os.WriteFile(env.fstab, []byte(testFstabPrelude+"tfacc-ds "+env.mountPoint+" tmpfs size=2m,nodev 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runMountCommand(t, "mount", "-t", "tmpfs", "-o", "size=2m,nodev,ro", "--", "tfacc-ds", env.mountPoint)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: mountProviderFactories(env.fstab, nil),
		Steps: []resource.TestStep{
			{
				Config: mountDataHCL(fmt.Sprintf("path = %q", env.mountPoint)),
				ConfigStateChecks: []statecheck.StateCheck{
					expectMountData("mounted", knownvalue.Bool(true)),
					expectMountData("source", knownvalue.StringExact("tfacc-ds")),
					expectMountData("fstype", knownvalue.StringExact("tmpfs")),
					expectMountData("options", knownvalue.ListPartial(map[int]knownvalue.Check{0: knownvalue.StringExact("ro")})),
					expectMountData("read_only", knownvalue.Bool(true)),
					expectMountData("in_fstab", knownvalue.Bool(true)),
					expectMountData("fstab_options", stringsExact("size=2m", "nodev")),
				},
			},
			{
				Config: mountDataHCL(`fstypes = ["tmpfs"]`),
				Check: resource.TestCheckTypeSetElemNestedAttrs(testMountDataSource, "mounts.*", map[string]string{
					"path":      env.mountPoint,
					"source":    "tfacc-ds",
					"read_only": "true",
					"in_fstab":  "true",
				}),
			},
		},
	})
}
