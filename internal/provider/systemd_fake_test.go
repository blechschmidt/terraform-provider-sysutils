package provider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
)

// fakeSystemd simulates enough of systemctl, backed by a real unit directory,
// to unit test sysutils_systemd_unit without systemd.
//
// A unit is known to the fake manager only after daemon-reload has seen its
// file, like in systemd. Units whose content contains "ExecStart=/bin/false"
// fail to start; Type=oneshot units without RemainAfterExit=yes become
// inactive right after starting. Units without an [Install] section are
// static and cannot be enabled; like in systemd, an enabled unit whose
// [Install] section is removed stays enabled until it is re-enabled.
//
// Instances such as "app@one.service" load from their own file or from their
// template's file. Templates cannot be shown, started or queried with
// is-active, and can only be enabled with DefaultInstance=. show accepts a
// glob pattern and then prints one block per matching loaded unit. Drop-in
// files in "<unit>.d" directories are part of a unit's content.
type fakeSystemd struct {
	t       *testing.T
	unitDir string

	mu       sync.Mutex
	loaded   map[string]string // name -> content at the last daemon-reload
	enabled  map[string]bool
	active   map[string]string
	vendor   map[string]string // name -> FragmentPath of units outside unitDir
	implicit map[string]string // name -> ActiveState of units without a unit file, such as kernel mounts
	calls    [][]string
	timeouts map[string]bool // verbs that time out
}

func newFakeSystemd(t *testing.T) *fakeSystemd {
	t.Helper()
	return &fakeSystemd{
		t:        t,
		unitDir:  t.TempDir(),
		loaded:   map[string]string{},
		enabled:  map[string]bool{},
		active:   map[string]string{},
		vendor:   map[string]string{},
		implicit: map[string]string{},
		timeouts: map[string]bool{},
	}
}

// providerFactories returns provider factories whose systemd unit resource
// talks to f.
func (f *fakeSystemd) providerFactories() map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		"sysutils": providerserver.NewProtocol6WithError(&sysutilsProvider{
			version: "test",
			systemd: &systemdConfig{unitDir: f.unitDir, run: f.run},
		}),
	}
}

func (f *fakeSystemd) unitFile(name string) string { return filepath.Join(f.unitDir, name) }

// callsOf returns how many times verb was run, optionally for one unit.
func (f *fakeSystemd) callsOf(verb, unit string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if len(c) > 0 && c[0] == verb && (unit == "" || c[len(c)-1] == unit) {
			n++
		}
	}
	return n
}

func (f *fakeSystemd) set(name string, enabled bool, active string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enabled[name] = enabled
	f.active[name] = active
}

func (f *fakeSystemd) get(name string) (enabled bool, active string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	active = f.active[name]
	if active == "" {
		active = "inactive"
	}
	return f.enabled[name], active
}

