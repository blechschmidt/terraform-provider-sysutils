package provider

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"golang.org/x/sys/unix"
)

const testFileAttrsResource = "sysutils_file_attributes.test"

// testFileAttrDir returns a directory on a file system with inode flags:
// as root, a freshly loop-mounted ext4 image, and otherwise (or if that
// fails, as in a container without loop devices) a temporary directory. The
// test is skipped if the file system of the latter has no inode flags, as
// with overlayfs on some hosts. Immutable and append-only flags left behind
// are cleared at the end, so the directory can be removed.
func testFileAttrDir(t *testing.T) string {
	t.Helper()
	var dir string
	if os.Geteuid() == 0 {
		dir = mountLoopExt4(t)
	}
	if dir == "" {
		dir = testRootDir(t)
	}
	requireFileAttrs(t, dir)
	t.Cleanup(func() { clearImmutableTree(t, dir) })
	return dir
}

// mountLoopExt4 mounts a new ext4 image and returns the mount point, or ""
// if that isn't possible here.
func mountLoopExt4(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Logf("mkfs.ext4 is not installed; not using an ext4 image")
		return ""
	}
	img := filepath.Join(t.TempDir(), "attrs.img")
	if err := os.WriteFile(img, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(img, 32*mib); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("mkfs.ext4", "-q", "-F", img).CombinedOutput(); err != nil {
		t.Logf("mkfs.ext4: %v: %s; not using an ext4 image", err, out)
		return ""
	}
	mnt := testRootDir(t)
	if out, err := exec.Command("mount", "-t", "ext4", "-o", "loop", img, mnt).CombinedOutput(); err != nil {
		t.Logf("cannot mount an ext4 image on a loop device: %v: %s; testing in %s", err, out, mnt)
		return ""
	}
	t.Logf("testing on an ext4 image mounted at %s", mnt)
	t.Cleanup(func() {
		if out, err := exec.Command("umount", mnt).CombinedOutput(); err != nil {
			t.Errorf("unmounting %s: %v: %s", mnt, err, out)
		}
	})
	return mnt
}

// requireFileAttrs skips the test if the file system of dir doesn't
// support inode flags (the no-dump flag, which needs no privileges).
func requireFileAttrs(t *testing.T, dir string) {
	t.Helper()
	probe := filepath.Join(dir, ".attrs-probe")
	mustWrite(t, probe, "")
	defer func() { _ = os.Remove(probe) }()
	f, err := os.Open(probe)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	flags, err := getFileAttrs(f)
	if err == nil {
		err = setFileAttrs(f, flags|fsNodumpFL)
	}
	if errors.Is(err, errFileAttrsNotSupported) {
		t.Skipf("the file system of %s does not support inode flags: %v", dir, err)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// clearImmutableTree clears the immutable and append-only flags of
// everything below dir.
func clearImmutableTree(t *testing.T, dir string) {
	t.Helper()
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || (!d.IsDir() && !d.Type().IsRegular()) {
			return nil
		}
		flags, err := fileAttrsOf(p)
		if err != nil || flags&(fsImmutableFL|fsAppendFL) == 0 {
			return nil
		}
		f, err := openNoFollow(p, os.O_RDONLY, 0)
		if err != nil {
			return nil
		}
		defer func() { _ = f.Close() }()
		if err := setFileAttrs(f, flags&^(fsImmutableFL|fsAppendFL)); err != nil {
			t.Errorf("clearing flags of %s: %v", p, err)
		}
		return nil
	})
}

func mustFileAttrs(t *testing.T, p string) uint32 {
	t.Helper()
	flags, err := fileAttrsOf(p)
	if err != nil {
		t.Fatal(err)
	}
	return flags
}

func mustSetFileAttrs(t *testing.T, p string, flags uint32) {
	t.Helper()
	f, err := openNoFollow(p, os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := setFileAttrs(f, flags); err != nil {
		t.Fatal(err)
	}
}

// checkFileAttrs checks the managed flags of p against want, given as
// chattr letters.
func checkFileAttrs(p, want string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		flags, err := fileAttrsOf(p)
		if err != nil {
			return err
		}
		if got := strings.Join(formatFileAttrs(flags), ""); got != want {
			return fmt.Errorf("flags of %s = %q, want %q", p, got, want)
		}
		return nil
	}
}

