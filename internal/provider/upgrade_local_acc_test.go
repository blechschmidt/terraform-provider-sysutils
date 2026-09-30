package provider

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// Upgrade tests from a local baseline build. The registry upgrade tests in
// upgrade_acc_test.go can only cover resources that are in a release. These
// tests instead build the provider at an earlier git commit, apply a
// configuration with that build, and then check that the current code plans
// no changes for it and keeps every attribute the baseline stored in state.
// That catches incompatible schema changes to resources that have not been
// released yet, before the first release that ships them fixes their state
// format for good.
//
// The baseline is installed into a temporary filesystem mirror, and
// Terraform is pointed at it with a provider_installation block in a CLI
// configuration file (TF_CLI_CONFIG_FILE). dev_overrides can't be used:
// with it, "tofu init" fails for a provider that is not in its registry.
//
// The tests run only if SYSUTILS_UPGRADE_FROM_REF is set:
//
//   - "auto": for each resource, the merge base of HEAD and main (or
//     origin/main) if the resource exists there, otherwise the commit that
//     added the resource. On main itself the merge base is HEAD, so the
//     latest v* release tag before HEAD (or HEAD^ if there is none) is used
//     instead.
//   - any other value: a git ref such as "v1.0.1", "main" or "HEAD~3". A
//     resource that does not exist at that ref is tested from the commit that
//     added it instead.
//
// They need git, the repository's history (not a shallow clone) and the Go
// toolchain, and network access if the baseline needs modules that are not in
// the module cache.

const upgradeFromRefEnv = "SYSUTILS_UPGRADE_FROM_REF"

// localBaseline is a provider binary built at commit and installed as
// version into the filesystem mirror below mirror.
type localBaseline struct {
	commit  string
	version string
	mirror  string
}

var localBaselines = struct {
	sync.Mutex
	dir    string
	builds map[string]*localBaselineBuild
}{builds: map[string]*localBaselineBuild{}}

type localBaselineBuild struct {
	once sync.Once
	b    localBaseline
	err  error
}

func TestMain(m *testing.M) {
	code := m.Run()
	localBaselines.Lock()
	if localBaselines.dir != "" {
		_ = os.RemoveAll(localBaselines.dir)
	}
	localBaselines.Unlock()
	os.Exit(code)
}

// upgradeGit runs git in the top-level directory of the repository (dir "")
// or in dir and returns its standard output without the trailing newline.
// safe.directory lets root use a repository owned by another user, such as
// the .git directory the test container mounts from the host.
func upgradeGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-c", "safe.directory=*"}, args...)...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// upgradeRepoRoot returns the top-level directory of the git repository the
// tests run in.
func upgradeRepoRoot(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("%s is set, but git is not installed: %v", upgradeFromRefEnv, err)
	}
	root, err := upgradeGit("", "rev-parse", "--show-toplevel")
	if err != nil {
		t.Fatalf("%s is set, but the tests do not run in a git repository: %v", upgradeFromRefEnv, err)
	}
	return root
}

