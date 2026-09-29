package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6/tf6server"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// concurrentResources is how many resources the tests below apply at once,
// twice Terraform's default parallelism of 10.
const concurrentResources = 20

// parallelTF runs the Terraform CLI against a provider served in this test
// process, like terraform-plugin-testing does. resource.Test cannot be used
// for these tests: it always runs Terraform with -parallelism=10, which
// overrides TF_CLI_ARGS_apply.
type parallelTF struct {
	t   *testing.T
	dir string
	cli string
	env []string
	// source is the provider's source address.
	source string
}

// newParallelTF serves the provider made by factory and returns a runner
// for the Terraform CLI in a new working directory. The test is skipped
// unless TF_ACC is set.
func newParallelTF(t *testing.T, factory func() (tfprotov6.ProviderServer, error)) *parallelTF {
	t.Helper()
	if os.Getenv(resource.EnvTfAcc) == "" {
		t.Skipf("Acceptance tests skipped unless env '%s' set", resource.EnvTfAcc)
	}
	cli := os.Getenv("TF_ACC_TERRAFORM_PATH")
	if cli == "" {
		for _, name := range []string{"terraform", "tofu"} {
			if p, err := exec.LookPath(name); err == nil {
				cli = p
				break
			}
		}
	}
	if cli == "" {
		t.Fatal("neither TF_ACC_TERRAFORM_PATH is set nor terraform or tofu found in PATH")
	}
	host, namespace := "registry.terraform.io", "hashicorp"
	if v := os.Getenv(resource.EnvTfAccProviderHost); v != "" {
		host = v
	}
	if v := os.Getenv(resource.EnvTfAccProviderNamespace); v != "" {
		namespace = v
	}
	source := host + "/" + namespace + "/sysutils"

	srv, err := factory()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	configCh := make(chan *plugin.ReattachConfig, 1)
	closeCh := make(chan struct{})
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- tf6server.Serve(source, func() tfprotov6.ProviderServer { return srv },
			tf6server.WithDebug(ctx, configCh, closeCh),
			tf6server.WithGoPluginLogger(hclog.NewNullLogger()),
			tf6server.WithoutLogStderrOverride(),
			tf6server.WithLoggingSink(t))
	}()
	var rc *plugin.ReattachConfig
	select {
	case rc = <-configCh:
	case err := <-serveErr:
		cancel()
		t.Fatalf("serving the provider: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		<-closeCh
	})
	reattach, err := json.Marshal(map[string]any{source: map[string]any{
		"Protocol":        rc.Protocol,
		"ProtocolVersion": rc.ProtocolVersion,
		"Pid":             rc.Pid,
		"Test":            true,
		"Addr":            map[string]string{"Network": rc.Addr.Network(), "String": rc.Addr.String()},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "TF_CLI_ARGS") && !strings.HasPrefix(kv, "TF_LOG") {
			env = append(env, kv)
		}
	}
	env = append(env, "TF_REATTACH_PROVIDERS="+string(reattach), "TF_IN_AUTOMATION=1", "TF_INPUT=0", "CHECKPOINT_DISABLE=1")
	return &parallelTF{t: t, dir: t.TempDir(), cli: cli, env: env, source: source}
}

// setConfig writes the configuration and initializes the working directory.
func (tf *parallelTF) setConfig(config string) {
	tf.t.Helper()
	full := fmt.Sprintf(`
terraform {
  required_providers {
    sysutils = {
      source = %q
    }
  }
}
%s`, tf.source, config)
	if err := os.WriteFile(filepath.Join(tf.dir, "main.tf"), []byte(full), 0o600); err != nil {
		tf.t.Fatal(err)
	}
	if out, err := tf.run("init"); err != nil {
		tf.t.Fatalf("init: %v\n%s", err, out)
	}
}

