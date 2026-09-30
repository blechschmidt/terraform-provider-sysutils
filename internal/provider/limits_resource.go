package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                   = (*limitsResource)(nil)
	_ resource.ResourceWithConfigure      = (*limitsResource)(nil)
	_ resource.ResourceWithImportState    = (*limitsResource)(nil)
	_ resource.ResourceWithModifyPlan     = (*limitsResource)(nil)
	_ resource.ResourceWithValidateConfig = (*limitsResource)(nil)
)

func NewLimitsResource() resource.Resource { return &limitsResource{} }

type limitsResource struct{ rootedResource }

type limitsModel struct {
	Domain types.String `tfsdk:"domain"`
	Type   types.String `tfsdk:"type"`
	Item   types.String `tfsdk:"item"`
	Value  types.String `tfsdk:"value"`
	File   types.String `tfsdk:"file"`
	Path   types.String `tfsdk:"path"`
	ID     types.String `tfsdk:"id"`
}

func (r *limitsResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_limits"
}

func (r *limitsResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages one `pam_limits` entry, `<domain> <type> <item> <value>`, in a drop-in file in `/etc/security/limits.d`, below the provider's `root_dir` if that is set. " +
			"Several `sysutils_limits` resources can share a file; comments and all other lines in it are kept, and the file's mode and ownership are preserved. " +
			"Changes are written atomically. On destroy only the entry is removed, and the file too if the provider created it and nothing else is left in it. " +
			"The limits apply to sessions that start after the change. Requires root privileges, unless used with `root_dir` on a tree you can write.",
		Attributes: map[string]schema.Attribute{
			"domain": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Who the limit applies to: a user name such as `\"postgres\"`, a group as `\"@developers\"`, `\"*\"` for everyone, " +
					"a UID range as `\"1000:1999\"`, `\"1000:\"` (1000 and above) or `\":1000\"` (exactly 1000), a GID range as `\"@1000:1999\"`, " +
					"or, for the `maxlogins` and `maxsyslogins` items only, `\"%\"`, `\"%group\"` or `\"%:<gid>\"`. " +
					"Names may only contain letters, digits and `_.-`, and a final `$`; a numeric name is refused, as `pam_limits` would compare it with names, not IDs. " +
					"Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringCheck("limits domain", validateLimitsDomain)},
			},
			"type": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "`\"soft\"` for the limit processes start with, `\"hard\"` for the ceiling up to which unprivileged processes may raise it, or `\"-\"` for both. " +
					"Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringCheck("limits type", validateLimitsType)},
			},
			"item": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The limit to set: `" + strings.Join(limitsItemNames(), "`, `") + "`. " +
					"See `limits.conf(5)` for what each one means and its unit. Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringCheck("limits item", validateLimitsItem)},
			},
			"value": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The value, as a string: a non-negative integer, or `\"unlimited\"`, `\"infinity\"` or `\"-1\"` for no limit. " +
					"`nice` and `priority` take a nice value from `-20` to `19` and have no unlimited; `nonewprivs` takes `0` or `1`. " +
					"The spellings of unlimited, and numbers with leading zeros, are compared as the same value, so they are not drift.",
				Validators: []validator.String{stringCheck("limits value", validateLimitsValueSyntax)},
			},
			"file": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Name of the drop-in file in `/etc/security/limits.d`, such as `\"90-postgres.conf\"`. " +
					"It is created with mode `0644` if needed, starting with a comment line that marks it as created by the provider. " +
					"Must be a plain file name ending in `.conf`, of letters, digits and `_.-+@`, not starting with `.` or `-`: `pam_limits` ignores other files, and a name can't refer to a file outside the directory. " +
					"Defaults to a name derived from `domain`: `\"90-terraform-user-postgres.conf\"` for `\"postgres\"`, `\"90-terraform-group-developers.conf\"` for `\"@developers\"`, " +
					"`\"90-terraform-default.conf\"` for `\"*\"` and `\"90-terraform-uid-1000-1999.conf\"` for `\"1000:1999\"`, so entries for one domain share a file. " +
					"Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{limitsFileDefault{}, stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringCheck("limits.d file name", validateLimitsFileName)},
			},
			"path": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Absolute path of the drop-in file, `/etc/security/limits.d/<file>`, inside the provider's `root_dir` if set.",
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "`<path>:<domain>:<type>:<item>`, the import ID.",
			},
		},
	}
}

// limitsFileDefault plans the default file name, derived from domain, when
// file is not configured.
type limitsFileDefault struct{}

func (limitsFileDefault) Description(context.Context) string {
	return "defaults to a file name derived from domain"
}

func (m limitsFileDefault) MarkdownDescription(ctx context.Context) string { return m.Description(ctx) }

