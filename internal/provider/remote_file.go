package provider

// Downloading files for sysutils_remote_file.
//
// A download is written to a private temporary file next to the target and
// only renamed over the target once its size and checksum have been
// verified, so the target never holds partial or unverified content, and a
// failed download leaves nothing behind. The following decisions apply:
//
//   - Only http and https URLs are fetched. A redirect never leaves https
//     once the original URL used it, and the configured headers, which often
//     carry credentials, are sent to the original host only.
//   - Responses are saved as served: the client does not ask for, and so
//     does not transparently decode, compressed transfer encodings, because
//     published checksums are those of the served bytes.
//   - The size limit is enforced while streaming, not only against
//     Content-Length, which a server may omit or understate.
//   - The temporary file has mode 0600 while data is written to it. Owner,
//     mode and the extended attributes of a replaced file are applied through
//     its descriptor after verification, immediately before the rename, with
//     the target's shared-file lock held (see lock.go); the download itself
//     runs without the lock, since it can take minutes.

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	checksumSHA256 = "sha256"
	checksumSHA512 = "sha512"

	// maxRemoteFileRedirects bounds the redirects followed for one download.
	maxRemoteFileRedirects = 10
	// remoteFileUserAgent is sent unless the headers set a User-Agent.
	remoteFileUserAgent = "terraform-provider-sysutils"
)

// testRemoteFileRootCAs, if set, replaces the system's trusted CAs for
// downloads, so that acceptance tests can use an httptest TLS server.
var testRemoteFileRootCAs atomic.Pointer[x509.CertPool]

// expectedChecksum is a parsed checksum attribute, such as "sha256:<hex>".
type expectedChecksum struct {
	algo string
	hex  string // Lower case.
}

// parseChecksum parses "sha256:<64 hex digits>" or "sha512:<128 hex
// digits>". Hex digits may be upper or lower case.
func parseChecksum(s string) (expectedChecksum, error) {
	algo, digest, ok := strings.Cut(s, ":")
	if !ok {
		return expectedChecksum{}, fmt.Errorf("checksum %q must have the form sha256:<hex> or sha512:<hex>", s)
	}
	var want int
	switch algo {
	case checksumSHA256:
		want = sha256.Size * 2
	case checksumSHA512:
		want = sha512.Size * 2
	default:
		return expectedChecksum{}, fmt.Errorf("checksum %q uses unsupported algorithm %q; use sha256 or sha512", s, algo)
	}
	if len(digest) != want {
		return expectedChecksum{}, fmt.Errorf("%s checksum %q must have %d hex digits, not %d", algo, s, want, len(digest))
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return expectedChecksum{}, fmt.Errorf("checksum %q is not hexadecimal", s)
	}
	return expectedChecksum{algo: algo, hex: strings.ToLower(digest)}, nil
}

func (c expectedChecksum) String() string { return c.algo + ":" + c.hex }

// fileDigests are the digests of a file: always SHA-256, and SHA-512 if it
// was asked for.
type fileDigests struct {
	sha256 string
	sha512 string
}

// get returns the digest for algo.
func (d fileDigests) get(algo string) string {
	if algo == checksumSHA512 {
		return d.sha512
	}
	return d.sha256
}

// digester computes the SHA-256 and optionally the SHA-512 digest of
// everything written to it.
type digester struct {
	s256, s512 hash.Hash
}

func newDigester(withSHA512 bool) *digester {
	d := &digester{s256: sha256.New()}
	if withSHA512 {
		d.s512 = sha512.New()
	}
	return d
}

func (d *digester) Write(p []byte) (int, error) {
	_, _ = d.s256.Write(p) // hash.Hash writes never fail.
	if d.s512 != nil {
		_, _ = d.s512.Write(p)
	}
	return len(p), nil
}

func (d *digester) digests() fileDigests {
	r := fileDigests{sha256: hex.EncodeToString(d.s256.Sum(nil))}
	if d.s512 != nil {
		r.sha512 = hex.EncodeToString(d.s512.Sum(nil))
	}
	return r
}

// localFileState is what refresh learns about a downloaded file.
type localFileState struct {
	digests fileDigests
	info    fs.FileInfo
}

