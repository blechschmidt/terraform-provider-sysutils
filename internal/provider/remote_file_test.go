package provider

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

func sha512Hex(data []byte) string {
	sum := sha512.Sum512(data)
	return hex.EncodeToString(sum[:])
}

// trustTestServer makes downloads trust the certificate of the httptest TLS
// server srv. All httptest TLS servers share one certificate, so concurrent
// tests can't get in each other's way.
func trustTestServer(srv *httptest.Server) {
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	testRemoteFileRootCAs.Store(pool)
}

func TestParseChecksum(t *testing.T) {
	s256 := sha256Hex([]byte("x"))
	s512 := sha512Hex([]byte("x"))
	for _, tc := range []struct {
		in      string
		want    string
		wantErr string
	}{
		{in: "sha256:" + s256, want: "sha256:" + s256},
		{in: "sha256:" + strings.ToUpper(s256), want: "sha256:" + s256},
		{in: "sha512:" + s512, want: "sha512:" + s512},
		{in: s256, wantErr: "must have the form"},
		{in: "md5:" + s256[:32], wantErr: "unsupported algorithm"},
		{in: "SHA256:" + s256, wantErr: "unsupported algorithm"},
		{in: "sha256:" + s256[:63], wantErr: "64 hex digits"},
		{in: "sha512:" + s256, wantErr: "128 hex digits"},
		{in: "sha256:" + strings.Repeat("g", 64), wantErr: "not hexadecimal"},
		{in: "sha256:", wantErr: "64 hex digits"},
	} {
		got, err := parseChecksum(tc.in)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("parseChecksum(%q) error = %v, want %q", tc.in, err, tc.wantErr)
			}
			continue
		}
		if err != nil || got.String() != tc.want {
			t.Errorf("parseChecksum(%q) = %v, %v; want %s", tc.in, got, err, tc.want)
		}
	}
}

func TestValidateDownloadURL(t *testing.T) {
	for _, tc := range []struct {
		in      string
		wantErr string
	}{
		{in: "https://example.com/a.tar.gz"},
		{in: "http://127.0.0.1:8080/x?y=1"},
		{in: "https://[::1]/x"},
		{in: "ftp://example.com/x", wantErr: "must use http or https"},
		{in: "file:///etc/passwd", wantErr: "must use http or https"},
		{in: "example.com/x", wantErr: "must use http or https"},
		{in: "https:///x", wantErr: "has no host"},
		{in: "https://:443/x", wantErr: "has no host"},
		{in: "https://user:secret@example.com/x", wantErr: "user information"},
		{in: "https://exa mple.com/", wantErr: "invalid URL"},
	} {
		_, err := validateDownloadURL(tc.in)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("validateDownloadURL(%q) = %v", tc.in, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("validateDownloadURL(%q) = %v, want %q", tc.in, err, tc.wantErr)
		}
		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Errorf("validateDownloadURL(%q) leaks the password: %v", tc.in, err)
		}
	}
}

