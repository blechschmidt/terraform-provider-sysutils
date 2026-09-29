package provider

// OS package management behind sysutils_package. Each supported package
// manager is a packageManager backend that runs the manager's own commands
// through a commandRunner, always with an argument vector and never through
// a shell. Unit tests substitute either a fake commandRunner, to check the
// commands a backend runs and how it parses their output, or a fake
// packageManager, to test the resource without touching the host.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// packageQueryTimeout bounds each read-only query of the package
	// database or index.
	packageQueryTimeout = 2 * time.Minute
	// packageChangeTimeout bounds each command that installs, upgrades or
	// removes packages or refreshes the package index. Large dependency
	// trees on slow mirrors can take a while, but must not hang the apply
	// forever.
	packageChangeTimeout = 30 * time.Minute
	// packageOutputLimit caps how much output is kept per stream. Only
	// short query results and the tail of error messages are ever used.
	packageOutputLimit = 256 << 10
	// packageErrorLines is how many trailing lines of a failed command's
	// output are quoted in the error.
	packageErrorLines = 20
	// aptLockTimeout is how many seconds apt waits for the dpkg lock held by
	// another process, such as unattended-upgrades, before failing.
	aptLockTimeout = "300"
	// apkLockTimeout is the same for apk's database lock.
	apkLockTimeout = "300"
	// defaultPackagePath is the PATH of package manager commands if the
	// provider has none. dpkg refuses to run without the sbin directories.
	defaultPackagePath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)

// Package manager kinds. packageManagerAuto selects the first one found.
const (
	packageManagerAuto = "auto"
	packageManagerApt  = "apt"
	packageManagerDnf  = "dnf"
	packageManagerYum  = "yum"
	packageManagerApk  = "apk"
)

// Values of the state attribute.
const (
	packageStatePresent = "present"
	packageStateAbsent  = "absent"
	packageStateLatest  = "latest"
)

// packageManagerKinds lists the kinds in the order auto-detection tries
// them. dnf comes before yum because on current RPM distributions yum is a
// compatibility alias of dnf.
var packageManagerKinds = []string{packageManagerApt, packageManagerDnf, packageManagerYum, packageManagerApk}

// packageManagerTools lists the executables each kind needs.
var packageManagerTools = map[string][]string{
	packageManagerApt: {"apt-get", "apt-cache", "dpkg-query"},
	packageManagerDnf: {"dnf", "rpm"},
	packageManagerYum: {"yum", "rpm"},
	packageManagerApk: {"apk"},
}

var (
	// packageNamePattern is the plan-time check of package names, common to
	// all managers. It excludes white space, shell and glob metacharacters,
	// "/" (which dnf would take for a file path), "(" (dnf provides), "="
	// and "<>" (apk and apt version constraints) and ":" (dpkg
	// architecture qualifiers). A name never starts with "-", so it can
	// never be taken for an option.
	packageNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)
	// debPackageNamePattern is Debian policy's rule for package names:
	// lowercase, at least two characters.
	debPackageNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.+-]{1,127}$`)
	// packageVersionPattern is the plan-time check of versions, common to
	// all managers.
	packageVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.+~^:_-]{0,127}$`)
	// debVersionPattern matches Debian versions: [epoch:]upstream[-revision],
	// with an upstream version that starts with a digit.
	debVersionPattern = regexp.MustCompile(`^([0-9]+:)?[0-9][A-Za-z0-9.+~:-]*$`)
	// rpmVersionPattern matches RPM versions in the [epoch:]version-release
	// form that rpm -q reports. The release is required: "dnf install
	// name-1.2" would accept any release, and the installed version would
	// then never equal the configured one.
	rpmVersionPattern = regexp.MustCompile(`^([0-9]+:)?[A-Za-z0-9._+~^]+-[A-Za-z0-9._+~^]+$`)
	// apkVersionPattern matches Alpine versions such as "2.2.1-r0".
	apkVersionPattern = regexp.MustCompile(`^[0-9][A-Za-z0-9._]*-r[0-9]+$`)
)

// validatePackageName reports why name is not an acceptable package name
// for any manager.
func validatePackageName(name string) error {
	if !packageNamePattern.MatchString(name) {
		return fmt.Errorf("package name %q must be 1 to 128 letters, digits, \".\", \"_\", \"+\" or \"-\", starting with a letter or digit", name)
	}
	return nil
}

