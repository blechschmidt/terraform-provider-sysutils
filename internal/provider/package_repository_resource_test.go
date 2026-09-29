package provider

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

const testRepoResource = "sysutils_package_repository.test"

// repoTestEnv is a fake system for sysutils_package_repository: a
// temporary root with the configuration directories of one family, a TLS
// server that serves signing keys, and a fake package manager that records
// index refreshes.
type repoTestEnv struct {
	root string
	srv  *httptest.Server
	// key is what the server serves at /key.gpg.
	key  atomic.Pointer[[]byte]
	pkg  *fakePackageManager
	cfg  *repoConfig
	hits atomic.Int32
}

func newRepoTestEnv(t *testing.T, kind string, dirs ...string) *repoTestEnv {
	t.Helper()
	env := &repoTestEnv{root: t.TempDir(), pkg: newFakePackageManager(kind)}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(env.root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	key := append([]byte{}, testPublicKey...)
	env.key.Store(&key)
	env.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/key.gpg" {
			http.NotFound(w, r)
			return
		}
		env.hits.Add(1)
		_, _ = w.Write(*env.key.Load())
	}))
	t.Cleanup(env.srv.Close)
	root, err := newFSRoot(env.root)
	if err != nil {
		t.Fatal(err)
	}
	env.cfg = &repoConfig{uid: uint32(os.Getuid()), gid: uint32(os.Getgid()), httpClient: env.srv.Client(), root: root}
	return env
}

func (env *repoTestEnv) factories() map[string]func() (tfprotov6.ProviderServer, error) {
	f := env.pkg
	return map[string]func() (tfprotov6.ProviderServer, error){
		"sysutils": providerserver.NewProtocol6WithError(&sysutilsProvider{
			version: "test",
			repo:    env.cfg,
			pkg: &packageConfig{manager: func(kind string) (packageManager, error) {
				if kind != packageManagerAuto && kind != f.kind {
					return nil, fmt.Errorf("package manager %s is not available", kind)
				}
				return f, nil
			}},
		}),
	}
}

// path returns the host path of the managed path p.
func (env *repoTestEnv) path(p string) string { return filepath.Join(env.root, p) }

func (env *repoTestEnv) updates() int {
	n := 0
	for _, c := range env.pkg.changes() {
		if c == "update" {
			n++
		}
	}
	return n
}

func repoHCL(body string) string {
	return fmt.Sprintf(`
resource "sysutils_package_repository" "test" {
%s
}
`, body)
}

// checkRepoFile checks that p contains want with mode 0644.
func checkRepoFile(p, want string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if string(data) != want {
			return fmt.Errorf("%s =\n%s\nwant\n%s", p, data, want)
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if info.Mode() != repoFileMode {
			return fmt.Errorf("%s has mode %v, want %v", p, info.Mode(), repoFileMode)
		}
		return nil
	}
}

func checkRepoGone(paths ...string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		for _, p := range paths {
			if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("%s still exists (err = %v)", p, err)
			}
		}
		return nil
	}
}

func checkRepoUpdates(env *repoTestEnv, want int) resource.TestCheckFunc {
	return func(*terraform.State) error {
		if got := env.updates(); got != want {
			return fmt.Errorf("package index refreshed %d times, want %d (calls %v)", got, want, env.pkg.changes())
		}
		return nil
	}
}

