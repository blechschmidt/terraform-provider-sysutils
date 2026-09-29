package provider

// Regression tests for the security review of sysutils_service,
// sysutils_archive_extract and sysutils_ssh_authorized_key.

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"golang.org/x/sys/unix"
)

// A directory of the destination that an archive is merged into may be
// swapped for a symlink or a hard link after planMerge checked it. Setting
// the archive's attributes on it must not change the file it now leads to.
func TestReview49_applyEntryAttrsRefusesSwappedEntry(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "viasymlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(secret, filepath.Join(dir, "viahardlink")); err != nil {
		t.Fatal(err)
	}
	dfd, err := unix.Open(dir, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(dfd) }()

	mode := fs.FileMode(0o755)
	spec := extractSpec{uid: -1, gid: -1, dirMode: &mode}
	for _, name := range []string{"viasymlink", "viahardlink"} {
		e := &archiveEntry{Name: name, Kind: archiveDir, Mode: 0o755, ModTime: testModTime}
		if err := applyEntryAttrs(dfd, name, e, spec); err == nil {
			t.Errorf("applyEntryAttrs(%s) = nil, want an error", name)
		}
		info, err := os.Stat(secret)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("after applyEntryAttrs(%s), %s has mode %v, want 0600", name, secret, info.Mode().Perm())
		}
	}
}

// Directories that the archive does not list count towards max_entries,
// and names are capped at PATH_MAX: otherwise one entry with a deeply nested
// name creates thousands of directories and a manifest of quadratic size.
func TestReview49_implicitDirectoriesCountAsEntries(t *testing.T) {
	deep := makeTar(t, arFile("a/b/c/d/e/f/g/h/i/j/file", "x"))
	if _, err := walkArchive(writeTestArchive(t, deep), archiveLimits{maxSize: 1 << 20, maxEntries: 5}, nil); err == nil ||
		!strings.Contains(err.Error(), "max_entries") {
		t.Errorf("walkArchive with 1 file in 10 unlisted directories, max_entries 5: err = %v, want a max_entries error", err)
	}
	// Within the limit, the same archive is fine.
	m, err := walkArchive(writeTestArchive(t, deep), archiveLimits{maxSize: 1 << 20, maxEntries: 11}, nil)
	if err != nil {
		t.Fatalf("walkArchive with max_entries 11: %v", err)
	}
	if n := len(m.entries); n != 11 {
		t.Errorf("manifest has %d entries, want 11", n)
	}

	long := makeTar(t, arFile(strings.Repeat("d/", 2500)+"file", "x"))
	if _, err := walkArchive(writeTestArchive(t, long), archiveLimits{maxSize: 1 << 20, maxEntries: 1 << 20}, nil); err == nil {
		t.Error("walkArchive accepted an entry name of 5004 bytes")
	}
}

// Checking where the archive's symlinks lead must not take unbounded time:
// each step looks up the path resolved so far, so many symlinks with long
// targets into a deep directory used to cost hours of CPU time per plan.
func TestReview49_symlinkCheckIsBounded(t *testing.T) {
	chain := strings.Repeat("d/", 1000)
	entries := []testEntry{arFile(chain+"file", "x")}
	for i := 0; i < 600; i++ {
		entries = append(entries, arSymlink(fmt.Sprintf("l%d", i), chain+"file"))
	}
	f := writeTestArchive(t, makeTar(t, entries...))
	start := time.Now()
	_, err := walkArchive(f, archiveLimits{maxSize: 1 << 20, maxEntries: 1 << 20}, nil)
	if err == nil || !strings.Contains(err.Error(), "too complex") {
		t.Errorf("walkArchive = %v after %s, want a \"too complex\" error", err, time.Since(start))
	}
}

// Keys are added to files with CRLF line endings with CRLF, and a managed
// line that ends with CRLF is left alone.
func TestReview49_authorizedKeysCRLF(t *testing.T) {
	a, b, c := testSSHKey(t, 0), testSSHKey(t, 1), testSSHKey(t, 2)
	at, bt, ct := testSSHKeyText(a), testSSHKeyText(b), testSSHKeyText(c)
	want := at + " managed"
	for _, tc := range []struct {
		name, in, out string
		changed       bool
	}{
		{"appended", "# keys\r\n" + bt + "\r\n", "# keys\r\n" + bt + "\r\n" + want + "\r\n", true},
		{"no final newline", bt + "\r\n" + ct, bt + "\r\n" + ct + "\r\n" + want + "\r\n", true},
		{"present", bt + "\r\n" + want + "\r\n", bt + "\r\n" + want + "\r\n", false},
		{"replaced in place", at + " old\r\n" + bt + "\r\n", want + "\r\n" + bt + "\r\n", true},
		{"LF file", bt + "\n", bt + "\n" + want + "\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := parseTextFile([]byte(tc.in))
			changed := ensureAuthorizedKey(text, matchSSHKey(a), want, -1)
			if got := string(text.bytes()); got != tc.out || changed != tc.changed {
				t.Errorf("got %q (changed %v), want %q (changed %v)", got, changed, tc.out, tc.changed)
			}
		})
	}
}

// A CRLF authorized_keys file that already holds the managed line neither
// plans a change nor is rewritten.
func TestReview49_authorizedKeysCRLFResource(t *testing.T) {
	home := t.TempDir()
	at := testSSHKeyText(testSSHKey(t, 0))
	other := testSSHKeyText(testSSHKey(t, 1)) + " unmanaged"
	content := "# keys\r\n" + at + " alice\r\n" + other + "\r\n"
	writeAuthorizedKeysFile(t, home, content)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: sshKeyProviderFactories(home),
		CheckDestroy:             checkAuthorizedKeys(home, "# keys\r\n"+other+"\r\n"),
		Steps: []resource.TestStep{
			{
				Config: sshKeyHCL(fmt.Sprintf(`  key = %q`, at+" alice")),
				Check:  checkAuthorizedKeys(home, content),
			},
			{
				Config: sshKeyHCL(fmt.Sprintf(`  key = %q`, at+" alice")),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: checkAuthorizedKeys(home, content),
			},
		},
	})
}

// systemctl cannot query a template unit itself; such names are refused
// with a clear message instead of failing with systemctl's.
func TestReview49_serviceTemplateNames(t *testing.T) {
	for _, bad := range []string{"getty@.service", "getty@", "container-getty@.service"} {
		if err := validateServiceNameFor(initSystemSystemd, bad); err == nil || !strings.Contains(err.Error(), "template") {
			t.Errorf("validateServiceNameFor(systemd, %q) = %v, want a template error", bad, err)
		}
	}
	for _, ok := range []string{"getty@tty1.service", "getty@tty1", "user@1000.service", `systemd-fsck@dev-disk-by\x2dlabel-root.service`} {
		if err := validateServiceNameFor(initSystemSystemd, ok); err != nil {
			t.Errorf("validateServiceNameFor(systemd, %q) = %v, want nil", ok, err)
		}
	}
}
