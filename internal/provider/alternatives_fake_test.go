package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
)

// fakeAlternatives simulates enough of dpkg's update-alternatives or of
// chkconfig's alternatives to unit test sysutils_alternatives without
// touching the host's link groups. It is a commandRunner. For chkconfig it
// also keeps the administrative files, whose second line records the
// master link, in a temporary directory.
type fakeAlternatives struct {
	t        *testing.T
	kind     string // alternativesDebian or alternativesRHEL
	adminDir string

	mu     sync.Mutex
	groups map[string]*fakeLinkGroup
	calls  [][]string
	// failSet makes --set exit with an error.
	failSet bool
	// ignoreSet makes --set succeed without effect.
	ignoreSet bool
	// checkPaths replaces the provider's path check. It is a no-op by
	// default: the fake's paths are made up, and whether the real
	// directories above them (/usr/local/bin, /opt) are trusted depends on
	// the host, for example on CI runners that run tests as a normal user.
	checkPaths func(link, path string) error
}

type fakeLinkGroup struct {
	link   string
	manual bool
	value  string
	alts   []alternativeEntry
}

// best returns the alternative with the highest priority, the first one
// on ties.
func (g *fakeLinkGroup) best() string {
	best := ""
	var prio int64
	for _, a := range g.alts {
		if best == "" || a.Priority > prio {
			best, prio = a.Path, a.Priority
		}
	}
	return best
}

func newFakeAlternatives(t *testing.T, kind string) *fakeAlternatives {
	t.Helper()
	return &fakeAlternatives{
		t: t, kind: kind, adminDir: t.TempDir(), groups: map[string]*fakeLinkGroup{},
		checkPaths: func(string, string) error { return nil },
	}
}

// fakeAlternativesDir is where the fake's commands appear to be. It must
// not exist: detection resolves symlinks, and on Fedora the real
// /usr/sbin/update-alternatives is a symlink to alternatives.
const fakeAlternativesDir = "/nonexistent/sysutils-fake-alternatives/"

func (f *fakeAlternatives) config() *alternativesConfig {
	return &alternativesConfig{
		run: f.run,
		lookPath: func(name string) (string, error) {
			if name == f.kind {
				return fakeAlternativesDir + name, nil
			}
			return "", errors.New("not found")
		},
		rhelAdminDir: f.adminDir,
		checkPaths:   f.checkPaths,
	}
}

func (f *fakeAlternatives) providerFactories() map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		"sysutils": providerserver.NewProtocol6WithError(&sysutilsProvider{version: "test", alternatives: f.config()}),
	}
}

// add registers alternatives as if by a package, in automatic mode.
func (f *fakeAlternatives) add(name, link string, alts ...alternativeEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g := &fakeLinkGroup{link: link, alts: alts}
	g.value = g.best()
	f.groups[name] = g
	f.syncAdminFile(name)
}

// set changes a link group as if by a command run outside Terraform.
func (f *fakeAlternatives) set(name string, manual bool, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g := f.groups[name]
	g.manual = manual
	g.value = value
	if !manual {
		g.value = g.best()
	}
	f.syncAdminFile(name)
}

func (f *fakeAlternatives) get(name string) *fakeLinkGroup {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, ok := f.groups[name]
	if !ok {
		return nil
	}
	c := *g
	c.alts = slices.Clone(g.alts)
	return &c
}

// changes returns the commands that change a link group, in order, with
// the command name and options stripped: "set editor /bin/nano".
func (f *fakeAlternatives) changes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if c[0] == "--query" || c[0] == "--display" {
			continue
		}
		out = append(out, strings.TrimPrefix(strings.Join(c, " "), "--"))
	}
	return out
}

func (f *fakeAlternatives) clearCalls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

