package provider

// Cron job handling behind sysutils_cron_job: validation of schedules and
// the other fields of a job, and rendering and parsing of the one-job files
// the resource keeps in /etc/cron.d. The directory and the owner of the
// files are injectable so that tests can use a temporary directory without
// root privileges.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	// defaultCronDir is where cron reads system crontabs with a user field
	// from, one job file per sysutils_cron_job.
	defaultCronDir = "/etc/cron.d"
	// cronFileMode is the mode of every cron.d file the resource writes.
	// cron ignores files that are writable by group or others.
	cronFileMode fs.FileMode = 0o644
	// maxCronFileSize bounds how much of a cron.d file is read.
	maxCronFileSize = 1 << 20
	// maxCronNameLen is NAME_MAX, the longest file name.
	maxCronNameLen = 255
	// maxCronLineLen bounds the command line and every environment line.
	// It is MAX_COMMAND and MAX_ENVSTR of Vixie cron, as used by Debian
	// and Ubuntu, which ignore longer lines.
	maxCronLineLen = 1000
	// maxCronCommentLen bounds the comment header.
	maxCronCommentLen = 64 << 10
	// cronFileHeader starts every cron.d file the resource writes.
	cronFileHeader = "# Managed by Terraform (sysutils_cron_job). Manual changes will be reverted."
)

var (
	// cronNamePattern matches the file names that every cron implementation
	// reads from cron.d. Debian's cron ignores names with other characters
	// (as run-parts does), notably ".", so "backup.cron" would be skipped.
	cronNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	// cronEnvNamePattern matches environment variable names.
	cronEnvNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// cronKeywords are the special schedules that replace the five time fields.
var cronKeywords = map[string]bool{
	"@reboot": true, "@yearly": true, "@annually": true, "@monthly": true,
	"@weekly": true, "@daily": true, "@midnight": true, "@hourly": true,
}

// cronField describes one of the five time fields of a schedule.
type cronField struct {
	name     string
	min, max int
	// names are the case-insensitive names allowed in place of numbers,
	// indexed from min.
	names []string
}

var cronFields = [5]cronField{
	{name: "minute", min: 0, max: 59},
	{name: "hour", min: 0, max: 23},
	{name: "day of month", min: 1, max: 31},
	{name: "month", min: 1, max: 12, names: []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}},
	// 0 and 7 are both Sunday.
	{name: "day of week", min: 0, max: 7, names: []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}},
}

// validateCronName reports why name cannot name a cron.d file.
func validateCronName(name string) error {
	if len(name) > maxCronNameLen {
		return fmt.Errorf("name must be at most %d bytes long", maxCronNameLen)
	}
	if !cronNamePattern.MatchString(name) {
		return fmt.Errorf("name %q must consist of letters, digits, \"_\" and \"-\" only; cron ignores cron.d files with other characters, such as \".\", in their names", name)
	}
	return nil
}

// validateCronSchedule reports why s is not a schedule that cron accepts:
// either five time fields separated by single spaces, or one of the
// @keywords.
func validateCronSchedule(s string) error {
	if strings.HasPrefix(s, "@") {
		if !cronKeywords[s] {
			return fmt.Errorf("schedule %q is not one of @reboot, @yearly, @annually, @monthly, @weekly, @daily, @midnight and @hourly", s)
		}
		return nil
	}
	fields := strings.Split(s, " ")
	if len(fields) != len(cronFields) {
		return fmt.Errorf("schedule %q must consist of five fields (minute, hour, day of month, month and day of week) separated by single spaces, or be an @keyword such as @daily", s)
	}
	for i, f := range fields {
		if err := cronFields[i].validate(f); err != nil {
			return fmt.Errorf("schedule %q: %w", s, err)
		}
	}
	return nil
}

// validate checks one time field: a comma-separated list of "*", values
// and ranges "a-b", where "*" and ranges may be followed by a step "/n".
func (c cronField) validate(f string) error {
	if f == "" {
		return fmt.Errorf("%s field is empty", c.name)
	}
	for _, item := range strings.Split(f, ",") {
		if err := c.validateItem(item); err != nil {
			return fmt.Errorf("%s field %q: %w", c.name, f, err)
		}
	}
	return nil
}