func mutateFile(t *testing.T, p string, f func(string) string) {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(f(string(data))), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPackageRepositoryResource_apt(t *testing.T) {
	env := newRepoTestEnv(t, packageManagerApt, "etc/apt")
	src := env.path("etc/apt/sources.list.d/docker.sources")
	key := env.path("etc/apt/keyrings/docker.asc")
	inline := strings.ReplaceAll(testArmoredKey, "\n", "\\n")
	content := repoFileHeader + `
X-Repolib-Name: Docker CE
Types: deb
URIs: https://download.docker.com/linux/debian
Suites: bookworm
Components: stable
Signed-By: /etc/apt/keyrings/docker.asc
`
	config := repoHCL(fmt.Sprintf(`
  name          = "docker"
  description   = "Docker CE"
  uris          = ["https://download.docker.com/linux/debian"]
  suites        = ["bookworm"]
  components    = ["stable"]
  signing_key   = "%s"
  refresh_cache = true
`, inline))

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: env.factories(),
		CheckDestroy:             checkRepoGone(src, key),
		Steps: []resource.TestStep{
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectKnownValue(testRepoResource, tfjsonpath.New("content"), knownvalue.StringExact(content)),
						plancheck.ExpectKnownValue(testRepoResource, tfjsonpath.New("path"), knownvalue.StringExact("/etc/apt/sources.list.d/docker.sources")),
						plancheck.ExpectKnownValue(testRepoResource, tfjsonpath.New("signing_key_path"), knownvalue.StringExact("/etc/apt/keyrings/docker.asc")),
						plancheck.ExpectKnownValue(testRepoResource, tfjsonpath.New("signing_key_sha256"), knownvalue.StringExact(sha256Hex([]byte(testArmoredKey)))),
					},
				},
				Check: resource.ComposeTestCheckFunc(
					checkRepoFile(src, content),
					checkRepoFile(key, testArmoredKey),
					checkRepoUpdates(env, 1),
				),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(testRepoResource, tfjsonpath.New("id"), knownvalue.StringExact("docker")),
					statecheck.ExpectKnownValue(testRepoResource, tfjsonpath.New("manager"), knownvalue.StringExact("auto")),
				},
			},
			{
				// No change, no refresh.
				Config:   config,
				PlanOnly: true,
			},
			{
				// Drift in the repository file is repaired and refreshes the
				// index.
				PreConfig: func() {
					mutateFile(t, src, func(s string) string { return strings.Replace(s, "stable", "nightly", 1) })
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testRepoResource, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeTestCheckFunc(checkRepoFile(src, content), checkRepoUpdates(env, 2)),
			},
			{
				// So is drift in the key file, including its mode.
				PreConfig: func() {
					mutateFile(t, key, func(string) string { return "tampered\n" })
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testRepoResource, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeTestCheckFunc(checkRepoFile(key, testArmoredKey), checkRepoUpdates(env, 3)),
			},
			{
				PreConfig: func() {
					if err := os.Chmod(key, 0o600); err != nil {
						t.Fatal(err)
					}
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testRepoResource, plancheck.ResourceActionUpdate)},
				},
				Check: checkRepoFile(key, testArmoredKey),
			},
			{
				// Import reads every attribute back.
				ResourceName:            testRepoResource,
				ImportState:             true,
				ImportStateId:           "docker",
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"refresh_cache"},
			},
			{
				// Without the key, the key file is removed; a disabled,
				// flat repository with several URIs.
				Config: repoHCL(`
  name    = "docker"
  uris    = ["https://download.docker.com/linux/debian", "file:///srv/mirror"]
  suites  = ["./"]
  types   = ["deb", "deb-src"]
  enabled = false
`),
				Check: resource.ComposeTestCheckFunc(
					checkRepoFile(src, repoFileHeader+"\nTypes: deb deb-src\nURIs: https://download.docker.com/linux/debian file:///srv/mirror\nSuites: ./\nEnabled: no\n"),
					checkRepoGone(key),
				),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(testRepoResource, tfjsonpath.New("signing_key_path"), knownvalue.Null()),
					statecheck.ExpectKnownValue(testRepoResource, tfjsonpath.New("signing_key_sha256"), knownvalue.Null()),
				},
			},
			{
				ResourceName:            testRepoResource,
				ImportState:             true,
				ImportStateId:           "apt:docker",
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"refresh_cache", "manager"},
			},
		},
	})
}

