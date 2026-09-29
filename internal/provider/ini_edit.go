package provider

// Pure INI parsing and editing behind sysutils_ini_value. Nothing in this
// file touches the filesystem, so the semantics can be unit tested in
// isolation.
//
// The editor works on the file line by line and only ever rewrites, inserts
// or removes the lines of the one key it manages. Comments, blank lines,
// ordering, indentation, unrelated keys and line endings (including "\r\n"
// and a missing newline at the end of the file) are kept byte for byte.

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// defaultIniSeparator is written between key and value unless configured
// otherwise.
const defaultIniSeparator = " = "

// iniLine is one line of an INI file. text excludes the line terminator,
// which is kept in eol so that "\n" and "\r\n" files, and files mixing both,
// are written back unchanged.
type iniLine struct {
	text string
	eol  string // "\n", "\r\n", or "" for an unterminated last line.
}

// iniFile is an INI file split into lines.
type iniFile struct {
	lines []iniLine
}

// parseIniFile splits data into lines. A final line terminator terminates
// the last line rather than starting an empty one.
func parseIniFile(data []byte) *iniFile {
	f := &iniFile{}
	s := string(data)
	for s != "" {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			f.lines = append(f.lines, iniLine{text: s})
			break
		}
		l := iniLine{text: s[:i], eol: "\n"}
		if strings.HasSuffix(l.text, "\r") {
			l.text, l.eol = l.text[:len(l.text)-1], "\r\n"
		}
		f.lines = append(f.lines, l)
		s = s[i+1:]
	}
	return f
}

// bytes is the inverse of parseIniFile.
func (f *iniFile) bytes() []byte {
	var b strings.Builder
	for _, l := range f.lines {
		b.WriteString(l.text)
		b.WriteString(l.eol)
	}
	return []byte(b.String())
}

// newline returns the line terminator for new lines: that of the first
// line, so that "\r\n" files stay "\r\n" files, and "\n" otherwise.
func (f *iniFile) newline() string {
	if len(f.lines) > 0 && f.lines[0].eol != "" {
		return f.lines[0].eol
	}
	return "\n"
}

// insert inserts texts as new lines before index idx. Inserted lines are
// always terminated; if they are appended after an unterminated last line,
// that line is terminated first.
func (f *iniFile) insert(idx int, texts ...string) {
	eol := f.newline()
	if idx >= len(f.lines) {
		idx = len(f.lines)
		if idx > 0 && f.lines[idx-1].eol == "" {
			f.lines[idx-1].eol = eol
		}
	}
	add := make([]iniLine, len(texts))
	for i, t := range texts {
		add[i] = iniLine{text: t, eol: eol}
	}
	f.lines = slices.Insert(f.lines, idx, add...)
}

// remove deletes the lines at the given indices. If the last line of the
// file is removed and it was unterminated, the new last line becomes
// unterminated, so a file without a final newline keeps not having one.
func (f *iniFile) remove(indices []int) {
	if len(indices) == 0 {
		return
	}
	lastUnterminated := f.lines[len(f.lines)-1].eol == ""
	drop := make(map[int]bool, len(indices))
	for _, i := range indices {
		drop[i] = true
	}
	out := f.lines[:0]
	for i, l := range f.lines {
		if !drop[i] {
			out = append(out, l)
		}
	}
	f.lines = out
	if lastUnterminated && len(f.lines) > 0 {
		f.lines[len(f.lines)-1].eol = ""
	}
}

// iniLineKind classifies a line of an INI file.
type iniLineKind int

const (
	iniOther   iniLineKind = iota // Anything else, such as "[bad] header".
	iniBlank                      // Empty or whitespace only.
	iniComment                    // First non-blank character is '#' or ';'.
	iniSection                    // "[name]", optionally followed by a comment.
	iniKey                        // "key<separator>value", or a bare "key".
)

// iniSyntax describes how key lines are split. delim is the separator
// without surrounding whitespace; if it is empty, key and value are
// separated by whitespace, as in sshd_config.
type iniSyntax struct {
	delim string
}

func newIniSyntax(separator string) iniSyntax {
	return iniSyntax{delim: strings.TrimSpace(separator)}
}

// iniParsedLine is the meaning of a line.
type iniParsedLine struct {
	kind    iniLineKind
	section string // For iniSection.
	key     string // For iniKey, and for iniComment if the comment is a commented-out key.
	value   string // For iniKey.
}

