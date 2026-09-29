package provider

// Kernel module handling behind sysutils_kernel_module. Loading and
// unloading go through the moduleLoader interface so that unit tests can
// substitute a fake; the modules-load.d and modprobe.d directories are
// injectable as well.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	// defaultModulesLoadDir holds the lists of modules systemd-modules-load
	// loads at boot.
	defaultModulesLoadDir = "/etc/modules-load.d"
	// defaultModprobeDir holds the modprobe configuration, including the
	// options modprobe passes when it loads a module.
	defaultModprobeDir = "/etc/modprobe.d"
	// defaultProcModules lists the loaded modules.
	defaultProcModules = "/proc/modules"
	// modprobeTimeout bounds each modprobe invocation. Loading a module can
	// wait for firmware or hardware, but must not hang the apply forever.
	modprobeTimeout = 2 * time.Minute
	// modprobeOutputLimit caps how much modprobe output is kept per stream.
	modprobeOutputLimit = 64 << 10
	// maxModuleConfSize bounds how much of a managed configuration file is
	// read.
	maxModuleConfSize = 1 << 20
	// moduleConfMode is the mode of the configuration files written by
	// sysutils_kernel_module.
	moduleConfMode fs.FileMode = 0o644
	// maxModuleParamValueLen bounds the length of a parameter value.
	maxModuleParamValueLen = 1024
	// moduleConfHeader starts every configuration file the resource writes.
	moduleConfHeader = "# Managed by Terraform (sysutils_kernel_module). Manual changes will be reverted."
)

