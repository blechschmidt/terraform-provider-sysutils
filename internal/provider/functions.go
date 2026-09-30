package provider

// Provider-defined functions (Terraform 1.8 and later, OpenTofu 1.7 and
// later), called as provider::sysutils::<name>(...). They are pure: they
// only parse their argument and never read files or look at the host, so
// their results are the same during plan and apply. Pair them with file()
// or the sysutils_file data source to parse a file.

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/function"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// stringFunction is a function of one string argument. run returns the
// result as a value that function.ResultData.Set accepts for ret, or an
// error that is reported against the argument.
type stringFunction struct {
	name        string
	summary     string
	description string
	param       function.StringParameter
	ret         function.Return
	run         func(string) (any, error)
}

var _ function.Function = (*stringFunction)(nil)

func (f *stringFunction) Metadata(_ context.Context, _ function.MetadataRequest, resp *function.MetadataResponse) {
	resp.Name = f.name
}

func (f *stringFunction) Definition(_ context.Context, _ function.DefinitionRequest, resp *function.DefinitionResponse) {
	resp.Definition = function.Definition{
		Summary:             f.summary,
		MarkdownDescription: f.description,
		Parameters:          []function.Parameter{f.param},
		Return:              f.ret,
	}
}

func (f *stringFunction) Run(ctx context.Context, req function.RunRequest, resp *function.RunResponse) {
	var arg string
	resp.Error = req.Arguments.Get(ctx, &arg)
	if resp.Error != nil {
		return
	}
	result, err := f.run(arg)
	if err != nil {
		// Terraform prefixes the message with the parameter name.
		resp.Error = function.NewArgumentFuncError(0, capitalize(err.Error())+".")
		return
	}
	resp.Error = resp.Result.Set(ctx, result)
}

// providerFunctions lists the provider-defined functions.
func providerFunctions() []func() function.Function {
	return []func() function.Function{
		newParseIniFunction,
		newParseOSReleaseFunction,
		newParsePasswdLineFunction,
		newParseFstabLineFunction,
		newModeToOctalFunction,
	}
}

func newParseIniFunction() function.Function {
	return &stringFunction{
		name:    "parse_ini",
		summary: "Parse INI content into a map of sections to maps of keys to values",
		description: "Parses the content of an INI-style file (`key = value` lines below `[section]` headers) the way " +
			"`sysutils_ini_value` reads it with its default `separator`, and returns a map from section name to a map from key to value.\n\n" +
			"- Keys before the first section header are in the section `\"\"`, which is only present if it has keys. " +
			"Every other section is present even if it has no keys.\n" +
			"- A section whose header appears more than once is merged; of a key set more than once, the last value wins.\n" +
			"- Keys and values are split at the first `=` and trimmed. A line without `=` is a key with an empty value, like `skip-name-resolve` in `my.cnf`.\n" +
			"- Values are taken literally: quotes and trailing comments are part of the value, because their meaning differs between INI dialects.\n" +
			"- Lines starting with `#` or `;`, blank lines and malformed section headers are skipped. A UTF-8 byte order mark and `\\r\\n` line endings are handled.",
		param: function.StringParameter{
			Name:                "content",
			MarkdownDescription: "The INI content, for example from `file()` or the `content` of the `sysutils_file` data source.",
		},
		ret: function.MapReturn{ElementType: types.MapType{ElemType: types.StringType}},
		run: func(s string) (any, error) { return parseIniSections(s), nil },
	}
}

func newParseOSReleaseFunction() function.Function {
	return &stringFunction{
		name:    "parse_os_release",
		summary: "Parse os-release(5) content into a map of fields",
		description: "Parses the content of an `os-release` file (`/etc/os-release` or `/usr/lib/os-release`) with the parser of the `sysutils_host` data source, " +
			"and returns a map from field name, such as `ID` or `VERSION_ID`, to its value.\n\n" +
			"Values in single or double quotes and backslash escapes are decoded as `os-release(5)` specifies. " +
			"Comments, blank lines, lines that are not `KEY=value` assignments and values with unquoted white space are skipped; of a key set more than once, the last value wins. " +
			"To read the facts of the host the provider runs on, use the `sysutils_host` data source instead; this function parses the file of any tree, such as an image.",
		param: function.StringParameter{
			Name:                "content",
			MarkdownDescription: fmt.Sprintf("The content of the `os-release` file, at most %d KiB.", maxOSReleaseSize>>10),
		},
		ret: function.MapReturn{ElementType: types.StringType},
		run: func(s string) (any, error) {
			if len(s) > maxOSReleaseSize {
				return nil, fmt.Errorf("os-release content is %d bytes, more than the limit of %d", len(s), maxOSReleaseSize)
			}
			return map[string]string(parseOSRelease([]byte(s))), nil
		},
	}
}

var passwdEntryAttrTypes = map[string]attr.Type{
	"name":     types.StringType,
	"password": types.StringType,
	"uid":      types.Int64Type,
	"gid":      types.Int64Type,
	"gecos":    types.StringType,
	"home":     types.StringType,
	"shell":    types.StringType,
}

