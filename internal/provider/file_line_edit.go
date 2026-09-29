package provider

// Pure text-editing logic behind sysutils_file_line. Nothing in this file
// touches the filesystem, so the semantics can be unit tested in isolation.

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

const (
	// defaultFileLineMarker is the default marker template for blocks. The
	// "{mark}" placeholder is replaced by markBegin and markEnd.
	defaultFileLineMarker = "# {mark} MANAGED BY TERRAFORM"
	markPlaceholder       = "{mark}"
	markBegin             = "BEGIN"
	markEnd               = "END"

	// insertAtEOF and insertAtBOF are the keywords accepted by insert_after
	// and insert_before in place of a regular expression.
	insertAtEOF = "EOF"
	insertAtBOF = "BOF"
)

// errUnterminatedBlock is returned when a BEGIN marker has no matching END
// marker after it. Guessing where such a block ends could delete or
// duplicate unrelated content, so every operation refuses instead.
var errUnterminatedBlock = errors.New("unterminated managed block")

// textFile is a file split into lines. Line terminators are not part of the
// lines; trailingNewline records whether the last line was terminated.
type textFile struct {
	lines           []string
	trailingNewline bool
}

// parseTextFile splits data on "\n". A final "\n" terminates the last line
// rather than starting an empty one.
func parseTextFile(data []byte) *textFile {
	if len(data) == 0 {
		return &textFile{}
	}
	lines := strings.Split(string(data), "\n")
	t := &textFile{lines: lines}
	if lines[len(lines)-1] == "" {
		t.lines = lines[:len(lines)-1]
		t.trailingNewline = true
	}
	return t
}

// bytes is the inverse of parseTextFile.
func (t *textFile) bytes() []byte {
	if len(t.lines) == 0 {
		return nil
	}
	s := strings.Join(t.lines, "\n")
	if t.trailingNewline {
		s += "\n"
	}
	return []byte(s)
}

// replace substitutes lines[start:end] with repl.
func (t *textFile) replace(start, end int, repl []string) {
	out := make([]string, 0, len(t.lines)-(end-start)+len(repl))
	out = append(out, t.lines[:start]...)
	out = append(out, repl...)
	out = append(out, t.lines[end:]...)
	t.lines = out
}

// insert inserts repl before index idx. Content appended at the end of the
// file always gets a terminating newline.
func (t *textFile) insert(idx int, repl []string) {
	if idx >= len(t.lines) {
		idx = len(t.lines)
		t.trailingNewline = true
	}
	t.replace(idx, idx, repl)
}

// fragmentStatus describes how the managed fragment appears in a file.
type fragmentStatus int

const (
	fragmentAbsent fragmentStatus = iota
	// fragmentDiffers means a line selected by regexp, or a block between the
	// markers, exists but its content differs from the desired content.
	fragmentDiffers
	fragmentPresent
)

// fragmentSpec is the compiled form of a sysutils_file_line configuration: a
// single line, optionally located by a regular expression, or a block of
// lines between a BEGIN and an END marker.
type fragmentSpec struct {
	isBlock bool

	// Line mode.
	line string
	re   *regexp.Regexp

	// Block mode.
	block      []string
	begin, end string

	// Where to insert the fragment if it is absent. At most one is set;
	// afterRe/beforeRe are nil for the EOF/BOF keywords.
	insertAfter, insertBefore string
	afterRe, beforeRe         *regexp.Regexp
}

// fragmentConfig holds the raw attribute values a fragmentSpec is built from.
// isBlock selects the mode; the fields of the other mode are ignored.
type fragmentConfig struct {
	isBlock      bool
	line         string
	block        string
	marker       string
	regexp       string
	insertAfter  string
	insertBefore string
}