// run runs the CLI with args and returns its combined output.
func (tf *parallelTF) run(args ...string) (string, error) {
	cmd := exec.Command(tf.cli, append(args, "-no-color")...)
	cmd.Dir = tf.dir
	cmd.Env = tf.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// apply applies the configuration with concurrentResources resources at a
// time.
func (tf *parallelTF) apply() (string, error) {
	return tf.run("apply", "-auto-approve", fmt.Sprintf("-parallelism=%d", concurrentResources))
}

// mustApply applies the configuration and fails the test on an error.
func (tf *parallelTF) mustApply() {
	tf.t.Helper()
	if out, err := tf.apply(); err != nil {
		tf.t.Fatalf("apply: %v\n%s", err, out)
	}
}

// mustPlanEmpty runs a refresh plan and fails the test unless it is empty.
func (tf *parallelTF) mustPlanEmpty() {
	tf.t.Helper()
	out, err := tf.run("plan", "-detailed-exitcode", fmt.Sprintf("-parallelism=%d", concurrentResources))
	if err != nil {
		tf.t.Fatalf("refresh plan is not empty (%v):\n%s", err, out)
	}
}

// mustDestroy destroys everything with concurrentResources resources at a
// time.
func (tf *parallelTF) mustDestroy() {
	tf.t.Helper()
	if out, err := tf.run("destroy", "-auto-approve", fmt.Sprintf("-parallelism=%d", concurrentResources)); err != nil {
		tf.t.Fatalf("destroy: %v\n%s", err, out)
	}
}

// editRace widens the window between reading and writing the file p in
// every read-modify-write cycle, and records how many edits of p were in
// progress or waiting for the lock at once.
type editRace struct {
	p       string
	waiting atomic.Int32
}

func newEditRace(t *testing.T, p string) *editRace {
	t.Helper()
	r := &editRace{p: p}
	key, err := fileLockKey(p)
	if err != nil {
		t.Fatal(err)
	}
	testHookAfterRead = func(read string) {
		if read != p {
			return
		}
		lockRegistry.Lock()
		if l := lockRegistry.locks["file:"+key]; l != nil && int32(l.refs) > r.waiting.Load() {
			r.waiting.Store(int32(l.refs))
		}
		lockRegistry.Unlock()
		time.Sleep(25 * time.Millisecond)
	}
	t.Cleanup(func() {
		testHookAfterRead = nil
		disableEditLocks.Store(false)
	})
	return r
}

// checkQueued checks that the edits really ran in parallel: at some point
// more resources waited for the lock than the default parallelism of 10
// allows.
func (r *editRace) checkQueued(t *testing.T) {
	t.Helper()
	if n := r.waiting.Load(); n <= 10 {
		t.Errorf("at most %d edits of %s were in progress at once, want more than 10", n, r.p)
	}
}

// checkFileHasLines checks that the file p contains each of want as a line.
func checkFileHasLines(t *testing.T, p string, want []string) {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, l := range strings.Split(string(data), "\n") {
		have[l] = true
	}
	var missing []string
	for _, l := range want {
		if !have[l] {
			missing = append(missing, l)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%s lacks %d of %d lines %q; content:\n%s", p, len(missing), len(want), missing, data)
	}
}

// concurrentEditRE matches the error of an edit that lost the race.
var concurrentEditRE = regexp.MustCompile(`modified\s+by\s+another\s+process`)

// testConcurrentEdits applies config, which edits the file p with
// concurrentResources resources, all at once, first without and then with
// the lock. The file starts with orig; managed are the lines the resources
// add and remove on destroy, and kept are further lines apply adds and
// destroy leaves in place.
func testConcurrentEdits(t *testing.T, p, orig, config string, managed, kept []string) {
	want := slices.Concat(strings.Split(strings.TrimSuffix(orig, "\n"), "\n"), managed, kept)
	mustWrite(t, p, orig)
	race := newEditRace(t, p)
	tf := newParallelTF(t, testAccProtoV6ProviderFactories["sysutils"])
	tf.setConfig(config)

	// Without the lock, the read-modify-write cycles overlap and the edits
	// get in each other's way. replaceFileAtomic notices that the file
	// changed after it was read, so apply fails rather than losing lines.
	disableEditLocks.Store(true)
	out, err := tf.apply()
	if err == nil || !concurrentEditRE.MatchString(out) {
		t.Fatalf("apply without the lock: err = %v, want concurrent modification errors; output:\n%s", err, out)
	}
	disableEditLocks.Store(false)
	race.waiting.Store(0)
	mustWrite(t, p, orig)

	tf.mustApply()
	checkFileHasLines(t, p, want)
	race.checkQueued(t)

	// A refresh finds every line in place.
	tf.mustPlanEmpty()
	checkFileHasLines(t, p, want)

	// Destroy removes every managed line and keeps the others.
	tf.mustDestroy()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(data), "\n") {
		if slices.Contains(managed, l) {
			t.Errorf("after destroy, %s still has %q:\n%s", p, l, data)
		}
	}
	checkFileHasLines(t, p, strings.Split(strings.TrimSuffix(orig, "\n"), "\n"))
}

// TestAccFileLine_concurrentEditsOfOneFile applies 20 sysutils_file_line
// resources that edit one file, all at once.
func TestAccFileLine_concurrentEditsOfOneFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hosts")
	config := fmt.Sprintf(`
resource "sysutils_file_line" "test" {
  count = %d
  path  = %q
  line  = "10.0.0.${count.index} host${count.index}"
}
`, concurrentResources, p)
	var managed []string
	for i := range concurrentResources {
		managed = append(managed, fmt.Sprintf("10.0.0.%d host%d", i, i))
	}
	testConcurrentEdits(t, p, "127.0.0.1 localhost\n", config, managed, nil)
}

