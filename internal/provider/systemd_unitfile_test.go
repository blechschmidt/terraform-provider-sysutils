package provider

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestParseUnitFile(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want unitFile
	}{
		{
			name: "basic",
			in:   "[Unit]\nDescription=Example\n\n[Service]\nExecStart=/bin/a\nExecStart=/bin/b\n",
			want: unitFile{
				{Name: "Unit", Entries: []unitFileEntry{{"Description", "Example"}}},
				{Name: "Service", Entries: []unitFileEntry{{"ExecStart", "/bin/a"}, {"ExecStart", "/bin/b"}}},
			},
		},
		{
			name: "whitespace, comments and CRLF",
			in:   "\xef\xbb\xbf# comment\r\n  [Unit]  \r\n ; other comment\r\n  Description =  spaced value \t\r\nAfter=\r\n",
			want: unitFile{{Name: "Unit", Entries: []unitFileEntry{{"Description", "spaced value"}, {"After", ""}}}},
		},
		{
			name: "continuation lines",
			in:   "[Service]\nExecStart=/bin/echo a \\\n# a comment inside is skipped\n  b \\\n c\nUser=x\n",
			want: unitFile{{Name: "Service", Entries: []unitFileEntry{{"ExecStart", "/bin/echo a    b   c"}, {"User", "x"}}}},
		},
		{
			name: "escaped backslash does not continue",
			in:   "[Service]\nEnvironment=A=b\\\\\nUser=x\n",
			want: unitFile{{Name: "Service", Entries: []unitFileEntry{{"Environment", `A=b\\`}, {"User", "x"}}}},
		},
		{
			name: "continuation at end of file",
			in:   "[Service]\nUser=x\\",
			want: unitFile{{Name: "Service", Entries: []unitFileEntry{{"User", "x"}}}},
		},
		{
			name: "ignored lines",
			in:   "Outside=1\n[Unit]\nno equals sign\n=no key\n[Broken\nInBroken=1\n",
			want: unitFile{{Name: "Unit"}},
		},
		{
			name: "repeated sections merge",
			in:   "[Unit]\nA=1\n[X-Custom]\nK=v\n[Unit]\nB=2\n[Empty]\n",
			want: unitFile{
				{Name: "Unit", Entries: []unitFileEntry{{"A", "1"}, {"B", "2"}}},
				{Name: "X-Custom", Entries: []unitFileEntry{{"K", "v"}}},
				{Name: "Empty"},
			},
		},
		{
			name: "value with equals signs",
			in:   "[Service]\nEnvironment=\"A=1\" B=2\n",
			want: unitFile{{Name: "Service", Entries: []unitFileEntry{{"Environment", `"A=1" B=2`}}}},
		},
		{name: "empty", in: "", want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseUnitFile([]byte(tt.in)); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseUnitFile(%q) =\n%#v\nwant\n%#v", tt.in, got, tt.want)
			}
		})
	}
}

func TestRenderUnitFile(t *testing.T) {
	f := unitFile{
		{Name: "Unit", Entries: []unitFileEntry{{"Description", "x"}}},
		{Name: "Service", Entries: []unitFileEntry{{"ExecStart", ""}, {"ExecStart", "/bin/true"}}},
		{Name: "X-Empty"},
	}
	want := "[Unit]\nDescription=x\n\n[Service]\nExecStart=\nExecStart=/bin/true\n\n[X-Empty]\n"
	if got := string(renderUnitFile(f)); got != want {
		t.Errorf("renderUnitFile() = %q, want %q", got, want)
	}
	if !reflect.DeepEqual(parseUnitFile([]byte(want)), f) {
		t.Errorf("rendered file does not parse back")
	}
}