// newFragmentSpec validates c and compiles its regular expressions.
func newFragmentSpec(c fragmentConfig) (*fragmentSpec, error) {
	s := &fragmentSpec{isBlock: c.isBlock, insertAfter: c.insertAfter, insertBefore: c.insertBefore}
	if c.isBlock {
		marker := c.marker
		if marker == "" {
			marker = defaultFileLineMarker
		}
		if err := validateMarker(marker); err != nil {
			return nil, err
		}
		s.begin = strings.ReplaceAll(marker, markPlaceholder, markBegin)
		s.end = strings.ReplaceAll(marker, markPlaceholder, markEnd)
		s.block = splitBlock(c.block)
		for _, l := range s.block {
			if l == s.begin || l == s.end {
				return nil, fmt.Errorf("block must not contain the marker line %q", l)
			}
		}
	} else {
		if err := validateSingleLine(c.line); err != nil {
			return nil, err
		}
		s.line = c.line
		if c.regexp != "" {
			re, err := regexp.Compile(c.regexp)
			if err != nil {
				return nil, fmt.Errorf("invalid regexp %q: %w", c.regexp, err)
			}
			s.re = re
		}
	}
	var err error
	if s.afterRe, err = compileInsertPosition(c.insertAfter, insertAtEOF); err != nil {
		return nil, fmt.Errorf("invalid insert_after: %w", err)
	}
	if s.beforeRe, err = compileInsertPosition(c.insertBefore, insertAtBOF); err != nil {
		return nil, fmt.Errorf("invalid insert_before: %w", err)
	}
	return s, nil
}

// compileInsertPosition compiles an insert_after/insert_before value. The
// empty string and keyword yield a nil regexp.
func compileInsertPosition(v, keyword string) (*regexp.Regexp, error) {
	if v == "" || v == keyword {
		return nil, nil
	}
	re, err := regexp.Compile(v)
	if err != nil {
		return nil, fmt.Errorf("%q is neither %s nor a valid regular expression: %w", v, keyword, err)
	}
	return re, nil
}

// validateMarker reports why marker is not a usable block marker template.
func validateMarker(marker string) error {
	if !strings.Contains(marker, markPlaceholder) {
		return fmt.Errorf("marker %q must contain the placeholder %q", marker, markPlaceholder)
	}
	if strings.ContainsAny(marker, "\r\n") {
		return fmt.Errorf("marker must be a single line")
	}
	return nil
}

// validateSingleLine reports why line cannot be managed as one line.
func validateSingleLine(line string) error {
	if strings.ContainsAny(line, "\r\n") {
		return fmt.Errorf("line must not contain line breaks; use block to manage several lines")
	}
	return nil
}

