package provider

import (
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/zclconf/go-cty/cty"
)

func testVars() map[string]cty.Value {
	return map[string]cty.Value{
		"name":    cty.StringVal("web"),
		"port":    cty.NumberIntVal(8080),
		"ratio":   cty.NumberFloatVal(0.5),
		"enabled": cty.True,
		"hosts":   cty.TupleVal([]cty.Value{cty.StringVal("a"), cty.StringVal("b")}),
		"db":      cty.ObjectVal(map[string]cty.Value{"user": cty.StringVal("app")}),
		"empty":   cty.StringVal(""),
		"nothing": cty.NullVal(cty.DynamicPseudoType),
	}
}

func TestRenderGoTemplate(t *testing.T) {
	cases := []struct {
		src, want string
	}{
		{"plain\n", "plain\n"},
		{"{{ .name }}:{{ .port }}", "web:8080"},
		{"{{ .ratio }}", "0.5"},
		{"{{ if .enabled }}on{{ end }}", "on"},
		{"{{ range .hosts }}[{{ . }}]{{ end }}", "[a][b]"},
		{"{{ .db.user }}", "app"},
		{`{{ join "," .hosts }}`, "a,b"},
		{`{{ .name | upper | replace "W" "V" }}`, "VEB"},
		{`{{ .empty | default "x" }}`, "x"},
		{`{{ .nothing | default "x" }}`, "x"},
		{`{{ indent 2 "a\nb" }}`, "  a\n  b"},
		{`{{ indent .port "" }}`, ""},
		{`{{ .db | toJson }}`, `{"user":"app"}`},
		{`{{ "hi" | b64enc | b64dec }}`, "hi"},
		{`{{ range keys .db }}{{ . }}{{ end }}`, "user"},
		{`{{ eq .port 8080 }}`, "true"},
		{`{{ quote .name }}`, `"web"`},
		{`{{ split "," "x,y" | len }}`, "2"},
	}
	for _, c := range cases {
		got, err := renderTemplate(syntaxGo, c.src, testVars())
		if err != nil {
			t.Errorf("%q: unexpected error: %v", c.src, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q: got %q, want %q", c.src, got, c.want)
		}
	}
}

func TestRenderTerraformTemplate(t *testing.T) {
	cases := []struct {
		src, want string
	}{
		{"plain\n", "plain\n"},
		{"${name}:${port}", "web:8080"},
		{"%{ if enabled }on%{ endif }", "on"},
		{"%{ for h in hosts }[${h}]%{ endfor }", "[a][b]"},
		{"${db.user}", "app"},
		{`${join(",", hosts)}`, "a,b"},
		{`${upper(replace(name, "/^w/", "v"))}`, "VEB"},
		{`${jsonencode(db)}`, `{"user":"app"}`},
		{`${base64decode(base64encode("hi"))}`, "hi"},
		{`${startswith(name, "we")}`, "true"},
		{`${length(range(3))}`, "3"},
		{`${try(db.missing, "fallback")}`, "fallback"},
		{"$${literal}", "${literal}"},
	}
	for _, c := range cases {
		got, err := renderTemplate(syntaxTerraform, c.src, testVars())
		if err != nil {
			t.Errorf("%q: unexpected error: %v", c.src, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q: got %q, want %q", c.src, got, c.want)
		}
	}
}

func TestParseTemplateErrorPosition(t *testing.T) {
	cases := []struct {
		syntax, src    string
		line, column   int
		msgContains    string
		executionError bool
	}{
		{syntax: syntaxGo, src: "ok\n{{ .name }\n", line: 2, msgContains: "unexpected"},
		{syntax: syntaxGo, src: "{{ if .x }}", line: 1, msgContains: "unexpected EOF"},
		{syntax: syntaxGo, src: "{{ nosuchfunc 1 }}", line: 1, msgContains: `function "nosuchfunc" not defined`},
		{syntax: syntaxTerraform, src: "a\nb ${name + }\n", line: 2, column: 12, msgContains: "Invalid expression"},
		{syntax: syntaxTerraform, src: "%{ if x }", line: 1},
	}
	for _, c := range cases {
		err := parseTemplate(c.syntax, c.src)
		var te *templateError
		if !errors.As(err, &te) {
			t.Errorf("%s %q: expected a *templateError, got %v", c.syntax, c.src, err)
			continue
		}
		if te.Line != c.line || (c.column != 0 && te.Column != c.column) {
			t.Errorf("%s %q: got position %d:%d, want %d:%d (%v)", c.syntax, c.src, te.Line, te.Column, c.line, c.column, err)
		}
		if !strings.Contains(te.Msg, c.msgContains) {
			t.Errorf("%s %q: message %q does not contain %q", c.syntax, c.src, te.Msg, c.msgContains)
		}
		if strings.HasPrefix(te.Msg, "template:") {
			t.Errorf("%s %q: message %q still has the raw prefix", c.syntax, c.src, te.Msg)
		}
	}
}

func TestRenderTemplateErrors(t *testing.T) {
	cases := []struct {
		syntax, src, want string
	}{
		{syntaxGo, "x\n{{ .missing }}", `line 2, column 3: <.missing>: map has no entry for key "missing"`},
		{syntaxGo, `{{ repeat 100000000 "0123456789" }}`, "exceeds"},
		{syntaxTerraform, "x\n${missing}", `line 2, column 3: vars does not contain "missing"`},
		{syntaxTerraform, "${hosts}", "cannot be converted to a string"},
		{syntaxTerraform, `${file("/etc/passwd")}`, "Call to unknown function"},
		{syntaxTerraform, `${timestamp()}`, "Call to unknown function"},
		{syntaxGo, `{{ env "HOME" }}`, `function "env" not defined`},
	}
	for _, c := range cases {
		_, err := renderTemplate(c.syntax, c.src, testVars())
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %q: got error %v, want one containing %q", c.syntax, c.src, err, c.want)
		}
	}
}

func TestRenderGoTemplateOutputLimit(t *testing.T) {
	vars := map[string]cty.Value{"s": cty.StringVal(strings.Repeat("x", 1<<20))}
	_, err := renderTemplate(syntaxGo, "{{ range $i := 20 }}{{ $.s }}{{ end }}", vars)
	if !errors.Is(err, errRenderedTooLarge) {
		t.Fatalf("expected errRenderedTooLarge, got %v", err)
	}
}

func TestTemplateVars(t *testing.T) {
	obj := tftypes.NewValue(tftypes.Object{AttributeTypes: map[string]tftypes.Type{
		"s": tftypes.String,
		"n": tftypes.Number,
		"l": tftypes.List{ElementType: tftypes.String},
		"m": tftypes.Map{ElementType: tftypes.Bool},
	}}, map[string]tftypes.Value{
		"s": tftypes.NewValue(tftypes.String, "x"),
		"n": tftypes.NewValue(tftypes.Number, big.NewFloat(1.5)),
		"l": tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, []tftypes.Value{tftypes.NewValue(tftypes.String, "a")}),
		"m": tftypes.NewValue(tftypes.Map{ElementType: tftypes.Bool}, map[string]tftypes.Value{"k": tftypes.NewValue(tftypes.Bool, true)}),
	})
	vars, known, err := templateVars(obj)
	if err != nil || !known {
		t.Fatalf("templateVars: known=%v err=%v", known, err)
	}
	got, err := renderTemplate(syntaxGo, "{{ .s }} {{ .n }} {{ index .l 0 }} {{ .m.k }}", vars)
	if err != nil || got != "x 1.5 a true" {
		t.Fatalf("got %q, %v", got, err)
	}

	if _, _, err := templateVars(tftypes.NewValue(tftypes.String, "x")); err == nil {
		t.Error("expected an error for a non-object value")
	}
	unknown := tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, map[string]tftypes.Value{
		"a": tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
	})
	if _, known, err := templateVars(unknown); known || err != nil {
		t.Errorf("partially unknown value: known=%v err=%v, want known=false", known, err)
	}
	vars, known, err = templateVars(tftypes.NewValue(tftypes.DynamicPseudoType, nil))
	if err != nil || !known || len(vars) != 0 {
		t.Errorf("null value: vars=%v known=%v err=%v", vars, known, err)
	}
}

func TestRedactSensitiveValues(t *testing.T) {
	secrets := map[string]cty.Value{
		"pattern": cty.StringVal("(hunter2"),
		"short":   cty.StringVal("ab"),
		"nested":  cty.ObjectVal(map[string]cty.Value{"pin": cty.NumberIntVal(98765)}),
	}
	// An invalid regular expression is quoted in the error message.
	_, err := renderTemplate(syntaxTerraform, `${regex(pattern, "x")}`, secrets)
	if err == nil || !strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("expected the raw error to contain the secret, got %v", err)
	}
	msg := redact(err.Error(), sensitiveStrings(secrets))
	if strings.Contains(msg, "hunter2") || !strings.Contains(msg, "(sensitive value)") {
		t.Errorf("secret not redacted: %q", msg)
	}
	if got := redact("pin 98765 ab", sensitiveStrings(secrets)); got != "pin (sensitive value) ab" {
		t.Errorf("got %q", got)
	}
}