var (
	// moduleNamePattern matches module names. The kernel limits them to 55
	// bytes (MODULE_NAME_LEN - 1 on 64-bit), and in practice they consist
	// of letters, digits, "_" and "-", which modprobe treats alike. The
	// leading character must not be "-", so a name is never an option.
	moduleNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,54}$`)
	// moduleParamNamePattern matches module parameter names. Dots separate
	// the prefix of parameters that belong to a group, as in
	// "fb.lockless_register_fb".
	moduleParamNamePattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	// moduleParamValuePattern matches module parameter values. Quotes, white
	// space and "#" are excluded because they would need quoting that
	// modprobe.d files and the kernel's parameter parser handle
	// differently.
	moduleParamValuePattern = regexp.MustCompile(`^[A-Za-z0-9_.,:/+=@%-]*$`)
)

// validateModuleName reports why name is not an acceptable module name.
func validateModuleName(name string) error {
	if !moduleNamePattern.MatchString(name) {
		return fmt.Errorf("module name %q must be 1 to 55 letters, digits, \"_\" or \"-\", starting with a letter or digit", name)
	}
	return nil
}

func validateModuleParamName(name string) error {
	if !moduleParamNamePattern.MatchString(name) {
		return fmt.Errorf("parameter name %q must be 1 to 128 letters, digits, \"_\", \"-\" or \".\", not starting with \"-\" or \".\"", name)
	}
	return nil
}

func validateModuleParamValue(v string) error {
	if len(v) > maxModuleParamValueLen || !moduleParamValuePattern.MatchString(v) {
		return fmt.Errorf("parameter value %q must be at most 1024 letters, digits or characters of \"_.,:/+=@%%-\"; white space, quotes and \"#\" are not supported", v)
	}
	return nil
}

// canonicalModuleName returns name as the kernel reports it in
// /proc/modules, where "-" is always "_".
func canonicalModuleName(name string) string {
	return strings.ReplaceAll(name, "-", "_")
}

// moduleParamArgs returns params as "name=value" arguments, sorted by name.
func moduleParamArgs(params map[string]string) []string {
	args := make([]string, 0, len(params))
	for k, v := range params {
		args = append(args, k+"="+v)
	}
	slices.Sort(args)
	return args
}

// loadedModule is a line of /proc/modules.
type loadedModule struct {
	name string
	// state is "Live", "Loading" or "Unloading".
	state string
	// refcount is the number of users, or -1 if the kernel does not track
	// them for the module.
	refcount int
}

// parseProcModules parses /proc/modules, whose lines have the form
// "name size refcount dependents state address [taints]".
func parseProcModules(data string) (map[string]loadedModule, error) {
	mods := map[string]loadedModule{}
	sc := bufio.NewScanner(strings.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		if len(f) < 5 {
			return nil, fmt.Errorf("line %d of /proc/modules has %d fields, want at least 5", n, len(f))
		}
		refcount := -1
		if f[2] != "-" {
			if _, err := fmt.Sscanf(f[2], "%d", &refcount); err != nil {
				return nil, fmt.Errorf("line %d of /proc/modules: invalid reference count %q", n, f[2])
			}
		}
		mods[f[0]] = loadedModule{name: f[0], state: f[4], refcount: refcount}
	}
	return mods, sc.Err()
}

// moduleLoader loads and unloads kernel modules and lists the loaded ones.
// systemModuleLoader is the production implementation.
type moduleLoader interface {
	// loaded returns the loaded modules by the name in /proc/modules.
	loaded() (map[string]loadedModule, error)
	// load loads name and its dependencies with the given "name=value"
	// parameters. Loading a module that is already loaded does nothing.
	load(ctx context.Context, name string, params []string) error
	// unload unloads name and the dependencies that are no longer used.
	unload(ctx context.Context, name string) error
}

// systemModuleLoader runs modprobe(8) and reads /proc/modules. modprobe
// rather than init_module(2) is used so that dependencies, aliases,
// blacklists, soft dependencies and the options in /etc/modprobe.d apply as
// they do at boot.
type systemModuleLoader struct {
	run         commandRunner
	procModules string
	timeout     time.Duration
}

func (l systemModuleLoader) loaded() (map[string]loadedModule, error) {
	data, err := os.ReadFile(l.procModules)
	if err != nil {
		return nil, fmt.Errorf("reading loaded modules: %w", err)
	}
	return parseProcModules(string(data))
}

func (l systemModuleLoader) load(ctx context.Context, name string, params []string) error {
	return runSystemCommand(ctx, l.run, l.timeout, modprobeOutputLimit, append([]string{"modprobe", "--", name}, params...)...)
}

func (l systemModuleLoader) unload(ctx context.Context, name string) error {
	return runSystemCommand(ctx, l.run, l.timeout, modprobeOutputLimit, "modprobe", "-r", "--", name)
}

// kernelModuleConfig is the provider-level configuration of
// sysutils_kernel_module. The zero value, or a nil pointer, selects the real
// modprobe and the directories in /etc.
type kernelModuleConfig struct {
	modulesLoadDir string
	modprobeDir    string
	loader         moduleLoader
}

func (c *kernelModuleConfig) loadDir() string {
	if c == nil || c.modulesLoadDir == "" {
		return defaultModulesLoadDir
	}
	return c.modulesLoadDir
}

func (c *kernelModuleConfig) optionsDir() string {
	if c == nil || c.modprobeDir == "" {
		return defaultModprobeDir
	}
	return c.modprobeDir
}

func (c *kernelModuleConfig) modules() moduleLoader {
	if c == nil || c.loader == nil {
		return systemModuleLoader{run: runCommand, procModules: defaultProcModules, timeout: modprobeTimeout}
	}
	return c.loader
}

// moduleFiles are the paths of the configuration files of one module.
type moduleFiles struct {
	// load lists the module, so that it is loaded at boot.
	load string
	// options holds the module's "options" line.
	options string
}

func (c *kernelModuleConfig) files(name string) moduleFiles {
	return moduleFiles{
		load:    filepath.Join(c.loadDir(), name+".conf"),
		options: filepath.Join(c.optionsDir(), name+".conf"),
	}
}

// renderModulesLoad returns the modules-load.d file that loads name.
func renderModulesLoad(name string) []byte {
	return []byte(moduleConfHeader + "\n" + name + "\n")
}

// renderModprobeOptions returns the modprobe.d file that sets params for
// name.
func renderModprobeOptions(name string, params map[string]string) []byte {
	return []byte(moduleConfHeader + "\n" + strings.Join(append([]string{"options", name}, moduleParamArgs(params)...), " ") + "\n")
}

// parseModprobeOptions returns the parameters that the "options" lines of a
// modprobe.d file set for name, and whether there are any such lines. A
// parameter without "=" has an empty value; later lines override earlier
// ones, as in modprobe. Lines continued with a backslash are joined.
func parseModprobeOptions(data []byte, name string) (map[string]string, bool) {
	params := map[string]string{}
	found := false
	text := strings.ReplaceAll(string(data), "\\\n", " ")
	for _, l := range strings.Split(text, "\n") {
		f := strings.Fields(l)
		if len(f) < 2 || f[0] != "options" || canonicalModuleName(f[1]) != canonicalModuleName(name) {
			continue
		}
		found = true
		for _, a := range f[2:] {
			k, v, _ := strings.Cut(a, "=")
			params[k] = v
		}
	}
	return params, found
}

// readModuleConf reads a managed configuration file. A missing file reads
// as nil data with a nil snapshot.
func readModuleConf(p string) ([]byte, *fileSnapshot, error) {
	data, snap, err := readRegularFileNoFollow(p, maxModuleConfSize)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	return data, snap, err
}

// moduleConfState is the content of a managed configuration file: nil if
// the file does not exist.
type moduleConfState []byte

// writeModuleConf makes the file at p contain want, or removes it if want is
// nil. It reports whether the file changed. An existing file is replaced
// atomically, keeping its mode, ownership and extended attributes.
func writeModuleConf(p string, want moduleConfState) (bool, error) {
	unlock := lockFileForEdit(p)
	defer unlock()
	cur, snap, err := readModuleConf(p)
	if err != nil {
		return false, err
	}
	switch {
	case want == nil && snap == nil:
		return false, nil
	case want == nil:
		if err := checkUnchanged(p, snap); err != nil {
			return false, err
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
		syncDir(filepath.Dir(p))
		return true, nil
	case snap != nil && string(cur) == string(want):
		return false, nil
	}
	if snap == nil {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return false, err
		}
	}
	return true, replaceFileAtomic(p, want, snap, moduleConfMode)
}
