package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// defaultSystemdUnitDir is where sysutils_systemd_unit writes unit files.
// Units there take precedence over vendor units in /usr/lib/systemd/system.
const defaultSystemdUnitDir = "/etc/systemd/system"

// systemctlOutputLimit caps how much systemctl output is kept per stream.
// Only the first line of stdout and a short stderr excerpt are ever used.
const systemctlOutputLimit = 64 << 10

// commandRunner runs a command. runCommand is the production implementation;
// unit tests substitute a fake to simulate systemctl.
type commandRunner func(ctx context.Context, spec execSpec) (*execResult, error)

// systemdConfig is the provider-level configuration of the systemd unit
// resource. The zero value, or a nil pointer, selects the real systemctl and
// /etc/systemd/system.
type systemdConfig struct {
	unitDir string
	run     commandRunner
}

func (c *systemdConfig) dir() string {
	if c == nil || c.unitDir == "" {
		return defaultSystemdUnitDir
	}
	return c.unitDir
}

func (c *systemdConfig) runner() commandRunner {
	if c == nil || c.run == nil {
		return runCommand
	}
	return c.run
}

// systemctl runs systemctl commands with a per-command timeout.
type systemctl struct {
	run     commandRunner
	timeout time.Duration
}

// exec runs "systemctl <args>" and returns its result. A command that could
// not be started or that timed out is reported as an error; a non-zero exit
// code is not, because several queries (is-enabled, is-active) use it to
// report a state.
func (s systemctl) exec(ctx context.Context, args ...string) (*execResult, error) {
	argv := append([]string{"systemctl", "--no-ask-password", "--no-pager"}, args...)
	res, err := s.run(ctx, execSpec{
		Argv:           argv,
		Env:            systemctlEnv(),
		Timeout:        s.timeout,
		MaxOutputBytes: systemctlOutputLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
	}
	if res.TimedOut {
		return nil, fmt.Errorf("systemctl %s: timed out after %s; a job started by it may still be running in systemd", strings.Join(args, " "), s.timeout)
	}
	return res, nil
}

// systemctlEnv is the environment systemctl runs with: the provider's own,
// with paging and colours disabled so that the output is plain text.
func systemctlEnv() []string {
	env := os.Environ()
	return append(env, "SYSTEMD_PAGER=", "SYSTEMD_COLORS=0", "LC_ALL=C")
}

// do runs "systemctl <args>" and fails on a non-zero exit code.
func (s systemctl) do(ctx context.Context, args ...string) error {
	res, err := s.exec(ctx, args...)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return commandFailedError(args, res)
	}
	return nil
}

func commandFailedError(args []string, res *execResult) error {
	msg := strings.TrimSpace(res.Stderr.String())
	if msg == "" {
		msg = strings.TrimSpace(res.Stdout.String())
	}
	if msg == "" {
		return fmt.Errorf("systemctl %s: exit status %d", strings.Join(args, " "), res.ExitCode)
	}
	return fmt.Errorf("systemctl %s: exit status %d: %s", strings.Join(args, " "), res.ExitCode, msg)
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(s)
}

// isEnabled returns the unit file state printed by "systemctl is-enabled",
// such as "enabled", "disabled", "static" or "masked". A unit file that does
// not exist is reported as "not-found", which older systemd versions only say
// on stderr.
func (s systemctl) isEnabled(ctx context.Context, name string) (string, error) {
	args := []string{"is-enabled", "--", name}
	res, err := s.exec(ctx, args...)
	if err != nil {
		return "", err
	}
	if state := firstLine(res.Stdout.String()); state != "" {
		return state, nil
	}
	if strings.Contains(res.Stderr.String(), "No such file or directory") {
		return "not-found", nil
	}
	return "", commandFailedError(args, res)
}

// isActive returns the active state printed by "systemctl is-active", such as
// "active", "inactive", "failed" or "activating".
func (s systemctl) isActive(ctx context.Context, name string) (string, error) {
	args := []string{"is-active", "--", name}
	res, err := s.exec(ctx, args...)
	if err != nil {
		return "", err
	}
	if state := firstLine(res.Stdout.String()); state != "" {
		return state, nil
	}
	return "", commandFailedError(args, res)
}

// show returns the requested properties of a unit from "systemctl show".
func (s systemctl) show(ctx context.Context, name string, props ...string) (map[string]string, error) {
	args := []string{"show"}
	for _, p := range props {
		args = append(args, "--property="+p)
	}
	args = append(args, "--", name)
	res, err := s.exec(ctx, args...)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, commandFailedError(args, res)
	}
	return parseShowOutput(res.Stdout.String()), nil
}

// parseShowOutput parses the KEY=VALUE lines printed by "systemctl show".
func parseShowOutput(out string) map[string]string {
	props := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			props[k] = v
		}
	}
	return props
}

// unitFileEnabled reports whether a unit file state from is-enabled means
// that the unit is persistently enabled. "enabled-runtime" does not count: it
// is lost on reboot, so enabling such a unit makes it persistent. Static,
// indirect and generated units cannot be enabled at all.
func unitFileEnabled(state string) bool {
	return state == "enabled"
}

// Values of the resource's state attribute.
const (
	unitStateRunning = "running"
	unitStateStopped = "stopped"
)

// unitRunState maps an active state from is-active to the resource's state
// attribute. "activating" counts as stopped: systemctl start waits for
// activation to finish, so a unit still activating afterwards is usually
// stuck in a restart loop.
func unitRunState(active string) string {
	switch active {
	case "active", "reloading", "refreshing":
		return unitStateRunning
	default:
		return unitStateStopped
	}
}

// unitSuffixes are the unit types that sysutils_systemd_unit can manage as a
// unit file. Device and scope units cannot be defined by unit files.
var unitSuffixes = []string{
	".service", ".socket", ".target", ".timer", ".path",
	".mount", ".automount", ".swap", ".slice",
}

// maxUnitNameLength is systemd's UNIT_NAME_MAX.
const maxUnitNameLength = 255

// validateUnitName checks that s is a plain systemd unit name that can safely
// be used as a file name in the unit directory and as a systemctl argument.
func validateUnitName(s string) error {
	if s == "" {
		return errors.New("unit name must not be empty")
	}
	if len(s) > maxUnitNameLength {
		return fmt.Errorf("unit name %q is longer than %d characters", s, maxUnitNameLength)
	}
	if strings.Contains(s, "@") {
		return fmt.Errorf("unit name %q contains \"@\"; template and instance units are not supported", s)
	}
	if s[0] == '-' || s[0] == '.' {
		return fmt.Errorf("unit name %q must not start with %q", s, s[0])
	}
	for _, c := range s {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == ':' || c == '-' || c == '_' || c == '.' || c == '\\'
		if !ok {
			return fmt.Errorf("unit name %q contains the invalid character %q; only ASCII letters, digits, \":\", \"-\", \"_\", \".\" and \"\\\" are allowed", s, c)
		}
	}
	for _, suffix := range unitSuffixes {
		if strings.HasSuffix(s, suffix) && len(s) > len(suffix) {
			return nil
		}
	}
	return fmt.Errorf("unit name %q must be a name followed by one of the suffixes %s", s, strings.Join(unitSuffixes, ", "))
}