func (limitsFileDefault) PlanModifyString(ctx context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse) {
	if !req.ConfigValue.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	var domain types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("domain"), &domain)...)
	if resp.Diagnostics.HasError() {
		return
	}
	switch {
	case domain.IsUnknown():
		resp.PlanValue = types.StringUnknown()
	case domain.IsNull() || validateLimitsDomain(domain.ValueString()) != nil:
		// Reported by the domain's validator.
	default:
		resp.PlanValue = types.StringValue(defaultLimitsFileName(domain.ValueString()))
	}
}

// ValidateConfig checks the value against the item, and the item against
// the domain.
func (r *limitsResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg limitsModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	known := func(v types.String) bool { return !v.IsNull() && !v.IsUnknown() }
	if !known(cfg.Item) || validateLimitsItem(cfg.Item.ValueString()) != nil {
		return
	}
	item := cfg.Item.ValueString()
	if known(cfg.Value) {
		if err := validateLimitsValue(item, cfg.Value.ValueString()); err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("value"), "Invalid limits value", capitalize(err.Error())+".")
		}
	}
	if known(cfg.Domain) && validateLimitsDomain(cfg.Domain.ValueString()) == nil {
		if err := validateLimitsDomainItem(cfg.Domain.ValueString(), item); err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("item"), "Item not supported for domain", capitalize(err.Error())+".")
		}
	}
}

// ModifyPlan computes path and id from file and the entry's key.
func (r *limitsResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // Destroy.
	}
	var plan limitsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	p, id := types.StringUnknown(), types.StringUnknown()
	if !plan.File.IsUnknown() && !plan.File.IsNull() {
		p = types.StringValue(limitsFilePath(plan.File.ValueString()))
		if k, ok := plan.knownKey(); ok {
			id = types.StringValue(limitsID(p.ValueString(), k))
		}
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("path"), p)...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("id"), id)...)
}

func limitsID(p string, k limitsKey) string {
	return p + ":" + k.domain + ":" + k.typ + ":" + k.item
}

// knownKey returns the key of the entry in m if domain, type and item are
// known.
func (m *limitsModel) knownKey() (limitsKey, bool) {
	for _, v := range []types.String{m.Domain, m.Type, m.Item} {
		if v.IsNull() || v.IsUnknown() {
			return limitsKey{}, false
		}
	}
	return limitsKey{m.Domain.ValueString(), m.Type.ValueString(), m.Item.ValueString()}, true
}

// check validates the key and file in m, which comes from a plan or from
// state that the schema did not validate, and returns the key and the
// managed path of the file. With withValue, the value is checked as well.
func (m *limitsModel) check(withValue bool) (limitsKey, string, error) {
	k, ok := m.knownKey()
	if !ok {
		return limitsKey{}, "", fmt.Errorf("domain, type and item must be set")
	}
	if err := validateLimitsDomain(k.domain); err != nil {
		return k, "", err
	}
	if err := validateLimitsType(k.typ); err != nil {
		return k, "", err
	}
	if err := validateLimitsItem(k.item); err != nil {
		return k, "", err
	}
	if err := validateLimitsDomainItem(k.domain, k.item); err != nil {
		return k, "", err
	}
	if withValue {
		if err := validateLimitsValue(k.item, m.Value.ValueString()); err != nil {
			return k, "", err
		}
	}
	if err := validateLimitsFileName(m.File.ValueString()); err != nil {
		return k, "", err
	}
	return k, limitsFilePath(m.File.ValueString()), nil
}

