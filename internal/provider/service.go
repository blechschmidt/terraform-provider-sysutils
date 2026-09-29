package provider

// Service runtime state behind sysutils_service. Each supported init system
// is a serviceManager backend that runs the init system's own commands
// through a commandRunner, always with an argument vector and never through
// a shell. Unit tests substitute a fake commandRunner that simulates
// systemctl or rc-service and rc-update.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Init system kinds, the values of the init_system attribute.
const (
	initSystemSystemd = "systemd"
	initSystemOpenRC  = "openrc"
)

// Values of the state attribute. They are the same as those of
// sysutils_systemd_unit.
const (
	serviceStateRunning = unitStateRunning
	serviceStateStopped = unitStateStopped
)

const (
	// defaultRunDir is where the init systems leave the markers that show
	// they booted the host.
	defaultRunDir = "/run"
	// defaultOpenRCRunlevel is the runlevel that rc-update add uses when
	// none is given, and that services are normally enabled in.
	defaultOpenRCRunlevel = "default"
	// serviceOutputLimit caps how much output is kept per stream. Only
	// short status lines and error messages are ever used.
	serviceOutputLimit = 64 << 10
	// maxServiceNameLength is systemd's UNIT_NAME_MAX, which is also far
	// above any OpenRC service name.
	maxServiceNameLength = 255
)

