package provider

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
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
	_ resource.Resource                   = (*hostsEntryResource)(nil)
	_ resource.ResourceWithConfigure      = (*hostsEntryResource)(nil)
	_ resource.ResourceWithImportState    = (*hostsEntryResource)(nil)
	_ resource.ResourceWithModifyPlan     = (*hostsEntryResource)(nil)
	_ resource.ResourceWithValidateConfig = (*hostsEntryResource)(nil)
)

const defaultHostsPath = "/etc/hosts"

func NewHostsEntryResource() resource.Resource { return &hostsEntryResource{} }

type hostsEntryResource struct{ rootedResource }

type hostsEntryModel struct {
	IP             types.String `tfsdk:"ip"`
	Hostnames      types.List   `tfsdk:"hostnames"`
	Comment        types.String `tfsdk:"comment"`
	Path           types.String `tfsdk:"path"`
	AllowDuplicate types.Bool   `tfsdk:"allow_duplicate"`
	ID             types.String `tfsdk:"id"`
}

func (r *hostsEntryResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_hosts_entry"
}

func (r *hostsEntryResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages one line of `/etc/hosts` that maps an IP address to hostnames. " +
			"All other lines, comments and blank lines are kept, and so are the file's mode, owner and group. " +
			"Changes are written atomically. On destroy, only the managed line is removed.",
		Attributes: map[string]schema.Attribute{
			"ip": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "IPv4 address in dotted-decimal form, such as `\"10.0.0.5\"`, or IPv6 address, such as `\"fd00::5\"`, without a zone. " +
					"Compared as an address, so `\"fd00::5\"` also finds a line written as `fd00:0:0:0:0:0:0:5`. " +
					"Changing it rewrites the managed line in place.",
				Validators: []validator.String{stringCheck("IP address", validateHostsIP)},
			},
			"hostnames": schema.ListAttribute{
				ElementType: types.StringType,
				Required:    true,
				MarkdownDescription: "Hostnames for `ip`, in order. The first is the canonical name, which reverse lookups of `ip` return; the others are aliases. " +
					"Each must be a hostname as defined by RFC 1123: labels of letters, digits and hyphens separated by dots, without a trailing dot. " +
					"Names must be unique, ignoring case. The line whose address is `ip` and whose first hostname is the first of these, ignoring case, is the managed line.",
				Validators: []validator.List{
					listvalidator.SizeAtLeast(1),
					listvalidator.ValueStringsAre(stringCheck("hostname", validateHostname)),
				},
			},
			"comment": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Comment written at the end of the line after `#`, such as `\"managed by Terraform\"`. " +
					"Must be a non-empty single line without leading or trailing white space. Unset means no comment.",
				Validators: []validator.String{stringCheck("comment", validateHostsComment)},
			},
			"path": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(defaultHostsPath),
				MarkdownDescription: "Absolute path of the hosts file, inside the provider's `root_dir` if set. " +
					"Created with mode `0644` if it does not exist; its directory must exist. A symlink at `path` is refused. " +
					"Defaults to `\"" + defaultHostsPath + "\"`. Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{absolutePath()},
			},
			"allow_duplicate": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				MarkdownDescription: "Allow a hostname that another line already maps to a different address of the same family (IPv4 or IPv6). " +
					"By default applying fails in that case, because the resolver returns the first matching line and the managed line might never be used. " +
					"Defaults to `false`.",
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "`<path>:<ip>`.",
			},
		},
	}
}

// ValidateConfig refuses a hostname listed twice.
func (r *hostsEntryResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg hostsEntryModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() || cfg.Hostnames.IsNull() || cfg.Hostnames.IsUnknown() {
		return
	}
	var names []types.String
	resp.Diagnostics.Append(cfg.Hostnames.ElementsAs(ctx, &names, false)...)
	known := make([]string, 0, len(names))
	for _, n := range names {
		if !n.IsNull() && !n.IsUnknown() {
			known = append(known, n.ValueString())
		}
	}
	if dup, ok := duplicateHostname(known); ok {
		resp.Diagnostics.AddAttributeError(path.Root("hostnames"), "Duplicate hostname",
			fmt.Sprintf("Hostname %q is listed more than once (names are compared ignoring case).", dup))
	}
}

// ModifyPlan computes the id, which changes with ip.
func (r *hostsEntryResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // Destroy.
	}
	var plan hostsEntryModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := types.StringUnknown()
	if !plan.Path.IsUnknown() && !plan.IP.IsUnknown() {
		id = types.StringValue(hostsEntryID(plan.Path.ValueString(), plan.IP.ValueString()))
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("id"), id)...)
}

func hostsEntryID(p, ip string) string { return p + ":" + ip }

