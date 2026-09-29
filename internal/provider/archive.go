package provider

// Reading and validating archives for sysutils_archive_extract.
//
// Archives are untrusted input: they may come from a download and be crafted
// to write outside the destination directory or to exhaust the disk.
// walkArchive therefore checks every entry before anyone acts on it:
//
//   - Names are relative, slash-separated paths. Absolute names, names with a
//     ".." component and names with NUL bytes are refused, whatever they
//     would resolve to (zip-slip), and so are names longer than PATH_MAX.
//   - Only directories, regular files, symlinks and hard links are accepted.
//     Device nodes, FIFOs and other special files are refused.
//   - No entry may be below another entry that is not a directory, so a
//     symlink in the archive can never be written through, and no path may
//     appear twice (except as a directory).
//   - Symlink targets must be relative. Each target is resolved inside the
//     archive the way the kernel would resolve it after extraction,
//     following the archive's other symlinks, and a target that leads above
//     the destination is refused. A purely lexical check is not enough:
//     with "sub/up -> ..", the link "sub/x -> up/.." looks harmless but
//     leads to the destination's parent.
//   - Hard links must refer to a regular file that appears earlier in the
//     same archive, so they can never link to a file outside it.
//   - The number of entries and the total size of the contents are capped,
//     both as declared in the headers and as actually decompressed, so a
//     decompression bomb fails early instead of filling the disk. The
//     directories that entries are in but that the archive does not list
//     count as entries too: otherwise a single entry with a deeply nested
//     name would create thousands of directories.

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/ulikunitz/xz"
)

// archiveKind is the type of an archive entry.
type archiveKind int

const (
	archiveDir archiveKind = iota
	archiveFile
	archiveSymlink
	archiveHardlink
)

func (k archiveKind) String() string {
	switch k {
	case archiveDir:
		return "directory"
	case archiveFile:
		return "regular file"
	case archiveSymlink:
		return "symbolic link"
	case archiveHardlink:
		return "hard link"
	default:
		return "unknown"
	}
}

// archiveEntry is one entry of an archive, after strip_components.
type archiveEntry struct {
	// Name is the entry's path relative to the destination: slash-separated,
	// never empty, absolute or containing "." or ".." components.
	Name string
	Kind archiveKind
	// Mode holds the permission bits recorded in the archive, masked with
	// archiveModeMask. A hard link has the mode of the file it links to.
	Mode fs.FileMode
	// Size is the size of a regular file, or of the file a hard link links
	// to.
	Size int64
	// Link is the target of a symlink, or for a hard link the Name of the
	// regular file it links to.
	Link    string
	ModTime time.Time
	// Implicit is set for directories that the archive has no entry for but
	// that must exist because entries below them do.
	Implicit bool
}

// archiveModeMask is applied to the modes recorded in an archive. It drops
// the setuid, setgid and sticky bits and write permission for group and
// others, like a umask of 022: zip archives made on Windows record 0666 and
// 0777, and extracting them as root must not leave world-writable files.
const archiveModeMask fs.FileMode = 0o755

// archiveLimits are the settings that walkArchive applies to every archive.
type archiveLimits struct {
	stripComponents int
	// maxSize caps the total size of the entries' contents, in bytes.
	maxSize int64
	// maxEntries caps the number of entries in the archive, including those
	// that strip_components removes.
	maxEntries int64
}

// archiveManifest is the validated list of entries of an archive.
type archiveManifest struct {
	entries map[string]*archiveEntry
	// order holds the entry names in the order they appear in the archive,
	// with implicit directories just before the first entry below them, so
	// that every directory comes before its contents.
	order []string
}

