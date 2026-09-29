package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/zclconf/go-cty/cty"
)

var (
	_ resource.Resource                   = (*templateFileResource)(nil)
	_ resource.ResourceWithImportState    = (*templateFileResource)(nil)
	_ resource.ResourceWithValidateConfig = (*templateFileResource)(nil)
	_ resource.ResourceWithModifyPlan     = (*templateFileResource)(nil)
)

func NewTemplateFileResource() resource.Resource { return &templateFileResource{} }

type templateFileResource struct{}

type templateFileModel struct {
	Path              types.String  `tfsdk:"path"`
	Template          types.String  `tfsdk:"template"`
	Syntax            types.String  `tfsdk:"syntax"`
	Vars              types.Dynamic `tfsdk:"vars"`
	SensitiveVars     types.Dynamic `tfsdk:"sensitive_vars"`
	Rendered          types.String  `tfsdk:"rendered"`
	RenderedSensitive types.String  `tfsdk:"rendered_sensitive"`
	ContentSHA256     types.String  `tfsdk:"content_sha256"`
	ContentMD5        types.String  `tfsdk:"content_md5"`
	Mode              types.String  `tfsdk:"mode"`
	Owner             types.String  `tfsdk:"owner"`
	Group             types.String  `tfsdk:"group"`
	ID                types.String  `tfsdk:"id"`
}

func (r *templateFileResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_template_file"
}

func (r *templateFileResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Renders a `template` with `vars` inside the provider and writes the result to the file at `path`. " +
			"Templates use Go `text/template` syntax (`{{ .name }}`) or Terraform template syntax (`${name}`), with a restricted set of functions that cannot read files or the environment. " +
			"The template is checked when planning, and the file is only rewritten when the rendered content differs from what is on disk.",
		Attributes: map[string]schema.Attribute{
			"path": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Absolute path of the file to write. " +
					"Must be in canonical form (no `.`/`..` segments, duplicate or trailing slashes) and must not be `/`. " +
					"A symlink at `path` is refused. Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{absolutePath()},
			},
			"template": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The template text, usually loaded with `file(\"${path.module}/app.conf.tmpl\")`. " +
					"It is parsed during validation and plan; a syntax error is reported with its line and column.",
			},
			"syntax": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(syntaxGo),
				MarkdownDescription: "Template syntax: `\"go\"` for Go [`text/template`](https://pkg.go.dev/text/template) (`{{ .name }}`) " +
					"or `\"terraform\"` for Terraform's [template syntax](https://developer.hashicorp.com/terraform/language/expressions/strings#string-templates) (`${name}`, `%{ if }`, `%{ for }`), as used by `templatefile()`. " +
					"Defaults to `\"go\"`.",
				Validators: []validator.String{stringvalidator.OneOf(syntaxGo, syntaxTerraform)},
			},
			"vars": schema.DynamicAttribute{
				Optional: true,
				MarkdownDescription: "Variables for the template, as an object or map. Values may be strings, numbers, bools, lists and nested objects. " +
					"In Go syntax a variable `name` is referenced as `{{ .name }}`, in Terraform syntax as `${name}`. " +
					"Put secrets into `sensitive_vars` instead: the provider cannot see whether a value in `vars` is marked sensitive, so the output would be stored in the non-sensitive `rendered`.",
			},
			"sensitive_vars": schema.DynamicAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "Like `vars`, for secrets. Names must not also appear in `vars`. " +
					"If set (even to an empty object), the output is stored only in `rendered_sensitive` and `rendered` is null, so it is never shown in plans. " +
					"Error messages about the template have these values replaced by `(sensitive value)`.",
			},
			"rendered": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The rendered content, known at plan time when all inputs are known. Null when `sensitive_vars` is set. " +
					"If the file is changed outside Terraform, refresh records the actual content here, so the plan shows the drift as a diff.",
			},
			"rendered_sensitive": schema.StringAttribute{
				Computed:            true,
				Sensitive:           true,
				MarkdownDescription: "The rendered content when `sensitive_vars` is set, marked sensitive; null otherwise.",
			},
			"content_sha256": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Hex-encoded SHA-256 checksum of the rendered content. " +
					"Computed during plan, and refreshed from the file on disk, so changes made outside Terraform are detected even when `sensitive_vars` is set.",
			},
			"content_md5": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Hex-encoded MD5 checksum of the rendered content, for interoperability. Use `content_sha256` where integrity matters.",
			},
			"mode": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(defaultFileMode),
				MarkdownDescription: "Octal mode with 3 or 4 digits, optionally with a leading zero, such as `\"0644\"` or `\"600\"`. " +
					"Applied with an explicit `chmod`, so the umask does not affect it. Defaults to `\"0644\"`.",
				Validators: []validator.String{octalMode()},
			},
			"owner": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Username or numeric UID that should own the file. Requires privileges to change. " +
					"If unset, the owner assigned at creation (normally the user running Terraform) is kept.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"group": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Group name or numeric GID of the file. Requires privileges to change. " +
					"If unset, the group assigned at creation is kept.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Resource identifier (equal to `path`).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