// readLocalFile hashes the regular file at p without following a symlink at
// p. withSHA512 adds its SHA-512 digest. A missing file returns an error
// wrapping fs.ErrNotExist.
func readLocalFile(p string, withSHA512 bool) (*localFileState, error) {
	f, err := openNoFollow(p, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if err := checkRegularFile(p, info); err != nil {
		return nil, err
	}
	d := newDigester(withSHA512)
	if _, err := io.Copy(d, f); err != nil {
		return nil, fmt.Errorf("reading %q: %w", p, err)
	}
	return &localFileState{digests: d.digests(), info: info}, nil
}

// validateDownloadURL checks that raw is an absolute http or https URL with
// a host and without user information, which would end up in the state and
// in logs in plain text; credentials belong in the sensitive headers.
func validateDownloadURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("URL %q must use http or https", u.Redacted())
	}
	if u.Host == "" || u.Hostname() == "" {
		return nil, fmt.Errorf("URL %q has no host", u.Redacted())
	}
	if u.User != nil {
		return nil, fmt.Errorf("URL %q contains user information; pass credentials in headers, which are sensitive, instead", u.Redacted())
	}
	return u, nil
}

// downloadRequest describes one download.
type downloadRequest struct {
	url      string
	headers  map[string]string
	timeout  time.Duration
	maxSize  int64
	checksum *expectedChecksum // nil for an unverified download.
}

// downloadedFile is a verified download in a temporary file next to its
// target, still open for writing. install renames it into place; discard
// removes it. Exactly one of them must be called.
type downloadedFile struct {
	f       *os.File
	tmp     string
	digests fileDigests
	size    int64
}

// downloadToTemp downloads req to a new temporary file in the directory of
// target, which must exist, and verifies its size and checksum. Nothing is
// left behind if it fails.
func downloadToTemp(ctx context.Context, req downloadRequest, target string) (_ *downloadedFile, err error) {
	orig, err := validateDownloadURL(req.url)
	if err != nil {
		return nil, err
	}

	f, err := createTempFileFor(target)
	if err != nil {
		return nil, err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()

	ctx, cancel := context.WithTimeout(ctx, req.timeout)
	defer cancel()
	httpReq, err := newDownloadRequest(ctx, orig, req.headers)
	if err != nil {
		return nil, err
	}
	resp, err := remoteFileClient(orig, req.headers).Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", orig.Redacted(), redactURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading %s: server responded with %s", orig.Redacted(), resp.Status)
	}
	if resp.ContentLength > req.maxSize {
		return nil, fmt.Errorf("downloading %s: the response has %d bytes, more than max_size_bytes (%d)", orig.Redacted(), resp.ContentLength, req.maxSize)
	}

	d := newDigester(req.checksum != nil && req.checksum.algo == checksumSHA512)
	n, err := io.Copy(io.MultiWriter(f, d), io.LimitReader(resp.Body, req.maxSize+1))
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", orig.Redacted(), redactURLError(err))
	}
	if n > req.maxSize {
		return nil, fmt.Errorf("downloading %s: the response is larger than max_size_bytes (%d)", orig.Redacted(), req.maxSize)
	}
	sums := d.digests()
	if req.checksum != nil {
		if got := sums.get(req.checksum.algo); got != req.checksum.hex {
			return nil, fmt.Errorf("downloading %s: checksum mismatch: expected %s, got %s:%s; the file was not written",
				orig.Redacted(), req.checksum, req.checksum.algo, got)
		}
	}
	if err := f.Sync(); err != nil {
		return nil, err
	}
	return &downloadedFile{f: f, tmp: tmp, digests: sums, size: n}, nil
}

// createTempFileFor creates an empty temporary file with mode 0600 in the
// directory of target. Its name starts with "." so that tools that read
// every file of a configuration directory skip it.
func createTempFileFor(target string) (*os.File, error) {
	dir, base := filepath.Split(target)
	tmp := filepath.Join(dir, "."+base+".sysutils-tmp-"+randomID())
	f, err := openNoFollow(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("creating temporary file: %w", err)
	}
	return f, nil
}

// newDownloadRequest builds the GET request for u with the configured
// headers.
func newDownloadRequest(ctx context.Context, u *url.URL, headers map[string]string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", remoteFileUserAgent)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

// remoteFileClient returns the HTTP client for a download from orig. It
// uses the environment's proxy settings, requires TLS 1.2, never asks for a
// compressed response, and follows at most maxRemoteFileRedirects
// redirects, none of which may leave https if orig uses it. The configured
// headers are removed from any request to a host other than orig's.
func remoteFileClient(orig *url.URL, headers map[string]string) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: testRemoteFileRootCAs.Load()}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return checkRemoteFileRedirect(orig, headers, req, via)
		},
	}
}

