package provider

// Host facts behind the sysutils_host data source. The distribution is read
// from os-release below root_dir; everything about the running system
// (names, kernel, CPUs, memory, init system, firewall) comes from the live
// kernel and procfs, whatever root_dir is.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// maxOSReleaseSize bounds the os-release file read. Real ones are well
	// below 1 KiB.
	maxOSReleaseSize = 64 << 10
	// fqdnLookupTimeout bounds the DNS lookup of the host name.
	fqdnLookupTimeout = 3 * time.Second
	defaultHostsFile  = "/etc/hosts"
)

// osReleasePaths are the locations of os-release, in the order
// os-release(5) says to try them.
var osReleasePaths = []string{"/etc/os-release", "/usr/lib/os-release"}

// liveHostFacts are the attributes of sysutils_host that describe the
// running host even when root_dir is set.
var liveHostFacts = []string{
	"hostname", "fqdn", "kernel_release", "architecture", "cpu_count",
	"memory_total_bytes", "init_system", "firewall_backend",
}

// hostConfig is the provider-level configuration of the sysutils_host data
// source. The zero value, or a nil pointer, selects the real host.
type hostConfig struct {
	// procDir replaces /proc when reading meminfo.
	procDir string
	// hostsFile replaces /etc/hosts when looking up the FQDN.
	hostsFile string
	// unameFn replaces unix.Uname.
	unameFn func(*unix.Utsname) error
	// lookupCNAME replaces the DNS lookup of the FQDN.
	lookupCNAME func(ctx context.Context, host string) (string, error)
}

// osRelease holds the fields of an os-release file.
type osRelease map[string]string

var osReleaseKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// parseOSRelease parses os-release(5) content: newline-separated KEY=value
// assignments in a subset of shell syntax, with values optionally in single
// or double quotes and backslash escapes. Comments and blank lines are
// skipped. Lines that are not assignments are ignored, as os-release(5)
// asks of parsers, and a later assignment wins.
func parseOSRelease(data []byte) osRelease {
	out := osRelease{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 4096), maxOSReleaseSize)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, raw, ok := strings.Cut(line, "=")
		if !ok || !osReleaseKeyPattern.MatchString(key) {
			continue
		}
		if v, ok := unquoteOSReleaseValue(raw); ok {
			out[key] = v
		}
	}
	return out
}

// unquoteOSReleaseValue decodes one value. In double quotes, and unquoted, a
// backslash escapes the next character; in single quotes nothing is
// special. Quoted and unquoted parts can be concatenated, as in a shell.
// Unquoted white space, and anything after it, is invalid.
func unquoteOSReleaseValue(raw string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		switch c := raw[i]; c {
		case '\'':
			end := strings.IndexByte(raw[i+1:], '\'')
			if end < 0 {
				return "", false
			}
			b.WriteString(raw[i+1 : i+1+end])
			i += end + 1
		case '"':
			i++
			for ; i < len(raw) && raw[i] != '"'; i++ {
				if raw[i] == '\\' && i+1 < len(raw) && strings.IndexByte("\\\"$`", raw[i+1]) >= 0 {
					i++
				}
				b.WriteByte(raw[i])
			}
			if i >= len(raw) {
				return "", false
			}
		case '\\':
			if i+1 >= len(raw) {
				return "", false
			}
			i++
			b.WriteByte(raw[i])
		case ' ', '\t':
			return "", false
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), true
}

// id returns ID, which defaults to "linux".
func (r osRelease) id() string {
	if v := r["ID"]; v != "" {
		return v
	}
	return "linux"
}

// idLike returns the space-separated ID_LIKE as a list, never nil.
func (r osRelease) idLike() []string {
	f := strings.Fields(r["ID_LIKE"])
	if f == nil {
		return []string{}
	}
	return f
}

// prettyName returns PRETTY_NAME, which defaults to "Linux".
func (r osRelease) prettyName() string {
	if v := r["PRETTY_NAME"]; v != "" {
		return v
	}
	return "Linux"
}

// versionCodename returns VERSION_CODENAME, or UBUNTU_CODENAME on releases
// of Ubuntu that predate VERSION_CODENAME.
func (r osRelease) versionCodename() string {
	if v := r["VERSION_CODENAME"]; v != "" {
		return v
	}
	return r["UBUNTU_CODENAME"]
}

// testHookRootedFileResolved, if set, runs after readRootedFile resolved a
// path below root_dir and before it opens it.
var testHookRootedFileResolved func(host string)