func TestPackageRepositoryResource_aptKeyURL(t *testing.T) {
	env := newRepoTestEnv(t, packageManagerApt, "etc/apt")
	key := env.path("etc/apt/keyrings/ext.asc")
	config := repoHCL(fmt.Sprintf(`
  name            = "ext"
  uris            = ["https://example.com/debian"]
  suites          = ["stable"]
  components      = ["main"]
  signing_key_url = "%s/key.gpg"
`, env.srv.URL))
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: env.factories(),
		CheckDestroy:             checkRepoGone(key),
		Steps: []resource.TestStep{
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectUnknownValue(testRepoResource, tfjsonpath.New("signing_key_sha256")),
						plancheck.ExpectKnownValue(testRepoResource, tfjsonpath.New("content"), knownvalue.StringRegexp(regexp.MustCompile(`Signed-By: /etc/apt/keyrings/ext.asc\n`))),
					},
				},
				// The binary key is stored armored.
				Check: resource.ComposeTestCheckFunc(
					checkRepoFile(key, testArmoredKey),
					resource.TestCheckResourceAttr(testRepoResource, "signing_key_sha256", sha256Hex([]byte(testArmoredKey))),
					checkRepoUpdates(env, 0),
				),
			},
			{
				// The key behind the URL is not fetched again during plans.
				PreConfig: func() {
					// With a User ID packet.
					other := append(append([]byte{}, testPublicKey...), 0xcd, 0x01, 'x')
					env.key.Store(&other)
				},
				Config:   config,
				PlanOnly: true,
			},
			{
				// A changed key file is fetched again, now with the new key.
				PreConfig: func() {
					if n := env.hits.Load(); n != 1 {
						t.Errorf("key fetched %d times, want 1", n)
					}
					mutateFile(t, key, func(string) string { return "tampered\n" })
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(testRepoResource, plancheck.ResourceActionUpdate),
						plancheck.ExpectUnknownValue(testRepoResource, tfjsonpath.New("signing_key_sha256")),
					},
				},
				Check: func(s *terraform.State) error {
					want := string(armorPublicKey(*env.key.Load()))
					return checkRepoFile(key, want)(s)
				},
			},
			{
				Config:   config,
				PlanOnly: true,
			},
		},
	})
}

func TestPackageRepositoryResource_dnf(t *testing.T) {
	env := newRepoTestEnv(t, packageManagerDnf, "etc/yum.repos.d")
	repo := env.path("etc/yum.repos.d/pgdg.repo")
	key := env.path("etc/pki/rpm-gpg/RPM-GPG-KEY-pgdg")
	inline := strings.ReplaceAll(testArmoredKey, "\n", "\\n")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: env.factories(),
		CheckDestroy:             checkRepoGone(repo, key),
		Steps: []resource.TestStep{
			{
				Config: repoHCL(`
  name            = "pgdg"
  description     = "PostgreSQL 16"
  uris            = ["https://download.postgresql.org/pub/repos/yum/16/fedora/fedora-$releasever-$basearch"]
  signing_key_url = "https://download.postgresql.org/pub/repos/yum/keys/PGDG-RPM-GPG-KEY-Fedora"
  refresh_cache   = true
`),
				Check: resource.ComposeTestCheckFunc(
					checkRepoFile(repo, repoFileHeader+`
[pgdg]
name=PostgreSQL 16
baseurl=https://download.postgresql.org/pub/repos/yum/16/fedora/fedora-$releasever-$basearch
enabled=1
gpgcheck=1
gpgkey=https://download.postgresql.org/pub/repos/yum/keys/PGDG-RPM-GPG-KEY-Fedora
`),
					checkRepoGone(key),
					resource.TestCheckNoResourceAttr(testRepoResource, "signing_key_path"),
					checkRepoUpdates(env, 1),
				),
			},
			{
				ResourceName:            testRepoResource,
				ImportState:             true,
				ImportStateId:           "pgdg",
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"refresh_cache"},
			},
			{
				// An inline key is stored in /etc/pki/rpm-gpg.
				Config: repoHCL(fmt.Sprintf(`
  name        = "pgdg"
  uris        = ["file:///srv/rpms", "https://mirror.example/rpms"]
  gpg_check   = false
  enabled     = false
  signing_key = "%s"
`, inline)),
				Check: resource.ComposeTestCheckFunc(
					checkRepoFile(repo, repoFileHeader+`
[pgdg]
name=pgdg
baseurl=file:///srv/rpms,https://mirror.example/rpms
enabled=0
gpgcheck=0
gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-pgdg
`),
					checkRepoFile(key, testArmoredKey),
					// refresh_cache is off now.
					checkRepoUpdates(env, 1),
				),
			},
			{
				ResourceName:      testRepoResource,
				ImportState:       true,
				ImportStateId:     "dnf:pgdg",
				ImportStateVerify: true,
				// manager is "dnf" after this import.
				ImportStateVerifyIgnore: []string{"manager"},
			},
		},
	})
}

