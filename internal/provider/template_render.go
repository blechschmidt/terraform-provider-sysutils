package provider

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"unicode/utf8"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/tryfunc"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
	"github.com/zclconf/go-cty/cty/function"
	"github.com/zclconf/go-cty/cty/function/stdlib"
)

const (
	// syntaxGo selects Go text/template syntax: {{ .name }}.
	syntaxGo = "go"
	// syntaxTerraform selects Terraform template syntax: ${name}, %{ if }.
	syntaxTerraform = "terraform"

	// maxRenderedSize bounds the output of a template, which is held in
	// memory and stored in state.
	maxRenderedSize = 16 << 20

	// templateName is the name templates are parsed under. It shows up in
	// raw error messages and is stripped from the ones we report.
	templateName = "template"

	// minRedactedLength is the shortest sensitive value that is redacted
	// from error messages. Shorter values would mangle unrelated text.
	minRedactedLength = 4
)

// templateError is a template parse or execution error with the position in
// the template it refers to. Line and Column are 1-based; 0 means unknown.
type templateError struct {
	Line   int
	Column int
	Msg    string
}

func (e *templateError) Error() string {
	switch {
	case e.Line > 0 && e.Column > 0:
		return fmt.Sprintf("line %d, column %d: %s", e.Line, e.Column, e.Msg)
	case e.Line > 0:
		return fmt.Sprintf("line %d: %s", e.Line, e.Msg)
	default:
		return e.Msg
	}
}

// parseTemplate checks that src parses as a template of the given syntax.
// Parse errors are returned as *templateError.
func parseTemplate(syntax, src string) error {
	switch syntax {
	case syntaxGo:
		_, err := parseGoTemplate(src)
		return err
	case syntaxTerraform:
		_, err := parseTerraformTemplate(src)
		return err
	default:
		return fmt.Errorf("unsupported template syntax %q", syntax)
	}
}

// renderTemplate renders src with vars. vars must be an object or map; its
// attributes become the template's variables. Rendering has no access to the
// filesystem, the environment, the network or the clock, so the output
// depends only on src and vars.
func renderTemplate(syntax, src string, vars map[string]cty.Value) (string, error) {
	switch syntax {
	case syntaxGo:
		return renderGoTemplate(src, vars)
	case syntaxTerraform:
		return renderTerraformTemplate(src, vars)
	default:
		return "", fmt.Errorf("unsupported template syntax %q", syntax)
	}
}

// --- Go text/template ------------------------------------------------------

// goErrorRE matches the "template: <name>:<line>[:<col>]: <msg>" prefix of
// text/template errors.
var goErrorRE = regexp.MustCompile(`(?s)^template: ` + templateName + `:(\d+):(?:(\d+):)? ?(.*)$`)

// goTemplateError converts a text/template error into a *templateError.
func goTemplateError(err error) error {
	m := goErrorRE.FindStringSubmatch(err.Error())
	if m == nil {
		return &templateError{Msg: err.Error()}
	}
	line, _ := strconv.Atoi(m[1])
	col, _ := strconv.Atoi(m[2])
	msg := strings.TrimPrefix(m[3], fmt.Sprintf("executing %q at ", templateName))
	return &templateError{Line: line, Column: col, Msg: msg}
}

func parseGoTemplate(src string) (*template.Template, error) {
	t, err := template.New(templateName).Option("missingkey=error").Funcs(goTemplateFuncs).Parse(src)
	if err != nil {
		return nil, goTemplateError(err)
	}
	return t, nil
}

func renderGoTemplate(src string, vars map[string]cty.Value) (string, error) {
	t, err := parseGoTemplate(src)
	if err != nil {
		return "", err
	}
	data := make(map[string]any, len(vars))
	for k, v := range vars {
		data[k] = ctyToGo(v)
	}
	w := &limitedBuffer{limit: maxRenderedSize}
	if err := t.Execute(w, data); err != nil {
		if errors.Is(err, errRenderedTooLarge) {
			return "", errRenderedTooLarge
		}
		return "", goTemplateError(err)
	}
	return w.String(), nil
}

var errRenderedTooLarge = fmt.Errorf("rendered output exceeds %d bytes", maxRenderedSize)

// limitedBuffer is a bytes.Buffer that fails writes beyond limit bytes.
type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		return 0, errRenderedTooLarge
	}
	return b.Buffer.Write(p)
}