// syncAdminFile writes chkconfig's administrative file of a group, or
// removes it if the group is gone.
func (f *fakeAlternatives) syncAdminFile(name string) {
	if f.kind != alternativesRHEL {
		return
	}
	p := filepath.Join(f.adminDir, name)
	g, ok := f.groups[name]
	if !ok {
		_ = os.Remove(p)
		return
	}
	mode := alternativesModeAuto
	if g.manual {
		mode = alternativesModeManual
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s\n%s\n\n", mode, g.link)
	for _, a := range g.alts {
		fmt.Fprintf(&sb, "%s\n%d\n", a.Path, a.Priority)
	}
	if err := os.WriteFile(p, []byte(sb.String()), 0o644); err != nil {
		f.t.Errorf("fake alternatives: %v", err)
	}
}

func (f *fakeAlternatives) run(_ context.Context, spec execSpec) (*execResult, error) {
	if spec.Timeout <= 0 {
		f.t.Errorf("fake alternatives: %q run without a timeout", spec.Argv)
	}
	if len(spec.Argv) < 2 {
		return nil, errors.New("fake alternatives: too few arguments")
	}
	if want := fakeAlternativesDir + f.kind; spec.Argv[0] != want {
		return nil, fmt.Errorf("fake alternatives: ran %q, want %q", spec.Argv[0], want)
	}
	res := &execResult{Stdout: newCappedOutput(spec.MaxOutputBytes), Stderr: newCappedOutput(spec.MaxOutputBytes)}
	out := func(code int, stdout, stderr string) (*execResult, error) {
		res.ExitCode = code
		_, _ = res.Stdout.Write([]byte(stdout))
		_, _ = res.Stderr.Write([]byte(stderr))
		return res, nil
	}
	fail := func(msg string) (*execResult, error) {
		if f.kind == alternativesDebian {
			return out(2, "", "update-alternatives: error: "+msg+"\n")
		}
		return out(2, "", msg+"\n")
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	args := spec.Argv[1:]
	// Names and paths must never look like options; only the priority of
	// --install may be negative.
	for i, a := range args[1:] {
		if strings.HasPrefix(a, "-") && (args[0] != "--install" || i != 3) {
			f.t.Errorf("fake alternatives: option-like argument %q in %q", a, spec.Argv)
		}
	}
	f.calls = append(f.calls, slices.Clone(args))
	verb, rest := args[0], args[1:]
	arity := map[string]int{"--query": 1, "--display": 1, "--install": 4, "--set": 2, "--auto": 1, "--remove": 2}
	if n, ok := arity[verb]; !ok || n != len(rest) {
		return fail(fmt.Sprintf("unsupported command %q", args))
	}
	if (verb == "--query" && f.kind != alternativesDebian) || (verb == "--display" && f.kind != alternativesRHEL) {
		return fail("unknown option " + verb)
	}
	name := rest[0]
	if verb == "--install" {
		name = rest[1]
	}
	g := f.groups[name]
	if g == nil && verb != "--install" {
		if f.kind == alternativesDebian {
			return fail("no alternatives for " + name)
		}
		return out(2, "", "")
	}
	defer f.syncAdminFile(name)

	switch verb {
	case "--query":
		return out(0, f.debianQuery(name, g), "")
	case "--display":
		return out(0, f.rhelDisplay(name, g), "")
	case "--install":
		link, p := rest[0], rest[2]
		prio, err := strconv.ParseInt(rest[3], 10, 64)
		if err != nil {
			return fail("priority must be an integer")
		}
		if g == nil {
			g = &fakeLinkGroup{}
			f.groups[name] = g
		}
		g.link = link
		i := slices.IndexFunc(g.alts, func(a alternativeEntry) bool { return a.Path == p })
		if i < 0 {
			g.alts = append(g.alts, alternativeEntry{Path: p, Priority: prio})
		} else {
			g.alts[i].Priority = prio
		}
		if !g.manual {
			g.value = g.best()
		}
		return out(0, "", "")
	case "--set":
		p := rest[1]
		if !slices.ContainsFunc(g.alts, func(a alternativeEntry) bool { return a.Path == p }) {
			return fail(fmt.Sprintf("alternative %s for %s not registered; not setting", p, name))
		}
		if f.failSet {
			return fail("cannot set")
		}
		if !f.ignoreSet {
			g.manual, g.value = true, p
		}
		return out(0, "", "")
	case "--auto":
		g.manual = false
		g.value = g.best()
		return out(0, "", "")
	case "--remove":
		p := rest[1]
		g.alts = slices.DeleteFunc(g.alts, func(a alternativeEntry) bool { return a.Path == p })
		if len(g.alts) == 0 {
			delete(f.groups, name)
			return out(0, "", "")
		}
		if g.value == p {
			g.manual = false
		}
		if !g.manual {
			g.value = g.best()
		}
		return out(0, "", "")
	}
	return fail("unreachable")
}

func (f *fakeAlternatives) debianQuery(name string, g *fakeLinkGroup) string {
	mode, value := alternativesModeAuto, g.value
	if g.manual {
		mode = alternativesModeManual
	}
	if value == "" {
		value = "none"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Name: %s\nLink: %s\nSlaves:\n %s.1.gz /usr/share/man/man1/%s.1.gz\nStatus: %s\nBest: %s\nValue: %s\n", name, g.link, name, name, mode, g.best(), value)
	for _, a := range g.alts {
		fmt.Fprintf(&sb, "\nAlternative: %s\nPriority: %d\nSlaves:\n %s.1.gz /usr/share/man/man1/%s.1.gz\n", a.Path, a.Priority, name, filepath.Base(a.Path))
	}
	return sb.String()
}

func (f *fakeAlternatives) rhelDisplay(name string, g *fakeLinkGroup) string {
	mode := alternativesModeAuto
	if g.manual {
		mode = alternativesModeManual
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s - status is %s.\n", name, mode)
	if g.value == "" {
		sb.WriteString(" link currently absent\n")
	} else {
		fmt.Fprintf(&sb, " link currently points to %s\n", g.value)
	}
	for i, a := range g.alts {
		if i%2 == 1 {
			fmt.Fprintf(&sb, "%s - family %s priority %d\n", a.Path, filepath.Base(a.Path), a.Priority)
		} else {
			fmt.Fprintf(&sb, "%s - priority %d\n", a.Path, a.Priority)
		}
		fmt.Fprintf(&sb, " follower %s.1.gz: /usr/share/man/man1/%s.1.gz\n", name, filepath.Base(a.Path))
	}
	fmt.Fprintf(&sb, "Current `best' version is %s.\n", g.best())
	return sb.String()
}