// validatePackageVersion reports why version is not an acceptable version
// for any manager.
func validatePackageVersion(version string) error {
	if !packageVersionPattern.MatchString(version) {
		return fmt.Errorf("version %q must be 1 to 128 letters, digits, \".\", \"+\", \"~\", \"^\", \":\", \"_\" or \"-\", starting with a letter or digit", version)
	}
	return nil
}

// validatePackageNameFor applies the stricter rules of a specific manager.
// Besides the manager's own naming rules, these exclude names that the
// manager's command line takes for something other than a package name:
// apt-get takes a trailing "-" as an instruction to remove the package
// ("apt-get install nginx-" removes nginx), and dnf, yum and apk install
// an argument ending in ".rpm" or ".apk" from a local file of that name,
// relative to the provider's working directory, even after "--".
func validatePackageNameFor(kind, name string) error {
	if err := validatePackageName(name); err != nil {
		return err
	}
	switch kind {
	case packageManagerApt:
		if !debPackageNamePattern.MatchString(name) {
			return fmt.Errorf("package name %q is not a valid Debian package name: at least 2 lowercase letters, digits, \".\", \"+\" or \"-\", starting with a letter or digit", name)
		}
		if strings.HasSuffix(name, "-") {
			return fmt.Errorf("package name %q must not end with \"-\", which apt-get takes as an instruction to remove the package", name)
		}
	case packageManagerDnf, packageManagerYum:
		if hasSuffixFold(name, ".rpm") {
			return fmt.Errorf("package name %q must not end with \".rpm\", which %s takes for a local package file", name, kind)
		}
	case packageManagerApk:
		if hasSuffixFold(name, ".apk") {
			return fmt.Errorf("package name %q must not end with \".apk\", which apk takes for a local package file", name)
		}
	}
	return nil
}

// hasSuffixFold is strings.HasSuffix, ignoring ASCII case.
func hasSuffixFold(s, suffix string) bool {
	return len(s) >= len(suffix) && strings.EqualFold(s[len(s)-len(suffix):], suffix)
}

// validatePackageVersionFor applies the stricter rules of a specific
// manager.
func validatePackageVersionFor(kind, version string) error {
	if err := validatePackageVersion(version); err != nil {
		return err
	}
	switch kind {
	case packageManagerApt:
		if !debVersionPattern.MatchString(version) {
			return fmt.Errorf("version %q is not a valid Debian version: [epoch:]upstream[-revision], with an upstream version starting with a digit, as dpkg-query reports it", version)
		}
	case packageManagerDnf, packageManagerYum:
		if !rpmVersionPattern.MatchString(version) {
			return fmt.Errorf("version %q is not a valid RPM version: [epoch:]version-release, such as \"2.2.1-1.fc42\", as rpm -q reports it; the release is required", version)
		}
		// The version ends the name-version argument of dnf and yum.
		if hasSuffixFold(version, ".rpm") {
			return fmt.Errorf("version %q must not end with \".rpm\", which %s takes for a local package file", version, kind)
		}
	case packageManagerApk:
		if !apkVersionPattern.MatchString(version) {
			return fmt.Errorf("version %q is not a valid Alpine package version: version-rN, such as \"2.2.1-r0\", as apk info reports it", version)
		}
	}
	return nil
}

// normalizePackageVersion drops an explicit zero epoch, which dpkg and rpm
// both treat as no epoch, so that "0:1.2-3" and "1.2-3" compare equal.
func normalizePackageVersion(v string) string {
	if rest, ok := strings.CutPrefix(v, "0:"); ok {
		return rest
	}
	return v
}

// samePackageVersion reports whether two version strings denote the same
// version.
func samePackageVersion(a, b string) bool {
	return normalizePackageVersion(a) == normalizePackageVersion(b)
}

// packageInfo is what the package database says about a package.
type packageInfo struct {
	Installed bool
	// Version is the installed version, in the manager's own format. Empty
	// if the package is not installed.
	Version string
}

