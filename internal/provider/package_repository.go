package provider

// Package repository handling behind sysutils_package_repository:
// validation of repository definitions, rendering and parsing of apt's
// deb822 .sources files, dnf and yum .repo files and the managed block in
// apk's repositories file, and handling of OpenPGP signing keys, which are
// given inline or fetched from a URL and stored in ASCII-armored form.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode"
)

// Repository families. apt, dnf/yum and apk each keep repositories in their
// own format; dnf and yum share theirs.
const (
	repoFamilyApt = "apt"
	repoFamilyRpm = "rpm"
	repoFamilyApk = "apk"
)

const (
	// aptSourcesDir holds one deb822 .sources file per apt repository.
	aptSourcesDir = "/etc/apt/sources.list.d"
	// aptKeyringDir holds the signing keys that .sources files refer to
	// with Signed-By, as recommended by Debian for third-party repositories.
	aptKeyringDir = "/etc/apt/keyrings"
	// yumReposDir holds one .repo file per dnf or yum repository.
	yumReposDir = "/etc/yum.repos.d"
	// rpmKeyDir holds the signing keys of dnf and yum repositories.
	rpmKeyDir = "/etc/pki/rpm-gpg"
	// apkRepositoriesFile lists apk's repositories, one per line.
	apkRepositoriesFile = "/etc/apk/repositories"

	// repoFileMode is the mode of repository and key files: world-readable,
	// because apt reads keys as the unprivileged _apt user.
	repoFileMode fs.FileMode = 0o644
	// maxRepoFileSize bounds how much of a repository file is read.
	maxRepoFileSize = 1 << 20
	// maxSigningKeySize bounds signing keys, fetched or inline.
	maxSigningKeySize = 1 << 20
	// signingKeyFetchTimeout bounds fetching a signing key over HTTPS.
	signingKeyFetchTimeout = time.Minute

	maxRepoNameLen        = 100
	maxRepoURILen         = 2048
	maxRepoDescriptionLen = 256
	maxRepoTokenLen       = 128

	// repoFileHeader starts every .sources and .repo file the resource
	// writes.
	repoFileHeader = "# Managed by Terraform (sysutils_package_repository). Manual changes will be reverted."
	// apkMarkerPrefix starts the comment line before each repository line
	// the resource manages in /etc/apk/repositories; the repository name
	// follows it.
	apkMarkerPrefix = "# sysutils_package_repository "
	// aptDescriptionField carries the description in a .sources file. apt
	// ignores fields starting with "X-"; repolib uses this one.
	aptDescriptionField = "X-Repolib-Name"
)