// goTemplateFuncs is the complete set of functions available to Go templates
// besides text/template's builtins. None of them can reach the filesystem,
// the environment or the network. Like in sprig, the value being operated on
// comes last, so that the functions work in pipelines:
// {{ .name | replace "-" "_" | upper }}.
var goTemplateFuncs = template.FuncMap{
	"lower":      strings.ToLower,
	"upper":      strings.ToUpper,
	"trim":       func(cutset, s string) string { return strings.Trim(s, cutset) },
	"trimSpace":  strings.TrimSpace,
	"trimPrefix": func(prefix, s string) string { return strings.TrimPrefix(s, prefix) },
	"trimSuffix": func(suffix, s string) string { return strings.TrimSuffix(s, suffix) },
	"replace":    func(old, repl, s string) string { return strings.ReplaceAll(s, old, repl) },
	"contains":   func(substr, s string) bool { return strings.Contains(s, substr) },
	"hasPrefix":  func(prefix, s string) bool { return strings.HasPrefix(s, prefix) },
	"hasSuffix":  func(suffix, s string) bool { return strings.HasSuffix(s, suffix) },
	"split":      func(sep, s string) []string { return strings.Split(s, sep) },
	"join":       goJoin,
	"repeat":     goRepeat,
	"indent":     func(n int, s string) string { return indentLines(n, s) },
	"nindent":    func(n int, s string) string { return "\n" + indentLines(n, s) },
	"quote":      func(v any) string { return strconv.Quote(fmt.Sprint(v)) },
	"default":    goDefault,
	"toJson":     goToJSON,
	"b64enc":     func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) },
	"b64dec":     goB64Dec,
	"keys":       goKeys,
}

func goJoin(sep string, list any) (string, error) {
	switch l := list.(type) {
	case []string:
		return strings.Join(l, sep), nil
	case []any:
		parts := make([]string, len(l))
		for i, v := range l {
			parts[i] = fmt.Sprint(v)
		}
		return strings.Join(parts, sep), nil
	default:
		return "", fmt.Errorf("join: expected a list, got %T", list)
	}
}

func goRepeat(n int, s string) (string, error) {
	if n < 0 {
		return "", errors.New("repeat: negative count")
	}
	if n > 0 && len(s) > maxRenderedSize/n {
		return "", errRenderedTooLarge
	}
	return strings.Repeat(s, n), nil
}

// indentLines prefixes every non-empty line of s with n spaces.
func indentLines(n int, s string) string {
	if n <= 0 {
		return s
	}
	pad := strings.Repeat(" ", min(n, 1024))
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = pad + l
		}
	}
	return strings.Join(lines, "\n")
}

// goDefault returns v, or def if v is empty: nil, false, zero, "" or an
// empty list or map.
func goDefault(def, v any) any {
	switch x := v.(type) {
	case nil:
		return def
	case string:
		if x == "" {
			return def
		}
	case bool:
		if !x {
			return def
		}
	case int:
		if x == 0 {
			return def
		}
	case float64:
		if x == 0 {
			return def
		}
	case []any:
		if len(x) == 0 {
			return def
		}
	case map[string]any:
		if len(x) == 0 {
			return def
		}
	}
	return v
}

func goToJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("toJson: %w", err)
	}
	return string(b), nil
}

func goB64Dec(s string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", fmt.Errorf("b64dec: %w", err)
	}
	if !utf8.Valid(b) {
		return "", errors.New("b64dec: decoded data is not valid UTF-8")
	}
	return string(b), nil
}

func goKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ctyToGo converts a known cty value into the plain Go values that
// text/template works with: string, bool, int (for whole numbers that fit),
// float64, []any, map[string]any and nil.
func ctyToGo(v cty.Value) any {
	if v.IsNull() {
		return nil
	}
	ty := v.Type()
	switch {
	case ty == cty.String:
		return v.AsString()
	case ty == cty.Bool:
		return v.True()
	case ty == cty.Number:
		f := v.AsBigFloat()
		if f.IsInt() {
			if i, acc := f.Int64(); acc == big.Exact && int64(int(i)) == i {
				return int(i)
			}
		}
		fl, _ := f.Float64()
		return fl
	case ty.IsListType() || ty.IsSetType() || ty.IsTupleType():
		out := make([]any, 0, v.LengthInt())
		for it := v.ElementIterator(); it.Next(); {
			_, e := it.Element()
			out = append(out, ctyToGo(e))
		}
		return out
	case ty.IsMapType() || ty.IsObjectType():
		out := make(map[string]any, v.LengthInt())
		for it := v.ElementIterator(); it.Next(); {
			k, e := it.Element()
			out[k.AsString()] = ctyToGo(e)
		}
		return out
	default:
		return nil
	}
}