// checkRemoteFileRedirect is the redirect policy of remoteFileClient.
// net/http copies the original request's headers to req before calling it.
func checkRemoteFileRedirect(orig *url.URL, headers map[string]string, req *http.Request, via []*http.Request) error {
	if len(via) > maxRemoteFileRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRemoteFileRedirects)
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return fmt.Errorf("refusing redirect to %s: only http and https are supported", req.URL.Redacted())
	}
	if orig.Scheme == "https" && req.URL.Scheme != "https" {
		return fmt.Errorf("refusing redirect from https to %s: the download would no longer be encrypted", req.URL.Redacted())
	}
	if !sameHost(orig, req.URL) {
		for k := range headers {
			req.Header.Del(k)
		}
		if req.Header.Get("User-Agent") == "" {
			req.Header.Set("User-Agent", remoteFileUserAgent)
		}
	}
	return nil
}

// sameHost reports whether a and b name the same host and port, with the
// scheme's default port filled in.
func sameHost(a, b *url.URL) bool {
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		if u.Scheme == "https" {
			return "443"
		}
		return "80"
	}
	return strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
}

// redactURLError removes user information from the URL of a *url.Error.
func redactURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		if u, perr := url.Parse(ue.URL); perr == nil {
			return fmt.Errorf("%s: %w", u.Redacted(), ue.Err)
		}
	}
	return err
}

// discard closes and removes the temporary file. Closing twice is harmless.
func (d *downloadedFile) discard() {
	_ = d.f.Close()
	_ = os.Remove(d.tmp)
}

// install renames the download over target, giving it mode and the owner
// and group names (empty keeps the replaced file's, or the provider user's
// for a new file). The shared-file lock of target is held from before the
// target is inspected until after the rename. A symlink, directory or other
// non-regular file at target is refused. The extended attributes of a
// replaced file, such as a POSIX ACL or SELinux label, are carried over
// (see readXattrs), before the mode is applied, so that the result is the
// same as changing the mode of the old file in place. The temporary file is
// removed if anything fails.
func (d *downloadedFile) install(target string, mode fs.FileMode, owner, group string) error {
	renamed := false
	defer func() {
		if !renamed {
			d.discard()
		}
	}()
	uid, gid, err := resolveOwnership(owner, group)
	if err != nil {
		return err
	}

	unlock, err := lockFileForEdit(target)
	if err != nil {
		return err
	}
	defer unlock()

	var orig *fileSnapshot
	var xattrs map[string][]byte
	if cur, err := openNoFollow(target, os.O_RDONLY, 0); err == nil {
		info, err := cur.Stat()
		if err == nil {
			err = checkRegularFile(target, info)
		}
		if err == nil {
			orig, err = snapshotOf(target, info)
		}
		if err == nil {
			xattrs, err = readXattrs(cur)
		}
		_ = cur.Close()
		if err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	// Keep what is not configured from the replaced file.
	if orig != nil {
		if uid < 0 {
			uid = int(orig.uid)
		}
		if gid < 0 {
			gid = int(orig.gid)
		}
	}
	if uid >= 0 || gid >= 0 {
		if err := d.f.Chown(uid, gid); err != nil {
			return fmt.Errorf("%q: setting ownership: %w", target, err)
		}
	}
	if err := writeXattrs(d.f, xattrs); err != nil {
		return fmt.Errorf("%q: carrying over extended attributes: %w", target, err)
	}
	// After chown, which may clear the setuid and setgid bits, and after the
	// ACL, whose mask chmod then sets as chmod on the old file would have.
	if err := d.f.Chmod(mode); err != nil {
		return fmt.Errorf("%q: setting mode: %w", target, err)
	}
	if err := d.f.Sync(); err != nil {
		return err
	}
	if err := d.f.Close(); err != nil {
		return err
	}

	if err := checkUnchanged(target, orig); err != nil {
		return err
	}
	if orig == nil {
		err = renameNoReplace(d.tmp, target)
	} else {
		err = os.Rename(d.tmp, target)
	}
	if errors.Is(err, syscall.EBUSY) {
		return fmt.Errorf("%q is a mount point and can't be replaced: %w", target, err)
	}
	if err != nil {
		return err
	}
	renamed = true
	syncDir(filepath.Dir(target))
	return nil
}