// capLinuxImmutable is CAP_LINUX_IMMUTABLE from linux/capability.h.
const capLinuxImmutable = 9

// requireImmutableCapability skips the test unless it runs as root with
// CAP_LINUX_IMMUTABLE, which setting and clearing the i and a flags needs.
// Docker leaves it out by default; testacc-docker and testacc-distros add it.
func requireImmutableCapability(t *testing.T) {
	t.Helper()
	requireRoot(t)
	if !hasEffectiveCapability(t, capLinuxImmutable) {
		t.Skip("CAP_LINUX_IMMUTABLE is not in the effective capability set; run docker with --cap-add LINUX_IMMUTABLE")
	}
}

func TestParseFileAttr(t *testing.T) {
	for _, f := range fileAttrFlags {
		got, err := parseFileAttr(f.letter)
		if err != nil || got != f.flag {
			t.Errorf("parseFileAttr(%q) = %#x, %v", f.letter, got, err)
		}
	}
	for _, s := range []string{"", "e", "E", "F", "ia", "I", "+i", "é"} {
		if _, err := parseFileAttr(s); err == nil {
			t.Errorf("parseFileAttr(%q) succeeded", s)
		}
	}
	if got := formatFileAttrs(fsImmutableFL | fsNodumpFL | fsNoatimeFL | 0x80000); strings.Join(got, "") != "idA" {
		t.Errorf("formatFileAttrs = %v", got)
	}
}

func TestNextFileAttrs(t *testing.T) {
	const extent = 0x80000 // e, never managed
	u32 := func(v uint32) *uint32 { return &v }
	tests := []struct {
		name      string
		cur       uint32
		prev      *fileAttrsSpec
		spec      fileAttrsSpec
		original  *uint32
		want      uint32
		wantState uint32
	}{
		{
			name: "create adds listed flags, keeps others",
			cur:  extent | fsNoatimeFL, spec: fileAttrsSpec{want: fsImmutableFL},
			want: extent | fsNoatimeFL | fsImmutableFL, wantState: fsImmutableFL,
		},
		{
			name: "exclusive clears unlisted supported flags only",
			cur:  extent | fsNoatimeFL | fsSyncFL, spec: fileAttrsSpec{want: fsNodumpFL, exclusive: true},
			want: extent | fsNodumpFL, wantState: fsNodumpFL,
		},
		{
			name: "flag removed from the list is restored",
			cur:  fsImmutableFL | fsNodumpFL,
			prev: &fileAttrsSpec{want: fsImmutableFL | fsNodumpFL}, spec: fileAttrsSpec{want: fsImmutableFL},
			original: u32(fsNodumpFL),
			want:     fsImmutableFL | fsNodumpFL, wantState: fsImmutableFL,
		},
		{
			name: "flag removed from the list without original is cleared",
			cur:  fsImmutableFL | fsNodumpFL,
			prev: &fileAttrsSpec{want: fsImmutableFL | fsNodumpFL}, spec: fileAttrsSpec{want: fsImmutableFL},
			want: fsImmutableFL, wantState: fsImmutableFL,
		},
		{
			name: "leaving exclusive restores the other flags",
			cur:  extent | fsNoatimeFL,
			prev: &fileAttrsSpec{want: fsNoatimeFL, exclusive: true}, spec: fileAttrsSpec{want: fsNoatimeFL},
			original: u32(extent | fsSyncFL | fsImmutableFL),
			want:     extent | fsNoatimeFL | fsSyncFL | fsImmutableFL, wantState: fsNoatimeFL,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nextFileAttrs(tt.cur, tt.prev, tt.spec, tt.original)
			if got != tt.want {
				t.Errorf("nextFileAttrs = %#x, want %#x", got, tt.want)
			}
			if s := observedFileAttrs(got, tt.spec); s != tt.wantState {
				t.Errorf("observedFileAttrs = %#x, want %#x", s, tt.wantState)
			}
		})
	}
}

