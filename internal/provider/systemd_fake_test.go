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
type fakeSystemd struct {
	t       *testing.T
	unitDir string

	mu       sync.Mutex
	loaded   map[string]string // name -> content at the last daemon-reload
	enabled  map[string]bool
	active   map[string]string
	vendor   map[string]string // name -> FragmentPath of units outside unitDir
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
	content, loaded := f.loaded[name]
	fileContent, fileErr := os.ReadFile(f.unitFile(name))
	fileExists := fileErr == nil

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
		return out(0, "", "")
	case "show":
		props := map[string]string{"LoadState": "not-found", "FragmentPath": ""}
		switch {
		case f.vendor[name] != "":
			props = map[string]string{"LoadState": "loaded", "FragmentPath": f.vendor[name]}
		case loaded && strings.Contains(content, "[Broken]"):
			props = map[string]string{"LoadState": "bad-setting", "FragmentPath": f.unitFile(name)}
		case loaded:
			props = map[string]string{"LoadState": "loaded", "FragmentPath": f.unitFile(name)}
		}
		var sb strings.Builder
		for _, a := range args[1:] {
			if p, ok := strings.CutPrefix(a, "--property="); ok {
				fmt.Fprintf(&sb, "%s=%s\n", p, props[p])
			}
		}
		return out(0, sb.String(), "")
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