var (
	// repoNamePattern matches repository names. The name is used as a file
	// name in sources.list.d and yum.repos.d, where apt silently ignores
	// files with other characters, and as the dnf repository id.
	repoNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	// aptSuitePattern matches apt suites, including the "$(ARCH)" variable
	// and the "./" style paths of flat repositories.
	aptSuitePattern = regexp.MustCompile(`^[A-Za-z0-9._/+~$()-]+$`)
	// aptComponentPattern matches apt components, such as "main" or
	// "main/debian-installer".
	aptComponentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/+-]*$`)
	// aptArchitecturePattern matches Debian architecture names.
	aptArchitecturePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	// apkTagPattern matches apk repository tags ("@tag" in the file).
	apkTagPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// aptTypes are the allowed values of the Types field.
var aptTypes = map[string]bool{"deb": true, "deb-src": true}

// repoFamily maps a manager kind, as in sysutils_package, to its repository
// family. "auto" and unknown kinds map to "".
func repoFamily(kind string) string {
	switch kind {
	case packageManagerApt:
		return repoFamilyApt
	case packageManagerDnf, packageManagerYum:
		return repoFamilyRpm
	case packageManagerApk:
		return repoFamilyApk
	}
	return ""
}

// detectRepoFamily returns the family whose configuration directory exists
// on the host or below root_dir. exists reports whether a managed path is a
// directory.
func detectRepoFamily(exists func(p string) bool) (string, error) {
	switch {
	case exists("/etc/apt"):
		return repoFamilyApt, nil
	case exists(yumReposDir), exists("/etc/dnf"), exists("/etc/yum"):
		return repoFamilyRpm, nil
	case exists("/etc/apk"):
		return repoFamilyApk, nil
	}
	return "", errors.New("no supported package manager configuration found: none of /etc/apt, /etc/yum.repos.d, /etc/dnf, /etc/yum or /etc/apk is a directory; set manager explicitly")
}

// validateRepoName reports why name cannot name a repository.
func validateRepoName(name string) error {
	if len(name) > maxRepoNameLen {
		return fmt.Errorf("name must be at most %d bytes long", maxRepoNameLen)
	}
	if !repoNamePattern.MatchString(name) {
		return fmt.Errorf("name %q must consist of letters, digits, \"_\", \".\" and \"-\", starting with a letter or digit", name)
	}
	return nil
}

// checkURLString rejects what no repository or key URL may contain
// wherever it is written: white space and control characters, which would
// end the value or the line, and "#", which starts a comment in some of the
// formats.
func checkURLString(what, s string) error {
	if s == "" {
		return fmt.Errorf("%s must not be empty", what)
	}
	if len(s) > maxRepoURILen {
		return fmt.Errorf("%s must be at most %d bytes long", what, maxRepoURILen)
	}
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return fmt.Errorf("%s %q must not contain white space or control characters", what, s)
		}
	}
	if strings.Contains(s, "#") {
		return fmt.Errorf("%s %q must not contain \"#\"", what, s)
	}
	return nil
}

// parseRepoURL parses s and checks it against the allowed schemes. Host
// URLs need a host and must not carry credentials, which would end up in a
// world-readable file; file URLs need an absolute path and no host.
func parseRepoURL(what, s string, schemes ...string) (*url.URL, error) {
	if err := checkURLString(what, s); err != nil {
		return nil, err
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("%s %q is not a valid URL: %w", what, s, err)
	}
	// url.Parse lowercases the scheme; the file must have it as checked.
	ok := false
	for _, scheme := range schemes {
		ok = ok || (u.Scheme == scheme && strings.HasPrefix(s, scheme+":"))
	}
	if !ok {
		return nil, fmt.Errorf("%s %q must be a %s URL, with the scheme in lowercase", what, s, strings.Join(schemes, " or "))
	}
	switch u.Scheme {
	case "file":
		if u.Host != "" && u.Host != "localhost" {
			return nil, fmt.Errorf("%s %q must not name a host", what, s)
		}
		if !strings.HasPrefix(u.Path, "/") || u.Opaque != "" {
			return nil, fmt.Errorf("%s %q must be file:///absolute/path", what, s)
		}
	default:
		if u.Host == "" {
			return nil, fmt.Errorf("%s %q must name a host", what, s)
		}
		if u.User != nil {
			return nil, fmt.Errorf("%s %q must not contain credentials, which would be stored in a world-readable file; configure them in the package manager's authentication settings instead", what, s)
		}
	}
	return u, nil
}

// validateRepoURI reports why s is not an acceptable repository URI for any
// manager: an http, https or file URL.
func validateRepoURI(s string) error {
	_, err := parseRepoURL("URI", s, "http", "https", "file")
	return err
}

// validateSigningKeyURL reports why s is not an acceptable URL to fetch a
// signing key from. Plain http is refused: anyone on the path could
// replace the key, and with it every package the repository serves.
func validateSigningKeyURL(s string) error {
	_, err := parseRepoURL("signing key URL", s, "https", "file")
	return err
}

// validateRepoDescription reports why s cannot describe a repository.
func validateRepoDescription(s string) error {
	if len(s) > maxRepoDescriptionLen {
		return fmt.Errorf("description must be at most %d bytes long", maxRepoDescriptionLen)
	}
	if strings.TrimSpace(s) != s || s == "" {
		return errors.New("description must not be empty or start or end with white space")
	}
	for _, r := range s {
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return fmt.Errorf("description %q must be a single line without control characters", s)
		}
	}
	return nil
}

func validateAptSuite(s string) error {
	if len(s) > maxRepoTokenLen || !aptSuitePattern.MatchString(s) || strings.HasPrefix(s, "-") {
		return fmt.Errorf("suite %q must be 1 to %d letters, digits or characters of \"._/+~$()-\", such as \"bookworm\" or \"./\"", s, maxRepoTokenLen)
	}
	return nil
}

func validateAptComponent(s string) error {
	if len(s) > maxRepoTokenLen || !aptComponentPattern.MatchString(s) {
		return fmt.Errorf("component %q must be 1 to %d letters, digits or characters of \"._/+-\", starting with a letter or digit, such as \"main\"", s, maxRepoTokenLen)
	}
	return nil
}

func validateAptArchitecture(s string) error {
	if len(s) > maxRepoTokenLen || !aptArchitecturePattern.MatchString(s) {
		return fmt.Errorf("architecture %q must be lowercase letters, digits and \"-\", such as \"amd64\"", s)
	}
	return nil
}

func validateAptType(s string) error {
	if !aptTypes[s] {
		return fmt.Errorf("type %q must be \"deb\" or \"deb-src\"", s)
	}
	return nil
}

func validateApkTag(s string) error {
	if !apkTagPattern.MatchString(s) {
		return fmt.Errorf("tag %q must be 1 to 64 letters, digits, \"_\" or \"-\"", s)
	}
	return nil
}

// repoSpec is a repository definition, as configured.
type repoSpec struct {
	name        string
	description string
	enabled     bool
	uris        []string

	// apt
	types         []string
	suites        []string
	components    []string
	architectures []string

	// dnf and yum
	gpgCheck bool

	// apk
	tag string

	// signingKeyPath is the path of the stored signing key that the
	// repository refers to (Signed-By, gpgkey), or "" if there is none.
	signingKeyPath string
	// signingKeyURL is the key URL that dnf and yum fetch themselves.
	signingKeyURL string
}

// validate checks s for family, beyond what the schema checks for every
// family.
func (s *repoSpec) validate(family string) error {
	if err := validateRepoName(s.name); err != nil {
		return err
	}
	if s.description != "" {
		if err := validateRepoDescription(s.description); err != nil {
			return err
		}
	}
	if len(s.uris) == 0 {
		return errors.New("uris must contain at least one URI")
	}
	for _, u := range s.uris {
		if err := validateRepoURI(u); err != nil {
			return err
		}
	}
	notFor := func(attr string, set bool) error {
		if set {
			return fmt.Errorf("%s is not supported with %s repositories", attr, familyDisplay(family))
		}
		return nil
	}
	var errs []error
	switch family {
	case repoFamilyApt:
		if len(s.suites) == 0 {
			return errors.New("suites must be set for apt repositories")
		}
		flat := false
		for _, suite := range s.suites {
			if err := validateAptSuite(suite); err != nil {
				return err
			}
			flat = flat || strings.HasSuffix(suite, "/")
		}
		if flat && len(s.components) > 0 {
			return errors.New("components must not be set when a suite ends with \"/\": such suites name the exact path of a flat repository")
		}
		if !flat && len(s.components) == 0 {
			return errors.New("components must be set for apt repositories, unless the suite is the path of a flat repository ending with \"/\"")
		}
		for _, c := range s.components {
			if err := validateAptComponent(c); err != nil {
				return err
			}
		}
		for _, a := range s.architectures {
			if err := validateAptArchitecture(a); err != nil {
				return err
			}
		}
		for _, t := range s.types {
			if err := validateAptType(t); err != nil {
				return err
			}
		}
		errs = append(errs, notFor("tag", s.tag != ""), notFor("gpg_check = false", !s.gpgCheck))
	case repoFamilyRpm:
		for _, u := range s.uris {
			if strings.Contains(u, ",") {
				return fmt.Errorf("URI %q must not contain \",\", which separates the URLs of dnf's baseurl", u)
			}
		}
		if strings.Contains(s.signingKeyURL, ",") {
			return fmt.Errorf("signing key URL %q must not contain \",\", which separates the URLs of dnf's gpgkey", s.signingKeyURL)
		}
		errs = append(errs,
			notFor("suites", len(s.suites) > 0), notFor("components", len(s.components) > 0),
			notFor("architectures", len(s.architectures) > 0), notFor("types", len(s.types) > 0),
			notFor("tag", s.tag != ""))
	case repoFamilyApk:
		if len(s.uris) != 1 {
			return errors.New("uris must contain exactly one URI for apk repositories")
		}
		if s.tag != "" {
			if err := validateApkTag(s.tag); err != nil {
				return err
			}
		}
		errs = append(errs,
			notFor("suites", len(s.suites) > 0), notFor("components", len(s.components) > 0),
			notFor("architectures", len(s.architectures) > 0), notFor("types", len(s.types) > 0),
			notFor("gpg_check = false", !s.gpgCheck),
			notFor("a signing key", s.signingKeyPath != "" || s.signingKeyURL != ""))
	default:
		return fmt.Errorf("unsupported repository family %q", family)
	}
	return errors.Join(errs...)
}

func familyDisplay(family string) string {
	if family == repoFamilyRpm {
		return "dnf and yum"
	}
	return family
}

// repoPath returns the managed path of the file that holds repository name.
func repoPath(family, name string) string {
	switch family {
	case repoFamilyApt:
		return aptSourcesDir + "/" + name + ".sources"
	case repoFamilyRpm:
		return yumReposDir + "/" + name + ".repo"
	}
	return apkRepositoriesFile
}

// signingKeyPath returns the managed path that the signing key of
// repository name is stored at, or "" if the family has none.
func signingKeyPath(family, name string) string {
	switch family {
	case repoFamilyApt:
		return aptKeyringDir + "/" + name + ".asc"
	case repoFamilyRpm:
		return rpmKeyDir + "/RPM-GPG-KEY-" + name
	}
	return ""
}

// render returns the contents of the repository file (apt, dnf and yum)
// or the managed lines (apk) for s.
func (s *repoSpec) render(family string) []byte {
	var b strings.Builder
	switch family {
	case repoFamilyApt:
		b.WriteString(repoFileHeader + "\n")
		if s.description != "" {
			fmt.Fprintf(&b, "%s: %s\n", aptDescriptionField, s.description)
		}
		types := s.types
		if len(types) == 0 {
			types = []string{"deb"}
		}
		fmt.Fprintf(&b, "Types: %s\n", strings.Join(types, " "))
		fmt.Fprintf(&b, "URIs: %s\n", strings.Join(s.uris, " "))
		fmt.Fprintf(&b, "Suites: %s\n", strings.Join(s.suites, " "))
		if len(s.components) > 0 {
			fmt.Fprintf(&b, "Components: %s\n", strings.Join(s.components, " "))
		}
		if len(s.architectures) > 0 {
			fmt.Fprintf(&b, "Architectures: %s\n", strings.Join(s.architectures, " "))
		}
		if s.signingKeyPath != "" {
			fmt.Fprintf(&b, "Signed-By: %s\n", s.signingKeyPath)
		}
		if !s.enabled {
			b.WriteString("Enabled: no\n")
		}
	case repoFamilyRpm:
		b.WriteString(repoFileHeader + "\n")
		fmt.Fprintf(&b, "[%s]\n", s.name)
		name := s.description
		if name == "" {
			name = s.name
		}
		fmt.Fprintf(&b, "name=%s\n", name)
		fmt.Fprintf(&b, "baseurl=%s\n", strings.Join(s.uris, ","))
		fmt.Fprintf(&b, "enabled=%s\n", boolDigit(s.enabled))
		fmt.Fprintf(&b, "gpgcheck=%s\n", boolDigit(s.gpgCheck))
		switch {
		case s.signingKeyPath != "":
			fmt.Fprintf(&b, "gpgkey=file://%s\n", s.signingKeyPath)
		case s.signingKeyURL != "":
			fmt.Fprintf(&b, "gpgkey=%s\n", s.signingKeyURL)
		}
	case repoFamilyApk:
		b.WriteString(apkMarkerPrefix + s.name)
		if s.description != "" {
			b.WriteString(": " + s.description)
		}
		b.WriteString("\n")
		b.WriteString(s.apkLine() + "\n")
	}
	return []byte(b.String())
}

// apkLine is the repository line of an apk repository.
func (s *repoSpec) apkLine() string {
	line := s.uris[0]
	if s.tag != "" {
		line = "@" + s.tag + " " + line
	}
	if !s.enabled {
		line = "#" + line
	}
	return line
}

func boolDigit(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// parsedRepo is what parsing a repository file found. Unlike repoSpec it
// records the raw signing key reference.
type parsedRepo struct {
	repoSpec
	// signedBy is apt's Signed-By, or the file:// path of dnf's gpgkey.
	signedBy string
	// extraStanzas counts additional deb822 stanzas, which the resource
	// removes on the next apply.
	extraStanzas int
}

// parseAptSources parses a deb822 .sources file. Only the first stanza is
// used; fields that the resource does not manage are ignored, since the
// content attribute shows them as drift anyway.
func parseAptSources(name string, data []byte) parsedRepo {
	p := parsedRepo{repoSpec: repoSpec{name: name, enabled: true, gpgCheck: true}}
	fields := map[string]string{}
	var last string
	stanza := 0
	inStanza := false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, " \t\r")
		switch {
		case strings.HasPrefix(line, "#"):
			continue
		case line == "":
			if inStanza {
				inStanza = false
				last = ""
			}
			continue
		}
		if !inStanza {
			inStanza = true
			stanza++
		}
		if stanza > 1 {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			// Continuation of the previous field.
			if last != "" {
				fields[last] += "\n" + strings.TrimSpace(line)
			}
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		last = strings.ToLower(strings.TrimSpace(k))
		fields[last] = strings.TrimSpace(v)
	}
	if stanza > 1 {
		p.extraStanzas = stanza - 1
	}
	p.description = fields[strings.ToLower(aptDescriptionField)]
	p.types = strings.Fields(fields["types"])
	p.uris = strings.Fields(fields["uris"])
	p.suites = strings.Fields(fields["suites"])
	p.components = strings.Fields(fields["components"])
	p.architectures = strings.Fields(fields["architectures"])
	p.signedBy = fields["signed-by"]
	if e, ok := fields["enabled"]; ok {
		p.enabled = !isFalseWord(e)
	}
	return p
}

// parseYumRepo parses the section of repository name in a .repo file.
func parseYumRepo(name string, data []byte) (parsedRepo, bool) {
	p := parsedRepo{repoSpec: repoSpec{name: name, enabled: true, gpgCheck: false}}
	fields := map[string]string{}
	found, in := false, false
	var last string
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";"):
			continue
		case strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]"):
			in = strings.TrimSpace(trimmed[1:len(trimmed)-1]) == name
			found = found || in
			last = ""
			continue
		case !in:
			continue
		case line[0] == ' ' || line[0] == '\t':
			if last != "" {
				fields[last] += " " + trimmed
			}
			continue
		}
		k, v, ok := strings.Cut(trimmed, "=")
		if !ok {
			continue
		}
		last = strings.ToLower(strings.TrimSpace(k))
		fields[last] = strings.TrimSpace(v)
	}
	if !found {
		return p, false
	}
	if n := fields["name"]; n != name {
		p.description = n
	}
	p.uris = strings.FieldsFunc(fields["baseurl"], func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
	if v, ok := fields["enabled"]; ok {
		p.enabled = !isFalseWord(v)
	}
	if v, ok := fields["gpgcheck"]; ok {
		p.gpgCheck = !isFalseWord(v)
	}
	if keys := strings.Fields(strings.ReplaceAll(fields["gpgkey"], ",", " ")); len(keys) > 0 {
		if rest, ok := strings.CutPrefix(keys[0], "file://"); ok {
			p.signedBy = rest
		} else {
			p.signingKeyURL = keys[0]
		}
	}
	return p, true
}

func isFalseWord(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "0", "no", "false", "off":
		return true
	}
	return false
}

// apkOwner tells the repository line of a managed block from a line of
// someone else that merely follows the marker, for instance because the
// managed line was deleted by hand: a line belongs to the block only if it
// names one of the resource's URIs or carries one of its tags. Updates and
// destroy therefore never replace or remove another repository. A nil
// *apkOwner, used on import, takes any line after the marker.
type apkOwner struct {
	uris, tags map[string]bool
}

// newApkOwner returns the owner of the lines of the given repositories,
// such as the prior and the planned one of an update.
func newApkOwner(specs ...*repoSpec) *apkOwner {
	o := &apkOwner{uris: map[string]bool{}, tags: map[string]bool{}}
	for _, s := range specs {
		if s == nil {
			continue
		}
		for _, u := range s.uris {
			o.uris[u] = true
		}
		if s.tag != "" {
			o.tags[s.tag] = true
		}
	}
	return o
}

// owns reports whether line, which is not a marker, belongs to the block.
func (o *apkOwner) owns(line string) bool {
	if o == nil {
		return true
	}
	_, tag, uri := parseApkLine(line)
	return (uri != "" && o.uris[uri]) || (tag != "" && o.tags[tag])
}

// parseApkLine splits a repository line of /etc/apk/repositories into its
// parts: "#" if disabled, "@tag" and the URL.
func parseApkLine(line string) (enabled bool, tag, uri string) {
	line = strings.TrimSpace(line)
	enabled = true
	if rest, ok := strings.CutPrefix(line, "#"); ok {
		enabled = false
		line = strings.TrimSpace(rest)
	}
	if strings.HasPrefix(line, "@") {
		var rest string
		tag, rest, _ = strings.Cut(line[1:], " ")
		line = strings.TrimSpace(rest)
	}
	return enabled, tag, line
}

// apkBlock locates the managed lines of repository name in the lines of
// /etc/apk/repositories: the marker line and, if owner owns the line after
// it, that repository line. It returns start < 0 if there is no marker, and
// the number of markers for name.
func apkBlock(lines []string, name string, owner *apkOwner) (start, end, count int) {
	start = -1
	for i, l := range lines {
		if n, _, ok := parseApkMarker(l); ok && n == name {
			count++
			if start < 0 {
				start = i
				end = i + 1
				if i+1 < len(lines) {
					if _, _, next := parseApkMarker(lines[i+1]); !next && owner.owns(lines[i+1]) {
						end = i + 2
					}
				}
			}
		}
	}
	return start, end, count
}

// parseApkMarker parses a marker line.
func parseApkMarker(line string) (name, description string, ok bool) {
	rest, ok := strings.CutPrefix(line, apkMarkerPrefix)
	if !ok {
		return "", "", false
	}
	name, description, _ = strings.Cut(rest, ": ")
	name = strings.TrimSpace(name)
	return name, description, name != ""
}

// parseApkRepo parses the managed lines of repository name.
func parseApkRepo(name string, lines []string, owner *apkOwner) (parsedRepo, bool) {
	p := parsedRepo{repoSpec: repoSpec{name: name, enabled: true, gpgCheck: true}}
	start, end, _ := apkBlock(lines, name, owner)
	if start < 0 {
		return p, false
	}
	_, p.description, _ = parseApkMarker(lines[start])
	if end == start+2 {
		var uri string
		p.enabled, p.tag, uri = parseApkLine(lines[start+1])
		if uri != "" {
			p.uris = []string{uri}
		}
	}
	return p, true
}

// setApkBlock replaces or appends the managed lines of repository name in
// t, and removes further blocks of the same name. It reports whether t
// changed.
func setApkBlock(t *textFile, name string, block []string, owner *apkOwner) bool {
	before := string(t.bytes())
	start, end, _ := apkBlock(t.lines, name, owner)
	if start < 0 {
		t.lines = append(t.lines, block...)
	} else {
		t.replace(start, end, block)
		removeApkBlocks(t, name, start+len(block), owner)
	}
	t.trailingNewline = true
	return string(t.bytes()) != before
}

// removeApkBlocks removes all managed lines of repository name from line
// from onwards, and reports whether there were any.
func removeApkBlocks(t *textFile, name string, from int, owner *apkOwner) bool {
	removed := false
	for {
		start, end, _ := apkBlock(t.lines[from:], name, owner)
		if start < 0 {
			return removed
		}
		t.replace(from+start, from+end, nil)
		removed = true
	}
}

// OpenPGP packet tags (RFC 4880, section 4.3).
const (
	pgpSecretKeyTag    = 5
	pgpPublicKeyTag    = 6
	pgpSecretSubkeyTag = 7
)

const (
	pgpArmorBegin = "-----BEGIN PGP PUBLIC KEY BLOCK-----"
	pgpArmorEnd   = "-----END PGP PUBLIC KEY BLOCK-----"
)

// firstPacketTag returns the tag of the OpenPGP packet data starts with.
func firstPacketTag(data []byte) (int, bool) {
	if len(data) == 0 || data[0]&0x80 == 0 {
		return 0, false
	}
	if data[0]&0x40 != 0 {
		return int(data[0] & 0x3f), true // New format.
	}
	return int(data[0]>>2) & 0x0f, true // Old format.
}

// normalizeSigningKey returns key as a single ASCII-armored OpenPGP public
// key block, which both apt (as a .asc keyring) and rpm accept. key is
// either armored already, possibly with CRLF line ends and surrounding
// white space, or binary OpenPGP data as in .gpg keyrings, which is armored
// here.
func normalizeSigningKey(key []byte) ([]byte, error) {
	if len(key) > maxSigningKeySize {
		return nil, fmt.Errorf("signing key is larger than %d bytes", maxSigningKeySize)
	}
	if tag, ok := firstPacketTag(key); ok {
		if tag != pgpPublicKeyTag {
			return nil, fmt.Errorf("signing key is binary OpenPGP data, but starts with a packet of type %d instead of a public key", tag)
		}
		if err := checkNoSecretPackets(key); err != nil {
			return nil, err
		}
		return armorPublicKey(key), nil
	}
	text := strings.TrimSpace(strings.ReplaceAll(string(key), "\r\n", "\n"))
	if !strings.HasPrefix(text, pgpArmorBegin) || !strings.HasSuffix(text, pgpArmorEnd) {
		return nil, fmt.Errorf("signing key must be an OpenPGP public key, either ASCII-armored (starting with %q and ending with %q) or binary", pgpArmorBegin, pgpArmorEnd)
	}
	// Further blocks after the first are kept as they are, so none of them
	// may be a private key.
	if strings.Contains(text, "PRIVATE KEY BLOCK") || strings.Contains(text, "SECRET KEY BLOCK") {
		return nil, errors.New("signing key contains a private key block, which must never be stored in a world-readable file")
	}
	body, err := dearmorBody(text)
	if err != nil {
		return nil, err
	}
	if tag, ok := firstPacketTag(body); !ok || tag != pgpPublicKeyTag {
		return nil, errors.New("signing key does not contain an OpenPGP public key")
	}
	if err := checkNoSecretPackets(body); err != nil {
		return nil, err
	}
	return []byte(text + "\n"), nil
}

// checkNoSecretPackets walks the OpenPGP packets in data and refuses secret
// key material anywhere, not just in the first packet: a public key
// followed by the secret one, as in a concatenated export, would otherwise
// be stored in a world-readable file. Data that does not parse as a
// sequence of packets is refused as well.
func checkNoSecretPackets(data []byte) error {
	for len(data) > 0 {
		tag, n, err := nextPGPPacket(data)
		if err != nil {
			return fmt.Errorf("signing key is not valid OpenPGP data: %w", err)
		}
		if tag == pgpSecretKeyTag || tag == pgpSecretSubkeyTag {
			return errors.New("signing key contains secret key material, which must never be stored in a world-readable file; export the public key only")
		}
		data = data[n:]
	}
	return nil
}

// nextPGPPacket returns the tag and the total length, header included, of
// the OpenPGP packet data starts with (RFC 4880, section 4.2).
func nextPGPPacket(data []byte) (tag, n int, err error) {
	truncated := errors.New("truncated packet")
	if data[0]&0x80 == 0 {
		return 0, 0, errors.New("invalid packet header")
	}
	if data[0]&0x40 == 0 {
		// Old format: the length type is in the two low bits.
		tag = int(data[0]>>2) & 0x0f
		var hdr, length int
		switch data[0] & 3 {
		case 0:
			hdr = 2
			if len(data) >= hdr {
				length = int(data[1])
			}
		case 1:
			hdr = 3
			if len(data) >= hdr {
				length = int(data[1])<<8 | int(data[2])
			}
		case 2:
			hdr = 5
			if len(data) >= hdr {
				length = int(data[1])<<24 | int(data[2])<<16 | int(data[3])<<8 | int(data[4])
			}
		default:
			return tag, len(data), nil // Indeterminate: up to the end.
		}
		if len(data) < hdr || length > len(data)-hdr {
			return 0, 0, truncated
		}
		return tag, hdr + length, nil
	}
	// New format, possibly with partial body lengths.
	tag = int(data[0] & 0x3f)
	pos := 1
	for {
		if pos >= len(data) {
			return 0, 0, truncated
		}
		o := int(data[pos])
		var length int
		partial := false
		switch {
		case o < 192:
			length, pos = o, pos+1
		case o < 224:
			if pos+1 >= len(data) {
				return 0, 0, truncated
			}
			length, pos = (o-192)<<8+int(data[pos+1])+192, pos+2
		case o == 255:
			if pos+4 >= len(data) {
				return 0, 0, truncated
			}
			length = int(data[pos+1])<<24 | int(data[pos+2])<<16 | int(data[pos+3])<<8 | int(data[pos+4])
			pos += 5
		default:
			length, pos, partial = 1<<(o&0x1f), pos+1, true
		}
		if length < 0 || length > len(data)-pos {
			return 0, 0, truncated
		}
		pos += length
		if !partial {
			return tag, pos, nil
		}
	}
}

// dearmorBody decodes the base64 data of an armored block, without
// checking its CRC.
func dearmorBody(text string) ([]byte, error) {
	lines := strings.Split(text, "\n")
	if len(lines) < 3 {
		return nil, errors.New("signing key contains no key data")
	}
	lines = lines[1 : len(lines)-1]
	// Armor headers ("Comment: ...") end with an empty line.
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			lines = lines[i+1:]
			break
		}
		if !strings.Contains(l, ": ") {
			break
		}
	}
	var b64 strings.Builder
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "=") || strings.HasPrefix(l, "-----") {
			break // CRC or the start of a further block.
		}
		b64.WriteString(l)
	}
	data, err := base64.StdEncoding.DecodeString(b64.String())
	if err != nil || len(data) == 0 {
		return nil, errors.New("signing key is not valid ASCII armor: the key data is not valid base64")
	}
	return data, nil
}

// armorPublicKey encodes binary OpenPGP data as an armored public key
// block (RFC 4880, section 6).
func armorPublicKey(data []byte) []byte {
	var b bytes.Buffer
	b.WriteString(pgpArmorBegin + "\n\n")
	enc := base64.StdEncoding.EncodeToString(data)
	for len(enc) > 64 {
		b.WriteString(enc[:64] + "\n")
		enc = enc[64:]
	}
	b.WriteString(enc + "\n")
	crc := crc24(data)
	b.WriteString("=" + base64.StdEncoding.EncodeToString([]byte{byte(crc >> 16), byte(crc >> 8), byte(crc)}) + "\n")
	b.WriteString(pgpArmorEnd + "\n")
	return b.Bytes()
}

// crc24 is the checksum of OpenPGP ASCII armor.
func crc24(data []byte) uint32 {
	const (
		crc24Init = 0xb704ce
		crc24Poly = 0x1864cfb
	)
	crc := uint32(crc24Init)
	for _, b := range data {
		crc ^= uint32(b) << 16
		for i := 0; i < 8; i++ {
			crc <<= 1
			if crc&0x1000000 != 0 {
				crc ^= crc24Poly
			}
		}
	}
	return crc & 0xffffff
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// fetchSigningKey reads the key at rawURL: an https URL, fetched with
// client, or a file URL, read from the host (root_dir does not apply). The
// result is not yet normalized.
func fetchSigningKey(ctx context.Context, client *http.Client, rawURL string) ([]byte, error) {
	u, err := parseRepoURL("signing key URL", rawURL, "https", "file")
	if err != nil {
		return nil, err
	}
	if u.Scheme == "file" {
		// O_NONBLOCK keeps a FIFO from blocking the apply forever; the
		// check below then refuses it.
		f, err := os.OpenFile(u.Path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		info, err := f.Stat()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular file", u.Path)
		}
		return readLimited(f, maxSigningKeySize)
	}

	if client == nil {
		client = &http.Client{}
	}
	// Redirects must not downgrade to plain http; the client's own policy
	// still bounds their number.
	c := *client
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return fmt.Errorf("refusing redirect to non-https URL %s", req.URL.Redacted())
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, signingKeyFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", u.Redacted(), resp.Status)
	}
	return readLimited(resp.Body, maxSigningKeySize)
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("signing key is larger than %d bytes", limit)
	}
	return data, nil
}