// parseLine classifies text, a line without its terminator. Keys are
// everything before the first delimiter and values everything after it,
// both without surrounding whitespace. Values are taken literally: quotes
// and trailing comments are part of the value, because their meaning
// differs between INI dialects.
func (s iniSyntax) parseLine(text string) iniParsedLine {
	trimmed := strings.TrimSpace(text)
	switch {
	case trimmed == "":
		return iniParsedLine{kind: iniBlank}
	case trimmed[0] == '#' || trimmed[0] == ';':
		// Remember which key a comment such as "#Port 22" or
		// "; extension=gd" comments out, to insert the key next to it.
		body := strings.TrimSpace(strings.TrimLeft(trimmed, "#;"))
		pl := iniParsedLine{kind: iniComment}
		if body != "" && body[0] != '[' {
			pl.key, _ = s.splitKeyValue(body)
		}
		return pl
	case trimmed[0] == '[':
		end := strings.IndexByte(trimmed, ']')
		if end < 0 {
			return iniParsedLine{kind: iniOther}
		}
		rest := strings.TrimSpace(trimmed[end+1:])
		if rest != "" && rest[0] != '#' && rest[0] != ';' {
			return iniParsedLine{kind: iniOther}
		}
		return iniParsedLine{kind: iniSection, section: strings.TrimSpace(trimmed[1:end])}
	}
	key, value := s.splitKeyValue(trimmed)
	return iniParsedLine{kind: iniKey, key: key, value: value}
}

// splitKeyValue splits trimmed, a non-empty line without surrounding
// whitespace, into key and value. A line without delimiter is a key with an
// empty value, like "skip-name-resolve" in my.cnf.
func (s iniSyntax) splitKeyValue(trimmed string) (key, value string) {
	if s.delim == "" {
		i := strings.IndexAny(trimmed, " \t")
		if i < 0 {
			return trimmed, ""
		}
		return trimmed[:i], strings.TrimSpace(trimmed[i:])
	}
	key, value, found := strings.Cut(trimmed, s.delim)
	if !found {
		return trimmed, ""
	}
	return strings.TrimSpace(key), strings.TrimSpace(value)
}

// iniSpec identifies the key an editor operation works on.
type iniSpec struct {
	section   string // "" for keys before the first section header.
	key       string
	separator string
	syntax    iniSyntax
}

func newIniSpec(section, key, separator string) (*iniSpec, error) {
	if err := validateIniSection(section); err != nil {
		return nil, err
	}
	if err := validateIniSeparator(separator); err != nil {
		return nil, err
	}
	if err := validateIniKey(key, separator); err != nil {
		return nil, err
	}
	return &iniSpec{section: section, key: key, separator: separator, syntax: newIniSyntax(separator)}, nil
}

// render returns the key line for value, indented with indent.
func (s *iniSpec) render(indent, value string) string {
	line := indent + s.key + s.separator + value
	if value == "" {
		// "key =" rather than "key = " with trailing whitespace.
		line = strings.TrimRight(line, " \t")
	}
	return line
}

// iniBlock is one contiguous region of a section: from its header (or the
// start of the file for the global section) up to the next header.
type iniBlock struct {
	header int // Index of the header line, or -1 for the global section.
	// lastKey is the index of the last key line (of any key) in the
	// block, or -1 if it has none.
	lastKey int
}

// iniMatch is everything the editor needs to know about the managed key.
type iniMatch struct {
	blocks    []iniBlock // Blocks of the section, in file order.
	active    []int      // Key lines of the managed key.
	commented []int      // Comment lines that comment the managed key out.
	// firstHeader is the index of the first section header of the file,
	// or len(lines) if there is none.
	firstHeader int
}

// find locates the managed key in f. A section whose header appears more
// than once is treated as one section, as most INI parsers do.
func (s *iniSpec) find(f *iniFile) *iniMatch {
	m := &iniMatch{firstHeader: len(f.lines)}
	var block *iniBlock
	if s.section == "" {
		m.blocks = append(m.blocks, iniBlock{header: -1, lastKey: -1})
		block = &m.blocks[0]
	}
	for i, l := range f.lines {
		pl := s.syntax.parseLine(l.text)
		if pl.kind == iniSection {
			block = nil
			if m.firstHeader == len(f.lines) {
				m.firstHeader = i
			}
			if s.section != "" && pl.section == s.section {
				m.blocks = append(m.blocks, iniBlock{header: i, lastKey: -1})
				block = &m.blocks[len(m.blocks)-1]
			}
			continue
		}
		if block == nil {
			continue // Not in the managed section.
		}
		switch pl.kind {
		case iniKey:
			block.lastKey = i
			if pl.key == s.key {
				m.active = append(m.active, i)
			}
		case iniComment:
			if pl.key == s.key {
				m.commented = append(m.commented, i)
			}
		}
	}
	return m
}

// values returns the values of every occurrence of the managed key, in file
// order.
func (s *iniSpec) values(f *iniFile) []string {
	m := s.find(f)
	out := make([]string, 0, len(m.active))
	for _, i := range m.active {
		out = append(out, s.syntax.parseLine(f.lines[i].text).value)
	}
	return out
}