// --- Terraform template syntax ---------------------------------------------

func parseTerraformTemplate(src string) (hclsyntax.Expression, error) {
	expr, diags := hclsyntax.ParseTemplate([]byte(src), templateName, hcl.InitialPos)
	if diags.HasErrors() {
		return nil, hclTemplateError(diags)
	}
	return expr, nil
}

func renderTerraformTemplate(src string, vars map[string]cty.Value) (string, error) {
	expr, err := parseTerraformTemplate(src)
	if err != nil {
		return "", err
	}
	// Report references to undefined variables with their position, like
	// Terraform's templatefile() does.
	for _, tr := range expr.Variables() {
		name := tr.RootName()
		if _, ok := vars[name]; !ok {
			rng := tr.SourceRange()
			return "", &templateError{Line: rng.Start.Line, Column: rng.Start.Column,
				Msg: fmt.Sprintf("vars does not contain %q", name)}
		}
	}
	ctx := &hcl.EvalContext{Variables: vars, Functions: terraformTemplateFuncs}
	val, diags := expr.Value(ctx)
	if diags.HasErrors() {
		return "", hclTemplateError(diags)
	}
	if val.IsNull() {
		return "", &templateError{Msg: "template result is null"}
	}
	str, err := convert.Convert(val, cty.String)
	if err != nil {
		return "", &templateError{Msg: fmt.Sprintf("template result cannot be converted to a string: %s", err)}
	}
	out := str.AsString()
	if len(out) > maxRenderedSize {
		return "", errRenderedTooLarge
	}
	return out, nil
}

// hclTemplateError converts the first error in diags into a *templateError.
func hclTemplateError(diags hcl.Diagnostics) error {
	for _, d := range diags {
		if d.Severity != hcl.DiagError {
			continue
		}
		msg := d.Summary
		if d.Detail != "" {
			msg += "; " + d.Detail
		}
		e := &templateError{Msg: msg}
		if d.Subject != nil {
			e.Line, e.Column = d.Subject.Start.Line, d.Subject.Start.Column
		}
		return e
	}
	return &templateError{Msg: diags.Error()}
}

// terraformTemplateFuncs is the complete set of functions available to
// templates in Terraform syntax: the pure functions of Terraform's language
// that do not touch the filesystem, the environment, the network or the
// clock. Functions such as file(), templatefile(), timestamp() and uuid() are
// deliberately absent.
var terraformTemplateFuncs = map[string]function.Function{
	"abs":             stdlib.AbsoluteFunc,
	"base64decode":    base64DecodeFunc,
	"base64encode":    base64EncodeFunc,
	"can":             tryfunc.CanFunc,
	"ceil":            stdlib.CeilFunc,
	"chomp":           stdlib.ChompFunc,
	"chunklist":       stdlib.ChunklistFunc,
	"coalesce":        stdlib.CoalesceFunc,
	"coalescelist":    stdlib.CoalesceListFunc,
	"compact":         stdlib.CompactFunc,
	"concat":          stdlib.ConcatFunc,
	"contains":        stdlib.ContainsFunc,
	"csvdecode":       stdlib.CSVDecodeFunc,
	"distinct":        stdlib.DistinctFunc,
	"element":         stdlib.ElementFunc,
	"endswith":        endsWithFunc,
	"flatten":         stdlib.FlattenFunc,
	"floor":           stdlib.FloorFunc,
	"format":          stdlib.FormatFunc,
	"formatlist":      stdlib.FormatListFunc,
	"indent":          stdlib.IndentFunc,
	"join":            stdlib.JoinFunc,
	"jsondecode":      stdlib.JSONDecodeFunc,
	"jsonencode":      stdlib.JSONEncodeFunc,
	"keys":            stdlib.KeysFunc,
	"length":          stdlib.LengthFunc,
	"log":             stdlib.LogFunc,
	"lookup":          stdlib.LookupFunc,
	"lower":           stdlib.LowerFunc,
	"max":             stdlib.MaxFunc,
	"merge":           stdlib.MergeFunc,
	"min":             stdlib.MinFunc,
	"parseint":        stdlib.ParseIntFunc,
	"pow":             stdlib.PowFunc,
	"range":           stdlib.RangeFunc,
	"regex":           stdlib.RegexFunc,
	"regexall":        stdlib.RegexAllFunc,
	"replace":         replaceFunc,
	"reverse":         stdlib.ReverseListFunc,
	"setintersection": stdlib.SetIntersectionFunc,
	"setsubtract":     stdlib.SetSubtractFunc,
	"setunion":        stdlib.SetUnionFunc,
	"signum":          stdlib.SignumFunc,
	"slice":           stdlib.SliceFunc,
	"sort":            stdlib.SortFunc,
	"split":           stdlib.SplitFunc,
	"startswith":      startsWithFunc,
	"strrev":          stdlib.ReverseFunc,
	"substr":          stdlib.SubstrFunc,
	"title":           stdlib.TitleFunc,
	"tobool":          stdlib.MakeToFunc(cty.Bool),
	"tolist":          stdlib.MakeToFunc(cty.List(cty.DynamicPseudoType)),
	"tomap":           stdlib.MakeToFunc(cty.Map(cty.DynamicPseudoType)),
	"tonumber":        stdlib.MakeToFunc(cty.Number),
	"toset":           stdlib.MakeToFunc(cty.Set(cty.DynamicPseudoType)),
	"tostring":        stdlib.MakeToFunc(cty.String),
	"trim":            stdlib.TrimFunc,
	"trimprefix":      stdlib.TrimPrefixFunc,
	"trimspace":       stdlib.TrimSpaceFunc,
	"trimsuffix":      stdlib.TrimSuffixFunc,
	"try":             tryfunc.TryFunc,
	"upper":           stdlib.UpperFunc,
	"values":          stdlib.ValuesFunc,
	"zipmap":          stdlib.ZipmapFunc,
}

