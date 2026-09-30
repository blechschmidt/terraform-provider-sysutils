package provider

// Swap areas behind sysutils_swap: parsing /proc/swaps, reading swap
// headers, probing for foreign signatures, allocating swap files and the
// fstab entries of swap areas. Parsing and editing are pure functions;
// mkswap, swapon and swapoff go through the swapManager interface so that
// unit tests can substitute a fake.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// defaultProcSwapsPath lists the active swap areas.
	defaultProcSwapsPath = "/proc/swaps"
	// swapCommandTimeout bounds mkswap, swapon and swapoff. swapoff has to
	// read every swapped-out page back into memory, which can take long on
	// a busy host with a large swap area.
	swapCommandTimeout = 30 * time.Minute
	// swapOutputLimit caps how much command output is kept per stream.
	swapOutputLimit = 64 << 10
	// maxProcSwapsSize bounds how much of /proc/swaps is read.
	maxProcSwapsSize = 1 << 20
	// swapFileMode is the mode of swap files; swapon(8) warns about any
	// other, since whoever can read the file can read swapped-out memory.
	swapFileMode fs.FileMode = 0o600
	// swapFstabType is the file system type of swap entries in fstab.
	swapFstabType = "swap"
	// swapFstabMountPoint is the mount point field of swap entries.
	swapFstabMountPoint = "none"
	// swapPriorityOption is the prefix of the fstab option that sets the
	// priority, as in "pri=10".
	swapPriorityOption = "pri="
	// maxSwapPriority is the highest priority swapon(8) accepts.
	maxSwapPriority = 32767
	// swapHeaderMagic ends the first page of a (version 1) swap area.
	swapHeaderMagic = "SWAPSPACE2"
	// swapHeaderLegacyMagic ends the first page of a version 0 swap area,
	// which current kernels no longer support.
	swapHeaderLegacyMagic = "SWAP-SPACE"
	// zeroFillChunk is the buffer size used when fallocate is unsupported.
	zeroFillChunk = 1 << 20
	// swapProbeSize is how much of a file or device is read to look for
	// file system signatures; the btrfs superblock ends below 68 KiB.
	swapProbeSize = 68 << 10
	// fsNoCOWFlag is FS_NOCOW_FL from <linux/fs.h>, the chattr +C flag.
	fsNoCOWFlag = 0x00800000
)

// swapEntry is one line of /proc/swaps.
type swapEntry struct {
	// filename is the resolved path of the swap file or device. A file
	// that was deleted while active ends in " (deleted)".
	filename string
	// kind is "file" or "partition".
	kind string
	// sizeKiB and usedKiB count the usable pages, which excludes the
	// header page.
	sizeKiB, usedKiB int64
	// priority is negative if it was assigned by the kernel.
	priority int64
}

// parseProcSwaps parses /proc/swaps:
//
//	Filename				Type		Size		Used		Priority
//	/swapfile                               file		2097148		0		-2
//
// White space, tabs, newlines and backslashes in file names are octal
// escapes such as \040.
func parseProcSwaps(data string) ([]swapEntry, error) {
	var entries []swapEntry
	for n, line := range strings.Split(data, "\n") {
		if strings.TrimSpace(line) == "" || n == 0 && strings.HasPrefix(line, "Filename") {
			continue
		}
		fields := asciiFields(line)
		if len(fields) != 5 {
			return nil, fmt.Errorf("malformed /proc/swaps line %d: %q", n+1, line)
		}
		var nums [3]int64
		for i, f := range fields[2:] {
			v, err := strconv.ParseInt(f, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("malformed number on /proc/swaps line %d: %q", n+1, f)
			}
			nums[i] = v
		}
		entries = append(entries, swapEntry{
			filename: unescapeOctal(fields[0]),
			kind:     fields[1],
			sizeKiB:  nums[0],
			usedKiB:  nums[1],
			priority: nums[2],
		})
	}
	return entries, nil
}

