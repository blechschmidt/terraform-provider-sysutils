package provider

// Pure parsers behind the provider-defined functions (functions.go). They
// reuse the parsers of the resources and data sources, so that a function
// reads a file the same way the matching resource does, and touch neither
// the filesystem nor the host.

import (
	"fmt"
	"strconv"
	"strings"
)

// parseIniSections parses INI content into sections of keys, the way
// sysutils_ini_value with the default separator reads it: keys before the
// first section header are in the section "", a section whose header
// appears more than once is merged, and of a key set more than once the
// last value wins. Comments, blank lines and malformed lines are skipped;
// values are taken literally. The section "" is only present if it has keys;
// every other section is present even if it is empty.
func parseIniSections(content string) map[string]map[string]string {
	f := parseIniFile([]byte(content))
	syntax := newIniSyntax(defaultIniSeparator)
	out := map[string]map[string]string{}
	section := ""
	for _, l := range f.lines {
		pl := syntax.parseLine(l.text)
		switch pl.kind {
		case iniSection:
			section = pl.section
			if out[section] == nil {
				out[section] = map[string]string{}
			}
		case iniKey:
			if out[section] == nil {
				out[section] = map[string]string{}
			}
			out[section][pl.key] = pl.value
		}
	}
	return out
}

// oneLine strips one trailing line terminator from line and rejects
// anything that still spans several lines, so that a function given a whole
// file by mistake fails instead of silently parsing its first line.
func oneLine(line string) (string, error) {
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if strings.ContainsAny(line, "\r\n") {
		return "", fmt.Errorf("must be a single line; split the file with split(\"\\n\", ...) first")
	}
	return line, nil
}

// passwdLine is one line of /etc/passwd, as described in passwd(5).
type passwdLine struct {
	name, password string
	uid, gid       int64
	gecos          string
	home, shell    string
}

// parsePasswdLine parses one line of /etc/passwd. ok is false for blank
// lines and "#" comments, which glibc and shadow's tools skip. A line
// without exactly seven colon-separated fields, with an empty name or with a
// uid or gid that is not a number between 0 and 4294967295 is an error.
func parsePasswdLine(line string) (e passwdLine, ok bool, err error) {
	line, err = oneLine(line)
	if err != nil {
		return passwdLine{}, false, err
	}
	if trimmed := strings.TrimSpace(line); trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return passwdLine{}, false, nil
	}
	fields := strings.Split(line, ":")
	if len(fields) != 7 {
		return passwdLine{}, false, fmt.Errorf("a passwd line has 7 colon-separated fields (name:password:uid:gid:gecos:home:shell), got %d", len(fields))
	}
	if fields[0] == "" {
		return passwdLine{}, false, fmt.Errorf("the user name (field 1) must not be empty")
	}
	e = passwdLine{name: fields[0], password: fields[1], gecos: fields[4], home: fields[5], shell: fields[6]}
	for _, id := range []struct {
		name  string
		field int
		dst   *int64
	}{{"uid", 2, &e.uid}, {"gid", 3, &e.gid}} {
		v, err := strconv.ParseUint(fields[id.field], 10, 32)
		if err != nil {
			return passwdLine{}, false, fmt.Errorf("the %s (field %d) must be a number between 0 and 4294967295, got %q", id.name, id.field+1, fields[id.field])
		}
		*id.dst = int64(v)
	}
	return e, true, nil
}

// parseFstabFunctionLine parses one line of /etc/fstab with the parser of
// sysutils_mount. ok is false for blank lines and comments. Unlike
// sysutils_mount, which reads a dump or pass field that is not a number as
// drift, it rejects such lines, and lines with only one field.
func parseFstabFunctionLine(line string) (e fstabEntry, ok bool, err error) {
	line, err = oneLine(line)
	if err != nil {
		return fstabEntry{}, false, err
	}
	fields := asciiFields(line)
	if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
		return fstabEntry{}, false, nil
	}
	e, ok = parseFstabLine(line)
	if !ok {
		return fstabEntry{}, false, fmt.Errorf("an fstab line needs at least a device and a mount point")
	}
	if e.dump < 0 || e.pass < 0 {
		return fstabEntry{}, false, fmt.Errorf("the dump and pass fields (5 and 6) must be non-negative numbers, got %q", strings.Join(fields[4:], " "))
	}
	return e, true, nil
}

