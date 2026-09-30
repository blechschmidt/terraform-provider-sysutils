package provider

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

const testRemoteFileResource = "sysutils_remote_file.test"

// artifactServer serves a changeable content at every path and counts the
// requests, so that tests can tell whether apply downloaded the file.
type artifactServer struct {
	*httptest.Server
	mu      sync.Mutex
	content []byte
	hits    atomic.Int64
	// token, if set, is required in the Authorization header.
	token string
}

func newArtifactServer(t *testing.T, tls bool, content string) *artifactServer {
	t.Helper()
	s := &artifactServer{content: []byte(content)}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		if s.token != "" && r.Header.Get("Authorization") != "Bearer "+s.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		s.mu.Lock()
		data := s.content
		s.mu.Unlock()
		_, _ = w.Write(data)
	})
	if tls {
		s.Server = httptest.NewTLSServer(h)
		trustTestServer(s.Server)
	} else {
		s.Server = httptest.NewServer(h)
	}
	t.Cleanup(s.Close)
	return s
}

func (s *artifactServer) setContent(c string) {
	s.mu.Lock()
	s.content = []byte(c)
	s.mu.Unlock()
}

func checkHits(s *artifactServer, want int64) resource.TestCheckFunc {
	return func(*terraform.State) error {
		if got := s.hits.Load(); got != want {
			return fmt.Errorf("the server got %d requests, want %d", got, want)
		}
		return nil
	}
}

// checkDirEmpty checks that dir exists and has no entries: a failed
// download must leave neither the file nor a temporary file behind.
func checkDirEmpty(dir string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		if len(entries) != 0 {
			return fmt.Errorf("%s contains %s after a failed download", dir, entries[0].Name())
		}
		return nil
	}
}

func remoteFileConfig(u, p, checksum, extra string) string {
	return fmt.Sprintf(`
resource "sysutils_remote_file" "test" {
  url      = %q
  path     = %q
  checksum = %q
%s
}
`, u, p, checksum, extra)
}

func TestAccRemoteFile_basic(t *testing.T) {
	const content = "#!/bin/sh\necho tool 1.0\n"
	srv := newArtifactServer(t, true, content)
	dir := t.TempDir()
	target := filepath.Join(dir, "bin", "tool")
	sum := "sha256:" + sha256Hex([]byte(content))

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkNotExist(target),
		Steps: []resource.TestStep{
			{
				Config: remoteFileConfig(srv.URL+"/tool-1.0", target, sum, `  mode = "0755"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						// A SHA-256 checksum says what sha256 will be.
						plancheck.ExpectKnownValue(testRemoteFileResource, tfjsonpath.New("sha256"), knownvalue.StringExact(sha256Hex([]byte(content)))),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testRemoteFileResource, "id", target),
					resource.TestCheckResourceAttr(testRemoteFileResource, "sha256", sha256Hex([]byte(content))),
					resource.TestCheckResourceAttr(testRemoteFileResource, "mode", "0755"),
					resource.TestCheckResourceAttr(testRemoteFileResource, "timeout", "60s"),
					resource.TestCheckResourceAttr(testRemoteFileResource, "max_size_bytes", "1073741824"),
					resource.TestCheckResourceAttr(testRemoteFileResource, "allow_unverified", "false"),
					resource.TestCheckResourceAttr(testRemoteFileResource, "force_redownload", "false"),
					resource.TestCheckResourceAttrSet(testRemoteFileResource, "owner"),
					checkFileContent(target, content),
					checkFileMode(target, 0o755),
					checkHits(srv, 1),
				),
			},
			{
				// Refresh and plan don't fetch the URL.
				Config:   remoteFileConfig(srv.URL+"/tool-1.0", target, sum, `  mode = "0755"`),
				PlanOnly: true,
				Check:    checkHits(srv, 1),
			},
			{
				// A new URL with the same checksum pins the same content:
				// no download.
				Config: remoteFileConfig(srv.URL+"/mirror/tool-1.0", target, sum, `  mode = "0750"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testRemoteFileResource, plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testRemoteFileResource, "url", srv.URL+"/mirror/tool-1.0"),
					checkFileMode(target, 0o750),
					checkHits(srv, 1),
				),
			},
			{
				ResourceName:            testRemoteFileResource,
				ImportState:             true,
				ImportStateId:           target,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"url", "checksum", "headers"},
			},
		},
	})
	if got := srv.hits.Load(); got != 1 {
		t.Fatalf("the server got %d requests in total, want 1", got)
	}
}

