package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/resourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                     = (*alternativesResource)(nil)
	_ resource.ResourceWithModifyPlan       = (*alternativesResource)(nil)
	_ resource.ResourceWithConfigure        = (*alternativesResource)(nil)
	_ resource.ResourceWithImportState      = (*alternativesResource)(nil)
	_ resource.ResourceWithConfigValidators = (*alternativesResource)(nil)
	_ resource.ResourceWithValidateConfig   = (*alternativesResource)(nil)
)

func NewAlternativesResource() resource.Resource { return &alternativesResource{} }

type alternativesResource struct {
	hostOnlyResource
	cfg *alternativesConfig
}

type alternativesModel struct {
	Name            types.String `tfsdk:"name"`
	Path            types.String `tfsdk:"path"`
	Link            types.String `tfsdk:"link"`
	Priority        types.Int64  `tfsdk:"priority"`
	Mode            types.String `tfsdk:"mode"`
	RemoveOnDestroy types.Bool   `tfsdk:"remove_on_destroy"`
	ID              types.String `tfsdk:"id"`
}

func (r *alternativesResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_alternatives"
}

func (r *alternativesResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Selects the alternative a link group of the alternatives system points to, such as which program `/usr/bin/editor` or `java` runs, " +
			"with `update-alternatives` on Debian, Ubuntu and SUSE or `alternatives` on Fedora and RHEL (detected automatically). " +
			"The selection is made in manual mode, so that installing a package with a higher-priority alternative does not change it. " +
			"With `link` and `priority`, an alternative that no package registered is registered first (`--install`). " +
			"The link group is read with `update-alternatives --query` or `alternatives --display`, so a different selection, a return to automatic mode or a removed alternative shows up as drift. " +
			"On destroy the link group returns to automatic mode or, with `remove_on_destroy`, the alternative is unregistered. " +
			"Commands are serialised with package manager commands of the provider, which run the same tools from package scripts. Requires root privileges.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Name of the link group, such as `\"editor\"` or `\"java\"`. ASCII letters, digits, `_`, `.`, `+`, `@`, `:`, `~` and `-`, starting with a letter, digit or `_`. " +
					"Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringCheck("alternatives name", validateAlternativesName)},
			},
			"path": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The alternative to select, such as `\"/usr/bin/vim.basic\"`: an absolute, canonical path without white space. " +
					"It must be registered in the link group already, unless `link` and `priority` are set to register it. " +
					"Because root runs it, it must belong to root (or the provider's user) and not be writable by others, and so must every directory and symlink on the way to it; otherwise nothing is changed and apply fails.",
				Validators: []validator.String{stringCheck("path", validateAlternativesPath)},
			},
			"link": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "The link group's master link, such as `\"/usr/bin/editor\"`. With `priority`, `path` is registered with `--install <link> <name> <path> <priority>` whenever it is not registered, " +
					"or the group's link or the alternative's priority differ; these then count as drift. An existing group's link is renamed to it. " +
					"Its directory, and every directory above it, must belong to root (or the provider's user) and not be writable by others, since the tool follows symlinks there. Requires `priority`.",
				Validators: []validator.String{stringCheck("link", validateAlternativesPath)},
			},
			"priority": schema.Int64Attribute{
				Optional: true,
				MarkdownDescription: "Priority `path` is registered with, used to pick an alternative in automatic mode; the highest wins. " +
					"A 32-bit signed integer. Requires `link`.",
				Validators: []validator.Int64{int64validator.Between(math.MinInt32, math.MaxInt32)},
			},
			"mode": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Mode of the link group: `manual` once the resource selected `path`, `auto` if the group was returned to automatic mode outside Terraform. " +
					"The resource always plans `manual`, so a return to automatic mode is drift that the next apply reverts.",
				PlanModifiers: []planmodifier.String{alternativesManualMode{}},
			},
			"remove_on_destroy": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				MarkdownDescription: "What happens on destroy. If `false`, the link group returns to automatic mode (`--auto`), and `path` stays registered. " +
					"If `true`, the alternative this resource last selected is unregistered (`--remove`), which selects the best remaining alternative, or removes the link group and its links if it was the last one. " +
					"An alternative selected outside Terraform since, which refresh reports as `path`, is never unregistered. " +
					"Use `true` for alternatives registered through `link` and `priority`. Defaults to `false`.",
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Resource identifier (equal to `name`).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

