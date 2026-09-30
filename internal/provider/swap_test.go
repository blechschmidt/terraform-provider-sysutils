package provider

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

func TestParseProcSwaps(t *testing.T) {
	data := "Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n" +
		"/swapfile                               file\t\t2097148\t\t1024\t\t-2\n" +
		"/dev/sda2                               partition\t1048572\t\t0\t\t10\n" +
		"/srv/my\\040swap                         file\t\t16380\t\t0\t\t-3\n" +
		"/old\\040(deleted)                       file\t\t4092\t\t0\t\t-4\n"
	got, err := parseProcSwaps(data)
	if err != nil {
		t.Fatal(err)
	}
	want := []swapEntry{
		{filename: "/swapfile", kind: "file", sizeKiB: 2097148, usedKiB: 1024, priority: -2},
		{filename: "/dev/sda2", kind: "partition", sizeKiB: 1048572, usedKiB: 0, priority: 10},
		{filename: "/srv/my swap", kind: "file", sizeKiB: 16380, priority: -3},
		{filename: "/old (deleted)", kind: "file", sizeKiB: 4092, priority: -4},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if e := findSwap(got, "/srv/my swap"); e == nil || e.sizeKiB != 16380 {
		t.Errorf("findSwap(/srv/my swap) = %+v", e)
	}
	// A file deleted while active never matches its old path.
	if e := findSwap(got, "/old"); e != nil {
		t.Errorf("findSwap(/old) = %+v, want nil", e)
	}
}

func TestParseProcSwaps_headerOnlyAndEmpty(t *testing.T) {
	for _, data := range []string{"", "Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n"} {
		got, err := parseProcSwaps(data)
		if err != nil || len(got) != 0 {
			t.Errorf("parseProcSwaps(%q) = %v, %v; want no entries", data, got, err)
		}
	}
}

func TestParseProcSwaps_malformed(t *testing.T) {
	for _, data := range []string{
		"Filename Type Size Used Priority\n/swapfile file 100 0\n",
		"Filename Type Size Used Priority\n/swapfile file big 0 -2\n",
		"Filename Type Size Used Priority\n/swapfile file 100 0 -2 extra\n",
		// A file name with an unescaped space would shift the fields.
		"Filename Type Size Used Priority\n/my swap file 100 0 -2\n",
	} {
		if got, err := parseProcSwaps(data); err == nil {
			t.Errorf("parseProcSwaps(%q) = %v, want an error", data, got)
		}
	}
}

// writeTestSwapHeader writes a version 1 swap header for the whole of the
// file at p, like mkswap(8), with a random UUID.
func writeTestSwapHeader(t testing.TB, p string) {
	t.Helper()
	if err := writeSwapHeaderLikeMkswap(p); err != nil {
		t.Fatal(err)
	}
}

func writeSwapHeaderLikeMkswap(p string) error {
	f, err := openNoFollow(p, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	ps := os.Getpagesize()
	pages := info.Size() / int64(ps)
	if pages < 10 {
		return errors.New("mkswap: swap area needs to be at least 40 KiB")
	}
	page := make([]byte, ps)
	binary.NativeEndian.PutUint32(page[1024:], 1)
	binary.NativeEndian.PutUint32(page[1028:], uint32(pages-1)) //nolint:gosec // Test sizes are small.
	if _, err := rand.Read(page[1036 : 1036+16]); err != nil {
		return err
	}
	copy(page[ps-len(swapHeaderMagic):], swapHeaderMagic)
	_, err = f.WriteAt(page, 0)
	return err
}

func TestReadSwapHeader(t *testing.T) {
	dir := t.TempDir()
	ps := int64(os.Getpagesize())

	p := filepath.Join(dir, "swap")
	if err := os.WriteFile(p, make([]byte, 16*ps), 0o600); err != nil {
		t.Fatal(err)
	}
	if h, err := readSwapHeader(p); err != nil || h != nil {
		t.Fatalf("zeroed file: readSwapHeader = %+v, %v; want nil, nil", h, err)
	}
	writeTestSwapHeader(t, p)
	h, err := readSwapHeader(p)
	if err != nil || h == nil {
		t.Fatalf("readSwapHeader = %+v, %v", h, err)
	}
	if h.bytes != 16*ps {
		t.Errorf("bytes = %d, want %d", h.bytes, 16*ps)
	}
	if len(h.uuid) != 36 || strings.Count(h.uuid, "-") != 4 {
		t.Errorf("uuid = %q", h.uuid)
	}

	// Files shorter than a page hold no swap area.
	short := filepath.Join(dir, "short")
	if err := os.WriteFile(short, []byte("SWAPSPACE2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if h, err := readSwapHeader(short); err != nil || h != nil {
		t.Errorf("short file: readSwapHeader = %+v, %v; want nil, nil", h, err)
	}

	// A symlink is not followed.
	link := filepath.Join(dir, "link")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readSwapHeader(link); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("symlink: err = %v, want a symlink refusal", err)
	}
}

func TestParseSwapHeader_invalid(t *testing.T) {
	ps := 4096
	valid := func() []byte {
		page := make([]byte, ps)
		binary.NativeEndian.PutUint32(page[1024:], 1)
		binary.NativeEndian.PutUint32(page[1028:], 255)
		copy(page[ps-10:], swapHeaderMagic)
		return page
	}
	if h := parseSwapHeader(valid()); h == nil || h.bytes != 256*int64(ps) || h.uuid != "" {
		t.Fatalf("valid header: %+v", h)
	}
	for name, mutate := range map[string]func([]byte){
		"legacy magic": func(b []byte) { copy(b[ps-10:], swapHeaderLegacyMagic) },
		"no magic":     func(b []byte) { copy(b[ps-10:], "0000000000") },
		"version 2":    func(b []byte) { binary.NativeEndian.PutUint32(b[1024:], 2) },
		"no pages":     func(b []byte) { binary.NativeEndian.PutUint32(b[1028:], 0) },
	} {
		page := valid()
		mutate(page)
		if h := parseSwapHeader(page); h != nil {
			t.Errorf("%s: parseSwapHeader = %+v, want nil", name, h)
		}
	}
}

func TestProbeSignatures(t *testing.T) {
	at := func(offset int, magic string) []byte {
		b := make([]byte, swapProbeSize)
		copy(b[offset:], magic)
		return b
	}
	for _, tc := range []struct {
		name string
		data []byte
		want string
	}{
		{"blank", make([]byte, swapProbeSize), ""},
		{"short blank", make([]byte, 100), ""},
		{"swap", at(4096-10, "SWAPSPACE2"), "swap"},
		{"swap 64k pages", at(65536-10, "SWAPSPACE2"), "swap"},
		{"ext4", at(1080, "\x53\xef"), "ext2/ext3/ext4"},
		{"xfs", at(0, "XFSB"), "xfs"},
		{"btrfs", at(65600, "_BHRfS_M"), "btrfs"},
		{"luks", at(0, "LUKS\xba\xbe"), "LUKS"},
		{"lvm", at(512, "LABELONE"), "LVM2 physical volume"},
		{"gpt", at(512, "EFI PART"), "GPT partition table"},
		{"mbr", at(510, "\x55\xaa"), "DOS boot sector or partition table"},
		// A magic cut off by the end of the data does not count.
		{"truncated", at(1080, "\x53\xef")[:1081], ""},
	} {
		if got := probeSignatures(tc.data); got != tc.want {
			t.Errorf("%s: probeSignatures = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestParseBlkidExport(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"DEVNAME=/dev/sdb1\nUUID=abc\nTYPE=ext4\nUSAGE=filesystem\n", "ext4"},
		{"DEVNAME=/dev/sdb\nPTUUID=1234\nPTTYPE=gpt\n", "gpt partition table"},
		{"DEVNAME=/dev/sdb\nPTTYPE=dos\nTYPE=vfat\n", "vfat"},
		{"", ""},
	} {
		if got := parseBlkidExport(tc.in); got != tc.want {
			t.Errorf("parseBlkidExport(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSwapFstabEntries(t *testing.T) {
	five := int64(5)
	tf := parseTextFile([]byte(testFstabPrelude + "/var/swap none swap sw,pri=1 0 0\n/var/swap none swap sw 0 0\n/mnt/swap /var/swap ext4 defaults 0 2\n"))

	e, n := lookupSwapFstabEntry(tf, "/var/swap")
	if e == nil || n != 2 {
		t.Fatalf("lookupSwapFstabEntry = %v, %d; want the first of 2", e, n)
	}
	if pri := fstabSwapPriority(e.options); pri == nil || *pri != 1 {
		t.Errorf("fstabSwapPriority(%v) = %v, want 1", e.options, pri)
	}
	if e, _ := lookupSwapFstabEntry(tf, "/mnt/swap"); e != nil {
		t.Errorf("an ext4 entry matched as swap: %v", e)
	}

	if !setSwapFstabEntry(tf, swapFstabEntry("/var/swap", &five)) {
		t.Fatal("setSwapFstabEntry reported no change")
	}
	want := testFstabPrelude + "/var/swap none swap sw,pri=5 0 0\n/mnt/swap /var/swap ext4 defaults 0 2\n"
	if got := string(tf.bytes()); got != want {
		t.Fatalf("after set:\n%s\nwant\n%s", got, want)
	}
	if setSwapFstabEntry(tf, swapFstabEntry("/var/swap", &five)) {
		t.Error("setting the same entry again reported a change")
	}

	if !setSwapFstabEntry(tf, swapFstabEntry("/dev/sdb2", nil)) {
		t.Fatal("adding an entry reported no change")
	}
	want += "/dev/sdb2 none swap sw 0 0\n"
	if got := string(tf.bytes()); got != want {
		t.Fatalf("after add:\n%s\nwant\n%s", got, want)
	}

	if !removeSwapFstabEntries(tf, "/var/swap") {
		t.Fatal("removeSwapFstabEntries reported no change")
	}
	want = testFstabPrelude + "/mnt/swap /var/swap ext4 defaults 0 2\n/dev/sdb2 none swap sw 0 0\n"
	if got := string(tf.bytes()); got != want {
		t.Fatalf("after remove:\n%s\nwant\n%s", got, want)
	}
	if removeSwapFstabEntries(tf, "/var/swap") {
		t.Error("removing a missing entry reported a change")
	}
}

func TestSwapFstabEntry_escapesPath(t *testing.T) {
	e := swapFstabEntry("/srv/my swap", nil)
	if got, want := e.String(), `/srv/my\040swap none swap sw 0 0`; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	tf := parseTextFile([]byte(e.String() + "\n"))
	if got, _ := lookupSwapFstabEntry(tf, "/srv/my swap"); got == nil {
		t.Error("escaped entry not found")
	}
}

func TestFstabSwapPriority(t *testing.T) {
	for _, tc := range []struct {
		opts []string
		want *int64
	}{
		{[]string{"sw"}, nil},
		{[]string{"defaults", "pri=10"}, ptr(int64(10))},
		{[]string{"pri=1", "pri=3"}, ptr(int64(3))},
		{[]string{"pri=high"}, nil},
	} {
		if got := fstabSwapPriority(tc.opts); !equalInt64Ptr(got, tc.want) {
			t.Errorf("fstabSwapPriority(%v) = %v, want %v", tc.opts, got, tc.want)
		}
	}
}

func ptr[T any](v T) *T { return &v }

func TestReportedSizeMiB(t *testing.T) {
	for _, tc := range []struct {
		bytes int64
		want  *int64
		out   int64
	}{
		{0, ptr(int64(16)), 0},
		{16 * mib, ptr(int64(16)), 16},
		{16 * mib, nil, 16},
		{16*mib + 1, ptr(int64(16)), 17},
		{16*mib - 1, ptr(int64(16)), 15},
		{16*mib - 1, nil, 16},
		{mib / 2, ptr(int64(1)), 0},
	} {
		if got := reportedSizeMiB(tc.bytes, tc.want); got != tc.out {
			t.Errorf("reportedSizeMiB(%d, %v) = %d, want %d", tc.bytes, tc.want, got, tc.out)
		}
	}
}

func TestIsSwapDevice(t *testing.T) {
	for p, want := range map[string]bool{
		"/dev/sdb2":                true,
		"/dev/disk/by-uuid/abc":    true,
		"/swapfile":                false,
		"/devices/swap":            false,
		"/var/lib/swap/dev/swapfi": false,
	} {
		if got := isSwapDevice(p); got != want {
			t.Errorf("isSwapDevice(%q) = %v, want %v", p, got, want)
		}
	}
}

// headerOnlyManager formats with writeSwapHeaderLikeMkswap and records the
// paths it was asked to format.
type headerOnlyManager struct {
	fakeSwapManager
	formatted []string
}

func (m *headerOnlyManager) mkswap(_ context.Context, p string, _ bool) error {
	m.formatted = append(m.formatted, p)
	return writeSwapHeaderLikeMkswap(p)
}

func TestAllocateSwapFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "swap")
	m := &headerOnlyManager{}
	if err := allocateSwapFile(context.Background(), m, p, 2*mib, false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode() != swapFileMode {
		t.Errorf("mode = %v, want %v", info.Mode(), swapFileMode)
	}
	if info.Size() != 2*mib {
		t.Errorf("size = %d, want %d", info.Size(), 2*mib)
	}
	if st := info.Sys().(*syscall.Stat_t); os.Geteuid() == 0 && (st.Uid != 0 || st.Gid != 0) {
		t.Errorf("owner = %d:%d, want 0:0", st.Uid, st.Gid)
	}
	// Allocated, not sparse: swap files must not have holes.
	if st := info.Sys().(*syscall.Stat_t); st.Blocks*512 < 2*mib {
		t.Errorf("only %d bytes allocated for a 2 MiB swap file", st.Blocks*512)
	}
	h, err := readSwapHeader(p)
	if err != nil || h == nil || h.bytes != 2*mib {
		t.Fatalf("readSwapHeader = %+v, %v", h, err)
	}
	// mkswap ran on the temporary file, before it was renamed into place.
	if len(m.formatted) != 1 || m.formatted[0] == p || filepath.Dir(m.formatted[0]) != dir {
		t.Errorf("mkswap ran on %v", m.formatted)
	}

	// Without replace, an existing file is never overwritten.
	if err := allocateSwapFile(context.Background(), m, p, 4*mib, false); !errors.Is(err, errFileChangedConcurrently) {
		t.Errorf("second allocation without replace: err = %v", err)
	}
	if err := allocateSwapFile(context.Background(), m, p, 4*mib, true); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Lstat(p); info.Size() != 4*mib {
		t.Errorf("size after replace = %d", info.Size())
	}
	// No temporary files are left behind.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want only the swap file", len(entries))
	}
}

func TestAllocateSwapFile_mkswapFailureCleansUp(t *testing.T) {
	dir := t.TempDir()
	m := &fakeSwapManager{failMkswap: true}
	if err := allocateSwapFile(context.Background(), m, filepath.Join(dir, "swap"), mib, false); err == nil {
		t.Fatal("allocateSwapFile succeeded although mkswap failed")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("left behind %d entries", len(entries))
	}
}

func TestCheckSwapFileDir(t *testing.T) {
	dir := t.TempDir()
	if err := checkSwapFileDir(dir); err != nil {
		t.Errorf("private directory: %v", err)
	}
	open := filepath.Join(dir, "open")
	if err := os.Mkdir(open, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := checkSwapFileDir(open); err == nil || !strings.Contains(err.Error(), "writable by other users") {
		t.Errorf("world-writable directory: err = %v", err)
	}
	// Sticky directories such as /tmp are fine: others cannot replace
	// entries they do not own.
	if err := os.Chmod(open, 0o777|fs.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if err := checkSwapFileDir(open); err != nil {
		t.Errorf("sticky directory: %v", err)
	}
	if err := checkSwapFileDir(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing directory accepted")
	}
	if os.Geteuid() == 0 {
		other := filepath.Join(dir, "other")
		if err := os.Mkdir(other, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(other, 65534, 65534); err != nil {
			t.Fatal(err)
		}
		if err := checkSwapFileDir(other); err == nil || !strings.Contains(err.Error(), "belongs to user") {
			t.Errorf("directory owned by another user: err = %v", err)
		}
	}
}