// procfs has no inode flags; the ioctl fails with ENOTTY.
func TestFileAttrsNotSupported(t *testing.T) {
	_, err := fileAttrsOf("/proc/version")
	if err == nil {
		t.Skip("/proc/version has inode flags here")
	}
	if !errors.Is(err, errFileAttrsNotSupported) || !errors.Is(err, unix.ENOTTY) {
		t.Fatalf("error %v does not match errFileAttrsNotSupported and ENOTTY", err)
	}
	if !strings.Contains(err.Error(), "does not support these inode flags") {
		t.Errorf("unexplained error: %v", err)
	}
}

func TestExplainImmutable(t *testing.T) {
	dir := testFileAttrDir(t)
	plain := filepath.Join(dir, "plain")
	mustWrite(t, plain, "")
	perm := &fs.PathError{Op: "open", Path: plain, Err: syscall.EPERM}
	if got := explainImmutable(perm, dir); got != perm {
		t.Errorf("EPERM without flags changed: %v", got)
	}
	other := &fs.PathError{Op: "open", Path: plain, Err: syscall.EIO}
	if got := explainImmutable(other); got != other {
		t.Errorf("EIO changed: %v", got)
	}
	if explainImmutable(nil, plain) != nil {
		t.Error("nil changed")
	}
}

