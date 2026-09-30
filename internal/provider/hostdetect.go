package provider

// Detection of what runs on this host: the init system, the package manager
// and, in firewall_backend.go, the firewall backend. The resources that act
// on these (sysutils_service, sysutils_timezone, sysutils_package,
// sysutils_package_repository, sysutils_firewall_rule) and the sysutils_host
// data source, which reports them, share these helpers, so that they always
// agree on what was detected.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

const (
	// initSystemSysvinit is only reported by the sysutils_host data source;
	// sysutils_service does not manage services with it.
	initSystemSysvinit = "sysvinit"
	// defaultProcDir is where procfs is mounted.
	defaultProcDir = "/proc"
)

// systemdBootedIn reports whether systemd booted the host, as sd_booted()
// checks: systemd creates <runDir>/systemd/system when it is PID 1.
func systemdBootedIn(runDir string) bool {
	fi, err := os.Stat(systemdMarker(runDir))
	return err == nil && fi.IsDir()
}

func systemdMarker(runDir string) string { return filepath.Join(runDir, "systemd", "system") }
func openrcMarker(runDir string) string  { return filepath.Join(runDir, "openrc", "softlevel") }

// initProbe is what probeInitSystem found out about the running init system.
type initProbe struct {
	// kind is initSystemSystemd, initSystemOpenRC, initSystemSysvinit, or
	// "" if the init system is not recognised, as in most containers.
	kind string
	// openrcErr is the error, other than "does not exist", of checking the
	// OpenRC marker.
	openrcErr error
	// pid1 is the command name of PID 1, or "" if it cannot be read.
	pid1 string
}

// probeInitSystem finds the init system that booted the host: systemd if
// the marker that sd_booted() checks exists, otherwise OpenRC if the marker
// that rc-service checks exists, otherwise SysV init if PID 1 is called
// "init". Only runtime state is examined, never what is installed.
func probeInitSystem(runDir, procDir string) initProbe {
	var p initProbe
	if comm, err := os.ReadFile(filepath.Join(procDir, "1", "comm")); err == nil {
		p.pid1 = strings.TrimSpace(string(comm))
	}
	if systemdBootedIn(runDir) {
		p.kind = initSystemSystemd
		return p
	}
	_, err := os.Stat(openrcMarker(runDir))
	switch {
	case err == nil:
		p.kind = initSystemOpenRC
		return p
	case !errors.Is(err, fs.ErrNotExist):
		p.openrcErr = err
	}
	// sysvinit's and BusyBox's init both run as "init". Upstart, which also
	// did, is long gone.
	if p.pid1 == "init" {
		p.kind = initSystemSysvinit
	}
	return p
}

// missingTools returns those of tools that lookPath cannot find.
func missingTools(lookPath func(string) (string, error), tools ...string) []string {
	var m []string
	for _, tool := range tools {
		if _, err := lookPath(tool); err != nil {
			m = append(m, tool)
		}
	}
	return m
}

// detectPackageManager returns the first kind of packageManagerKinds whose
// tools are all found by lookPath.
func detectPackageManager(lookPath func(string) (string, error)) (string, error) {
	for _, k := range packageManagerKinds {
		if len(missingTools(lookPath, packageManagerTools[k]...)) == 0 {
			return k, nil
		}
	}
	return "", errors.New("no supported package manager found: none of apt (apt-get, apt-cache, dpkg-query), dnf (dnf, rpm), yum (yum, rpm) or apk (apk) is installed")
}

// rootSearchPath is the PATH searched for executables inside a root_dir
// tree. It is the default PATH of Debian's, Fedora's and Alpine's root
// shells, so that it finds the tools wherever the distribution puts them.
var rootSearchPath = []string{"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"}

// lookPathIn returns an exec.LookPath equivalent that finds executables in
// root. For the host root it is exec.LookPath itself, which uses the
// provider's PATH. Below root_dir, rootSearchPath is searched instead, with
// symlinks resolved inside the root, as a chroot would; nothing found there
// is ever run.
func lookPathIn(root *fsRoot) func(string) (string, error) {
	if root.isHost() {
		return exec.LookPath
	}
	return func(name string) (string, error) {
		if name == "" || strings.Contains(name, "/") {
			return "", fmt.Errorf("%q: invalid command name", name)
		}
		for _, dir := range rootSearchPath {
			p := path.Join(dir, name)
			host, err := root.resolveFollow(p)
			if err != nil {
				continue
			}
			if fi, err := os.Stat(host); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
				return p, nil
			}
		}
		return "", fmt.Errorf("%s: executable file not found below %s", name, root)
	}
}