func upgradeResolveCommit(root, ref string) (string, error) {
	return upgradeGit(root, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
}

// upgradeBaselineCommit returns the commit to build the baseline of
// resourceType from, following SYSUTILS_UPGRADE_FROM_REF (see above), and
// skips the test if it is not set.
func upgradeBaselineCommit(t *testing.T, root, resourceType string) string {
	t.Helper()
	ref := os.Getenv(upgradeFromRefEnv)
	if ref == "" {
		t.Skipf("%s is not set; set it to a git ref or to \"auto\" to test upgrades from a local build", upgradeFromRefEnv)
	}

	var base string
	if ref == "auto" {
		var mainCommit string
		for _, candidate := range []string{"main", "origin/main"} {
			if c, err := upgradeResolveCommit(root, candidate); err == nil {
				mainCommit = c
				break
			}
		}
		if mainCommit == "" {
			t.Fatalf("%s=auto: neither main nor origin/main exists; fetch main or set a ref", upgradeFromRefEnv)
		}
		var err error
		if base, err = upgradeGit(root, "merge-base", "HEAD", mainCommit); err != nil {
			t.Fatalf("%s=auto: %v (a shallow clone has no merge base; fetch the full history)", upgradeFromRefEnv, err)
		}
		// On main (or a commit already merged into it), upgrading from
		// the merge base would test HEAD against itself.
		if head, err := upgradeResolveCommit(root, "HEAD"); err == nil && head == base {
			prev := "HEAD^"
			if tag, err := upgradeGit(root, "describe", "--tags", "--abbrev=0", "--match", "v*", "HEAD^"); err == nil {
				prev = tag
			}
			if base, err = upgradeResolveCommit(root, prev); err != nil {
				t.Fatalf("%s=auto: HEAD is on main and %s is not a commit: %v", upgradeFromRefEnv, prev, err)
			}
		}
	} else {
		var err error
		if base, err = upgradeResolveCommit(root, ref); err != nil {
			t.Fatalf("%s=%s is not a commit: %v", upgradeFromRefEnv, ref, err)
		}
	}

	// Resources are defined in internal/provider/<name>_resource.go.
	source := "internal/provider/" + strings.TrimPrefix(resourceType, "sysutils_") + "_resource.go"
	if _, err := upgradeGit(root, "cat-file", "-e", base+":"+source); err == nil {
		t.Logf("upgrading %s from %s (%s=%s)", resourceType, base, upgradeFromRefEnv, ref)
		return base
	}
	added, err := upgradeGit(root, "log", "--diff-filter=A", "--format=%H", "HEAD", "--", source)
	if err != nil {
		t.Fatal(err)
	}
	if added == "" {
		t.Skipf("%s is not in any commit yet, so there is no earlier build to upgrade from", resourceType)
	}
	// The oldest commit that added the file comes last.
	commit := added[strings.LastIndexByte(added, '\n')+1:]
	t.Logf("upgrading %s from %s, the commit that added it: it does not exist at %s (%s=%s)", resourceType, commit, base, upgradeFromRefEnv, ref)
	return commit
}

// upgradeLocalBaseline returns the baseline build for resourceType, building
// it on first use. Builds are shared by all tests of a run and removed when
// the run ends.
func upgradeLocalBaseline(t *testing.T, resourceType string) localBaseline {
	t.Helper()
	// Skip before looking for git and the repository, which are only
	// needed, and only mounted into the test container, when it is set.
	if os.Getenv(upgradeFromRefEnv) == "" {
		t.Skipf("%s is not set; set it to a git ref or to \"auto\" to test upgrades from a local build", upgradeFromRefEnv)
	}
	root := upgradeRepoRoot(t)
	commit := upgradeBaselineCommit(t, root, resourceType)

	localBaselines.Lock()
	if localBaselines.dir == "" {
		dir, err := os.MkdirTemp("", "sysutils-upgrade-")
		if err != nil {
			localBaselines.Unlock()
			t.Fatal(err)
		}
		localBaselines.dir = dir
	}
	dir := localBaselines.dir
	build := localBaselines.builds[commit]
	if build == nil {
		build = &localBaselineBuild{}
		localBaselines.builds[commit] = build
	}
	localBaselines.Unlock()

	build.once.Do(func() {
		build.b, build.err = buildLocalBaseline(root, commit, filepath.Join(dir, commit))
	})
	if build.err != nil {
		t.Fatalf("building the baseline provider at %s: %v", commit, build.err)
	}
	return build.b
}

// buildLocalBaseline builds the provider at commit into a filesystem mirror
// below dir. Its version is 0.0.N, N being the number of commits up to and
// including commit, so that different baselines have different versions.
func buildLocalBaseline(root, commit, dir string) (localBaseline, error) {
	count, err := upgradeGit(root, "rev-list", "--count", commit)
	if err != nil {
		return localBaseline{}, err
	}
	if _, err := strconv.Atoi(count); err != nil {
		return localBaseline{}, fmt.Errorf("git rev-list --count %s printed %q", commit, count)
	}
	b := localBaseline{commit: commit, version: "0.0." + count, mirror: filepath.Join(dir, "mirror")}

	src := filepath.Join(dir, "src")
	if err := upgradeExtractCommit(root, commit, src); err != nil {
		return localBaseline{}, err
	}

	// The unpacked layout of a filesystem mirror:
	// HOSTNAME/NAMESPACE/TYPE/VERSION/TARGET/terraform-provider-TYPE_vVERSION.
	bin := filepath.Join(b.mirror, upgradeProviderHost, upgradeProviderNamespace, "sysutils", b.version,
		runtime.GOOS+"_"+runtime.GOARCH, "terraform-provider-sysutils_v"+b.version)
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", "-X main.version="+b.version, "-o", bin, ".")
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=readonly")
	if out, err := cmd.CombinedOutput(); err != nil {
		return localBaseline{}, fmt.Errorf("go build: %w: %s", err, out)
	}
	// The sources are no longer needed; the binary is.
	_ = os.RemoveAll(src)
	return b, nil
}

// upgradeExtractCommit writes the tree of commit to dir with git archive.
func upgradeExtractCommit(root, commit, dir string) error {
	cmd := exec.Command("git", "-c", "safe.directory=*", "archive", "--format=tar", commit)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	extractErr := extractTar(stdout, dir)
	// Drain the pipe so that git can exit if extraction stopped early.
	_, _ = io.Copy(io.Discard, stdout)
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("git archive %s: %w: %s", commit, err, strings.TrimSpace(stderr.String()))
	}
	return extractErr
}