func newParsePasswdLineFunction() function.Function {
	return &stringFunction{
		name:    "parse_passwd_line",
		summary: "Parse one line of /etc/passwd into an object",
		description: "Parses one line of `/etc/passwd` (`name:password:uid:gid:gecos:home:shell`, see `passwd(5)`) and returns an object with the attributes " +
			"`name`, `password` (usually `x`, as the hash is in `/etc/shadow`), `uid`, `gid`, `gecos`, `home` and `shell`.\n\n" +
			"Returns `null` for a blank line or a `#` comment, so that a whole file can be parsed with a `for` expression that skips them. " +
			"A line with other than seven fields, an empty name or a `uid` or `gid` that is not a number between 0 and 4294967295 is an error, and so is an argument with more than one line. " +
			"To look up a user of the host the provider runs on, use the `sysutils_user` data source instead.",
		param: function.StringParameter{
			Name:                "line",
			MarkdownDescription: "One line of a passwd file. A trailing `\\n` or `\\r\\n` is ignored.",
		},
		ret: function.ObjectReturn{AttributeTypes: passwdEntryAttrTypes},
		run: func(s string) (any, error) {
			e, ok, err := parsePasswdLine(s)
			if err != nil {
				return nil, err
			}
			if !ok {
				return types.ObjectNull(passwdEntryAttrTypes), nil
			}
			obj, diags := types.ObjectValue(passwdEntryAttrTypes, map[string]attr.Value{
				"name":     types.StringValue(e.name),
				"password": types.StringValue(e.password),
				"uid":      types.Int64Value(e.uid),
				"gid":      types.Int64Value(e.gid),
				"gecos":    types.StringValue(e.gecos),
				"home":     types.StringValue(e.home),
				"shell":    types.StringValue(e.shell),
			})
			if diags.HasError() {
				return nil, fmt.Errorf("building the result: %v", diags)
			}
			return obj, nil
		},
	}
}

var fstabEntryAttrTypes = map[string]attr.Type{
	"device":      types.StringType,
	"mount_point": types.StringType,
	"fstype":      types.StringType,
	"options":     types.ListType{ElemType: types.StringType},
	"dump":        types.Int64Type,
	"pass":        types.Int64Type,
}

func newParseFstabLineFunction() function.Function {
	return &stringFunction{
		name:    "parse_fstab_line",
		summary: "Parse one line of /etc/fstab into an object",
		description: "Parses one line of `/etc/fstab` (see `fstab(5)`) with the parser of `sysutils_mount`, and returns an object with the attributes " +
			"`device`, `mount_point`, `fstype`, `options` (a list), `dump` and `pass`.\n\n" +
			"Fields are separated by spaces or tabs, and octal escapes such as `\\040` for a space are decoded. " +
			"Missing trailing fields get their `fstab(5)` defaults: an empty `fstype`, `options` of `[\"defaults\"]` and `0` for `dump` and `pass`. " +
			"Commas inside double quotes, as in `context=\"system_u:object_r:tmp_t:s0:c127,c456\"`, do not separate options. " +
			"Fields after the sixth are ignored, as `mount(8)` ignores them.\n\n" +
			"Returns `null` for a blank line or a `#` comment, so that a whole file can be parsed with a `for` expression that skips them. " +
			"A line with only one field, a `dump` or `pass` that is not a non-negative number, or an argument with more than one line is an error.",
		param: function.StringParameter{
			Name:                "line",
			MarkdownDescription: "One line of an fstab file. A trailing `\\n` or `\\r\\n` is ignored.",
		},
		ret: function.ObjectReturn{AttributeTypes: fstabEntryAttrTypes},
		run: func(s string) (any, error) {
			e, ok, err := parseFstabFunctionLine(s)
			if err != nil {
				return nil, err
			}
			if !ok {
				return types.ObjectNull(fstabEntryAttrTypes), nil
			}
			options, diags := types.ListValueFrom(context.Background(), types.StringType, e.options)
			if diags.HasError() {
				return nil, fmt.Errorf("building the result: %v", diags)
			}
			obj, diags := types.ObjectValue(fstabEntryAttrTypes, map[string]attr.Value{
				"device":      types.StringValue(e.device),
				"mount_point": types.StringValue(e.mountPoint),
				"fstype":      types.StringValue(e.fstype),
				"options":     options,
				"dump":        types.Int64Value(e.dump),
				"pass":        types.Int64Value(e.pass),
			})
			if diags.HasError() {
				return nil, fmt.Errorf("building the result: %v", diags)
			}
			return obj, nil
		},
	}
}

func newModeToOctalFunction() function.Function {
	return &stringFunction{
		name:    "mode_to_octal",
		summary: "Normalise a file mode to a four-digit octal string",
		description: "Converts a file mode to the four-digit octal form, such as `\"0644\"`, that the `mode` attributes of this provider accept and report, " +
			"so that modes written in different ways can be compared or passed on. It accepts:\n\n" +
			"- Octal modes, as the `mode` attributes do: `\"644\"`, `\"0644\"`, `\"1777\"`.\n" +
			"- `ls -l` permission strings, optionally with the file type character: `\"rw-r--r--\"`, `\"rwsr-xr-x\"` (setuid), `\"drwxrwxrwt\"` (sticky).\n" +
			"- `chmod(1)` symbolic modes, applied to the empty mode `0000`: `\"u=rw,go=r\"`, `\"a=rx,u+w\"`, `\"ug=rwx,o=\"`, `\"u=rwxs,g=rx\"`, `\"a=rwx,+t\"`. " +
			"Without `u`, `g`, `o` or `a` a clause applies to all classes, and the umask is not applied. " +
			"The copies `u`, `g` and `o` and the conditional `X` on the right-hand side are errors, as their result depends on an existing file.",
		param: function.StringParameter{
			Name:                "mode",
			MarkdownDescription: "The mode in octal, `ls -l` or `chmod` symbolic notation.",
		},
		ret: function.StringReturn{},
		run: func(s string) (any, error) { return normaliseMode(s) },
	}
}
