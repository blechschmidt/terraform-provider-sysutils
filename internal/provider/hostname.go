package provider

// Hostname handling behind sysutils_hostname. A Linux host has three
// hostnames, as systemd names them:
//
//   - the static hostname, the first line of /etc/hostname, which the init
//     system gives the kernel at boot;
//   - the transient hostname, the kernel's (sethostname(2), uname -n), which
//     is lost on reboot;
//   - the pretty hostname, a free-form name in PRETTY_HOSTNAME= of
//     /etc/machine-info, which only systemd-hostnamed and its clients use.
//
// On the host root, when systemd booted the host, hostnamectl is installed
// and the provider runs in the UTS and mount namespaces of PID 1,
// hostnamectl sets the static and pretty hostnames, so that
// systemd-hostnamed sees the change as its own. Otherwise, and when
// hostnamectl fails, the files are written directly through the
// symlink-safe helpers of safefs.go and the kernel hostname is set with
// sethostname(2). Below root_dir only the files are written.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	// hostnameFilePath holds the static hostname.
	hostnameFilePath = "/etc/hostname"
	// machineInfoPath holds the pretty hostname; see machine-info(5).
	machineInfoPath = "/etc/machine-info"
	// prettyHostnameKey is the key of the pretty hostname in machine-info.
	prettyHostnameKey = "PRETTY_HOSTNAME"
	// hostnameLoopbackIP is the address Debian and its derivatives map the
	// hostname to in /etc/hosts when the host has no permanent address.
	hostnameLoopbackIP = "127.0.1.1"
	// maxKernelHostnameLen is the kernel's limit (HOST_NAME_MAX), which
	// hostnamectl also enforces for the static hostname.
	maxKernelHostnameLen = 64
	// maxPrettyHostnameLen bounds the pretty hostname.
	maxPrettyHostnameLen = 255
	// maxHostnameFileSize bounds how much of /etc/hostname and
	// /etc/machine-info is read.
	maxHostnameFileSize = 64 << 10
	// hostnameFileMode is the mode of an /etc/hostname or /etc/machine-info
	// created by the provider; an existing file keeps its own.
	hostnameFileMode fs.FileMode = 0o644
	// hostnamectlTimeout bounds one hostnamectl call.
	hostnamectlTimeout = 30 * time.Second
)

var hostnameLoopbackAddr = netip.MustParseAddr(hostnameLoopbackIP)

// hostnameConfig is the provider-level configuration of sysutils_hostname.
// The zero value, or a nil pointer, selects the real hostnamectl, /run,
// /proc and kernel.
type hostnameConfig struct {
	// run runs hostnamectl; nil selects runCommand.
	run commandRunner
	// lookPath finds hostnamectl; nil selects exec.LookPath.
	lookPath func(string) (string, error)
	// runDir replaces /run when checking whether systemd booted the host.
	runDir string
	// procDir replaces /proc when comparing namespaces with PID 1.
	procDir string
	// getKernel and setKernel replace reading and setting the kernel
	// hostname.
	getKernel func() (string, error)
	setKernel func(string) error
	// rootIsHost makes a root_dir count as the running host, so that unit
	// tests can exercise the kernel hostname and hostnamectl with fakes
	// against files in a temporary directory.
	rootIsHost bool
}

// managesKernel reports whether the kernel hostname is managed for root:
// only on the host root, since below root_dir the files describe another
// system.
func (c *hostnameConfig) managesKernel(root *fsRoot) bool {
	return root.isHost() || c != nil && c.rootIsHost
}

func (c *hostnameConfig) runner() commandRunner {
	if c == nil || c.run == nil {
		return runCommand
	}
	return c.run
}

// kernelHostname returns the kernel hostname of the provider's UTS
// namespace.
func (c *hostnameConfig) kernelHostname() (string, error) {
	if c != nil && c.getKernel != nil {
		return c.getKernel()
	}
	return os.Hostname()
}