// normaliseMode converts a mode to the four-digit octal form the resources
// use ("0644"). It accepts:
//
//   - octal modes as accepted by the mode attributes: "644", "0644", "1777";
//   - ls -l permission strings: "rwxr-xr-x", "rwsr-x--T", optionally with a
//     file type character in front ("drwxrwxrwt");
//   - chmod(1) symbolic modes, applied to an empty mode: "u=rwx,g=rx,o=",
//     "a=r,u+w", "ug=rwx,o-rwx", "u=rwxs,+t". An omitted "who" means "a"
//     (the umask is not applied). The permission copies "u", "g" and "o"
//     and the conditional "X" are rejected, because their result depends on
//     an existing file.
func normaliseMode(s string) (string, error) {
	if s == "" {
		return "", fmt.Errorf("mode must not be empty")
	}
	if octalModePattern.MatchString(s) {
		m, err := parseMode(s)
		if err != nil {
			return "", err
		}
		return formatMode(m), nil
	}
	if v, ok := parseLsMode(s); ok {
		return fmt.Sprintf("%04o", v), nil
	}
	v, err := parseSymbolicMode(s)
	if err != nil {
		return "", fmt.Errorf("mode %q is neither octal (\"0644\"), an ls -l permission string (\"rw-r--r--\") nor a chmod symbolic mode (\"u=rw,go=r\"): %w", s, err)
	}
	return fmt.Sprintf("%04o", v), nil
}

// parseLsMode parses the nine permission characters of ls -l, optionally
// preceded by a file type character.
func parseLsMode(s string) (uint32, bool) {
	if len(s) == 10 && strings.IndexByte("-dlcbps", s[0]) >= 0 {
		s = s[1:]
	}
	if len(s) != 9 {
		return 0, false
	}
	var v uint32
	for i := range 3 {
		triad := s[3*i : 3*i+3]
		shift := uint(3 * (2 - i))
		if triad[0] == 'r' {
			v |= 4 << shift
		} else if triad[0] != '-' {
			return 0, false
		}
		if triad[1] == 'w' {
			v |= 2 << shift
		} else if triad[1] != '-' {
			return 0, false
		}
		// The execute position shows the special bit of its class:
		// lowercase if execute is set as well, uppercase if not.
		special, lower, upper := [3]uint32{octalSetuid, octalSetgid, octalSticky}[i], "sst"[i], "SST"[i]
		switch triad[2] {
		case 'x':
			v |= 1 << shift
		case lower:
			v |= 1<<shift | special
		case upper:
			v |= special
		case '-':
		default:
			return 0, false
		}
	}
	return v, true
}

// parseSymbolicMode applies a chmod(1) symbolic mode to the mode 0000. See
// normaliseMode for the accepted syntax.
func parseSymbolicMode(s string) (uint32, error) {
	var v uint32
	for clause := range strings.SplitSeq(s, ",") {
		i := 0
		var who uint32 // Bits of the classes the clause applies to.
		for ; i < len(clause) && strings.IndexByte("ugoa", clause[i]) >= 0; i++ {
			who |= map[byte]uint32{'u': 0o4700, 'g': 0o2070, 'o': 0o1007, 'a': 0o7777}[clause[i]]
		}
		if who == 0 {
			who = 0o7777
		}
		if i == len(clause) {
			return 0, fmt.Errorf("clause %q has no operator (+, - or =)", clause)
		}
		for i < len(clause) {
			op := clause[i]
			if op != '+' && op != '-' && op != '=' {
				return 0, fmt.Errorf("clause %q: expected +, - or = at %q", clause, clause[i:])
			}
			i++
			var perm uint32
			for ; i < len(clause) && strings.IndexByte("+-=", clause[i]) < 0; i++ {
				switch c := clause[i]; c {
				case 'r':
					perm |= 0o444
				case 'w':
					perm |= 0o222
				case 'x':
					perm |= 0o111
				case 's':
					perm |= octalSetuid | octalSetgid
				case 't':
					perm |= octalSticky
				case 'X', 'u', 'g', 'o':
					return 0, fmt.Errorf("clause %q: %q depends on an existing file and is not supported", clause, string(c))
				default:
					return 0, fmt.Errorf("clause %q: unknown permission %q", clause, string(c))
				}
			}
			perm &= who
			switch op {
			case '+':
				v |= perm
			case '-':
				v &^= perm
			case '=':
				v = v&^who | perm
			}
		}
	}
	return v, nil
}