func TestValidateUnitFileValue(t *testing.T) {
	for _, ok := range []string{"", "x", "/bin/echo a b", `a\\`, `C:\\x\\`, "a\tb", "%n %i", "#not a comment", "[x]", `"quoted"`, "ünïcödé"} {
		if err := validateUnitFileValue(ok); err != nil {
			t.Errorf("validateUnitFileValue(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"a\nb", "a\rb", "a\x00", " x", "x ", "\tx", "x\t", `x\`, `x\\\`} {
		if err := validateUnitFileValue(bad); err == nil {
			t.Errorf("validateUnitFileValue(%q) = nil, want error", bad)
		}
	}
	for _, ok := range []string{"X-Foo", "ExecStart", "a_b-1"} {
		if err := validateUnitFileKey(ok); err != nil {
			t.Errorf("validateUnitFileKey(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-a", "a=b", "a b", "#a", "[a]", "a.b"} {
		if err := validateUnitFileKey(bad); err == nil {
			t.Errorf("validateUnitFileKey(%q) = nil, want error", bad)
		}
	}
	for _, ok := range []string{"X-Custom", "X-Fleet", "Foo.Bar:1"} {
		if err := validateUnitFileSectionName(ok); err != nil {
			t.Errorf("validateUnitFileSectionName(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Service", "Unit", "Install", "Scope", "a]b", "a b", "-x"} {
		if err := validateUnitFileSectionName(bad); err == nil {
			t.Errorf("validateUnitFileSectionName(%q) = nil, want error", bad)
		}
	}
}

// The generated table must map every directive to a unique, well-formed
// attribute name, and know the sections of every unit type.
func TestSystemdDirectiveTable(t *testing.T) {
	attrName := regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)
	for _, s := range unitSectionSpecs {
		ds := systemdDirectives[s.Name]
		if len(ds) == 0 {
			t.Errorf("no directives for [%s]", s.Name)
		}
		seenAttr, seenKey := map[string]bool{}, map[string]bool{}
		for _, d := range ds {
			if !attrName.MatchString(d.Attr) || d.Attr == unitExtraAttr {
				t.Errorf("[%s] %s: bad attribute name %q", s.Name, d.Key, d.Attr)
			}
			if seenAttr[d.Attr] || seenKey[d.Key] {
				t.Errorf("[%s] duplicate directive %s/%s", s.Name, d.Key, d.Attr)
			}
			seenAttr[d.Attr], seenKey[d.Key] = true, true
			if validateUnitFileKey(d.Key) != nil {
				t.Errorf("[%s] directive %q is not a valid key", s.Name, d.Key)
			}
			if d.Until != 0 && d.Until <= d.Since {
				t.Errorf("[%s] %s: until %d <= since %d", s.Name, d.Key, d.Until, d.Since)
			}
		}
	}
	if len(systemdDirectives) != len(unitSectionSpecs) {
		t.Errorf("table has %d sections, specs %d", len(systemdDirectives), len(unitSectionSpecs))
	}
	// Spot checks of the list/scalar classification.
	idx := unitSectionIndex()
	for _, c := range []struct {
		sec, key string
		list     bool
	}{
		{"Unit", "After", true}, {"Unit", "Description", false}, {"Unit", "ConditionPathExists", true},
		{"Service", "ExecStart", true}, {"Service", "Environment", true}, {"Service", "Type", false},
		{"Service", "User", false}, {"Service", "ReadWritePaths", true}, {"Socket", "ListenStream", true},
		{"Timer", "OnCalendar", true}, {"Path", "PathExists", true}, {"Install", "WantedBy", true},
		{"Install", "DefaultInstance", false}, {"Slice", "MemoryMax", false}, {"Scope", "RuntimeMaxSec", false},
		{"Mount", "What", false}, {"Swap", "Priority", false}, {"Automount", "TimeoutIdleSec", false},
	} {
		d, ok := idx[c.sec][c.key]
		if !ok || d.List != c.list {
			t.Errorf("[%s] %s: found=%v list=%v, want list=%v", c.sec, c.key, ok, d.List, c.list)
		}
	}
}

// fullSectionsModel sets every directive of every section in specs: scalars
// to a value derived from the name (or "" for every third one), lists to
// three elements including an empty assignment. Each section also gets
// extra directives, and there are two extra sections.
func fullSectionsModel(t *testing.T, specs []unitSectionSpec, m *unitSectionsModel, scope *types.Object) {
	t.Helper()
	attrTypes := unitSectionAttrTypes()
	secs := m.sections(scope)
	for _, s := range specs {
		vals := map[string]attr.Value{}
		for i, d := range systemdDirectives[s.Name] {
			v := fmt.Sprintf("%s value %d with %%n and \\\\", d.Key, i)
			if i%3 == 0 {
				v = ""
			}
			if d.List {
				vals[d.Attr] = types.ListValueMust(types.StringType, []attr.Value{
					types.StringValue(v), types.StringValue(""), types.StringValue("second " + d.Key),
				})
			} else {
				vals[d.Attr] = types.StringValue(v)
			}
		}
		vals[unitExtraAttr] = types.MapValueMust(unitStringList, map[string]attr.Value{
			"X-Custom": types.ListValueMust(types.StringType, []attr.Value{types.StringValue("a"), types.StringValue("b")}),
			"Zzz":      types.ListValueMust(types.StringType, []attr.Value{types.StringValue("")}),
		})
		*secs[s.Name] = types.ObjectValueMust(attrTypes[s.Name], vals)
	}
	m.ExtraSections = types.MapValueMust(unitExtraType, map[string]attr.Value{
		"X-B": types.MapValueMust(unitStringList, map[string]attr.Value{}),
		"X-A": types.MapValueMust(unitStringList, map[string]attr.Value{
			"Key": types.ListValueMust(types.StringType, []attr.Value{types.StringValue("v")}),
		}),
	})
}

// Every directive of every section survives rendering and parsing back,
// which keeps planned and applied values equal.
func TestUnitFileRoundTrip(t *testing.T) {
	for _, s := range unitSectionSpecs {
		if s.Type == "" {
			continue
		}
		t.Run(s.Type, func(t *testing.T) {
			var specs []unitSectionSpec
			for _, o := range unitSectionSpecs {
				if o.applies(s.Type) {
					specs = append(specs, o)
				}
			}
			var in unitSectionsModel
			var inScope types.Object
			fullSectionsModel(t, specs, &in, &inScope)
			// Sections of other types stay null.
			attrTypes := unitSectionAttrTypes()
			for _, o := range unitSectionSpecs {
				if !o.applies(s.Type) {
					*in.sections(&inScope)[o.Name] = types.ObjectNull(attrTypes[o.Name])
				}
			}
			f, known := unitFileFromModel(unitSectionSpecs, &in, &inScope)
			if !known {
				t.Fatal("model not known")
			}
			data := renderUnitFile(f)
			for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
				if k, v, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(line, "[") {
					if validateUnitFileValue(v) != nil || validateUnitFileKey(k) != nil {
						t.Fatalf("rendered invalid line %q", line)
					}
				}
			}
			var out unitSectionsModel
			var outScope types.Object
			setModelFromUnitFile(unitSectionSpecs, s.Type, parseUnitFile(data), &out, &outScope)
			for name, v := range in.sections(&inScope) {
				if got := *out.sections(&outScope)[name]; !got.Equal(*v) {
					t.Errorf("[%s] did not round-trip:\n got %v\nwant %v", name, got, *v)
				}
			}
			if !out.ExtraSections.Equal(in.ExtraSections) {
				t.Errorf("extra_sections did not round-trip: got %v, want %v", out.ExtraSections, in.ExtraSections)
			}
		})
	}
}

// Sections that do not belong to the unit's type, or that are unknown, are
// reported in extra_sections when a hand-written file is read.
func TestSetModelFromUnitFile_foreignSections(t *testing.T) {
	var m unitSectionsModel
	f := parseUnitFile([]byte("[Unit]\nDescription=d\nX-Foo=1\n[Socket]\nListenStream=80\n[Service]\nUser=a\nUser=b\n[X-Mine]\nK=v\n"))
	setModelFromUnitFile(unitFileSpecs, "service", f, &m, nil)
	if got := m.Unit.Attributes()["description"].(types.String).ValueString(); got != "d" {
		t.Errorf("description = %q", got)
	}
	if got := m.Unit.Attributes()["extra"].String(); !strings.Contains(got, `"X-Foo"`) {
		t.Errorf("unit.extra = %s", got)
	}
	// The last assignment of a single-valued directive wins, as in systemd.
	if got := m.Service.Attributes()["user"].(types.String).ValueString(); got != "b" {
		t.Errorf("user = %q, want b", got)
	}
	if !m.Socket.IsNull() || !m.Install.IsNull() {
		t.Errorf("socket and install should be null: %v %v", m.Socket, m.Install)
	}
	extra := m.ExtraSections.Elements()
	if _, ok := extra["Socket"]; !ok {
		t.Errorf("[Socket] of a service missing from extra_sections: %v", m.ExtraSections)
	}
	if _, ok := extra["X-Mine"]; !ok {
		t.Errorf("[X-Mine] missing from extra_sections: %v", m.ExtraSections)
	}
}

func TestUnitFilesEquivalent(t *testing.T) {
	a := []byte("[Unit]\nDescription=x\n")
	if !unitFilesEquivalent(a, []byte("# comment\n[Unit]\n  Description = x\n\n")) {
		t.Error("formatting changes should be equivalent")
	}
	if unitFilesEquivalent(a, []byte("[Unit]\nDescription=y\n")) {
		t.Error("value change should not be equivalent")
	}
}

func TestValidateDropInNames(t *testing.T) {
	for _, ok := range []string{"service", "scope", "ssh.service", "getty@.service", "getty@tty1.service", "app-.service", "session-1.scope", "dev-sda.device", "user.slice"} {
		if err := validateDropInUnit(ok); err != nil {
			t.Errorf("validateDropInUnit(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "svc", "../x.service", "a/b.service", "x.conf", ".service"} {
		if err := validateDropInUnit(bad); err == nil {
			t.Errorf("validateDropInUnit(%q) = nil, want error", bad)
		}
	}
	for _, ok := range []string{"override", "50-limits", "a.b", "x@y"} {
		if err := validateDropInName(ok); err != nil {
			t.Errorf("validateDropInName(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", ".hidden", "a/b", "..", "a b", strings.Repeat("a", 201)} {
		if err := validateDropInName(bad); err == nil {
			t.Errorf("validateDropInName(%q) = nil, want error", bad)
		}
	}
	for unit, want := range map[string]bool{
		"ssh.service": true, "x.socket": true, "getty@tty1.service": true, "getty@.service": false,
		"service": false, "app-.service": false, "user.slice": false, "s.scope": false, "multi-user.target": false,
	} {
		if got := dropInRestartable(unit); got != want {
			t.Errorf("dropInRestartable(%q) = %v, want %v", unit, got, want)
		}
	}
}
