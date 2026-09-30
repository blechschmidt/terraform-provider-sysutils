package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
)

// fakeInit simulates enough of systemctl, or of rc-service and rc-update,
// to unit test sysutils_service without an init system. It is a
// commandRunner, and a temporary /run holds the marker that makes
// detection pick its kind.
type fakeInit struct {
	t      *testing.T
	kind   string // initSystemSystemd, initSystemOpenRC or "" for none
	runDir string

	mu       sync.Mutex
	services map[string]*fakeService
	aliases  map[string]string // systemd alias -> unit
	calls    [][]string
}

type fakeService struct {
	// static services have no [Install] section (systemd); enabling them
	// succeeds without effect.
	static bool
	// exits services stop right after starting.
	exits bool
	// failRestart makes restart fail.
	failRestart bool
	// needReload services have changed unit files; start and restart
	// fail until daemon-reload ran, as if the old configuration were bad.
	needReload bool
	enabled    bool
	running    bool
	// runlevels lists the OpenRC runlevels the service is in.
	runlevels []string
	// reloads counts successful reloads.
	reloads int
}

func newFakeInit(t *testing.T, kind string) *fakeInit {
	t.Helper()
	f := &fakeInit{t: t, kind: kind, runDir: t.TempDir(), services: map[string]*fakeService{}, aliases: map[string]string{}}
	switch kind {
	case initSystemSystemd:
		if err := os.MkdirAll(filepath.Join(f.runDir, "systemd", "system"), 0o755); err != nil {
			t.Fatal(err)
		}
	case initSystemOpenRC:
		if err := os.MkdirAll(filepath.Join(f.runDir, "openrc"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.runDir, "openrc", "softlevel"), []byte("default"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *fakeInit) config() *serviceConfig {
	return &serviceConfig{
		run:      f.run,
		lookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		runDir:   f.runDir,
	}
}

func (f *fakeInit) providerFactories() map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		"sysutils": providerserver.NewProtocol6WithError(&sysutilsProvider{version: "test", service: f.config()}),
	}
}

// add defines a service. With systemd, names get a ".service" suffix.
func (f *fakeInit) add(name string, svc fakeService) *fakeService {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := svc
	f.services[name] = &s
	return &s
}

// set changes a service as if by a command run outside Terraform.
func (f *fakeInit) set(name string, enabled, running bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.services[name]
	s.running = running
	if f.kind == initSystemOpenRC {
		s.runlevels = nil
		if enabled {
			s.runlevels = []string{defaultOpenRCRunlevel}
		}
		return
	}
	s.enabled = enabled
}

func (f *fakeInit) get(name string) fakeService {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.services[name]
}

func (f *fakeInit) remove(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.services, name)
}

// changes returns the commands that change a service, in order, as
// "verb unit" (systemd) or "verb unit [runlevel]" (OpenRC).
func (f *fakeInit) changes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		switch {
		case c[0] == "systemctl" && slices.Contains([]string{"enable", "disable", "start", "stop", "restart", "reload"}, c[1]):
			out = append(out, c[1]+" "+c[len(c)-1])
		case c[0] == "systemctl" && c[1] == "daemon-reload":
			out = append(out, "daemon-reload")
		case c[0] == "rc-update" && c[1] != "show":
			out = append(out, c[1]+" "+strings.Join(c[2:], " "))
		case c[0] == "rc-service" && c[1] != "--exists" && c[len(c)-1] != "status":
			out = append(out, c[len(c)-1]+" "+c[1])
		}
	}
	return out
}

func (f *fakeInit) clearCalls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