// findSwap returns the entry for the swap area at the resolved path
// filename, or nil.
func findSwap(entries []swapEntry, filename string) *swapEntry {
	for i := range entries {
		if entries[i].filename == filename {
			return &entries[i]
		}
	}
	return nil
}

// swapHeader is the information in the first page of a swap area written
// by mkswap(8).
type swapHeader struct {
	// bytes is the size of the swap area, including the header page.
	bytes int64
	uuid  string
}

// parseSwapHeader parses the first page of a swap area. The layout is the
// kernel's union swap_header: 1024 boot bytes, then version, last_page and
// nr_badpages (native-endian 32-bit), the UUID and the label, and the magic
// in the last ten bytes of the page. It returns nil if page holds no
// version 1 swap header.
func parseSwapHeader(page []byte) *swapHeader {
	if len(page) < 1024+28 || string(page[len(page)-len(swapHeaderMagic):]) != swapHeaderMagic {
		return nil
	}
	version := binary.NativeEndian.Uint32(page[1024:])
	lastPage := binary.NativeEndian.Uint32(page[1028:])
	if version != 1 || lastPage == 0 {
		return nil
	}
	return &swapHeader{
		bytes: (int64(lastPage) + 1) * int64(len(page)),
		uuid:  formatUUID(page[1036 : 1036+16]),
	}
}