func stringFunc(params []string, impl func(args []string) (cty.Value, error), ret cty.Type) function.Function {
	spec := &function.Spec{Type: function.StaticReturnType(ret)}
	for _, p := range params {
		spec.Params = append(spec.Params, function.Parameter{Name: p, Type: cty.String})
	}
	spec.Impl = func(args []cty.Value, _ cty.Type) (cty.Value, error) {
		strs := make([]string, len(args))
		for i, a := range args {
			strs[i] = a.AsString()
		}
		return impl(strs)
	}
	return function.New(spec)
}

var base64EncodeFunc = stringFunc([]string{"str"}, func(a []string) (cty.Value, error) {
	return cty.StringVal(base64.StdEncoding.EncodeToString([]byte(a[0]))), nil
}, cty.String)

var base64DecodeFunc = stringFunc([]string{"str"}, func(a []string) (cty.Value, error) {
	b, err := base64.StdEncoding.DecodeString(a[0])
	if err != nil {
		return cty.UnknownVal(cty.String), fmt.Errorf("failed to decode base64 data: %w", err)
	}
	if !utf8.Valid(b) {
		return cty.UnknownVal(cty.String), errors.New("the decoded data is not valid UTF-8")
	}
	return cty.StringVal(string(b)), nil
}, cty.String)

var startsWithFunc = stringFunc([]string{"str", "prefix"}, func(a []string) (cty.Value, error) {
	return cty.BoolVal(strings.HasPrefix(a[0], a[1])), nil
}, cty.Bool)

var endsWithFunc = stringFunc([]string{"str", "suffix"}, func(a []string) (cty.Value, error) {
	return cty.BoolVal(strings.HasSuffix(a[0], a[1])), nil
}, cty.Bool)

// replaceFunc is Terraform's replace(): a substring wrapped in forward
// slashes is treated as a regular expression.
var replaceFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "str", Type: cty.String},
		{Name: "substr", Type: cty.String},
		{Name: "replace", Type: cty.String},
	},
	Type: function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
		substr := args[1].AsString()
		if len(substr) > 1 && strings.HasPrefix(substr, "/") && strings.HasSuffix(substr, "/") {
			re, err := regexp.Compile(substr[1 : len(substr)-1])
			if err != nil {
				return cty.UnknownVal(cty.String), err
			}
			return cty.StringVal(re.ReplaceAllString(args[0].AsString(), args[2].AsString())), nil
		}
		return cty.StringVal(strings.ReplaceAll(args[0].AsString(), substr, args[2].AsString())), nil
	},
})

