package provider

// Acceptance tests of sysutils_hostname against the real kernel hostname,
// /etc/hostname and /etc/hosts. They must never change the test host's
// hostname, so each test re-runs itself in a child process with a UTS
// namespace and a mount namespace of its own. In the child, /etc is covered
// by an overlay whose writable layer is a tmpfs, so that the files are
// written as usual, atomically, but the host's stay untouched. Being in
// another UTS namespace than PID 1 also stops the provider from using
// hostnamectl, which would change the host's hostname through
// systemd-hostnamed. The parent checks afterwards that the host's hostname
// and files are unchanged.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const (
	// hostnameSandboxEnv is set in the child to the UTS namespace of the
	// parent, which the child must not be in.
	hostnameSandboxEnv = "SYSUTILS_TEST_HOSTNAME_SANDBOX"
	// hostnameSandboxSkipEnv names the file the child writes its reason to
	// if it skips.
	hostnameSandboxSkipEnv = "SYSUTILS_TEST_HOSTNAME_SANDBOX_SKIP"
)

// inHostnameSandbox runs the calling test in a sandbox and reports whether
// the caller is inside it: in the parent it starts the child, waits for it,
// reports its outcome and returns false; in the child it sets up the mounts
// and returns true.
func inHostnameSandbox(t *testing.T) bool {
	t.Helper()
	if os.Getenv("TF_ACC") == "" {
		t.Skip("acceptance test; set TF_ACC=1 to run")
	}
	if parentNS := os.Getenv(hostnameSandboxEnv); parentNS != "" {
		self, err := os.Readlink("/proc/self/ns/uts")
		if err != nil || self == parentNS {
			t.Fatalf("%s is set, but the test is not in a UTS namespace of its own (%s, %v)", hostnameSandboxEnv, self, err)
		}
		setupHostnameSandboxMounts(t)
		return true
	}
	requireRoot(t)

	parentNS, err := os.Readlink("/proc/self/ns/uts")
	if err != nil {
		t.Skipf("cannot create a UTS namespace here: reading /proc/self/ns/uts: %v", err)
	}
	files := []string{hostnameFilePath, defaultHostsPath, machineInfoPath}
	before := snapshotHostHostname(t, files)

	skipFile := filepath.Join(t.TempDir(), "skip")
	cmd := exec.Command(os.Args[0], "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.count=1")
	cmd.Env = append(os.Environ(), hostnameSandboxEnv+"="+parentNS, hostnameSandboxSkipEnv+"="+skipFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWUTS | syscall.CLONE_NEWNS}
	out, runErr := cmd.CombinedOutput()

	if after := snapshotHostHostname(t, files); after != before {
		t.Errorf("the test changed the host:\nbefore: %s\nafter:  %s", before, after)
	}
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		t.Skipf("cannot create a UTS namespace here: %v", runErr)
	}
	if runErr != nil {
		// Indented, so that go test and test2json don't take the child's
		// result lines for the parent's.
		t.Fatalf("the test failed in its UTS namespace: %v\n%s", runErr, prefixLines(string(out), "| "))
	}
	if reason, err := os.ReadFile(skipFile); err == nil {
		t.Skipf("%s", reason)
	}
	return false
}

// snapshotHostHostname describes the host's hostname and files.
func snapshotHostHostname(t *testing.T, files []string) string {
	t.Helper()
	name, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "hostname=%q", name)
	for _, f := range files {
		data, err := os.ReadFile(f)
		fmt.Fprintf(&b, " %s=%q(%v)", f, data, err)
	}
	return b.String()
}

func prefixLines(s, prefix string) string {
	return prefix + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n"+prefix)
}

