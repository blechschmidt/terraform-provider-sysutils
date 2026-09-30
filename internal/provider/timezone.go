package provider

// Time zone handling behind sysutils_timezone. The system time zone is the
// zoneinfo file that /etc/localtime points to; Debian and Alpine also name
// it in /etc/timezone. Everything is read and written below the provider's
// root, through the symlink-safe helpers of safefs.go, except that
// timedatectl sets the zone when systemd booted the host and root_dir is
// not set, so that systemd-timedated sees the change as its own.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	// zoneinfoDir is where tzdata installs the zone files.
	zoneinfoDir = "/usr/share/zoneinfo"
	// localtimePath is the symlink to the system's zone file.
	localtimePath = "/etc/localtime"
	// timezoneFilePath names the zone on Debian-based distributions and
	// Alpine.
	timezoneFilePath = "/etc/timezone"
	// maxTimezoneNameLen bounds the length of a zone name. The longest
	// names in tzdata are about 30 bytes.
	maxTimezoneNameLen = 255
	// maxZoneinfoFileSize bounds how much of a zone file is read. Zone
	// files are a few KiB at most.
	maxZoneinfoFileSize = 1 << 20
	// maxTimezoneFileSize bounds how much of /etc/timezone is read.
	maxTimezoneFileSize = 64 << 10
	// timezoneFileMode is the mode of an /etc/timezone or /etc/localtime
	// file created by the provider.
	timezoneFileMode fs.FileMode = 0o644
	// timedatectlTimeout bounds one timedatectl call.
	timedatectlTimeout = 30 * time.Second
	// singletonID is the id and import ID of the resources that manage a
	// setting of the whole system.
	singletonID = "system"
)

// tzifMagic starts every compiled zone file; see tzfile(5).
var tzifMagic = []byte("TZif")