func (c cronField) validateItem(item string) error {
	if item == "" {
		return errors.New("empty list element")
	}
	rng, step, hasStep := strings.Cut(item, "/")
	if hasStep {
		n, err := strconv.Atoi(step)
		if err != nil || n < 1 || n > c.max || strings.HasPrefix(step, "+") {
			return fmt.Errorf("step %q must be a number from 1 to %d", step, c.max)
		}
	}
	if rng == "*" {
		return nil
	}
	lo, hi, isRange := strings.Cut(rng, "-")
	if hasStep && !isRange {
		return fmt.Errorf("a step is only allowed after \"*\" or a range, not after the single value %q", rng)
	}
	a, err := c.value(lo)
	if err != nil {
		return err
	}
	if !isRange {
		return nil
	}
	b, err := c.value(hi)
	if err != nil {
		return err
	}
	if a > b {
		return fmt.Errorf("range %q must not end before it starts", rng)
	}
	return nil
}

// value parses a number or name of the field.
func (c cronField) value(s string) (int, error) {
	for i, n := range c.names {
		if strings.EqualFold(s, n) {
			return c.min + i, nil
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || s == "" || s[0] < '0' || s[0] > '9' {
		if c.names != nil {
			return 0, fmt.Errorf("%q is not a number from %d to %d or a three-letter name such as %q", s, c.min, c.max, c.names[0])
		}
		return 0, fmt.Errorf("%q is not a number from %d to %d", s, c.min, c.max)
	}
	if n < c.min || n > c.max {
		return 0, fmt.Errorf("%d is out of range; the allowed values are %d to %d", n, c.min, c.max)
	}
	return n, nil
}

// validateCronCommand reports why command cannot be the command of a job.
func validateCronCommand(command string) error {
	switch {
	case command == "":
		return errors.New("command must not be empty")
	case strings.ContainsAny(command, "\n\r\x00"):
		return errors.New("command must be a single line without NUL bytes; use a script for longer commands")
	case strings.TrimSpace(command) != command:
		return errors.New("command must not start or end with white space")
	case len(command) > maxCronLineLen:
		return fmt.Errorf("command must be at most %d bytes long; use a script for longer commands", maxCronLineLen)
	}
	return nil
}

// validateCronEnvName reports why name cannot be set in a cron.d file.
func validateCronEnvName(name string) error {
	if !cronEnvNamePattern.MatchString(name) {
		return fmt.Errorf("environment variable name %q must consist of letters, digits and \"_\", and must not start with a digit", name)
	}
	return nil
}

// validateCronEnvValue reports why value cannot be set in a cron.d file.
func validateCronEnvValue(value string) error {
	if strings.ContainsAny(value, "\n\r\x00") {
		return errors.New("environment variable value must be a single line without NUL bytes")
	}
	if cronEnvNeedsQuotes(value) && strings.Contains(value, `"`) && strings.Contains(value, "'") {
		return fmt.Errorf("environment variable value %q contains both kinds of quotes and white space or a leading quote, which cron cannot represent", value)
	}
	return nil
}

// validateCronComment reports why comment cannot be the comment header.
func validateCronComment(comment string) error {
	if strings.ContainsAny(comment, "\r\x00") {
		return errors.New("comment must not contain carriage returns or NUL bytes")
	}
	if len(comment) > maxCronCommentLen {
		return fmt.Errorf("comment must be at most %d bytes long", maxCronCommentLen)
	}
	return nil
}

// cronJob is the content of a cron.d file managed by sysutils_cron_job.
type cronJob struct {
	schedule, user, command string
	env                     map[string]string
	// comment is the text of the comment lines after the header, without
	// their "# " prefixes, joined by newlines. hasComment distinguishes an
	// empty comment line from no comment.
	comment    string
	hasComment bool
}

// validate checks all fields of j, so that rendering it yields a file that
// parses back into j.
func (j *cronJob) validate() error {
	if err := validateCronSchedule(j.schedule); err != nil {
		return err
	}
	if err := validateAccountName(j.user); err != nil {
		return fmt.Errorf("user: %w", err)
	}
	if err := validateCronCommand(j.command); err != nil {
		return err
	}
	for k, v := range j.env {
		if err := validateCronEnvName(k); err != nil {
			return err
		}
		if err := validateCronEnvValue(v); err != nil {
			return err
		}
		if len(renderCronEnv(k, v)) > maxCronLineLen {
			return fmt.Errorf("environment variable %s must be at most %d bytes long including its name", k, maxCronLineLen)
		}
	}
	return validateCronComment(j.comment)
}

// cronEnvNeedsQuotes reports whether value must be quoted so that cron reads
// it back unchanged: cron trims white space around values and strips a
// pair of quotes around them.
func cronEnvNeedsQuotes(value string) bool {
	return value == "" || strings.ContainsAny(value, " \t") || value[0] == '"' || value[0] == '\''
}

// renderCronEnv returns the line that sets name to value.
func renderCronEnv(name, value string) string {
	switch {
	case !cronEnvNeedsQuotes(value):
		return name + "=" + value
	case !strings.Contains(value, `"`):
		return name + `="` + value + `"`
	default:
		return name + "='" + value + "'"
	}
}

// render returns the cron.d file for j: the header, the comment, the
// environment sorted by name, and the job line.
func (j *cronJob) render() []byte {
	var b strings.Builder
	b.WriteString(cronFileHeader + "\n")
	if j.hasComment {
		for _, l := range strings.Split(j.comment, "\n") {
			if l == "" {
				b.WriteString("#\n")
			} else {
				b.WriteString("# " + l + "\n")
			}
		}
	}
	names := make([]string, 0, len(j.env))
	for k := range j.env {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		b.WriteString(renderCronEnv(k, j.env[k]) + "\n")
	}
	b.WriteString(j.schedule + " " + j.user + " " + j.command + "\n")
	return []byte(b.String())
}

// parsedCronFile is what parseCronFile finds in a cron.d file.
type parsedCronFile struct {
	job cronJob
	// hasJob reports whether the file has a job line. job holds the first
	// one.
	hasJob bool
	// jobs is the number of lines that are neither blank, comments nor
	// environment settings.
	jobs int
}

// isCronComment reports whether l is a comment line.
func isCronComment(l string) bool {
	return strings.HasPrefix(strings.TrimLeft(l, " \t"), "#")
}

// parseCronFile parses a cron.d file as cron reads it, recovering the
// fields render wrote. The comment consists of the comment lines at the
// start of the file, after the header if there is one. The environment
// consists of the settings before the first job line; later ones do not
// apply to it.
func parseCronFile(data []byte) parsedCronFile {
	var p parsedCronFile
	p.job.env = map[string]string{}
	text := strings.TrimSuffix(string(data), "\n")
	if text == "" {
		return p
	}
	lines := strings.Split(text, "\n")
	i := 0
	if lines[0] == cronFileHeader {
		i++
	}
	var comment []string
	for ; i < len(lines) && isCronComment(lines[i]); i++ {
		l := strings.TrimPrefix(strings.TrimLeft(lines[i], " \t"), "#")
		comment = append(comment, strings.TrimPrefix(l, " "))
	}
	if len(comment) > 0 {
		p.job.comment, p.job.hasComment = strings.Join(comment, "\n"), true
	}
	for ; i < len(lines); i++ {
		l := strings.TrimSuffix(lines[i], "\r")
		if strings.TrimSpace(l) == "" || isCronComment(l) {
			continue
		}
		if name, value, ok := parseCronEnvLine(l); ok {
			if !p.hasJob {
				p.job.env[name] = value
			}
			continue
		}
		p.jobs++
		if p.hasJob {
			continue
		}
		if schedule, user, command, ok := parseCronJobLine(l); ok {
			p.job.schedule, p.job.user, p.job.command = schedule, user, command
			p.hasJob = true
		}
	}
	return p
}

// parseCronEnvLine parses an environment setting "name = value". As in
// cron, white space around the name and value is ignored, and a pair of
// quotes around the value is removed.
func parseCronEnvLine(l string) (name, value string, ok bool) {
	name, value, ok = strings.Cut(l, "=")
	if !ok {
		return "", "", false
	}
	name = strings.TrimSpace(name)
	if !cronEnvNamePattern.MatchString(name) {
		return "", "", false
	}
	value = strings.TrimSpace(value)
	if n := len(value); n >= 2 && (value[0] == '"' || value[0] == '\'') && value[n-1] == value[0] {
		value = value[1 : n-1]
	}
	return name, value, true
}

// parseCronJobLine parses a job line of a cron.d file: the five time fields
// or an @keyword, the user, and the command. The time fields are joined with
// single spaces.
func parseCronJobLine(l string) (schedule, user, command string, ok bool) {
	n := len(cronFields)
	if strings.HasPrefix(strings.TrimLeft(l, " \t"), "@") {
		n = 1
	}
	fields, rest := cutFields(l, n+1)
	if len(fields) < n+1 || rest == "" {
		return "", "", "", false
	}
	return strings.Join(fields[:n], " "), fields[n], rest, true
}

// cutFields splits the first n white-space separated fields off s and
// returns them and the remainder with leading white space removed.
func cutFields(s string, n int) (fields []string, rest string) {
	rest = s
	for len(fields) < n {
		rest = strings.TrimLeft(rest, " \t")
		if rest == "" {
			break
		}
		end := strings.IndexAny(rest, " \t")
		if end < 0 {
			end = len(rest)
		}
		fields = append(fields, rest[:end])
		rest = rest[end:]
	}
	return fields, strings.TrimLeft(rest, " \t")
}

// cronConfig is the provider-level configuration of sysutils_cron_job. The
// zero value, or a nil pointer, selects /etc/cron.d and files owned by
// root, which cron requires; tests set a temporary directory and their own
// user.
type cronConfig struct {
	dir      string
	uid, gid uint32
}

func (c *cronConfig) directory() string {
	if c == nil || c.dir == "" {
		return defaultCronDir
	}
	return c.dir
}

func (c *cronConfig) owner() (uid, gid uint32) {
	if c == nil {
		return 0, 0
	}
	return c.uid, c.gid
}

// jobPath returns the path of the cron.d file of the job name, which must
// be valid.
func (c *cronConfig) jobPath(name string) string {
	return filepath.Join(c.directory(), name)
}

// readCronFile reads a cron.d file without following symlinks. A missing
// file reads as nil data with a nil snapshot.
func readCronFile(p string) ([]byte, *fileSnapshot, error) {
	data, snap, err := readRegularFileNoFollow(p, maxCronFileSize)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	return data, snap, err
}

// errCronFileExists is returned by writeCronFile when a new job's file
// already exists.
var errCronFileExists = errors.New("file already exists")

// writeCronFile atomically makes the cron.d file at p contain data, with
// mode 0644 and owned by uid:gid, whatever the mode and owner of an
// existing file were. The mode and owner are set before the file is renamed
// into place, so the new contents are never writable by anyone but the
// owner. Extended attributes such as the SELinux label are carried over,
// except an ACL. With create set, an existing file is an error.
func writeCronFile(p string, data []byte, uid, gid uint32, create bool) error {
	unlock, err := lockFileForEdit(p)
	if err != nil {
		return err
	}
	defer unlock()
	_, snap, err := readCronFile(p)
	if err != nil {
		return err
	}
	if create && snap != nil {
		return fmt.Errorf("%s: %w", p, errCronFileExists)
	}
	attrs := replaceAttrs{mode: cronFileMode, chown: true, uid: uid, gid: gid, dropACL: true}
	if snap != nil {
		attrs.xattrs = make(map[string][]byte, len(snap.xattrs))
		for k, v := range snap.xattrs {
			if k != aclAccessXattr {
				attrs.xattrs[k] = v
			}
		}
	}
	return writeManagedFileWith(p, data, snap, attrs)
}

// removeCronFile removes the cron.d file at p. A missing file is not an
// error; anything other than a regular file is left alone.
func removeCronFile(p string) error {
	unlock, err := lockFileForEdit(p)
	if err != nil {
		return err
	}
	defer unlock()
	info, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := checkRegularFile(p, info); err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	syncDir(filepath.Dir(p))
	return nil
}