// ValidateConfig parses the template and checks the shape of the variables,
// so that mistakes are reported by terraform validate and plan, pointing at
// the attribute and, for the template, the line and column.
func (r *templateFileResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var config templateFileModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	syntax := config.syntax()
	if config.Syntax.IsUnknown() || (syntax != syntaxGo && syntax != syntaxTerraform) {
		return // An invalid syntax is reported by the attribute's validator.
	}

	if !config.Template.IsUnknown() && !config.Template.IsNull() {
		if err := parseTemplate(syntax, config.Template.ValueString()); err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("template"), "Invalid template",
				fmt.Sprintf("The template is not valid %s template syntax: %s.", syntaxLabel(syntax), err))
		}
	}

	names := map[string]string{}
	for _, a := range []struct {
		name  string
		value types.Dynamic
	}{{"vars", config.Vars}, {"sensitive_vars", config.SensitiveVars}} {
		vars, known, err := dynamicTemplateVars(ctx, a.value)
		if err != nil {
			resp.Diagnostics.AddAttributeError(path.Root(a.name), "Invalid template variables",
				fmt.Sprintf("The value of %s %s.", a.name, err))
			continue
		}
		if !known {
			continue
		}
		for _, k := range sortedKeys(vars) {
			if syntax == syntaxTerraform && !hclsyntax.ValidIdentifier(k) {
				resp.Diagnostics.AddAttributeError(path.Root(a.name), "Invalid template variables",
					fmt.Sprintf("%q is not a valid variable name for Terraform template syntax. "+
						"Names must start with a letter or underscore and contain only letters, digits, underscores and dashes.", k))
			}
			if other, dup := names[k]; dup {
				resp.Diagnostics.AddAttributeError(path.Root(a.name), "Invalid template variables",
					fmt.Sprintf("Variable %q is set in both %s and %s.", k, other, a.name))
			}
			names[k] = a.name
		}
	}
}

// ModifyPlan renders the template and plans the rendered content and its
// checksums. Refresh records the checksums of the file on disk, so any
// difference -- from changed inputs or from edits outside Terraform -- shows
// up as an update. If an input is not known yet, the outputs are unknown.
func (r *templateFileResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // Destroy.
	}
	var plan templateFileModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, known, diags := plan.render(ctx)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if known {
		plan.setRendered(out)
	} else {
		plan.ContentSHA256, plan.ContentMD5 = types.StringUnknown(), types.StringUnknown()
		plan.Rendered, plan.RenderedSensitive = types.StringUnknown(), types.StringUnknown()
		switch {
		case plan.SensitiveVars.IsUnknown():
		case plan.SensitiveVars.IsNull():
			plan.RenderedSensitive = types.StringNull()
		default:
			plan.Rendered = types.StringNull()
		}
	}
	for name, v := range map[string]types.String{
		"rendered":           plan.Rendered,
		"rendered_sensitive": plan.RenderedSensitive,
		"content_sha256":     plan.ContentSHA256,
		"content_md5":        plan.ContentMD5,
	} {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root(name), v)...)
	}
}

