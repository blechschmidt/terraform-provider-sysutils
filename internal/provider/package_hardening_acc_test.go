package provider

// Acceptance regression tests for the security review of the package
// resource.

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// lockedRunner makes a scriptedRunner safe for the provider, which may run
// commands from several goroutines.
type lockedRunner struct {
	mu sync.Mutex
	s  scriptedRunner
}

func (l *lockedRunner) run(ctx context.Context, spec execSpec) (*execResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.s.run(ctx, spec)
}

// checkNoAptGet fails if apt-get was run at all.
func (l *lockedRunner) checkNoAptGet() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range l.s.commands() {
		if strings.HasPrefix(c, "apt-get") {
			return fmt.Errorf("ran %s", c)
		}
	}
	return nil
}

// scriptedAptFactories returns provider factories whose sysutils_package
// uses the real apt backend, with commands answered by l.
func scriptedAptFactories(l *lockedRunner) map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		"sysutils": providerserver.NewProtocol6WithError(&sysutilsProvider{
			version: "test",
			pkg: &packageConfig{
				run: l.run,
				lookPath: func(name string) (string, error) {
					if name == "apt-get" || name == "apt-cache" || name == "dpkg-query" {
						return "/usr/bin/" + name, nil
					}
					return "", fmt.Errorf("%s not found", name)
				},
			},
		}),
	}
}

// Names that apt-get takes for a removal ("zstd-") or a regular expression
// ("zstd+", "lib.+") never reach apt-get. On the old code, "zstd-" as the
// name of a package to install removed zstd and everything depending on it.
func TestAccPackage_aptNamesThatAreNotPackages(t *testing.T) {
	l := &lockedRunner{s: scriptedRunner{rules: []scriptedRule{
		{prefix: "dpkg-query", exit: 1, stderr: "dpkg-query: no packages found matching x\n"},
		{prefix: "apt-cache policy -- zstd+", stdout: "librust-zstd-sys-2.0.9-dev:\n  Installed: (none)\n  Candidate: 2.0.9-1\n"},
		{prefix: "apt-cache policy -- lib.+", stdout: "libzstd-jni1:\n  Installed: (none)\n  Candidate: 1.5.2-5+ds-3build1\n"},
		{prefix: "apt-cache policy"},
		{prefix: "apt-get"},
	}}}
	config := func(name, manager string) string {
		return fmt.Sprintf(`
resource "sysutils_package" "test" {
  name    = %q
  manager = %q
}`, name, manager)
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: scriptedAptFactories(l),
		CheckDestroy:             func(*terraform.State) error { return l.checkNoAptGet() },
		Steps: []resource.TestStep{
			{
				// Refused at plan time when the manager is known...
				Config:      config("zstd-", "apt"),
				ExpectError: regexp.MustCompile(`must\s+not\s+end\s+with\s+"-"`),
			},
			{
				// ... and at apply time when it is detected.
				Config:      config("zstd-", "auto"),
				ExpectError: regexp.MustCompile(`must\s+not\s+end\s+with\s+"-"`),
			},
			{
				Config:      config("zstd+", "auto"),
				ExpectError: regexp.MustCompile(`(?i)package\s+zstd\+\s+is\s+not\s+in\s+the\s+package\s+index(?s:.*)librust-zstd-sys-2\.0\.9-dev\)`),
			},
			{
				Config:      config("lib.+", "apt"),
				ExpectError: regexp.MustCompile(`(?i)package\s+lib\.\+\s+is\s+not\s+in\s+the\s+package\s+index(?s:.*)libzstd-jni1\)`),
			},
			{
				Config:      config("sysutils-test-nosuch", "apt"),
				ExpectError: regexp.MustCompile(`not\s+in\s+the\s+package\s+index`),
			},
		},
	})
	if err := l.checkNoAptGet(); err != nil {
		t.Error(err)
	}
}

// The same against the host's real apt: a name that apt would expand as a
// regular expression is refused, and nothing is installed.
func TestAccPackage_aptRealRegexName(t *testing.T) {
	name := accPackageName()
	mgr := requirePackageManager(t, name)
	if mgr.Kind() != packageManagerApt {
		t.Skipf("package manager is %s, not apt", mgr.Kind())
	}
	// "tre.+" matches tree and many other packages, but no package has
	// that name.
	pattern := name[:len(name)-1] + ".+"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: fmt.Sprintf(`
resource "sysutils_package" "test" {
  name    = %q
  manager = "apt"
}`, pattern),
			ExpectError: regexp.MustCompile(`not\s+in\s+the\s+package\s+index`),
		}},
	})
	if info, err := mgr.Query(context.Background(), name); err != nil || info.Installed {
		t.Errorf("%s: installed=%v, %v", name, info.Installed, err)
	}
}
