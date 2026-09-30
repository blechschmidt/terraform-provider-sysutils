package provider

import (
	"context"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// watchOpens returns a function that reports whether p has been opened
// since watchOpens was called, using inotify's IN_OPEN event.
func watchOpens(t *testing.T, p string) func() bool {
	t.Helper()
	fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	if _, err := unix.InotifyAddWatch(fd, p, unix.IN_OPEN); err != nil {
		t.Fatal(err)
	}
	return func() bool {
		buf := make([]byte, 4096)
		n, err := unix.Read(fd, buf)
		if err == unix.EAGAIN {
			return false
		}
		if err != nil {
			t.Fatal(err)
		}
		return n > 0
	}
}

// A special file at path, which in an image tree below root_dir may be a
// device node whose opening has side effects, is refused without being
// opened, by refresh, by the attribute update and by install.
func TestRemoteFileNeverOpensSpecialFiles(t *testing.T) {
	content := []byte("artifact")
	srv := fileServer(t, content)
	sum, _ := parseChecksum("sha256:" + sha256Hex(content))
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	opened := watchOpens(t, fifo)

	if _, err := readLocalFile(fifo, false); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("readLocalFile(fifo) = %v, want a not-a-regular-file error", err)
	}
	if opened() {
		t.Error("readLocalFile opened the FIFO")
	}
	if err := setRemoteFileAttrs(fifo, 0o644, "", ""); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("setRemoteFileAttrs(fifo) = %v, want a not-a-regular-file error", err)
	}
	if opened() {
		t.Error("setRemoteFileAttrs opened the FIFO")
	}
	d, err := downloadToTemp(context.Background(), downloadRequest{url: srv.URL, timeout: 10 * time.Second, maxSize: 1 << 20, checksum: &sum}, fifo)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.install(fifo, 0o644, "", ""); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("install over a FIFO = %v, want a not-a-regular-file error", err)
	}
	if opened() {
		t.Error("install opened the FIFO")
	}
	assertOnlyEntries(t, dir, "fifo")
}

// max_size_bytes = 9223372036854775807 must not turn into a limit of
// nothing at all when one is added to detect oversized responses.
func TestDownloadToTempMaxInt64Size(t *testing.T) {
	content := []byte("artifact")
	srv := fileServer(t, content)
	sum, _ := parseChecksum("sha256:" + sha256Hex(content))
	d, err := downloadToTemp(context.Background(), downloadRequest{url: srv.URL, timeout: 10 * time.Second, maxSize: math.MaxInt64, checksum: &sum},
		filepath.Join(t.TempDir(), "f"))
	if err != nil {
		t.Fatalf("download with the largest max_size_bytes: %v", err)
	}
	defer d.discard()
	if d.size != int64(len(content)) {
		t.Fatalf("downloaded %d bytes, want %d", d.size, len(content))
	}
	// Without a checksum an empty file would have been accepted silently.
	d2, err := downloadToTemp(context.Background(), downloadRequest{url: srv.URL, timeout: 10 * time.Second, maxSize: math.MaxInt64},
		filepath.Join(t.TempDir(), "f"))
	if err != nil {
		t.Fatal(err)
	}
	defer d2.discard()
	if d2.digests.sha256 != sha256Hex(content) {
		t.Fatalf("unverified download has SHA-256 %s, want that of %q", d2.digests.sha256, content)
	}
}

// The URL's query may hold a token; net/http would send it on to the
// redirect target as the Referer.
func TestDownloadRedirectSendsNoReferer(t *testing.T) {
	content := []byte("artifact")
	sum, _ := parseChecksum("sha256:" + sha256Hex(content))
	var referer string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		referer = r.Header.Get("Referer")
		_, _ = w.Write(content)
	}))
	t.Cleanup(other.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/file", http.StatusFound)
	}))
	t.Cleanup(origin.Close)
	d, err := downloadToTemp(context.Background(), downloadRequest{url: origin.URL + "/file?token=s3cret", timeout: 10 * time.Second, maxSize: 1 << 20, checksum: &sum},
		filepath.Join(t.TempDir(), "f"))
	if err != nil {
		t.Fatal(err)
	}
	d.discard()
	if referer != "" {
		t.Fatalf("the redirect target got Referer %q", referer)
	}
}