// setupHostnameSandboxMounts covers /etc with an overlay on a tmpfs, in the
// child's own mount namespace, and skips if that is not possible.
func setupHostnameSandboxMounts(t *testing.T) {
	t.Helper()
	skip := func(format string, args ...any) {
		reason := fmt.Sprintf(format, args...)
		if p := os.Getenv(hostnameSandboxSkipEnv); p != "" {
			_ = os.WriteFile(p, []byte(reason), 0o600)
		}
		t.Skip(reason)
	}
	// Keep the mounts below from propagating to the host.
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		skip("cannot make the mounts of the test's mount namespace private: %v", err)
	}
	d := t.TempDir()
	if err := syscall.Mount("tmpfs", d, "tmpfs", 0, "mode=0700"); err != nil {
		skip("cannot mount a tmpfs for the overlay on /etc: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Unmount(d, syscall.MNT_DETACH) })
	upper, work := filepath.Join(d, "upper"), filepath.Join(d, "work")
	for _, dir := range []string{upper, work} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	opts := fmt.Sprintf("lowerdir=/etc,upperdir=%s,workdir=%s", upper, work)
	if err := syscall.Mount("overlay", "/etc", "overlay", 0, opts); err != nil {
		skip("cannot mount an overlay on /etc: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Unmount("/etc", syscall.MNT_DETACH) })
}

func checkKernelHostname(want string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		got, err := os.Hostname()
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("kernel hostname is %q, want %q", got, want)
		}
		return nil
	}
}

func mustSethostname(t *testing.T, name string) {
	t.Helper()
	if err := syscall.Sethostname([]byte(name)); err != nil {
		t.Fatal(err)
	}
}

func TestAccHostname_utsNamespace(t *testing.T) {
	if !inHostnameSandbox(t) {
		return
	}
	// A known starting point, inside the sandbox.
	mustSethostname(t, "sysutils-before")
	mustWrite(t, hostnameFilePath, "sysutils-before\n")
	hostsBefore := "127.0.0.1\tlocalhost\n127.0.1.1\tsysutils-before\n::1\tlocalhost ip6-localhost\n"
	mustWrite(t, defaultHostsPath, hostsBefore)
	_ = os.Remove(machineInfoPath)
	hostsWith := func(line string) string {
		return strings.Replace(hostsBefore, "127.0.1.1\tsysutils-before", line, 1)
	}
	config := func(name string) string {
		return hostnameResourceConfig("", fmt.Sprintf(`  hostname           = %q
  pretty_hostname    = "Acceptance test"
  manage_hosts_entry = true
  restore_on_destroy = true`, name))
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkKernelHostname("sysutils-before"),
			checkFileContent(hostnameFilePath, "sysutils-before\n"),
			checkFileContent(defaultHostsPath, hostsBefore),
			checkNotExist(machineInfoPath),
		),
		Steps: []resource.TestStep{
			{
				Config: config("sysutils-acc.example.test"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkKernelHostname("sysutils-acc.example.test"),
					checkFileContent(hostnameFilePath, "sysutils-acc.example.test\n"),
					checkFileContent(defaultHostsPath, hostsWith("127.0.1.1\tsysutils-acc.example.test sysutils-acc")),
					checkFileContent(machineInfoPath, "PRETTY_HOSTNAME=\"Acceptance test\"\n"),
					checkNoTempFiles("/etc"),
					resource.TestCheckResourceAttr(testHostnameResource, "hostname", "sysutils-acc.example.test"),
					resource.TestCheckResourceAttr(testHostnameResource, "transient_hostname", "sysutils-acc.example.test"),
				),
			},
			{
				Config:           config("sysutils-acc.example.test"),
				ConfigPlanChecks: expectEmptyPlan,
			},
			{
				// Only the kernel hostname changed.
				PreConfig:        func() { mustSethostname(t, "sysutils-drifted") },
				Config:           config("sysutils-acc.example.test"),
				ConfigPlanChecks: expectHostnameUpdate(),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkKernelHostname("sysutils-acc.example.test"),
					resource.TestCheckResourceAttr(testHostnameResource, "transient_hostname", "sysutils-acc.example.test"),
				),
			},
			{
				// Only /etc/hostname changed.
				PreConfig:        func() { mustWrite(t, hostnameFilePath, "sysutils-drifted\n") },
				Config:           config("sysutils-acc.example.test"),
				ConfigPlanChecks: expectHostnameUpdate(),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(hostnameFilePath, "sysutils-acc.example.test\n"),
					checkKernelHostname("sysutils-acc.example.test"),
				),
			},
			{
				Config: config("sysutils-acc2"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkKernelHostname("sysutils-acc2"),
					checkFileContent(hostnameFilePath, "sysutils-acc2\n"),
					checkFileContent(defaultHostsPath, hostsWith("127.0.1.1\tsysutils-acc2")),
				),
			},
			{
				ResourceName:            testHostnameResource,
				ImportState:             true,
				ImportStateId:           "system",
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"pretty_hostname", "manage_hosts_entry", "restore_on_destroy"},
			},
		},
	})
}

func TestAccHostname_utsNamespaceLeftOnDestroy(t *testing.T) {
	if !inHostnameSandbox(t) {
		return
	}
	mustSethostname(t, "sysutils-before")
	mustWrite(t, hostnameFilePath, "sysutils-before\n")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		// Without restore_on_destroy the hostname stays.
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkKernelHostname("sysutils-kept"),
			checkFileContent(hostnameFilePath, "sysutils-kept\n"),
		),
		Steps: []resource.TestStep{{
			Config: hostnameResourceConfig("", `  hostname = "sysutils-kept"`),
			Check: resource.ComposeAggregateTestCheckFunc(
				checkKernelHostname("sysutils-kept"),
				checkFileContent(hostnameFilePath, "sysutils-kept\n"),
			),
		}},
	})
}