// splitBlock splits the block attribute into lines. A single trailing
// newline, as produced by heredocs, is not treated as an extra empty line.
func splitBlock(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// joinBlock is the inverse of splitBlock. trailingNewline selects whether
// the result ends in "\n", so that a value read back from disk can match the
// configured spelling.
func joinBlock(lines []string, trailingNewline bool) string {
	if len(lines) == 0 {
		return ""
	}
	s := strings.Join(lines, "\n")
	if trailingNewline {
		s += "\n"
	}
	return s
}

// render returns the lines the fragment consists of.
func (s *fragmentSpec) render() []string {
	if !s.isBlock {
		return []string{s.line}
	}
	out := make([]string, 0, len(s.block)+2)
	out = append(out, s.begin)
	out = append(out, s.block...)
	return append(out, s.end)
}

// sameIdentity reports whether s and o address the same fragment in a file,
// i.e. the same exact line or the same pair of markers. Content between
// markers does not matter.
func (s *fragmentSpec) sameIdentity(o *fragmentSpec) bool {
	if s.isBlock != o.isBlock {
		return false
	}
	if s.isBlock {
		return s.begin == o.begin && s.end == o.end
	}
	return s.line == o.line
}

// locateBlock returns the range [start, end) spanning the first BEGIN
// marker and the first END marker after it.
func (s *fragmentSpec) locateBlock(lines []string) (start, end int, found bool, err error) {
	for i, l := range lines {
		if l != s.begin {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			if lines[j] == s.end {
				return i, j + 1, true, nil
			}
		}
		return 0, 0, false, fmt.Errorf("%w: found %q on line %d but no %q after it", errUnterminatedBlock, s.begin, i+1, s.end)
	}
	return 0, 0, false, nil
}

// inspect reports whether the fragment is present in lines. For
// fragmentPresent and fragmentDiffers, [start, end) is the range it occupies
// and actual holds its current content (the selected line, or the lines
// between the markers).
//
// In line mode with a regexp, the last line matching the regexp decides:
// the fragment is present if that line equals the desired line, and differs
// otherwise. Only if no line matches is an exact copy of the desired line
// anywhere in the file accepted as present. This mirrors what apply does, so
// a refresh never reports "present" for a file that apply would modify.
func (s *fragmentSpec) inspect(lines []string) (status fragmentStatus, start, end int, actual []string, err error) {
	if s.isBlock {
		start, end, found, err := s.locateBlock(lines)
		if err != nil || !found {
			return fragmentAbsent, 0, 0, nil, err
		}
		actual = lines[start+1 : end-1]
		if slices.Equal(actual, s.block) {
			return fragmentPresent, start, end, actual, nil
		}
		return fragmentDiffers, start, end, actual, nil
	}
	if s.re != nil {
		if i := lastMatch(lines, s.re); i >= 0 {
			if lines[i] == s.line {
				return fragmentPresent, i, i + 1, lines[i : i+1], nil
			}
			return fragmentDiffers, i, i + 1, lines[i : i+1], nil
		}
	}
	for i, l := range lines {
		if l == s.line {
			return fragmentPresent, i, i + 1, lines[i : i+1], nil
		}
	}
	return fragmentAbsent, 0, 0, nil, nil
}

// ensure makes the fragment present in t and reports whether t changed. If
// the fragment is absent it is inserted at hint when hint >= 0, and
// according to insert_after/insert_before otherwise.
func (s *fragmentSpec) ensure(t *textFile, hint int) (bool, error) {
	status, start, end, _, err := s.inspect(t.lines)
	if err != nil {
		return false, err
	}
	switch status {
	case fragmentPresent:
		return false, nil
	case fragmentDiffers:
		t.replace(start, end, s.render())
		return true, nil
	default:
		idx := hint
		if idx < 0 {
			idx = s.insertionIndex(t.lines)
		}
		t.insert(idx, s.render())
		return true, nil
	}
}

// remove deletes the fragment from t: every line equal to the managed line,
// or the first block delimited by the markers. It reports whether t changed
// and the index at which the (first) removed content started.
func (s *fragmentSpec) remove(t *textFile) (changed bool, at int, err error) {
	if s.isBlock {
		start, end, found, err := s.locateBlock(t.lines)
		if err != nil || !found {
			return false, -1, err
		}
		t.replace(start, end, nil)
		return true, start, nil
	}
	at = -1
	kept := make([]string, 0, len(t.lines))
	for i, l := range t.lines {
		if l == s.line {
			if at < 0 {
				at = i
			}
			continue
		}
		kept = append(kept, l)
	}
	if at < 0 {
		return false, -1, nil
	}
	t.lines = kept
	return true, at, nil
}

// insertionIndex returns where an absent fragment is inserted: before the
// last line matching insert_before, at the beginning for BOF, after the last
// line matching insert_after, and at the end otherwise (including when a
// regular expression matches no line).
func (s *fragmentSpec) insertionIndex(lines []string) int {
	switch {
	case s.insertBefore == insertAtBOF:
		return 0
	case s.beforeRe != nil:
		if i := lastMatch(lines, s.beforeRe); i >= 0 {
			return i
		}
	case s.afterRe != nil:
		if i := lastMatch(lines, s.afterRe); i >= 0 {
			return i + 1
		}
	}
	return len(lines)
}

// lastMatch returns the index of the last line matching re, or -1.
func lastMatch(lines []string, re *regexp.Regexp) int {
	for i := len(lines) - 1; i >= 0; i-- {
		if re.MatchString(lines[i]) {
			return i
		}
	}
	return -1
}