func (r *templateFileResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan templateFileModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target := plan.Path.ValueString()
	// Config validation already enforces this; re-check as defense in depth.
	if err := validateAbsolutePath(target); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("path"), "Invalid path", capitalize(err.Error())+".")
		return
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		resp.Diagnostics.AddError("Creating parent directory", err.Error())
		return
	}
	// Owner and group are unknown when not configured; ValueString returns ""
	// for unknown values, meaning "leave unchanged".
	resp.Diagnostics.Append(r.apply(ctx, &plan, plan.Owner.ValueString(), plan.Group.ValueString())...)
	if plan.ID.IsUnknown() {
		return // The file was not written, or could not be refreshed.
	}
	// The file exists; record it even on error so that a failure taints it
	// rather than orphaning it.
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *templateFileResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state templateFileModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	found, diags := refreshTemplateFile(&state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *templateFileResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state templateFileModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Only chown what actually changed, so that an unrelated update does not
	// require the privileges needed to (re)assign ownership.
	var owner, group string
	if !plan.Owner.IsUnknown() && !plan.Owner.Equal(state.Owner) {
		owner = plan.Owner.ValueString()
	}
	if !plan.Group.IsUnknown() && !plan.Group.Equal(state.Group) {
		group = plan.Group.ValueString()
	}
	// apply leaves the id unknown unless the file was written and refreshed.
	plan.ID = types.StringUnknown()
	resp.Diagnostics.Append(r.apply(ctx, &plan, owner, group)...)
	if plan.ID.IsUnknown() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *templateFileResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state templateFileModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target := state.Path.ValueString()
	// State is not validated by the schema; never act on an invalid path.
	if err := validateAbsolutePath(target); err != nil {
		resp.Diagnostics.AddError("Refusing to remove file", capitalize(err.Error())+".")
		return
	}
	// unlink never follows symlinks and never removes directories.
	if err := syscall.Unlink(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
		resp.Diagnostics.AddError("Removing file", (&fs.PathError{Op: "unlink", Path: target, Err: err}).Error())
	}
}

