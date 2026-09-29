package provider

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                   = (*iniValueResource)(nil)
	_ resource.ResourceWithImportState    = (*iniValueResource)(nil)
	_ resource.ResourceWithValidateConfig = (*iniValueResource)(nil)
	_ resource.ResourceWithModifyPlan     = (*iniValueResource)(nil)
	_ resource.ResourceWithConfigure      = (*iniValueResource)(nil)
)

const (
	iniStatePresent = "present"
	iniStateAbsent  = "absent"
)

func NewIniValueResource() resource.Resource { return &iniValueResource{} }

type iniValueResource struct{ rootedResource }

type iniValueModel struct {
	Path      types.String `tfsdk:"path"`
	Section   types.String `tfsdk:"section"`
	Key       types.String `tfsdk:"key"`
	Value     types.String `tfsdk:"value"`
	State     types.String `tfsdk:"state"`
	Separator types.String `tfsdk:"separator"`
	Create    types.Bool   `tfsdk:"create"`
	ID        types.String `tfsdk:"id"`
}

func (r *iniValueResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_ini_value"
}

func (r *iniValueResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a single `key = value` setting inside a section of an INI-style file, such as a systemd drop-in, " +
			"`php.ini`, a git config or `sshd_config`, without taking over the rest of the file, like Ansible's `ini_file`. " +
			"Comments, ordering and unrelated content are kept; a missing section is created. " +
			"Changes are written atomically and keep the file's mode and ownership.",
		Attributes: map[string]schema.Attribute{
			"path": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Absolute path of the file to edit. " +
					"Must be in canonical form (no `.`/`..` segments, duplicate or trailing slashes) and must not be `/`. " +
					"A symlink at `path` is refused. Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{absolutePath()},
			},
			"section": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(""),
				MarkdownDescription: "Name of the section, as written between the brackets of its header, for example `Service` for `[Service]` " +
					"or `remote \"origin\"` for a git config. Empty (the default) means the global section: keys before the first section header. " +
					"Compared exactly, including case. Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{iniCheck("a valid INI section name", validateIniSection)},
			},
			"key": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Name of the key. Compared exactly, including case. " +
					"Must not contain the separator, and must not start with `#`, `;` or `[`. Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"value": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Value of the key. Required when `state` is `\"present\"` and must not be set when it is `\"absent\"`. " +
					"Written and compared literally, so quotes and trailing comments are part of the value. " +
					"Must be a single line without leading or trailing whitespace; may be empty. " +
					"If the key occurs more than once in the section, all its values are recorded, separated by line breaks, so the plan shows the duplicates.",
				Validators: []validator.String{iniCheck("a single line without leading or trailing whitespace", validateIniValue)},
			},
			"state": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(iniStatePresent),
				MarkdownDescription: "`\"present\"` to make `value` the key's only value, or `\"absent\"` to remove every occurrence of the key. " +
					"Refreshed from the file, so a key that was added or removed outside Terraform shows up as a change of `state`. Defaults to `\"present\"`.",
				Validators: []validator.String{stringvalidator.OneOf(iniStatePresent, iniStateAbsent)},
			},
			"separator": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(defaultIniSeparator),
				MarkdownDescription: "Written between key and value. Its non-whitespace part is the delimiter used to split existing lines into key and value, " +
					"for example `\" = \"`, `\"=\"` or `\": \"`; if it is whitespace only (`\" \"`, as in `sshd_config`), the key ends at the first whitespace. " +
					"Existing lines that already have the right value are not reformatted. Defaults to `\"" + defaultIniSeparator + "\"`.",
				Validators: []validator.String{iniCheck("a delimiter surrounded by optional whitespace, or whitespace only", validateIniSeparator)},
			},
			"create": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				MarkdownDescription: "Create the file (mode `0644`, owned by the user running Terraform, missing parent directories with mode `0755`) if it does not exist. " +
					"If `false`, a missing file is an error when `state` is `\"present\"`. Destroy never deletes the file. Defaults to `false`.",
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "`path`, `section` and `key`, separated by colons. The value is left out, so that a secret in it is not shown in plans as part of the id.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

// ValidateConfig checks the attributes that depend on each other.
func (r *iniValueResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg iniValueModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !cfg.Key.IsUnknown() && !cfg.Separator.IsUnknown() {
		separator := defaultIniSeparator
		if !cfg.Separator.IsNull() {
			separator = cfg.Separator.ValueString()
		}
		// An invalid separator is reported by its own validator.
		if validateIniSeparator(separator) == nil {
			if err := validateIniKey(cfg.Key.ValueString(), separator); err != nil {
				resp.Diagnostics.AddAttributeError(path.Root("key"), "Invalid key", capitalize(err.Error())+".")
			}
		}
	}
	if cfg.State.IsUnknown() || cfg.Value.IsUnknown() {
		return
	}
	absent := cfg.State.ValueString() == iniStateAbsent
	switch {
	case !absent && cfg.Value.IsNull():
		resp.Diagnostics.AddAttributeError(path.Root("value"), "Missing value",
			`"value" is required unless "state" is "absent".`)
	case absent && !cfg.Value.IsNull():
		resp.Diagnostics.AddAttributeError(path.Root("value"), "Unexpected value",
			`"value" must not be set when "state" is "absent"; every occurrence of the key is removed.`)
	}
}