// setKernelHostname sets the kernel hostname of the provider's UTS
// namespace.
func (c *hostnameConfig) setKernelHostname(name string) error {
	if c != nil && c.setKernel != nil {
		return c.setKernel(name)
	}
	if err := syscall.Sethostname([]byte(name)); err != nil {
		if errors.Is(err, syscall.EPERM) {
			return fmt.Errorf("sethostname(%q): %w; setting the kernel hostname needs root (CAP_SYS_ADMIN)", name, err)
		}
		return fmt.Errorf("sethostname(%q): %w", name, err)
	}
	return nil
}

// useHostnamectl reports whether the static and pretty hostnames are set
// with hostnamectl: only on the host root, when systemd booted the host,
// hostnamectl is installed, and the provider shares the UTS and mount
// namespaces of PID 1. systemd-hostnamed acts in those namespaces; from a
// container, or from a test's own UTS namespace, it would change the
// hostname of another system than the one the provider looks at.
func (c *hostnameConfig) useHostnamectl(root *fsRoot) bool {
	if !c.managesKernel(root) {
		return false
	}
	lookPath, runDir, procDir := exec.LookPath, defaultRunDir, defaultProcDir
	if c != nil && c.lookPath != nil {
		lookPath = c.lookPath
	}
	if c != nil && c.runDir != "" {
		runDir = c.runDir
	}
	if c != nil && c.procDir != "" {
		procDir = c.procDir
	}
	if !systemdBootedIn(runDir) || !sharesNamespacesWithInit(procDir, "uts", "mnt") {
		return false
	}
	_, err := lookPath("hostnamectl")
	return err == nil
}

// sharesNamespacesWithInit reports whether the process is in the same
// namespaces of the given kinds as PID 1. Where that cannot be told, for
// example because /proc/1/ns is not readable, it assumes it is, as is the
// case on almost every host.
func sharesNamespacesWithInit(procDir string, kinds ...string) bool {
	for _, kind := range kinds {
		self, err1 := os.Readlink(filepath.Join(procDir, "self", "ns", kind))
		init, err2 := os.Readlink(filepath.Join(procDir, "1", "ns", kind))
		if err1 == nil && err2 == nil && self != init {
			return false
		}
	}
	return true
}

// hostnamectl runs "hostnamectl <flag> set-hostname -- <value>", where flag
// is --static or --pretty. set-hostname is the command's name before
// systemd 249 and an alias since.
func (c *hostnameConfig) hostnamectl(ctx context.Context, flag, value string) error {
	argv := []string{"hostnamectl", "--no-ask-password", flag, "set-hostname", "--", value}
	res, err := c.runner()(ctx, execSpec{
		Argv:           argv,
		Env:            append(os.Environ(), "SYSTEMD_PAGER=", "SYSTEMD_COLORS=0", "LC_ALL=C"),
		Timeout:        hostnamectlTimeout,
		MaxOutputBytes: serviceOutputLimit,
	})
	display := fmt.Sprintf("hostnamectl %s set-hostname %q", flag, value)
	if err != nil {
		return fmt.Errorf("%s: %w", display, err)
	}
	if res.TimedOut {
		return fmt.Errorf("%s: timed out after %s", display, hostnamectlTimeout)
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(res.Stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(res.Stdout.String())
		}
		return fmt.Errorf("%s: exit status %d: %s", display, res.ExitCode, msg)
	}
	return nil
}

// validatePrettyHostname reports why s cannot be the pretty hostname: it
// must be non-empty valid UTF-8 without control characters or surrounding
// white space, as systemd-hostnamed requires, and at most 255 bytes long.
func validatePrettyHostname(s string) error {
	switch {
	case s == "":
		return errors.New("pretty hostname must not be empty; leave it unset to leave the pretty hostname alone")
	case len(s) > maxPrettyHostnameLen:
		return fmt.Errorf("pretty hostname must be at most %d bytes long", maxPrettyHostnameLen)
	case !utf8.ValidString(s):
		return errors.New("pretty hostname must be valid UTF-8")
	case strings.TrimSpace(s) != s:
		return errors.New("pretty hostname must not start or end with white space")
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0 {
			return fmt.Errorf("pretty hostname must not contain control characters such as %q", r)
		}
	}
	return nil
}

// validateKernelHostname reports why name, a valid hostname, cannot be the
// kernel hostname.
func validateKernelHostname(name string) error {
	if len(name) > maxKernelHostnameLen {
		return fmt.Errorf("hostname %q is %d characters long, but the kernel allows at most %d; "+
			"use a shorter name, such as the first label, as the hostname and map the full name in /etc/hosts",
			name, len(name), maxKernelHostnameLen)
	}
	return nil
}

// staticHostname returns the static hostname in the content of
// /etc/hostname: the first line that is neither blank nor a comment,
// without surrounding white space, as systemd reads it.
func staticHostname(content string) string {
	for line := range strings.SplitSeq(content, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			return line
		}
	}
	return ""
}

// readOptionalFile returns the content of the regular file at the host
// path p, or nil if it does not exist. Symlinks are refused.
func readOptionalFile(p string) (*string, *fileSnapshot, error) {
	data, snap, err := readRegularFileNoFollow(p, maxHostnameFileSize)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	s := string(data)
	return &s, snap, nil
}

// setFileContent makes the file at the host path p hold content, keeping
// its mode, owner and other attributes, or removes it if content is nil.
// Nothing is written if it already holds content.
func setFileContent(p string, content *string) error {
	unlock, err := lockFileForEdit(p)
	if err != nil {
		return err
	}
	defer unlock()
	cur, snap, err := readOptionalFile(p)
	if err != nil {
		return err
	}
	switch {
	case content == nil && cur == nil:
		return nil
	case content == nil:
		return removeManagedFile(p, snap)
	case cur != nil && *cur == *content:
		return nil
	default:
		return writeManagedFile(p, []byte(*content), snap, hostnameFileMode)
	}
}

// Parsing and editing of /etc/machine-info, an environment-like file of
// KEY=VALUE lines (see machine-info(5)). Values are written the way
// systemd's write_env_file() writes them: bare if they hold no character a
// shell would treat specially, otherwise in double quotes with ", \, ` and
// $ escaped.

// envNeedsQuotes lists the characters that make systemd quote a value.
const envNeedsQuotes = "\"\\`$*?[]'()<>|&;! \t\n\r#~{}"

// quoteEnvValue renders v as the value of a machine-info assignment.
func quoteEnvValue(v string) string {
	if v != "" && !strings.ContainsAny(v, envNeedsQuotes) {
		return v
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(v); i++ {
		if strings.IndexByte("\"\\`$", v[i]) >= 0 {
			b.WriteByte('\\')
		}
		b.WriteByte(v[i])
	}
	b.WriteByte('"')
	return b.String()
}

// parseEnvValue returns the value of the raw right-hand side of an
// assignment: double-quoted with backslash escapes of ", \, ` and $,
// single-quoted without escapes, or bare with backslash escapes and
// surrounding white space removed.
func parseEnvValue(raw string) string {
	raw = strings.TrimSpace(raw)
	var b strings.Builder
	switch {
	case strings.HasPrefix(raw, `"`):
		for i := 1; i < len(raw); i++ {
			c := raw[i]
			switch {
			case c == '"':
				return b.String()
			case c == '\\' && i+1 < len(raw) && strings.IndexByte("\"\\`$", raw[i+1]) >= 0:
				i++
				b.WriteByte(raw[i])
			default:
				b.WriteByte(c)
			}
		}
		return b.String()
	case strings.HasPrefix(raw, "'"):
		v, _, _ := strings.Cut(raw[1:], "'")
		return v
	default:
		for i := 0; i < len(raw); i++ {
			if raw[i] == '\\' && i+1 < len(raw) {
				i++
			}
			b.WriteByte(raw[i])
		}
		return b.String()
	}
}

// envAssignment returns the key and raw value of an assignment line, and
// false for blank lines, comments and anything else.
func envAssignment(line string) (key, raw string, ok bool) {
	line = strings.TrimSuffix(line, "\r")
	trimmed := strings.TrimLeft(line, " \t")
	if trimmed == "" || trimmed[0] == '#' || trimmed[0] == ';' {
		return "", "", false
	}
	key, raw, ok = strings.Cut(trimmed, "=")
	if !ok {
		return "", "", false
	}
	return strings.TrimSpace(key), raw, true
}

// machineInfoValue returns the value of key in the content of a
// machine-info file: that of its last assignment, which is the one systemd
// uses, and whether there is one.
func machineInfoValue(content, key string) (string, bool) {
	value, found := "", false
	for _, line := range parseTextFile([]byte(content)).lines {
		if k, raw, ok := envAssignment(line); ok && k == key {
			value, found = parseEnvValue(raw), true
		}
	}
	return value, found
}

// setMachineInfoValue returns content with key set to value, or removed if
// value is nil. The first assignment of key is changed in place and any
// others are removed; if there is none, one is appended. All other lines
// are kept byte for byte.
func setMachineInfoValue(content, key string, value *string) string {
	t := parseTextFile([]byte(content))
	var indices []int
	for i, line := range t.lines {
		if k, _, ok := envAssignment(line); ok && k == key {
			indices = append(indices, i)
		}
	}
	keep := len(indices)
	if value != nil && keep > 0 {
		keep = 1
		line := key + "=" + quoteEnvValue(*value)
		if strings.HasSuffix(t.lines[indices[0]], "\r") {
			line += "\r"
		}
		t.lines[indices[0]] = line
		indices = indices[1:]
	}
	for i := len(indices) - 1; i >= 0; i-- {
		t.replace(indices[i], indices[i]+1, nil)
	}
	if value != nil && keep == 0 {
		t.insert(len(t.lines), []string{key + "=" + quoteEnvValue(*value)})
	}
	return string(t.bytes())
}

// hostnameHostsNames returns the hostnames of the /etc/hosts line for name:
// the name itself and, if it has more than one label, its first label, as
// Debian writes the line ("127.0.1.1 web1.example.com web1").
func hostnameHostsNames(name string) []string {
	if short, _, ok := strings.Cut(name, "."); ok && !strings.EqualFold(short, name) {
		return []string{name, short}
	}
	return []string{name}
}

// hostnameHostsIdentities returns the identities of the /etc/hosts lines
// for the given hostnames on the loopback address, skipping empty and
// invalid ones.
func hostnameHostsIdentities(names ...string) []hostsIdentity {
	var ids []hostsIdentity
	for _, n := range names {
		if n != "" && validateHostname(n) == nil {
			ids = append(ids, hostsIdentity{addr: hostnameLoopbackAddr, canonical: n})
		}
	}
	return ids
}

// readHostsLines returns the lines of the hosts file at the host path p and
// its snapshot; no lines and a nil snapshot if it does not exist.
func readHostsLines(p string) (*textFile, *fileSnapshot, error) {
	data, snap, err := readRegularFileNoFollow(p, maxFileLineSize)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, err
	}
	return parseTextFile(data), snap, nil
}

// ensureHostnameHostsLine makes the hosts file at the host path p map the
// loopback address to name (and its first label). A line on that address
// whose canonical name is name or one of previous, the hostnames the line
// was written for before, is updated in place; otherwise one is appended.
// It returns a warning for each other line that maps one of the names to
// another IPv4 address, which the resolver may use instead.
func ensureHostnameHostsLine(p, name string, previous ...string) ([]hostnameWarning, error) {
	unlock, err := lockFileForEdit(p)
	if err != nil {
		return nil, err
	}
	defer unlock()
	text, snap, err := readHostsLines(p)
	if err != nil {
		return nil, err
	}
	e := &hostsEntry{ipText: hostnameLoopbackIP, addr: hostnameLoopbackAddr, names: hostnameHostsNames(name)}
	indices := findHostsEntries(text.lines, hostnameHostsIdentities(append([]string{name}, previous...)...)...)
	var warnings []hostnameWarning
	for _, c := range hostsConflicts(text.lines, e.addr, e.names, indices) {
		warnings = append(warnings, hostnameWarning{"Hostname mapped to another address", fmt.Sprintf("In %s, %s. The resolver uses the first line that maps a name, so it may resolve the hostname to that address rather than %s.",
			defaultHostsPath, c, hostnameLoopbackIP)})
	}
	if ensureHostsEntry(text, indices, e) || snap == nil {
		if err := replaceFileAtomic(p, text.bytes(), snap, hostsFileMode); err != nil {
			return warnings, err
		}
	}
	return warnings, nil
}

// hostnameHostsLineCurrent reports whether the hosts file at the host path
// p has the line that ensureHostnameHostsLine writes for name.
func hostnameHostsLineCurrent(p, name string) (bool, error) {
	text, _, err := readHostsLines(p)
	if err != nil {
		return false, err
	}
	indices := findHostsEntries(text.lines, hostnameHostsIdentities(name)...)
	if len(indices) != 1 {
		return false, nil
	}
	hl, _ := parseHostsLine(text.lines[indices[0]])
	want := hostnameHostsNames(name)
	if len(hl.names) != len(want) {
		return false, nil
	}
	for i := range want {
		if !strings.EqualFold(hl.names[i], want[i]) {
			return false, nil
		}
	}
	return true, nil
}

// hostnameHostsLine returns the first line of the hosts file at the host
// path p that maps the loopback address with canonical name name, or nil.
func hostnameHostsLine(p, name string) (*string, error) {
	text, _, err := readHostsLines(p)
	if err != nil {
		return nil, err
	}
	indices := findHostsEntries(text.lines, hostnameHostsIdentities(name)...)
	if len(indices) == 0 {
		return nil, nil
	}
	line := text.lines[indices[0]]
	return &line, nil
}

// restoreHostnameHostsLine replaces the loopback lines for name in the
// hosts file at the host path p with the recorded line, or removes them if
// recorded is nil. If there is no line for name, nothing is changed.
func restoreHostnameHostsLine(p, name string, recorded *string) error {
	if recorded != nil && strings.ContainsAny(*recorded, "\n\x00") {
		return fmt.Errorf("the recorded %s line is not a single line", defaultHostsPath)
	}
	unlock, err := lockFileForEdit(p)
	if err != nil {
		return err
	}
	defer unlock()
	text, snap, err := readHostsLines(p)
	if err != nil || snap == nil {
		return err
	}
	indices := findHostsEntries(text.lines, hostnameHostsIdentities(name)...)
	if len(indices) == 0 {
		return nil
	}
	removeHostsEntries(text, indices[1:])
	if recorded != nil {
		text.lines[indices[0]] = *recorded
	} else {
		text.replace(indices[0], indices[0]+1, nil)
	}
	return replaceFileAtomic(p, text.bytes(), snap, hostsFileMode)
}

// hostnamePaths are the host paths of the files that hold the hostnames of
// a root.
type hostnamePaths struct {
	hostname, machineInfo, hosts string
}

func hostnamePathsIn(root *fsRoot) (hostnamePaths, error) {
	var p hostnamePaths
	var err error
	if p.hostname, err = root.resolve(hostnameFilePath); err != nil {
		return p, err
	}
	if p.machineInfo, err = root.resolve(machineInfoPath); err != nil {
		return p, err
	}
	if p.hosts, err = root.resolve(defaultHostsPath); err != nil {
		return p, err
	}
	return p, nil
}

// hostnameSnapshot is the hostname configuration of a root. It is recorded
// in private state before the first apply, so that destroy can put it
// back.
type hostnameSnapshot struct {
	// HostnameFile is the content of /etc/hostname, or nil if it does not
	// exist.
	HostnameFile *string `json:"hostname_file,omitempty"`
	// Pretty is the pretty hostname, or nil if machine-info sets none.
	Pretty *string `json:"pretty,omitempty"`
	// MachineInfo records whether /etc/machine-info existed.
	MachineInfo bool `json:"machine_info,omitempty"`
	// Kernel is the kernel hostname; empty below root_dir.
	Kernel string `json:"kernel,omitempty"`
	// HostsLine is the /etc/hosts line that mapped the loopback address to
	// the static hostname, or nil if there was none.
	HostsLine *string `json:"hosts_line,omitempty"`
}