// names returns the names of all entries in ascending byte order. Every
// directory sorts before its contents.
func (m *archiveManifest) names() []string {
	names := make([]string, 0, len(m.entries))
	for name := range m.entries {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// fileList returns the manifest as stored in the files attribute: the sorted
// names, with a trailing slash on directories.
func (m *archiveManifest) fileList() []string {
	names := m.names()
	for i, name := range names {
		if m.entries[name].Kind == archiveDir {
			names[i] = name + "/"
		}
	}
	return names
}

// Magic numbers of the supported (and some unsupported) formats.
var (
	magicGzip  = []byte{0x1f, 0x8b}
	magicXz    = []byte{0xfd, '7', 'z', 'X', 'Z', 0x00}
	magicZip   = []byte("PK\x03\x04")
	magicZipE  = []byte("PK\x05\x06") // An empty zip archive.
	magicBzip2 = []byte("BZh")
	magicZstd  = []byte{0x28, 0xb5, 0x2f, 0xfd}
)

// archiveFormat returns the format of the archive f from its first bytes:
// "zip", "tar.gz", "tar.xz" or "tar".
func archiveFormat(f io.ReaderAt) (string, error) {
	head := make([]byte, 512)
	n, err := f.ReadAt(head, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	head = head[:n]
	switch {
	case bytes.HasPrefix(head, magicZip), bytes.HasPrefix(head, magicZipE):
		return "zip", nil
	case bytes.HasPrefix(head, magicGzip):
		return "tar.gz", nil
	case bytes.HasPrefix(head, magicXz):
		return "tar.xz", nil
	case bytes.HasPrefix(head, magicBzip2):
		return "", errors.New("bzip2-compressed archives are not supported; use .tar, .tar.gz, .tar.xz or .zip")
	case bytes.HasPrefix(head, magicZstd):
		return "", errors.New("zstd-compressed archives are not supported; use .tar, .tar.gz, .tar.xz or .zip")
	case n == 0:
		return "", errors.New("the archive is empty")
	}
	// Anything else is read as an uncompressed tar archive; the tar reader
	// reports an error if it is not one.
	return "tar", nil
}

// archiveSHA256 returns the hex-encoded SHA-256 checksum of the whole file f.
func archiveSHA256(f *os.File) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(f, 0, 1<<62)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// tarOverheadPerEntry bounds the bytes of a tar stream that are not entry
// contents: the header block, padding, and GNU long name or PAX records.
// Together with maxSize it caps how much a compressed tar stream may
// decompress to, so that padding or oversized headers cannot be used as a
// decompression bomb either.
const tarOverheadPerEntry = 16 << 10

// maxSymlinkTargetLen is the longest symlink target accepted, PATH_MAX.
const maxSymlinkTargetLen = 4096

// maxArchiveNameLen is the longest entry name accepted, after
// strip_components: PATH_MAX, like symlink targets.
const maxArchiveNameLen = 4096

// walkArchive reads the archive f, validates every entry as described in the
// file comment, and calls visit, if not nil, for every entry that survives
// strip_components, in archive order. content reads the contents of a
// regular file and is empty for other entries. Implicit directories are not
// visited. The symlink check needs every entry, so it runs after the last
// visit: a visitor must not make the result visible until walkArchive has
// returned without error.
func walkArchive(f *os.File, lim archiveLimits, visit func(e *archiveEntry, content io.Reader) error) (*archiveManifest, error) {
	if lim.maxSize < 1 || lim.maxEntries < 1 || lim.stripComponents < 0 {
		return nil, fmt.Errorf("invalid limits: max_size %d, max_entries %d, strip_components %d", lim.maxSize, lim.maxEntries, lim.stripComponents)
	}
	format, err := archiveFormat(f)
	if err != nil {
		return nil, err
	}
	w := &archiveWalker{
		lim:   lim,
		visit: visit,
		m:     &archiveManifest{entries: map[string]*archiveEntry{}},
	}
	if format == "zip" {
		err = w.walkZip(f)
	} else {
		err = w.walkTar(f, format)
	}
	if err != nil {
		return nil, err
	}
	if err := w.checkSymlinks(); err != nil {
		return nil, err
	}
	return w.m, nil
}

type archiveWalker struct {
	lim     archiveLimits
	visit   func(e *archiveEntry, content io.Reader) error
	m       *archiveManifest
	entries int64 // Entries seen, including stripped ones.
	size    int64 // Declared content bytes seen, including stripped entries.
}

// count accounts for one more entry with size bytes of content.
func (w *archiveWalker) count(name string, size int64) error {
	w.entries++
	if w.entries > w.lim.maxEntries {
		return fmt.Errorf("the archive has more than %d entries (max_entries)", w.lim.maxEntries)
	}
	if size < 0 {
		return fmt.Errorf("entry %q has a negative size", name)
	}
	if size > w.lim.maxSize-w.size {
		return fmt.Errorf("the contents of the archive are larger than %d bytes (max_size), at entry %q", w.lim.maxSize, name)
	}
	w.size += size
	return nil
}

func (w *archiveWalker) walkTar(f *os.File, format string) error {
	var r io.Reader = io.NewSectionReader(f, 0, 1<<62)
	switch format {
	case "tar.gz":
		zr, err := gzip.NewReader(r)
		if err != nil {
			return fmt.Errorf("reading gzip stream: %w", err)
		}
		defer func() { _ = zr.Close() }()
		r = zr
	case "tar.xz":
		// The xz reader allocates the dictionary size a block header asks
		// for, but only as address space: pages are touched only as data is
		// decompressed, which the stream limit below bounds.
		xr, err := xz.NewReader(r)
		if err != nil {
			return fmt.Errorf("reading xz stream: %w", err)
		}
		r = xr
	}
	// Two zero blocks end the archive, and a record is padded to 10 KiB.
	streamLimit := w.lim.maxSize
	overhead := (w.lim.maxEntries + 1) * tarOverheadPerEntry
	if streamLimit > (1<<62)-overhead {
		streamLimit = 1 << 62
	} else {
		streamLimit += overhead
	}
	lr := &limitErrReader{r: r, n: streamLimit, err: fmt.Errorf(
		"the archive decompresses to more than %d bytes, which max_size and max_entries allow", streamLimit)}
	tr := tar.NewReader(lr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			if lr.exceeded {
				return lr.err
			}
			return fmt.Errorf("reading tar archive: %w", err)
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		if err := w.count(h.Name, h.Size); err != nil {
			return err
		}
		var kind archiveKind
		switch h.Typeflag {
		case tar.TypeDir:
			kind = archiveDir
		case tar.TypeReg, tar.TypeRegA, tar.TypeGNUSparse: //nolint:staticcheck // TypeRegA is what old archives contain.
			kind = archiveFile
		case tar.TypeSymlink:
			kind = archiveSymlink
		case tar.TypeLink:
			kind = archiveHardlink
		case tar.TypeChar, tar.TypeBlock:
			return fmt.Errorf("entry %q is a device node; only directories, regular files, symbolic links and hard links are supported", h.Name)
		case tar.TypeFifo:
			return fmt.Errorf("entry %q is a FIFO; only directories, regular files, symbolic links and hard links are supported", h.Name)
		default:
			return fmt.Errorf("entry %q has the unsupported tar type %q", h.Name, h.Typeflag)
		}
		e := &archiveEntry{
			Kind:    kind,
			Mode:    fs.FileMode(h.Mode) & archiveModeMask,
			Link:    h.Linkname,
			ModTime: h.ModTime,
		}
		if kind == archiveFile {
			e.Size = h.Size
		}
		var content io.Reader = eofReader{}
		if kind == archiveFile {
			content = &exactReader{r: tr, name: h.Name, left: h.Size}
		}
		if err := w.add(h.Name, e, content); err != nil {
			if lr.exceeded {
				return lr.err
			}
			return err
		}
	}
}

func (w *archiveWalker) walkZip(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	zr, err := zip.NewReader(f, info.Size())
	if err != nil {
		return fmt.Errorf("reading zip archive: %w", err)
	}
	// Check the limits for the whole archive before extracting anything.
	if int64(len(zr.File)) > w.lim.maxEntries {
		return fmt.Errorf("the archive has more than %d entries (max_entries)", w.lim.maxEntries)
	}
	var total uint64
	for _, zf := range zr.File {
		total += zf.UncompressedSize64
		if zf.UncompressedSize64 > uint64(w.lim.maxSize) || total > uint64(w.lim.maxSize) {
			return fmt.Errorf("the contents of the archive are larger than %d bytes (max_size), at entry %q", w.lim.maxSize, zf.Name)
		}
	}
	for _, zf := range zr.File {
		if err := w.count(zf.Name, int64(zf.UncompressedSize64)); err != nil {
			return err
		}
		mode := zf.Mode()
		e := &archiveEntry{Mode: mode & archiveModeMask, ModTime: zf.Modified}
		switch {
		case mode.IsDir() || strings.HasSuffix(zf.Name, "/"):
			e.Kind = archiveDir
		case mode&fs.ModeSymlink != 0:
			e.Kind = archiveSymlink
			if zf.UncompressedSize64 > maxSymlinkTargetLen {
				return fmt.Errorf("symbolic link %q has a target longer than %d bytes", zf.Name, maxSymlinkTargetLen)
			}
			target, err := readZipEntry(zf, maxSymlinkTargetLen)
			if err != nil {
				return err
			}
			e.Link = string(target)
		case mode.IsRegular():
			e.Kind = archiveFile
			e.Size = int64(zf.UncompressedSize64)
		default:
			return fmt.Errorf("entry %q is a %s; only directories, regular files and symbolic links are supported in zip archives", zf.Name, fileTypeName(mode))
		}
		if e.Kind != archiveFile {
			if err := w.add(zf.Name, e, eofReader{}); err != nil {
				return err
			}
			continue
		}
		// Opened lazily, so that plan-time scans don't decompress anything.
		content := &lazyZipReader{zf: zf}
		err := w.add(zf.Name, e, content)
		if cerr := content.close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// readZipEntry returns the contents of zf, which must be at most limit
// bytes.
func readZipEntry(zf *zip.File, limit int64) ([]byte, error) {
	rc, err := zf.Open()
	if err != nil {
		return nil, fmt.Errorf("reading %q: %w", zf.Name, err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, fmt.Errorf("reading %q: %w", zf.Name, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("entry %q is larger than %d bytes", zf.Name, limit)
	}
	return data, nil
}

// fileTypeName describes the type of mode for error messages.
func fileTypeName(mode fs.FileMode) string {
	switch {
	case mode&fs.ModeDevice != 0, mode&fs.ModeCharDevice != 0:
		return "device node"
	case mode&fs.ModeNamedPipe != 0:
		return "FIFO"
	case mode&fs.ModeSocket != 0:
		return "socket"
	default:
		return "special file"
	}
}

// add normalizes the raw archive name of e, validates e against the entries
// seen so far, records it and visits it.
func (w *archiveWalker) add(raw string, e *archiveEntry, content io.Reader) error {
	name, ok, err := normalizeArchiveName(raw, w.lim.stripComponents)
	if err != nil {
		return err
	}
	if !ok {
		return nil // Removed by strip_components, or the destination itself.
	}
	if len(name) > maxArchiveNameLen {
		return fmt.Errorf("entry %.64q... has a name longer than %d bytes", name, maxArchiveNameLen)
	}
	e.Name = name

	switch e.Kind {
	case archiveSymlink:
		if err := validateArchiveSymlinkTarget(raw, e.Link); err != nil {
			return err
		}
	case archiveHardlink:
		target, ok, err := normalizeArchiveName(e.Link, w.lim.stripComponents)
		if err != nil {
			return fmt.Errorf("hard link %q: %w", raw, err)
		}
		if !ok {
			return fmt.Errorf("hard link %q links to %q, which is not extracted (strip_components)", raw, e.Link)
		}
		base := w.m.entries[target]
		if base == nil || (base.Kind != archiveFile && base.Kind != archiveHardlink) {
			return fmt.Errorf("hard link %q must link to a regular file earlier in the archive, not to %q", raw, e.Link)
		}
		if base.Kind == archiveHardlink {
			base = w.m.entries[base.Link]
		}
		e.Link, e.Size, e.Mode = base.Name, base.Size, base.Mode
	}

	// Every ancestor must be a directory; create implicit ones.
	for i := 0; i < len(name); i++ {
		if name[i] != '/' {
			continue
		}
		parent := name[:i]
		if pe := w.m.entries[parent]; pe != nil {
			if pe.Kind != archiveDir {
				return fmt.Errorf("entry %q is inside %q, which is a %s in the archive, not a directory", raw, parent, pe.Kind)
			}
			continue
		}
		w.entries++
		if w.entries > w.lim.maxEntries {
			return fmt.Errorf("the archive has more than %d entries (max_entries), counting the directories its entries are in", w.lim.maxEntries)
		}
		w.m.entries[parent] = &archiveEntry{Name: parent, Kind: archiveDir, Mode: 0o755, Implicit: true}
		w.m.order = append(w.m.order, parent)
	}

	if prev := w.m.entries[name]; prev != nil {
		if prev.Kind != archiveDir || e.Kind != archiveDir {
			return fmt.Errorf("the archive contains %q more than once", name)
		}
		// A directory listed again, or listed after its contents: the
		// later entry's attributes win, as with tar.
		*prev = *e
	} else {
		w.m.entries[name] = e
		w.m.order = append(w.m.order, name)
	}
	if w.visit == nil {
		return nil
	}
	return w.visit(e, content)
}

// normalizeArchiveName returns the path below the destination for the
// archive entry name raw, with the first strip components removed. ok is
// false if nothing is left, which is the case for the entry of the archive's
// root directory ("./") and for entries that strip removes.
func normalizeArchiveName(raw string, strip int) (name string, ok bool, err error) {
	switch {
	case raw == "":
		return "", false, errors.New("the archive contains an entry without a name")
	case strings.ContainsRune(raw, 0):
		return "", false, fmt.Errorf("entry name %q contains a NUL byte", raw)
	case strings.HasPrefix(raw, "/"):
		return "", false, fmt.Errorf("entry %q has an absolute path; refusing to extract outside the destination", raw)
	}
	var parts []string
	for _, c := range strings.Split(raw, "/") {
		switch c {
		case "", ".":
		case "..":
			return "", false, fmt.Errorf("entry %q contains a \"..\" path component; refusing to extract outside the destination", raw)
		default:
			parts = append(parts, c)
		}
	}
	if len(parts) <= strip {
		return "", false, nil
	}
	return strings.Join(parts[strip:], "/"), true, nil
}

// validateArchiveSymlinkTarget checks the target of the symlink raw on its
// own; checkSymlinks checks where it leads.
func validateArchiveSymlinkTarget(raw, target string) error {
	switch {
	case target == "":
		return fmt.Errorf("symbolic link %q has an empty target", raw)
	case len(target) > maxSymlinkTargetLen:
		return fmt.Errorf("symbolic link %q has a target longer than %d bytes", raw, maxSymlinkTargetLen)
	case strings.ContainsRune(target, 0):
		return fmt.Errorf("symbolic link %q has a target with a NUL byte", raw)
	case strings.HasPrefix(target, "/"):
		return fmt.Errorf("symbolic link %q has the absolute target %q; refusing to create a link that leads outside the destination", raw, target)
	}
	return nil
}

// checkSymlinks resolves the target of every symlink in the manifest as the
// kernel would after extraction, following the other symlinks of the
// archive, and fails if one leads above the destination.
func (w *archiveWalker) checkSymlinks() error {
	work := int64(maxSymlinkCheckWork)
	for _, name := range w.m.order {
		e := w.m.entries[name]
		if e.Kind != archiveSymlink {
			continue
		}
		if err := w.m.resolveSymlink(e, &work); err != nil {
			return err
		}
	}
	return nil
}

// maxSymlinkCheckWork bounds the bytes of path names that checkSymlinks
// looks up for a whole archive. Each lookup costs the length of the path
// resolved so far, so an archive of many symlinks with long targets into
// deep directories, each leading through other such symlinks, could
// otherwise keep a plan busy for hours. Real archives stay far below it.
const maxSymlinkCheckWork = 1 << 28

// resolveSymlink follows link inside the archive. work is the remaining
// budget of maxSymlinkCheckWork.
func (m *archiveManifest) resolveSymlink(link *archiveEntry, work *int64) error {
	var stack []string // Components of the path resolved so far.
	if dir := path.Dir(link.Name); dir != "." {
		stack = strings.Split(dir, "/")
	}
	todo := strings.Split(link.Link, "/")
	hops := 0
	for len(todo) > 0 {
		c := todo[0]
		todo = todo[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			if len(stack) == 0 {
				return fmt.Errorf("symbolic link %q -> %q leads outside the destination; refusing to extract it", link.Name, link.Link)
			}
			stack = stack[:len(stack)-1]
			continue
		}
		stack = append(stack, c)
		key := strings.Join(stack, "/")
		if *work -= int64(len(key)); *work < 0 {
			return fmt.Errorf("the symbolic links of the archive are too complex to check that they stay inside the destination, at %q", link.Name)
		}
		e := m.entries[key]
		if e == nil || e.Kind != archiveSymlink {
			// Names the archive does not contain are treated as plain
			// directories: nothing the archive creates is followed there.
			continue
		}
		hops++
		if hops > maxSymlinkHops {
			return fmt.Errorf("symbolic link %q -> %q: too many levels of symbolic links in the archive", link.Name, link.Link)
		}
		stack = stack[:len(stack)-1]
		todo = append(strings.Split(e.Link, "/"), todo...)
	}
	return nil
}

// limitErrReader reads from r, failing with err once more than n bytes
// have been read.
type limitErrReader struct {
	r        io.Reader
	n        int64
	err      error
	exceeded bool
}

func (l *limitErrReader) Read(p []byte) (int, error) {
	if l.exceeded {
		return 0, l.err
	}
	if int64(len(p)) > l.n+1 {
		p = p[:l.n+1]
	}
	n, err := l.r.Read(p)
	l.n -= int64(n)
	if l.n < 0 {
		l.exceeded = true
		return 0, l.err
	}
	return n, err
}

// exactReader reads the contents of an archive entry and fails if they are
// not exactly left bytes long.
type exactReader struct {
	r    io.Reader
	name string
	left int64
}

func (e *exactReader) Read(p []byte) (int, error) {
	if e.left <= 0 {
		// Check that there is no more data than declared. Reading up to
		// the end also makes the zip reader verify the CRC-32.
		var one [1]byte
		n, err := e.r.Read(one[:])
		switch {
		case n > 0:
			return 0, fmt.Errorf("entry %q has more data than its declared size", e.name)
		case err != nil && !errors.Is(err, io.EOF):
			return 0, fmt.Errorf("entry %q: %w", e.name, err)
		}
		return 0, io.EOF
	}
	if int64(len(p)) > e.left {
		p = p[:e.left]
	}
	n, err := e.r.Read(p)
	e.left -= int64(n)
	if errors.Is(err, io.EOF) && e.left > 0 {
		return n, fmt.Errorf("entry %q is truncated: %w", e.name, io.ErrUnexpectedEOF)
	}
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return n, err
}

// lazyZipReader opens a zip entry on the first Read and reads exactly its
// declared size. Go's zip reader checks the size and CRC-32 at the end.
type lazyZipReader struct {
	zf *zip.File
	rc io.ReadCloser
	r  io.Reader
}

func (l *lazyZipReader) Read(p []byte) (int, error) {
	if l.r == nil {
		rc, err := l.zf.Open()
		if err != nil {
			return 0, fmt.Errorf("reading %q: %w", l.zf.Name, err)
		}
		l.rc = rc
		l.r = &exactReader{r: rc, name: l.zf.Name, left: int64(l.zf.UncompressedSize64)}
	}
	return l.r.Read(p)
}

func (l *lazyZipReader) close() error {
	if l.rc == nil {
		return nil
	}
	return l.rc.Close()
}

// eofReader is the content of entries without data.
type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }
