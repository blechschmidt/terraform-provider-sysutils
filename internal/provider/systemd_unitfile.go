package provider

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/mapvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// This file maps systemd unit files to Terraform attributes. Every section of
// a unit file is a nested attribute named after it ("service" for
// [Service]), and every directive of systemd's own table
// (systemd_directives.go) is an attribute of its section: a string if a later
// assignment replaces an earlier one, a list of strings if the directive may
// be repeated. Directives and sections that are not in the table go into
// "extra" and "extra_sections".
//
// The mapping is lossless for the files the provider writes:
// parseUnitFile(renderUnitFile(f)) == f for every f whose values pass
// validateUnitFileValue. That is what keeps planned and applied values equal.

// unitDirective describes one directive of a unit file section.
type unitDirective struct {
	Key  string // Directive name, such as "ExecStart".
	Attr string // Attribute name, such as "exec_start".
	// List is true for directives that may be repeated; each assignment adds
	// to the earlier ones.
	List bool
	// Since is the first systemd version that knows the directive, or 0 if
	// it predates the oldest version in the table. Until is the first
	// version that ignores it, or 0 if current versions still support it.
	Since, Until int
	// Context names the man page of shared directives: "exec", "kill" or
	// "resource-control". It is empty for directives of the section's own
	// man page.
	Context string
}

// unitSectionSpec describes a section of a unit file.
type unitSectionSpec struct {
	Name string // Section name, such as "Service".
	Attr string // Attribute name, such as "service".
	// Type is the unit type the section belongs to, or "" for sections that
	// every unit may have.
	Type string
	// Man is the man page that documents the section's own directives.
	Man string
}

// unitSectionSpecs lists the sections in the order they are written.
var unitSectionSpecs = []unitSectionSpec{
	{Name: "Unit", Attr: "unit", Man: "systemd.unit"},
	{Name: "Service", Attr: "service", Type: "service", Man: "systemd.service"},
	{Name: "Socket", Attr: "socket", Type: "socket", Man: "systemd.socket"},
	{Name: "Mount", Attr: "mount", Type: "mount", Man: "systemd.mount"},
	{Name: "Automount", Attr: "automount", Type: "automount", Man: "systemd.automount"},
	{Name: "Swap", Attr: "swap", Type: "swap", Man: "systemd.swap"},
	{Name: "Timer", Attr: "timer", Type: "timer", Man: "systemd.timer"},
	// "path" is the path of the unit file itself.
	{Name: "Path", Attr: "path_section", Type: "path", Man: "systemd.path"},
	{Name: "Slice", Attr: "slice", Type: "slice", Man: "systemd.slice"},
	{Name: "Scope", Attr: "scope", Type: "scope", Man: "systemd.scope"},
	{Name: "Install", Attr: "install", Man: "systemd.unit"},
}

// unitFileSpecs are the sections of unit files. Scope units cannot be
// defined by unit files, only configured by drop-ins.
var unitFileSpecs = withoutSection(unitSectionSpecs, "Scope")

func withoutSection(specs []unitSectionSpec, name string) []unitSectionSpec {
	var out []unitSectionSpec
	for _, s := range specs {
		if s.Name != name {
			out = append(out, s)
		}
	}
	return out
}

// applies reports whether the section can appear in a unit of type typ.
func (s unitSectionSpec) applies(typ string) bool {
	return s.Type == "" || s.Type == typ
}

const (
	// unitExtraAttr is the attribute of every section for directives that
	// are not in the table.
	unitExtraAttr = "extra"
	// unitExtraSectionsAttr is the attribute for sections that are not in
	// the table, or that do not belong to the unit's type.
	unitExtraSectionsAttr = "extra_sections"
)

var unitSectionIndex = sync.OnceValue(func() map[string]map[string]unitDirective {
	idx := map[string]map[string]unitDirective{}
	for sec, ds := range systemdDirectives {
		idx[sec] = map[string]unitDirective{}
		for _, d := range ds {
			idx[sec][d.Key] = d
		}
	}
	return idx
})

// knownUnitSection reports whether name is a section in the table.
func knownUnitSection(name string) bool {
	for _, s := range unitSectionSpecs {
		if s.Name == name {
			return true
		}
	}
	return false
}