// packageManager is a package manager backend. Implementations are not
// safe for concurrent use; the resources make all calls with the
// package-manager lock (lockPackageManager) held.
type packageManager interface {
	// Kind returns the manager kind, such as "apt".
	Kind() string
	// Query reads the package database. It never changes the host.
	Query(ctx context.Context, name string) (packageInfo, error)
	// UpToDate reports whether the installed package is the newest version
	// available in the local package index. It never refreshes the index.
	UpToDate(ctx context.Context, name string) (bool, error)
	// Install installs the package; with a non-empty version exactly that
	// version, upgrading or downgrading an installed one.
	Install(ctx context.Context, name, version string) error
	// Upgrade installs the newest available version of the package, whether
	// or not it is installed.
	Upgrade(ctx context.Context, name string) error
	// Remove removes the package. It is only called for installed
	// packages: some managers fail for packages they know nothing about.
	Remove(ctx context.Context, name string) error
	// UpdateCache refreshes the package index from the repositories.
	UpdateCache(ctx context.Context) error
}

// packageConfig is the provider-level configuration of sysutils_package.
// The zero value, or a nil pointer, selects the real package managers.
type packageConfig struct {
	// run runs the backends' commands; nil selects runCommand.
	run commandRunner
	// lookPath finds executables during detection; nil selects
	// exec.LookPath.
	lookPath func(string) (string, error)
	// manager, if set, replaces backend construction and detection
	// entirely: it is called with the configured kind ("auto" included)
	// and returns the backend to use. Unit tests use it to install a fake.
	manager func(kind string) (packageManager, error)

	// cacheUpdated records the kinds whose index was already refreshed in
	// this provider process, so that many resources with update_cache
	// refresh it only once per apply. It is guarded by the package-manager
	// lock (lockPackageManager).
	cacheUpdated map[string]bool
}

// defaultPackageConfig is used when the provider has no override. It is a
// single shared value so that all resources share its cacheUpdated.
var defaultPackageConfig = &packageConfig{}

func (c *packageConfig) orDefault() *packageConfig {
	if c == nil {
		return defaultPackageConfig
	}
	return c
}

func (c *packageConfig) runner() commandRunner {
	if c.run == nil {
		return runCommand
	}
	return c.run
}

// resolve returns the backend for kind, which is one of the manager kinds
// or "auto".
func (c *packageConfig) resolve(kind string) (packageManager, error) {
	if c.manager != nil {
		return c.manager(kind)
	}
	lookPath := c.lookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	missing := func(k string) []string {
		var m []string
		for _, tool := range packageManagerTools[k] {
			if _, err := lookPath(tool); err != nil {
				m = append(m, tool)
			}
		}
		return m
	}
	if kind == packageManagerAuto || kind == "" {
		for _, k := range packageManagerKinds {
			if len(missing(k)) == 0 {
				return newPackageBackend(k, c.runner()), nil
			}
		}
		return nil, errors.New("no supported package manager found: none of apt (apt-get, apt-cache, dpkg-query), dnf (dnf, rpm), yum (yum, rpm) or apk (apk) is installed")
	}
	if _, ok := packageManagerTools[kind]; !ok {
		return nil, fmt.Errorf("unsupported package manager %q", kind)
	}
	if m := missing(kind); len(m) > 0 {
		return nil, fmt.Errorf("package manager %s is not available: %s not found in PATH", kind, strings.Join(m, ", "))
	}
	return newPackageBackend(kind, c.runner()), nil
}

// updateCacheOnce refreshes the index of m unless that was already done in
// this provider process. The caller holds the package-manager lock.
func (c *packageConfig) updateCacheOnce(ctx context.Context, m packageManager) error {
	if c.cacheUpdated[m.Kind()] {
		return nil
	}
	if err := m.UpdateCache(ctx); err != nil {
		return err
	}
	if c.cacheUpdated == nil {
		c.cacheUpdated = map[string]bool{}
	}
	c.cacheUpdated[m.Kind()] = true
	return nil
}

func newPackageBackend(kind string, run commandRunner) packageManager {
	base := packageCommands{run: run}
	switch kind {
	case packageManagerApt:
		return aptBackend{base}
	case packageManagerDnf, packageManagerYum:
		return rpmBackend{packageCommands: base, tool: kind}
	case packageManagerApk:
		return apkBackend{base}
	}
	panic("unknown package manager " + kind)
}

// packageCommands runs package manager commands.
type packageCommands struct {
	run commandRunner
}