func (r *limitsResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan limitsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.apply(&plan, true)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *limitsResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan limitsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.apply(&plan, false)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// apply makes the drop-in file hold the entry in plan. On create, an entry
// with the same key that is already in the file is refused, because
// destroying the resource would remove it.
func (r *limitsResource) apply(plan *limitsModel, create bool) diag.Diagnostics {
	var diags diag.Diagnostics
	k, p, err := plan.check(true)
	if err != nil {
		diags.AddError("Invalid limits entry", capitalize(err.Error())+".")
		return diags
	}
	target, d := resolvePathAttr(r.root(), p, false)
	diags.Append(d...)
	if diags.HasError() {
		return diags
	}
	e := limitsEntry{domain: k.domain, typ: k.typ, item: k.item, value: plan.Value.ValueString()}
	var existing string
	err = editLimitsFile(target, func(t *textFile) bool {
		if create {
			if v, n := lookupLimitsEntry(t, k); n > 0 {
				existing = v
				return false
			}
		}
		return setLimitsEntry(t, e)
	})
	if err != nil {
		diags.AddAttributeError(path.Root("file"), "Editing limits file", fmt.Sprintf("Setting %s %s %s in %s: %s.", k.domain, k.typ, k.item, p, err))
		return diags
	}
	if existing != "" {
		diags.AddAttributeError(path.Root("item"), "Entry already in file",
			fmt.Sprintf("%s already sets %s %s %s to %s. Import it with \"terraform import\" and the ID %q instead, or remove the entry.",
				p, k.domain, k.typ, k.item, existing, limitsID(p, k)))
		return diags
	}
	plan.Path = types.StringValue(p)
	plan.ID = types.StringValue(limitsID(p, k))
	return diags
}

func (r *limitsResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state limitsModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	found, diags := r.refresh(&state)
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

// refresh updates the value in m from the drop-in file. It reports
// found=false if the file or the entry is missing. After import, m holds no
// value yet, and a missing entry is an error.
func (r *limitsResource) refresh(m *limitsModel) (found bool, diags diag.Diagnostics) {
	imported := m.Value.IsNull()
	k, p, err := m.check(false)
	if err != nil {
		diags.AddError("Invalid state", capitalize(err.Error())+".")
		return false, diags
	}
	target, d := resolvePathAttr(r.root(), p, false)
	diags.Append(d...)
	if diags.HasError() {
		return false, diags
	}
	t, _, err := readLimitsFile(target)
	if err != nil {
		diags.AddAttributeError(path.Root("file"), "Reading limits file", capitalize(err.Error())+".")
		return false, diags
	}
	v, n := lookupLimitsEntry(t, k)
	if n == 0 {
		if imported {
			diags.AddError("Cannot import limits entry", fmt.Sprintf("%s has no entry %s %s %s.", p, k.domain, k.typ, k.item))
		}
		return false, diags
	}
	if n > 1 {
		diags.AddWarning("Duplicate limits entries",
			fmt.Sprintf("%s sets %s %s %s %d times. pam_limits uses the last one; the next apply that changes this resource removes the others.", p, k.domain, k.typ, k.item, n))
	}
	if imported || !limitsValuesEqual(v, m.Value.ValueString()) {
		m.Value = types.StringValue(v)
	}
	m.Path = types.StringValue(p)
	m.ID = types.StringValue(limitsID(p, k))
	return true, diags
}

func (r *limitsResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state limitsModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// State is not validated by the schema; never edit an arbitrary file.
	k, p, err := state.check(false)
	if err != nil {
		resp.Diagnostics.AddError("Invalid state", capitalize(err.Error())+".")
		return
	}
	target, diags := resolvePathAttr(r.root(), p, false)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	err = editLimitsFile(target, func(t *textFile) bool { return removeLimitsEntries(t, k) })
	switch {
	case err == nil:
	case isRefusedFileType(target):
		// The file was replaced by a symlink or something else after it was
		// last refreshed. Leave the replacement and whatever it points to
		// untouched.
		resp.Diagnostics.AddWarning("Limits entry not removed",
			fmt.Sprintf("%s. The entry was not removed; the resource is removed from state only.", capitalize(err.Error())))
	default:
		resp.Diagnostics.AddError("Editing limits file", fmt.Sprintf("Removing %s %s %s from %s: %s.", k.domain, k.typ, k.item, p, err))
	}
}

// ImportState accepts "<path>:<domain>:<type>:<item>", such as
// "/etc/security/limits.d/90-postgres.conf:postgres:soft:nofile". The path
// ends at the first colon, and type and item are the last two fields, so a
// domain may contain colons, as UID and GID ranges do.
func (r *limitsResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	fail := func(msg string) {
		resp.Diagnostics.AddError("Invalid import ID",
			fmt.Sprintf("Import ID must have the form <path>:<domain>:<type>:<item>, such as %q: %s.",
				defaultLimitsDir+"/90-postgres.conf:postgres:soft:nofile", msg))
	}
	p, rest, ok := strings.Cut(req.ID, ":")
	i := strings.LastIndexByte(rest, ':')
	if !ok || i < 0 {
		fail(fmt.Sprintf("got %q", req.ID))
		return
	}
	item := rest[i+1:]
	rest = rest[:i]
	i = strings.LastIndexByte(rest, ':')
	if i < 0 {
		fail(fmt.Sprintf("got %q", req.ID))
		return
	}
	domain, typ := rest[:i], rest[i+1:]
	file, err := limitsFileNameFromPath(p)
	if err != nil {
		fail(err.Error())
		return
	}
	m := limitsModel{Domain: types.StringValue(domain), Type: types.StringValue(typ), Item: types.StringValue(item), File: types.StringValue(file)}
	if _, _, err := m.check(false); err != nil {
		fail(err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("domain"), domain)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("type"), typ)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("item"), item)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("file"), file)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("path"), p)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), limitsID(p, limitsKey{domain, typ, item}))...)
}
