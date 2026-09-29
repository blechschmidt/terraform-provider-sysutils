package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
	_ resource.Resource                = (*sysctlResource)(nil)
	_ resource.ResourceWithConfigure   = (*sysctlResource)(nil)
	_ resource.ResourceWithImportState = (*sysctlResource)(nil)
)

func NewSysctlResource() resource.Resource { return &sysctlResource{} }

type sysctlResource struct {
	cfg *sysctlConfig
}

type sysctlModel struct {
	Name    types.String `tfsdk:"name"`
	Value   types.String `tfsdk:"value"`
	Persist types.Bool   `tfsdk:"persist"`
	File    types.String `tfsdk:"file"`
	ID      types.String `tfsdk:"id"`
}

func (r *sysctlResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_sysctl"
}

func (r *sysctlResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a kernel parameter, like `sysctl -w`, and optionally persists it in a `sysctl.d` file so that it is set again at boot. " +
			"The value is written to and read back from `/proc/sys`. On destroy the entry is removed from the `sysctl.d` file; the running kernel keeps its current value. " +
			"Requires root privileges.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The parameter in the dotted form of `sysctl(8)`, such as `\"net.ipv4.ip_forward\"`. " +
					"As in `sysctl(8)`, a `/` stands for a `.` inside a component, so the VLAN interface `eth0.100` is written `\"net.ipv4.conf.eth0/100.forwarding\"`. " +
					"Only letters, digits and `_-.@+/` are allowed, and no component may be empty or `..`, so the name always refers to a file below `/proc/sys`. " +
					"Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringCheck("sysctl name", validateSysctlName)},
			},
			"value": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The value to set, such as `\"1\"` or `\"1024 65535\"`. " +
					"Values are compared with runs of white space collapsed, because the kernel separates the fields of multi-value parameters with tabs. " +
					"Must be a single line without leading or trailing white space.",
				Validators: []validator.String{stringCheck("sysctl value", validateSysctlValue)},
			},
			"persist": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
				MarkdownDescription: "Whether to write `<name> = <value>` to `file`, so that `systemd-sysctl` or `sysctl --system` sets the parameter again at boot. " +
					"If `false`, any entry for `name` is removed from `file` and the value only lasts until the next reboot. Defaults to `true`.",
			},
			"file": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(defaultSysctlFile),
				MarkdownDescription: "The `sysctl.d` file to persist the parameter in. It is created if needed and removed again when its last line is removed. " +
					"Several `sysutils_sysctl` resources can share a file; comments and other entries in it are kept. " +
					"Must be an absolute path in canonical form. Defaults to `\"" + defaultSysctlFile + "\"`.",
				Validators: []validator.String{absolutePath()},
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Resource identifier (equal to `name`).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *sysctlResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	data, diags := providerDataFrom(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	if data != nil {
		r.cfg = data.sysctl
	}
}

func (r *sysctlResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan sysctlModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name, file := plan.Name.ValueString(), plan.File.ValueString()
	if err := validateAbsolutePath(file); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("file"), "Invalid file", capitalize(err.Error())+".")
		return
	}
	// Refuse to take over an entry that already exists: destroying the
	// resource would remove it.
	t, _, err := readSysctlFile(file)
	if err != nil {
		resp.Diagnostics.AddError("Reading sysctl file", capitalize(err.Error())+".")
		return
	}
	if v, n := lookupSysctlEntry(t, name); n > 0 {
		resp.Diagnostics.AddAttributeError(path.Root("name"), "Parameter already in file",
			fmt.Sprintf("%s already sets %s = %s. Import it with \"terraform import\" instead, or remove the entry.", file, name, v))
		return
	}

	plan.ID = types.StringValue(name)
	changed, diags := r.apply(&plan, nil)
	resp.Diagnostics.Append(diags...)
	// After a partial apply, record the resource so that it is tainted and
	// destroy can clean up the file entry.
	if !resp.Diagnostics.HasError() || changed {
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	}
}

func (r *sysctlResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state sysctlModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	gone, diags := r.refresh(&state)
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

func (r *sysctlResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state sysctlModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = types.StringValue(plan.Name.ValueString())
	_, diags := r.apply(&plan, &state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *sysctlResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state sysctlModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name, file := state.Name.ValueString(), state.File.ValueString()
	// State is not validated by the schema; never edit an arbitrary file.
	if err := validateSysctlName(name); err != nil {
		resp.Diagnostics.AddError("Invalid state", capitalize(err.Error())+".")
		return
	}
	if err := validateAbsolutePath(file); err != nil {
		resp.Diagnostics.AddError("Invalid state", capitalize(err.Error())+".")
		return
	}
	if err := editSysctlFile(file, func(t *textFile) bool { return removeSysctlEntries(t, name) }); err != nil {
		resp.Diagnostics.AddError("Editing sysctl file", fmt.Sprintf("Removing %s from %s: %s.", name, file, err))
	}
}

// ImportState imports a parameter by name, or by "<name>:<file>" if it is
// persisted in a file other than the default one.
func (r *sysctlResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	name, file, ok := strings.Cut(req.ID, ":")
	if !ok {
		file = defaultSysctlFile
	}
	if err := validateSysctlName(name); err != nil {
		resp.Diagnostics.AddError("Invalid import ID",
			fmt.Sprintf("Import ID must be a sysctl name, optionally followed by \":\" and the absolute path of the sysctl.d file: %s.", err))
		return
	}
	if err := validateAbsolutePath(file); err != nil {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("Invalid sysctl.d file: %s.", err))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), name)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("file"), file)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), name)...)
}

