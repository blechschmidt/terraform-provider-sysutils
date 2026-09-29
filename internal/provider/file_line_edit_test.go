package provider

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestTextFileRoundTrip(t *testing.T) {
	for _, in := range []string{"", "a", "a\n", "a\nb", "a\nb\n", "\n", "\n\n", "a\n\nb\n"} {
		if got := string(parseTextFile([]byte(in)).bytes()); got != in {
			t.Errorf("round trip of %q = %q", in, got)
		}
	}
}

func mustSpec(t *testing.T, c fragmentConfig) *fragmentSpec {
	t.Helper()
	s, err := newFragmentSpec(c)
	if err != nil {
		t.Fatalf("newFragmentSpec(%+v): %v", c, err)
	}
	return s
}

func TestFragmentEnsure(t *testing.T) {
	cases := []struct {
		name string
		cfg  fragmentConfig
		in   string
		want string
	}{
		{"line appended", fragmentConfig{line: "c"}, "a\nb\n", "a\nb\nc\n"},
		{"line appended without trailing newline", fragmentConfig{line: "c"}, "a\nb", "a\nb\nc\n"},
		{"line into empty file", fragmentConfig{line: "c"}, "", "c\n"},
		{"line present", fragmentConfig{line: "b"}, "a\nb\nc", "a\nb\nc"},
		{"regexp replaces last match", fragmentConfig{line: "Port 2222", regexp: `^#?Port `},
			"Port 22\nX\n#Port 23\nY\n", "Port 22\nX\nPort 2222\nY\n"},
		{"regexp without match inserts", fragmentConfig{line: "Port 2222", regexp: `^Port `, insertBefore: "BOF"},
			"X\n", "Port 2222\nX\n"},
		{"regexp match equal to line", fragmentConfig{line: "Port 22", regexp: `^Port `}, "Port 22\n", "Port 22\n"},
		{"insert_before BOF", fragmentConfig{line: "z", insertBefore: "BOF"}, "a\nb\n", "z\na\nb\n"},
		{"insert_before regexp", fragmentConfig{line: "z", insertBefore: "^b"}, "a\nb\nb\n", "a\nb\nz\nb\n"},
		{"insert_before unmatched", fragmentConfig{line: "z", insertBefore: "^q"}, "a\n", "a\nz\n"},
		{"insert_after regexp", fragmentConfig{line: "z", insertAfter: "^a"}, "a\na\nb", "a\na\nz\nb"},
		{"insert_after EOF", fragmentConfig{line: "z", insertAfter: "EOF"}, "a\n", "a\nz\n"},
		{"block inserted", fragmentConfig{isBlock: true, block: "x\ny\n"}, "a\n",
			"a\n# BEGIN MANAGED BY TERRAFORM\nx\ny\n# END MANAGED BY TERRAFORM\n"},
		{"block replaced in place", fragmentConfig{isBlock: true, block: "new", marker: "## {mark} m"},
			"a\n## BEGIN m\nold\nold2\n## END m\nb\n", "a\n## BEGIN m\nnew\n## END m\nb\n"},
		{"block present", fragmentConfig{isBlock: true, block: "x\n", marker: "{mark}"},
			"BEGIN\nx\nEND\n", "BEGIN\nx\nEND\n"},
		{"empty block", fragmentConfig{isBlock: true, block: "", marker: "{mark}"}, "a", "a\nBEGIN\nEND\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := mustSpec(t, c.cfg)
			text := parseTextFile([]byte(c.in))
			changed, err := s.ensure(text, -1)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(text.bytes()); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
			if changed != (c.in != c.want) {
				t.Errorf("changed = %v", changed)
			}
			// Applying again must be a no-op.
			if again, _ := s.ensure(text, -1); again {
				t.Errorf("second ensure changed the file to %q", text.bytes())
			}
		})
	}
}

func TestFragmentInspect(t *testing.T) {
	s := mustSpec(t, fragmentConfig{line: "PermitRootLogin no", regexp: `^#?PermitRootLogin`})
	status, _, _, actual, err := s.inspect([]string{"PermitRootLogin no", "#PermitRootLogin yes"})
	if err != nil || status != fragmentDiffers || actual[0] != "#PermitRootLogin yes" {
		t.Errorf("last regexp match should decide: status=%v actual=%q err=%v", status, actual, err)
	}

	b := mustSpec(t, fragmentConfig{isBlock: true, block: "x\n", marker: "# {mark}"})
	status, start, end, actual, err := b.inspect([]string{"a", "# BEGIN", "y", "# END"})
	if err != nil || status != fragmentDiffers || start != 1 || end != 4 || strings.Join(actual, ",") != "y" {
		t.Errorf("block: status=%v [%d,%d) actual=%q err=%v", status, start, end, actual, err)
	}
	if _, _, _, _, err := b.inspect([]string{"# BEGIN", "x"}); !errors.Is(err, errUnterminatedBlock) {
		t.Errorf("unterminated block: err = %v", err)
	}
	if status, _, _, _, _ := b.inspect([]string{"# END", "x"}); status != fragmentAbsent {
		t.Errorf("END without BEGIN: status = %v", status)
	}
}