// entry validates m, whose values must all be known, and returns the entry
// it describes.
func (m *hostsEntryModel) entry(ctx context.Context) (*hostsEntry, diag.Diagnostics) {
	var diags diag.Diagnostics
	addr, err := parseHostsIP(m.IP.ValueString())
	if err != nil {
		diags.AddAttributeError(path.Root("ip"), "Invalid IP address", capitalize(err.Error())+".")
		return nil, diags
	}
	e := &hostsEntry{ipText: m.IP.ValueString(), addr: addr, comment: m.Comment.ValueString()}
	diags.Append(m.Hostnames.ElementsAs(ctx, &e.names, false)...)
	if diags.HasError() {
		return nil, diags
	}
	if len(e.names) == 0 {
		diags.AddAttributeError(path.Root("hostnames"), "Missing hostname", "At least one hostname is required.")
	}
	for _, n := range e.names {
		if err := validateHostname(n); err != nil {
			diags.AddAttributeError(path.Root("hostnames"), "Invalid hostname", capitalize(err.Error())+".")
		}
	}
	if dup, ok := duplicateHostname(e.names); ok {
		diags.AddAttributeError(path.Root("hostnames"), "Duplicate hostname", fmt.Sprintf("Hostname %q is listed more than once.", dup))
	}
	if !m.Comment.IsNull() {
		if err := validateHostsComment(e.comment); err != nil {
			diags.AddAttributeError(path.Root("comment"), "Invalid comment", capitalize(err.Error())+".")
		}
	}
	if diags.HasError() {
		return nil, diags
	}
	return e, diags
}

// identity returns the identity of the entry recorded in m, a prior state,
// and whether m holds one: after import, it holds no hostnames yet.
func (m *hostsEntryModel) identity(ctx context.Context) (hostsIdentity, bool, diag.Diagnostics) {
	var diags diag.Diagnostics
	addr, err := parseHostsIP(m.IP.ValueString())
	if err != nil {
		diags.AddError("Invalid state", capitalize(err.Error())+".")
		return hostsIdentity{}, false, diags
	}
	if m.Hostnames.IsNull() || m.Hostnames.IsUnknown() {
		return hostsIdentity{addr: addr}, false, diags
	}
	var names []string
	diags.Append(m.Hostnames.ElementsAs(ctx, &names, false)...)
	if diags.HasError() || len(names) == 0 {
		return hostsIdentity{addr: addr}, false, diags
	}
	return hostsIdentity{addr: addr, canonical: names[0]}, true, diags
}