// TestAccRemoteFile_notFetchedOnRead stops the server after the download:
// refresh and plan must not need it.
func TestAccRemoteFile_notFetchedOnRead(t *testing.T) {
	const content = "data"
	srv := newArtifactServer(t, false, content)
	target := filepath.Join(t.TempDir(), "data")
	config := remoteFileConfig(srv.URL+"/data", target, "sha256:"+sha256Hex([]byte(content)), "")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkNotExist(target),
		Steps: []resource.TestStep{
			{Config: config, Check: checkFileContent(target, content)},
			{PreConfig: srv.Close, Config: config, PlanOnly: true},
		},
	})
}

func TestAccRemoteFile_checksumMismatch(t *testing.T) {
	const content = "release 2.0\n"
	srv := newArtifactServer(t, true, content)
	dir := t.TempDir()
	target := filepath.Join(dir, "release")
	good := "sha256:" + sha256Hex([]byte(content))
	bad := "sha256:" + sha256Hex([]byte("something else"))

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkDirEmpty(dir),
		Steps: []resource.TestStep{
			{
				Config:      remoteFileConfig(srv.URL+"/release", target, bad, ""),
				ExpectError: regexp.MustCompile(`checksum\s+mismatch:\s+expected\s+` + bad + `,\s+got\s+` + good),
			},
			{
				// Nothing was written.
				PreConfig:          func() { assertOnlyEntries(t, dir) },
				Config:             remoteFileConfig(srv.URL+"/release", target, good, ""),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				Config: remoteFileConfig(srv.URL+"/release", target, good, ""),
				Check:  checkFileContent(target, content),
			},
			{
				// The server now serves something else: a forced download
				// fails the checksum and leaves the file on disk alone.
				PreConfig:   func() { srv.setContent("tampered") },
				Config:      remoteFileConfig(srv.URL+"/release", target, good, "  force_redownload = true"),
				ExpectError: regexp.MustCompile(`checksum\s+mismatch`),
			},
			{
				PreConfig: func() {
					if got, err := os.ReadFile(target); err != nil || string(got) != content {
						t.Fatalf("file after the failed download = %q, %v", got, err)
					}
					assertOnlyEntries(t, dir, "release")
				},
				// sha512 checksums work too; the file matches, so no
				// download is needed.
				Config: remoteFileConfig(srv.URL+"/release", target, "sha512:"+sha512Hex([]byte(content)), ""),
				Check:  checkFileContent(target, content),
			},
		},
	})
}

func TestAccRemoteFile_oversize(t *testing.T) {
	big := bytes.Repeat([]byte("x"), 4096)
	mux := http.NewServeMux()
	mux.HandleFunc("/length", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(big)))
		_, _ = w.Write(big)
	})
	mux.HandleFunc("/chunked", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(big[:1000])
		w.(http.Flusher).Flush()
		_, _ = w.Write(big[1000:])
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	trustTestServer(srv)
	dir := t.TempDir()
	target := filepath.Join(dir, "big")
	sum := "sha256:" + sha256Hex(big)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkNotExist(target),
		Steps: []resource.TestStep{
			{
				Config:      remoteFileConfig(srv.URL+"/length", target, sum, "  max_size_bytes = 4095"),
				ExpectError: regexp.MustCompile(`has\s+4096\s+bytes,\s+more\s+than\s+max_size_bytes\s+\(4095\)`),
			},
			{
				Config:      remoteFileConfig(srv.URL+"/chunked", target, sum, "  max_size_bytes = 4095"),
				ExpectError: regexp.MustCompile(`larger\s+than\s+max_size_bytes\s+\(4095\)`),
			},
			{
				Config:      remoteFileConfig(srv.URL+"/chunked", target, sum, "  max_size_bytes = 0"),
				ExpectError: regexp.MustCompile(`must\s+be\s+at\s+least\s+1`),
			},
			{
				PreConfig: func() { assertOnlyEntries(t, dir) },
				Config:    remoteFileConfig(srv.URL+"/chunked", target, sum, "  max_size_bytes = 4096"),
				Check:     checkFileContent(target, string(big)),
			},
		},
	})
}