func (f *fakeSystemd) run(_ context.Context, spec execSpec) (*execResult, error) {
	if len(spec.Argv) == 0 || spec.Argv[0] != "systemctl" {
		return nil, fmt.Errorf("fake systemd: unexpected command %q", spec.Argv)
	}
	if spec.Timeout <= 0 {
		f.t.Errorf("fake systemd: %q run without a timeout", spec.Argv)
	}
	// Drop global options and the "--" separator.
	var args []string
	rest := spec.Argv[1:]
	for i, a := range rest {
		if a == "--" {
			args = append(args, rest[i+1:]...)
			break
		}
		if a == "--no-ask-password" || a == "--no-pager" {
			continue
		}
		args = append(args, a)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)

	res := &execResult{Stdout: newCappedOutput(spec.MaxOutputBytes), Stderr: newCappedOutput(spec.MaxOutputBytes)}
	out := func(code int, stdout, stderr string) (*execResult, error) {
		res.ExitCode = code
		_, _ = res.Stdout.Write([]byte(stdout))
		_, _ = res.Stderr.Write([]byte(stderr))
		return res, nil
	}
	if len(args) == 0 {
		return out(1, "", "fake systemd: no command\n")
	}
	verb := args[0]
	if f.timeouts[verb] {
		res.TimedOut = true
		res.ExitCode = -1
		return res, nil
	}
	name := args[len(args)-1]
	content, _, loaded := f.resolve(name)
	fileContent, fileErr := os.ReadFile(f.unitFile(name))
	fileExists := fileErr == nil
	template := isTemplateUnit(name)
	if template {
		switch verb {
		case "show", "is-active", "start", "stop", "restart", "try-restart", "reset-failed":
			return out(1, "", fmt.Sprintf("Unit name %s is neither a valid invocation ID nor unit name.\n", name))
		}
	}

	switch verb {
	case "daemon-reload":
		f.loaded = map[string]string{}
		entries, err := os.ReadDir(f.unitDir)
		if err != nil {
			return out(1, "", err.Error())
		}
		for _, e := range entries {
			data, err := os.ReadFile(f.unitFile(e.Name()))
			if err == nil {
				f.loaded[e.Name()] = string(data)
			}
		}
		for _, e := range entries {
			if unit, ok := strings.CutSuffix(e.Name(), ".d"); ok && e.IsDir() {
				dropIns, _ := filepath.Glob(filepath.Join(f.unitDir, e.Name(), "*.conf"))
				for _, d := range dropIns {
					data, _ := os.ReadFile(d)
					f.loaded[unit] += "\n" + string(data)
				}
			}
		}
		return out(0, "", "")
	case "show":
		names := []string{name}
		if strings.Contains(name, "*") {
			names = nil
			for _, n := range f.knownUnits() {
				if ok, _ := filepath.Match(name, n); ok {
					names = append(names, n)
				}
			}
		}
		var blocks []string
		for _, n := range names {
			c, frag, ok := f.resolve(n)
			props := map[string]string{"Id": n, "LoadState": "not-found", "FragmentPath": "", "ActiveState": f.active[n]}
			switch {
			case f.implicit[n] != "" && !ok:
				props["LoadState"], props["ActiveState"] = "loaded", f.implicit[n]
			case f.vendor[n] != "":
				props["LoadState"], props["FragmentPath"] = "loaded", f.vendor[n]
			case ok && strings.Contains(c, "[Broken]"):
				props["LoadState"], props["FragmentPath"] = "bad-setting", frag
			case ok:
				props["LoadState"], props["FragmentPath"] = "loaded", frag
			}
			var sb strings.Builder
			for _, a := range args[1:] {
				if p, ok := strings.CutPrefix(a, "--property="); ok {
					fmt.Fprintf(&sb, "%s=%s\n", p, props[p])
				}
			}
			blocks = append(blocks, sb.String())
		}
		return out(0, strings.Join(blocks, "\n"), "")
	case "is-enabled":
		switch {
		case !fileExists:
			return out(1, "", fmt.Sprintf("Failed to get unit file state for %s: No such file or directory\n", name))
		case f.enabled[name]:
			// Existing symlinks win, even if [Install] was removed since.
			return out(0, "enabled\n", "")
		case !strings.Contains(string(fileContent), "[Install]"):
			return out(0, "static\n", "")
		default:
			return out(1, "disabled\n", "")
		}
	case "is-active":
		state := f.active[name]
		if state == "" {
			state = f.implicit[name]
		}
		if state == "" {
			state = "inactive"
		}
		code := 3
		if state == "active" {
			code = 0
		}
		return out(code, state+"\n", "")
	case "enable", "disable", "reenable":
		if !fileExists {
			return out(1, "", fmt.Sprintf("Failed to %s unit: Unit file %s does not exist.\n", verb, name))
		}
		if template && !strings.Contains(string(fileContent), "DefaultInstance=") {
			return out(1, "", fmt.Sprintf("Failed to %s unit: Unit %s is a template without DefaultInstance=.\n", verb, name))
		}
		if !strings.Contains(string(fileContent), "[Install]") {
			// reenable disables first, which removes the old symlinks.
			if verb != "enable" {
				f.enabled[name] = false
			}
			return out(0, "", "The unit files have no installation config (WantedBy=, RequiredBy=, ...).\n")
		}
		f.enabled[name] = verb != "disable"
		return out(0, "", "")
	case "start", "restart", "try-restart":
		if !loaded {
			return out(5, "", fmt.Sprintf("Failed to %s %s: Unit %s not found.\n", verb, name, name))
		}
		if verb == "try-restart" && f.active[name] != "active" {
			return out(0, "", "")
		}
		switch {
		case strings.Contains(content, "ExecStart=/bin/false"):
			f.active[name] = "failed"
			return out(1, "", fmt.Sprintf("Job for %s failed because the control process exited with error code.\n", name))
		case strings.Contains(content, "Type=oneshot") && !strings.Contains(content, "RemainAfterExit=yes"):
			f.active[name] = "inactive"
		default:
			f.active[name] = "active"
		}
		return out(0, "", "")
	case "stop":
		if !loaded && f.active[name] == "" {
			return out(5, "", fmt.Sprintf("Failed to stop %s: Unit %s not loaded.\n", name, name))
		}
		f.active[name] = "inactive"
		return out(0, "", "")
	case "reset-failed":
		if f.active[name] == "failed" {
			f.active[name] = "inactive"
		}
		return out(0, "", "")
	default:
		return out(1, "", fmt.Sprintf("fake systemd: unsupported command %q\n", args))
	}
}

// resolve returns the content and fragment path that name is loaded from:
// its own file or, for an instance, its template's file. It must be called
// with f.mu held.
func (f *fakeSystemd) resolve(name string) (content, fragment string, loaded bool) {
	if c, ok := f.loaded[name]; ok {
		return c, f.unitFile(name), true
	}
	at := strings.IndexByte(name, '@')
	dot := strings.LastIndexByte(name, '.')
	if at > 0 && dot > at+1 {
		tmpl := name[:at+1] + name[dot:]
		if c, ok := f.loaded[tmpl]; ok {
			return c, f.unitFile(tmpl), true
		}
	}
	return "", "", false
}

// knownUnits returns the units the fake manager has loaded: unit files
// other than templates, and units that were started. It must be called
// with f.mu held.
func (f *fakeSystemd) knownUnits() []string {
	seen := map[string]bool{}
	for n := range f.loaded {
		if !isTemplateUnit(n) {
			seen[n] = true
		}
	}
	for n := range f.active {
		seen[n] = true
	}
	return sortedKeys(seen)
}