// Writes to an immutable file and removals from an append-only directory
// fail with an error that names the flag, through every shared helper.
func TestImmutableFileErrors(t *testing.T) {
	requireImmutableCapability(t)
	dir := testFileAttrDir(t)
	p := filepath.Join(dir, "locked.conf")
	mustWrite(t, p, "old\n")
	mustSetFileAttrs(t, p, mustFileAttrs(t, p)|fsImmutableFL)
	immutable := regexp.MustCompile(`the file ".*locked.conf" has the immutable attribute \(i, see lsattr\) set.*chattr -i`)

	_, snap, err := readRegularFileNoFollow(p, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"replaceFileAtomic": replaceFileAtomic(p, []byte("new\n"), snap, 0o644),
		"writeFile": func() error {
			_, err := writeFile(p, strings.NewReader("new\n"), 0o644, "", "")
			return err
		}(),
		"removeManagedFile": removeManagedFile(p, snap),
		"writeAccessACL": func() error {
			f, err := openACLTarget(p)
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()
			return f.writeAccessACL(posixACL{
				{Tag: aclUserObj, Perm: 6}, {Tag: aclUser, ID: 65534, Perm: 4},
				{Tag: aclGroupObj, Perm: 4}, {Tag: aclMask, Perm: 4}, {Tag: aclOther, Perm: 4},
			})
		}(),
		"setOwnershipAndMode": func() error {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()
			return setOwnershipAndMode(f, "", "", 0o600)
		}(),
	} {
		if err == nil || !immutable.MatchString(err.Error()) || !errors.Is(err, syscall.EPERM) {
			t.Errorf("%s: error %v does not explain the immutable flag", name, err)
		}
	}
	if got, err := os.ReadFile(p); err != nil || string(got) != "old\n" {
		t.Errorf("content changed to %q (%v)", got, err)
	}

	// In an append-only directory, files can be created but not renamed
	// over or removed.
	d := filepath.Join(dir, "spool")
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	q := filepath.Join(d, "entry")
	mustWrite(t, q, "x\n")
	mustSetFileAttrs(t, d, mustFileAttrs(t, d)|fsAppendFL)
	appendOnly := fmt.Sprintf("the directory %q has the append-only attribute (a, see lsattr) set", d)
	if err := writeManagedFile(filepath.Join(d, "new"), []byte("x\n"), nil, 0o644); err == nil || !strings.Contains(err.Error(), appendOnly) {
		t.Errorf("renaming into an append-only directory: %v", err)
	}
	_, snap, err = readRegularFileNoFollow(q, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	err = removeManagedFile(q, snap)
	if err == nil || !strings.Contains(err.Error(), appendOnly) {
		t.Errorf("removing from an append-only directory: %v", err)
	}
	err = removeAllNoFollow(d)
	if err == nil || !strings.Contains(err.Error(), "append-only attribute") {
		t.Errorf("removing an append-only directory tree: %v", err)
	}
}

func TestAccFileAttributes_file(t *testing.T) {
	requireImmutableCapability(t)
	dir := testFileAttrDir(t)
	p := filepath.Join(dir, "data.txt")
	mustWrite(t, p, "data\n")
	// Set before the resource exists, so restored by destroy.
	mustSetFileAttrs(t, p, mustFileAttrs(t, p)|fsNodumpFL)
	cfg := func(attrs string, exclusive bool) string {
		return fmt.Sprintf(`
resource "sysutils_file_attributes" "test" {
  path       = %q
  attributes = %s
  exclusive  = %v
}`, p, attrs, exclusive)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: cfg(`["i", "A"]`, false),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testFileAttrsResource, "id", p),
					resource.TestCheckResourceAttr(testFileAttrsResource, "attributes.#", "2"),
					resource.TestCheckResourceAttr(testFileAttrsResource, "exclusive", "false"),
					checkFileAttrs(p, "idA"),
				),
			},
			{
				// Exclusive: the immutable flag goes, no dump is now listed.
				Config: cfg(`["A", "d"]`, true),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(testFileAttrsResource, plancheck.ResourceActionUpdate),
				}},
				Check: checkFileAttrs(p, "dA"),
			},
			{
				ResourceName:            testFileAttrsResource,
				ImportState:             true,
				ImportStateId:           p,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"exclusive"},
			},
			{
				// Drift: a flag set outside Terraform is cleared.
				PreConfig: func() { mustSetFileAttrs(t, p, mustFileAttrs(t, p)|fsSyncFL) },
				Config:    cfg(`["A", "d"]`, true),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(testFileAttrsResource, plancheck.ResourceActionUpdate),
				}},
				Check: checkFileAttrs(p, "dA"),
			},
			{
				// Drift: a listed flag cleared outside Terraform is set.
				PreConfig: func() { mustSetFileAttrs(t, p, mustFileAttrs(t, p)&^fsNoatimeFL) },
				Config:    cfg(`["A", "d"]`, true),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(testFileAttrsResource, plancheck.ResourceActionUpdate),
				}},
				Check: checkFileAttrs(p, "dA"),
			},
			{
				// No longer exclusive and no longer listing d: the flags it
				// no longer manages get their original values, so d stays.
				Config: cfg(`["A"]`, false),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testFileAttrsResource, "attributes.#", "1"),
					checkFileAttrs(p, "dA"),
				),
			},
		},
		// Destroy restores A to what it was before: unset.
		CheckDestroy: checkFileAttrs(p, "d"),
	})
}