// --- Variables ---------------------------------------------------------------

// templateVars converts the value of a vars attribute (an object or map) into
// template variables. Null yields no variables. It returns known=false if the
// value is not fully known yet.
func templateVars(v tftypes.Value) (vars map[string]cty.Value, known bool, err error) {
	if !v.IsFullyKnown() {
		return nil, false, nil
	}
	if v.IsNull() {
		return map[string]cty.Value{}, true, nil
	}
	if !v.Type().Is(tftypes.Object{}) && !v.Type().Is(tftypes.Map{}) {
		return nil, true, fmt.Errorf("must be an object or a map, got %s", v.Type())
	}
	var attrs map[string]tftypes.Value
	if err := v.As(&attrs); err != nil {
		return nil, true, err
	}
	vars = make(map[string]cty.Value, len(attrs))
	for k, a := range attrs {
		if vars[k], err = tftypesToCty(a); err != nil {
			return nil, true, fmt.Errorf("%q: %w", k, err)
		}
	}
	return vars, true, nil
}

// tftypesToCty converts a fully known Terraform value into a cty value.
// Lists, sets and tuples become tuples, and maps and objects become objects,
// so that elements need not share a type.
func tftypesToCty(v tftypes.Value) (cty.Value, error) {
	if v.IsNull() {
		return cty.NullVal(cty.DynamicPseudoType), nil
	}
	ty := v.Type()
	switch {
	case ty.Is(tftypes.String):
		var s string
		if err := v.As(&s); err != nil {
			return cty.NilVal, err
		}
		return cty.StringVal(s), nil
	case ty.Is(tftypes.Number):
		f := new(big.Float)
		if err := v.As(&f); err != nil {
			return cty.NilVal, err
		}
		return cty.NumberVal(f), nil
	case ty.Is(tftypes.Bool):
		var b bool
		if err := v.As(&b); err != nil {
			return cty.NilVal, err
		}
		return cty.BoolVal(b), nil
	case ty.Is(tftypes.List{}) || ty.Is(tftypes.Set{}) || ty.Is(tftypes.Tuple{}):
		var elems []tftypes.Value
		if err := v.As(&elems); err != nil {
			return cty.NilVal, err
		}
		if len(elems) == 0 {
			return cty.EmptyTupleVal, nil
		}
		out := make([]cty.Value, len(elems))
		for i, e := range elems {
			c, err := tftypesToCty(e)
			if err != nil {
				return cty.NilVal, err
			}
			out[i] = c
		}
		return cty.TupleVal(out), nil
	case ty.Is(tftypes.Map{}) || ty.Is(tftypes.Object{}):
		var attrs map[string]tftypes.Value
		if err := v.As(&attrs); err != nil {
			return cty.NilVal, err
		}
		if len(attrs) == 0 {
			return cty.EmptyObjectVal, nil
		}
		out := make(map[string]cty.Value, len(attrs))
		for k, a := range attrs {
			c, err := tftypesToCty(a)
			if err != nil {
				return cty.NilVal, err
			}
			out[k] = c
		}
		return cty.ObjectVal(out), nil
	default:
		return cty.NilVal, fmt.Errorf("unsupported type %s", ty)
	}
}

// sensitiveStrings returns the string and number leaves of vars that are
// long enough to be redacted from error messages.
func sensitiveStrings(vars map[string]cty.Value) []string {
	var out []string
	var walk func(v cty.Value)
	walk = func(v cty.Value) {
		if v.IsNull() || !v.IsKnown() {
			return
		}
		ty := v.Type()
		switch {
		case ty == cty.String:
			if s := v.AsString(); len(s) >= minRedactedLength {
				out = append(out, s)
			}
		case ty == cty.Number:
			if s := v.AsBigFloat().Text('f', -1); len(s) >= minRedactedLength {
				out = append(out, s)
			}
		case ty.IsCollectionType() || ty.IsTupleType() || ty.IsObjectType():
			for it := v.ElementIterator(); it.Next(); {
				_, e := it.Element()
				walk(e)
			}
		}
	}
	for _, v := range vars {
		walk(v)
	}
	// Longest first, so that a secret containing another is fully replaced.
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// redact replaces every occurrence of a secret in msg.
func redact(msg string, secrets []string) string {
	for _, s := range secrets {
		msg = strings.ReplaceAll(msg, s, "(sensitive value)")
	}
	return msg
}