func TestAccRemoteFile_redirects(t *testing.T) {
	const content = "artifact"
	sum := "sha256:" + sha256Hex([]byte(content))
	plain := newArtifactServer(t, false, content)
	var mirrorAuth atomic.Value
	mirror := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mirrorAuth.Store(r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(content))
	}))
	t.Cleanup(mirror.Close)
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer t0ken" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/downgrade":
			http.Redirect(w, r, plain.URL+"/artifact", http.StatusFound)
		case "/mirror":
			http.Redirect(w, r, mirror.URL+"/artifact", http.StatusFound)
		default:
			_, _ = w.Write([]byte(content))
		}
	}))
	t.Cleanup(origin.Close)
	trustTestServer(origin)
	dir := t.TempDir()
	target := filepath.Join(dir, "artifact")
	headers := `  headers = { Authorization = "Bearer t0ken" }`

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkNotExist(target),
		Steps: []resource.TestStep{
			{
				Config:      remoteFileConfig(origin.URL+"/direct", target, sum, ""),
				ExpectError: regexp.MustCompile(`server\s+responded\s+with\s+401\s+Unauthorized`),
			},
			{
				Config:      remoteFileConfig(origin.URL+"/downgrade", target, sum, headers),
				ExpectError: regexp.MustCompile(`refusing\s+redirect\s+from\s+https\s+to\s+http://`),
				Check:       checkHits(plain, 0),
			},
			{
				PreConfig: func() {
					assertOnlyEntries(t, dir)
					if plain.hits.Load() != 0 {
						t.Fatal("the plain http server was contacted")
					}
				},
				Config: remoteFileConfig(origin.URL+"/mirror", target, sum, headers),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(target, content),
					func(*terraform.State) error {
						if got, _ := mirrorAuth.Load().(string); got != "" {
							return fmt.Errorf("the mirror on another port got Authorization %q", got)
						}
						return nil
					},
				),
			},
		},
	})
}

func TestAccRemoteFile_drift(t *testing.T) {
	requireRoot(t)
	nobody, nogroup := lookupNobody(t)
	nobodyUID, _ := strconv.Atoi(nobody.Uid)
	nogroupGID, _ := strconv.Atoi(nogroup.Gid)

	const content = "server binary\n"
	srv := newArtifactServer(t, true, content)
	target := filepath.Join(t.TempDir(), "server")
	config := remoteFileConfig(srv.URL+"/server", target, "sha256:"+sha256Hex([]byte(content)), `
  mode  = "0750"
  owner = "root"
  group = "root"`)
	inSync := func(hits int64) resource.TestCheckFunc {
		return resource.ComposeAggregateTestCheckFunc(
			resource.TestCheckResourceAttr(testRemoteFileResource, "mode", "0750"),
			resource.TestCheckResourceAttr(testRemoteFileResource, "owner", "root"),
			resource.TestCheckResourceAttr(testRemoteFileResource, "group", "root"),
			checkFileContent(target, content),
			checkFileMode(target, 0o750),
			checkFileUID(target, "0"),
			checkFileGID(target, "0"),
			checkHits(srv, hits),
		)
	}
	expect := func(action plancheck.ResourceActionType) resource.ConfigPlanChecks {
		return resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testRemoteFileResource, action)}}
	}
	mutate := func(fn func() error) func() {
		return func() {
			if err := fn(); err != nil {
				t.Fatal(err)
			}
		}
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkNotExist(target),
		Steps: []resource.TestStep{
			{Config: config, Check: inSync(1)},
			{
				// Mode drift is repaired in place, without a download.
				PreConfig:        mutate(func() error { return os.Chmod(target, 0o777) }),
				Config:           config,
				ConfigPlanChecks: expect(plancheck.ResourceActionUpdate),
				Check:            inSync(1),
			},
			{
				// So is owner and group drift.
				PreConfig:        mutate(func() error { return os.Chown(target, nobodyUID, nogroupGID) }),
				Config:           config,
				ConfigPlanChecks: expect(plancheck.ResourceActionUpdate),
				Check:            inSync(1),
			},
			{
				// Content drift shows as the file's actual checksum...
				PreConfig: mutate(func() error { return os.WriteFile(target, []byte("patched\n"), 0o600) }),
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(testRemoteFileResource, plancheck.ResourceActionUpdate),
					expectBefore(testRemoteFileResource, "checksum", "sha256:"+sha256Hex([]byte("patched\n"))),
				}},
				// ...and apply downloads the file again.
				Check: inSync(2),
			},
			{
				// A deleted file is downloaded again.
				PreConfig:        mutate(func() error { return os.Remove(target) }),
				Config:           config,
				ConfigPlanChecks: expect(plancheck.ResourceActionCreate),
				Check:            inSync(3),
			},
		},
	})
}