func TestPackageRepositoryResource_apk(t *testing.T) {
	env := newRepoTestEnv(t, packageManagerApk, "etc/apk")
	p := env.path("etc/apk/repositories")
	orig := "https://dl-cdn.alpinelinux.org/alpine/v3.20/main\nhttps://dl-cdn.alpinelinux.org/alpine/v3.20/community\n"
	if err := os.WriteFile(p, []byte(orig), 0o640); err != nil {
		t.Fatal(err)
	}
	config := repoHCL(`
  name          = "testing"
  description   = "Edge testing"
  uris          = ["https://dl-cdn.alpinelinux.org/alpine/edge/testing"]
  tag           = "testing"
  refresh_cache = true
`)
	block := "# sysutils_package_repository testing: Edge testing\n@testing https://dl-cdn.alpinelinux.org/alpine/edge/testing\n"
	checkFile := func(want string) resource.TestCheckFunc {
		return func(*terraform.State) error {
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if string(data) != want {
				return fmt.Errorf("%s =\n%s\nwant\n%s", p, data, want)
			}
			// The file's own mode is kept.
			if info, err := os.Stat(p); err != nil || info.Mode().Perm() != 0o640 {
				return fmt.Errorf("mode of %s changed: %v %v", p, info.Mode(), err)
			}
			return nil
		}
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: env.factories(),
		CheckDestroy:             checkFile(orig),
		Steps: []resource.TestStep{
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectKnownValue(testRepoResource, tfjsonpath.New("content"), knownvalue.StringExact(block)),
						plancheck.ExpectKnownValue(testRepoResource, tfjsonpath.New("path"), knownvalue.StringExact("/etc/apk/repositories")),
					},
				},
				Check: resource.ComposeTestCheckFunc(checkFile(orig+block), checkRepoUpdates(env, 1)),
			},
			{
				// Lines added by others are left alone; a changed managed
				// line is drift.
				PreConfig: func() {
					mutateFile(t, p, func(s string) string {
						return strings.Replace(s, "@testing https", "@testing http", 1) + "https://example.com/other\n"
					})
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testRepoResource, plancheck.ResourceActionUpdate)},
				},
				Check: checkFile(orig + block + "https://example.com/other\n"),
			},
			{
				ResourceName:            testRepoResource,
				ImportState:             true,
				ImportStateId:           "testing",
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"refresh_cache"},
			},
			{
				Config: repoHCL(`
  name    = "testing"
  uris    = ["https://dl-cdn.alpinelinux.org/alpine/edge/testing"]
  enabled = false
`),
				Check: checkFile(orig + "# sysutils_package_repository testing\n#https://dl-cdn.alpinelinux.org/alpine/edge/testing\nhttps://example.com/other\n"),
			},
			{
				// The managed lines removed outside Terraform: the resource
				// is gone and created again.
				PreConfig: func() {
					mutateFile(t, p, func(string) string { return orig })
					if err := os.Chmod(p, 0o640); err != nil {
						t.Fatal(err)
					}
				},
				Config: repoHCL(`
  name    = "testing"
  uris    = ["https://dl-cdn.alpinelinux.org/alpine/edge/testing"]
  enabled = false
`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testRepoResource, plancheck.ResourceActionCreate)},
				},
				Check: checkFile(orig + "# sysutils_package_repository testing\n#https://dl-cdn.alpinelinux.org/alpine/edge/testing\n"),
			},
		},
	})
}