func extractTar(r io.Reader, dir string) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(h.Name)
		if !filepath.IsLocal(name) {
			return fmt.Errorf("git archive contains the non-local path %q", h.Name)
		}
		p := filepath.Join(dir, name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(h.Mode)&0o755|0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(h.Linkname, p); err != nil {
				return err
			}
		case tar.TypeXGlobalHeader:
			// The commit ID git archive records; nothing to extract.
		default:
			return fmt.Errorf("git archive contains %q of unsupported type %c", h.Name, h.Typeflag)
		}
	}
}

// localUpgradeSteps returns the steps of an upgrade test for resourceType
// from a local baseline build (see above):
//
//  1. apply config with the baseline provider and record the state;
//  2. plan config with the current code, which must plan no changes, then
//     apply that empty plan, which stores the upgraded state; checks run
//     here, and every attribute of the recorded state must still have its
//     value (attributes the baseline did not have may be added);
//  3. plan config again with the current code, with and without a refresh:
//     both plans must be empty.
//
// Step 2 plans with a refresh, as "terraform plan" does. A plan without one
// is only checked after the upgraded state has been stored: until then,
// Terraform plans an in-place update without any changed value for a
// resource whose schema gained a sensitive attribute since the baseline,
// because the state has no sensitivity marks for it yet.
//
// The test case destroys with the current code.
func localUpgradeSteps(t *testing.T, resourceType, config string, checks ...statecheck.StateCheck) []resource.TestStep {
	t.Helper()
	return localUpgradeStepsChanging(t, resourceType, config, nil, checks...)
}

