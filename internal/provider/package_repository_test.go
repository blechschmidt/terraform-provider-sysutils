package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// testPublicKey is the smallest binary OpenPGP data that passes as a public
// key: an old-format packet header of tag 6 and a two-byte length. The
// provider checks the packet type only, not the key material.
var testPublicKey = []byte{0x99, 0x00, 0x03, 0x04, 0x01, 0x02}

// testArmoredKey is testPublicKey, ASCII-armored as the provider stores it.
var testArmoredKey = string(armorPublicKey(testPublicKey))

func TestValidateRepoName(t *testing.T) {
	for _, ok := range []string{"docker", "docker-ce", "a", "pgdg_16", "gh.cli", "X9"} {
		if err := validateRepoName(ok); err != nil {
			t.Errorf("validateRepoName(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-x", ".x", "a/b", "../x", "a b", "a\nb", "a:b", "é", strings.Repeat("a", 101)} {
		if err := validateRepoName(bad); err == nil {
			t.Errorf("validateRepoName(%q) = nil", bad)
		}
	}
}

func TestValidateRepoURI(t *testing.T) {
	for _, ok := range []string{
		"https://download.docker.com/linux/debian",
		"http://deb.debian.org/debian",
		"file:///srv/repo",
		"file:/srv/repo",
		"https://mirror.example.com/fedora/$releasever/$basearch/",
		"https://example.com:8443/repo?x=1",
	} {
		if err := validateRepoURI(ok); err != nil {
			t.Errorf("validateRepoURI(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{
		"",
		"ftp://example.com/repo",
		"HTTPS://example.com/repo",
		"https://example.com/repo\nSigned-By: /tmp/evil",
		"https://example.com/a b",
		"https://example.com/a\tb",
		"https://example.com/repo#frag",
		"https://user:secret@example.com/repo",
		"https:///nohost",
		"file://host/srv/repo",
		"file:relative",
		"/srv/repo",
		"cdrom:[Debian]/",
		"mirror+file:/etc/apt/mirrors.txt",
		"https://example.com/" + strings.Repeat("a", maxRepoURILen),
	} {
		if err := validateRepoURI(bad); err == nil {
			t.Errorf("validateRepoURI(%q) = nil", bad)
		}
	}
}

func TestValidateSigningKeyURL(t *testing.T) {
	for _, ok := range []string{"https://download.docker.com/linux/debian/gpg", "file:///usr/share/keyrings/debian-archive-keyring.gpg"} {
		if err := validateSigningKeyURL(ok); err != nil {
			t.Errorf("validateSigningKeyURL(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"http://example.com/key.asc", "ftp://example.com/key", "https://example.com/k\n", "https://u:p@example.com/k"} {
		if err := validateSigningKeyURL(bad); err == nil {
			t.Errorf("validateSigningKeyURL(%q) = nil", bad)
		}
	}
}

func TestValidateRepoDescription(t *testing.T) {
	if err := validateRepoDescription("Docker CE (stable)"); err != nil {
		t.Error(err)
	}
	for _, bad := range []string{"", " x", "x ", "a\nb", "a\rb", "a\x00b", "a b", strings.Repeat("a", 257)} {
		if err := validateRepoDescription(bad); err == nil {
			t.Errorf("validateRepoDescription(%q) = nil", bad)
		}
	}
}

func TestRepoSpecValidate(t *testing.T) {
	apt := func() repoSpec {
		return repoSpec{name: "x", enabled: true, gpgCheck: true, uris: []string{"https://example.com/debian"}, suites: []string{"bookworm"}, components: []string{"main"}}
	}
	rpm := func() repoSpec {
		return repoSpec{name: "x", enabled: true, gpgCheck: true, uris: []string{"https://example.com/fedora/$releasever/"}}
	}
	apk := func() repoSpec {
		return repoSpec{name: "x", enabled: true, gpgCheck: true, uris: []string{"https://dl-cdn.alpinelinux.org/alpine/edge/testing"}}
	}
	tests := []struct {
		name   string
		family string
		spec   func() repoSpec
		edit   func(*repoSpec)
		want   string // "" for valid
	}{
		{"apt ok", repoFamilyApt, apt, func(*repoSpec) {}, ""},
		{"apt flat", repoFamilyApt, apt, func(s *repoSpec) { s.suites, s.components = []string{"./"}, nil }, ""},
		{"apt flat with components", repoFamilyApt, apt, func(s *repoSpec) { s.suites = []string{"./"} }, "components must not be set"},
		{"apt without suites", repoFamilyApt, apt, func(s *repoSpec) { s.suites = nil }, "suites must be set"},
		{"apt without components", repoFamilyApt, apt, func(s *repoSpec) { s.components = nil }, "components must be set"},
		{"apt bad type", repoFamilyApt, apt, func(s *repoSpec) { s.types = []string{"rpm"} }, "type"},
		{"apt bad suite", repoFamilyApt, apt, func(s *repoSpec) { s.suites = []string{"a b"} }, "suite"},
		{"apt bad component", repoFamilyApt, apt, func(s *repoSpec) { s.components = []string{"main\nSigned-By: x"} }, "component"},
		{"apt bad architecture", repoFamilyApt, apt, func(s *repoSpec) { s.architectures = []string{"AMD64"} }, "architecture"},
		{"apt tag", repoFamilyApt, apt, func(s *repoSpec) { s.tag = "t" }, "tag is not supported"},
		{"apt gpg_check", repoFamilyApt, apt, func(s *repoSpec) { s.gpgCheck = false }, "gpg_check = false is not supported"},
		{"rpm ok", repoFamilyRpm, rpm, func(*repoSpec) {}, ""},
		{"rpm several uris", repoFamilyRpm, rpm, func(s *repoSpec) { s.uris = append(s.uris, "https://mirror.example.org/") }, ""},
		{"rpm comma", repoFamilyRpm, rpm, func(s *repoSpec) { s.uris = []string{"https://example.com/a,b"} }, "must not contain \",\""},
		{"rpm suites", repoFamilyRpm, rpm, func(s *repoSpec) { s.suites = []string{"x"} }, "suites is not supported"},
		{"apk ok", repoFamilyApk, apk, func(s *repoSpec) { s.tag = "testing" }, ""},
		{"apk two uris", repoFamilyApk, apk, func(s *repoSpec) { s.uris = append(s.uris, "https://x.example/") }, "exactly one"},
		{"apk key", repoFamilyApk, apk, func(s *repoSpec) { s.signingKeyPath = "/k" }, "signing key is not supported"},
		{"apk bad tag", repoFamilyApk, apk, func(s *repoSpec) { s.tag = "a b" }, "tag"},
		{"no uris", repoFamilyApk, apk, func(s *repoSpec) { s.uris = nil }, "at least one"},
		{"bad uri", repoFamilyRpm, rpm, func(s *repoSpec) { s.uris = []string{"https://e.com/\n[evil]"} }, "white space"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.spec()
			tt.edit(&s)
			err := s.validate(tt.family)
			switch {
			case tt.want == "" && err != nil:
				t.Errorf("validate = %v", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("validate = %v, want error containing %q", err, tt.want)
			}
		})
	}
}

func TestRepoRenderApt(t *testing.T) {
	s := repoSpec{
		name: "docker", description: "Docker CE", enabled: true, gpgCheck: true,
		uris: []string{"https://download.docker.com/linux/debian"}, suites: []string{"bookworm"},
		components: []string{"stable"}, architectures: []string{"amd64", "arm64"},
		signingKeyPath: "/etc/apt/keyrings/docker.asc",
	}
	want := repoFileHeader + `
X-Repolib-Name: Docker CE
Types: deb
URIs: https://download.docker.com/linux/debian
Suites: bookworm
Components: stable
Architectures: amd64 arm64
Signed-By: /etc/apt/keyrings/docker.asc
`
	if got := string(s.render(repoFamilyApt)); got != want {
		t.Errorf("render =\n%s\nwant\n%s", got, want)
	}

	s = repoSpec{name: "local", enabled: false, gpgCheck: true, types: []string{"deb", "deb-src"}, uris: []string{"file:///srv/repo", "http://mirror.example/repo"}, suites: []string{"./"}}
	want = repoFileHeader + `
Types: deb deb-src
URIs: file:///srv/repo http://mirror.example/repo
Suites: ./
Enabled: no
`
	if got := string(s.render(repoFamilyApt)); got != want {
		t.Errorf("render =\n%s\nwant\n%s", got, want)
	}
}

func TestRepoRenderRpm(t *testing.T) {
	s := repoSpec{name: "pgdg", enabled: true, gpgCheck: true, uris: []string{"https://a.example/$releasever/", "https://b.example/"}, signingKeyURL: "https://a.example/key"}
	want := repoFileHeader + `
[pgdg]
name=pgdg
baseurl=https://a.example/$releasever/,https://b.example/
enabled=1
gpgcheck=1
gpgkey=https://a.example/key
`
	if got := string(s.render(repoFamilyRpm)); got != want {
		t.Errorf("render =\n%s\nwant\n%s", got, want)
	}
	s = repoSpec{name: "local", description: "Local packages", enabled: false, gpgCheck: false, uris: []string{"file:///srv/rpms"}, signingKeyPath: "/etc/pki/rpm-gpg/RPM-GPG-KEY-local"}
	want = repoFileHeader + `
[local]
name=Local packages
baseurl=file:///srv/rpms
enabled=0
gpgcheck=0
gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-local
`
	if got := string(s.render(repoFamilyRpm)); got != want {
		t.Errorf("render =\n%s\nwant\n%s", got, want)
	}
}

func TestRepoRenderApk(t *testing.T) {
	s := repoSpec{name: "testing", description: "Edge testing", enabled: true, tag: "testing", uris: []string{"https://dl-cdn.alpinelinux.org/alpine/edge/testing"}}
	want := "# sysutils_package_repository testing: Edge testing\n@testing https://dl-cdn.alpinelinux.org/alpine/edge/testing\n"
	if got := string(s.render(repoFamilyApk)); got != want {
		t.Errorf("render = %q, want %q", got, want)
	}
	s = repoSpec{name: "local", enabled: false, uris: []string{"file:///srv/apk"}}
	want = "# sysutils_package_repository local\n#file:///srv/apk\n"
	if got := string(s.render(repoFamilyApk)); got != want {
		t.Errorf("render = %q, want %q", got, want)
	}
}

// Rendering and parsing round-trip, so that import reproduces the
// configuration.
func TestRepoParseRoundTrip(t *testing.T) {
	apt := repoSpec{
		name: "docker", description: "Docker CE", enabled: false, gpgCheck: true, types: []string{"deb", "deb-src"},
		uris: []string{"https://download.docker.com/linux/debian"}, suites: []string{"bookworm", "trixie"},
		components: []string{"stable", "test"}, architectures: []string{"amd64"},
		signingKeyPath: "/etc/apt/keyrings/docker.asc",
	}
	p := parseAptSources("docker", apt.render(repoFamilyApt))
	if p.signedBy != apt.signingKeyPath {
		t.Errorf("signedBy = %q", p.signedBy)
	}
	p.signingKeyPath = p.signedBy
	if !reflect.DeepEqual(p.repoSpec, apt) {
		t.Errorf("parseAptSources = %+v, want %+v", p.repoSpec, apt)
	}

	rpm := repoSpec{name: "pgdg", description: "PostgreSQL", enabled: true, gpgCheck: false, uris: []string{"https://a.example/", "https://b.example/"}, signingKeyURL: "https://a.example/key"}
	pr, ok := parseYumRepo("pgdg", rpm.render(repoFamilyRpm))
	if !ok || !reflect.DeepEqual(pr.repoSpec, rpm) {
		t.Errorf("parseYumRepo = %+v, %v, want %+v", pr.repoSpec, ok, rpm)
	}
	rpm = repoSpec{name: "local", enabled: false, gpgCheck: true, uris: []string{"file:///srv"}, signingKeyPath: "/etc/pki/rpm-gpg/RPM-GPG-KEY-local"}
	pr, _ = parseYumRepo("local", rpm.render(repoFamilyRpm))
	if pr.signedBy != rpm.signingKeyPath || pr.description != "" || pr.enabled || !pr.gpgCheck {
		t.Errorf("parseYumRepo = %+v", pr)
	}

	apk := repoSpec{name: "testing", description: "Edge: testing", enabled: false, gpgCheck: true, tag: "edge", uris: []string{"https://dl-cdn.alpinelinux.org/alpine/edge/testing"}}
	pa, ok := parseApkRepo("testing", parseTextFile(apk.render(repoFamilyApk)).lines, nil)
	if !ok || !reflect.DeepEqual(pa.repoSpec, apk) {
		t.Errorf("parseApkRepo = %+v, %v, want %+v", pa.repoSpec, ok, apk)
	}
}

func TestParseAptSourcesForeign(t *testing.T) {
	data := `# comment
Types: deb
URIs: https://a.example/debian
  https://b.example/debian
Suites: stable
Components: main
Signed-By:
 -----BEGIN PGP PUBLIC KEY BLOCK-----
 .
 xyz
 -----END PGP PUBLIC KEY BLOCK-----
Enabled: false

Types: deb-src
URIs: https://c.example/
Suites: stable
Components: main
`
	p := parseAptSources("x", []byte(data))
	if want := []string{"https://a.example/debian", "https://b.example/debian"}; !reflect.DeepEqual(p.uris, want) {
		t.Errorf("uris = %v, want %v", p.uris, want)
	}
	if p.enabled || p.extraStanzas != 1 || !strings.Contains(p.signedBy, "BEGIN PGP") {
		t.Errorf("parsed = %+v", p)
	}
}

func TestParseYumRepoForeign(t *testing.T) {
	data := `[other]
name=Other
baseurl=https://other.example/

[x]
name = X repo
baseurl = https://a.example/
  https://b.example/
gpgcheck = 1
enabled = 0
gpgkey = file:///etc/pki/rpm-gpg/RPM-GPG-KEY-x
  https://a.example/other-key
`
	p, ok := parseYumRepo("x", []byte(data))
	if !ok {
		t.Fatal("section not found")
	}
	if want := []string{"https://a.example/", "https://b.example/"}; !reflect.DeepEqual(p.uris, want) {
		t.Errorf("uris = %v, want %v", p.uris, want)
	}
	if p.description != "X repo" || p.enabled || !p.gpgCheck || p.signedBy != "/etc/pki/rpm-gpg/RPM-GPG-KEY-x" {
		t.Errorf("parsed = %+v", p)
	}
	if _, ok := parseYumRepo("missing", []byte(data)); ok {
		t.Error("found a missing section")
	}
}

func TestApkBlockEditing(t *testing.T) {
	orig := "https://dl-cdn.alpinelinux.org/alpine/v3.20/main\nhttps://dl-cdn.alpinelinux.org/alpine/v3.20/community"
	tf := parseTextFile([]byte(orig))
	block := []string{apkMarkerPrefix + "testing", "@testing https://dl-cdn.alpinelinux.org/alpine/edge/testing"}
	if !setApkBlock(tf, "testing", block, nil) {
		t.Fatal("setApkBlock reported no change")
	}
	want := orig + "\n" + strings.Join(block, "\n") + "\n"
	if got := string(tf.bytes()); got != want {
		t.Fatalf("after append =\n%s\nwant\n%s", got, want)
	}
	if setApkBlock(tf, "testing", block, nil) {
		t.Error("setApkBlock changed an up-to-date block")
	}
	// A second repository, then an update of the first.
	other := []string{apkMarkerPrefix + "other", "file:///srv/apk"}
	setApkBlock(tf, "other", other, nil)
	block2 := []string{apkMarkerPrefix + "testing: Edge", "#@testing https://dl-cdn.alpinelinux.org/alpine/edge/testing"}
	setApkBlock(tf, "testing", block2, nil)
	want = orig + "\n" + strings.Join(block2, "\n") + "\n" + strings.Join(other, "\n") + "\n"
	if got := string(tf.bytes()); got != want {
		t.Fatalf("after update =\n%s\nwant\n%s", got, want)
	}
	// A duplicate block is removed by the next update.
	tf.lines = append(tf.lines, block...)
	if _, _, n := apkBlock(tf.lines, "testing", nil); n != 2 {
		t.Fatalf("apkBlock count = %d, want 2", n)
	}
	setApkBlock(tf, "testing", block2, nil)
	if got := string(tf.bytes()); got != want {
		t.Fatalf("after dedup =\n%s\nwant\n%s", got, want)
	}
	// A marker at the end of the file, without its line.
	tf.lines = append(tf.lines, apkMarkerPrefix+"dangling")
	if s, e, _ := apkBlock(tf.lines, "dangling", nil); e != s+1 {
		t.Errorf("dangling block = %d..%d", s, e)
	}
	if !removeApkBlocks(tf, "dangling", 0, nil) || !removeApkBlocks(tf, "testing", 0, nil) || !removeApkBlocks(tf, "other", 0, nil) {
		t.Error("removeApkBlocks found nothing")
	}
	if got := string(tf.bytes()); got != orig+"\n" {
		t.Errorf("after removal = %q, want %q", got, orig+"\n")
	}
	// A marker directly followed by another one owns no line.
	lines := []string{apkMarkerPrefix + "a", apkMarkerPrefix + "b", "https://b.example/"}
	if s, e, _ := apkBlock(lines, "a", nil); s != 0 || e != 1 {
		t.Errorf("apkBlock(a) = %d..%d, want 0..1", s, e)
	}
	// With an owner, the line after the marker belongs to the block only if
	// it names one of the owner's URIs or tags.
	owner := newApkOwner(&repoSpec{uris: []string{"https://a.example/"}, tag: "a"})
	for line, owned := range map[string]bool{
		"https://a.example/":         true,
		"#@x https://a.example/":     true,
		"@a https://mirror.example/": true,
		"https://main.example/":      false,
		"@b https://main.example/":   false,
		"":                           false,
		"https://a.example/extra":    false,
	} {
		if _, e, _ := apkBlock([]string{apkMarkerPrefix + "a", line}, "a", owner); (e == 2) != owned {
			t.Errorf("apkBlock with line %q: end = %d, want owned = %v", line, e, owned)
		}
	}
	// Names are matched exactly.
	if s, _, _ := apkBlock([]string{apkMarkerPrefix + "testing2", "x"}, "testing", nil); s != -1 {
		t.Error("apkBlock matched a longer name")
	}
}

func TestCRC24(t *testing.T) {
	// The check value of CRC-24/OPENPGP.
	if got := crc24([]byte("123456789")); got != 0x21cf02 {
		t.Errorf("crc24 = %06x, want 21cf02", got)
	}
}

func TestNormalizeSigningKey(t *testing.T) {
	armored := string(armorPublicKey(testPublicKey))
	if !strings.HasPrefix(armored, pgpArmorBegin+"\n\n") || !strings.HasSuffix(armored, pgpArmorEnd+"\n") {
		t.Fatalf("armor = %q", armored)
	}
	body, err := dearmorBody(strings.TrimSpace(armored))
	if err != nil || !bytes.Equal(body, testPublicKey) {
		t.Fatalf("dearmorBody = %x, %v", body, err)
	}
	// Binary keys are armored.
	got, err := normalizeSigningKey(testPublicKey)
	if err != nil || string(got) != armored {
		t.Errorf("normalizeSigningKey(binary) = %q, %v", got, err)
	}
	// Armored keys are kept, with CRLF and surrounding white space removed.
	crlf := "\n  " + strings.ReplaceAll(armored, "\n", "\r\n") + "\r\n\n"
	if got, err := normalizeSigningKey([]byte(crlf)); err != nil || string(got) != armored {
		t.Errorf("normalizeSigningKey(crlf) = %q, %v", got, err)
	}
	// Armor headers are allowed.
	withHeader := strings.Replace(armored, pgpArmorBegin+"\n", pgpArmorBegin+"\nComment: test\n", 1)
	if got, err := normalizeSigningKey([]byte(withHeader)); err != nil || string(got) != withHeader {
		t.Errorf("normalizeSigningKey(header) = %q, %v", got, err)
	}

	secret := append([]byte{0x95}, testPublicKey[1:]...) // Tag 5: secret key.
	publicThenSecret := append(append([]byte{}, testPublicKey...), secret...)
	for name, bad := range map[string][]byte{
		"empty":          nil,
		"text":           []byte("not a key"),
		"secret key":     secret,
		"armored junk":   []byte(pgpArmorBegin + "\n\n!!!\n" + pgpArmorEnd),
		"armored secret": []byte(pgpArmorBegin + "\n\n" + base64.StdEncoding.EncodeToString(secret) + "\n" + pgpArmorEnd),
		"private block":  []byte("-----BEGIN PGP PRIVATE KEY BLOCK-----\n\nxx\n-----END PGP PRIVATE KEY BLOCK-----"),
		"too large":      append(append([]byte{}, testPublicKey...), make([]byte, maxSigningKeySize)...),
		// Secret key material after the public key, as in a concatenated
		// export, would end up in a world-readable file.
		"public then secret":         publicThenSecret,
		"armored public then secret": armorPublicKey(publicThenSecret),
		"public then secret subkey":  append(append([]byte{}, testPublicKey...), 0xc7, 0x01, 0x00),
		"public then private block":  []byte(strings.TrimSpace(armored) + "\n" + strings.Replace(strings.Replace(strings.TrimSpace(string(armorPublicKey(secret))), "PUBLIC", "PRIVATE", 1), "PUBLIC", "PRIVATE", 1) + "\n" + strings.TrimSpace(armored)),
		"truncated packet":           testPublicKey[:len(testPublicKey)-1],
	} {
		if _, err := normalizeSigningKey(bad); err == nil {
			t.Errorf("normalizeSigningKey(%s) = nil error", name)
		}
	}
}

func TestFetchSigningKey(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/key.gpg":
			_, _ = w.Write(testPublicKey)
		case "/redirect-http":
			http.Redirect(w, r, "http://"+r.Host+"/key.gpg", http.StatusFound)
		case "/redirect-https":
			http.Redirect(w, r, "/key.gpg", http.StatusFound)
		case "/huge":
			_, _ = w.Write(make([]byte, maxSigningKeySize+1))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx := context.Background()

	for _, p := range []string{"/key.gpg", "/redirect-https"} {
		got, err := fetchSigningKey(ctx, srv.Client(), srv.URL+p)
		if err != nil || !bytes.Equal(got, testPublicKey) {
			t.Errorf("fetch %s = %x, %v", p, got, err)
		}
	}
	for _, p := range []string{"/redirect-http", "/huge", "/missing"} {
		if _, err := fetchSigningKey(ctx, srv.Client(), srv.URL+p); err == nil {
			t.Errorf("fetch %s: no error", p)
		}
	}
	if _, err := fetchSigningKey(ctx, srv.Client(), strings.Replace(srv.URL, "https:", "http:", 1)+"/key.gpg"); err == nil {
		t.Error("fetch over http: no error")
	}

	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key.gpg")
	if err := os.WriteFile(keyFile, testPublicKey, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := fetchSigningKey(ctx, nil, "file://"+keyFile); err != nil || !bytes.Equal(got, testPublicKey) {
		t.Errorf("fetch file = %x, %v", got, err)
	}
	if _, err := fetchSigningKey(ctx, nil, "file://"+dir); err == nil {
		t.Error("fetch of a directory: no error")
	}
	// A FIFO is refused rather than blocking the apply forever.
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := fetchSigningKey(ctx, nil, "file://"+fifo)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("fetch of a FIFO: no error")
		}
	case <-time.After(5 * time.Second):
		t.Error("fetch of a FIFO blocks")
		// Unblock the goroutine.
		if w, err := os.OpenFile(fifo, os.O_WRONLY, 0); err == nil {
			_ = w.Close()
		}
	}
}

func TestDetectRepoFamily(t *testing.T) {
	for _, tt := range []struct {
		dirs []string
		want string
	}{
		{[]string{"/etc/apt", "/etc/yum.repos.d"}, repoFamilyApt},
		{[]string{"/etc/yum.repos.d"}, repoFamilyRpm},
		{[]string{"/etc/dnf"}, repoFamilyRpm},
		{[]string{"/etc/apk"}, repoFamilyApk},
		{nil, ""},
	} {
		got, err := detectRepoFamily(func(p string) bool {
			for _, d := range tt.dirs {
				if d == p {
					return true
				}
			}
			return false
		})
		if got != tt.want || (tt.want == "") != (err != nil) {
			t.Errorf("detectRepoFamily(%v) = %q, %v, want %q", tt.dirs, got, err, tt.want)
		}
	}
}