var (
	// serviceNamePattern is the plan-time check of service names, common to
	// all init systems. It admits systemd unit names, with or without a
	// type suffix and including template instances such as
	// "getty@tty1.service" and "\x2d" escapes, and OpenRC service names such
	// as "net.eth0". A name never starts with "-", so it can never be taken
	// for an option, and never contains "/", so it can never be taken for a
	// path.
	serviceNamePattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9:_.@\\-]*$`)
	// openrcServiceNamePattern is the stricter rule for OpenRC, whose
	// service names are file names in /etc/init.d.
	openrcServiceNamePattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.+-]*$`)
	// openrcRunlevelPattern matches runlevel names, which are directory
	// names in /etc/runlevels.
	openrcRunlevelPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,63}$`)
)

// validateServiceName reports why name is not an acceptable service name
// for any init system.
func validateServiceName(name string) error {
	if name == "" {
		return errors.New("service name must not be empty")
	}
	if len(name) > maxServiceNameLength {
		return fmt.Errorf("service name %q is longer than %d characters", name, maxServiceNameLength)
	}
	if !serviceNamePattern.MatchString(name) {
		return fmt.Errorf("service name %q must consist of ASCII letters, digits, \":\", \"_\", \".\", \"@\", \"\\\" or \"-\", starting with a letter, digit or \"_\"", name)
	}
	// systemctl cannot query a template unit such as "getty@.service",
	// only its instances, and fails with an obscure message.
	if _, instance, ok := strings.Cut(name, "@"); ok && (instance == "" || instance[0] == '.') {
		return fmt.Errorf("service name %q is a systemd template unit, which cannot be started or queried itself; name an instance of it, such as %q", name, strings.Replace(name, "@", "@instance", 1))
	}
	return nil
}

// validateServiceNameFor applies the stricter rules of a specific init
// system.
func validateServiceNameFor(kind, name string) error {
	if err := validateServiceName(name); err != nil {
		return err
	}
	if kind == initSystemOpenRC && !openrcServiceNamePattern.MatchString(name) {
		return fmt.Errorf("service name %q is not a valid OpenRC service name: letters, digits, \"_\", \".\", \"+\" or \"-\", starting with a letter, digit or \"_\"", name)
	}
	return nil
}

// validateRunlevel reports why s is not an acceptable OpenRC runlevel.
func validateRunlevel(s string) error {
	if !openrcRunlevelPattern.MatchString(s) {
		return fmt.Errorf("runlevel %q must be 1 to 64 letters, digits, \"_\", \".\" or \"-\", starting with a letter, digit or \"_\"", s)
	}
	return nil
}

// serviceStatus is what the init system reports about a service.
type serviceStatus struct {
	// Found is false if the init system knows no service of that name.
	Found bool
	// Unit is the name the backend acts on: for systemd the unit's
	// primary name, with its type suffix and aliases resolved; for OpenRC
	// the service name itself.
	Unit string
	// Enabled reports whether the service starts at boot.
	Enabled bool
	// Running reports whether the service is running.
	Running bool
	// Detail is the init system's own word for the enablement and active
	// state, such as "static/inactive", for error messages.
	Detail string
}

// serviceManager is an init system backend.
type serviceManager interface {
	// Kind returns the init system kind, such as "systemd".
	Kind() string
	// Status reads the state of a service. It never changes the host.
	Status(ctx context.Context, name string) (serviceStatus, error)
	// SetEnabled enables or disables unit, as returned by Status, at boot.
	SetEnabled(ctx context.Context, unit string, enable bool) error
	// SetRunning starts or stops unit.
	SetRunning(ctx context.Context, unit string, run bool) error
	// Restart restarts unit, starting it if it is stopped.
	Restart(ctx context.Context, unit string) error
	// EnableHint and StartHint explain why enabling or starting a service
	// may appear to succeed without effect.
	EnableHint(unit string) string
	StartHint(unit string) string
}

// serviceConfig is the provider-level configuration of sysutils_service.
// The zero value, or a nil pointer, selects the real init system.
type serviceConfig struct {
	// run runs the backends' commands; nil selects runCommand.
	run commandRunner
	// lookPath finds executables during detection; nil selects
	// exec.LookPath.
	lookPath func(string) (string, error)
	// runDir replaces /run during detection.
	runDir string
}

func (c *serviceConfig) runner() commandRunner {
	if c == nil || c.run == nil {
		return runCommand
	}
	return c.run
}

// detect returns the init system that manages services on this host:
// systemd if it booted the host, as sd_booted() checks, otherwise OpenRC
// if it booted the host, as rc-service itself checks.
func (c *serviceConfig) detect() (string, error) {
	lookPath, runDir := exec.LookPath, defaultRunDir
	if c != nil && c.lookPath != nil {
		lookPath = c.lookPath
	}
	if c != nil && c.runDir != "" {
		runDir = c.runDir
	}
	missing := func(tools ...string) []string {
		var m []string
		for _, tool := range tools {
			if _, err := lookPath(tool); err != nil {
				m = append(m, tool)
			}
		}
		return m
	}

	systemdMarker := filepath.Join(runDir, "systemd", "system")
	if fi, err := os.Stat(systemdMarker); err == nil && fi.IsDir() {
		if m := missing("systemctl"); len(m) > 0 {
			return "", errors.New("systemd is running, but systemctl was not found in PATH")
		}
		return initSystemSystemd, nil
	}
	openrcMarker := filepath.Join(runDir, "openrc", "softlevel")
	_, openrcErr := os.Stat(openrcMarker)
	if openrcErr == nil {
		if m := missing("rc-service", "rc-update"); len(m) > 0 {
			return "", fmt.Errorf("OpenRC booted this host, but %s not found in PATH", strings.Join(m, " and "))
		}
		return initSystemOpenRC, nil
	}

	pid1 := "unknown"
	if comm, err := os.ReadFile("/proc/1/comm"); err == nil {
		pid1 = fmt.Sprintf("%q", strings.TrimSpace(string(comm)))
	}
	var hints []string
	if len(missing("systemctl")) == 0 {
		hints = append(hints, "systemctl is installed, but systemd is not PID 1")
	}
	if len(missing("rc-service", "rc-update")) == 0 {
		hints = append(hints, fmt.Sprintf("OpenRC is installed, but %s does not exist, so OpenRC did not boot this host", openrcMarker))
	}
	msg := fmt.Sprintf("no supported init system found: systemd is not running (%s does not exist) and OpenRC is not running (%s does not exist); PID 1 is %s",
		systemdMarker, openrcMarker, pid1)
	if len(hints) > 0 {
		msg += "; " + strings.Join(hints, "; ")
	}
	if !errors.Is(openrcErr, fs.ErrNotExist) {
		msg += fmt.Sprintf("; checking %s: %v", openrcMarker, openrcErr)
	}
	return "", errors.New(msg + ". sysutils_service supports systemd and OpenRC; containers usually run neither")
}

// manager returns the backend for kind. runlevel is only used by OpenRC.
func (c *serviceConfig) manager(kind string, timeout time.Duration, runlevel string) (serviceManager, error) {
	switch kind {
	case initSystemSystemd:
		return systemdServices{sc: systemctl{run: c.runner(), timeout: timeout}}, nil
	case initSystemOpenRC:
		return openrcServices{run: c.runner(), timeout: timeout, runlevel: runlevel}, nil
	default:
		return nil, fmt.Errorf("unsupported init system %q", kind)
	}
}

// systemdServices manages services with systemctl.
type systemdServices struct {
	sc systemctl
}

func (systemdServices) Kind() string { return initSystemSystemd }

// Status resolves name to the unit's primary name first. systemctl accepts
// names without a type suffix, and aliases such as "sshd.service", but
// is-enabled reports "alias" for an alias, and enable refuses to operate on
// one.
func (s systemdServices) Status(ctx context.Context, name string) (serviceStatus, error) {
	props, err := s.sc.show(ctx, name, "LoadState", "Id")
	if err != nil {
		return serviceStatus{}, err
	}
	unit := props["Id"]
	if props["LoadState"] == "not-found" || unit == "" {
		return serviceStatus{Unit: name}, nil
	}
	enabled, err := s.sc.isEnabled(ctx, unit)
	if err != nil {
		return serviceStatus{}, err
	}
	active, err := s.sc.isActive(ctx, unit)
	if err != nil {
		return serviceStatus{}, err
	}
	return serviceStatus{
		Found:   true,
		Unit:    unit,
		Enabled: unitFileEnabled(enabled),
		Running: unitRunState(active) == unitStateRunning,
		Detail:  fmt.Sprintf("is-enabled %q, is-active %q", enabled, active),
	}, nil
}

func (s systemdServices) SetEnabled(ctx context.Context, unit string, enable bool) error {
	verb := "disable"
	if enable {
		verb = "enable"
	}
	return s.sc.do(ctx, verb, "--", unit)
}

func (s systemdServices) SetRunning(ctx context.Context, unit string, run bool) error {
	if !run {
		return s.sc.do(ctx, "stop", "--", unit)
	}
	if err := s.reloadIfNeeded(ctx, unit); err != nil {
		return err
	}
	return s.sc.do(ctx, "start", "--", unit)
}

// Restart reloads systemd's configuration first. restart_on_change
// typically watches a unit file or drop-in, and systemd would otherwise
// restart the unit with the configuration it loaded before; its
// NeedDaemonReload property does not cover drop-ins created since, so it
// cannot tell whether the reload is needed.
func (s systemdServices) Restart(ctx context.Context, unit string) error {
	if err := s.sc.do(ctx, "daemon-reload"); err != nil {
		return err
	}
	return s.sc.do(ctx, "restart", "--", unit)
}

// reloadIfNeeded runs daemon-reload if systemd reports that the unit's
// file or drop-ins changed since it loaded them, so that start uses the
// current configuration.
func (s systemdServices) reloadIfNeeded(ctx context.Context, unit string) error {
	props, err := s.sc.show(ctx, unit, "NeedDaemonReload")
	if err != nil {
		return err
	}
	if props["NeedDaemonReload"] != "yes" {
		return nil
	}
	return s.sc.do(ctx, "daemon-reload")
}

func (systemdServices) EnableHint(unit string) string {
	return fmt.Sprintf("Only units with an [Install] section that sets WantedBy=, RequiredBy= or Alias= can be enabled, and masked units cannot be enabled; `systemctl status %s` shows the unit's state.", unit)
}

func (systemdServices) StartHint(unit string) string {
	return fmt.Sprintf("The unit may have exited right after starting; a Type=oneshot service needs RemainAfterExit=yes to stay active. `systemctl status %s` and the journal show more.", unit)
}

// openrcServices manages services with rc-service and rc-update. Enabled
// means that the service is in runlevel.
type openrcServices struct {
	run      commandRunner
	timeout  time.Duration
	runlevel string
}

func (openrcServices) Kind() string { return initSystemOpenRC }

func (o openrcServices) exec(ctx context.Context, argv ...string) (*execResult, error) {
	res, err := o.run(ctx, execSpec{
		Argv:           argv,
		Env:            append(os.Environ(), "EINFO_COLOR=NO", "LC_ALL=C"),
		Timeout:        o.timeout,
		MaxOutputBytes: serviceOutputLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
	}
	if res.TimedOut {
		return nil, fmt.Errorf("%s: timed out after %s", strings.Join(argv, " "), o.timeout)
	}
	return res, nil
}

func (o openrcServices) do(ctx context.Context, argv ...string) error {
	res, err := o.exec(ctx, argv...)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return serviceCommandError(argv, res)
	}
	return nil
}

// Status checks that an init script exists with "rc-service --exists",
// whether the service is in the runlevel with "rc-update show", and whether
// it is running with "rc-service status", which exits with 0 only for a
// started service. --exists takes the service name as its argument, so it
// cannot be preceded by "--"; the name never starts with "-".
func (o openrcServices) Status(ctx context.Context, name string) (serviceStatus, error) {
	if err := validateServiceNameFor(initSystemOpenRC, name); err != nil {
		return serviceStatus{}, err
	}
	res, err := o.exec(ctx, "rc-service", "--exists", name)
	if err != nil {
		return serviceStatus{}, err
	}
	if res.ExitCode != 0 {
		return serviceStatus{Unit: name}, nil
	}

	argv := []string{"rc-update", "show", "--", o.runlevel}
	res, err = o.exec(ctx, argv...)
	if err != nil {
		return serviceStatus{}, err
	}
	if res.ExitCode != 0 {
		return serviceStatus{}, serviceCommandError(argv, res)
	}
	enabled := parseRCUpdateShow(res.Stdout.String(), name, o.runlevel)

	argv = []string{"rc-service", "--", name, "status"}
	res, err = o.exec(ctx, argv...)
	if err != nil {
		return serviceStatus{}, err
	}
	status := firstLine(res.Stdout.String())
	if status == "" {
		status = fmt.Sprintf("exit status %d", res.ExitCode)
	}
	// "status: started" exits with 0; stopped, crashed, starting and
	// stopping services exit with 3 and above. Exit status 1 means that
	// rc-service itself failed.
	if res.ExitCode == 1 {
		return serviceStatus{}, serviceCommandError(argv, res)
	}
	inRunlevel := "not in runlevel " + o.runlevel
	if enabled {
		inRunlevel = "in runlevel " + o.runlevel
	}
	return serviceStatus{
		Found:   true,
		Unit:    name,
		Enabled: enabled,
		Running: res.ExitCode == 0,
		Detail:  fmt.Sprintf("%s, %s", inRunlevel, strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(status, "*")), "status: ")),
	}, nil
}

// parseRCUpdateShow reports whether the output of "rc-update show
// <runlevel>" lists name in runlevel. Each line is "<name> | <runlevels>".
func parseRCUpdateShow(out, name, runlevel string) bool {
	for _, line := range strings.Split(out, "\n") {
		svc, levels, ok := strings.Cut(line, "|")
		if !ok || strings.TrimSpace(svc) != name {
			continue
		}
		for _, l := range strings.Fields(levels) {
			if l == runlevel {
				return true
			}
		}
	}
	return false
}

func (o openrcServices) SetEnabled(ctx context.Context, unit string, enable bool) error {
	verb := "del"
	if enable {
		verb = "add"
	}
	return o.do(ctx, "rc-update", verb, "--", unit, o.runlevel)
}

func (o openrcServices) SetRunning(ctx context.Context, unit string, run bool) error {
	verb := "stop"
	if run {
		verb = "start"
	}
	return o.do(ctx, "rc-service", "--", unit, verb)
}

func (o openrcServices) Restart(ctx context.Context, unit string) error {
	return o.do(ctx, "rc-service", "--", unit, "restart")
}

func (o openrcServices) EnableHint(string) string {
	return fmt.Sprintf("`rc-update show %s` lists the services in the runlevel.", o.runlevel)
}

func (openrcServices) StartHint(unit string) string {
	return fmt.Sprintf("The service may have exited right after starting; `rc-service %s status` and its log show more.", unit)
}

// serviceCommandError describes a command that exited with a non-zero
// status, quoting its error output.
func serviceCommandError(argv []string, res *execResult) error {
	msg := strings.TrimSpace(res.Stderr.String())
	if msg == "" {
		msg = strings.TrimSpace(res.Stdout.String())
	}
	if msg == "" {
		return fmt.Errorf("%s: exit status %d", strings.Join(argv, " "), res.ExitCode)
	}
	return fmt.Errorf("%s: exit status %d: %s", strings.Join(argv, " "), res.ExitCode, msg)
}