// A failed refresh_cache fails the apply and is retried by the next one,
// even though the repository file is already up to date.
func TestPackageRepositoryResource_refreshRetry(t *testing.T) {
	env := newRepoTestEnv(t, packageManagerApt, "etc/apt")
	config := repoHCL(`
  name          = "r"
  uris          = ["https://example.com/debian"]
  suites        = ["stable"]
  components    = ["main"]
  refresh_cache = true
`)
	changed := repoHCL(`
  name          = "r"
  uris          = ["https://example.com/debian"]
  suites        = ["stable"]
  components    = ["main", "contrib"]
  refresh_cache = true
`)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: env.factories(),
		Steps: []resource.TestStep{
			{Config: config, Check: checkRepoUpdates(env, 1)},
			{
				PreConfig:   func() { env.pkg.updateErr = errors.New("E: Failed to fetch") },
				Config:      changed,
				ExpectError: regexp.MustCompile(`refreshing\s+the\s+package\s+index\s+failed`),
			},
			{
				// The pending refresh plans an update.
				PreConfig: func() { env.pkg.updateErr = nil },
				Config:    changed,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testRepoResource, plancheck.ResourceActionUpdate)},
				},
				Check: checkRepoUpdates(env, 3),
			},
			{Config: changed, PlanOnly: true},
		},
	})
}

func TestPackageRepositoryResource_existing(t *testing.T) {
	env := newRepoTestEnv(t, packageManagerApt, "etc/apt/sources.list.d", "etc/apt/keyrings")
	if err := os.WriteFile(env.path("etc/apt/sources.list.d/taken.sources"), []byte("Types: deb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.path("etc/apt/keyrings/keyed.asc"), []byte("admin key\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	inline := strings.ReplaceAll(testArmoredKey, "\n", "\\n")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: env.factories(),
		Steps: []resource.TestStep{
			{
				Config: repoHCL(`
  name       = "taken"
  uris       = ["https://example.com/debian"]
  suites     = ["stable"]
  components = ["main"]
`),
				ExpectError: regexp.MustCompile(`already\s+exists`),
			},
			{
				// An existing key file is not overwritten either.
				Config: repoHCL(fmt.Sprintf(`
  name        = "keyed"
  uris        = ["https://example.com/debian"]
  suites      = ["stable"]
  components  = ["main"]
  signing_key = "%s"
`, inline)),
				ExpectError: regexp.MustCompile(`already\s+exists`),
				Check:       checkRepoFile(env.path("etc/apt/keyrings/keyed.asc"), "admin key\n"),
			},
		},
	})
}