// localUpgradeStepsChanging is localUpgradeSteps for a resource whose
// upgrade changes stored attributes on purpose. changed lists them as
// "<resource address>.<attribute>"; they may, but need not, change.
func localUpgradeStepsChanging(t *testing.T, resourceType, config string, changed []string, checks ...statecheck.StateCheck) []resource.TestStep {
	t.Helper()
	// resource.Test would skip the test, but only after the baseline has
	// been built.
	if os.Getenv("TF_ACC") == "" {
		t.Skip("acceptance test; set TF_ACC=1 to run")
	}
	requireRoot(t)
	b := upgradeLocalBaseline(t, resourceType)

	// Only the sysutils provider comes from the mirror; with the CLI
	// configuration replaced, a plugin cache of the user can't hand out a
	// different binary of the same version either.
	addr := upgradeProviderHost + "/" + upgradeProviderNamespace + "/sysutils"
	cliConfig := filepath.Join(t.TempDir(), "upgrade.tfrc")
	if err := os.WriteFile(cliConfig, fmt.Appendf(nil, `provider_installation {
  filesystem_mirror {
    path    = %q
    include = [%q]
  }
  direct {
    exclude = [%q]
  }
}
`, b.mirror, addr, addr), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TF_CLI_CONFIG_FILE", cliConfig)
	t.Setenv("TF_PLUGIN_CACHE_DIR", "")
	// The in-process provider of the later steps must have the baseline's
	// address to take over its state, with Terraform and with OpenTofu.
	t.Setenv("TF_ACC_PROVIDER_HOST", upgradeProviderHost)
	t.Setenv("TF_ACC_PROVIDER_NAMESPACE", upgradeProviderNamespace)

	var before map[string]map[string]string
	return []resource.TestStep{
		{
			ExternalProviders: map[string]resource.ExternalProvider{
				"sysutils": {Source: addr, VersionConstraint: b.version},
			},
			Config: config,
			Check: func(s *terraform.State) error {
				before = upgradeStateAttributes(s)
				if len(before) == 0 {
					return errors.New("the baseline apply left no resources in state")
				}
				return nil
			},
		},
		{
			ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
			Config:                   config,
			ConfigPlanChecks: resource.ConfigPlanChecks{
				PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
			},
			ConfigStateChecks: checks,
			Check: func(s *terraform.State) error {
				return upgradeCompareState(before, upgradeStateAttributes(s), changed)
			},
		},
		{
			ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
			Config:                   config,
			PlanOnly:                 true,
			ExpectNonEmptyPlan:       false,
		},
	}
}

// upgradeStateAttributes returns the flattened attributes of every resource
// in the root module of s, by resource address.
func upgradeStateAttributes(s *terraform.State) map[string]map[string]string {
	attrs := map[string]map[string]string{}
	for addr, rs := range s.RootModule().Resources {
		if rs.Primary == nil {
			continue
		}
		m := make(map[string]string, len(rs.Primary.Attributes))
		for k, v := range rs.Primary.Attributes {
			m[k] = v
		}
		attrs[addr] = m
	}
	return attrs
}

// upgradeCompareState reports every attribute of before that is missing or
// different in after, except for those in changed ("<address>.<attribute>")
// and the number of attributes, which grows with every attribute added.
func upgradeCompareState(before, after map[string]map[string]string, changed []string) error {
	var errs []error
	for addr, old := range before {
		cur, ok := after[addr]
		if !ok {
			errs = append(errs, fmt.Errorf("%s is no longer in state after the upgrade", addr))
			continue
		}
		for k, v := range old {
			if k == "%" || slices.Contains(changed, addr+"."+k) {
				continue
			}
			if got, ok := cur[k]; !ok {
				errs = append(errs, fmt.Errorf("%s: attribute %s = %q is gone after the upgrade", addr, k, v))
			} else if got != v {
				errs = append(errs, fmt.Errorf("%s: attribute %s changed from %q to %q in the upgrade", addr, k, v, got))
			}
		}
	}
	return errors.Join(errs...)
}

func checkPathGone(p string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			return fmt.Errorf("%s still exists after destroy (err=%v)", p, err)
		}
		return nil
	}
}

func TestAccUpgradeLocal_file(t *testing.T) {
	target := filepath.Join(t.TempDir(), "app.conf")
	resource.Test(t, resource.TestCase{
		Steps: localUpgradeSteps(t, "sysutils_file", fmt.Sprintf(`
resource "sysutils_file" "test" {
  path    = %q
  content = "listen = 8080\n"
  mode    = "0640"
}
`, target),
			statecheck.ExpectKnownValue("sysutils_file.test", tfjsonpath.New("content_sha256"),
				knownvalue.StringExact(sha256Hex([]byte("listen = 8080\n")))),
		),
		CheckDestroy: checkPathGone(target),
	})
}

func TestAccUpgradeLocal_exec(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "runs")
	config := fmt.Sprintf(`
resource "sysutils_exec" "test" {
  command  = ["/bin/sh", "-c", "echo run >> %s; echo out; echo err >&2"]
  triggers = { rev = "1" }
}
`, marker)
	resource.Test(t, resource.TestCase{
		Steps: localUpgradeSteps(t, "sysutils_exec", config,
			statecheck.ExpectKnownValue("sysutils_exec.test", tfjsonpath.New("truncated"), knownvalue.Bool(false)),
		),
		CheckDestroy: func(*terraform.State) error {
			// The upgrade must not run the command again.
			b, err := os.ReadFile(marker)
			if err != nil {
				return err
			}
			if string(b) != "run\n" {
				return fmt.Errorf("command ran %q, want %q", b, "run\n")
			}
			return nil
		},
	})
}

func TestAccUpgradeLocal_user(t *testing.T) {
	requireRoot(t)
	name := uniqueUsername("tfupu")
	resource.Test(t, resource.TestCase{
		Steps: localUpgradeSteps(t, "sysutils_user", fmt.Sprintf(`
resource "sysutils_user" "test" {
  name        = %q
  shell       = "/usr/sbin/nologin"
  comment     = "sysutils upgrade test"
  system      = true
  create_home = false
}
`, name)),
		CheckDestroy: func(*terraform.State) error {
			if _, err := user.Lookup(name); err == nil {
				return fmt.Errorf("user %s still exists after destroy", name)
			}
			return nil
		},
	})
}