func TestAccRemoteFile_rootDir(t *testing.T) {
	const content = "config"
	srv := newArtifactServer(t, true, content)
	root := testRootDir(t)
	hostPath := filepath.Join(root, "opt", "app", "app.conf")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkNotExist(hostPath),
		Steps: []resource.TestStep{
			{
				PreConfig: func() {
					// A symlink in the tree that points to the host's /opt
					// must be resolved inside the tree.
					if err := os.Symlink("/opt/app-real", filepath.Join(root, "opt-link")); err != nil {
						t.Fatal(err)
					}
				},
				Config: rootedProvider(root) + remoteFileConfig(srv.URL+"/app.conf", "/opt/app/app.conf", "sha256:"+sha256Hex([]byte(content)), ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testRemoteFileResource, "path", "/opt/app/app.conf"),
					checkFileContent(hostPath, content),
					checkNotExist("/opt/app/app.conf"),
				),
			},
			{
				ResourceName:            testRemoteFileResource,
				ImportState:             true,
				ImportStateId:           "/opt/app/app.conf",
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"url", "checksum", "headers"},
			},
			{
				Config: rootedProvider(root) + remoteFileConfig(srv.URL+"/app.conf", "/opt-link/app.conf", "sha256:"+sha256Hex([]byte(content)), ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(filepath.Join(root, "opt", "app-real", "app.conf"), content),
					checkNotExist("/opt/app-real"),
				),
			},
		},
	})
}

func TestAccRemoteFile_unverified(t *testing.T) {
	srv := newArtifactServer(t, false, "v1")
	target := filepath.Join(t.TempDir(), "latest")
	config := func(extra string) string {
		return fmt.Sprintf(`
resource "sysutils_remote_file" "test" {
  url  = %q
  path = %q
%s
}
`, srv.URL+"/latest", target, extra)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkNotExist(target),
		Steps: []resource.TestStep{
			{
				Config:      config(""),
				ExpectError: regexp.MustCompile(`Missing\s+checksum`),
			},
			{
				Config: config("  allow_unverified = true"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testRemoteFileResource, "sha256", sha256Hex([]byte("v1"))),
					resource.TestCheckNoResourceAttr(testRemoteFileResource, "checksum"),
					checkFileContent(target, "v1"),
					checkHits(srv, 1),
				),
			},
			{
				// A new release on the server is not noticed: the URL is
				// never fetched during plan.
				PreConfig: func() { srv.setContent("v2") },
				Config:    config("  allow_unverified = true"),
				PlanOnly:  true,
			},
			{
				// Local changes are noticed, and the file is downloaded
				// again.
				PreConfig: func() {
					if err := os.WriteFile(target, []byte("local edit"), 0o644); err != nil {
						t.Fatal(err)
					}
				},
				Config: config("  allow_unverified = true"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(testRemoteFileResource, plancheck.ResourceActionUpdate),
					plancheck.ExpectUnknownValue(testRemoteFileResource, tfjsonpath.New("sha256")),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(target, "v2"),
					checkHits(srv, 2),
				),
			},
			{
				Config: config("  allow_unverified = true"),
				Check:  checkHits(srv, 2),
			},
			{
				// force_redownload downloads on every apply.
				PreConfig: func() { srv.setContent("v3") },
				Config:    config("  allow_unverified = true\n  force_redownload = true"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(target, "v3"),
					checkHits(srv, 3),
				),
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

func TestAccRemoteFile_invalidConfig(t *testing.T) {
	target := filepath.Join(t.TempDir(), "f")
	sum := "sha256:" + sha256Hex([]byte("x"))
	for _, tc := range []struct {
		name, config, want string
	}{
		{"url scheme", remoteFileConfig("ftp://example.com/f", target, sum, ""), `must\s+use\s+http\s+or\s+https`},
		{"url credentials", remoteFileConfig("https://u:p@example.com/f", target, sum, ""), `contains\s+user\s+information`},
		{"checksum algorithm", remoteFileConfig("https://example.com/f", target, "md5:"+sha256Hex([]byte("x"))[:32], ""), `unsupported\s+algorithm`},
		{"checksum length", remoteFileConfig("https://example.com/f", target, "sha512:"+sha256Hex([]byte("x")), ""), `128\s+hex\s+digits`},
		{"relative path", remoteFileConfig("https://example.com/f", "tmp/f", sum, ""), `absolute`},
		{"timeout", remoteFileConfig("https://example.com/f", target, sum, `  timeout = "0s"`), `greater\s+than\s+zero`},
		{"header name", remoteFileConfig("https://example.com/f", target, sum, `  headers = { "X Token" = "a" }`), `invalid\s+character`},
		{"header value", remoteFileConfig("https://example.com/f", target, sum, `  headers = { "X-Token" = "a\nb" }`), `line\s+break`},
		{"host header", remoteFileConfig("https://example.com/f", target, sum, `  headers = { host = "a" }`), `set\s+by\s+the\s+HTTP\s+client`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps:                    []resource.TestStep{{Config: tc.config, PlanOnly: true, ExpectError: regexp.MustCompile(tc.want)}},
			})
		})
	}
}