func TestValidateHeaderName(t *testing.T) {
	for _, ok := range []string{"Authorization", "x-api-key", "PRIVATE-TOKEN", "Accept"} {
		if err := validateHeaderName(ok); err != nil {
			t.Errorf("validateHeaderName(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "X Token", "X:Token", "X\nY", "Host", "content-length", "Accept-Encoding", "Ä"} {
		if err := validateHeaderName(bad); err == nil {
			t.Errorf("validateHeaderName(%q) = nil, want an error", bad)
		}
	}
}

func TestSameHost(t *testing.T) {
	u := func(s string) *url.URL {
		v, err := url.Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"https://example.com/a", "https://EXAMPLE.com:443/b", true},
		{"http://example.com/a", "http://example.com:80/b", true},
		{"http://example.com/a", "https://example.com/b", false}, // Port 80 vs 443.
		{"https://example.com/a", "https://example.com:8443/b", false},
		{"https://example.com/a", "https://cdn.example.com/b", false},
		{"https://127.0.0.1:1234/", "https://127.0.0.1:1234/x", true},
	} {
		if got := sameHost(u(tc.a), u(tc.b)); got != tc.want {
			t.Errorf("sameHost(%s, %s) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// fileServer serves content at every path, counting requests.
func fileServer(t *testing.T, content []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// assertOnlyEntries fails unless dir contains exactly the names given.
func assertOnlyEntries(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if strings.Join(got, ",") != strings.Join(names, ",") {
		t.Fatalf("%s contains %q, want %q", dir, got, names)
	}
}

func TestDownloadToTemp(t *testing.T) {
	content := []byte("#!/bin/sh\necho hello\n")
	srv := fileServer(t, content)
	dir := t.TempDir()
	target := filepath.Join(dir, "tool")
	want, err := parseChecksum("sha512:" + sha512Hex(content))
	if err != nil {
		t.Fatal(err)
	}

	d, err := downloadToTemp(context.Background(), downloadRequest{
		url: srv.URL + "/tool", timeout: 10 * time.Second, maxSize: 1 << 20, checksum: &want,
	}, target)
	if err != nil {
		t.Fatal(err)
	}
	if d.size != int64(len(content)) || d.digests.sha256 != sha256Hex(content) || d.digests.sha512 != sha512Hex(content) {
		t.Fatalf("download = %d bytes, %+v", d.size, d.digests)
	}
	if !strings.HasPrefix(filepath.Base(d.tmp), ".tool.sysutils-tmp-") || filepath.Dir(d.tmp) != dir {
		t.Fatalf("temporary file %s is not a hidden file next to the target", d.tmp)
	}
	info, err := os.Stat(d.tmp)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("temporary file: %v, %v; want mode 0600", info, err)
	}
	if err := d.install(target, 0o755, "", ""); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("target = %q, %v", got, err)
	}
	if info, _ := os.Stat(target); info.Mode().Perm() != 0o755 {
		t.Fatalf("target mode = %v, want 0755", info.Mode())
	}
	assertOnlyEntries(t, dir, "tool")
}

func TestDownloadToTempFailuresLeaveNothing(t *testing.T) {
	content := bytes.Repeat([]byte("x"), 2048)
	mux := http.NewServeMux()
	mux.HandleFunc("/file", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(content) })
	mux.HandleFunc("/chunked", func(w http.ResponseWriter, _ *http.Request) {
		// Flushing before the end forces chunked encoding: no Content-Length.
		_, _ = w.Write(content[:1024])
		w.(http.Flusher).Flush()
		_, _ = w.Write(content[1024:])
	})
	mux.HandleFunc("/truncated", func(w http.ResponseWriter, _ *http.Request) {
		// Promises more bytes than it sends.
		w.Header().Set("Content-Length", "4096")
		_, _ = w.Write(content)
	})
	mux.HandleFunc("/missing", http.NotFound)
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	good, _ := parseChecksum("sha256:" + sha256Hex(content))
	bad, _ := parseChecksum("sha256:" + sha256Hex([]byte("other")))
	for _, tc := range []struct {
		name     string
		path     string
		maxSize  int64
		timeout  time.Duration
		checksum *expectedChecksum
		wantErr  string
	}{
		{name: "checksum mismatch", path: "/file", checksum: &bad, wantErr: "checksum mismatch: expected sha256:" + bad.hex + ", got sha256:" + good.hex},
		{name: "content length too large", path: "/file", maxSize: 1024, checksum: &good, wantErr: "the response has 2048 bytes, more than max_size_bytes (1024)"},
		{name: "chunked too large", path: "/chunked", maxSize: 1024, checksum: &good, wantErr: "larger than max_size_bytes (1024)"},
		{name: "not found", path: "/missing", checksum: &good, wantErr: "server responded with 404 Not Found"},
		{name: "timeout", path: "/slow", timeout: 200 * time.Millisecond, checksum: &good, wantErr: "deadline exceeded"},
		{name: "truncated", path: "/truncated", checksum: &good, wantErr: "unexpected EOF"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			req := downloadRequest{url: srv.URL + tc.path, timeout: tc.timeout, maxSize: tc.maxSize, checksum: tc.checksum}
			if req.timeout == 0 {
				req.timeout = 10 * time.Second
			}
			if req.maxSize == 0 {
				req.maxSize = 1 << 20
			}
			d, err := downloadToTemp(context.Background(), req, filepath.Join(dir, "f"))
			if err == nil {
				d.discard()
				t.Fatal("download succeeded")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
			assertOnlyEntries(t, dir)
		})
	}
}

// TestDownloadToTempNoDecompression checks that a response with
// Content-Encoding gzip is saved as served, as curl and wget do, so that it
// matches the checksum published for the file.
func TestDownloadToTempNoDecompression(t *testing.T) {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte("payload"))
	_ = zw.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ae := r.Header.Get("Accept-Encoding"); ae != "" {
			t.Errorf("request asked for Accept-Encoding %q", ae)
		}
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(gz.Bytes())
	}))
	t.Cleanup(srv.Close)
	want, _ := parseChecksum("sha256:" + sha256Hex(gz.Bytes()))
	d, err := downloadToTemp(context.Background(), downloadRequest{url: srv.URL + "/a.gz", timeout: 10 * time.Second, maxSize: 1 << 20, checksum: &want},
		filepath.Join(t.TempDir(), "a.gz"))
	if err != nil {
		t.Fatal(err)
	}
	d.discard()
}