func TestFragmentRemove(t *testing.T) {
	cases := []struct {
		name   string
		cfg    fragmentConfig
		in     string
		want   string
		wantAt int
	}{
		{"line", fragmentConfig{line: "b"}, "a\nb\nc\nb\n", "a\nc\n", 1},
		{"line absent", fragmentConfig{line: "q"}, "a\n", "a\n", -1},
		{"only line", fragmentConfig{line: "a"}, "a\n", "", 0},
		{"regexp ignored", fragmentConfig{line: "b", regexp: "."}, "a\nb\n", "a\n", 1},
		{"block", fragmentConfig{isBlock: true, marker: "<{mark}>"}, "a\n<BEGIN>\nx\n<END>\nb\n", "a\nb\n", 1},
		{"block absent", fragmentConfig{isBlock: true, marker: "<{mark}>"}, "a\n", "a\n", -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text := parseTextFile([]byte(c.in))
			changed, at, err := mustSpec(t, c.cfg).remove(text)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(text.bytes()); got != c.want || at != c.wantAt || changed != (c.in != c.want) {
				t.Errorf("got %q at %d (changed %v), want %q at %d", got, at, changed, c.want, c.wantAt)
			}
		})
	}

	text := parseTextFile([]byte("<BEGIN>\nx\n"))
	if _, _, err := mustSpec(t, fragmentConfig{isBlock: true, marker: "<{mark}>"}).remove(text); !errors.Is(err, errUnterminatedBlock) {
		t.Errorf("unterminated block: err = %v", err)
	}
}

func TestFragmentReplaceAtHint(t *testing.T) {
	// An update that changes the line removes the old one and puts the new
	// one in its place rather than at the end.
	old := mustSpec(t, fragmentConfig{line: "10.0.0.1 db"})
	desired := mustSpec(t, fragmentConfig{line: "10.0.0.2 db"})
	text := parseTextFile([]byte("127.0.0.1 localhost\n10.0.0.1 db\n::1 localhost\n"))
	_, at, err := old.remove(text)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := desired.ensure(text, at); err != nil {
		t.Fatal(err)
	}
	if got, want := string(text.bytes()), "127.0.0.1 localhost\n10.0.0.2 db\n::1 localhost\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestNewFragmentSpecErrors(t *testing.T) {
	cases := []struct {
		cfg     fragmentConfig
		wantErr string
	}{
		{fragmentConfig{line: "a\nb"}, "line breaks"},
		{fragmentConfig{line: "a", regexp: "("}, "invalid regexp"},
		{fragmentConfig{line: "a", insertAfter: "("}, "insert_after"},
		{fragmentConfig{line: "a", insertBefore: "["}, "insert_before"},
		{fragmentConfig{isBlock: true, marker: "no placeholder"}, "{mark}"},
		{fragmentConfig{isBlock: true, marker: "{mark}\nx"}, "single line"},
		{fragmentConfig{isBlock: true, block: "x\n# END MANAGED BY TERRAFORM\n"}, "marker line"},
	}
	for _, c := range cases {
		_, err := newFragmentSpec(c.cfg)
		if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("newFragmentSpec(%+v) = %v, want error containing %q", c.cfg, err, c.wantErr)
		}
	}
}

func TestJoinSplitBlock(t *testing.T) {
	for _, s := range []string{"", "a", "a\n", "a\nb\n", "\n", "a\n\n"} {
		if got := joinBlock(splitBlock(s), strings.HasSuffix(s, "\n")); got != s {
			t.Errorf("joinBlock(splitBlock(%q)) = %q", s, got)
		}
	}
}

func TestReplaceFileAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o640); err != nil {
		t.Fatal(err)
	}
	data, snap, err := readRegularFileNoFollow(p, 1024)
	if err != nil || string(data) != "old\n" {
		t.Fatalf("read: %q, %v", data, err)
	}
	if err := replaceFileAtomic(p, []byte("new\n"), snap, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p)
	if err != nil || string(got) != "new\n" {
		t.Fatalf("content %q, %v", got, err)
	}
	info, err := os.Stat(p)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Errorf("mode not preserved: %v, %v", info.Mode(), err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("temporary file left behind: %v", entries)
	}

	// A write based on a stale snapshot is refused and leaves the file alone.
	if err := os.WriteFile(p, []byte("concurrent edit\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := replaceFileAtomic(p, []byte("lost\n"), snap, 0o644); !errors.Is(err, errFileChangedConcurrently) {
		t.Errorf("stale snapshot: err = %v", err)
	}
	if got, _ := os.ReadFile(p); string(got) != "concurrent edit\n" {
		t.Errorf("file overwritten despite concurrent change: %q", got)
	}

	// Creating a new file must not clobber one created in the meantime.
	q := filepath.Join(dir, "new")
	if err := replaceFileAtomic(q, []byte("x\n"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(q); err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("created file: %v, %v", info, err)
	}
	if err := replaceFileAtomic(q, []byte("y\n"), nil, 0o644); !errors.Is(err, errFileChangedConcurrently) {
		t.Errorf("create over existing file: err = %v", err)
	}
	entries, _ = os.ReadDir(dir)
	if len(entries) != 2 {
		t.Errorf("temporary files left behind: %v", entries)
	}
}

func TestReadRegularFileNoFollow(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	mustWrite(t, victim, "secret\n")
	link := filepath.Join(dir, "link")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readRegularFileNoFollow(link, 1024); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("symlink: err = %v", err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readRegularFileNoFollow(fifo, 1024); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("fifo: err = %v", err)
	}
	if _, _, err := readRegularFileNoFollow(victim, 3); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("size limit: err = %v", err)
	}
	if _, _, err := readRegularFileNoFollow(filepath.Join(dir, "missing"), 3); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing: err = %v", err)
	}
}