// apply sets the parameter and brings the sysctl.d files in line with plan.
// prev is the prior state on update and nil on create. It reports whether
// the host may have been changed, so that a failed create can still be
// recorded.
func (r *sysctlResource) apply(plan, prev *sysctlModel) (changed bool, diags diag.Diagnostics) {
	name, value, file := plan.Name.ValueString(), plan.Value.ValueString(), plan.File.ValueString()
	if err := validateSysctlName(name); err != nil {
		diags.AddAttributeError(path.Root("name"), "Invalid name", capitalize(err.Error())+".")
		return false, diags
	}
	if err := validateAbsolutePath(file); err != nil {
		diags.AddAttributeError(path.Root("file"), "Invalid file", capitalize(err.Error())+".")
		return false, diags
	}
	root := r.cfg.root()

	// Set the running value before persisting it, so that a value the kernel
	// rejects fails the apply before it is applied again at every boot.
	cur, err := readSysctl(root, name)
	if err != nil {
		diags.AddAttributeError(path.Root("name"), "Reading kernel parameter", capitalize(err.Error())+".")
		return false, diags
	}
	if !sysctlValuesEqual(cur, value) {
		if err := writeSysctl(root, name, value); err != nil {
			diags.AddAttributeError(path.Root("value"), "Setting kernel parameter", capitalize(err.Error())+".")
			return false, diags
		}
		changed = true
		// Some parameters silently clamp or round what is written.
		got, err := readSysctl(root, name)
		if err != nil {
			diags.AddError("Reading kernel parameter", capitalize(err.Error())+".")
			return changed, diags
		}
		if !sysctlValuesEqual(got, value) {
			diags.AddAttributeError(path.Root("value"), "Kernel parameter not set",
				fmt.Sprintf("After writing %q to %s, the kernel reports %q. The kernel may have adjusted the value to its limits; configure the value it reports.", value, name, got))
			return changed, diags
		}
	}

	if prev != nil && prev.File.ValueString() != file && validateAbsolutePath(prev.File.ValueString()) == nil {
		old := prev.File.ValueString()
		if err := editSysctlFile(old, func(t *textFile) bool { return removeSysctlEntries(t, name) }); err != nil {
			diags.AddError("Editing sysctl file", fmt.Sprintf("Removing %s from %s: %s.", name, old, err))
			return changed, diags
		}
		changed = true
	}
	err = editSysctlFile(file, func(t *textFile) bool {
		if plan.Persist.ValueBool() {
			return setSysctlEntry(t, name, value)
		}
		return removeSysctlEntries(t, name)
	})
	if err != nil {
		diags.AddError("Editing sysctl file", fmt.Sprintf("Updating %s in %s: %s.", name, file, err))
		return changed, diags
	}
	return true, diags
}

// refresh updates m from /proc/sys and the sysctl.d file. It reports gone if
// the kernel no longer has the parameter.
func (r *sysctlResource) refresh(m *sysctlModel) (gone bool, diags diag.Diagnostics) {
	name, file := m.Name.ValueString(), m.File.ValueString()
	if err := validateSysctlName(name); err != nil {
		diags.AddError("Invalid state", capitalize(err.Error())+".")
		return false, diags
	}
	if err := validateAbsolutePath(file); err != nil {
		diags.AddError("Invalid state", capitalize(err.Error())+".")
		return false, diags
	}
	imported := m.Value.IsNull()
	live, err := readSysctl(r.cfg.root(), name)
	if errors.Is(err, errSysctlNotFound) {
		if imported {
			diags.AddError("Cannot import kernel parameter", capitalize(err.Error())+".")
		}
		return true, diags
	}
	if err != nil {
		diags.AddError("Reading kernel parameter", capitalize(err.Error())+".")
		return false, diags
	}
	// What was last applied, to compare the file entry against. After
	// import, the running value takes that role.
	applied := live
	if !imported {
		applied = m.Value.ValueString()
	}

	t, _, err := readSysctlFile(file)
	if err != nil {
		diags.AddError("Reading sysctl file", capitalize(err.Error())+".")
		return false, diags
	}
	persisted, count := lookupSysctlEntry(t, name)
	if count > 1 {
		diags.AddWarning("Duplicate sysctl entries",
			fmt.Sprintf("%s sets %s %d times. The last one is used at boot; the next apply that changes this resource removes the others.", file, name, count))
	}

	m.ID = types.StringValue(name)
	// An entry with a different value does not persist this resource's
	// value, so it reads as not persisted and the next apply rewrites it.
	m.Persist = types.BoolValue(count > 0 && sysctlValuesEqual(persisted, applied))
	if imported || !sysctlValuesEqual(live, m.Value.ValueString()) {
		m.Value = types.StringValue(live)
	}
	return false, diags
}