// readOSRelease reads the first os-release file that exists below root,
// following symlinks inside it (/etc/os-release usually is one). It
// returns the path read, and an error wrapping fs.ErrNotExist if there is
// none.
func readOSRelease(root *fsRoot) (osRelease, string, error) {
	for _, p := range osReleasePaths {
		data, err := readRootedFile(root, p, maxOSReleaseSize)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, p, err
		}
		return parseOSRelease(data), p, nil
	}
	return nil, "", fmt.Errorf("neither %s exists below %s: %w", strings.Join(osReleasePaths, " nor "), root, fs.ErrNotExist)
}

// readRootedFile reads the regular file at the managed path p below root,
// following symlinks inside the root as a chroot would, and fails if it is
// larger than limit. Errors for a missing file wrap fs.ErrNotExist.
func readRootedFile(root *fsRoot, p string, limit int64) ([]byte, error) {
	host, err := root.resolveFollow(p)
	if err != nil {
		return nil, err
	}
	if root.isHost() {
		return readBounded(host, limit)
	}
	if testHookRootedFileResolved != nil {
		testHookRootedFileResolved(host)
	}
	return readBoundedNoSymlinks(host, limit)
}

// readBounded reads the regular file at p, failing if it is larger than
// limit. O_NONBLOCK keeps a FIFO at p from blocking the provider forever.
func readBounded(p string, limit int64) ([]byte, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return readBoundedFile(f, p, limit)
}

// readBoundedNoSymlinks is readBounded for a path that contains no
// symlinks, as resolved below root_dir. It opens the path component by
// component without following symlinks, so that whoever can write to the
// tree cannot swap a directory on the way for a symlink out of it between
// resolving and opening the path, and checks that the file is regular
// before opening it, so that no device node is ever opened.
func readBoundedNoSymlinks(p string, limit int64) ([]byte, error) {
	dirfd, err := openDirPathNoSymlinks(filepath.Dir(p))
	if err != nil {
		if errors.Is(err, errNotRecursivelyRemovable) {
			return nil, fmt.Errorf("%s changed while it was being read: a directory on the way was replaced by a symbolic link", p)
		}
		return nil, err
	}
	defer func() { _ = unix.Close(dirfd) }()
	base := filepath.Base(p)
	var before unix.Stat_t
	if err := unix.Fstatat(dirfd, base, &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, &fs.PathError{Op: "lstat", Path: p, Err: err}
	}
	if before.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, fmt.Errorf("%s is not a regular file", p)
	}
	fd, err := unix.Openat(dirfd, base, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: p, Err: err}
	}
	f := os.NewFile(uintptr(fd), p)
	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil {
		_ = f.Close()
		return nil, &fs.PathError{Op: "fstat", Path: p, Err: err}
	}
	if after.Dev != before.Dev || after.Ino != before.Ino {
		_ = f.Close()
		return nil, fmt.Errorf("%s changed while it was being read", p)
	}
	return readBoundedFile(f, p, limit)
}

// readBoundedFile reads f, which was opened from p, and closes it.
func readBoundedFile(f *os.File, p string, limit int64) ([]byte, error) {
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", p)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", p, limit)
	}
	return data, nil
}

// parseMemTotal returns MemTotal of /proc/meminfo content in bytes.
func parseMemTotal(data []byte) (int64, error) {
	for _, line := range strings.Split(string(data), "\n") {
		rest, ok := strings.CutPrefix(line, "MemTotal:")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) != 2 || f[1] != "kB" {
			return 0, fmt.Errorf("unexpected MemTotal line %q", line)
		}
		kb, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil || kb < 0 || kb > (1<<53)/1024 {
			return 0, fmt.Errorf("unexpected MemTotal line %q", line)
		}
		return kb * 1024, nil
	}
	return 0, errors.New("no MemTotal line")
}

func (c *hostConfig) procDirOrDefault() string {
	if c != nil && c.procDir != "" {
		return c.procDir
	}
	return defaultProcDir
}

// memTotal returns the total memory that the kernel manages, in bytes.
func (c *hostConfig) memTotal() (int64, error) {
	p := filepath.Join(c.procDirOrDefault(), "meminfo")
	data, err := readBounded(p, 1<<20)
	if err != nil {
		return 0, err
	}
	n, err := parseMemTotal(data)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", p, err)
	}
	return n, nil
}

// kernel is the part of uname(2) that sysutils_host reports.
type kernel struct {
	nodename, release, machine string
}