// ModifyPlan computes the id, which only changes together with attributes
// that force replacement.
func (r *iniValueResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // Destroy.
	}
	var plan iniValueModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := types.StringUnknown()
	if !plan.Path.IsUnknown() && !plan.Section.IsUnknown() && !plan.Key.IsUnknown() {
		id = plan.id()
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("id"), id)...)
}

func (r *iniValueResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan iniValueModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.apply(&plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *iniValueResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state iniValueModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, diags := resolvePathAttr(r.root(), state.Path.ValueString(), false)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(refreshIniValue(target, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *iniValueResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	// path, section and key force replacement, so an update always
	// addresses the same key as the prior state.
	var plan iniValueModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.apply(&plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *iniValueResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state iniValueModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if state.State.ValueString() == iniStateAbsent {
		return // Nothing was added, so there is nothing to remove.
	}
	// State is not validated by the schema; never act on an invalid path.
	if err := validateAbsolutePath(state.Path.ValueString()); err != nil {
		resp.Diagnostics.AddError("Refusing to edit file", capitalize(err.Error())+".")
		return
	}
	spec, err := state.spec()
	if err != nil {
		resp.Diagnostics.AddError("Invalid state", capitalize(err.Error())+".")
		return
	}
	target, diags := resolvePathAttr(r.root(), state.Path.ValueString(), false)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	unlock, err := lockFileForEdit(target)
	if err != nil {
		resp.Diagnostics.AddError("Locking file", capitalize(err.Error())+".")
		return
	}
	defer unlock()

	data, snap, err := readRegularFileNoFollow(target, maxFileLineSize)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return // Nothing left to remove.
	case err != nil && isRefusedFileType(target):
		// The file was replaced by a symlink or something else after it was
		// last refreshed. Leave the replacement and whatever it points to
		// untouched.
		resp.Diagnostics.AddWarning("Managed key not removed",
			fmt.Sprintf("%s. The key was not removed; the resource is removed from state only.", capitalize(err.Error())))
		return
	case err != nil:
		resp.Diagnostics.AddError("Reading file", err.Error())
		return
	}
	f := parseIniFile(data)
	if !spec.remove(f) {
		return
	}
	if err := replaceFileAtomic(target, f.bytes(), snap, fileLineCreateMode); err != nil {
		resp.Diagnostics.AddError("Writing file", err.Error())
	}
}

// ImportState accepts "<path>:<section>:<key>". The path ends at the first
// colon and the key starts after the last one, so the section may contain
// colons (as in git's [url "https://example.com/"]) but the path and the
// key may not. The section is empty for global keys: "<path>::<key>".
func (r *iniValueResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	target, rest, ok1 := strings.Cut(req.ID, ":")
	i := strings.LastIndexByte(rest, ':')
	if !ok1 || i < 0 {
		resp.Diagnostics.AddError("Invalid import ID",
			fmt.Sprintf("Import ID must have the form <path>:<section>:<key>, with an empty section for global keys, got %q.", req.ID))
		return
	}
	section, key := rest[:i], rest[i+1:]
	if err := validateAbsolutePath(target); err != nil {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("Invalid path in import ID: %s.", err))
		return
	}
	if _, err := newIniSpec(section, key, defaultIniSeparator); err != nil {
		resp.Diagnostics.AddError("Invalid import ID", capitalize(err.Error())+".")
		return
	}
	m := iniValueModel{Path: types.StringValue(target), Section: types.StringValue(section), Key: types.StringValue(key)}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("path"), target)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("section"), section)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("key"), key)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), m.id())...)
}

// apply makes the file match plan. On success, plan is left unchanged: the
// file was verified to have exactly the planned content for the key.
func (r *iniValueResource) apply(plan *iniValueModel) diag.Diagnostics {
	var diags diag.Diagnostics
	// Config validation already enforces this; re-check as defense in depth.
	if err := validateAbsolutePath(plan.Path.ValueString()); err != nil {
		diags.AddAttributeError(path.Root("path"), "Invalid path", capitalize(err.Error())+".")
		return diags
	}
	spec, err := plan.spec()
	if err != nil {
		diags.AddError("Invalid configuration", capitalize(err.Error())+".")
		return diags
	}
	present := plan.State.ValueString() != iniStateAbsent
	if present && plan.Value.IsNull() {
		diags.AddAttributeError(path.Root("value"), "Missing value", `"value" is required unless "state" is "absent".`)
		return diags
	}
	target, diags := resolvePathAttr(r.root(), plan.Path.ValueString(), false)
	if diags.HasError() {
		return diags
	}

	unlock, err := lockFileForEdit(target)
	if err != nil {
		diags.AddError("Locking file", capitalize(err.Error())+".")
		return diags
	}
	defer unlock()

	data, snap, err := readRegularFileNoFollow(target, maxFileLineSize)
	switch {
	case errors.Is(err, fs.ErrNotExist) && !present:
		return diags // No file, no key.
	case errors.Is(err, fs.ErrNotExist) && !plan.Create.ValueBool():
		diags.AddAttributeError(path.Root("path"), "File does not exist",
			fmt.Sprintf("File %q does not exist. Set create = true to create it.", target))
		return diags
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			diags.AddError("Creating parent directory", err.Error())
			return diags
		}
		data, snap = nil, nil
	case err != nil:
		diags.AddAttributeError(path.Root("path"), "Reading file", capitalize(err.Error())+".")
		return diags
	}

	f := parseIniFile(data)
	var changed bool
	if present {
		if changed, err = spec.ensure(f, plan.Value.ValueString()); err != nil {
			diags.AddAttributeError(path.Root("value"), "Invalid value", capitalize(err.Error())+".")
			return diags
		}
	} else {
		changed = spec.remove(f)
	}
	if changed || snap == nil {
		if err := replaceFileAtomic(target, f.bytes(), snap, fileLineCreateMode); err != nil {
			diags.AddError("Writing file", err.Error())
			return diags
		}
	}

	// Verify the result the same way Read will see it, so a file that
	// cannot represent the planned value is reported now rather than as a
	// perpetual diff.
	got := *plan
	diags.Append(refreshIniValue(target, &got)...)
	if diags.HasError() {
		return diags
	}
	if got.State.ValueString() != plan.State.ValueString() || (present && got.Value.ValueString() != plan.Value.ValueString()) {
		diags.AddError("Writing file", fmt.Sprintf("Key %q in %q does not have the planned value immediately after writing it.", spec.key, target))
	}
	return diags
}

// refreshIniValue updates m from the file at the host path target. state
// becomes "present" if the key occurs in the file and "absent" if it or the
// file does not. If the key occurs, value becomes its value, or all its
// values separated by "\n" if it occurs more than once, which can never
// equal a configured value and so always shows up in the plan. If the key
// is missing, value is left as it was, so the plan only shows the change of
// state.
func refreshIniValue(target string, m *iniValueModel) diag.Diagnostics {
	var diags diag.Diagnostics
	if m.Section.IsNull() || m.Section.IsUnknown() {
		m.Section = types.StringValue("")
	}
	// The separator is only unknown right after import; see below.
	detect := m.Separator.IsNull() || m.Separator.IsUnknown()
	if detect {
		m.Separator = types.StringValue(defaultIniSeparator)
	}
	if m.Create.IsNull() || m.Create.IsUnknown() {
		m.Create = types.BoolValue(false)
	}
	m.ID = m.id()
	spec, err := m.spec()
	if err != nil {
		diags.AddError("Invalid state", capitalize(err.Error())+".")
		return diags
	}

	data, _, err := readRegularFileNoFollow(target, maxFileLineSize)
	var values []string
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		diags.AddAttributeError(path.Root("path"), "Reading file", capitalize(err.Error())+".")
		return diags
	default:
		f := parseIniFile(data)
		values = spec.values(f)
		if detect && len(values) == 0 {
			// The import ID has no separator. Try the other common
			// styles, so that a key such as sshd_config's
			// "PermitRootLogin no" is found as well.
			for _, sep := range iniImportSeparators {
				alt, err := newIniSpec(spec.section, spec.key, sep)
				if err != nil {
					continue // The key cannot be written with sep.
				}
				if values = alt.values(f); len(values) > 0 {
					m.Separator = types.StringValue(sep)
					break
				}
			}
		}
	}

	if len(values) == 0 {
		m.State = types.StringValue(iniStateAbsent)
		return diags
	}
	m.State = types.StringValue(iniStatePresent)
	m.Value = types.StringValue(strings.ToValidUTF8(strings.Join(values, "\n"), "�"))
	return diags
}

// iniImportSeparators are tried in order, after defaultIniSeparator, to find
// an imported key.
var iniImportSeparators = []string{": ", " "}

func (m *iniValueModel) spec() (*iniSpec, error) {
	separator := m.Separator.ValueString()
	if m.Separator.IsNull() {
		separator = defaultIniSeparator
	}
	return newIniSpec(m.Section.ValueString(), m.Key.ValueString(), separator)
}

// id returns "<path>:<section>:<key>", the import ID of m. The value is
// deliberately left out: it may be a secret, and Terraform does not carry
// the sensitivity of value over to the computed id.
func (m *iniValueModel) id() types.String {
	return types.StringValue(strings.Join([]string{m.Path.ValueString(), m.Section.ValueString(), m.Key.ValueString()}, ":"))
}

// iniCheckValidator adapts a validation function to a string validator.
type iniCheckValidator struct {
	desc  string
	check func(string) error
}

func iniCheck(desc string, check func(string) error) validator.String {
	return iniCheckValidator{desc: desc, check: check}
}

func (v iniCheckValidator) Description(_ context.Context) string {
	return "value must be " + v.desc
}

func (v iniCheckValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v iniCheckValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if err := v.check(req.ConfigValue.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid "+req.Path.String(), capitalize(err.Error())+".")
	}
}
