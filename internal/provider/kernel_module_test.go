package provider

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeModuleLoader is an in-memory module table for unit testing
// sysutils_kernel_module without privileges.
type fakeModuleLoader struct {
	mu sync.Mutex
	// modules maps loaded modules, by the name in /proc/modules, to the
	// parameters they were loaded with.
	modules map[string][]string
	calls   []string
	// inUse modules cannot be unloaded, missing ones cannot be loaded,
	// builtin ones load successfully without appearing in /proc/modules,
	// and bad parameters are rejected.
	inUse, missing, builtin map[string]bool
	badParams               map[string]bool
}

func newFakeModuleLoader() *fakeModuleLoader {
	return &fakeModuleLoader{
		modules: map[string][]string{},
		inUse:   map[string]bool{}, missing: map[string]bool{}, builtin: map[string]bool{},
		badParams: map[string]bool{},
	}
}

func (f *fakeModuleLoader) loaded() (map[string]loadedModule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mods := map[string]loadedModule{}
	for name := range f.modules {
		mods[name] = loadedModule{name: name, state: "Live"}
	}
	return mods, nil
}

func (f *fakeModuleLoader) load(_ context.Context, name string, params []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.TrimSpace("load "+name+" "+strings.Join(params, " ")))
	c := canonicalModuleName(name)
	switch {
	case f.missing[c]:
		return fmt.Errorf("modprobe: FATAL: Module %s not found", name)
	case f.builtin[c]:
		return nil
	}
	for _, p := range params {
		if f.badParams[p] {
			return fmt.Errorf("modprobe: ERROR: could not insert '%s': Invalid argument", c)
		}
	}
	if _, ok := f.modules[c]; !ok {
		f.modules[c] = slices.Clone(params)
	}
	return nil
}

func (f *fakeModuleLoader) unload(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "unload "+name)
	c := canonicalModuleName(name)
	if f.inUse[c] {
		return fmt.Errorf("modprobe: FATAL: Module %s is in use", c)
	}
	delete(f.modules, c)
	return nil
}

// loadOutOfBand simulates a module loaded outside Terraform.
func (f *fakeModuleLoader) loadOutOfBand(name string, params ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.modules[canonicalModuleName(name)] = params
}

func (f *fakeModuleLoader) unloadOutOfBand(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.modules, canonicalModuleName(name))
}

func (f *fakeModuleLoader) params(name string) ([]string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.modules[canonicalModuleName(name)]
	return p, ok
}

func (f *fakeModuleLoader) callCount(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeModuleLoader) set(m map[string]bool, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m[canonicalModuleName(name)] = true
}

func TestValidateModuleNames(t *testing.T) {
	for _, n := range []string{"dummy", "br_netfilter", "nf-conntrack", "8021q", "a", strings.Repeat("a", 55)} {
		if err := validateModuleName(n); err != nil {
			t.Errorf("validateModuleName(%q) = %v", n, err)
		}
	}
	for _, n := range []string{"", "-r", "_x", "../../etc/passwd", "a/b", "a.b", "a b", "dummy\n", "a;b", strings.Repeat("a", 56)} {
		if err := validateModuleName(n); err == nil {
			t.Errorf("validateModuleName(%q) succeeded", n)
		}
	}
	for _, n := range []string{"numdummies", "fb.lockless_register_fb", "_x", "debug-level"} {
		if err := validateModuleParamName(n); err != nil {
			t.Errorf("validateModuleParamName(%q) = %v", n, err)
		}
	}
	for _, n := range []string{"", "-x", ".x", "a b", "a=b", "a\"b"} {
		if err := validateModuleParamName(n); err == nil {
			t.Errorf("validateModuleParamName(%q) succeeded", n)
		}
	}
	for _, v := range []string{"", "1", "Y", "0x1f", "1,2,3", "/lib/firmware/x.bin", "a:b=c", "50%"} {
		if err := validateModuleParamValue(v); err != nil {
			t.Errorf("validateModuleParamValue(%q) = %v", v, err)
		}
	}
	for _, v := range []string{"a b", "\"x\"", "a#b", "a\nb", "a\\b", "'x'", strings.Repeat("1", maxModuleParamValueLen+1)} {
		if err := validateModuleParamValue(v); err == nil {
			t.Errorf("validateModuleParamValue(%q) succeeded", v)
		}
	}
}

