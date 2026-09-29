package provider

import (
	"archive/tar"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

var testSpec = extractSpec{limits: testLimits, uid: -1, gid: -1}

// listTree returns every path below dir, relative to it, with a trailing
// slash on directories.
func listTree(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dir {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if d.IsDir() {
			rel += "/"
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

func readTestFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInstallArchive_newDestination(t *testing.T) {
	parent := t.TempDir()
	dest := filepath.Join(parent, "a", "app")
	f := writeTestArchive(t, gzipBytes(t, makeTar(t,
		arDir("bin/"),
		testEntry{name: "bin/tool", typ: tar.TypeReg, body: "#!/bin/sh\n", mode: 0o4755},
		testEntry{name: "secret", typ: tar.TypeReg, body: "s", mode: 0o600},
		testEntry{name: "shared", typ: tar.TypeReg, body: "w", mode: 0o666},
		testEntry{name: "tmp/", typ: tar.TypeDir, mode: 0o1777},
		arFile("share/doc/README", "readme"),
		arSymlink("current", "bin/tool"),
		arHardlink("bin/tool2", "bin/tool"),
	)))
	res, err := installArchive(context.Background(), f, dest, testSpec, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.created || !res.placed {
		t.Errorf("created = %v, placed = %v; want both", res.created, res.placed)
	}
	want := []string{"bin/", "bin/tool", "bin/tool2", "current", "secret", "share/", "share/doc/", "share/doc/README", "shared", "tmp/"}
	if got := res.manifest.fileList(); !slices.Equal(got, want) {
		t.Errorf("manifest = %q, want %q", got, want)
	}
	if got := listTree(t, dest); !slices.Equal(got, want) {
		t.Errorf("tree = %q, want %q", got, want)
	}
	// Nothing else is left next to the destination.
	if got := listTree(t, filepath.Join(parent, "a")); len(got) != len(want)+1 {
		t.Errorf("parent contains %q", got)
	}
	if got := readTestFile(t, filepath.Join(dest, "share/doc/README")); got != "readme" {
		t.Errorf("README = %q", got)
	}
	for p, mode := range map[string]fs.FileMode{
		"":                 0o755,
		"bin":              0o755,
		"bin/tool":         0o755, // The setuid bit is dropped.
		"secret":           0o600,
		"shared":           0o644, // No write permission for others.
		"tmp":              0o755,
		"share":            0o755, // Implicit.
		"share/doc/README": 0o644,
	} {
		info, err := os.Lstat(filepath.Join(dest, p))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky); got != mode {
			t.Errorf("mode of %q = %v, want %v", p, got, mode)
		}
	}
	if target, err := os.Readlink(filepath.Join(dest, "current")); err != nil || target != "bin/tool" {
		t.Errorf("current -> %q, %v", target, err)
	}
	a, _ := os.Stat(filepath.Join(dest, "bin/tool"))
	b, _ := os.Stat(filepath.Join(dest, "bin/tool2"))
	if !os.SameFile(a, b) {
		t.Error("bin/tool2 is not a hard link to bin/tool")
	}
	if info, _ := os.Stat(filepath.Join(dest, "secret")); !info.ModTime().Equal(testModTime) {
		t.Errorf("mtime of secret = %v, want %v", info.ModTime(), testModTime)
	}
}

func TestInstallArchive_modes(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "app")
	f := writeTestArchive(t, makeZip(t,
		testEntry{name: "run", typ: tar.TypeReg, body: "x", mode: 0o755},
		testEntry{name: "d/", typ: tar.TypeDir},
		arFile("d/f", "y"),
	))
	fileMode, dirMode := fs.FileMode(0o640), fs.FileMode(0o750)
	spec := testSpec
	spec.fileMode, spec.dirMode = &fileMode, &dirMode
	if _, err := installArchive(context.Background(), f, dest, spec, nil, false); err != nil {
		t.Fatal(err)
	}
	for p, mode := range map[string]fs.FileMode{"": 0o750, "run": 0o640, "d": 0o750, "d/f": 0o640} {
		info, err := os.Stat(filepath.Join(dest, p))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != mode {
			t.Errorf("mode of %q = %v, want %v", p, got, mode)
		}
	}
}

// TestInstallArchive_maliciousLeavesNothing checks that an archive that is
// refused because of its last entry leaves no trace: its valid entries are
// never moved into the destination, and the temporary directory is gone.
func TestInstallArchive_maliciousLeavesNothing(t *testing.T) {
	for name, data := range map[string][]byte{
		"zip-slip":       makeZip(t, arFile("good", "x"), arFile("../evil", "x")),
		"symlink escape": gzipBytes(t, makeTar(t, arFile("good", "x"), arSymlink("sub/up", ".."), arSymlink("sub/x", "up/.."))),
		"too large":      gzipBytes(t, makeTar(t, arFile("good", "x"), arFile("big", strings.Repeat("x", 2<<20)))),
	} {
		t.Run(name, func(t *testing.T) {
			for _, existing := range []bool{false, true} {
				parent := t.TempDir()
				dest := filepath.Join(parent, "dest")
				if existing {
					if err := os.Mkdir(dest, 0o755); err != nil {
						t.Fatal(err)
					}
				}
				res, err := installArchive(context.Background(), writeTestArchive(t, data), dest, testSpec, nil, false)
				if err == nil || res.placed {
					t.Fatalf("existing=%v: err = %v, placed = %v; want an error before anything is placed", existing, err, res.placed)
				}
				want := []string(nil)
				if existing {
					want = []string{"dest/"}
				}
				if got := listTree(t, parent); !slices.Equal(got, want) {
					t.Errorf("existing=%v: left behind %q", existing, got)
				}
				if _, err := os.Lstat(filepath.Join(filepath.Dir(parent), "evil")); err == nil {
					t.Error("the zip-slip entry was written")
				}
			}
		})
	}
}

func TestInstallArchive_merge(t *testing.T) {
	dest := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dest, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "bin", "other"), []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "conf"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	data := makeTar(t, arFile("bin/tool", "tool"), arFile("conf", "archive"), arFile("new/file", "n"))
	ctx := context.Background()

	// A file that exists already and was not extracted is a conflict, and
	// nothing is changed.
	_, err := installArchive(ctx, writeTestArchive(t, data), dest, testSpec, nil, false)
	if err == nil || !strings.Contains(err.Error(), "overwrite") {
		t.Fatalf("err = %v, want a conflict", err)
	}
	if got := listTree(t, dest); !slices.Equal(got, []string{"bin/", "bin/other", "conf"}) {
		t.Errorf("tree after conflict = %q", got)
	}

	// With overwrite, it is replaced; other files stay.
	res, err := installArchive(ctx, writeTestArchive(t, data), dest, testSpec, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.created {
		t.Error("created is set for an existing destination")
	}
	if got := listTree(t, dest); !slices.Equal(got, []string{"bin/", "bin/other", "bin/tool", "conf", "new/", "new/file"}) {
		t.Errorf("tree = %q", got)
	}
	if got := readTestFile(t, filepath.Join(dest, "conf")); got != "archive" {
		t.Errorf("conf = %q", got)
	}
	// The implicit directory bin existed already and keeps its mode.
	if info, _ := os.Stat(filepath.Join(dest, "bin")); info.Mode().Perm() != 0o700 {
		t.Errorf("mode of bin = %v, want 0700", info.Mode().Perm())
	}

	// A second extraction replaces the files it owns without overwrite,
	// and removing it leaves the other files and non-empty directories.
	data2 := makeTar(t, arFile("bin/tool", "tool v2"), arFile("conf", "archive v2"))
	res2, err := installArchive(ctx, writeTestArchive(t, data2), dest, testSpec, manifestNamesFrom(res.manifest.fileList()), false)
	if err != nil {
		t.Fatal(err)
	}
	stale := staleEntries(res.manifest.fileList(), res2.manifest)
	if !slices.Equal(stale, []string{"new/", "new/file"}) {
		t.Errorf("stale = %q", stale)
	}
	if warnings, err := removeExtracted(dest, stale); err != nil || len(warnings) != 0 {
		t.Fatalf("removeExtracted: %v, %q", err, warnings)
	}
	if got := listTree(t, dest); !slices.Equal(got, []string{"bin/", "bin/other", "bin/tool", "conf"}) {
		t.Errorf("tree after update = %q", got)
	}
	if got := readTestFile(t, filepath.Join(dest, "bin/tool")); got != "tool v2" {
		t.Errorf("bin/tool = %q", got)
	}
	if warnings, err := removeExtracted(dest, res2.manifest.fileList()); err != nil || len(warnings) != 0 {
		t.Fatalf("removeExtracted: %v, %q", err, warnings)
	}
	if got := listTree(t, dest); !slices.Equal(got, []string{"bin/", "bin/other"}) {
		t.Errorf("tree after removal = %q", got)
	}
}

// TestInstallArchive_plantedSymlinks checks that symlinks planted in an
// existing destination are replaced, never written through.
func TestInstallArchive_plantedSymlinks(t *testing.T) {
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim")
	if err := os.WriteFile(victim, []byte("untouched"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	// A symlink where the archive has a file, and one where it has a
	// directory.
	if err := os.Symlink(victim, filepath.Join(dest, "conf")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dest, "bin")); err != nil {
		t.Fatal(err)
	}
	data := makeTar(t, arFile("conf", "archive"), arFile("bin/victim", "archive"))
	ctx := context.Background()

	if _, err := installArchive(ctx, writeTestArchive(t, data), dest, testSpec, nil, false); err == nil {
		t.Fatal("expected a conflict")
	}
	res, err := installArchive(ctx, writeTestArchive(t, data), dest, testSpec, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, victim); got != "untouched" {
		t.Fatalf("the file outside the destination was changed to %q", got)
	}
	for _, p := range []string{"conf", "bin"} {
		if info, err := os.Lstat(filepath.Join(dest, p)); err != nil || info.Mode()&fs.ModeSymlink != 0 {
			t.Errorf("%s: %v, %v; want a real file or directory", p, info.Mode(), err)
		}
	}

	// Removal does not follow a symlink that replaced an extracted
	// directory either.
	if err := os.RemoveAll(filepath.Join(dest, "bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dest, "bin")); err != nil {
		t.Fatal(err)
	}
	warnings, err := removeExtracted(dest, res.manifest.fileList())
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) == 0 {
		t.Error("expected a warning for the replaced directory")
	}
	if got := readTestFile(t, victim); got != "untouched" {
		t.Fatalf("the file outside the destination was changed to %q", got)
	}
	if _, err := os.Lstat(filepath.Join(dest, "conf")); !os.IsNotExist(err) {
		t.Errorf("conf was not removed: %v", err)
	}
}

func TestInstallArchive_destinationNotADirectory(t *testing.T) {
	parent := t.TempDir()
	data := makeTar(t, arFile("f", "x"))
	link := filepath.Join(parent, "link")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	if _, err := installArchive(context.Background(), writeTestArchive(t, data), link, testSpec, nil, false); err == nil {
		t.Error("a symlink as destination was accepted")
	}
	regular := filepath.Join(parent, "file")
	if err := os.WriteFile(regular, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := installArchive(context.Background(), writeTestArchive(t, data), regular, testSpec, nil, false); err == nil {
		t.Error("a file as destination was accepted")
	}
}

func TestRemoveExtracted_invalidNames(t *testing.T) {
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim")
	if err := os.WriteFile(victim, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(outside, "dest")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	// Names that a tampered state could contain.
	if _, err := removeExtracted(dest, []string{"../victim", victim, "./../victim", "", "/"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("victim was removed: %v", err)
	}
}

func TestCountDrift(t *testing.T) {
	data := gzipBytes(t, makeTar(t,
		arDir("d/"),
		arFile("d/f", "content"),
		arFile("g", "other"),
		arSymlink("l", "d/f"),
		arHardlink("h", "d/f"),
		arFile("implicit/x", "x"),
	))
	fileMode := fs.FileMode(0o640)
	spec := testSpec
	spec.fileMode = &fileMode

	for _, tt := range []struct {
		name  string
		drift func(t *testing.T, dest string)
		want  int64
		// wantNoArchive is the drift found without the archive.
		wantNoArchive int64
	}{
		{name: "none", drift: func(*testing.T, string) {}},
		{name: "content", want: 1, drift: func(t *testing.T, dest string) {
			if err := os.WriteFile(filepath.Join(dest, "g"), []byte("OTHER"), 0o640); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "content of hard-linked file", want: 2, drift: func(t *testing.T, dest string) {
			if err := os.WriteFile(filepath.Join(dest, "d/f"), []byte("CONTENT"), 0o640); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "mode", want: 1, drift: func(t *testing.T, dest string) {
			if err := os.Chmod(filepath.Join(dest, "g"), 0o777); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "directory mode", want: 1, drift: func(t *testing.T, dest string) {
			if err := os.Chmod(filepath.Join(dest, "d"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "symlink target", want: 1, drift: func(t *testing.T, dest string) {
			_ = os.Remove(filepath.Join(dest, "l"))
			if err := os.Symlink("g", filepath.Join(dest, "l")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "removed", want: 1, wantNoArchive: 1, drift: func(t *testing.T, dest string) {
			if err := os.Remove(filepath.Join(dest, "g")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "removed directory", want: 2, wantNoArchive: 2, drift: func(t *testing.T, dest string) {
			if err := os.RemoveAll(filepath.Join(dest, "implicit")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "file replaced by symlink", want: 1, drift: func(t *testing.T, dest string) {
			_ = os.Remove(filepath.Join(dest, "g"))
			if err := os.Symlink("d/f", filepath.Join(dest, "g")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "directory replaced by symlink", want: 2, wantNoArchive: 2, drift: func(t *testing.T, dest string) {
			if err := os.RemoveAll(filepath.Join(dest, "d")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(t.TempDir(), filepath.Join(dest, "d")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "dest")
			f := writeTestArchive(t, data)
			res, err := installArchive(context.Background(), f, dest, spec, nil, false)
			if err != nil {
				t.Fatal(err)
			}
			tt.drift(t, dest)
			files := res.manifest.fileList()
			got, err := countDrift(context.Background(), dest, files, f, spec)
			if err != nil || got != tt.want {
				t.Errorf("countDrift with archive = %d, %v; want %d", got, err, tt.want)
			}
			got, err = countDrift(context.Background(), dest, files, nil, spec)
			if err != nil || got != tt.wantNoArchive {
				t.Errorf("countDrift without archive = %d, %v; want %d", got, err, tt.wantNoArchive)
			}
		})
	}
}

func TestInstallArchive_ownership(t *testing.T) {
	requireRoot(t)
	dest := filepath.Join(t.TempDir(), "app")
	f := writeTestArchive(t, makeTar(t, arFile("d/f", "x"), arSymlink("l", "d/f")))
	spec := testSpec
	spec.uid, spec.gid = 1234, 2345
	res, err := installArchive(context.Background(), f, dest, spec, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"", "d", "d/f", "l"} {
		info, err := os.Lstat(filepath.Join(dest, p))
		if err != nil {
			t.Fatal(err)
		}
		st := info.Sys().(*syscall.Stat_t)
		if st.Uid != 1234 || st.Gid != 2345 {
			t.Errorf("%q is owned by %d:%d, want 1234:2345", p, st.Uid, st.Gid)
		}
	}
	if n, err := countDrift(context.Background(), dest, res.manifest.fileList(), f, spec); err != nil || n != 0 {
		t.Errorf("countDrift = %d, %v", n, err)
	}
	if err := os.Lchown(filepath.Join(dest, "l"), 0, 0); err != nil {
		t.Fatal(err)
	}
	if n, err := countDrift(context.Background(), dest, res.manifest.fileList(), f, spec); err != nil || n != 1 {
		t.Errorf("countDrift after chown = %d, %v; want 1", n, err)
	}
}