// formatUUID formats 16 bytes as a UUID string, or returns "" if they are
// all zero, which mkswap writes when it has no UUID generator.
func formatUUID(b []byte) string {
	if bytes.Equal(b, make([]byte, 16)) {
		return ""
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// readSwapHeader reads the swap header of the file or device at p, which is
// not followed if it is a symlink. It returns nil without an error if p
// holds no swap header.
func readSwapHeader(p string) (*swapHeader, error) {
	f, err := openNoFollow(p, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	page := make([]byte, os.Getpagesize())
	if _, err := io.ReadFull(f, page); errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return parseSwapHeader(page), nil
}

// signatureMagic is a signature that marks data a swap area must not
// overwrite.
type signatureMagic struct {
	name   string
	offset int
	magic  string
}

// knownSignatures are the file system, volume and partition table
// signatures that probeSignatures recognises without blkid(8).
var knownSignatures = []signatureMagic{
	{"ext2/ext3/ext4", 1080, "\x53\xef"},
	{"xfs", 0, "XFSB"},
	{"btrfs", 65600, "_BHRfS_M"},
	{"LUKS", 0, "LUKS\xba\xbe"},
	{"LVM2 physical volume", 512, "LABELONE"},
	{"LVM2 physical volume", 0, "LABELONE"},
	{"GPT partition table", 512, "EFI PART"},
	{"squashfs", 0, "hsqs"},
	{"iso9660", 32769, "CD001"},
	{"ntfs", 3, "NTFS    "},
	{"f2fs", 1024, "\x10\x20\xf5\xf2"},
	{"bcache", 4120, "\xc6\x85\x73\xf6\x4e\x1a\x45\xca\x82\x65\xf5\x7f\x48\xba\x6d\x81"},
	{"DOS boot sector or partition table", 510, "\x55\xaa"},
}

// probeSignatures looks for known signatures in the first swapProbeSize
// bytes of data. It returns the name of the first one found, "swap" for a
// swap area without any other signature, or "" if there is none. Other
// signatures take precedence over a swap header, so that data written over
// an old swap area, without wiping it, is not taken for swap.
func probeSignatures(data []byte) string {
	for _, s := range knownSignatures {
		if len(data) >= s.offset+len(s.magic) && string(data[s.offset:s.offset+len(s.magic)]) == s.magic {
			return s.name
		}
	}
	for _, ps := range []int{4096, 8192, 16384, 65536} {
		if len(data) >= ps {
			m := string(data[ps-len(swapHeaderMagic) : ps])
			if m == swapHeaderMagic || m == swapHeaderLegacyMagic {
				return "swap"
			}
		}
	}
	return ""
}

// readProbeData reads the start of the file or device at p for
// probeSignatures, following a symlink at p for devices only.
func readProbeData(p string, follow bool) ([]byte, error) {
	var f *os.File
	var err error
	if follow {
		f, err = os.OpenFile(p, os.O_RDONLY|syscall.O_CLOEXEC, 0)
	} else {
		f, err = openNoFollow(p, os.O_RDONLY, 0)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, swapProbeSize)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	return buf[:n], nil
}

// parseBlkidExport returns the signature that `blkid -p -o export` reports,
// or "" if it reports none.
func parseBlkidExport(out string) string {
	var pt string
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		switch {
		case !ok:
		case k == "TYPE" && v != "":
			return v
		case k == "PTTYPE" && v != "":
			pt = v + " partition table"
		}
	}
	return pt
}

// checkBlockDeviceUnused opens the block device p exclusively, which the
// kernel refuses with EBUSY while it is mounted, active as swap, or held by
// device-mapper, md or another exclusive opener.
func checkBlockDeviceUnused(p string) error {
	fd, err := unix.Open(p, unix.O_RDONLY|unix.O_EXCL|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.EBUSY) {
		return fmt.Errorf("device %q is in use (mounted, active, or part of a device-mapper, RAID or LVM volume)", p)
	}
	if err != nil {
		return fmt.Errorf("opening %q: %w", p, err)
	}
	return unix.Close(fd)
}

// swapManager formats, activates and deactivates swap areas and lists the
// active ones. systemSwapManager is the production implementation.
type swapManager interface {
	// swaps returns the active swap areas.
	swaps() ([]swapEntry, error)
	// mkswap writes a swap header to the file or device at p. force also
	// overwrites a partition table on a whole disk.
	mkswap(ctx context.Context, p string, force bool) error
	// swapon activates p, with priority unless it is nil.
	swapon(ctx context.Context, p string, priority *int64) error
	swapoff(ctx context.Context, p string) error
	// probe returns the file system or partition table signature on the
	// file or device at p, "swap" for a swap area, or "" if there is none.
	probe(ctx context.Context, p string, device bool) (string, error)
}

// swapConfig is the provider-level configuration of sysutils_swap. The zero
// value, or a nil pointer, selects /etc/fstab inside root_dir and the real
// swap tools.
type swapConfig struct {
	// fstabPath is a host path that replaces /etc/fstab inside root_dir.
	fstabPath string
	manager   swapManager
}

func (c *swapConfig) mgr() swapManager {
	if c == nil || c.manager == nil {
		return systemSwapManager{run: runCommand, procSwaps: defaultProcSwapsPath, timeout: swapCommandTimeout}
	}
	return c.manager
}

// fstab returns the host path of the fstab that the resource edits.
func (c *swapConfig) fstab(root *fsRoot) (string, error) {
	if c != nil && c.fstabPath != "" {
		return c.fstabPath, nil
	}
	return root.resolve(defaultFstabPath)
}

// systemSwapManager runs mkswap(8), swapon(8), swapoff(8) and blkid(8) and
// reads /proc/swaps.
type systemSwapManager struct {
	run       commandRunner
	procSwaps string
	timeout   time.Duration
}

func (m systemSwapManager) swaps() ([]swapEntry, error) {
	f, err := os.Open(m.procSwaps)
	if err != nil {
		return nil, fmt.Errorf("reading active swap areas: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxProcSwapsSize))
	if err != nil {
		return nil, fmt.Errorf("reading active swap areas: %w", err)
	}
	return parseProcSwaps(string(data))
}

func (m systemSwapManager) mkswap(ctx context.Context, p string, force bool) error {
	argv := []string{"mkswap"}
	if force {
		argv = append(argv, "-f")
	}
	return m.do(ctx, append(argv, "--", p)...)
}

func (m systemSwapManager) swapon(ctx context.Context, p string, priority *int64) error {
	argv := []string{"swapon"}
	if priority != nil {
		argv = append(argv, "-p", strconv.FormatInt(*priority, 10))
	}
	return m.do(ctx, append(argv, "--", p)...)
}

func (m systemSwapManager) swapoff(ctx context.Context, p string) error {
	return m.do(ctx, "swapoff", "--", p)
}

// probe checks the well-known signatures itself and then asks blkid(8), if
// it is installed, which knows many more.
func (m systemSwapManager) probe(ctx context.Context, p string, device bool) (string, error) {
	data, err := readProbeData(p, device)
	if err != nil {
		return "", err
	}
	builtin := probeSignatures(data)
	if builtin != "" && builtin != "swap" {
		return builtin, nil
	}
	// blkid also reports a swap area that holds another signature as
	// ambivalent.
	res, err := m.run(ctx, execSpec{
		Argv:           []string{"blkid", "-p", "-o", "export", "--", p},
		Env:            append(os.Environ(), "LC_ALL=C"),
		Timeout:        time.Minute,
		MaxOutputBytes: swapOutputLimit,
	})
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return builtin, nil // Without blkid, the built-in checks have to do.
	case err != nil:
		return "", fmt.Errorf("blkid: %w", err)
	case res.TimedOut:
		return "", errors.New("blkid: timed out")
	}
	switch res.ExitCode {
	case 0:
		if sig := parseBlkidExport(res.Stdout.String()); sig != "" {
			return sig, nil
		}
		return "unknown signature", nil
	case 2:
		return builtin, nil // Nothing found.
	case 8:
		return "several conflicting signatures", nil
	default:
		return "", fmt.Errorf("blkid -p %s: exit status %d: %s", p, res.ExitCode, strings.TrimSpace(res.Stderr.String()))
	}
}

func (m systemSwapManager) do(ctx context.Context, argv ...string) error {
	return runSystemCommand(ctx, m.run, m.timeout, swapOutputLimit, argv...)
}

// allocateSwapFile writes a new swap file of size bytes to p: it is created
// under a temporary name in the same directory with mode 0600 (and owned by
// root when the provider runs as root), allocated with fallocate(2) or, where
// that is unsupported, by writing zeros like dd(1), formatted with mkswap,
// and renamed to p. If replace is false, the rename fails if p exists;
// otherwise it replaces the regular file at p. Nothing is left behind on
// failure.
func allocateSwapFile(ctx context.Context, m swapManager, p string, size int64, replace bool) (err error) {
	dir, base := filepath.Split(p)
	if err := checkSwapFileDir(dir); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+base+".sysutils-"+randomID())
	f, err := openNoFollow(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, swapFileMode)
	if err != nil {
		return fmt.Errorf("creating swap file: %w", err)
	}
	defer func() {
		if f != nil {
			_ = f.Close()
		}
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if os.Geteuid() == 0 {
		if err := f.Chown(0, 0); err != nil {
			return fmt.Errorf("setting ownership of swap file: %w", err)
		}
	}
	// The umask may have cleared bits, but never adds any; set it anyway.
	if err := f.Chmod(swapFileMode); err != nil {
		return fmt.Errorf("setting mode of swap file: %w", err)
	}
	disableCopyOnWrite(f)
	if err := fillSwapFile(ctx, f, size); err != nil {
		return fmt.Errorf("allocating %d bytes for swap file: %w", size, err)
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		f = nil
		return err
	}
	f = nil
	if err := m.mkswap(ctx, tmp, false); err != nil {
		return err
	}
	if replace {
		if info, lerr := os.Lstat(p); lerr == nil && !info.Mode().IsRegular() {
			return fmt.Errorf("%q is not a regular file; refusing to replace it", p)
		}
		err = os.Rename(tmp, p)
	} else {
		err = renameNoReplace(tmp, p)
	}
	if err != nil {
		return err
	}
	syncDir(dir)
	return nil
}

// checkSwapFileDir checks that the directory for a swap file exists and
// that nobody but root or the provider's user can change what its path
// resolves to. mkswap(8) and swapon(8) take a path and follow symlinks, so
// if another user could replace the file, the directory or any directory
// above it (or a symlink on the way) with a symlink, mkswap could be made to
// overwrite any file or device, and swapon to swap to it. See
// checkTrustedDir.
func checkSwapFileDir(dir string) error {
	_, err := checkTrustedDir(dir, "swap file")
	return err
}

// fillSwapFile allocates size bytes for f with fallocate(2), or writes size
// zero bytes if the file system does not support it. Swap files must not
// have holes, so a sparse file made with truncate(1) will not do.
func fillSwapFile(ctx context.Context, f *os.File, size int64) error {
	err := unix.Fallocate(int(f.Fd()), 0, 0, size)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EOPNOTSUPP), errors.Is(err, unix.ENOSYS), errors.Is(err, unix.EINVAL):
	default:
		return err
	}
	buf := make([]byte, zeroFillChunk)
	for written := int64(0); written < size; {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := min(int64(len(buf)), size-written)
		if _, err := f.Write(buf[:n]); err != nil {
			return err
		}
		written += n
	}
	return nil
}

// disableCopyOnWrite sets the no-copy-on-write attribute (chattr +C) on the
// empty file f if it is on btrfs, which refuses to swap to files that
// have copy-on-write enabled. Failures are ignored: swapon reports them.
// Statfs_t.Type is an int32 on 32-bit platforms, where the magic number
// does not fit, so both sides are compared as uint32.
func disableCopyOnWrite(f *os.File) {
	var st unix.Statfs_t
	if unix.Fstatfs(int(f.Fd()), &st) != nil || uint32(st.Type) != uint32(unix.BTRFS_SUPER_MAGIC) {
		return
	}
	flags, err := unix.IoctlGetUint32(int(f.Fd()), unix.FS_IOC_GETFLAGS)
	if err != nil {
		return
	}
	v := int(flags | fsNoCOWFlag)
	_ = unix.IoctlSetPointerInt(int(f.Fd()), unix.FS_IOC_SETFLAGS, v)
}

// swapFstabEntry returns the fstab entry for the swap area at p.
func swapFstabEntry(p string, priority *int64) fstabEntry {
	opts := []string{"sw"}
	if priority != nil {
		opts = append(opts, swapPriorityOption+strconv.FormatInt(*priority, 10))
	}
	return fstabEntry{device: p, mountPoint: swapFstabMountPoint, fstype: swapFstabType, options: opts}
}

// isSwapFstabEntryFor reports whether e is a swap entry for the file or
// device p.
func isSwapFstabEntryFor(e fstabEntry, p string) bool {
	return e.fstype == swapFstabType && e.device == p
}

// fstabSwapPriority returns the priority set by the options of a swap
// entry, or nil if there is none or it is not a valid number.
func fstabSwapPriority(options []string) *int64 {
	var pri *int64
	for _, o := range options {
		if v, ok := strings.CutPrefix(o, swapPriorityOption); ok {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return nil
			}
			pri = &n
		}
	}
	return pri
}

// lookupSwapFstabEntry returns the first swap entry for p and how many
// there are.
func lookupSwapFstabEntry(t *textFile, p string) (*fstabEntry, int) {
	match := func(e fstabEntry) bool { return isSwapFstabEntryFor(e, p) }
	idx := findFstabEntriesFunc(t.lines, match)
	if len(idx) == 0 {
		return nil, 0
	}
	e, _ := parseFstabLine(t.lines[idx[0]])
	return &e, len(idx)
}

// setSwapFstabEntry makes e the only swap entry for its device; see
// setFstabEntry.
func setSwapFstabEntry(t *textFile, e fstabEntry) bool {
	return setFstabEntryFunc(t, e, func(o fstabEntry) bool { return isSwapFstabEntryFor(o, e.device) })
}

// removeSwapFstabEntries removes every swap entry for p and reports whether
// t changed.
func removeSwapFstabEntries(t *textFile, p string) bool {
	return removeFstabEntriesFunc(t, func(e fstabEntry) bool { return isSwapFstabEntryFor(e, p) })
}