func TestParseProcModules(t *testing.T) {
	const data = `dummy 16384 0 - Live 0x0000000000000000
br_netfilter 32768 0 - Live 0x0000000000000000
bridge 311296 1 br_netfilter, Live 0x0000000000000000
nvidia 56713216 2 nvidia_modeset, Live 0x0000000000000000 (POE)
going 16384 0 - Unloading 0x0000000000000000
`
	mods, err := parseProcModules(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := slices.Sorted(maps.Keys(mods)); !slices.Equal(got, []string{"br_netfilter", "bridge", "dummy", "going", "nvidia"}) {
		t.Errorf("modules = %q", got)
	}
	if m := mods["bridge"]; m.refcount != 1 || m.state != "Live" {
		t.Errorf("bridge = %+v", m)
	}
	if m := mods["going"]; m.state != "Unloading" {
		t.Errorf("going = %+v", m)
	}
	if _, err := parseProcModules("broken line\n"); err == nil {
		t.Error("parsing a malformed line succeeded")
	}

	f := newFakeModuleLoader()
	f.loadOutOfBand("nf_conntrack")
	if ok, err := isModuleLoaded(f, "nf-conntrack"); err != nil || !ok {
		t.Errorf("isModuleLoaded(nf-conntrack) = %v, %v; want true, as - and _ are alike", ok, err)
	}
}

func TestModprobeOptionsRoundTrip(t *testing.T) {
	params := map[string]string{"numdummies": "2", "b": "x,y", "flag": ""}
	data := renderModprobeOptions("dummy", params)
	if want := moduleConfHeader + "\noptions dummy b=x,y flag= numdummies=2\n"; string(data) != want {
		t.Errorf("rendered\n%s\nwant\n%s", data, want)
	}
	got, found := parseModprobeOptions(data, "dummy")
	if !found || !maps.Equal(got, params) {
		t.Errorf("parsed %v, %v; want %v", got, found, params)
	}

	const handWritten = "# comment\noptions other x=1\noptions dummy_mod a=1 \\\n  b=2\noptions dummy-mod a=3 debug\nblacklist dummy_mod\n"
	got, found = parseModprobeOptions([]byte(handWritten), "dummy-mod")
	if want := map[string]string{"a": "3", "b": "2", "debug": ""}; !found || !maps.Equal(got, want) {
		t.Errorf("parsed hand-written file: %v, %v; want %v", got, found, want)
	}
	if _, found := parseModprobeOptions([]byte(handWritten), "missing"); found {
		t.Error("found options for a module the file does not mention")
	}
}

func TestSystemModuleLoaderCommands(t *testing.T) {
	var last execSpec
	l := systemModuleLoader{run: cannedRunner(&last, 0, "", "", false), timeout: time.Minute}
	ctx := context.Background()

	if err := l.load(ctx, "dummy", []string{"numdummies=2"}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"modprobe", "--", "dummy", "numdummies=2"}; !slices.Equal(last.Argv, want) {
		t.Errorf("load argv = %q, want %q", last.Argv, want)
	}
	if last.Timeout != time.Minute || last.MaxOutputBytes == 0 {
		t.Errorf("load spec: timeout %s, max output %d", last.Timeout, last.MaxOutputBytes)
	}
	if err := l.unload(ctx, "dummy"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"modprobe", "-r", "--", "dummy"}; !slices.Equal(last.Argv, want) {
		t.Errorf("unload argv = %q, want %q", last.Argv, want)
	}

	l.run = cannedRunner(&last, 1, "", "modprobe: FATAL: Module nope not found in directory /lib/modules/6.8.0\n", false)
	if err := l.load(ctx, "nope", nil); err == nil || !strings.Contains(err.Error(), "not found in directory") {
		t.Errorf("failed load: %v", err)
	}
	l.run = cannedRunner(&last, 0, "", "", true)
	if err := l.load(ctx, "slow", nil); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("timed-out load: %v", err)
	}
}

func TestWriteModuleConf(t *testing.T) {
	p := t.TempDir() + "/modules-load.d/x.conf"
	if changed, err := writeModuleConf(p, nil); err != nil || changed {
		t.Errorf("removing a missing file: %v, %v", changed, err)
	}
	if changed, err := writeModuleConf(p, renderModulesLoad("x")); err != nil || !changed {
		t.Fatalf("creating: %v, %v", changed, err)
	}
	if changed, err := writeModuleConf(p, renderModulesLoad("x")); err != nil || changed {
		t.Errorf("rewriting the same content: %v, %v", changed, err)
	}
	if changed, err := writeModuleConf(p, nil); err != nil || !changed {
		t.Errorf("removing: %v, %v", changed, err)
	}
	if data, _, err := readModuleConf(p); err != nil || data != nil {
		t.Errorf("after removal: %q, %v", data, err)
	}
}