func (c *hostConfig) uname() (kernel, error) {
	fn := unix.Uname
	if c != nil && c.unameFn != nil {
		fn = c.unameFn
	}
	var u unix.Utsname
	if err := fn(&u); err != nil {
		return kernel{}, fmt.Errorf("uname: %w", err)
	}
	return kernel{
		nodename: unix.ByteSliceToString(u.Nodename[:]),
		release:  unix.ByteSliceToString(u.Release[:]),
		machine:  unix.ByteSliceToString(u.Machine[:]),
	}, nil
}

// fqdn returns the host's fully qualified domain name as "hostname -f"
// finds it: hostname itself if it has a dot, else the canonical name of
// the first hosts file line that lists it, if that has a dot, else the
// canonical name in DNS, if the lookup succeeds and has a dot, else
// hostname.
func (c *hostConfig) fqdn(ctx context.Context, hostname string) string {
	if strings.Contains(hostname, ".") || hostname == "" {
		return hostname
	}
	hostsFile := defaultHostsFile
	if c != nil && c.hostsFile != "" {
		hostsFile = c.hostsFile
	}
	if data, err := readBounded(hostsFile, 1<<20); err == nil {
		if name := canonicalNameInHosts(data, hostname); name != "" {
			if strings.Contains(name, ".") {
				return name
			}
		}
	}
	lookup := net.DefaultResolver.LookupCNAME
	if c != nil && c.lookupCNAME != nil {
		lookup = c.lookupCNAME
	}
	ctx, cancel := context.WithTimeout(ctx, fqdnLookupTimeout)
	defer cancel()
	if name, err := lookup(ctx, hostname); err == nil {
		name = strings.TrimSuffix(name, ".")
		if strings.Contains(name, ".") {
			return name
		}
	}
	return hostname
}

// canonicalNameInHosts returns the canonical name (the first name) of the
// first hosts file line that lists name, or "".
func canonicalNameInHosts(data []byte, name string) string {
	for _, line := range strings.Split(string(data), "\n") {
		line, _, _ = strings.Cut(line, "#")
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		for _, n := range f[1:] {
			if strings.EqualFold(n, name) {
				return f[1]
			}
		}
	}
	return ""
}

// hostFacts is everything sysutils_host reports. Empty strings and a nil
// osRelease stand for facts that could not be determined.
type hostFacts struct {
	hostname, fqdn string
	osRelease      osRelease
	osReleasePath  string
	kernel         kernel
	cpuCount       int
	// memTotal is -1 if it could not be read.
	memTotal        int64
	initSystem      string
	packageManager  string
	firewallBackend string
}

// hostFactSources are the detectors that collectHostFacts shares with the
// resources.
type hostFactSources struct {
	root     *fsRoot
	host     *hostConfig
	service  *serviceConfig
	pkg      *packageConfig
	firewall *firewallConfig
}

// hostFactWarning is a fact that could not be determined.
type hostFactWarning struct {
	summary, detail string
}

// collectHostFacts gathers the facts. A missing os-release file, an
// unreadable /proc/meminfo (as in containers without procfs), and
// undetectable init systems, package managers and firewall backends, are
// not errors: the facts are left empty, the first two with a warning.
func collectHostFacts(ctx context.Context, src hostFactSources) (hostFacts, []hostFactWarning, error) {
	var f hostFacts
	var warnings []hostFactWarning

	k, err := src.host.uname()
	if err != nil {
		return f, nil, err
	}
	f.kernel = k
	f.hostname = k.nodename
	f.fqdn = src.host.fqdn(ctx, f.hostname)
	f.cpuCount = runtime.NumCPU()
	if f.memTotal, err = src.host.memTotal(); err != nil {
		f.memTotal = -1
		warnings = append(warnings, hostFactWarning{"Cannot read memory size",
			capitalize(err.Error()) + "; memory_total_bytes is null."})
	}

	f.osRelease, f.osReleasePath, err = readOSRelease(src.root)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		warnings = append(warnings, hostFactWarning{"No os-release file", capitalize(err.Error()) + "; the os_* attributes are null."})
	case err != nil:
		return f, nil, fmt.Errorf("reading %s: %w", f.osReleasePath, err)
	}

	f.initSystem = src.service.probe().kind

	// Below root_dir, look for the tools in the tree; on the host, use the
	// lookPath that sysutils_package itself uses.
	lookPath := lookPathIn(src.root)
	if src.root.isHost() && src.pkg != nil && src.pkg.lookPath != nil {
		lookPath = src.pkg.lookPath
	}
	f.packageManager, _ = detectPackageManager(lookPath)

	f.firewallBackend, _ = src.firewall.detect(ctx)
	return f, warnings, nil
}