// timezoneComponentPattern matches one "/"-separated component of a zone
// name, such as "America", "Port-au-Prince" or "GMT+5".
var timezoneComponentPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_+-]*$`)

// timezoneConfig is the provider-level configuration of sysutils_timezone.
// The zero value, or a nil pointer, selects the real timedatectl and /run.
type timezoneConfig struct {
	// run runs timedatectl; nil selects runCommand.
	run commandRunner
	// lookPath finds timedatectl; nil selects exec.LookPath.
	lookPath func(string) (string, error)
	// runDir replaces /run when checking whether systemd booted the host.
	runDir string
}

func (c *timezoneConfig) runner() commandRunner {
	if c == nil || c.run == nil {
		return runCommand
	}
	return c.run
}

// useTimedatectl reports whether the zone is set with timedatectl: only on
// the host root, when systemd booted the host (as sd_booted() checks) and
// timedatectl is installed.
func (c *timezoneConfig) useTimedatectl(root *fsRoot) bool {
	if !root.isHost() {
		return false
	}
	lookPath, runDir := exec.LookPath, defaultRunDir
	if c != nil && c.lookPath != nil {
		lookPath = c.lookPath
	}
	if c != nil && c.runDir != "" {
		runDir = c.runDir
	}
	if !systemdBootedIn(runDir) {
		return false
	}
	_, err := lookPath("timedatectl")
	return err == nil
}

// setWithTimedatectl runs "timedatectl set-timezone <name>".
func (c *timezoneConfig) setWithTimedatectl(ctx context.Context, name string) error {
	argv := []string{"timedatectl", "--no-ask-password", "set-timezone", "--", name}
	res, err := c.runner()(ctx, execSpec{
		Argv:           argv,
		Env:            append(os.Environ(), "SYSTEMD_PAGER=", "SYSTEMD_COLORS=0", "LC_ALL=C"),
		Timeout:        timedatectlTimeout,
		MaxOutputBytes: serviceOutputLimit,
	})
	if err != nil {
		return fmt.Errorf("timedatectl set-timezone %s: %w", name, err)
	}
	if res.TimedOut {
		return fmt.Errorf("timedatectl set-timezone %s: timed out after %s", name, timedatectlTimeout)
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(res.Stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(res.Stdout.String())
		}
		return fmt.Errorf("timedatectl set-timezone %s: exit status %d: %s", name, res.ExitCode, msg)
	}
	return nil
}

// validateTimezoneName reports why name is not an acceptable IANA time zone
// name. Every component must be a plain file name, so the zone file is
// always below the zoneinfo directory.
func validateTimezoneName(name string) error {
	if name == "" {
		return errors.New("time zone must not be empty")
	}
	if len(name) > maxTimezoneNameLen {
		return fmt.Errorf("time zone must be at most %d bytes long", maxTimezoneNameLen)
	}
	for _, c := range strings.Split(name, "/") {
		if !timezoneComponentPattern.MatchString(c) {
			return fmt.Errorf("time zone %q must be an IANA time zone name such as \"Europe/Berlin\" or \"UTC\": "+
				"components separated by \"/\", each made of letters, digits, \"_\", \"+\" and \"-\" and starting with a letter, digit or \"_\"", name)
		}
	}
	return nil
}

// zoneinfoFile returns the path of the zone file of name inside the root.
func zoneinfoFile(name string) string { return zoneinfoDir + "/" + name }

// localtimeLinkTarget is what /etc/localtime points to for name. The target
// is relative, as systemd writes it, so it is also right when the root is
// looked at from outside, such as an image tree below root_dir.
func localtimeLinkTarget(name string) string { return "../usr/share/zoneinfo/" + name }

// readZoneinfo reads the zone file of name below root and checks that it is
// a compiled zone file. Symlinks in the zoneinfo tree, such as
// "UTC -> Etc/UTC", are followed inside the root as long as they stay in
// the zoneinfo directory (see resolveZoneFile).
func readZoneinfo(root *fsRoot, name string) ([]byte, error) {
	if err := validateTimezoneName(name); err != nil {
		return nil, err
	}
	display := zoneinfoFile(name)
	host, err := resolveZoneFile(root, name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("unknown time zone %q: %s does not exist; is tzdata installed?", name, display)
	}
	if err != nil {
		return nil, fmt.Errorf("unknown time zone %q: %w", name, err)
	}
	data, _, err := readRegularFileNoFollow(host, maxZoneinfoFileSize)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("unknown time zone %q: %s does not exist; is tzdata installed?", name, display)
	}
	if err != nil {
		return nil, fmt.Errorf("unknown time zone %q: %w", name, err)
	}
	if !bytes.HasPrefix(data, tzifMagic) {
		return nil, fmt.Errorf("unknown time zone %q: %s is not a compiled zone file", name, display)
	}
	return data, nil
}

// maxZoneinfoLinks bounds the symlinks followed to find a zone file, as
// the kernel's limit of 40 does.
const maxZoneinfoLinks = 40

// resolveZoneFile returns the host path of the zone file of name below
// root. Symlinks are followed, inside the root, but only while they stay in
// the zoneinfo directory: Debian's tzdata has "localtime -> /etc/localtime",
// and an /etc/localtime that pointed to that zone would point to itself. A
// zone whose file is reached only through a symlink out of the zoneinfo
// directory is therefore refused, even if the file it leads to is a zone
// file.
func resolveZoneFile(root *fsRoot, name string) (string, error) {
	follow := func(p string) (string, error) {
		host, err := root.resolveFollow(p)
		if err == nil && root.isHost() {
			host, err = filepath.EvalSymlinks(host)
		}
		return host, err
	}
	zdir, err := follow(zoneinfoDir)
	if err != nil {
		return "", err
	}
	p := zoneinfoFile(name)
	for range maxZoneinfoLinks {
		dir, base := path.Split(p)
		hdir, err := follow(path.Clean(dir))
		if err != nil {
			return "", err
		}
		rel, err := filepath.Rel(zdir, hdir)
		if err != nil || rel == ".." || strings.HasPrefix(rel, "../") || base == "" {
			return "", fmt.Errorf("%s leads out of %s to %s", zoneinfoFile(name), zoneinfoDir, p)
		}
		host := filepath.Join(hdir, base)
		info, err := os.Lstat(host)
		if err != nil {
			return "", err
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			return host, nil
		}
		target, err := os.Readlink(host)
		if err != nil {
			return "", err
		}
		link := path.Join(zoneinfoDir, filepath.ToSlash(rel), base)
		if err := root.checkSymlinkTargetInRoot(link, target); err != nil {
			return "", err
		}
		if path.IsAbs(target) {
			p = path.Clean(target)
		} else {
			p = path.Join(path.Dir(link), target)
		}
	}
	return "", fmt.Errorf("%s: too many levels of symbolic links", zoneinfoFile(name))
}

// zoneFromLinkTarget returns the zone name that an /etc/localtime symlink
// with the given target selects, and false if the target is not below the
// zoneinfo directory.
func zoneFromLinkTarget(target string) (string, bool) {
	p := target
	if !strings.HasPrefix(p, "/") {
		p = path.Join(path.Dir(localtimePath), p)
	}
	p = path.Clean(p)
	name, ok := strings.CutPrefix(p, zoneinfoDir+"/")
	if !ok || validateTimezoneName(name) != nil {
		return "", false
	}
	return name, true
}

// localtimeEntry is what is at /etc/localtime. It is recorded in private
// state before the first apply, so that destroy can put it back.
type localtimeEntry struct {
	// Kind is "symlink", "file" or "absent".
	Kind string `json:"kind"`
	// Target is the target of a symlink.
	Target string `json:"target,omitempty"`
	// Content is the content of a regular file.
	Content []byte `json:"content,omitempty"`
}

const (
	localtimeSymlink = "symlink"
	localtimeFile    = "file"
	localtimeAbsent  = "absent"
)

// timezoneSnapshot is the time zone configuration of a root.
type timezoneSnapshot struct {
	Localtime localtimeEntry `json:"localtime"`
	// TimezoneFile is the content of /etc/timezone, or nil if it does not
	// exist.
	TimezoneFile *string `json:"timezone_file,omitempty"`
}

// timezonePaths returns the host paths of /etc/localtime and /etc/timezone
// in root.
func timezonePaths(root *fsRoot) (localtime, tzfile string, err error) {
	if localtime, err = root.resolve(localtimePath); err != nil {
		return "", "", err
	}
	if tzfile, err = root.resolve(timezoneFilePath); err != nil {
		return "", "", err
	}
	return localtime, tzfile, nil
}

// readLocaltime returns what is at the host path p of /etc/localtime.
func readLocaltime(p string) (localtimeEntry, error) {
	info, err := os.Lstat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return localtimeEntry{Kind: localtimeAbsent}, nil
	case err != nil:
		return localtimeEntry{}, err
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(p)
		if err != nil {
			return localtimeEntry{}, err
		}
		return localtimeEntry{Kind: localtimeSymlink, Target: target}, nil
	case info.Mode().IsRegular():
		data, _, err := readRegularFileNoFollow(p, maxZoneinfoFileSize)
		if err != nil {
			return localtimeEntry{}, err
		}
		return localtimeEntry{Kind: localtimeFile, Content: data}, nil
	default:
		return localtimeEntry{}, fmt.Errorf("%q exists but is neither a symlink nor a regular file", p)
	}
}

// readTimezoneFile returns the content of /etc/timezone at the host path p,
// or nil if it does not exist.
func readTimezoneFile(p string) (*string, error) {
	data, _, err := readRegularFileNoFollow(p, maxTimezoneFileSize)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s := string(data)
	return &s, nil
}

// readTimezoneSnapshot reads the time zone configuration of root.
func readTimezoneSnapshot(root *fsRoot) (*timezoneSnapshot, error) {
	lt, tz, err := timezonePaths(root)
	if err != nil {
		return nil, err
	}
	s := &timezoneSnapshot{}
	if s.Localtime, err = readLocaltime(lt); err != nil {
		return nil, err
	}
	if s.TimezoneFile, err = readTimezoneFile(tz); err != nil {
		return nil, err
	}
	return s, nil
}

// timezoneFileZone returns the zone named by the content of /etc/timezone:
// its first line, without surrounding white space.
func timezoneFileZone(content string) string {
	return firstLine(content)
}

// zone returns the zone that s selects, as the value to record in state,
// and whether it is a zone name:
//
//   - for a symlink into the zoneinfo directory, the zone it points to;
//   - for another symlink, its target, which is not a zone name and shows
//     up as drift;
//   - for a regular file, the zone named in /etc/timezone if the file is a
//     copy of that zone's file (as older installers write it);
//   - "" if no zone can be determined.
//
// If /etc/localtime selects applied, the zone last applied, but
// /etc/timezone names another zone, that zone is returned instead, so that
// the difference shows up as drift.
func (s *timezoneSnapshot) zone(root *fsRoot, applied string) (string, bool) {
	var live string
	switch s.Localtime.Kind {
	case localtimeSymlink:
		name, ok := zoneFromLinkTarget(s.Localtime.Target)
		if !ok {
			return s.Localtime.Target, false
		}
		live = name
	case localtimeFile:
		if s.TimezoneFile != nil {
			name := timezoneFileZone(*s.TimezoneFile)
			if data, err := readZoneinfo(root, name); err == nil && bytes.Equal(data, s.Localtime.Content) {
				live = name
			}
		}
	}
	if live == "" {
		return "", false
	}
	if applied != "" && live == applied && s.TimezoneFile != nil {
		if named := timezoneFileZone(*s.TimezoneFile); named != applied {
			return named, validateTimezoneName(named) == nil
		}
	}
	return live, true
}

// usesTimezoneFile reports whether the distribution in root names the zone
// in /etc/timezone: the file exists already, or the root is Debian-based or
// Alpine, whose tools read it.
func usesTimezoneFile(root *fsRoot, s *timezoneSnapshot) bool {
	if s.TimezoneFile != nil {
		return true
	}
	for _, marker := range []string{"/etc/debian_version", "/etc/alpine-release"} {
		if p, err := root.resolve(marker); err == nil {
			if _, err := os.Lstat(p); err == nil {
				return true
			}
		}
	}
	return false
}

// setTimezone makes root use the zone name. The zone file must exist. It
// reports warnings, such as a failed timedatectl call that was worked
// around, as strings.
func setTimezone(ctx context.Context, cfg *timezoneConfig, root *fsRoot, name string) (warnings []string, err error) {
	if _, err := readZoneinfo(root, name); err != nil {
		return nil, err
	}
	lt, tz, err := timezonePaths(root)
	if err != nil {
		return nil, err
	}

	if cfg.useTimedatectl(root) {
		cur, err := readLocaltime(lt)
		if err != nil {
			return nil, err
		}
		if linked, ok := zoneFromLinkTarget(cur.Target); cur.Kind != localtimeSymlink || !ok || linked != name {
			if err := cfg.setWithTimedatectl(ctx, name); err != nil {
				warnings = append(warnings, fmt.Sprintf("%s. %s was set directly instead.", capitalize(err.Error()), localtimePath))
			}
		}
	}
	if err := ensureLocaltimeLink(lt, name); err != nil {
		return warnings, err
	}

	snap, err := readTimezoneSnapshot(root)
	if err != nil {
		return warnings, err
	}
	if usesTimezoneFile(root, snap) {
		if err := writeTimezoneFile(tz, name+"\n"); err != nil {
			return warnings, err
		}
	}
	return warnings, nil
}

// ensureLocaltimeLink makes the host path lt of /etc/localtime a symlink to
// the zone file of name, unless it already is one.
func ensureLocaltimeLink(lt, name string) error {
	unlock, err := lockFileForEdit(lt)
	if err != nil {
		return err
	}
	defer unlock()
	cur, err := readLocaltime(lt)
	if err != nil {
		return err
	}
	if linked, ok := zoneFromLinkTarget(cur.Target); cur.Kind == localtimeSymlink && ok && linked == name {
		return nil
	}
	return replaceWithSymlink(lt, localtimeLinkTarget(name))
}

// writeTimezoneFile sets the content of /etc/timezone at the host path p,
// keeping its mode and owner.
func writeTimezoneFile(p, content string) error {
	unlock, err := lockFileForEdit(p)
	if err != nil {
		return err
	}
	defer unlock()
	data, snap, err := readRegularFileNoFollow(p, maxTimezoneFileSize)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if snap != nil && string(data) == content {
		return nil
	}
	return writeManagedFile(p, []byte(content), snap, timezoneFileMode)
}

// replaceWithSymlink atomically replaces whatever is at p, a symlink, a
// regular file or nothing, with a symlink to target. A directory or other
// entry at p is refused.
func replaceWithSymlink(p, target string) error {
	if err := checkReplaceable(p); err != nil {
		return err
	}
	tmp, err := createTempSymlink(p, target)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	syncDir(filepath.Dir(p))
	return nil
}

// replaceWithFile atomically replaces whatever is at p, a symlink, a regular
// file or nothing, with a regular file holding data.
func replaceWithFile(p string, data []byte, mode fs.FileMode) (err error) {
	if err := checkReplaceable(p); err != nil {
		return err
	}
	dir, base := filepath.Split(p)
	tmp := filepath.Join(dir, "."+base+".sysutils-tmp-"+randomID())
	f, err := openNoFollow(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("creating temporary file: %w", err)
	}
	defer func() {
		_ = f.Close()
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if err = f.Chmod(mode); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, p); err != nil {
		return err
	}
	syncDir(dir)
	return nil
}

// checkReplaceable fails unless p is a symlink, a regular file or absent.
func checkReplaceable(p string) error {
	info, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&fs.ModeSymlink == 0 && !info.Mode().IsRegular() {
		return fmt.Errorf("%q exists but is neither a symlink nor a regular file", p)
	}
	return nil
}

// restoreTimezone puts back the configuration recorded in s. Where
// setTimezone would use timedatectl and s selects a zone, timedatectl sets
// that zone first: systemd-timedated keeps the zone it last read or set in
// memory until it exits, and would otherwise report the zone set by the
// resource. The files are then put back exactly as they were.
func restoreTimezone(ctx context.Context, cfg *timezoneConfig, root *fsRoot, s *timezoneSnapshot) (warnings []string, err error) {
	lt, tz, err := timezonePaths(root)
	if err != nil {
		return nil, err
	}
	if name, ok := zoneFromLinkTarget(s.Localtime.Target); s.Localtime.Kind == localtimeSymlink && ok && cfg.useTimedatectl(root) {
		if _, err := readZoneinfo(root, name); err == nil {
			if err := cfg.setWithTimedatectl(ctx, name); err != nil {
				warnings = append(warnings, fmt.Sprintf("%s. %s was restored directly instead.", capitalize(err.Error()), localtimePath))
			}
		}
	}
	if err := restoreLocaltime(lt, s.Localtime); err != nil {
		return warnings, err
	}
	if s.TimezoneFile != nil {
		return warnings, writeTimezoneFile(tz, *s.TimezoneFile)
	}
	return warnings, removeTimezoneFile(tz)
}

func restoreLocaltime(lt string, e localtimeEntry) error {
	unlock, err := lockFileForEdit(lt)
	if err != nil {
		return err
	}
	defer unlock()
	cur, err := readLocaltime(lt)
	if err != nil {
		return err
	}
	switch e.Kind {
	case localtimeSymlink:
		if cur.Kind == localtimeSymlink && cur.Target == e.Target {
			return nil
		}
		if err := validateSymlinkTarget(e.Target); err != nil {
			return fmt.Errorf("recorded %s: %w", localtimePath, err)
		}
		return replaceWithSymlink(lt, e.Target)
	case localtimeFile:
		if cur.Kind == localtimeFile && bytes.Equal(cur.Content, e.Content) {
			return nil
		}
		return replaceWithFile(lt, e.Content, timezoneFileMode)
	case localtimeAbsent:
		if cur.Kind == localtimeAbsent {
			return nil
		}
		if err := os.Remove(lt); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		syncDir(filepath.Dir(lt))
		return nil
	default:
		return fmt.Errorf("recorded %s has unknown kind %q", localtimePath, e.Kind)
	}
}

// removeTimezoneFile removes /etc/timezone at the host path p if it is a
// regular file.
func removeTimezoneFile(p string) error {
	unlock, err := lockFileForEdit(p)
	if err != nil {
		return err
	}
	defer unlock()
	_, snap, err := readRegularFileNoFollow(p, maxTimezoneFileSize)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return removeManagedFile(p, snap)
}