func TestAccUpgradeLocal_directory(t *testing.T) {
	target := filepath.Join(t.TempDir(), "a", "data")
	resource.Test(t, resource.TestCase{
		Steps: localUpgradeSteps(t, "sysutils_directory", fmt.Sprintf(`
resource "sysutils_directory" "test" {
  path           = %q
  mode           = "0750"
  owner          = "root"
  group          = "0"
  create_parents = true
  force_destroy  = true
}
`, target),
			statecheck.ExpectKnownValue("sysutils_directory.test", tfjsonpath.New("recursive_mode"), knownvalue.Bool(false)),
			statecheck.ExpectKnownValue("sysutils_directory.test", tfjsonpath.New("nonconforming_entries"), knownvalue.Int64Exact(0)),
		),
		CheckDestroy: checkPathGone(target),
	})
}

func TestAccUpgradeLocal_symlink(t *testing.T) {
	link := filepath.Join(t.TempDir(), "current")
	resource.Test(t, resource.TestCase{
		Steps: localUpgradeSteps(t, "sysutils_symlink", fmt.Sprintf(`
resource "sysutils_symlink" "test" {
  path   = %q
  target = "releases/v1"
  owner  = "root"
}
`, link)),
		CheckDestroy: checkPathGone(link),
	})
}

func TestAccUpgradeLocal_group(t *testing.T) {
	requireRoot(t)
	name := uniqueUsername("tfupg")
	gid := freeGID(t, 62000)
	resource.Test(t, resource.TestCase{
		Steps: localUpgradeSteps(t, "sysutils_group", fmt.Sprintf(`
resource "sysutils_group" "test" {
  name    = %q
  gid     = %d
  members = ["root"]
}
`, name, gid)),
		CheckDestroy: checkGroupDestroyed(name),
	})
}

func TestAccUpgradeLocal_fileLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sshd_config")
	if err := os.WriteFile(p, []byte("Port 22\n#PermitRootLogin yes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resource.Test(t, resource.TestCase{
		// The id of a line no longer contains the line, which may be a
		// secret; refresh rewrites ids of the old form.
		Steps: localUpgradeStepsChanging(t, "sysutils_file_line", fmt.Sprintf(`
resource "sysutils_file_line" "line" {
  path   = %[1]q
  line   = "PermitRootLogin no"
  regexp = "^#?PermitRootLogin "
}

resource "sysutils_file_line" "block" {
  path   = %[1]q
  block  = "Match User backup\n  ForceCommand internal-sftp\n"
  marker = "# {mark} backup"
}
`, p), []string{"sysutils_file_line.line.id"}),
		CheckDestroy: checkFileLineContent(p, "Port 22\n"),
	})
}

func TestAccUpgradeLocal_templateFile(t *testing.T) {
	target := filepath.Join(t.TempDir(), "app.conf")
	resource.Test(t, resource.TestCase{
		Steps: localUpgradeSteps(t, "sysutils_template_file", fmt.Sprintf(`
resource "sysutils_template_file" "go" {
  path     = %q
  template = "port = {{ .port }}\n{{ range .hosts }}host = {{ . }}\n{{ end }}"
  vars     = { port = 8080, hosts = ["a", "b"] }
  mode     = "0600"
}

resource "sysutils_template_file" "terraform" {
  path     = "%[1]s.tf"
  template = "name = $${name}\n"
  syntax   = "terraform"
  vars     = { name = "app" }
}
`, target),
			statecheck.ExpectKnownValue("sysutils_template_file.go", tfjsonpath.New("rendered"),
				knownvalue.StringExact("port = 8080\nhost = a\nhost = b\n")),
		),
		CheckDestroy: resource.ComposeTestCheckFunc(checkPathGone(target), checkPathGone(target+".tf")),
	})
}

func TestAccUpgradeLocal_systemdUnit(t *testing.T) {
	requireSystemd(t)
	name := "tfacc-sysutils-upgrade-" + randomID() + ".service"
	resource.Test(t, resource.TestCase{
		Steps: localUpgradeSteps(t, "sysutils_systemd_unit", fmt.Sprintf(`
resource "sysutils_systemd_unit" "test" {
  name    = %q
  content = "[Unit]\nDescription=sysutils upgrade test\n\n[Service]\nExecStart=/bin/sleep infinity\n\n[Install]\nWantedBy=multi-user.target\n"
  enabled = true
  state   = "running"
  timeout = "30s"
}
`, name)),
		CheckDestroy: checkRealUnitRemoved(name),
	})
}