// unitTypeOf returns the type of a unit name or drop-in target, such as
// "service" for "app.service", "app@.service" and "service".
func unitTypeOf(name string) string {
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		return name[i+1:]
	}
	return name
}

// unitFileEntry is one "Key=Value" assignment.
type unitFileEntry struct {
	Key, Value string
}

// unitFileSection is a section and its assignments in file order.
type unitFileSection struct {
	Name    string
	Entries []unitFileEntry
}

// unitFile is the parsed form of a unit file. Sections appear in the order
// of their first header; a section that occurs several times is merged.
type unitFile []unitFileSection

func (f *unitFile) section(name string) *unitFileSection {
	for i := range *f {
		if (*f)[i].Name == name {
			return &(*f)[i]
		}
	}
	*f = append(*f, unitFileSection{Name: name})
	return &(*f)[len(*f)-1]
}

// parseUnitFile parses a unit file the way systemd's config_parse does:
// whitespace around lines, keys and values is stripped, lines starting with
// "#" or ";" are comments, a line ending in an unescaped backslash continues
// on the next line, and assignments outside a section or without "=" are
// ignored.
func parseUnitFile(data []byte) unitFile {
	var f unitFile
	var cur *unitFileSection
	var continuation *string
	handle := func(l string) {
		l = strings.Trim(l, " \t\n\r")
		if l == "" {
			return
		}
		if l[0] == '[' {
			cur = nil
			if l[len(l)-1] == ']' {
				cur = f.section(l[1 : len(l)-1])
			}
			return
		}
		key, value, ok := strings.Cut(l, "=")
		key = strings.Trim(key, " \t\n\r")
		if cur == nil || !ok || key == "" {
			return
		}
		cur.Entries = append(cur.Entries, unitFileEntry{Key: key, Value: strings.Trim(value, " \t\n\r")})
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	for _, line := range splitUnitFileLines(string(data)) {
		if t := strings.TrimLeft(line, " \t\n\r"); t != "" && (t[0] == '#' || t[0] == ';') {
			continue
		}
		p := line
		if continuation != nil {
			p = *continuation + line
		}
		if endsWithUnescapedBackslash(p) {
			p = p[:len(p)-1] + " "
			continuation = &p
			continue
		}
		continuation = nil
		handle(p)
	}
	if continuation != nil {
		handle(*continuation)
	}
	return f
}

// splitUnitFileLines splits data into lines terminated by "\n", "\r\n",
// "\r" or NUL, like systemd's read_line.
func splitUnitFileLines(data string) []string {
	var lines []string
	for data != "" {
		i := strings.IndexAny(data, "\n\r\x00")
		if i < 0 {
			lines = append(lines, data)
			break
		}
		lines = append(lines, data[:i])
		if data[i] == '\r' && i+1 < len(data) && data[i+1] == '\n' {
			i++
		}
		data = data[i+1:]
	}
	return lines
}

// endsWithUnescapedBackslash reports whether s ends in a backslash that is
// not itself escaped by a backslash, which makes systemd continue the line.
func endsWithUnescapedBackslash(s string) bool {
	escaped := false
	for i := 0; i < len(s); i++ {
		if escaped {
			escaped = false
		} else if s[i] == '\\' {
			escaped = true
		}
	}
	return escaped
}

// renderUnitFile writes f in the canonical form: one "Key=Value" line per
// assignment and an empty line between sections.
func renderUnitFile(f unitFile) []byte {
	var b bytes.Buffer
	for i, s := range f {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "[%s]\n", s.Name)
		for _, e := range s.Entries {
			fmt.Fprintf(&b, "%s=%s\n", e.Key, e.Value)
		}
	}
	return b.Bytes()
}

// validateUnitFileValue reports why v cannot be written as the value of an
// assignment such that systemd reads back exactly v.
func validateUnitFileValue(v string) error {
	if strings.ContainsAny(v, "\n\r\x00") {
		return fmt.Errorf("value %q must not contain line breaks or NUL characters", v)
	}
	if strings.Trim(v, " \t") != v {
		return fmt.Errorf("value %q must not start or end with whitespace, which systemd strips", v)
	}
	if endsWithUnescapedBackslash(v) {
		return fmt.Errorf("value %q must not end with an odd number of backslashes, which makes systemd continue the line", v)
	}
	return nil
}