func (r *hostsEntryResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan hostsEntryModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.apply(ctx, &plan, nil)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *hostsEntryResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state hostsEntryModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.apply(ctx, &plan, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// apply makes the file hold the entry in plan. prev is the prior state on
// update and nil on create: its entry, if the address or canonical name
// changed, is rewritten in place. An existing line with the planned address
// and canonical name is taken over, not duplicated.
func (r *hostsEntryResource) apply(ctx context.Context, plan, prev *hostsEntryModel) diag.Diagnostics {
	var diags diag.Diagnostics
	// Config validation already enforces this; re-check as defense in depth.
	if err := validateAbsolutePath(plan.Path.ValueString()); err != nil {
		diags.AddAttributeError(path.Root("path"), "Invalid path", capitalize(err.Error())+".")
		return diags
	}
	e, diags := plan.entry(ctx)
	if diags.HasError() {
		return diags
	}
	ids := []hostsIdentity{{addr: e.addr, canonical: e.names[0]}}
	if prev != nil {
		if id, ok, d := prev.identity(ctx); ok && !d.HasError() {
			ids = append(ids, id)
		}
	}
	target, d := resolvePathAttr(r.root(), plan.Path.ValueString(), false)
	diags.Append(d...)
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
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		diags.AddAttributeError(path.Root("path"), "Reading hosts file", capitalize(err.Error())+".")
		return diags
	}
	text := parseTextFile(data)
	indices := findHostsEntries(text.lines, ids...)

	if !plan.AllowDuplicate.ValueBool() {
		if conflicts := hostsConflicts(text.lines, e.addr, e.names, indices); len(conflicts) > 0 {
			msgs := make([]string, len(conflicts))
			for i, c := range conflicts {
				msgs[i] = c.String()
			}
			diags.AddAttributeError(path.Root("hostnames"), "Hostname already mapped to another address",
				fmt.Sprintf("In %s, %s. The resolver would use the first line that maps a name, so the entry for %s might never be used. "+
					"Remove the other mapping, or set allow_duplicate = true to add the entry anyway.",
					plan.Path.ValueString(), strings.Join(msgs, ", "), e.ipText))
			return diags
		}
	}

	if ensureHostsEntry(text, indices, e) || snap == nil {
		if err := replaceFileAtomic(target, text.bytes(), snap, hostsFileMode); err != nil {
			diags.AddError("Writing hosts file", capitalize(err.Error())+".")
			return diags
		}
	}
	plan.ID = types.StringValue(hostsEntryID(plan.Path.ValueString(), plan.IP.ValueString()))
	return diags
}

func (r *hostsEntryResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state hostsEntryModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	found, diags := r.refresh(ctx, &state)
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

// refresh updates m from the hosts file. It reports found=false if the file
// or the managed line is missing. After import, m holds only the path and
// the address, and the first line with that address is the managed one.
func (r *hostsEntryResource) refresh(ctx context.Context, m *hostsEntryModel) (found bool, diags diag.Diagnostics) {
	if m.Path.IsNull() || m.Path.ValueString() == "" {
		m.Path = types.StringValue(defaultHostsPath)
	}
	if m.AllowDuplicate.IsNull() || m.AllowDuplicate.IsUnknown() {
		m.AllowDuplicate = types.BoolValue(false)
	}
	if err := validateAbsolutePath(m.Path.ValueString()); err != nil {
		diags.AddError("Invalid state", capitalize(err.Error())+".")
		return false, diags
	}
	id, hasIdentity, d := m.identity(ctx)
	diags.Append(d...)
	if diags.HasError() {
		return false, diags
	}
	target, d := resolvePathAttr(r.root(), m.Path.ValueString(), false)
	diags.Append(d...)
	if diags.HasError() {
		return false, diags
	}
	data, _, err := readRegularFileNoFollow(target, maxFileLineSize)
	if errors.Is(err, fs.ErrNotExist) {
		return false, diags
	}
	if err != nil {
		diags.AddAttributeError(path.Root("path"), "Reading hosts file", capitalize(err.Error())+".")
		return false, diags
	}
	lines := parseTextFile(data).lines
	idx := -1
	if hasIdentity {
		if indices := findHostsEntries(lines, id); len(indices) > 0 {
			idx = indices[0]
		}
	} else if indices := findHostsAddr(lines, id.addr); len(indices) > 0 {
		idx = indices[0]
		if len(indices) > 1 {
			first, _ := parseHostsLine(lines[idx])
			diags.AddWarning("Several lines map the imported address",
				fmt.Sprintf("%d lines of %s map %s. The first one, with canonical name %q, was imported. "+
					"To import another one, add its canonical name to the import ID: %s:%s,<hostname>.",
					len(indices), m.Path.ValueString(), m.IP.ValueString(), first.canonical(), m.Path.ValueString(), m.IP.ValueString()))
		}
	}
	if idx < 0 {
		return false, diags
	}
	hl, _ := parseHostsLine(lines[idx])
	m.Hostnames = stringList(hl.names)
	m.Comment = types.StringNull()
	if hl.comment != "" {
		m.Comment = types.StringValue(strings.ToValidUTF8(hl.comment, "�"))
	}
	m.ID = types.StringValue(hostsEntryID(m.Path.ValueString(), m.IP.ValueString()))
	return true, diags
}

func (r *hostsEntryResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state hostsEntryModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// State is not validated by the schema; never act on an invalid path.
	if err := validateAbsolutePath(state.Path.ValueString()); err != nil {
		resp.Diagnostics.AddError("Refusing to edit file", capitalize(err.Error())+".")
		return
	}
	id, ok, diags := state.identity(ctx)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !ok {
		resp.Diagnostics.AddError("Invalid state", "The state holds no hostnames, so the managed line cannot be identified.")
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
		resp.Diagnostics.AddWarning("Hosts entry not removed",
			fmt.Sprintf("%s. The entry was not removed; the resource is removed from state only.", capitalize(err.Error())))
		return
	case err != nil:
		resp.Diagnostics.AddError("Reading hosts file", capitalize(err.Error())+".")
		return
	}
	text := parseTextFile(data)
	if !removeHostsEntries(text, findHostsEntries(text.lines, id)) {
		return
	}
	if err := replaceFileAtomic(target, text.bytes(), snap, hostsFileMode); err != nil {
		resp.Diagnostics.AddError("Writing hosts file", capitalize(err.Error())+".")
	}
}

// ImportState accepts "<path>:<ip>" and "<path>:<ip>,<hostname>". The path
// ends at the first colon, so an IPv6 address needs no quoting but the path
// must not contain a colon. Without a hostname, the first line that maps ip
// is imported; with one, the line whose canonical name it is.
func (r *hostsEntryResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	p, rest, ok := strings.Cut(req.ID, ":")
	if !ok {
		resp.Diagnostics.AddError("Invalid import ID",
			fmt.Sprintf("Import ID must have the form <path>:<ip> or <path>:<ip>,<hostname>, such as \"/etc/hosts:10.0.0.5\", got %q.", req.ID))
		return
	}
	ip, canonical, withName := strings.Cut(rest, ",")
	if err := validateAbsolutePath(p); err != nil {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("Invalid path in import ID: %s.", err))
		return
	}
	if err := validateHostsIP(ip); err != nil {
		resp.Diagnostics.AddError("Invalid import ID", capitalize(err.Error())+".")
		return
	}
	if withName {
		if err := validateHostname(canonical); err != nil {
			resp.Diagnostics.AddError("Invalid import ID", capitalize(err.Error())+".")
			return
		}
		// refresh finds the line by its canonical name and reads all
		// hostnames from it.
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("hostnames"), []string{canonical})...)
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("path"), p)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("ip"), ip)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("allow_duplicate"), false)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), hostsEntryID(p, ip))...)
}