func TestAccUpgradeLocal_service(t *testing.T) {
	requireSystemd(t)
	name := installTestUnit(t)
	resource.Test(t, resource.TestCase{
		Steps: localUpgradeSteps(t, "sysutils_service", fmt.Sprintf(`
resource "sysutils_service" "test" {
  name              = %q
  enabled           = true
  state             = "running"
  restart_on_change = { config = "v1" }
  timeout           = "30s"
}
`, name)),
	})
}

func TestAccUpgradeLocal_mount(t *testing.T) {
	env := newMountAccEnv(t)
	// persist = false: the baseline build can't be pointed at a temporary
	// fstab, and the real /etc/fstab must not be changed.
	resource.Test(t, resource.TestCase{
		Steps: localUpgradeSteps(t, "sysutils_mount", fmt.Sprintf(`
resource "sysutils_mount" "test" {
  path    = %q
  device  = "tfacc-upgrade"
  fstype  = "tmpfs"
  options = ["size=1m", "mode=0750"]
  persist = false
}
`, env.mountPoint)),
		CheckDestroy: checkRealMount(t, env.mountPoint, ""),
	})
}

func TestAccUpgradeLocal_sysctl(t *testing.T) {
	orig := requireWritableSysctl(t, accSysctlName)
	n, err := strconv.Atoi(orig)
	if err != nil {
		t.Fatalf("%s = %q is not a number", accSysctlName, orig)
	}
	value := strconv.Itoa(n + 7)
	file := filepath.Join(t.TempDir(), "sysctl.d", "99-terraform.conf")
	resource.Test(t, resource.TestCase{
		Steps: localUpgradeSteps(t, "sysutils_sysctl", fmt.Sprintf(`
resource "sysutils_sysctl" "test" {
  name  = %q
  value = %q
  file  = %q
}
`, accSysctlName, value, file)),
		CheckDestroy: checkFileText(file, ""),
	})
}

func TestAccUpgradeLocal_kernelModule(t *testing.T) {
	requireLoadableModule(t, accModuleName)
	resource.Test(t, resource.TestCase{
		Steps: localUpgradeSteps(t, "sysutils_kernel_module", fmt.Sprintf(`
resource "sysutils_kernel_module" "test" {
  name       = %q
  parameters = { numdummies = "0" }
  persist    = false
}
`, accModuleName)),
	})
}

func TestAccUpgradeLocal_cronJob(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("acceptance test; set TF_ACC=1 to run")
	}
	requireRoot(t)
	name := "sysutils-acc-upgrade-" + randomID()
	p := filepath.Join(defaultCronDir, name)
	t.Cleanup(func() { _ = os.Remove(p) })
	resource.Test(t, resource.TestCase{
		Steps: localUpgradeSteps(t, "sysutils_cron_job", fmt.Sprintf(`
resource "sysutils_cron_job" "test" {
  name        = %q
  schedule    = "0 0 1 1 *"
  command     = "true"
  user        = "root"
  environment = { MAILTO = "" }
  comment     = "Created by the sysutils upgrade tests; safe to delete."
}
`, name)),
		CheckDestroy: checkCronJobGone(p),
	})
}

func TestAccUpgradeLocal_sudoers(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("acceptance test; set TF_ACC=1 to run")
	}
	requireRoot(t)
	requireVisudo(t)
	name := "sysutils-acc-upgrade-" + randomID()
	p := filepath.Join(defaultSudoersDir, name)
	t.Cleanup(func() { _ = os.Remove(p) })
	resource.Test(t, resource.TestCase{
		Steps: localUpgradeSteps(t, "sysutils_sudoers", fmt.Sprintf(`
resource "sysutils_sudoers" "test" {
  name = %q
  rules = [{
    users    = ["root"]
    runas    = "root"
    nopasswd = true
    commands = ["/usr/bin/true"]
  }]
}
`, name)),
		CheckDestroy: checkSudoersGone(p),
	})
}