// alternativesManualMode plans mode = "manual" whenever the resource is
// created or updated, which is what applying it establishes. A link group
// that was returned to automatic mode outside Terraform therefore shows a
// diff.
type alternativesManualMode struct{}

func (alternativesManualMode) Description(context.Context) string {
	return "The mode is always planned as manual."
}

func (m alternativesManualMode) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (alternativesManualMode) PlanModifyString(_ context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}
	resp.PlanValue = types.StringValue(alternativesModeManual)
}

func (r *alternativesResource) ConfigValidators(context.Context) []resource.ConfigValidator {
	return []resource.ConfigValidator{
		resourcevalidator.RequiredTogether(path.MatchRoot("link"), path.MatchRoot("priority")),
	}
}

func (r *alternativesResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg alternativesModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if cfg.Link.IsNull() || cfg.Link.IsUnknown() || cfg.Path.IsNull() || cfg.Path.IsUnknown() {
		return
	}
	if cfg.Link.ValueString() == cfg.Path.ValueString() {
		resp.Diagnostics.AddAttributeError(path.Root("link"), "Invalid link",
			"The link must differ from path: the link is the symlink that points to the selected alternative.")
	}
}

func (r *alternativesResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	data, diags := providerDataFrom(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	if data != nil {
		r.cfg = data.alternatives
		r.fsRoot = data.root
	}
}

// lockedTool detects the tool and takes the package-manager lock: dpkg and
// rpm run the same tools from package scripts while holding their database
// lock, and two changes to one link group at once would lose one of them.
func (r *alternativesResource) lockedTool(ctx context.Context) (*alternativesTool, func(), diag.Diagnostics) {
	var diags diag.Diagnostics
	tool, err := r.cfg.tool()
	if err != nil {
		diags.AddError("Alternatives tool not found", capitalize(err.Error())+".")
		return nil, nil, diags
	}
	unlock, err := lockPackageManager(ctx)
	if err != nil {
		diags.AddError("Locking package manager", capitalize(err.Error())+".")
		return nil, nil, diags
	}
	return tool, unlock, diags
}

func (r *alternativesResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan alternativesModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	tool, unlock, diags := r.lockedTool(ctx)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	defer unlock()
	plan.ID = types.StringValue(plan.Name.ValueString())
	changed, diags := r.apply(ctx, tool, &plan)
	resp.Diagnostics.Append(diags...)
	// After a partial apply, record the resource so that it is tainted and
	// destroy can clean up.
	if !resp.Diagnostics.HasError() || changed {
		resp.Diagnostics.Append(writeAppliedAlternative(ctx, resp.Private, plan.Path.ValueString())...)
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	}
}

func (r *alternativesResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state alternativesModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	tool, unlock, diags := r.lockedTool(ctx)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	defer unlock()
	// State written by versions without private state: until the next
	// apply, the path in state stands for the applied alternative, as long
	// as refresh has not replaced it.
	if _, found, diags := readAppliedAlternative(ctx, req.Private); !diags.HasError() && !found &&
		!state.Mode.IsNull() && state.Path.ValueString() != "" {
		resp.Diagnostics.Append(writeAppliedAlternative(ctx, resp.Private, state.Path.ValueString())...)
	}
	gone, diags := r.refresh(ctx, tool, &state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if gone {
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *alternativesResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan alternativesModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	tool, unlock, diags := r.lockedTool(ctx)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	defer unlock()
	plan.ID = types.StringValue(plan.Name.ValueString())
	_, diags = r.apply(ctx, tool, &plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(writeAppliedAlternative(ctx, resp.Private, plan.Path.ValueString())...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *alternativesResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state alternativesModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// State is not validated by the schema; never pass an invalid name or
	// path to the tool.
	name, p := state.Name.ValueString(), state.Path.ValueString()
	if err := validateAlternativesName(name); err != nil {
		resp.Diagnostics.AddError("Invalid state", capitalize(err.Error())+".")
		return
	}
	remove := state.RemoveOnDestroy.ValueBool()
	// Refresh replaces path with whatever the link group points to now,
	// which may be an alternative that someone else selected. Only the one
	// this resource applied is ever unregistered.
	applied, found, diags := readAppliedAlternative(ctx, req.Private)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if found {
		p = applied
	}
	if remove && p == "" {
		// Nothing of this resource's to unregister.
		return
	}
	if remove {
		if err := validateAlternativesPath(p); err != nil {
			resp.Diagnostics.AddError("Invalid state", capitalize(err.Error())+".")
			return
		}
	}
	tool, unlock, diags := r.lockedTool(ctx)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	defer unlock()
	st, err := tool.Query(ctx, name)
	if err != nil {
		resp.Diagnostics.AddError("Reading alternatives", capitalize(err.Error())+".")
		return
	}
	if !st.Found {
		return
	}
	if remove {
		if _, ok := st.entry(p); !ok {
			return
		}
		if err := tool.Remove(ctx, name, p); err != nil {
			resp.Diagnostics.AddError("Removing alternative", capitalize(err.Error())+".")
		}
		return
	}
	if st.Mode == alternativesModeAuto {
		return
	}
	if err := tool.Auto(ctx, name); err != nil {
		resp.Diagnostics.AddError("Returning to automatic mode", capitalize(err.Error())+".")
	}
}

// ImportState imports a link group by name. The alternative it points to
// becomes path; link and priority stay unset.
func (r *alternativesResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if err := validateAlternativesName(req.ID); err != nil {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("Import ID must be the name of a link group: %s.", err))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("remove_on_destroy"), false)...)
}

// apply registers plan's alternative if needed and selects it in manual
// mode. It reports whether the host may have been changed, so that a failed
// create can still be recorded.
func (r *alternativesResource) apply(ctx context.Context, tool *alternativesTool, plan *alternativesModel) (changed bool, diags diag.Diagnostics) {
	name, p := plan.Name.ValueString(), plan.Path.ValueString()
	if err := validateAlternativesName(name); err != nil {
		diags.AddAttributeError(path.Root("name"), "Invalid name", capitalize(err.Error())+".")
		return false, diags
	}
	if err := validateAlternativesPath(p); err != nil {
		diags.AddAttributeError(path.Root("path"), "Invalid path", capitalize(err.Error())+".")
		return false, diags
	}
	install := !plan.Link.IsNull()
	if install {
		if err := validateAlternativesPath(plan.Link.ValueString()); err != nil {
			diags.AddAttributeError(path.Root("link"), "Invalid link", capitalize(err.Error())+".")
			return false, diags
		}
	}

	st, err := tool.Query(ctx, name)
	if err != nil {
		diags.AddError("Reading alternatives", capitalize(err.Error())+".")
		return false, diags
	}
	entry, registered := st.entry(p)
	checkPaths := r.cfg.checkPathsFn()
	if install {
		link, priority := plan.Link.ValueString(), plan.Priority.ValueInt64()
		if !registered || st.Link != link || entry.Priority != priority {
			if err := checkPaths(link, p); err != nil {
				diags.AddAttributeError(path.Root("path"), "Untrusted path", capitalize(err.Error())+".")
				return false, diags
			}
			changed = true
			if err := tool.Install(ctx, link, name, p, priority); err != nil {
				diags.AddAttributeError(path.Root("path"), "Registering alternative", capitalize(err.Error())+".")
				return changed, diags
			}
			if st, err = tool.Query(ctx, name); err != nil {
				diags.AddError("Reading alternatives", capitalize(err.Error())+".")
				return changed, diags
			}
			if _, registered = st.entry(p); !registered {
				diags.AddAttributeError(path.Root("path"), "Alternative not registered",
					fmt.Sprintf("%s --install succeeded, but %s is not an alternative of %s afterwards.", tool.kind, p, name))
				return changed, diags
			}
		}
	} else if !registered {
		switch {
		case !st.Found:
			diags.AddAttributeError(path.Root("name"), "Unknown link group",
				fmt.Sprintf("%s knows no link group %q. Set link and priority to create it with %s as its alternative.", tool.kind, name, p))
		default:
			diags.AddAttributeError(path.Root("path"), "Alternative not registered",
				fmt.Sprintf("%s is not an alternative of %s; the registered alternatives are %s. Set link and priority to register it.", p, name, alternativesPaths(st)))
		}
		return false, diags
	}

	if st.Mode != alternativesModeManual || st.Value != p {
		if err := checkPaths("", p); err != nil {
			diags.AddAttributeError(path.Root("path"), "Untrusted path", capitalize(err.Error())+".")
			return changed, diags
		}
		changed = true
		if err := tool.Set(ctx, name, p); err != nil {
			diags.AddAttributeError(path.Root("path"), "Selecting alternative", capitalize(err.Error())+".")
			return changed, diags
		}
		if st, err = tool.Query(ctx, name); err != nil {
			diags.AddError("Reading alternatives", capitalize(err.Error())+".")
			return changed, diags
		}
		if st.Mode != alternativesModeManual || st.selected() != p {
			diags.AddAttributeError(path.Root("path"), "Alternative not selected",
				fmt.Sprintf("%s --set succeeded, but %s points to %q in %s mode afterwards.", tool.kind, name, st.Value, st.Mode))
			return changed, diags
		}
	}
	plan.Mode = types.StringValue(alternativesModeManual)
	return changed, diags
}

// refresh updates m from the link group. It reports gone if the link group
// does not exist.
func (r *alternativesResource) refresh(ctx context.Context, tool *alternativesTool, m *alternativesModel) (gone bool, diags diag.Diagnostics) {
	name := m.Name.ValueString()
	if err := validateAlternativesName(name); err != nil {
		diags.AddError("Invalid state", capitalize(err.Error())+".")
		return false, diags
	}
	// Mode is set by every create and refresh, so it is only null right
	// after import. Path can't tell: it is empty, not null, while the link
	// group points to no registered alternative.
	imported := m.Mode.IsNull()
	st, err := tool.Query(ctx, name)
	if err != nil {
		diags.AddError("Reading alternatives", capitalize(err.Error())+".")
		return false, diags
	}
	if !st.Found {
		if imported {
			diags.AddError("Cannot import alternatives", fmt.Sprintf("%s knows no link group %q.", tool.kind, name))
		}
		return true, diags
	}

	// The priority is that of the alternative last applied, looked up
	// before path is overwritten with the current selection. If it is no
	// longer registered, the path differs anyway.
	if !m.Priority.IsNull() {
		if e, ok := st.entry(m.Path.ValueString()); ok {
			m.Priority = types.Int64Value(e.Priority)
		}
	}
	if !m.Link.IsNull() {
		m.Link = types.StringValue(st.Link)
	}
	// An empty path, which the configuration cannot contain, stands for a
	// link group that points to no registered alternative.
	m.Path = types.StringValue(st.selected())
	m.Mode = types.StringValue(st.Mode)
	m.ID = types.StringValue(name)
	if m.RemoveOnDestroy.IsNull() {
		m.RemoveOnDestroy = types.BoolValue(false)
	}
	return false, diags
}

// alternativesPaths lists the registered alternatives for messages.
func alternativesPaths(st alternativesStatus) string {
	if len(st.Alternatives) == 0 {
		return "none"
	}
	s := ""
	for i, e := range st.Alternatives {
		if i > 0 {
			s += ", "
		}
		s += e.Path
	}
	return s
}

// privateAppliedAlternative is the key in the resource's private state of
// the alternative the resource last selected, as a JSON string. It is not
// the path attribute, which refresh replaces with the current selection to
// show drift.
const privateAppliedAlternative = "applied_path"

// readAppliedAlternative returns the path stored under
// privateAppliedAlternative; found is false if none is.
func readAppliedAlternative(ctx context.Context, p interface {
	GetKey(context.Context, string) ([]byte, diag.Diagnostics)
}) (applied string, found bool, diags diag.Diagnostics) {
	data, diags := p.GetKey(ctx, privateAppliedAlternative)
	if diags.HasError() || len(data) == 0 {
		return "", false, diags
	}
	if err := json.Unmarshal(data, &applied); err != nil {
		diags.AddError("Invalid private state", fmt.Sprintf("Unable to decode %s: %s.", privateAppliedAlternative, err))
		return "", false, diags
	}
	if applied != "" {
		if err := validateAlternativesPath(applied); err != nil {
			diags.AddError("Invalid private state", capitalize(err.Error())+".")
			return "", false, diags
		}
	}
	return applied, true, diags
}

// writeAppliedAlternative stores p under privateAppliedAlternative.
func writeAppliedAlternative(ctx context.Context, priv interface {
	SetKey(context.Context, string, []byte) diag.Diagnostics
}, p string) diag.Diagnostics {
	data, err := json.Marshal(p)
	if err != nil {
		var diags diag.Diagnostics
		diags.AddError("Encoding private state", err.Error())
		return diags
	}
	return priv.SetKey(ctx, privateAppliedAlternative, data)
}