func (f *fakeInit) run(_ context.Context, spec execSpec) (*execResult, error) {
	if spec.Timeout <= 0 {
		f.t.Errorf("fake init: %q run without a timeout", spec.Argv)
	}
	if len(spec.Argv) == 0 {
		return nil, errors.New("fake init: empty command")
	}
	res := &execResult{Stdout: newCappedOutput(spec.MaxOutputBytes), Stderr: newCappedOutput(spec.MaxOutputBytes)}
	out := func(code int, stdout, stderr string) (*execResult, error) {
		res.ExitCode = code
		_, _ = res.Stdout.Write([]byte(stdout))
		_, _ = res.Stderr.Write([]byte(stderr))
		return res, nil
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	switch spec.Argv[0] {
	case "systemctl":
		if f.kind != initSystemSystemd {
			return nil, fmt.Errorf("fake init: systemctl run on %q", f.kind)
		}
		// Drop global options and the "--" separator.
		args := []string{"systemctl"}
		for _, a := range spec.Argv[1:] {
			if a != "--no-ask-password" && a != "--no-pager" && a != "--" {
				args = append(args, a)
			}
		}
		f.calls = append(f.calls, args)
		return f.systemctl(args[1:], out)
	case "rc-service", "rc-update":
		if f.kind != initSystemOpenRC {
			return nil, fmt.Errorf("fake init: %s run on %q", spec.Argv[0], f.kind)
		}
		var args []string
		for _, a := range spec.Argv {
			if a != "--" {
				args = append(args, a)
			}
		}
		f.calls = append(f.calls, args)
		return f.openrc(args, out)
	default:
		return nil, fmt.Errorf("fake init: unexpected command %q", spec.Argv)
	}
}

type fakeOut func(code int, stdout, stderr string) (*execResult, error)

func (f *fakeInit) systemctl(args []string, out fakeOut) (*execResult, error) {
	if len(args) == 1 && args[0] == "daemon-reload" {
		for _, s := range f.services {
			s.needReload = false
		}
		return out(0, "", "")
	}
	if len(args) < 2 {
		return out(1, "", "fake systemctl: too few arguments\n")
	}
	verb, name := args[0], args[len(args)-1]
	if !strings.Contains(name, ".") {
		name += ".service"
	}
	if verb == "show" {
		unit := name
		if target, ok := f.aliases[name]; ok {
			unit = target
		}
		props := map[string]string{"LoadState": "loaded", "Id": unit, "NeedDaemonReload": "no"}
		if s := f.services[unit]; s == nil {
			props["LoadState"] = "not-found"
		} else if s.needReload {
			props["NeedDaemonReload"] = "yes"
		}
		var sb strings.Builder
		for _, a := range args[1:] {
			if p, ok := strings.CutPrefix(a, "--property="); ok {
				fmt.Fprintf(&sb, "%s=%s\n", p, props[p])
			}
		}
		return out(0, sb.String(), "")
	}
	if _, ok := f.aliases[name]; ok && (verb == "enable" || verb == "disable" || verb == "is-enabled") {
		if verb == "is-enabled" {
			return out(0, "alias\n", "")
		}
		return out(1, "", "Failed to "+verb+" unit: Refusing to operate on alias name or linked unit file: "+name+"\n")
	}
	s := f.services[name]
	switch verb {
	case "is-enabled":
		switch {
		case s == nil:
			return out(4, "not-found\n", "")
		case s.enabled:
			return out(0, "enabled\n", "")
		case s.static:
			return out(0, "static\n", "")
		default:
			return out(1, "disabled\n", "")
		}
	case "is-active":
		if s != nil && s.running {
			return out(0, "active\n", "")
		}
		return out(3, "inactive\n", "")
	}
	if s == nil {
		return out(5, "", fmt.Sprintf("Failed to %s %s: Unit %s not found.\n", verb, name, name))
	}
	switch verb {
	case "enable", "disable":
		if s.static {
			return out(0, "", "The unit files have no installation config (WantedBy=, RequiredBy=, ...).\n")
		}
		s.enabled = verb == "enable"
	case "start", "restart":
		if s.failRestart && verb == "restart" || s.needReload {
			s.running = false
			return out(1, "", fmt.Sprintf("Job for %s failed because the control process exited with error code.\n", name))
		}
		s.running = !s.exits
	case "stop":
		s.running = false
	case "reload":
		if !s.running {
			return out(1, "", fmt.Sprintf("Job for %s failed.\nUnit %s is not active.\n", name, name))
		}
		s.reloads++
	default:
		return out(1, "", fmt.Sprintf("fake systemctl: unsupported command %q\n", args))
	}
	return out(0, "", "")
}

func (f *fakeInit) openrc(args []string, out fakeOut) (*execResult, error) {
	if len(args) < 3 {
		return out(1, "", " * "+args[0]+": too few arguments\n")
	}
	if args[0] == "rc-update" {
		switch args[1] {
		case "show":
			level := args[2]
			var sb strings.Builder
			var names []string
			for n := range f.services {
				names = append(names, n)
			}
			slices.Sort(names)
			for _, n := range names {
				if slices.Contains(f.services[n].runlevels, level) {
					fmt.Fprintf(&sb, "%20s | %s\n", n, level)
				}
			}
			return out(0, sb.String(), "")
		case "add", "del":
			if len(args) != 4 {
				return out(1, "", " * rc-update: bad arguments\n")
			}
			s, level := f.services[args[2]], args[3]
			if s == nil {
				return out(1, "", fmt.Sprintf(" * rc-update: service `%s' does not exist\n", args[2]))
			}
			if level != defaultOpenRCRunlevel && level != "boot" {
				return out(1, "", fmt.Sprintf(" * rc-update: `%s' is not a valid runlevel\n", level))
			}
			if args[1] == "add" {
				if !slices.Contains(s.runlevels, level) {
					s.runlevels = append(s.runlevels, level)
				}
				return out(0, fmt.Sprintf(" * service %s added to runlevel %s\n", args[2], level), "")
			}
			if !slices.Contains(s.runlevels, level) {
				return out(1, "", fmt.Sprintf(" * rc-update: service `%s' is not in the runlevel `%s'\n", args[2], level))
			}
			s.runlevels = slices.DeleteFunc(s.runlevels, func(l string) bool { return l == level })
			return out(0, "", "")
		}
		return out(1, "", " * rc-update: unsupported command\n")
	}

	if args[1] == "--exists" {
		if f.services[args[2]] == nil {
			return out(1, "", "")
		}
		return out(0, "", "")
	}
	name, verb := args[1], args[2]
	s := f.services[name]
	if s == nil {
		return out(1, "", fmt.Sprintf(" * rc-service: service `%s' does not exist\n", name))
	}
	switch verb {
	case "status":
		if s.running {
			return out(0, " * status: started\n", "")
		}
		return out(3, " * status: stopped\n", "")
	case "start":
		s.running = !s.exits
	case "stop":
		s.running = false
	case "restart":
		if s.failRestart {
			s.running = false
			return out(1, "", fmt.Sprintf(" * ERROR: %s failed to start\n", name))
		}
		s.running = !s.exits
	case "reload":
		if !s.running {
			return out(1, "", fmt.Sprintf(" * ERROR: %s has not yet been started\n", name))
		}
		s.reloads++
	default:
		return out(1, "", " * rc-service: unsupported command "+verb+"\n")
	}
	return out(0, "", "")
}
