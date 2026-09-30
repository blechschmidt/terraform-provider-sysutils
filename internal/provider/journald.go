package provider

// journald drop-in handling behind sysutils_journald_config: validation of
// drop-in names, of the typed [Journal] settings and of extra keys, and
// rendering and parsing of the drop-in. The directory is injectable so that
// tests can use a temporary one.

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const (
	// defaultJournaldDir is the drop-in directory of journald.conf that
	// the administrator owns; /usr/lib and /run have their own.
	defaultJournaldDir = "/etc/systemd/journald.conf.d"
	// journaldFileMode is the mode of every drop-in the resource writes.
	journaldFileMode = 0o644
	// maxJournaldFileSize bounds how much of a drop-in is read.
	maxJournaldFileSize = 1 << 20
	// journaldFileHeader starts every drop-in the resource writes.
	journaldFileHeader = "# Managed by Terraform (sysutils_journald_config). Manual changes will be reverted."
	// journaldSection is the only section of journald.conf.
	journaldSection = "Journal"
	// journaldUnit is the unit restart restarts.
	journaldUnit = "systemd-journald.service"
	// maxJournaldValueLen bounds a value, far above any real one.
	maxJournaldValueLen = 4096
)

// The keys of the typed attributes.
const (
	journaldKeyStorage         = "Storage"
	journaldKeySystemMaxUse    = "SystemMaxUse"
	journaldKeyMaxRetentionSec = "MaxRetentionSec"
	journaldKeyCompress        = "Compress"
	journaldKeyForwardToSyslog = "ForwardToSyslog"
)

// journaldKeyAttrs maps the keys of the typed attributes to the attributes.
var journaldKeyAttrs = map[string]string{
	journaldKeyStorage:         "storage",
	journaldKeySystemMaxUse:    "system_max_use",
	journaldKeyMaxRetentionSec: "max_retention_sec",
	journaldKeyCompress:        "compress",
	journaldKeyForwardToSyslog: "forward_to_syslog",
}

// journaldStorageValues are the values of Storage=.
var journaldStorageValues = []string{"volatile", "persistent", "auto", "none"}