// ensure makes value the one and only value of the managed key and reports
// whether f was changed.
//
// If the key exists, its first occurrence is rewritten in place (keeping
// its indentation) when the value differs, and all further occurrences are
// removed. A line that already has the value is left alone even if it is
// formatted with a different separator, such as "key=value".
//
// Otherwise the key is inserted: right after the last commented-out
// occurrence of the key in the section if there is one, else after the last
// key line of the section, else right after the section header. A missing
// section is appended to the end of the file.
func (s *iniSpec) ensure(f *iniFile, value string) (changed bool, err error) {
	if err := validateIniValue(value); err != nil {
		return false, err
	}
	m := s.find(f)
	if len(m.active) > 0 {
		first := m.active[0]
		if s.syntax.parseLine(f.lines[first].text).value != value {
			f.lines[first].text = s.render(leadingSpace(f.lines[first].text), value)
			changed = true
		}
		if len(m.active) > 1 {
			f.remove(m.active[1:])
			changed = true
		}
		return changed, nil
	}

	switch {
	case len(m.commented) > 0:
		at := m.commented[len(m.commented)-1]
		f.insert(at+1, s.render(leadingSpace(f.lines[at].text), value))
	case len(m.blocks) == 0:
		// Missing section: append it, separated by a blank line.
		var add []string
		if n := len(f.lines); n > 0 && strings.TrimSpace(f.lines[n-1].text) != "" {
			add = append(add, "")
		}
		add = append(add, "["+s.section+"]", s.render("", value))
		f.insert(len(f.lines), add...)
	default:
		b := m.blocks[len(m.blocks)-1]
		switch {
		case b.lastKey >= 0:
			f.insert(b.lastKey+1, s.render(leadingSpace(f.lines[b.lastKey].text), value))
		case b.header >= 0:
			f.insert(b.header+1, s.render("", value))
		default:
			s.insertGlobal(f, m, value)
		}
	}
	return true, nil
}

// insertGlobal inserts the managed key into a global section that has no
// keys yet: at the end of a file without sections, or else above the first
// section header and the comment lines directly above it, which usually
// describe that section, followed by a blank line.
func (s *iniSpec) insertGlobal(f *iniFile, m *iniMatch, value string) {
	at := m.firstHeader
	if at == len(f.lines) {
		f.insert(at, s.render("", value))
		return
	}
	for at > 0 && s.syntax.parseLine(f.lines[at-1].text).kind == iniComment {
		at--
	}
	f.insert(at, s.render("", value), "")
}

// remove deletes every occurrence of the managed key and reports whether f
// was changed. Comments, including commented-out occurrences, and the
// section header are kept, even if the section becomes empty.
func (s *iniSpec) remove(f *iniFile) bool {
	m := s.find(f)
	f.remove(m.active)
	return len(m.active) > 0
}

// leadingSpace returns the indentation of text.
func leadingSpace(text string) string {
	return text[:len(text)-len(strings.TrimLeft(text, " \t"))]
}

// validateIniSection checks that section can be written as "[section]" and
// read back as the same name.
func validateIniSection(section string) error {
	switch {
	case strings.ContainsAny(section, "\r\n"):
		return errors.New("section must not contain line breaks")
	case strings.ContainsAny(section, "[]"):
		return errors.New("section must not contain '[' or ']'")
	case strings.TrimSpace(section) != section:
		return errors.New("section must not start or end with whitespace")
	}
	return nil
}

// validateIniSeparator checks that separator is a delimiter, optionally
// surrounded by whitespace, or whitespace only.
func validateIniSeparator(separator string) error {
	delim := strings.TrimSpace(separator)
	switch {
	case separator == "":
		return errors.New("separator must not be empty")
	case strings.ContainsAny(separator, "\r\n"):
		return errors.New("separator must not contain line breaks")
	case strings.ContainsAny(delim, " \t\v\f"):
		return errors.New("separator must not contain whitespace other than at its start and end")
	case delim != "" && strings.ContainsAny(delim[:1], "#;["):
		return fmt.Errorf("separator must not start with %q, which would make the line a comment or section header", delim[:1])
	}
	return nil
}

// validateIniKey checks that key, written with separator, is read back as
// the same key and cannot be mistaken for a comment or section header.
func validateIniKey(key, separator string) error {
	delim := strings.TrimSpace(separator)
	switch {
	case key == "":
		return errors.New("key must not be empty")
	case strings.ContainsAny(key, "\r\n"):
		return errors.New("key must not contain line breaks")
	case strings.TrimSpace(key) != key:
		return errors.New("key must not start or end with whitespace")
	case strings.ContainsAny(key[:1], "#;["):
		return fmt.Errorf("key must not start with %q, which would make the line a comment or section header", key[:1])
	case delim == "" && strings.ContainsAny(key, " \t\v\f"):
		return errors.New("key must not contain whitespace when the separator is whitespace only")
	case delim != "" && strings.Contains(key, delim):
		return fmt.Errorf("key must not contain the separator %q", delim)
	}
	return nil
}

// validateIniValue checks that value is read back unchanged.
func validateIniValue(value string) error {
	switch {
	case strings.ContainsAny(value, "\r\n"):
		return errors.New("value must not contain line breaks")
	case strings.TrimSpace(value) != value:
		return errors.New("value must not start or end with whitespace, which is not preserved when the file is read")
	}
	return nil
}