// A server must not be able to redirect the provider to the cloud metadata
// service (or another link-local address) and have its answer saved.
func TestDownloadRedirectToMetadataServiceRefused(t *testing.T) {
	var hits int
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/iam/security-credentials/", http.StatusFound)
	}))
	t.Cleanup(origin.Close)
	dir := t.TempDir()
	_, err := downloadToTemp(context.Background(), downloadRequest{url: origin.URL, timeout: 3 * time.Second, maxSize: 1 << 20},
		filepath.Join(dir, "f"))
	if err == nil || !strings.Contains(err.Error(), "refusing to connect to 169.254.169.254") ||
		!strings.Contains(err.Error(), "link-local or cloud metadata address") {
		t.Fatalf("redirect to the metadata service: error = %v, want a refusal", err)
	}
	if hits != 1 {
		t.Fatalf("origin hit %d times", hits)
	}
	assertOnlyEntries(t, dir)
}

// redirectDialGuard enforces the policy on the address actually connected
// to, whatever the host name.
func TestRedirectDialGuard(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	ctx := context.Background()
	dial := func(g *redirectDialGuard, host string) error {
		c, err := g.dialContext(ctx, "tcp", net.JoinHostPort(host, port))
		if err == nil {
			_ = c.Close()
		}
		return err
	}

	// The first connection to the original host is not restricted, and
	// records the kind of its address.
	g := &redirectDialGuard{origHost: "127.0.0.1"}
	if err := dial(g, "127.0.0.1"); err != nil {
		t.Fatalf("original host: %v", err)
	}
	if !g.connected || g.origKind != "loopback" {
		t.Fatalf("guard = %+v after connecting to the original host", g)
	}
	// Redirects from a loopback host to another loopback host are fine.
	if err := dial(g, "localhost"); err != nil {
		t.Fatalf("loopback to loopback: %v", err)
	}

	// From a public host, a redirect must not reach loopback, whether by
	// name, by address or through 0.0.0.0.
	public := &redirectDialGuard{origHost: "downloads.example.com", connected: true}
	for _, host := range []string{"localhost", "127.0.0.1", "0.0.0.0", "::1"} {
		if err := dial(public, host); err == nil || !strings.Contains(err.Error(), "loopback address") {
			t.Errorf("redirect from a public host to %s: %v, want a refusal", host, err)
		}
	}
	// Nor a redirect back to the original host name once it resolves to
	// loopback (DNS rebinding).
	rebound := &redirectDialGuard{origHost: "localhost", connected: true}
	if err := dial(rebound, "localhost"); err == nil || !strings.Contains(err.Error(), "loopback address") {
		t.Errorf("rebound original host: %v, want a refusal", err)
	}
	// A proxy is dialed without restrictions.
	proxied := &redirectDialGuard{origHost: "downloads.example.com", connected: true}
	proxied.addProxy(&url.URL{Scheme: "http", Host: "127.0.0.1:" + port})
	if err := dial(proxied, "127.0.0.1"); err != nil {
		t.Errorf("proxy: %v", err)
	}
}

func TestInternalAddrKind(t *testing.T) {
	for addr, want := range map[string]string{
		"127.0.0.1":        "loopback",
		"127.1.2.3":        "loopback",
		"::1":              "loopback",
		"0.0.0.0":          "loopback",
		"::":               "loopback",
		"::ffff:127.0.0.1": "loopback",
		"169.254.169.254":  "link-local or cloud metadata",
		"fe80::1":          "link-local or cloud metadata",
		"fd00:ec2::254":    "link-local or cloud metadata",
		"100.100.100.200":  "link-local or cloud metadata",
		"10.0.0.1":         "",
		"192.168.1.1":      "",
		"fd12::1":          "",
		"93.184.216.34":    "",
	} {
		if got := internalAddrKind(netip.MustParseAddr(addr)); got != want {
			t.Errorf("internalAddrKind(%s) = %q, want %q", addr, got, want)
		}
	}
}