func TestAccUpgradeLocal_package(t *testing.T) {
	name := accPackageName()
	mgr := requirePackageManager(t, name)
	resource.Test(t, resource.TestCase{
		Steps: localUpgradeSteps(t, "sysutils_package", fmt.Sprintf(`
resource "sysutils_package" "test" {
  name         = %q
  update_cache = true
}
`, name),
			statecheck.ExpectKnownValue("sysutils_package.test", tfjsonpath.New("installed_version"), knownvalue.NotNull()),
		),
		CheckDestroy: checkRealPackage(mgr, name, ""),
	})
}

func TestAccUpgradeLocal_iniValue(t *testing.T) {
	p := filepath.Join(t.TempDir(), "app.ini")
	if err := os.WriteFile(p, []byte("; settings\n[server]\nport = 80\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resource.Test(t, resource.TestCase{
		Steps: localUpgradeSteps(t, "sysutils_ini_value", fmt.Sprintf(`
resource "sysutils_ini_value" "port" {
  path    = %[1]q
  section = "server"
  key     = "port"
  value   = "8080"
}

resource "sysutils_ini_value" "created" {
  path      = %[1]q
  section   = "log"
  key       = "level"
  value     = "info"
  separator = "="
}
`, p)),
		// Destroy removes the keys, not the section headers.
		CheckDestroy: checkFileText(p, "; settings\n[server]\n\n[log]\n"),
	})
}

func TestAccUpgradeLocal_packageRepository(t *testing.T) {
	// Below a root_dir, so that the host's repositories are left alone.
	root := t.TempDir()
	for _, d := range []string{"etc/apt", "etc/apk"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	resource.Test(t, resource.TestCase{
		Steps: localUpgradeSteps(t, "sysutils_package_repository", fmt.Sprintf(`
provider "sysutils" {
  root_dir = %q
}

resource "sysutils_package_repository" "apt" {
  name          = "upgrade"
  manager       = "apt"
  description   = "Upgrade test"
  uris          = ["https://example.com/debian"]
  suites        = ["stable"]
  components    = ["main"]
  architectures = ["amd64"]
  signing_key   = %q
}

resource "sysutils_package_repository" "apk" {
  name    = "upgrade"
  manager = "apk"
  uris    = ["https://example.com/alpine"]
  tag     = "upgrade"
  enabled = false
}
`, root, testArmoredKey)),
		CheckDestroy: checkRepoGone(
			filepath.Join(root, "etc/apt/sources.list.d/upgrade.sources"),
			filepath.Join(root, "etc/apt/keyrings/upgrade.asc"),
		),
	})
}

func TestAccUpgradeLocal_archiveExtract(t *testing.T) {
	work := t.TempDir()
	archive := filepath.Join(work, "app.tar.gz")
	dest := filepath.Join(work, "app")
	if err := os.WriteFile(archive, gzipBytes(t, makeTar(t,
		arDir("app-1.0/"),
		testEntry{name: "app-1.0/bin/app", typ: tar.TypeReg, body: "#!/bin/sh\n", mode: 0o755},
		arFile("app-1.0/README", "readme\n"),
		arSymlink("app-1.0/current", "bin/app"),
	)), 0o644); err != nil {
		t.Fatal(err)
	}
	resource.Test(t, resource.TestCase{
		Steps: localUpgradeSteps(t, "sysutils_archive_extract", fmt.Sprintf(`
resource "sysutils_archive_extract" "test" {
  source           = %q
  destination      = %q
  strip_components = 1
  file_mode        = "0640"
}
`, archive, dest),
			statecheck.ExpectKnownValue("sysutils_archive_extract.test", tfjsonpath.New("drifted_entries"), knownvalue.Int64Exact(0)),
		),
		CheckDestroy: checkPathGone(dest),
	})
}

func TestAccUpgradeLocal_sshAuthorizedKey(t *testing.T) {
	requireRoot(t)
	name := uniqueUsername("tfupssh")
	home := filepath.Join(t.TempDir(), name)
	resource.Test(t, resource.TestCase{
		Steps: localUpgradeSteps(t, "sysutils_ssh_authorized_key", fmt.Sprintf(`
resource "sysutils_user" "u" {
  name        = %q
  home        = %q
  create_home = true
}

resource "sysutils_ssh_authorized_key" "test" {
  user    = sysutils_user.u.name
  key     = %q
  options = ["no-pty"]
}
`, name, home, testSSHKeyText(testSSHKey(t, 9))+" upgrade")),
	})
}

func TestAccUpgradeLocal_hostsEntry(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hosts")
	const orig = "127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost\n"
	if err := os.WriteFile(p, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	resource.Test(t, resource.TestCase{
		Steps: localUpgradeSteps(t, "sysutils_hosts_entry", fmt.Sprintf(`
resource "sysutils_hosts_entry" "v4" {
  path      = %[1]q
  ip        = "10.0.0.5"
  hostnames = ["db.internal", "db"]
  comment   = "upgrade"
}

resource "sysutils_hosts_entry" "v6" {
  path            = %[1]q
  ip              = "fd00::5"
  hostnames       = ["db.internal", "db"]
  allow_duplicate = true
}
`, p)),
		// Destroy removes only the managed lines.
		CheckDestroy: checkFileText(p, orig),
	})
}

func TestAccUpgradeLocal_timezone(t *testing.T) {
	// Below a root_dir, so that the host's time zone is left alone.
	root := testTimezoneRoot(t, "etc/debian_version")
	lt := filepath.Join(root, "etc", "localtime")
	mustSymlink(t, "/usr/share/zoneinfo/Etc/UTC", lt)
	resource.Test(t, resource.TestCase{
		Steps: localUpgradeSteps(t, "sysutils_timezone", fmt.Sprintf(`
provider "sysutils" {
  root_dir = %q
}

resource "sysutils_timezone" "test" {
  timezone           = "Europe/Berlin"
  restore_on_destroy = true
}
`, root)),
		// The private state recorded by the baseline still restores.
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkSymlinkTarget(lt, "/usr/share/zoneinfo/Etc/UTC"),
			checkPathGone(filepath.Join(root, "etc", "timezone")),
		),
	})
}

func TestAccUpgradeLocal_hostname(t *testing.T) {
	// Below a root_dir, so that only files are written and the host's
	// hostname is left alone.
	root := testRootDir(t)
	mustWrite(t, filepath.Join(root, "etc", "hostname"), "before\n")
	mustWrite(t, filepath.Join(root, "etc", "hosts"), "127.0.0.1 localhost\n127.0.1.1 before\n")
	resource.Test(t, resource.TestCase{
		Steps: localUpgradeSteps(t, "sysutils_hostname", fmt.Sprintf(`
provider "sysutils" {
  root_dir = %q
}

resource "sysutils_hostname" "test" {
  hostname           = "web1.example.com"
  pretty_hostname    = "Web server 1"
  manage_hosts_entry = true
  restore_on_destroy = true
}
`, root)),
		// The private state recorded by the baseline still restores.
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkFileText(filepath.Join(root, "etc", "hostname"), "before\n"),
			checkFileText(filepath.Join(root, "etc", "hosts"), "127.0.0.1 localhost\n127.0.1.1 before\n"),
			checkPathGone(filepath.Join(root, "etc", "machine-info")),
		),
	})
}