var (
	unitFileKeyPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
	unitFileSectionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]*$`)
)

func validateUnitFileKey(k string) error {
	if !unitFileKeyPattern.MatchString(k) {
		return fmt.Errorf("directive name %q must consist of ASCII letters, digits, \"_\" and \"-\", starting with a letter or digit", k)
	}
	return nil
}

func validateUnitFileSectionName(s string) error {
	if !unitFileSectionPattern.MatchString(s) {
		return fmt.Errorf("section name %q must consist of ASCII letters, digits, \"_\", \".\", \":\" and \"-\", starting with a letter or digit", s)
	}
	if knownUnitSection(s) {
		return fmt.Errorf("section %q has its own attribute; use that instead of %s", s, unitExtraSectionsAttr)
	}
	return nil
}

func unitFileValue() validator.String {
	return stringCheck("unit file value", validateUnitFileValue)
}

func unitFileKey() validator.String {
	return stringCheck("unit file directive name", validateUnitFileKey)
}

func unitFileSectionName() validator.String {
	return stringCheck("unit file section name", validateUnitFileSectionName)
}

var (
	unitStringList = types.ListType{ElemType: types.StringType}
	unitExtraType  = types.MapType{ElemType: unitStringList}
)

// unitSectionAttrTypes returns the object type of a section attribute.
var unitSectionAttrTypes = sync.OnceValue(func() map[string]map[string]attr.Type {
	out := map[string]map[string]attr.Type{}
	for _, s := range unitSectionSpecs {
		t := map[string]attr.Type{unitExtraAttr: unitExtraType}
		for _, d := range systemdDirectives[s.Name] {
			if d.List {
				t[d.Attr] = unitStringList
			} else {
				t[d.Attr] = types.StringType
			}
		}
		out[s.Name] = t
	}
	return out
})

func unitDirectiveURL(s unitSectionSpec, d unitDirective) string {
	man := s.Man
	if d.Context != "" {
		man = "systemd." + d.Context
	}
	return fmt.Sprintf("https://www.freedesktop.org/software/systemd/man/latest/%s.html#%s=", man, d.Key)
}

func unitDirectiveDescription(s unitSectionSpec, d unitDirective) string {
	desc := fmt.Sprintf("[`%s=`](%s)", d.Key, unitDirectiveURL(s, d))
	if d.List {
		desc += ", one line per element"
	}
	desc += "."
	if d.Since > 0 {
		desc += fmt.Sprintf(" Requires systemd %d or later.", d.Since)
	}
	if d.Until > 0 {
		desc += fmt.Sprintf(" Ignored by systemd %d and later.", d.Until)
	}
	return desc
}

// unitSectionSchema returns the schema attributes of the given sections and
// of extra_sections. what is "unit" or "drop-in", for the descriptions.
func unitSectionSchema(specs []unitSectionSpec, what string) map[string]schema.Attribute {
	valueList := []validator.List{listvalidator.SizeAtLeast(1), listvalidator.NoNullValues(), listvalidator.ValueStringsAre(unitFileValue())}
	extraValidators := []validator.Map{
		mapvalidator.SizeAtLeast(1),
		mapvalidator.NoNullValues(),
		mapvalidator.KeysAre(unitFileKey()),
		mapvalidator.ValueListsAre(valueList...),
	}
	out := map[string]schema.Attribute{}
	for _, s := range specs {
		attrs := map[string]schema.Attribute{
			unitExtraAttr: schema.MapAttribute{
				Optional:    true,
				ElementType: unitStringList,
				MarkdownDescription: fmt.Sprintf("Directives of `[%s]` that have no attribute of their own, such as `X-` directives or directives of systemd versions newer than this provider, by name. "+
					"Each element of a list becomes one line. They are written after the other directives, sorted by name.", s.Name),
				Validators: extraValidators,
			},
		}
		for _, d := range systemdDirectives[s.Name] {
			if d.List {
				attrs[d.Attr] = schema.ListAttribute{
					Optional:            true,
					ElementType:         types.StringType,
					MarkdownDescription: unitDirectiveDescription(s, d),
					Validators:          valueList,
				}
			} else {
				attrs[d.Attr] = schema.StringAttribute{
					Optional:            true,
					MarkdownDescription: unitDirectiveDescription(s, d),
					Validators:          []validator.String{unitFileValue()},
				}
			}
		}
		applies := "Any " + what + " may have it."
		if s.Type != "" {
			applies = fmt.Sprintf("Only for `.%s` units.", s.Type)
		}
		out[s.Attr] = schema.SingleNestedAttribute{
			Optional: true,
			Computed: true,
			MarkdownDescription: fmt.Sprintf("The `[%s]` section, see [%s(5)](https://www.freedesktop.org/software/systemd/man/latest/%s.html). %s "+
				"Every directive is an attribute named in snake case; directives that may be repeated are lists. "+
				"An empty string writes an empty assignment (`Directive=`), which resets the directive to its default or empties its list. "+
				"When the %s is configured with `content` or `source`, or was imported, the section as read from the file is reported here.",
				s.Name, s.Man, s.Man, applies, what),
			Attributes: attrs,
		}
	}
	out[unitExtraSectionsAttr] = schema.MapAttribute{
		Optional:    true,
		Computed:    true,
		ElementType: unitExtraType,
		MarkdownDescription: "Further sections, by name, each a map of directive names to lists of values. " +
			"Use it for custom `X-` sections, which systemd ignores. The sections above have their own attributes and are not allowed here. " +
			"Sections are written after the others, sorted by name, and their directives sorted by name.",
		Validators: []validator.Map{
			mapvalidator.NoNullValues(),
			mapvalidator.KeysAre(unitFileSectionName()),
			mapvalidator.ValueMapsAre(mapvalidator.NoNullValues(), mapvalidator.KeysAre(unitFileKey()), mapvalidator.ValueListsAre(valueList...)),
		},
	}
	return out
}

// unitSectionsModel holds the section attributes shared by
// sysutils_systemd_unit and sysutils_systemd_dropin.
type unitSectionsModel struct {
	Unit          types.Object `tfsdk:"unit"`
	Service       types.Object `tfsdk:"service"`
	Socket        types.Object `tfsdk:"socket"`
	Mount         types.Object `tfsdk:"mount"`
	Automount     types.Object `tfsdk:"automount"`
	Swap          types.Object `tfsdk:"swap"`
	Timer         types.Object `tfsdk:"timer"`
	Path          types.Object `tfsdk:"path_section"`
	Slice         types.Object `tfsdk:"slice"`
	Install       types.Object `tfsdk:"install"`
	ExtraSections types.Map    `tfsdk:"extra_sections"`
}

// sections returns pointers to the section fields by section name. scope is
// the [Scope] field of drop-ins, or nil.
func (m *unitSectionsModel) sections(scope *types.Object) map[string]*types.Object {
	out := map[string]*types.Object{
		"Unit": &m.Unit, "Service": &m.Service, "Socket": &m.Socket, "Mount": &m.Mount,
		"Automount": &m.Automount, "Swap": &m.Swap, "Timer": &m.Timer, "Path": &m.Path,
		"Slice": &m.Slice, "Install": &m.Install,
	}
	if scope != nil {
		out["Scope"] = scope
	}
	return out
}

// configured reports whether any section attribute is set (or unknown).
func (m *unitSectionsModel) configured(scope *types.Object) bool {
	for _, v := range m.sections(scope) {
		if !v.IsNull() {
			return true
		}
	}
	return !m.ExtraSections.IsNull()
}

// valueFullyKnown reports whether v and everything inside it is known.
func valueFullyKnown(v attr.Value) bool {
	if v.IsUnknown() {
		return false
	}
	switch v := v.(type) {
	case types.Object:
		for _, a := range v.Attributes() {
			if !valueFullyKnown(a) {
				return false
			}
		}
	case types.List:
		for _, e := range v.Elements() {
			if !valueFullyKnown(e) {
				return false
			}
		}
	case types.Map:
		for _, e := range v.Elements() {
			if !valueFullyKnown(e) {
				return false
			}
		}
	}
	return true
}

// unitFileFromModel builds the unit file that the section attributes of m
// describe. known is false if any value is unknown.
func unitFileFromModel(specs []unitSectionSpec, m *unitSectionsModel, scope *types.Object) (f unitFile, known bool) {
	secs := m.sections(scope)
	for _, s := range specs {
		obj := *secs[s.Name]
		if !valueFullyKnown(obj) {
			return nil, false
		}
		if obj.IsNull() {
			continue
		}
		attrs := obj.Attributes()
		sec := unitFileSection{Name: s.Name}
		for _, d := range systemdDirectives[s.Name] {
			switch v := attrs[d.Attr].(type) {
			case types.String:
				if !v.IsNull() {
					sec.Entries = append(sec.Entries, unitFileEntry{Key: d.Key, Value: v.ValueString()})
				}
			case types.List:
				for _, e := range v.Elements() {
					sec.Entries = append(sec.Entries, unitFileEntry{Key: d.Key, Value: e.(types.String).ValueString()})
				}
			}
		}
		if extra, ok := attrs[unitExtraAttr].(types.Map); ok {
			sec.Entries = append(sec.Entries, extraEntries(extra)...)
		}
		f = append(f, sec)
	}
	if !valueFullyKnown(m.ExtraSections) {
		return nil, false
	}
	elems := m.ExtraSections.Elements()
	for _, name := range sortedKeys(elems) {
		f = append(f, unitFileSection{Name: name, Entries: extraEntries(elems[name].(types.Map))})
	}
	return f, true
}

// extraEntries returns the assignments of a map of directive names to
// lists of values, sorted by name.
func extraEntries(m types.Map) []unitFileEntry {
	var out []unitFileEntry
	elems := m.Elements()
	for _, key := range sortedKeys(elems) {
		for _, e := range elems[key].(types.List).Elements() {
			out = append(out, unitFileEntry{Key: key, Value: e.(types.String).ValueString()})
		}
	}
	return out
}

// setModelFromUnitFile sets the section attributes of m from f, as read by
// systemd for a unit of type typ. Sections that do not belong to typ, or
// that are not in the table, go into extra_sections. A scalar directive that
// is assigned several times keeps the last value, as in systemd.
func setModelFromUnitFile(specs []unitSectionSpec, typ string, f unitFile, m *unitSectionsModel, scope *types.Object) {
	attrTypes := unitSectionAttrTypes()
	idx := unitSectionIndex()
	secs := m.sections(scope)
	typed := map[string]bool{}
	for _, s := range specs {
		if s.applies(typ) {
			typed[s.Name] = true
		}
		*secs[s.Name] = types.ObjectNull(attrTypes[s.Name])
	}
	extraSections := map[string]attr.Value{}
	for _, sec := range f {
		if !typed[sec.Name] {
			extraSections[sec.Name] = extraMapValue(sec.Entries)
			continue
		}
		scalars := map[string]attr.Value{}
		lists := map[string][]attr.Value{}
		var extra []unitFileEntry
		for _, e := range sec.Entries {
			d, ok := idx[sec.Name][e.Key]
			switch {
			case !ok:
				extra = append(extra, e)
			case d.List:
				lists[d.Attr] = append(lists[d.Attr], types.StringValue(e.Value))
			default:
				scalars[d.Attr] = types.StringValue(e.Value)
			}
		}
		vals := map[string]attr.Value{}
		for name, t := range attrTypes[sec.Name] {
			switch {
			case name == unitExtraAttr:
				vals[name] = types.MapNull(unitStringList)
				if len(extra) > 0 {
					vals[name] = extraMapValue(extra)
				}
			case t == unitStringList:
				vals[name] = types.ListNull(types.StringType)
				if l, ok := lists[name]; ok {
					vals[name] = types.ListValueMust(types.StringType, l)
				}
			default:
				vals[name] = types.StringNull()
				if v, ok := scalars[name]; ok {
					vals[name] = v
				}
			}
		}
		*secs[sec.Name] = types.ObjectValueMust(attrTypes[sec.Name], vals)
	}
	m.ExtraSections = types.MapNull(unitExtraType)
	if len(extraSections) > 0 {
		m.ExtraSections = types.MapValueMust(unitExtraType, extraSections)
	}
}

// extraMapValue groups assignments by key into a map of lists.
func extraMapValue(entries []unitFileEntry) types.Map {
	grouped := map[string][]attr.Value{}
	for _, e := range entries {
		grouped[e.Key] = append(grouped[e.Key], types.StringValue(e.Value))
	}
	vals := map[string]attr.Value{}
	for k, l := range grouped {
		vals[k] = types.ListValueMust(types.StringType, l)
	}
	return types.MapValueMust(unitStringList, vals)
}

// validateUnitSections checks what the attribute validators cannot: that
// type-specific sections match the unit type typ (if known), and that
// "extra" does not repeat directives that have an attribute.
func validateUnitSections(specs []unitSectionSpec, typ string, m *unitSectionsModel, scope *types.Object) diag.Diagnostics {
	var diags diag.Diagnostics
	idx := unitSectionIndex()
	secs := m.sections(scope)
	for _, s := range specs {
		obj := *secs[s.Name]
		if obj.IsNull() || obj.IsUnknown() {
			continue
		}
		if typ != "" && !s.applies(typ) {
			diags.AddAttributeError(path.Root(s.Attr), "Section does not match the unit type",
				fmt.Sprintf("The [%s] section is only valid for .%s units, not for .%s units.", s.Name, s.Type, typ))
			continue
		}
		extra, ok := obj.Attributes()[unitExtraAttr].(types.Map)
		if !ok || extra.IsUnknown() {
			continue
		}
		for key := range extra.Elements() {
			if d, known := idx[s.Name][key]; known {
				diags.AddAttributeError(path.Root(s.Attr).AtName(unitExtraAttr).AtMapKey(key), "Directive has its own attribute",
					fmt.Sprintf("Set %s= with the attribute %s.%s instead of %s.", key, s.Attr, d.Attr, unitExtraAttr))
			}
		}
	}
	return diags
}

// unitContentModel holds the attributes that define the contents of a unit
// file or drop-in: exactly one of content, source or the sections.
type unitContentModel struct {
	Content       types.String `tfsdk:"content"`
	Source        types.String `tfsdk:"source"`
	ContentSHA256 types.String `tfsdk:"content_sha256"`
	unitSectionsModel
}

// unitContentSchema returns the schema attributes of unitContentModel.
func unitContentSchema(specs []unitSectionSpec, what string) map[string]schema.Attribute {
	attrs := unitSectionSchema(specs, what)
	attrs["content"] = schema.StringAttribute{
		Optional: true,
		Computed: true,
		MarkdownDescription: fmt.Sprintf("Contents of the %[1]s file. Exactly one of `content`, `source` and the section attributes (`unit`, `service`, ..., `install`, `extra_sections`) must be set. "+
			"When the %[1]s is configured with `source` or the section attributes, this reports the file the provider writes, so that the plan shows a diff of the file.", what),
	}
	attrs["source"] = schema.StringAttribute{
		Optional: true,
		MarkdownDescription: fmt.Sprintf("Path to a local file whose contents are used as the %s file. Exactly one of `content`, `source` and the section attributes must be set. "+
			"Relative paths are resolved against Terraform's working directory; prefer `${path.module}/...`. "+
			"The source is read during every plan, so a change to its contents plans an update.", what),
		Validators: []validator.String{stringvalidator.LengthAtLeast(1)},
	}
	attrs["content_sha256"] = schema.StringAttribute{
		Computed:            true,
		MarkdownDescription: fmt.Sprintf("Hex-encoded SHA-256 checksum of the %s file. Known at plan time, so other resources can use it to react to changes.", what),
	}
	return attrs
}

// validateUnitContentConfig checks that exactly one way of defining the
// contents is used, and validates the sections for a unit of type typ ("" if
// not known yet).
func validateUnitContentConfig(specs []unitSectionSpec, typ string, m *unitContentModel, scope *types.Object) diag.Diagnostics {
	var diags diag.Diagnostics
	n := 0
	for _, set := range []bool{!m.Content.IsNull(), !m.Source.IsNull(), m.configured(scope)} {
		if set {
			n++
		}
	}
	if n != 1 {
		diags.AddError("Invalid unit file definition",
			"Exactly one of content, source and the section attributes (unit, install, the section of the unit's type, and extra_sections) must be set.")
		return diags
	}
	diags.Append(validateUnitSections(specs, typ, &m.unitSectionsModel, scope)...)
	return diags
}

// desiredUnitBytes returns the file contents configured in m. known is false
// if they depend on unknown values or, while planning, on a source file that
// does not exist yet.
func desiredUnitBytes(specs []unitSectionSpec, m *unitContentModel, scope *types.Object, planning bool) (data []byte, known bool, err error) {
	switch {
	case !m.Content.IsNull():
		if m.Content.IsUnknown() {
			return nil, false, nil
		}
		return []byte(m.Content.ValueString()), true, nil
	case !m.Source.IsNull():
		if m.Source.IsUnknown() {
			return nil, false, nil
		}
		src, err := openSource(m.Source.ValueString())
		if planning && errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		defer func() { _ = src.Close() }()
		data, err := io.ReadAll(io.LimitReader(src, maxUnitFileSize+1))
		if err != nil {
			return nil, false, fmt.Errorf("reading source: %w", err)
		}
		if len(data) > maxUnitFileSize {
			return nil, false, fmt.Errorf("source %q is larger than %d bytes", m.Source.ValueString(), maxUnitFileSize)
		}
		return data, true, nil
	default:
		f, known := unitFileFromModel(specs, &m.unitSectionsModel, scope)
		if !known {
			return nil, false, nil
		}
		return renderUnitFile(f), true, nil
	}
}

// planUnitContent sets content, content_sha256 and the sections of plan
// from config, for a unit of type typ ("" if the name is not known yet).
// Configured sections are kept as they are; the others are derived from the
// file that will be written, so they are known at plan time whenever the
// file is.
func planUnitContent(specs []unitSectionSpec, typ string, config *unitContentModel, cfgScope *types.Object, plan *unitContentModel, planScope *types.Object) error {
	data, known, err := desiredUnitBytes(specs, config, cfgScope, true)
	if err != nil {
		return err
	}
	structured := config.configured(cfgScope)
	switch {
	case structured:
		plan.unitSectionsModel = config.unitSectionsModel
		if planScope != nil {
			*planScope = *cfgScope
		}
	case known && typ != "":
		setModelFromUnitFile(specs, typ, parseUnitFile(data), &plan.unitSectionsModel, planScope)
	default:
		setUnitSectionsUnknown(specs, &plan.unitSectionsModel, planScope)
	}
	plan.ContentSHA256 = types.StringUnknown()
	if known {
		sum := sha256.Sum256(data)
		plan.ContentSHA256 = types.StringValue(hex.EncodeToString(sum[:]))
	}
	switch {
	case !config.Content.IsNull():
		plan.Content = config.Content
	case !known:
		plan.Content = types.StringUnknown()
	case utf8.Valid(data):
		plan.Content = types.StringValue(string(data))
	default:
		plan.Content = types.StringNull()
	}
	return nil
}

func setUnitSectionsUnknown(specs []unitSectionSpec, m *unitSectionsModel, scope *types.Object) {
	attrTypes := unitSectionAttrTypes()
	secs := m.sections(scope)
	for _, s := range specs {
		*secs[s.Name] = types.ObjectUnknown(attrTypes[s.Name])
	}
	m.ExtraSections = types.MapUnknown(unitExtraType)
}

// refreshUnitContent sets content, content_sha256 and the sections of m
// from the file contents data of a unit of type typ.
func refreshUnitContent(specs []unitSectionSpec, typ string, data []byte, m *unitContentModel, scope *types.Object) {
	sum := sha256.Sum256(data)
	m.ContentSHA256 = types.StringValue(hex.EncodeToString(sum[:]))
	m.Content = types.StringNull()
	if utf8.Valid(data) {
		m.Content = types.StringValue(string(data))
	}
	setModelFromUnitFile(specs, typ, parseUnitFile(data), &m.unitSectionsModel, scope)
}

// unitContentAttribute returns the path of the attribute that defines the
// contents in m, for attaching diagnostics.
func unitContentAttribute(m *unitContentModel) path.Path {
	switch {
	case !m.Source.IsNull():
		return path.Root("source")
	default:
		return path.Root("content")
	}
}

// unitFilesEquivalent reports whether systemd reads a and b the same way,
// that is, whether they differ at most in comments, blank lines, whitespace
// and line continuations.
func unitFilesEquivalent(a, b []byte) bool {
	return bytes.Equal(renderUnitFile(parseUnitFile(a)), renderUnitFile(parseUnitFile(b)))
}
