package provider

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ulikunitz/xz"
)

// testEntry describes an entry of a test archive.
type testEntry struct {
	name string
	typ  byte // A tar type flag; zip archives support dirs, files and symlinks.
	body string
	link string
	mode int64
}

func arFile(name, body string) testEntry { return testEntry{name: name, typ: tar.TypeReg, body: body} }
func arDir(name string) testEntry        { return testEntry{name: name, typ: tar.TypeDir} }
func arSymlink(name, target string) testEntry {
	return testEntry{name: name, typ: tar.TypeSymlink, link: target}
}
func arHardlink(name, target string) testEntry {
	return testEntry{name: name, typ: tar.TypeLink, link: target}
}

var testModTime = time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)

func makeTar(t *testing.T, entries ...testEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		mode := e.mode
		if mode == 0 {
			mode = 0o644
			if e.typ == tar.TypeDir {
				mode = 0o755
			}
		}
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Linkname: e.link, Mode: mode, ModTime: testModTime, Format: tar.FormatPAX}
		if e.typ == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatalf("writing tar header %q: %v", e.name, err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func xzBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	xw, err := xz.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := xw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := xw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func makeZip(t *testing.T, entries ...testEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate, Modified: testModTime}
		body := e.body
		switch e.typ {
		case tar.TypeDir:
			h.SetMode(fs.ModeDir | 0o755)
			if !strings.HasSuffix(h.Name, "/") {
				h.Name += "/"
			}
		case tar.TypeSymlink:
			h.SetMode(fs.ModeSymlink | 0o777)
			body = e.link
		case tar.TypeFifo:
			h.SetMode(fs.ModeNamedPipe | 0o644)
		default:
			mode := fs.FileMode(e.mode)
			if mode == 0 {
				mode = 0o644
			}
			h.SetMode(mode)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// writeTestArchive writes data to a new file and returns it, opened.
func writeTestArchive(t *testing.T, data []byte) *os.File {
	t.Helper()
	p := filepath.Join(t.TempDir(), "archive")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

var testLimits = archiveLimits{maxSize: 1 << 20, maxEntries: 1000}

func TestWalkArchive_formats(t *testing.T) {
	entries := []testEntry{
		arDir("app/"),
		arFile("app/README", "hello\n"),
		{name: "app/bin/tool", typ: tar.TypeReg, body: "#!/bin/sh\n", mode: 0o755},
		arSymlink("app/current", "bin/tool"),
	}
	want := []string{"app/", "app/README", "app/bin/", "app/bin/tool", "app/current"}
	tarData := makeTar(t, entries...)
	for name, data := range map[string][]byte{
		"tar":    tarData,
		"tar.gz": gzipBytes(t, tarData),
		"tar.xz": xzBytes(t, tarData),
		"zip":    makeZip(t, entries...),
	} {
		t.Run(name, func(t *testing.T) {
			f := writeTestArchive(t, data)
			format, err := archiveFormat(f)
			if err != nil || format != name {
				t.Fatalf("archiveFormat = %q, %v; want %q", format, err, name)
			}
			var bodies []string
			m, err := walkArchive(f, testLimits, func(e *archiveEntry, content io.Reader) error {
				b, err := io.ReadAll(content)
				if e.Kind == archiveFile {
					bodies = append(bodies, e.Name+"="+string(b))
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := m.fileList(); !slices.Equal(got, want) {
				t.Errorf("fileList = %q, want %q", got, want)
			}
			if !m.entries["app/bin"].Implicit || m.entries["app"].Implicit {
				t.Errorf("implicit flags wrong: app/bin %v, app %v", m.entries["app/bin"].Implicit, m.entries["app"].Implicit)
			}
			if got := m.entries["app/bin/tool"].Mode; got != 0o755 {
				t.Errorf("mode of app/bin/tool = %o, want 755", got)
			}
			if !slices.Equal(bodies, []string{"app/README=hello\n", "app/bin/tool=#!/bin/sh\n"}) {
				t.Errorf("contents = %q", bodies)
			}
		})
	}
}

func TestWalkArchive_unsupportedFormats(t *testing.T) {
	for name, data := range map[string][]byte{
		"empty": nil,
		"bzip2": []byte("BZh91AY&SY"),
		"zstd":  {0x28, 0xb5, 0x2f, 0xfd, 0, 0},
		"text":  []byte(strings.Repeat("not an archive\n", 100)),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := walkArchive(writeTestArchive(t, data), testLimits, nil); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestWalkArchive_stripComponents(t *testing.T) {
	f := writeTestArchive(t, makeTar(t,
		arDir("./"),
		arDir("./app-1.2.3/"),
		arFile("./app-1.2.3/bin/app", "x"),
		arFile("./app-1.2.3/README", "y"),
		arFile("./TOPLEVEL", "stripped"),
		arSymlink("./app-1.2.3/current", "bin/app"),
		arHardlink("./app-1.2.3/app.hard", "./app-1.2.3/bin/app"),
	))
	lim := testLimits
	lim.stripComponents = 1
	m, err := walkArchive(f, lim, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"README", "app.hard", "bin/", "bin/app", "current"}
	if got := m.fileList(); !slices.Equal(got, want) {
		t.Errorf("fileList = %q, want %q", got, want)
	}
	if e := m.entries["app.hard"]; e.Kind != archiveHardlink || e.Link != "bin/app" {
		t.Errorf("hard link = %+v", e)
	}

	// A hard link to a file that strip_components removes is refused.
	f = writeTestArchive(t, makeTar(t, arFile("top", "x"), arHardlink("dir/link", "top")))
	if _, err := walkArchive(f, lim, nil); err == nil || !strings.Contains(err.Error(), "not extracted") {
		t.Errorf("err = %v, want a hard link error", err)
	}
}

// TestWalkArchive_malicious checks that crafted archives that would write
// outside the destination are refused as a whole.
func TestWalkArchive_malicious(t *testing.T) {
	tests := []struct {
		name    string
		zip     bool
		entries []testEntry
		wantErr string
	}{
		{name: "tar zip-slip", entries: []testEntry{arFile("ok", "x"), arFile("../evil", "x")}, wantErr: `".."`},
		{name: "tar nested zip-slip", entries: []testEntry{arFile("a/../../evil", "x")}, wantErr: `".."`},
		{name: "tar dot-dot inside", entries: []testEntry{arFile("a/../b", "x")}, wantErr: `".."`},
		{name: "tar absolute path", entries: []testEntry{arFile("/etc/evil", "x")}, wantErr: "absolute path"},
		{name: "zip zip-slip", zip: true, entries: []testEntry{arFile("ok", "x"), arFile("../../evil", "x")}, wantErr: `".."`},
		{name: "zip absolute path", zip: true, entries: []testEntry{arFile("/tmp/evil", "x")}, wantErr: "absolute path"},
		{name: "symlink absolute", entries: []testEntry{arSymlink("etc", "/etc")}, wantErr: "absolute target"},
		{name: "symlink up", entries: []testEntry{arSymlink("up", "..")}, wantErr: "leads outside"},
		{name: "symlink nested up", entries: []testEntry{arSymlink("a/b/c", "../../../x")}, wantErr: "leads outside"},
		{name: "symlink up and down", entries: []testEntry{arSymlink("a", "b/../../x")}, wantErr: "leads outside"},
		{
			// Lexically, sub/x -> up/.. stays inside, but sub/up is the
			// destination itself, so up/.. is its parent.
			name:    "symlink chain",
			entries: []testEntry{arSymlink("sub/up", ".."), arSymlink("sub/x", "up/..")},
			wantErr: "leads outside",
		},
		{
			// The escaping link comes first; the link it goes through is
			// only defined later in the archive.
			name:    "symlink chain defined later",
			entries: []testEntry{arSymlink("x", "y/.."), arSymlink("y", ".")},
			wantErr: "leads outside",
		},
		{name: "symlink loop", entries: []testEntry{arSymlink("a", "b"), arSymlink("b", "a")}, wantErr: "too many levels"},
		{name: "zip symlink escape", zip: true, entries: []testEntry{arSymlink("link", "../../../etc/passwd")}, wantErr: "leads outside"},
		{name: "zip symlink absolute", zip: true, entries: []testEntry{arSymlink("link", "/etc/passwd")}, wantErr: "absolute target"},
		{
			// A symlink followed by a file below it would write through it.
			name:    "write through symlink",
			entries: []testEntry{arSymlink("dir", "sub"), arDir("sub/"), arFile("dir/evil", "x")},
			wantErr: "not a directory",
		},
		{
			name:    "zip write through symlink",
			zip:     true,
			entries: []testEntry{arSymlink("dir", "."), arFile("dir/evil", "x")},
			wantErr: "not a directory",
		},
		{name: "file below file", entries: []testEntry{arFile("a", "x"), arFile("a/b", "y")}, wantErr: "not a directory"},
		{name: "file replaces implicit directory", entries: []testEntry{arFile("a/b", "y"), arFile("a", "x")}, wantErr: "more than once"},
		{name: "duplicate file", entries: []testEntry{arFile("a", "x"), arFile("a", "y")}, wantErr: "more than once"},
		{name: "symlink then file", entries: []testEntry{arSymlink("a", "b"), arFile("a", "y")}, wantErr: "more than once"},
		{name: "hard link dot-dot", entries: []testEntry{arHardlink("passwd", "../../etc/passwd")}, wantErr: `".."`},
		{name: "hard link absolute", entries: []testEntry{arHardlink("passwd", "/etc/passwd")}, wantErr: "absolute path"},
		{name: "hard link to missing file", entries: []testEntry{arHardlink("passwd", "etc/passwd")}, wantErr: "earlier in the archive"},
		{name: "hard link to later file", entries: []testEntry{arHardlink("a", "b"), arFile("b", "x")}, wantErr: "earlier in the archive"},
		{name: "hard link to symlink", entries: []testEntry{arSymlink("s", "t"), arHardlink("a", "s")}, wantErr: "earlier in the archive"},
		{name: "hard link to directory", entries: []testEntry{arDir("d/"), arHardlink("a", "d")}, wantErr: "earlier in the archive"},
		{name: "character device", entries: []testEntry{{name: "null", typ: tar.TypeChar}}, wantErr: "device node"},
		{name: "block device", entries: []testEntry{{name: "sda", typ: tar.TypeBlock}}, wantErr: "device node"},
		{name: "fifo", entries: []testEntry{{name: "pipe", typ: tar.TypeFifo}}, wantErr: "FIFO"},
		{name: "zip fifo", zip: true, entries: []testEntry{{name: "pipe", typ: tar.TypeFifo}}, wantErr: "FIFO"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var data []byte
			if tt.zip {
				data = makeZip(t, tt.entries...)
			} else {
				data = gzipBytes(t, makeTar(t, tt.entries...))
			}
			_, err := walkArchive(writeTestArchive(t, data), testLimits, nil)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestWalkArchive_allowedSymlinks(t *testing.T) {
	// Links that stay inside the destination, including through other links.
	f := writeTestArchive(t, makeTar(t,
		arDir("usr/lib64/"),
		arSymlink("lib", "usr/lib64"),
		arSymlink("usr/lib", "lib64"),
		arSymlink("usr/bin/python", "../lib/python3"),
		arSymlink("self", "."),
		arSymlink("a/b/c", "../../lib/.."),
		arSymlink("dangling", "does/not/exist"),
		arSymlink("via", "lib/../usr"),
	))
	if _, err := walkArchive(f, testLimits, nil); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizeArchiveName(t *testing.T) {
	for _, tt := range []struct {
		raw   string
		strip int
		want  string
		ok    bool
		err   bool
	}{
		{raw: "a/b", want: "a/b", ok: true},
		{raw: "./a//b/", want: "a/b", ok: true},
		{raw: "./", ok: false},
		{raw: ".", ok: false},
		{raw: "a/b", strip: 1, want: "b", ok: true},
		{raw: "a/", strip: 1, ok: false},
		{raw: "a/b", strip: 2, ok: false},
		{raw: "", err: true},
		{raw: "/a", err: true},
		{raw: "a/../b", err: true},
		{raw: "..", err: true},
		{raw: "a/\x00b", err: true},
	} {
		got, ok, err := normalizeArchiveName(tt.raw, tt.strip)
		if (err != nil) != tt.err || ok != tt.ok || got != tt.want {
			t.Errorf("normalizeArchiveName(%q, %d) = %q, %v, %v; want %q, %v, error %v", tt.raw, tt.strip, got, ok, err, tt.want, tt.ok, tt.err)
		}
	}
}

// TestWalkArchive_decompressionBomb checks that archives whose contents are
// larger than max_size are refused, however small they are compressed.
func TestWalkArchive_decompressionBomb(t *testing.T) {
	zeros := strings.Repeat("\x00", 8<<20)
	lim := archiveLimits{maxSize: 1 << 20, maxEntries: 1000}

	t.Run("tar.gz", func(t *testing.T) {
		data := gzipBytes(t, makeTar(t, arFile("bomb", zeros)))
		if len(data) > 64<<10 {
			t.Fatalf("the test bomb is %d bytes compressed", len(data))
		}
		_, err := walkArchive(writeTestArchive(t, data), lim, nil)
		if err == nil || !strings.Contains(err.Error(), "max_size") {
			t.Fatalf("err = %v, want a max_size error", err)
		}
	})
	t.Run("tar.xz", func(t *testing.T) {
		data := xzBytes(t, makeTar(t, arFile("bomb", zeros)))
		_, err := walkArchive(writeTestArchive(t, data), lim, nil)
		if err == nil || !strings.Contains(err.Error(), "max_size") {
			t.Fatalf("err = %v, want a max_size error", err)
		}
	})
	t.Run("many small files", func(t *testing.T) {
		var entries []testEntry
		for i := range 5 {
			entries = append(entries, arFile("f"+string(rune('a'+i)), strings.Repeat("x", 300<<10)))
		}
		_, err := walkArchive(writeTestArchive(t, gzipBytes(t, makeTar(t, entries...))), lim, nil)
		if err == nil || !strings.Contains(err.Error(), "max_size") {
			t.Fatalf("err = %v, want a max_size error", err)
		}
	})
	t.Run("zip", func(t *testing.T) {
		data := makeZip(t, arFile("bomb", zeros))
		_, err := walkArchive(writeTestArchive(t, data), lim, nil)
		if err == nil || !strings.Contains(err.Error(), "max_size") {
			t.Fatalf("err = %v, want a max_size error", err)
		}
	})
	t.Run("zip with a lying size", func(t *testing.T) {
		// The header claims 10 bytes; the data decompresses to 8 MiB.
		var compressed bytes.Buffer
		fw, _ := flate.NewWriter(&compressed, flate.BestCompression)
		_, _ = io.WriteString(fw, zeros)
		_ = fw.Close()
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		w, err := zw.CreateRaw(&zip.FileHeader{
			Name: "bomb", Method: zip.Deflate,
			CRC32:              crc32.ChecksumIEEE([]byte(zeros[:10])),
			CompressedSize64:   uint64(compressed.Len()),
			UncompressedSize64: 10,
		})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(compressed.Bytes())
		_ = zw.Close()
		f := writeTestArchive(t, buf.Bytes())
		var read int64
		_, err = walkArchive(f, lim, func(_ *archiveEntry, content io.Reader) error {
			n, err := io.Copy(io.Discard, content)
			read += n
			return err
		})
		if err == nil || read > 10 {
			t.Fatalf("err = %v after reading %d bytes, want an error after at most 10", err, read)
		}
	})
	t.Run("max_entries", func(t *testing.T) {
		var entries []testEntry
		for i := range 20 {
			entries = append(entries, arFile(strings.Repeat("f", i+1), ""))
		}
		for name, data := range map[string][]byte{"tar": makeTar(t, entries...), "zip": makeZip(t, entries...)} {
			_, err := walkArchive(writeTestArchive(t, data), archiveLimits{maxSize: 1 << 20, maxEntries: 10}, nil)
			if err == nil || !strings.Contains(err.Error(), "max_entries") {
				t.Errorf("%s: err = %v, want a max_entries error", name, err)
			}
		}
	})
}

func TestLimitErrReader(t *testing.T) {
	r := &limitErrReader{r: strings.NewReader(strings.Repeat("x", 100)), n: 10, err: io.ErrShortBuffer}
	n, err := io.Copy(io.Discard, r)
	if err != io.ErrShortBuffer || n > 10 {
		t.Fatalf("read %d bytes, err %v", n, err)
	}
	r = &limitErrReader{r: strings.NewReader(strings.Repeat("x", 10)), n: 10, err: io.ErrShortBuffer}
	if n, err := io.Copy(io.Discard, r); err != nil || n != 10 {
		t.Fatalf("read %d bytes, err %v", n, err)
	}
}

func TestExactReader(t *testing.T) {
	if _, err := io.ReadAll(&exactReader{r: strings.NewReader("abc"), name: "x", left: 5}); err == nil {
		t.Error("truncated entry: expected an error")
	}
	if _, err := io.ReadAll(&exactReader{r: strings.NewReader("abcdef"), name: "x", left: 5}); err == nil {
		t.Error("oversized entry: expected an error")
	}
	if b, err := io.ReadAll(&exactReader{r: strings.NewReader("abcde"), name: "x", left: 5}); err != nil || string(b) != "abcde" {
		t.Errorf("got %q, %v", b, err)
	}
}