// static returns the static hostname s records, or "".
func (s *hostnameSnapshot) static() string {
	if s.HostnameFile == nil {
		return ""
	}
	return staticHostname(*s.HostnameFile)
}

// readHostnameSnapshot reads the hostname configuration of root, and the
// kernel hostname if root is the host root.
func readHostnameSnapshot(cfg *hostnameConfig, root *fsRoot) (*hostnameSnapshot, error) {
	p, err := hostnamePathsIn(root)
	if err != nil {
		return nil, err
	}
	s := &hostnameSnapshot{}
	if s.HostnameFile, _, err = readOptionalFile(p.hostname); err != nil {
		return nil, err
	}
	mi, _, err := readOptionalFile(p.machineInfo)
	if err != nil {
		return nil, err
	}
	if mi != nil {
		s.MachineInfo = true
		if v, ok := machineInfoValue(*mi, prettyHostnameKey); ok {
			s.Pretty = &v
		}
	}
	if cfg.managesKernel(root) {
		if s.Kernel, err = cfg.kernelHostname(); err != nil {
			return nil, fmt.Errorf("reading the kernel hostname: %w", err)
		}
	}
	if static := s.static(); static != "" {
		if s.HostsLine, err = hostnameHostsLine(p.hosts, static); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// hostnameWarning is a problem that setHostname or restoreHostname worked
// around or that the user should know of.
type hostnameWarning struct {
	Summary, Detail string
}

func hostnamectlWarning(err error, fallback string) hostnameWarning {
	return hostnameWarning{"hostnamectl failed", fmt.Sprintf("%s. %s", capitalize(err.Error()), fallback)}
}

// hostnameSpec is the desired hostname configuration.
type hostnameSpec struct {
	// Static is the static and kernel hostname.
	Static string
	// Pretty is the pretty hostname; nil leaves it alone.
	Pretty *string
	// HostsLine selects whether /etc/hosts gets a loopback line for Static.
	HostsLine bool
	// PreviousNames are hostnames whose loopback line may be taken over.
	PreviousNames []string
}

// setHostname makes root use spec. It reports warnings, such as a failed
// hostnamectl call that was worked around.
func setHostname(ctx context.Context, cfg *hostnameConfig, root *fsRoot, spec hostnameSpec) (warnings []hostnameWarning, err error) {
	if err := validateHostname(spec.Static); err != nil {
		return nil, err
	}
	if cfg.managesKernel(root) {
		if err := validateKernelHostname(spec.Static); err != nil {
			return nil, err
		}
	}
	if spec.Pretty != nil {
		if err := validatePrettyHostname(*spec.Pretty); err != nil {
			return nil, err
		}
	}
	p, err := hostnamePathsIn(root)
	if err != nil {
		return nil, err
	}
	cur, err := readHostnameSnapshot(cfg, root)
	if err != nil {
		return nil, err
	}
	content := spec.Static + "\n"

	if cfg.useHostnamectl(root) {
		if cur.HostnameFile == nil || *cur.HostnameFile != content {
			if err := cfg.hostnamectl(ctx, "--static", spec.Static); err != nil {
				warnings = append(warnings, hostnamectlWarning(err, hostnameFilePath+" was written directly instead."))
			}
		}
		if spec.Pretty != nil && (cur.Pretty == nil || *cur.Pretty != *spec.Pretty) {
			if err := cfg.hostnamectl(ctx, "--pretty", *spec.Pretty); err != nil {
				warnings = append(warnings, hostnamectlWarning(err, machineInfoPath+" was written directly instead."))
			}
		}
	}

	// Whatever hostnamectl did, the files end up exactly as intended.
	if err := setFileContent(p.hostname, &content); err != nil {
		return warnings, err
	}
	if spec.Pretty != nil {
		if err := setMachineInfoPretty(p.machineInfo, spec.Pretty, false); err != nil {
			return warnings, err
		}
	}
	if spec.HostsLine {
		w, err := ensureHostnameHostsLine(p.hosts, spec.Static, spec.PreviousNames...)
		warnings = append(warnings, w...)
		if err != nil {
			return warnings, err
		}
	}
	if cfg.managesKernel(root) {
		if err := ensureKernelHostname(cfg, spec.Static); err != nil {
			return warnings, err
		}
	}
	return warnings, nil
}

// ensureKernelHostname sets the kernel hostname to name unless it already
// is name.
func ensureKernelHostname(cfg *hostnameConfig, name string) error {
	cur, err := cfg.kernelHostname()
	if err != nil {
		return fmt.Errorf("reading the kernel hostname: %w", err)
	}
	if cur == name {
		return nil
	}
	return cfg.setKernelHostname(name)
}

// setMachineInfoPretty sets or, if value is nil, removes the pretty
// hostname in the machine-info file at the host path p. A missing file is
// created only to set a value. With removeEmpty, a file left without any
// content but white space is removed.
func setMachineInfoPretty(p string, value *string, removeEmpty bool) error {
	unlock, err := lockFileForEdit(p)
	if err != nil {
		return err
	}
	defer unlock()
	cur, snap, err := readOptionalFile(p)
	if err != nil {
		return err
	}
	if cur == nil && value == nil {
		return nil
	}
	old := ""
	if cur != nil {
		old = *cur
	}
	updated := setMachineInfoValue(old, prettyHostnameKey, value)
	if removeEmpty && strings.TrimSpace(updated) == "" {
		return removeManagedFile(p, snap)
	}
	if cur != nil && updated == old {
		return nil
	}
	return writeManagedFile(p, []byte(updated), snap, hostnameFileMode)
}

// restoreHostname puts back the configuration recorded in s. current is the
// static hostname the resource set, whose /etc/hosts line is restored if
// hostsLine is set; the pretty hostname is restored if pretty is set.
// Where setHostname would use hostnamectl, it restores a recorded static
// and pretty hostname first, so that systemd-hostnamed hears of it; the
// files are then put back exactly as they were.
func restoreHostname(ctx context.Context, cfg *hostnameConfig, root *fsRoot, s *hostnameSnapshot, current string, pretty, hostsLine bool) (warnings []hostnameWarning, err error) {
	p, err := hostnamePathsIn(root)
	if err != nil {
		return nil, err
	}
	if cfg.useHostnamectl(root) {
		if static := s.static(); static != "" && validateHostname(static) == nil && validateKernelHostname(static) == nil {
			if err := cfg.hostnamectl(ctx, "--static", static); err != nil {
				warnings = append(warnings, hostnamectlWarning(err, hostnameFilePath+" was restored directly instead."))
			}
		}
		if pretty && s.Pretty != nil && validatePrettyHostname(*s.Pretty) == nil {
			if err := cfg.hostnamectl(ctx, "--pretty", *s.Pretty); err != nil {
				warnings = append(warnings, hostnamectlWarning(err, machineInfoPath+" was restored directly instead."))
			}
		}
	}
	if hostsLine && current != "" {
		if err := restoreHostnameHostsLine(p.hosts, current, s.HostsLine); err != nil {
			return warnings, err
		}
	}
	if err := setFileContent(p.hostname, s.HostnameFile); err != nil {
		return warnings, err
	}
	if pretty {
		if err := setMachineInfoPretty(p.machineInfo, s.Pretty, !s.MachineInfo); err != nil {
			return warnings, err
		}
	}
	if cfg.managesKernel(root) && s.Kernel != "" {
		if err := ensureKernelHostname(cfg, s.Kernel); err != nil {
			return warnings, err
		}
	}
	return warnings, nil
}