func TestAccRemoteFile_notRegularFile(t *testing.T) {
	const content = "x"
	srv := newArtifactServer(t, true, content)
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      remoteFileConfig(srv.URL+"/x", link, "sha256:"+sha256Hex([]byte(content)), ""),
				ExpectError: regexp.MustCompile(`symbolic\s+link`),
			},
			{
				Config:      remoteFileConfig(srv.URL+"/x", dir, "sha256:"+sha256Hex([]byte(content)), ""),
				ExpectError: regexp.MustCompile(`not\s+a\s+regular\s+file`),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(victim, "keep"),
				),
			},
		},
		CheckDestroy: func(*terraform.State) error {
			if got, err := os.ReadFile(victim); err != nil || string(got) != "keep" {
				return fmt.Errorf("symlink target changed: %q, %v", got, err)
			}
			if dest, err := os.Readlink(link); err != nil || dest != victim {
				return fmt.Errorf("symlink changed: %q, %v", dest, err)
			}
			return nil
		},
	})
}

// TestAccRemoteFile_adoptMatchingFile checks that a file that already
// matches the checksum is adopted without a download, and so is an imported
// file whose url and checksum the first apply records.
func TestAccRemoteFile_adoptMatchingFile(t *testing.T) {
	const content = "already here"
	srv := newArtifactServer(t, true, content)
	sum := "sha256:" + sha256Hex([]byte(content))
	adopted := filepath.Join(t.TempDir(), "f")
	imported := filepath.Join(t.TempDir(), "g")
	for _, p := range []string{adopted, imported} {
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkNotExist(adopted),
		Steps: []resource.TestStep{
			{
				Config: remoteFileConfig(srv.URL+"/f", adopted, sum, ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileMode(adopted, 0o644),
					checkHits(srv, 0),
				),
			},
		},
	})

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkNotExist(imported),
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf("import {\n  to = %s\n  id = %q\n}\n", testRemoteFileResource, imported) +
					remoteFileConfig(srv.URL+"/g", imported, sum, `  mode = "0640"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testRemoteFileResource, "url", srv.URL+"/g"),
					resource.TestCheckResourceAttr(testRemoteFileResource, "checksum", sum),
					resource.TestCheckResourceAttr(testRemoteFileResource, "sha256", sha256Hex([]byte(content))),
					checkFileMode(imported, 0o640),
					checkHits(srv, 0),
				),
			},
		},
	})
}