// packageEnvInherited lists the only variables of the provider's
// environment that package manager commands inherit: the search path, and
// proxy settings, which hosts without direct internet access need to reach
// their repositories. Everything else is dropped, because package managers
// read much of their configuration from the environment: APT_CONFIG
// replaces apt's configuration file, DPKG_ADMINDIR and DPKG_ROOT point dpkg
// and dpkg-query at another database, DPKG_FRONTEND_LOCKED makes dpkg skip
// its frontend lock, DNF_VAR_* and YUM0-9 rewrite repository URLs, and
// DEBCONF_* and UCF_* change how configuration questions are answered.
var packageEnvInherited = []string{
	"PATH",
	"http_proxy", "https_proxy", "ftp_proxy", "no_proxy", "all_proxy",
	"HTTP_PROXY", "HTTPS_PROXY", "FTP_PROXY", "NO_PROXY", "ALL_PROXY",
}

// packageEnv is the environment of package manager commands: the few
// inherited variables of packageEnvInherited, the C locale so that output
// can be parsed, and every interactive prompt that the Debian tool chain
// knows of disabled. needrestart, run by apt on Ubuntu, only lists services
// to restart rather than asking or restarting them.
func packageEnv() []string {
	var env []string
	for _, k := range packageEnvInherited {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	if os.Getenv("PATH") == "" {
		env = append(env, "PATH="+defaultPackagePath)
	}
	return append(env,
		"HOME="+packageHome(),
		"LC_ALL=C",
		"LANG=C",
		"DEBIAN_FRONTEND=noninteractive",
		"DEBCONF_NONINTERACTIVE_SEEN=true",
		"APT_LISTCHANGES_FRONTEND=none",
		"APT_LISTBUGS_FRONTEND=none",
		"UCF_FORCE_CONFFOLD=1",
		"NEEDRESTART_MODE=l",
	)
}

// packageHome returns the home directory of the user the provider runs as,
// from the user database rather than the inherited HOME: rpm reads macros
// from ~/.rpmmacros, which can run shell commands, so with a HOME kept by
// "sudo -E" the invoking user would control commands run as root.
func packageHome() string {
	if u, err := user.LookupId(strconv.Itoa(os.Geteuid())); err == nil && filepath.IsAbs(u.HomeDir) {
		return u.HomeDir
	}
	return "/"
}

// exec runs argv and returns its result. A command that could not be
// started or timed out is an error; a non-zero exit status is not.
func (p packageCommands) exec(ctx context.Context, timeout time.Duration, argv ...string) (*execResult, error) {
	res, err := p.run(ctx, execSpec{
		Argv:           argv,
		Env:            packageEnv(),
		Timeout:        timeout,
		MaxOutputBytes: packageOutputLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
	}
	if res.TimedOut {
		return nil, fmt.Errorf("%s: timed out after %s", strings.Join(argv, " "), timeout)
	}
	return res, nil
}

// change runs a command that changes the host and fails on a non-zero exit
// status.
func (p packageCommands) change(ctx context.Context, argv ...string) error {
	res, err := p.exec(ctx, packageChangeTimeout, argv...)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return packageCommandError(argv, res)
	}
	return nil
}

// packageCommandError quotes the end of a failed command's output, where
// package managers print the reason for a failure.
func packageCommandError(argv []string, res *execResult) error {
	msg := tailLines(res.Stderr.String(), packageErrorLines)
	if msg == "" {
		msg = tailLines(res.Stdout.String(), packageErrorLines)
	}
	if msg == "" {
		return fmt.Errorf("%s: exit status %d", strings.Join(argv, " "), res.ExitCode)
	}
	return fmt.Errorf("%s: exit status %d:\n%s", strings.Join(argv, " "), res.ExitCode, msg)
}

// tailLines returns the last n non-empty lines of s.
func tailLines(s string, n int) string {
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimRight(l, " \t\r"); strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// aptBackend manages packages with apt-get, and queries them with
// dpkg-query and apt-cache.
type aptBackend struct{ packageCommands }

func (aptBackend) Kind() string { return packageManagerApt }

// aptGetArgs are passed to every changing apt-get command. Configuration
// files changed locally are kept and new defaults installed where there are
// no local changes, rather than asking. A dpkg lock held by another process
// is waited for instead of failing at once.
var aptGetArgs = []string{
	"-y", "-q",
	"-o", "Dpkg::Options::=--force-confdef",
	"-o", "Dpkg::Options::=--force-confold",
	"-o", "DPkg::Lock::Timeout=" + aptLockTimeout,
}

func (a aptBackend) aptGet(ctx context.Context, cmd string, extra ...string) error {
	argv := append([]string{"apt-get", cmd}, aptGetArgs...)
	return a.change(ctx, append(argv, extra...)...)
}

func (a aptBackend) Query(ctx context.Context, name string) (packageInfo, error) {
	argv := []string{"dpkg-query", "--show", "--showformat=${Package}\\t${db:Status-Abbrev}\\t${Version}\\n", "--", name}
	res, err := a.exec(ctx, packageQueryTimeout, argv...)
	if err != nil {
		return packageInfo{}, err
	}
	info := parseDpkgQuery(res.Stdout.String(), name)
	if res.ExitCode != 0 && !info.Installed {
		// dpkg-query exits with 1 for packages it knows nothing about.
		if !strings.Contains(res.Stderr.String(), "no packages found matching") {
			return packageInfo{}, packageCommandError(argv, res)
		}
	}
	return info, nil
}

// parseDpkgQuery parses "package\tstatus\tversion" lines. A package counts
// as installed if its status is installed, or installed with triggers
// awaited or pending. Half-installed or half-configured packages do not, so
// that the next apply repairs them. With several architectures installed,
// the first installed one is reported.
func parseDpkgQuery(out, name string) packageInfo {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 3 || fields[0] != name {
			continue
		}
		status := fields[1]
		if len(status) < 2 || !strings.ContainsRune("iWt", rune(status[1])) {
			continue
		}
		return packageInfo{Installed: true, Version: strings.TrimSpace(fields[2])}
	}
	return packageInfo{}
}

func (a aptBackend) UpToDate(ctx context.Context, name string) (bool, error) {
	installed, candidate, err := a.policy(ctx, name)
	if err != nil {
		return false, err
	}
	// Without a candidate, the package is in no repository apt knows of,
	// so there is nothing to upgrade to.
	return candidate == "" || candidate == installed, nil
}

// policy runs "apt-cache policy" for name and returns its installed and
// candidate versions. It fails unless apt resolves name to exactly the
// package of that name, which must therefore be in the package index.
//
// apt takes a name that no package has for a pattern: as a regular
// expression if it contains "." or "+" ("lib.+" selects every package
// whose name contains "lib"), and apt-get takes a trailing "+" or "-" as an
// instruction to install or remove the package without it. "--" does not
// turn this off, so a name must never reach apt-get unless it is in the
// index verbatim. apt-cache resolves names like apt-get, so the package
// headers of its output show what apt-get would act on.
func (a aptBackend) policy(ctx context.Context, name string) (installed, candidate string, err error) {
	argv := []string{"apt-cache", "policy", "--", name}
	res, err := a.exec(ctx, packageQueryTimeout, argv...)
	if err != nil {
		return "", "", err
	}
	if res.ExitCode != 0 {
		return "", "", packageCommandError(argv, res)
	}
	out := res.Stdout.String()
	headers := aptPolicyHeaders(out)
	if len(headers) == 0 {
		return "", "", fmt.Errorf("package %s is not in the package index of apt; check the name, or set update_cache = true if the index is missing or out of date", name)
	}
	for _, h := range headers {
		if !isAptPolicyHeaderFor(h, name) {
			others := make([]string, 0, len(headers))
			for _, h := range headers {
				others = append(others, strings.TrimSuffix(h, ":"))
			}
			if len(others) > 5 {
				others = append(others[:5], "...")
			}
			return "", "", fmt.Errorf("package %s is not in the package index of apt, which would take the name for a pattern and act on other packages (%s) instead; use the exact name of a package", name, strings.Join(others, ", "))
		}
	}
	installed, candidate = parseAptPolicy(out)
	return installed, candidate, nil
}

// aptPolicyHeaders returns the package headers of "apt-cache policy"
// output: the unindented lines, such as "hello:" or "hello:i386:".
func aptPolicyHeaders(out string) []string {
	var headers []string
	for _, line := range strings.Split(out, "\n") {
		if line != "" && line[0] != ' ' && line[0] != '\t' {
			headers = append(headers, strings.TrimSpace(line))
		}
	}
	return headers
}

// aptArchPattern matches Debian architecture names.
var aptArchPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// isAptPolicyHeaderFor reports whether header is that of the package name,
// for the native ("name:") or another architecture ("name:arch:").
func isAptPolicyHeaderFor(header, name string) bool {
	rest, ok := strings.CutPrefix(header, name+":")
	if !ok {
		return false
	}
	if rest == "" {
		return true
	}
	arch, ok := strings.CutSuffix(rest, ":")
	return ok && aptArchPattern.MatchString(arch)
}

// parseAptPolicy returns the installed and candidate versions from
// "apt-cache policy" output for a single package; "(none)" is returned as
// "". Only the first package block counts: with several architectures
// installed, that of the native one.
func parseAptPolicy(out string) (installed, candidate string) {
	headers := 0
	for _, line := range strings.Split(out, "\n") {
		if line != "" && line[0] != ' ' && line[0] != '\t' {
			if headers++; headers > 1 {
				break
			}
			continue
		}
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "(none)" {
			value = ""
		}
		switch key {
		case "Installed":
			installed = value
		case "Candidate":
			candidate = value
		}
	}
	return installed, candidate
}

func (a aptBackend) Install(ctx context.Context, name, version string) error {
	if _, _, err := a.policy(ctx, name); err != nil {
		return err
	}
	if version == "" {
		return a.aptGet(ctx, "install", "--", name)
	}
	return a.aptGet(ctx, "install", "--allow-downgrades", "--", name+"="+version)
}

// Upgrade installs the candidate version; "apt-get install" upgrades a
// package that is already installed.
func (a aptBackend) Upgrade(ctx context.Context, name string) error {
	if _, _, err := a.policy(ctx, name); err != nil {
		return err
	}
	return a.aptGet(ctx, "install", "--", name)
}

func (a aptBackend) Remove(ctx context.Context, name string) error {
	if _, _, err := a.policy(ctx, name); err != nil {
		return err
	}
	return a.aptGet(ctx, "remove", "--", name)
}

func (a aptBackend) UpdateCache(ctx context.Context) error {
	return a.change(ctx, "apt-get", "update", "-q", "-o", "DPkg::Lock::Timeout="+aptLockTimeout)
}

// rpmBackend manages packages with dnf or yum, and queries them with rpm.
// Package arguments always follow "--", which dnf4, dnf5 and yum accept, so
// that they can never be taken for options.
type rpmBackend struct {
	packageCommands
	tool string // "dnf" or "yum"
}

func (r rpmBackend) Kind() string { return r.tool }

// rpmQueryFormat prints the name and [epoch:]version-release of each
// matching package.
const rpmQueryFormat = `%{NAME}\t%|EPOCH?{%{EPOCH}:}:{}|%{VERSION}-%{RELEASE}\n`

func (r rpmBackend) Query(ctx context.Context, name string) (packageInfo, error) {
	argv := []string{"rpm", "--query", "--queryformat", rpmQueryFormat, "--", name}
	res, err := r.exec(ctx, packageQueryTimeout, argv...)
	if err != nil {
		return packageInfo{}, err
	}
	info := parseRPMQuery(res.Stdout.String(), name)
	if res.ExitCode != 0 && !info.Installed {
		// rpm exits with 1 and says so on stdout for a package that is
		// not installed.
		if !strings.Contains(res.Stdout.String(), "is not installed") {
			return packageInfo{}, packageCommandError(argv, res)
		}
	}
	return info, nil
}

// parseRPMQuery parses "name\tversion" lines. rpm -q also matches
// name-version patterns, so lines for other packages are ignored. With
// several architectures installed, the first one is reported.
func parseRPMQuery(out, name string) packageInfo {
	for _, line := range strings.Split(out, "\n") {
		n, v, ok := strings.Cut(line, "\t")
		if ok && n == name && v != "" {
			return packageInfo{Installed: true, Version: strings.TrimSpace(v)}
		}
	}
	return packageInfo{}
}

// UpToDate uses "check-update", which exits with 100 if updates are
// available and 0 if not.
func (r rpmBackend) UpToDate(ctx context.Context, name string) (bool, error) {
	argv := []string{r.tool, "check-update", "-q", "--", name}
	res, err := r.exec(ctx, packageQueryTimeout, argv...)
	if err != nil {
		return false, err
	}
	switch res.ExitCode {
	case 0:
		return true, nil
	case 100:
		return false, nil
	}
	return false, packageCommandError(argv, res)
}

func (r rpmBackend) Install(ctx context.Context, name, version string) error {
	spec := name
	if version != "" {
		spec = name + "-" + version
	}
	if err := r.change(ctx, r.tool, "install", "-y", "-q", "--", spec); err != nil {
		return err
	}
	if version == "" {
		return nil
	}
	// "install" does nothing if a newer version is installed; that takes
	// "downgrade".
	info, err := r.Query(ctx, name)
	if err != nil {
		return err
	}
	if info.Installed && samePackageVersion(info.Version, version) {
		return nil
	}
	return r.change(ctx, r.tool, "downgrade", "-y", "-q", "--", spec)
}

func (r rpmBackend) Upgrade(ctx context.Context, name string) error {
	info, err := r.Query(ctx, name)
	if err != nil {
		return err
	}
	if !info.Installed {
		return r.change(ctx, r.tool, "install", "-y", "-q", "--", name)
	}
	return r.change(ctx, r.tool, "upgrade", "-y", "-q", "--", name)
}

func (r rpmBackend) Remove(ctx context.Context, name string) error {
	return r.change(ctx, r.tool, "remove", "-y", "-q", "--", name)
}

func (r rpmBackend) UpdateCache(ctx context.Context) error {
	return r.change(ctx, r.tool, "makecache", "-q")
}

// apkBackend manages packages with Alpine's apk. Package arguments always
// follow "--". Changes wait for apk's database lock, held by another apk
// process, rather than failing at once.
type apkBackend struct{ packageCommands }

func (apkBackend) Kind() string { return packageManagerApk }

func (a apkBackend) Query(ctx context.Context, name string) (packageInfo, error) {
	argv := []string{"apk", "info", "--installed", "--verbose", "--", name}
	res, err := a.exec(ctx, packageQueryTimeout, argv...)
	if err != nil {
		return packageInfo{}, err
	}
	info := parseApkInfo(res.Stdout.String(), name)
	// apk info -e exits with 1, silently, if the package is not
	// installed.
	if res.ExitCode != 0 && !info.Installed && strings.TrimSpace(res.Stderr.String()) != "" {
		return packageInfo{}, packageCommandError(argv, res)
	}
	return info, nil
}

// parseApkInfo parses the "name-version" lines of "apk info -e -v".
func parseApkInfo(out, name string) packageInfo {
	for _, line := range strings.Split(out, "\n") {
		v, ok := strings.CutPrefix(strings.TrimSpace(line), name+"-")
		if ok && v != "" && v[0] >= '0' && v[0] <= '9' {
			return packageInfo{Installed: true, Version: v}
		}
	}
	return packageInfo{}
}

func (a apkBackend) UpToDate(ctx context.Context, name string) (bool, error) {
	info, err := a.Query(ctx, name)
	if err != nil || !info.Installed {
		return false, err
	}
	argv := []string{"apk", "version", "--", name}
	res, err := a.exec(ctx, packageQueryTimeout, argv...)
	if err != nil {
		return false, err
	}
	if res.ExitCode != 0 {
		return false, packageCommandError(argv, res)
	}
	return !apkVersionOutdated(res.Stdout.String(), name+"-"+info.Version), nil
}

// apkVersionOutdated reports whether "apk version" output lists pkgver
// ("name-version") with a newer version available.
func apkVersionOutdated(out, pkgver string) bool {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == pkgver {
			return fields[1] == "<"
		}
	}
	return false
}

// Install adds the package to the world file; with a version, as
// name=version, which also keeps "apk upgrade" from changing it.
func (a apkBackend) Install(ctx context.Context, name, version string) error {
	spec := name
	if version != "" {
		spec = name + "=" + version
	}
	return a.change(ctx, "apk", "add", "--quiet", "--no-progress", "--wait", apkLockTimeout, "--", spec)
}

func (a apkBackend) Upgrade(ctx context.Context, name string) error {
	return a.change(ctx, "apk", "add", "--quiet", "--no-progress", "--wait", apkLockTimeout, "--upgrade", "--", name)
}

func (a apkBackend) Remove(ctx context.Context, name string) error {
	return a.change(ctx, "apk", "del", "--quiet", "--no-progress", "--wait", apkLockTimeout, "--", name)
}

func (a apkBackend) UpdateCache(ctx context.Context) error {
	return a.change(ctx, "apk", "update", "--quiet", "--no-progress")
}