// TestAccIniValue_concurrentEditsOfOneFile is the same for 20
// sysutils_ini_value resources setting different keys of one file.
func TestAccIniValue_concurrentEditsOfOneFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "app.ini")
	config := fmt.Sprintf(`
resource "sysutils_ini_value" "test" {
  count   = %d
  path    = %q
  section = "section${count.index %% 3}"
  key     = "key${count.index}"
  value   = "value${count.index}"
}
`, concurrentResources, p)
	var managed []string
	for i := range concurrentResources {
		managed = append(managed, fmt.Sprintf("key%d = value%d", i, i))
	}
	// Destroy leaves the sections that apply created, empty.
	testConcurrentEdits(t, p, "[main]\nname = app\n", config, managed, []string{"[section0]", "[section1]", "[section2]"})
}

// concurrencyProbe is a package manager that records how many of its
// commands, and of the writes of repository files, ran at the same time.
type concurrencyProbe struct {
	*fakePackageManager
	active, max atomic.Int32
}

// enter marks the start of an operation that takes a while, and returns the
// function that marks its end.
func (c *concurrencyProbe) enter() func() {
	n := c.active.Add(1)
	for {
		m := c.max.Load()
		if n <= m || c.max.CompareAndSwap(m, n) {
			break
		}
	}
	time.Sleep(10 * time.Millisecond)
	return func() { c.active.Add(-1) }
}

func (c *concurrencyProbe) Query(ctx context.Context, name string) (packageInfo, error) {
	defer c.enter()()
	return c.fakePackageManager.Query(ctx, name)
}

func (c *concurrencyProbe) UpToDate(ctx context.Context, name string) (bool, error) {
	defer c.enter()()
	return c.fakePackageManager.UpToDate(ctx, name)
}

func (c *concurrencyProbe) Install(ctx context.Context, name, version string) error {
	defer c.enter()()
	return c.fakePackageManager.Install(ctx, name, version)
}

func (c *concurrencyProbe) Upgrade(ctx context.Context, name string) error {
	defer c.enter()()
	return c.fakePackageManager.Upgrade(ctx, name)
}

func (c *concurrencyProbe) Remove(ctx context.Context, name string) error {
	defer c.enter()()
	return c.fakePackageManager.Remove(ctx, name)
}

func (c *concurrencyProbe) UpdateCache(ctx context.Context) error {
	defer c.enter()()
	return c.fakePackageManager.UpdateCache(ctx)
}

// TestAccPackage_concurrentOperationsAreSerialised applies many
// sysutils_package and sysutils_package_repository resources at once and
// checks that no two package manager commands, and no package manager
// command and repository change, ever overlapped.
func TestAccPackage_concurrentOperationsAreSerialised(t *testing.T) {
	env := newRepoTestEnv(t, packageManagerApt, "etc/apt")
	probe := &concurrencyProbe{fakePackageManager: env.pkg}

	const packages, repos = 14, 6
	for i := range packages {
		env.pkg.available[fmt.Sprintf("pkg%d", i)] = []string{"1.0"}
	}
	sourcesDir := env.path("etc/apt/sources.list.d") + string(filepath.Separator)
	// Repository files are only read by refresh, which needs no lock, but
	// writing one must not overlap with package manager commands.
	var repoWrites atomic.Int32
	testHookBeforeRename = func(p string) {
		if strings.HasPrefix(p, sourcesDir) {
			repoWrites.Add(1)
			probe.enter()()
		}
	}
	t.Cleanup(func() { testHookBeforeRename = nil })

	tf := newParallelTF(t, providerserver.NewProtocol6WithError(&sysutilsProvider{
		version: "test",
		repo:    env.cfg,
		pkg: &packageConfig{manager: func(string) (packageManager, error) {
			return probe, nil
		}},
	}))
	tf.setConfig(fmt.Sprintf(`
resource "sysutils_package" "test" {
  count        = %d
  name         = "pkg${count.index}"
  update_cache = true
}

resource "sysutils_package_repository" "test" {
  count         = %d
  name          = "repo${count.index}"
  uris          = ["https://example.com/repo${count.index}"]
  suites        = ["stable"]
  components    = ["main"]
  refresh_cache = true
}
`, packages, repos))

	checkSerialised := func(phase string) {
		t.Helper()
		if n := probe.max.Load(); n != 1 {
			t.Errorf("%s: %d package operations ran at once, want 1; calls: %v", phase, n, env.pkg.changes())
		}
	}

	tf.mustApply()
	checkSerialised("apply")
	for i := range packages {
		if _, ok := env.pkg.version(fmt.Sprintf("pkg%d", i)); !ok {
			t.Errorf("pkg%d is not installed", i)
		}
	}
	for i := range repos {
		if _, err := os.Stat(env.path(fmt.Sprintf("etc/apt/sources.list.d/repo%d.sources", i))); err != nil {
			t.Error(err)
		}
	}
	if n := repoWrites.Load(); n < repos {
		t.Errorf("the probe saw %d repository file writes, want at least %d", n, repos)
	}

	tf.mustPlanEmpty()
	checkSerialised("refresh")

	tf.mustDestroy()
	checkSerialised("destroy")
	for i := range packages {
		if _, ok := env.pkg.version(fmt.Sprintf("pkg%d", i)); ok {
			t.Errorf("pkg%d is still installed", i)
		}
	}
}