func TestAccUpgradeLocal_locale(t *testing.T) {
	// Below a root_dir, so that the host's locale is left alone.
	root := testRootDir(t)
	p := filepath.Join(root, "etc", "default", "locale")
	mustWrite(t, filepath.Join(root, "etc", "debian_version"), "12.5\n")
	mustWrite(t, p, "LANG=C.UTF-8\n")
	resource.Test(t, resource.TestCase{
		Steps: localUpgradeSteps(t, "sysutils_locale", fmt.Sprintf(`
provider "sysutils" {
  root_dir = %q
}

resource "sysutils_locale" "test" {
  lang               = "en_US.UTF-8"
  lc                 = { LC_TIME = "en_GB.UTF-8" }
  restore_on_destroy = true
}
`, root)),
		CheckDestroy: checkFileText(p, "LANG=C.UTF-8\n"),
	})
}

func TestAccUpgradeLocal_alternatives(t *testing.T) {
	// A link group of its own, with the link and alternatives in a
	// temporary directory, so that the host's link groups are left alone.
	e := newAltAccEnv(t)
	resource.Test(t, resource.TestCase{
		Steps: localUpgradeSteps(t, "sysutils_alternatives", fmt.Sprintf(`
resource "sysutils_alternatives" "test" {
  name              = %q
  path              = %q
  link              = %q
  priority          = 10
  remove_on_destroy = true
}
`, e.name, e.a, e.link)),
		CheckDestroy: e.check("gone", ""),
	})
}
