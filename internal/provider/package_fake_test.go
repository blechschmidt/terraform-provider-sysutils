package provider

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
)

// fakePackageManager is an in-memory package manager for unit testing
// sysutils_package without root or a real package manager. Versions compare
// as strings, which is enough for the versions the tests use.
type fakePackageManager struct {
	mu   sync.Mutex
	kind string
	// available lists the versions each package has in the (fake)
	// repositories, oldest first. Packages in stale are only available
	// after UpdateCache.
	available map[string][]string
	stale     map[string][]string
	installed map[string]string
	// held packages are silently left alone by Install, Upgrade and
	// Remove, as apt-mark hold does for upgrades.
	held map[string]bool
	// provides maps virtual package names to the real package that
	// installing them installs.
	provides map[string]string
	calls    []string
	// updateErr, if set, is returned by UpdateCache.
	updateErr error
	// inspectErr, if set, is returned by Inspect.
	inspectErr error
}

func newFakePackageManager(kind string) *fakePackageManager {
	return &fakePackageManager{
		kind:      kind,
		available: map[string][]string{},
		stale:     map[string][]string{},
		installed: map[string]string{},
		held:      map[string]bool{},
		provides:  map[string]string{},
	}
}

// packageProviderFactories returns provider factories whose
// sysutils_package uses f whatever the manager attribute says.
func packageProviderFactories(f *fakePackageManager) map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		"sysutils": providerserver.NewProtocol6WithError(&sysutilsProvider{
			version: "test",
			pkg: &packageConfig{manager: func(kind string) (packageManager, error) {
				if kind != packageManagerAuto && kind != f.kind {
					return nil, fmt.Errorf("package manager %s is not available: %s not found in PATH", kind, kind)
				}
				return f, nil
			}},
		}),
	}
}

func (f *fakePackageManager) Kind() string { return f.kind }

func (f *fakePackageManager) record(format string, args ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *fakePackageManager) newest(name string) string {
	v := f.available[name]
	if len(v) == 0 {
		return ""
	}
	return v[len(v)-1]
}

func (f *fakePackageManager) Query(_ context.Context, name string) (packageInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("query %s", name)
	v, ok := f.installed[name]
	return packageInfo{Installed: ok, Version: v}, nil
}

func (f *fakePackageManager) UpToDate(_ context.Context, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("uptodate %s", name)
	newest := f.newest(name)
	return newest == "" || f.installed[name] >= newest, nil
}

func (f *fakePackageManager) Install(_ context.Context, name, version string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("install %s %s", name, version)
	if real, ok := f.provides[name]; ok {
		f.installed[real] = f.newest(real)
		return nil
	}
	if len(f.available[name]) == 0 {
		return fmt.Errorf("E: Unable to locate package %s", name)
	}
	if f.held[name] {
		return nil
	}
	if version == "" {
		f.installed[name] = f.newest(name)
		return nil
	}
	if !slices.Contains(f.available[name], version) {
		return fmt.Errorf("E: Version '%s' for '%s' was not found", version, name)
	}
	f.installed[name] = version
	return nil
}

func (f *fakePackageManager) Upgrade(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("upgrade %s", name)
	if len(f.available[name]) == 0 {
		return fmt.Errorf("E: Unable to locate package %s", name)
	}
	if f.held[name] {
		return nil
	}
	f.installed[name] = f.newest(name)
	return nil
}

func (f *fakePackageManager) Remove(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("remove %s", name)
	if name == "essential" {
		return errors.New("E: Removing essential system-critical packages is not permitted")
	}
	if !f.held[name] {
		delete(f.installed, name)
	}
	return nil
}

// Inspect reports installed packages as built for "amd64", and the newest
// available version as the candidate.
func (f *fakePackageManager) Inspect(_ context.Context, name string) (packageDetails, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("inspect %s", name)
	if f.inspectErr != nil {
		return packageDetails{}, f.inspectErr
	}
	d := packageDetails{Candidate: f.newest(name)}
	if v, ok := f.installed[name]; ok {
		d.packageInfo = packageInfo{Installed: true, Version: v, Architecture: "amd64"}
	}
	return d, nil
}

func (f *fakePackageManager) UpdateCache(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("update")
	if f.updateErr != nil {
		return f.updateErr
	}
	for name, versions := range f.stale {
		f.available[name] = append(f.available[name], versions...)
	}
	f.stale = map[string][]string{}
	return nil
}

// set changes the package database as if by a command run outside
// Terraform.
func (f *fakePackageManager) set(name, version string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if version == "" {
		delete(f.installed, name)
	} else {
		f.installed[name] = version
	}
}

func (f *fakePackageManager) version(name string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.installed[name]
	return v, ok
}

// changes returns the calls that change the host, in order.
func (f *fakePackageManager) changes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "query ") && !strings.HasPrefix(c, "uptodate ") && !strings.HasPrefix(c, "inspect ") {
			out = append(out, c)
		}
	}
	return out
}