func TestDownloadRedirects(t *testing.T) {
	content := []byte("artifact")
	sum, _ := parseChecksum("sha256:" + sha256Hex(content))

	// other is a second host (another port) that records the headers it got.
	var otherToken, otherUA string
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherToken, otherUA = r.Header.Get("X-Token"), r.Header.Get("User-Agent")
		_, _ = w.Write(content)
	}))
	t.Cleanup(other.Close)
	plain := fileServer(t, content)

	var originToken string
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originToken = r.Header.Get("X-Token")
		switch r.URL.Path {
		case "/same":
			http.Redirect(w, r, "/file", http.StatusFound)
		case "/other":
			http.Redirect(w, r, other.URL+"/file", http.StatusFound)
		case "/downgrade":
			http.Redirect(w, r, plain.URL+"/file", http.StatusMovedPermanently)
		case "/ftp":
			http.Redirect(w, r, "ftp://127.0.0.1/file", http.StatusFound)
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		default:
			_, _ = w.Write(content)
		}
	}))
	t.Cleanup(origin.Close)
	trustTestServer(origin)

	download := func(u string) error {
		dir := t.TempDir()
		d, err := downloadToTemp(context.Background(), downloadRequest{
			url: u, timeout: 10 * time.Second, maxSize: 1 << 20, checksum: &sum,
			headers: map[string]string{"x-token": "s3cret", "User-Agent": "custom"},
		}, filepath.Join(dir, "f"))
		if err != nil {
			assertOnlyEntries(t, dir)
			return err
		}
		d.discard()
		return nil
	}

	if err := download(origin.URL + "/same"); err != nil {
		t.Fatalf("same-host redirect: %v", err)
	}
	if originToken != "s3cret" {
		t.Fatalf("origin got X-Token %q after a same-host redirect", originToken)
	}
	if err := download(origin.URL + "/other"); err != nil {
		t.Fatalf("redirect to another https host: %v", err)
	}
	if otherToken != "" || otherUA != remoteFileUserAgent {
		t.Fatalf("other host got X-Token %q and User-Agent %q; configured headers must stay with the original host", otherToken, otherUA)
	}
	for path, want := range map[string]string{
		"/downgrade": "refusing redirect from https to " + plain.URL + "/file",
		"/ftp":       "refusing redirect to ftp://127.0.0.1/file",
		"/loop":      "stopped after 10 redirects",
	} {
		err := download(origin.URL + path)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error = %v, want %q", path, err, want)
		}
	}
	// A plain http URL may be upgraded to https.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, origin.URL+"/file", http.StatusFound)
	}))
	t.Cleanup(up.Close)
	if err := download(up.URL); err != nil {
		t.Fatalf("http to https redirect: %v", err)
	}
}

func TestDownloadedFileInstallReplaces(t *testing.T) {
	content := []byte("new")
	srv := fileServer(t, content)
	sum, _ := parseChecksum("sha256:" + sha256Hex(content))
	dir := t.TempDir()
	target := filepath.Join(dir, "f")
	req := downloadRequest{url: srv.URL, timeout: 10 * time.Second, maxSize: 1 << 20, checksum: &sum}

	// A symlink at the target is refused and left alone.
	if err := os.Symlink("/etc/passwd", target); err != nil {
		t.Fatal(err)
	}
	d, err := downloadToTemp(context.Background(), req, target)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.install(target, 0o644, "", ""); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("install over a symlink = %v", err)
	}
	if dest, err := os.Readlink(target); err != nil || dest != "/etc/passwd" {
		t.Fatalf("symlink changed: %q, %v", dest, err)
	}
	assertOnlyEntries(t, dir, "f")

	// A regular file is replaced.
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err = downloadToTemp(context.Background(), req, target)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.install(target, 0o640, "", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(target); !bytes.Equal(got, content) {
		t.Fatalf("target = %q", got)
	}
	assertOnlyEntries(t, dir, "f")

	// A directory is refused.
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	d, err = downloadToTemp(context.Background(), req, sub)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.install(sub, 0o644, "", ""); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("install over a directory = %v", err)
	}
	assertOnlyEntries(t, dir, "f", "sub")
}

func TestReadLocalFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := readLocalFile(p, true)
	if err != nil || l.digests.sha256 != sha256Hex([]byte("abc")) || l.digests.sha512 != sha512Hex([]byte("abc")) {
		t.Fatalf("readLocalFile = %+v, %v", l, err)
	}
	if _, err := readLocalFile(filepath.Join(dir, "missing"), false); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file: %v", err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readLocalFile(link, false); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("symlink: %v", err)
	}
}

func TestRemoteFileDownloadDecisions(t *testing.T) {
	sum := "sha256:" + sha256Hex([]byte("a"))
	local := &localFileState{digests: fileDigests{sha256: sha256Hex([]byte("a"))}}
	changed := &localFileState{digests: fileDigests{sha256: sha256Hex([]byte("b"))}}
	model := func(u, checksum string, force bool) *remoteFileModel {
		m := &remoteFileModel{URL: types.StringValue(u), Checksum: types.StringNull(), ForceRedownload: types.BoolValue(force),
			SHA256: types.StringValue(local.digests.sha256)}
		if checksum != "" {
			m.Checksum = types.StringValue(checksum)
		}
		return m
	}
	for _, tc := range []struct {
		name        string
		plan, state *remoteFileModel
		local       *localFileState
		prior       string
		want        bool
	}{
		{"create", model("u", sum, false), nil, nil, "", true},
		{"adopt matching file", model("u", sum, false), nil, local, "", false},
		{"adopt without checksum", model("u", "", false), nil, local, "", true},
		{"in sync", model("u", sum, false), model("u", sum, false), local, local.digests.sha256, false},
		{"url changed with checksum", model("v", sum, false), model("u", sum, false), local, local.digests.sha256, false},
		{"checksum spelling", model("u", sum[:7]+strings.ToUpper(sum[7:]), false), model("u", sum, false), local, "", false},
		{"content drift", model("u", sum, false), model("u", sum, false), changed, local.digests.sha256, true},
		{"missing", model("u", sum, false), model("u", sum, false), nil, "", true},
		{"force", model("u", sum, true), model("u", sum, false), local, "", true},
		{"unverified in sync", model("u", "", false), model("u", "", false), local, local.digests.sha256, false},
		{"unverified imported", model("u", "", false), model("u", "", false), local, "", false},
		{"unverified url changed", model("v", "", false), model("u", "", false), local, local.digests.sha256, true},
		{"unverified drift", model("u", "", false), model("u", "", false), changed, local.digests.sha256, true},
	} {
		if got := needsDownload(tc.plan, tc.state, tc.local, tc.prior); got != tc.want {
			t.Errorf("%s: needsDownload = %v, want %v", tc.name, got, tc.want)
		}
	}

	// The plan-time prediction, from the refreshed state.
	drifted := model("u", "sha256:"+changed.digests.sha256, false)
	drifted.SHA256 = types.StringValue(changed.digests.sha256)
	unverifiedDrift := model("u", "", false)
	unverifiedDrift.SHA256 = types.StringValue(changed.digests.sha256)
	for _, tc := range []struct {
		name        string
		plan, state *remoteFileModel
		prior       string
		want        bool
	}{
		{"create", model("u", sum, false), nil, "", true},
		{"in sync", model("u", sum, false), model("u", sum, false), local.digests.sha256, false},
		{"checksum spelling", model("u", sum[:7]+strings.ToUpper(sum[7:]), false), model("u", sum, false), "", false},
		{"url changed with checksum", model("v", sum, false), model("u", sum, false), "", false},
		{"content drift", model("u", sum, false), drifted, local.digests.sha256, true},
		{"force", model("u", sum, true), model("u", sum, false), "", true},
		{"unverified in sync", model("u", "", false), model("u", "", false), local.digests.sha256, false},
		{"unverified drift", model("u", "", false), unverifiedDrift, local.digests.sha256, true},
		{"unverified url changed", model("v", "", false), model("u", "", false), local.digests.sha256, true},
	} {
		if got := remoteFileDownloadPlanned(tc.plan, tc.state, tc.prior); got != tc.want {
			t.Errorf("%s: remoteFileDownloadPlanned = %v, want %v", tc.name, got, tc.want)
		}
	}
}
