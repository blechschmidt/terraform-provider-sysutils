package provider

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"golang.org/x/sys/unix"
)

// readOSReleaseWithin runs readOSRelease, failing the test instead of
// hanging if it blocks.
func readOSReleaseWithin(t *testing.T, root *fsRoot) (osRelease, error) {
	t.Helper()
	type result struct {
		r   osRelease
		err error
	}
	done := make(chan result, 1)
	go func() {
		r, _, err := readOSRelease(root)
		done <- result{r, err}
	}()
	select {
	case res := <-done:
		return res.r, res.err
	case <-time.After(10 * time.Second):
		t.Fatal("readOSRelease blocked")
		return nil, nil
	}
}

// TestReadOSReleaseFIFO checks that a FIFO planted at /etc/os-release in
// the tree is refused rather than blocking the provider forever.
func TestReadOSReleaseFIFO(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "etc", "os-release")
	if err := unix.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	// Keep a writer from ever appearing: nothing opens the FIFO for
	// writing, so a blocking open would never return.
	root, err := newFSRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = readOSReleaseWithin(t, root)
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("readOSRelease error = %v, want \"not a regular file\"", err)
	}
	// Unblock a reader of the old code, if any, so the test binary exits.
	if f, err := os.OpenFile(fifo, os.O_WRONLY|unix.O_NONBLOCK, 0); err == nil {
		_ = f.Close()
	}
}

// TestReadOSReleaseDirectorySwap checks that a directory of the tree that
// is swapped for a symlink out of it between resolving and opening
// os-release is not followed.
func TestReadOSReleaseDirectorySwap(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"root/etc/os-release": "ID=tree\n",
		"outside/os-release":  "ID=outside\nSECRET=leaked\n",
	})
	root, err := newFSRoot(filepath.Join(dir, "root"))
	if err != nil {
		t.Fatal(err)
	}
	testHookRootedFileResolved = func(string) {
		etc := filepath.Join(dir, "root", "etc")
		if err := os.Rename(etc, etc+".old"); err != nil {
			t.Error(err)
		}
		if err := os.Symlink(filepath.Join(dir, "outside"), etc); err != nil {
			t.Error(err)
		}
	}
	defer func() { testHookRootedFileResolved = nil }()
	r, err := readOSReleaseWithin(t, root)
	if err == nil {
		t.Fatalf("readOSRelease read %v through a swapped directory", r)
	}
	if !strings.Contains(err.Error(), "changed while it was being read") {
		t.Errorf("readOSRelease error = %v, want \"changed while it was being read\"", err)
	}
}

// TestHostDataSource_noMeminfo checks that a host without a readable
// /proc/meminfo, as a container without procfs, is a warning with
// memory_total_bytes null, not an error that hides every other fact.
func TestHostDataSource_noMeminfo(t *testing.T) {
	h := newFakeHost(t, map[string]string{"etc/os-release": fixture(t, "alpine-3.21")})
	if err := os.Remove(filepath.Join(h.dir, "proc", "meminfo")); err != nil {
		t.Fatal(err)
	}
	const ds = "data.sysutils_host.this"
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: h.providerFactories(),
		Steps: []resource.TestStep{{
			Config: fmt.Sprintf(`
provider "sysutils" {
  root_dir = %q
}

data "sysutils_host" "this" {}
`, h.root),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckNoResourceAttr(ds, "memory_total_bytes"),
				resource.TestCheckResourceAttr(ds, "os_id", "alpine"),
				resource.TestCheckResourceAttr(ds, "kernel_release", "6.1.0-99-fake"),
			),
		}},
	})
}

// TestCollectHostFactsWarnings checks the warnings' summaries: each names
// the fact that is missing.
func TestCollectHostFactsWarnings(t *testing.T) {
	dir := t.TempDir()
	root, err := newFSRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, warnings, err := collectHostFacts(t.Context(), hostFactSources{
		root:     root,
		host:     &hostConfig{procDir: filepath.Join(dir, "noproc"), hostsFile: filepath.Join(dir, "nohosts")},
		service:  &serviceConfig{runDir: filepath.Join(dir, "run"), procDir: filepath.Join(dir, "noproc")},
		firewall: &firewallConfig{lookPath: func(n string) (string, error) { return "", fmt.Errorf("%s: not found", n) }},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, w := range warnings {
		got = append(got, w.summary+": "+w.detail)
	}
	joined := strings.Join(got, "\n")
	for _, want := range []*regexp.Regexp{
		regexp.MustCompile(`(?m)^Cannot read memory size: .*meminfo.*memory_total_bytes is null\.$`),
		regexp.MustCompile(`(?m)^No os-release file: .*the os_\* attributes are null\.$`),
	} {
		if !want.MatchString(joined) {
			t.Errorf("warnings:\n%s\nwant a match for %s", joined, want)
		}
	}
}