var (
	journaldNamePattern = regexp.MustCompile(`^[A-Za-z0-9_@][A-Za-z0-9_.+@-]*$`)
	journaldKeyPattern  = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)
	// journaldSizePattern matches the sizes systemd's parse_size accepts
	// in their common form: a number with an optional fraction and an
	// optional base-1024 suffix.
	journaldSizePattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?[KMGTPE]?$`)
	// journaldTimespanPattern matches the time spans systemd's parse_sec
	// accepts: numbers, each with an optional unit (seconds if there is
	// none), such as "1month", "2 weeks" or "1d 12h".
	journaldTimespanPattern = regexp.MustCompile(`^([0-9]+(\.[0-9]+)? *(usec|us|µs|μs|msec|ms|seconds|second|sec|s|minutes|minute|min|months|month|M|m|hours|hour|hr|h|days|day|d|weeks|week|w|years|year|y)? *)+$`)
)

// validateJournaldName reports why name cannot name a drop-in; the file is
// <name>.conf.
func validateJournaldName(name string) error {
	switch {
	case name == "":
		return errors.New("name must not be empty")
	case len(name) > 250:
		return errors.New("name must be at most 250 bytes long")
	case strings.HasSuffix(name, ".conf"):
		return fmt.Errorf("name %q must not end with \".conf\", which is appended to it", name)
	case !journaldNamePattern.MatchString(name):
		return fmt.Errorf("name %q must consist of letters, digits and \"_.+@-\", and must not start with \".\", \"+\" or \"-\"; it names a file directly in the journald.conf.d directory", name)
	}
	return nil
}

func validateJournaldStorage(s string) error {
	if !slices.Contains(journaldStorageValues, s) {
		return fmt.Errorf("storage %q must be one of %q", s, journaldStorageValues)
	}
	return nil
}

func validateJournaldSize(s string) error {
	if !journaldSizePattern.MatchString(s) {
		return fmt.Errorf("size %q must be a number of bytes with an optional suffix K, M, G, T, P or E (base 1024), such as \"500M\" or \"2G\"", s)
	}
	return nil
}

func validateJournaldTimespan(s string) error {
	if s != "infinity" && (strings.TrimSpace(s) != s || !journaldTimespanPattern.MatchString(s)) {
		return fmt.Errorf("time span %q must be a systemd time span such as \"1month\", \"2weeks\", \"36h\" or \"1d 12h\", or \"0\" to turn it off", s)
	}
	return nil
}

// validateJournaldKey reports why k cannot be a key of extra.
func validateJournaldKey(k string) error {
	if len(k) > 64 || !journaldKeyPattern.MatchString(k) {
		return fmt.Errorf("key %q must be a journald.conf setting name such as \"RateLimitBurst\": a capital letter followed by letters and digits", k)
	}
	return nil
}

// validateJournaldValue reports why v cannot be a value of extra.
func validateJournaldValue(v string) error {
	switch {
	case len(v) > maxJournaldValueLen:
		return fmt.Errorf("value must be at most %d bytes long", maxJournaldValueLen)
	case strings.TrimSpace(v) != v:
		return fmt.Errorf("value %q must not start or end with white space, which systemd strips", v)
	case strings.HasSuffix(v, "\\"):
		return fmt.Errorf("value %q must not end with a backslash, which continues the line", v)
	}
	for _, c := range v {
		if (c < ' ' && c != '\t') || c == 0x7f {
			return fmt.Errorf("value %q must be a single line without control characters", v)
		}
	}
	return nil
}

// journaldSpec is the [Journal] section of a drop-in. Unset values ("" or
// nil) are not written, so the defaults and other drop-ins apply.
type journaldSpec struct {
	storage, systemMaxUse, maxRetentionSec string
	compress, forwardToSyslog              *bool
	extra                                  map[string]string
}

// validate checks every field of s; the error names the attribute at
// fault.
func (s *journaldSpec) validate() (attr string, err error) {
	for _, f := range []struct {
		attr, v string
		check   func(string) error
	}{
		{"storage", s.storage, validateJournaldStorage},
		{"system_max_use", s.systemMaxUse, validateJournaldSize},
		{"max_retention_sec", s.maxRetentionSec, validateJournaldTimespan},
	} {
		if f.v != "" {
			if err := f.check(f.v); err != nil {
				return f.attr, err
			}
		}
	}
	for _, k := range s.extraKeys() {
		if err := validateJournaldKey(k); err != nil {
			return "extra", err
		}
		if err := validateJournaldValue(s.extra[k]); err != nil {
			return "extra", fmt.Errorf("%s: %w", k, err)
		}
	}
	return s.validateCombination()
}

// validateCombination checks the keys of extra against the typed fields.
func (s *journaldSpec) validateCombination() (attr string, err error) {
	set := s.typed()
	for _, k := range s.extraKeys() {
		if _, ok := set[k]; ok {
			return "extra", fmt.Errorf("key %s is also set by %s; set only one of them", k, journaldKeyAttrs[k])
		}
	}
	return "", nil
}

func (s *journaldSpec) extraKeys() []string {
	keys := make([]string, 0, len(s.extra))
	for k := range s.extra {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// typed returns the typed fields that are set, by key, as written.
func (s *journaldSpec) typed() map[string]string {
	out := map[string]string{}
	yesNo := func(b bool) string {
		if b {
			return "yes"
		}
		return "no"
	}
	if s.storage != "" {
		out[journaldKeyStorage] = s.storage
	}
	if s.systemMaxUse != "" {
		out[journaldKeySystemMaxUse] = s.systemMaxUse
	}
	if s.maxRetentionSec != "" {
		out[journaldKeyMaxRetentionSec] = s.maxRetentionSec
	}
	if s.compress != nil {
		out[journaldKeyCompress] = yesNo(*s.compress)
	}
	if s.forwardToSyslog != nil {
		out[journaldKeyForwardToSyslog] = yesNo(*s.forwardToSyslog)
	}
	return out
}

// render returns the drop-in for s, which must be valid: the typed keys in
// a fixed order, then the extra keys sorted.
func (s *journaldSpec) render() string {
	var b strings.Builder
	b.WriteString(journaldFileHeader + "\n[" + journaldSection + "]\n")
	typed := s.typed()
	for _, k := range []string{journaldKeyStorage, journaldKeySystemMaxUse, journaldKeyMaxRetentionSec, journaldKeyCompress, journaldKeyForwardToSyslog} {
		if v, ok := typed[k]; ok {
			b.WriteString(k + "=" + v + "\n")
		}
	}
	for _, k := range s.extraKeys() {
		b.WriteString(k + "=" + s.extra[k] + "\n")
	}
	return b.String()
}

// parseSystemdBool parses a boolean as systemd's parse_boolean does.
func parseSystemdBool(v string) (bool, bool) {
	switch strings.ToLower(v) {
	case "1", "yes", "y", "true", "t", "on":
		return true, true
	case "0", "no", "n", "false", "f", "off":
		return false, true
	}
	return false, false
}

// parseJournaldFile parses a drop-in back into a spec, for import. Values
// of typed keys that the typed attributes can't hold, such as a size
// threshold for Compress, go to extra, as do all other keys. As in systemd,
// a later assignment of a key wins.
func parseJournaldFile(content string) (*journaldSpec, error) {
	s := &journaldSpec{extra: map[string]string{}}
	inJournal := false
	for i, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
			continue
		case strings.HasSuffix(line, "\\"):
			return nil, fmt.Errorf("line %d: continued lines are not supported", i+1)
		case strings.HasPrefix(line, "["):
			if line != "["+journaldSection+"]" {
				return nil, fmt.Errorf("line %d: section %s is not supported; journald.conf only has [%s]", i+1, line, journaldSection)
			}
			inJournal = true
			continue
		case !inJournal:
			return nil, fmt.Errorf("line %d: setting outside the [%s] section", i+1, journaldSection)
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: %q is not a key=value assignment", i+1, line)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		delete(s.extra, k)
		var typed bool
		switch k {
		case journaldKeyStorage:
			s.storage = ""
			if typed = validateJournaldStorage(v) == nil; typed {
				s.storage = v
			}
		case journaldKeySystemMaxUse:
			s.systemMaxUse = ""
			if typed = validateJournaldSize(v) == nil; typed {
				s.systemMaxUse = v
			}
		case journaldKeyMaxRetentionSec:
			s.maxRetentionSec = ""
			if typed = validateJournaldTimespan(v) == nil; typed {
				s.maxRetentionSec = v
			}
		case journaldKeyCompress, journaldKeyForwardToSyslog:
			b, ok := parseSystemdBool(v)
			dst := &s.compress
			if k == journaldKeyForwardToSyslog {
				dst = &s.forwardToSyslog
			}
			*dst = nil
			if typed = ok; typed {
				*dst = &b
			}
		default:
			typed = false
		}
		if !typed {
			s.extra[k] = v
		}
	}
	if !inJournal {
		return nil, fmt.Errorf("no [%s] section found", journaldSection)
	}
	if attr, err := s.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", attr, err)
	}
	return s, nil
}

// journaldConfig is the provider-level configuration of
// sysutils_journald_config. The zero value, or a nil pointer, selects
// /etc/systemd/journald.conf.d.
type journaldConfig struct {
	dir string
}

func (c *journaldConfig) directory() string {
	if c == nil || c.dir == "" {
		return defaultJournaldDir
	}
	return c.dir
}

// filePath returns the path of the drop-in name, which must be valid.
func (c *journaldConfig) filePath(name string) string {
	return filepath.Join(c.directory(), name+".conf")
}