func TestPackageRepositoryResource_validation(t *testing.T) {
	env := newRepoTestEnv(t, packageManagerApt, "etc/apt")
	apk := newRepoTestEnv(t, packageManagerApk, "etc/apk")
	dnf := newRepoTestEnv(t, packageManagerDnf, "etc/yum.repos.d")
	steps := []struct {
		env    *repoTestEnv
		config string
		want   string
	}{
		{env, `name = "x/y"` + "\n" + `uris = ["https://e.com/"]`, `Invalid\s+repository\s+name`},
		{env, `name = "x"` + "\n" + `uris = ["https://e.com/\nSigned-By: /tmp/k"]`, `white\s+space`},
		{env, `name = "x"` + "\n" + `uris = ["ftp://e.com/"]`, `must\s+be\s+a\s+http`},
		{env, `name = "x"` + "\n" + `uris = []`, `at\s+least\s+1`},
		{env, `name = "x"` + "\n" + `uris = ["https://e.com/"]`, `suites\s+must\s+be\s+set`},
		{env, `name = "x"` + "\n" + `uris = ["https://e.com/"]` + "\n" + `suites = ["s"]` + "\n" + `components = ["main"]` + "\n" + `signing_key_url = "http://e.com/key"`, `must\s+be\s+a\s+https`},
		{env, `name = "x"` + "\n" + `uris = ["https://e.com/"]` + "\n" + `suites = ["s"]` + "\n" + `components = ["main"]` + "\n" + `signing_key = "junk"`, `Invalid\s+signing\s+key`},
		{env, `name = "x"` + "\n" + `uris = ["https://e.com/"]` + "\n" + `suites = ["s"]` + "\n" + `components = ["main"]` + "\n" + `signing_key = "k"` + "\n" + `signing_key_url = "https://e.com/k"`, `cannot\s+be\s+configured\s+together`},
		{env, `name = "x"` + "\n" + `uris = ["https://e.com/"]` + "\n" + `suites = ["s"]` + "\n" + `components = ["main"]` + "\n" + `tag = "t"`, `tag\s+is\s+not\s+supported`},
		{apk, `name = "x"` + "\n" + `uris = ["https://e.com/", "https://f.com/"]`, `exactly\s+one`},
		{apk, `name = "x"` + "\n" + `uris = ["https://e.com/"]` + "\n" + `signing_key_url = "https://e.com/k"`, `signing\s+key\s+is\s+not\s+supported`},
		{env, `name = "x"` + "\n" + `manager = "pacman"` + "\n" + `uris = ["https://e.com/"]`, `value\s+must\s+be\s+one\s+of`},
		// dnf reads gpgkey as a list of URLs: a comma would add a second key.
		{dnf, `name = "x"` + "\n" + `uris = ["https://e.com/"]` + "\n" + `signing_key_url = "https://e.com/k,https://evil.example/k"`, `must\s+not\s+contain\s+","`},
	}
	for i, s := range steps {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: s.env.factories(),
				Steps: []resource.TestStep{{
					Config:      repoHCL(s.config),
					PlanOnly:    true,
					ExpectError: regexp.MustCompile(s.want),
				}},
			})
		})
	}
}

// root_dir confines the files to the tree, and refresh_cache is refused.
func TestPackageRepositoryResource_rootDir(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc/yum.repos.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A symlink in the tree must not lead out of it.
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "etc/pki")); err != nil {
		t.Fatal(err)
	}
	f := newFakePackageManager(packageManagerDnf)
	factories := map[string]func() (tfprotov6.ProviderServer, error){
		"sysutils": providerserver.NewProtocol6WithError(&sysutilsProvider{
			version: "test",
			repo:    &repoConfig{uid: uint32(os.Getuid()), gid: uint32(os.Getgid())},
			pkg:     &packageConfig{manager: func(string) (packageManager, error) { return f, nil }},
		}),
	}
	provider := fmt.Sprintf("provider \"sysutils\" {\n  root_dir = %q\n}\n", root)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config: provider + repoHCL(`
  name          = "img"
  uris          = ["https://example.com/rpms"]
  refresh_cache = true
`),
				ExpectError: regexp.MustCompile(`Not\s+supported\s+with\s+root_dir`),
			},
			{
				Config: provider + repoHCL(`
  name = "img"
  uris = ["https://example.com/rpms"]
`),
				Check: resource.ComposeTestCheckFunc(
					checkRepoFile(filepath.Join(root, "etc/yum.repos.d/img.repo"), repoFileHeader+"\n[img]\nname=img\nbaseurl=https://example.com/rpms\nenabled=1\ngpgcheck=1\n"),
					resource.TestCheckResourceAttr(testRepoResource, "path", "/etc/yum.repos.d/img.repo"),
				),
			},
			{
				// The symlinked /etc/pki resolves inside the root, so the key
				// lands in <root>/<outside>/rpm-gpg... or fails; it never
				// lands in the real outside directory.
				Config: provider + repoHCL(fmt.Sprintf(`
  name        = "img"
  uris        = ["https://example.com/rpms"]
  signing_key = "%s"
`, strings.ReplaceAll(testArmoredKey, "\n", "\\n"))),
				Check: func(*terraform.State) error {
					entries, err := os.ReadDir(outside)
					if err != nil {
						return err
					}
					if len(entries) != 0 {
						return fmt.Errorf("key written outside root_dir: %v", entries)
					}
					return nil
				},
			},
		},
	})
	if slices.Contains(f.changes(), "update") {
		t.Error("package index refreshed with root_dir")
	}
}