// Needs no root: the no dump and no atime flags only need the file's owner.
func TestAccFileAttributes_directory(t *testing.T) {
	dir := testFileAttrDir(t)
	d := filepath.Join(dir, "backup-excluded")
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	mustSetFileAttrs(t, d, mustFileAttrs(t, d)|fsNoatimeFL)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "sysutils_file_attributes" "test" {
  path       = %q
  attributes = ["d"]
}`, d),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testFileAttrsResource, "attributes.#", "1"),
					resource.TestCheckTypeSetElemAttr(testFileAttrsResource, "attributes.*", "d"),
					// Not exclusive: A is left alone.
					checkFileAttrs(d, "dA"),
				),
			},
			{
				// Flags set by others don't show up without exclusive.
				PreConfig: func() { mustSetFileAttrs(t, d, mustFileAttrs(t, d)|fsSyncFL) },
				Config: fmt.Sprintf(`
resource "sysutils_file_attributes" "test" {
  path       = %q
  attributes = ["d"]
}`, d),
				PlanOnly: true,
			},
		},
		CheckDestroy: checkFileAttrs(d, "SA"),
	})
}

// sysutils_file can't change an immutable file and says why.
func TestAccFileAttributes_immutableFile(t *testing.T) {
	requireImmutableCapability(t)
	dir := testFileAttrDir(t)
	p := filepath.Join(dir, "resolv.conf")
	cfg := func(content string) string {
		return fmt.Sprintf(`
resource "sysutils_file" "f" {
  path    = %q
  content = %q
}

resource "sysutils_file_attributes" "test" {
  path       = sysutils_file.f.path
  attributes = ["i"]
}`, p, content)
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: cfg("nameserver 10.0.0.1\n"),
				Check:  checkFileAttrs(p, "i"),
			},
			{
				Config:      cfg("nameserver 10.0.0.2\n"),
				ExpectError: regexp.MustCompile(`has\s+the\s+immutable\s+attribute\s+\(i,\s+see\s+lsattr\)\s+set`),
			},
		},
		// The flag is cleared first, so the file can be removed.
		CheckDestroy: checkPathGone(p),
	})
}

func TestAccFileAttributes_rootDir(t *testing.T) {
	root := testFileAttrDir(t)
	p := filepath.Join(root, "srv", "data")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, p, "")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: rootedProvider(root) + `
resource "sysutils_file_attributes" "test" {
  path       = "/srv/data"
  attributes = ["d"]
}`,
			Check: checkFileAttrs(p, "d"),
		}},
		CheckDestroy: checkFileAttrs(p, ""),
	})
}

func TestAccFileAttributes_errors(t *testing.T) {
	dir := testFileAttrDir(t)
	file := filepath.Join(dir, "file")
	mustWrite(t, file, "")
	link := filepath.Join(dir, "link")
	mustSymlink(t, file, link)
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := func(p, attrs string) string {
		return fmt.Sprintf(`
resource "sysutils_file_attributes" "test" {
  path       = %q
  attributes = %s
}`, p, attrs)
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      cfg(link, `["d"]`),
				ExpectError: regexp.MustCompile(`is\s+a\s+symbolic\s+link;\s+refusing\s+to\s+follow\s+it`),
			},
			{
				Config:      cfg(fifo, `["d"]`),
				ExpectError: regexp.MustCompile(`neither\s+a\s+regular\s+file\s+nor\s+a\s+directory`),
			},
			{
				Config:      cfg(filepath.Join(dir, "missing"), `["d"]`),
				ExpectError: regexp.MustCompile(`no\s+such\s+file\s+or\s+directory`),
			},
			{
				Config:      cfg(file, `["e"]`),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`"e"\s+is\s+not\s+a\s+supported\s+flag`),
			},
			{
				Config:      cfg(file, `["id"]`),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`"id"\s+is\s+not\s+a\s+single\s+flag\s+letter`),
			},
			{
				Config:      cfg("relative/path", `["d"]`),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`must\s+be\s+absolute`),
			},
		},
	})
	if err := checkFileAttrs(file, "")(nil); err != nil {
		t.Error(err)
	}
}

// tmpfs supports only a few flags; the others are refused with EOPNOTSUPP,
// which the diagnostic explains.
func TestAccFileAttributes_unsupportedFlag(t *testing.T) {
	requireRoot(t)
	mnt := testRootDir(t)
	if out, err := exec.Command("mount", "-t", "tmpfs", "-o", "size=1m", "tmpfs", mnt).CombinedOutput(); err != nil {
		t.Skipf("cannot mount tmpfs: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("umount", mnt).Run() })
	p := filepath.Join(mnt, "f")
	mustWrite(t, p, "")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: fmt.Sprintf(`
resource "sysutils_file_attributes" "test" {
  path       = %q
  attributes = ["S"]
}`, p),
			ExpectError: regexp.MustCompile(`does\s+not\s+support\s+these\s+inode\s+flags|did\s+not\s+apply\s+the\s+flags\s+S`),
		}},
	})
}