// ImportState takes the absolute path of the file. The template and its
// variables cannot be recovered from the file; they come from the
// configuration on the next apply, which rewrites the file only if the
// rendered content differs.
func (r *templateFileResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if err := validateAbsolutePath(req.ID); err != nil {
		resp.Diagnostics.AddError("Invalid import ID",
			fmt.Sprintf("Import ID must be the absolute path of the file: %s.", err))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("path"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}

// apply renders plan, writes the file if its content differs from the
// rendered output, applies mode and ownership (empty owner or group leave
// them unchanged), and refreshes plan from the file. plan.ID stays unknown
// unless the refresh succeeded, so callers must leave it unknown beforehand.
func (r *templateFileResource) apply(ctx context.Context, plan *templateFileModel, owner, group string) diag.Diagnostics {
	var diags diag.Diagnostics
	target := plan.Path.ValueString()
	mode, err := parseMode(plan.Mode.ValueString())
	if err != nil {
		diags.AddAttributeError(path.Root("mode"), "Invalid mode", err.Error())
		return diags
	}
	out, known, d := plan.render(ctx)
	diags.Append(d...)
	if diags.HasError() {
		return diags
	}
	if !known {
		diags.AddError("Rendering template", "The template inputs are still unknown during apply. This is a bug in the provider.")
		return diags
	}
	planned := plan.ContentSHA256
	plan.setRendered(out)
	if !planned.IsUnknown() && !planned.Equal(plan.ContentSHA256) {
		diags.AddError("Rendering template", fmt.Sprintf(
			"The template rendered to content with SHA-256 %s during apply, but to %s during plan. This is a bug in the provider.",
			plan.ContentSHA256.ValueString(), planned.ValueString()))
		return diags
	}

	current, err := fileChecksum(target)
	switch {
	case err == nil && current.sha256 == plan.ContentSHA256.ValueString():
		// Content is already right; don't touch it, so that the modification
		// time only changes when the content does.
		err = setFileOwnershipAndMode(target, owner, group, mode)
	case err == nil || errors.Is(err, fs.ErrNotExist):
		_, err = writeFile(target, strings.NewReader(out), mode, owner, group)
	}
	if err != nil {
		diags.AddError("Writing file", err.Error())
		return diags
	}

	found, d := refreshTemplateFile(plan)
	diags.Append(d...)
	if !found && !diags.HasError() {
		diags.AddError("Writing file", fmt.Sprintf("File %q disappeared immediately after it was written.", target))
	}
	return diags
}

// syntax returns the configured syntax, or the default if it is null (as it
// is in configuration that does not set it).
func (m *templateFileModel) syntax() string {
	if m.Syntax.IsNull() {
		return syntaxGo
	}
	return m.Syntax.ValueString()
}

// render renders the template with vars and sensitive_vars. known is false
// if any input is not known yet. Errors are attributed to the template and
// have sensitive values redacted.
func (m *templateFileModel) render(ctx context.Context) (out string, known bool, diags diag.Diagnostics) {
	if m.Template.IsUnknown() || m.Syntax.IsUnknown() {
		return "", false, diags
	}
	vars, known, err := dynamicTemplateVars(ctx, m.Vars)
	if err != nil {
		diags.AddAttributeError(path.Root("vars"), "Invalid template variables", fmt.Sprintf("The value of vars %s.", err))
	}
	secrets, secretsKnown, err := dynamicTemplateVars(ctx, m.SensitiveVars)
	if err != nil {
		diags.AddAttributeError(path.Root("sensitive_vars"), "Invalid template variables", fmt.Sprintf("The value of sensitive_vars %s.", err))
	}
	if diags.HasError() || !known || !secretsKnown {
		return "", false, diags
	}
	for k, v := range secrets {
		if _, dup := vars[k]; dup {
			diags.AddAttributeError(path.Root("sensitive_vars"), "Invalid template variables",
				fmt.Sprintf("Variable %q is set in both vars and sensitive_vars.", k))
			return "", false, diags
		}
		vars[k] = v
	}
	out, err = renderTemplate(m.syntax(), m.Template.ValueString(), vars)
	if err != nil {
		diags.AddAttributeError(path.Root("template"), "Template rendering failed",
			fmt.Sprintf("Rendering the %s template failed: %s.", syntaxLabel(m.syntax()), redact(err.Error(), sensitiveStrings(secrets))))
		return "", false, diags
	}
	return out, true, diags
}

// setRendered stores out and its checksums in m, in rendered_sensitive if
// sensitive_vars is set and in rendered otherwise.
func (m *templateFileModel) setRendered(out string) {
	sums, _ := checksumReader(strings.NewReader(out)) // Reading a string never fails.
	m.ContentSHA256, m.ContentMD5 = types.StringValue(sums.sha256), types.StringValue(sums.md5)
	if m.SensitiveVars.IsNull() {
		m.Rendered, m.RenderedSensitive = types.StringValue(out), types.StringNull()
	} else {
		m.Rendered, m.RenderedSensitive = types.StringNull(), types.StringValue(out)
	}
}

// dynamicTemplateVars converts a vars attribute into template variables; see
// templateVars.
func dynamicTemplateVars(ctx context.Context, d types.Dynamic) (map[string]cty.Value, bool, error) {
	if d.IsUnknown() || d.IsUnderlyingValueUnknown() {
		return nil, false, nil
	}
	v, err := d.ToTerraformValue(ctx)
	if err != nil {
		return nil, false, err
	}
	return templateVars(v)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func syntaxLabel(syntax string) string {
	if syntax == syntaxTerraform {
		return "Terraform"
	}
	return "Go"
}

// fileChecksum returns the checksums of the regular file target without
// following a symlink at target. Errors for a missing file wrap
// fs.ErrNotExist.
func fileChecksum(target string) (checksums, error) {
	f, err := openRegularNoFollow(target, os.O_RDONLY)
	if err != nil {
		return checksums{}, err
	}
	defer func() { _ = f.Close() }()
	return checksumReader(f)
}

// setFileOwnershipAndMode applies ownership and mode to the regular file
// target without following a symlink at target.
func setFileOwnershipAndMode(target, owner, group string, mode fs.FileMode) error {
	f, err := openRegularNoFollow(target, os.O_RDONLY)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return setOwnershipAndMode(f, owner, group, mode)
}

// openRegularNoFollow opens target without following a symlink at it, and
// fails unless the opened file is a regular file.
func openRegularNoFollow(target string, flag int) (*os.File, error) {
	if info, err := os.Lstat(target); err != nil {
		return nil, err
	} else if err := checkRegularFile(target, info); err != nil {
		return nil, err
	}
	f, err := openNoFollow(target, flag, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil {
		err = checkRegularFile(target, info)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// refreshTemplateFile refreshes m's checksums, mode, owner, group and id from
// the file at m.Path. It reports found=false if the file does not exist.
//
// If the file is valid UTF-8 and not larger than maxRenderedSize, its content
// is recorded in whichever of rendered and rendered_sensitive is in use, so
// that drift shows up as a diff (hidden for rendered_sensitive). After import
// neither is in use and only the checksums are recorded, so that the content
// of a file that may contain secrets is not put into a non-sensitive
// attribute. Drift is always detected through content_sha256.
func refreshTemplateFile(m *templateFileModel) (found bool, diags diag.Diagnostics) {
	target := m.Path.ValueString()
	f, err := openRegularNoFollow(target, os.O_RDONLY)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, diags
	case err != nil:
		diags.AddAttributeError(path.Root("path"), "Reading file", capitalize(err.Error())+".")
		return true, diags
	}
	defer func() { _ = f.Close() }()

	head, err := io.ReadAll(io.LimitReader(f, maxRenderedSize+1))
	if err != nil {
		diags.AddError("Reading file", err.Error())
		return true, diags
	}
	h := newChecksummer()
	_, _ = h.Write(head)
	if _, err := io.Copy(h, f); err != nil {
		diags.AddError("Reading file", err.Error())
		return true, diags
	}
	sums := h.sums()
	m.ContentSHA256, m.ContentMD5 = types.StringValue(sums.sha256), types.StringValue(sums.md5)
	if len(head) <= maxRenderedSize && utf8.Valid(head) {
		switch {
		case !m.RenderedSensitive.IsNull() && !m.RenderedSensitive.IsUnknown():
			m.RenderedSensitive = types.StringValue(string(head))
		case !m.Rendered.IsNull() && !m.Rendered.IsUnknown():
			m.Rendered = types.StringValue(string(head))
		}
	}

	info, err := f.Stat()
	if err != nil {
		diags.AddError("Stat failed", err.Error())
		return true, diags
	}
	m.Mode = types.StringValue(reconcileMode(knownString(m.Mode), info.Mode()))
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		diags.AddError("Stat failed", fmt.Sprintf("Unable to determine ownership of %q on this platform.", target))
		return true, diags
	}
	m.Owner = types.StringValue(reconcileOwner(knownString(m.Owner), st.Uid))
	m.Group = types.StringValue(reconcileGroup(knownString(m.Group), st.Gid))
	m.ID = m.Path
	return true, diags
}