// A create that fails because the repository file already exists removes
// the key it wrote and records nothing, so that no later destroy of a
// tainted resource removes the file it does not own.
func TestPackageRepositoryResource_existingWithKey(t *testing.T) {
	env := newRepoTestEnv(t, packageManagerApt, "etc/apt/sources.list.d", "etc/apt/keyrings")
	foreign := env.path("etc/apt/sources.list.d/taken.sources")
	if err := os.WriteFile(foreign, []byte("Types: deb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	key := env.path("etc/apt/keyrings/taken.asc")
	checkForeign := func(*terraform.State) error {
		if err := checkRepoFile(foreign, "Types: deb\n")(nil); err != nil {
			return err
		}
		return checkRepoGone(key)(nil)
	}
	inline := strings.ReplaceAll(testArmoredKey, "\n", "\\n")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: env.factories(),
		CheckDestroy:             checkForeign,
		Steps: []resource.TestStep{
			{
				Config: repoHCL(fmt.Sprintf(`
  name        = "taken"
  uris        = ["https://example.com/debian"]
  suites      = ["stable"]
  components  = ["main"]
  signing_key = "%s"
`, inline)),
				ExpectError: regexp.MustCompile(`already\s+exists`),
			},
			{
				// Would replace a tainted resource, destroying the file.
				Config: repoHCL(`
  name       = "other"
  uris       = ["https://example.com/debian"]
  suites     = ["stable"]
  components = ["main"]
`),
				Check: checkForeign,
			},
		},
	})
}

// A line of someone else that follows the marker, because the managed line
// was deleted by hand, is neither replaced by an update nor removed by
// destroy.
func TestPackageRepositoryResource_apkForeignLine(t *testing.T) {
	env := newRepoTestEnv(t, packageManagerApk, "etc/apk")
	p := env.path("etc/apk/repositories")
	orig := "https://dl-cdn.alpinelinux.org/alpine/v3.20/main\n"
	if err := os.WriteFile(p, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	marker := "# sysutils_package_repository testing\n"
	line := "@testing https://dl-cdn.alpinelinux.org/alpine/edge/testing\n"
	foreign := "https://dl-cdn.alpinelinux.org/alpine/v3.20/community\n"
	checkFile := func(want string) resource.TestCheckFunc {
		return func(*terraform.State) error {
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if string(data) != want {
				return fmt.Errorf("%s =\n%s\nwant\n%s", p, data, want)
			}
			return nil
		}
	}
	dropManagedLine := func() { mutateFile(t, p, func(s string) string { return strings.Replace(s, line, "", 1) }) }
	config := repoHCL(`
  name = "testing"
  uris = ["https://dl-cdn.alpinelinux.org/alpine/edge/testing"]
  tag  = "testing"
`)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: env.factories(),
		CheckDestroy:             checkFile(orig + foreign),
		Steps: []resource.TestStep{
			{Config: config, Check: checkFile(orig + marker + line)},
			{
				PreConfig: func() {
					dropManagedLine()
					mutateFile(t, p, func(s string) string { return s + foreign })
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testRepoResource, plancheck.ResourceActionUpdate)},
				},
				Check: checkFile(orig + marker + line + foreign),
			},
			{
				// The destroy after this step must keep the foreign line.
				PreConfig:          dropManagedLine,
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
}
