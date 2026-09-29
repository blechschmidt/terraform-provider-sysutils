package provider

import (
	"bytes"
	"context"
	"fmt"
	"maps"

	"github.com/hashicorp/terraform-plugin-framework-validators/mapvalidator"
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
	_ resource.Resource                = (*kernelModuleResource)(nil)
	_ resource.ResourceWithModifyPlan  = (*kernelModuleResource)(nil)
	_ resource.ResourceWithConfigure   = (*kernelModuleResource)(nil)
	_ resource.ResourceWithImportState = (*kernelModuleResource)(nil)
)

func NewKernelModuleResource() resource.Resource { return &kernelModuleResource{} }

type kernelModuleResource struct {
	hostOnlyResource
	cfg *kernelModuleConfig
}

type kernelModuleModel struct {
	Name       types.String `tfsdk:"name"`
	Parameters types.Map    `tfsdk:"parameters"`
	Persist    types.Bool   `tfsdk:"persist"`
	ID         types.String `tfsdk:"id"`
}

func (r *kernelModuleResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_kernel_module"
}

func (r *kernelModuleResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Loads a kernel module with `modprobe(8)` and, optionally, configures it to be loaded at boot through `/etc/modules-load.d/<name>.conf` and `/etc/modprobe.d/<name>.conf`. " +
			"Whether the module is loaded is read from `/proc/modules`. On destroy the configuration files are removed and the module is unloaded with `modprobe -r`. " +
			"Requires root privileges.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Name of the module, such as `\"br_netfilter\"`. 1 to 55 letters, digits, `_` or `-`, starting with a letter or digit; " +
					"`-` and `_` are interchangeable, as in `modprobe`. Also names the configuration files. Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringCheck("module name", validateModuleName)},
			},
			"parameters": schema.MapAttribute{
				ElementType: types.StringType,
				Optional:    true,
				MarkdownDescription: "Module parameters, such as `{ numdummies = \"2\" }`, passed to `modprobe` as `name=value` and, with `persist`, written to `/etc/modprobe.d/<name>.conf` as an `options` line. " +
					"Parameters only take effect when the module is loaded, so changing them on a loaded module unloads and reloads it, which fails while the module is in use. " +
					"Names consist of letters, digits, `_`, `-` and `.`; values of letters, digits and `_.,:/+=@%-`, without white space or quotes.",
				Validators: []validator.Map{
					mapvalidator.KeysAre(stringCheck("module parameter name", validateModuleParamName)),
					mapvalidator.ValueStringsAre(stringCheck("module parameter value", validateModuleParamValue)),
				},
			},
			"persist": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
				MarkdownDescription: "Whether the module is loaded at boot. If `true`, `/etc/modules-load.d/<name>.conf` lists the module and, if `parameters` is not empty, `/etc/modprobe.d/<name>.conf` sets its parameters; " +
					"the parameters then also apply when the module is loaded by other means, such as automatically for a device. If `false`, both files are removed. Defaults to `true`.",
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Resource identifier (equal to `name`).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *kernelModuleResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	data, diags := providerDataFrom(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	if data != nil {
		r.cfg = data.kernelModule
		r.fsRoot = data.root
	}
}

func (r *kernelModuleResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan kernelModuleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := plan.Name.ValueString()
	if err := validateModuleName(name); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("name"), "Invalid name", capitalize(err.Error())+".")
		return
	}
	// Refuse to overwrite configuration files that the resource did not
	// write, such as those shipped by a package: destroying the resource
	// would remove them.
	files := r.cfg.files(name)
	for _, p := range []string{files.load, files.options} {
		data, _, err := readModuleConf(p)
		if err != nil {
			resp.Diagnostics.AddError("Reading module configuration", capitalize(err.Error())+".")
			return
		}
		if data != nil && !bytes.HasPrefix(data, []byte(moduleConfHeader+"\n")) {
			resp.Diagnostics.AddAttributeError(path.Root("name"), "Configuration file exists",
				fmt.Sprintf("%s already exists and was not written by sysutils_kernel_module. Import the module with \"terraform import\" to take the file over, or remove it.", p))
			return
		}
	}

	plan.ID = types.StringValue(name)
	changed, diags := r.apply(ctx, &plan, nil)
	resp.Diagnostics.Append(diags...)
	// After a partial apply, record the resource so that it is tainted and
	// destroy can clean up.
	if !resp.Diagnostics.HasError() || changed {
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	}
}

func (r *kernelModuleResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state kernelModuleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	gone, diags := r.refresh(ctx, &state)
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

func (r *kernelModuleResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state kernelModuleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = types.StringValue(plan.Name.ValueString())
	_, diags := r.apply(ctx, &plan, &state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *kernelModuleResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state kernelModuleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.Name.ValueString()
	// State is not validated by the schema; never act on an invalid name,
	// which also names the files removed here.
	if err := validateModuleName(name); err != nil {
		resp.Diagnostics.AddError("Invalid state", capitalize(err.Error())+".")
		return
	}
	// Remove the configuration first, so that a module that cannot be
	// unloaded is at least not loaded again at the next boot.
	files := r.cfg.files(name)
	for _, p := range []string{files.load, files.options} {
		if _, err := writeModuleConf(p, nil); err != nil {
			resp.Diagnostics.AddError("Removing module configuration", fmt.Sprintf("Removing %s: %s.", p, err))
			return
		}
	}
	loader := r.cfg.modules()
	loaded, err := isModuleLoaded(loader, name)
	if err != nil {
		resp.Diagnostics.AddError("Reading loaded modules", capitalize(err.Error())+".")
		return
	}
	if !loaded {
		return
	}
	// A module in use, such as a driver of active hardware or a module
	// other modules depend on, cannot be unloaded. Failing the destroy
	// would leave the resource undeletable, so only warn.
	if err := loader.unload(ctx, name); err != nil {
		resp.Diagnostics.AddWarning("Module not unloaded",
			fmt.Sprintf("%s. Its configuration files were removed, so it is no longer loaded at boot by this resource, but it stays loaded until it is unloaded manually or the system reboots.", capitalize(err.Error())))
	}
}

// ImportState imports a loaded module by name. Its parameters are taken
// from /etc/modprobe.d/<name>.conf, if the file sets any.
func (r *kernelModuleResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if err := validateModuleName(req.ID); err != nil {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("Import ID must be a module name: %s.", err))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}

// isModuleLoaded reports whether /proc/modules lists name as loaded or
// being loaded.
func isModuleLoaded(l moduleLoader, name string) (bool, error) {
	mods, err := l.loaded()
	if err != nil {
		return false, err
	}
	m, ok := mods[canonicalModuleName(name)]
	return ok && m.state != "Unloading", nil
}

// apply loads the module and brings its configuration files in line with
// plan. prev is the prior state on update and nil on create. It reports
// whether the host may have been changed, so that a failed create can still
// be recorded.
func (r *kernelModuleResource) apply(ctx context.Context, plan, prev *kernelModuleModel) (changed bool, diags diag.Diagnostics) {
	name := plan.Name.ValueString()
	if err := validateModuleName(name); err != nil {
		diags.AddAttributeError(path.Root("name"), "Invalid name", capitalize(err.Error())+".")
		return false, diags
	}
	params, d := plan.params(ctx)
	diags.Append(d...)
	if diags.HasError() {
		return false, diags
	}
	for k, v := range params {
		if err := validateModuleParamName(k); err != nil {
			diags.AddAttributeError(path.Root("parameters"), "Invalid parameter", capitalize(err.Error())+".")
		}
		if err := validateModuleParamValue(v); err != nil {
			diags.AddAttributeError(path.Root("parameters").AtMapKey(k), "Invalid parameter", capitalize(err.Error())+".")
		}
	}
	if diags.HasError() {
		return false, diags
	}
	var prevParams map[string]string
	if prev != nil {
		prevParams, d = prev.params(ctx)
		diags.Append(d...)
		if diags.HasError() {
			return false, diags
		}
	}

	loader := r.cfg.modules()
	loaded, err := isModuleLoaded(loader, name)
	if err != nil {
		diags.AddError("Reading loaded modules", capitalize(err.Error())+".")
		return false, diags
	}
	reload := loaded && prev != nil && !maps.Equal(params, prevParams)
	if loaded && prev == nil && len(params) > 0 {
		// Unloading a module the resource did not load could take down a
		// device or a network interface.
		diags.AddWarning("Parameters not applied",
			fmt.Sprintf("Module %s is already loaded, so its parameters take effect only when it is next loaded, for example after a reboot. "+
				"To apply them now, taint the resource or unload the module before applying.", name))
	}

	// Unload before touching the configuration, so that a module in use
	// fails the apply without any change.
	if reload {
		if err := loader.unload(ctx, name); err != nil {
			diags.AddAttributeError(path.Root("parameters"), "Unloading module",
				fmt.Sprintf("Changed parameters take effect only when the module is loaded again, but unloading it failed: %s. "+
					"Stop what uses the module, or reboot after changing only the persisted configuration.", err))
			return false, diags
		}
		changed = true
		loaded = false
	}

	// Write the configuration before loading: modprobe also applies the
	// options in /etc/modprobe.d, which must not include stale ones. If
	// loading fails, the previous files are restored so that parameters the
	// module rejects are not applied at every boot.
	files := r.cfg.files(name)
	var wantLoad, wantOptions moduleConfState
	if plan.Persist.ValueBool() {
		wantLoad = renderModulesLoad(name)
		if len(params) > 0 {
			wantOptions = renderModprobeOptions(name, params)
		}
	}
	oldLoad, _, err := readModuleConf(files.load)
	if err != nil {
		diags.AddError("Reading module configuration", capitalize(err.Error())+".")
		return changed, diags
	}
	oldOptions, _, err := readModuleConf(files.options)
	if err != nil {
		diags.AddError("Reading module configuration", capitalize(err.Error())+".")
		return changed, diags
	}
	restore := func() {
		for _, f := range []struct {
			path string
			data moduleConfState
		}{{files.load, oldLoad}, {files.options, oldOptions}} {
			if _, err := writeModuleConf(f.path, f.data); err != nil {
				diags.AddError("Restoring module configuration", fmt.Sprintf("Restoring %s: %s.", f.path, err))
			}
		}
	}
	for _, f := range []struct {
		path string
		data moduleConfState
	}{{files.load, wantLoad}, {files.options, wantOptions}} {
		if _, err := writeModuleConf(f.path, f.data); err != nil {
			diags.AddError("Writing module configuration", fmt.Sprintf("Writing %s: %s.", f.path, err))
			restore()
			return true, diags
		}
	}

	if !loaded {
		if err := loader.load(ctx, name, moduleParamArgs(params)); err != nil {
			diags.AddError("Loading module", capitalize(err.Error())+".")
			restore()
			if reload {
				// Best effort: bring back what was running before.
				if err := loader.load(ctx, name, moduleParamArgs(prevParams)); err != nil {
					diags.AddError("Reloading module with previous parameters", capitalize(err.Error())+".")
				}
			}
			return true, diags
		}
	}

	// Verify the result: modprobe succeeds without loading anything for a
	// module built into the kernel, and install commands in modprobe.d can
	// do something else entirely.
	if loaded, err = isModuleLoaded(loader, name); err != nil {
		diags.AddError("Reading loaded modules", capitalize(err.Error())+".")
	} else if !loaded {
		diags.AddError("Module not loaded",
			fmt.Sprintf("modprobe succeeded, but /proc/modules does not list %s. The module may be built into the kernel, or an install command in /etc/modprobe.d may have replaced loading it. "+
				"Built-in modules cannot be managed by this resource.", name))
	}
	return true, diags
}

// refresh updates m from /proc/modules and the configuration files. It
// reports gone if the module is not loaded.
func (r *kernelModuleResource) refresh(ctx context.Context, m *kernelModuleModel) (gone bool, diags diag.Diagnostics) {
	name := m.Name.ValueString()
	if err := validateModuleName(name); err != nil {
		diags.AddError("Invalid state", capitalize(err.Error())+".")
		return false, diags
	}
	imported := m.Persist.IsNull()
	loaded, err := isModuleLoaded(r.cfg.modules(), name)
	if err != nil {
		diags.AddError("Reading loaded modules", capitalize(err.Error())+".")
		return false, diags
	}
	if !loaded {
		if imported {
			diags.AddError("Cannot import kernel module", fmt.Sprintf("Module %s is not loaded according to /proc/modules.", name))
		}
		return true, diags
	}

	files := r.cfg.files(name)
	loadData, _, err := readModuleConf(files.load)
	if err != nil {
		diags.AddError("Reading module configuration", capitalize(err.Error())+".")
		return false, diags
	}
	optData, _, err := readModuleConf(files.options)
	if err != nil {
		diags.AddError("Reading module configuration", capitalize(err.Error())+".")
		return false, diags
	}

	// What was last applied, to compare the files against. After import,
	// the parameters in the modprobe.d file take that role.
	var params map[string]string
	if imported {
		if p, found := parseModprobeOptions(optData, name); found && len(p) > 0 {
			params = p
			v, d := types.MapValueFrom(ctx, types.StringType, params)
			diags.Append(d...)
			m.Parameters = v
		} else {
			m.Parameters = types.MapNull(types.StringType)
		}
	} else {
		var d diag.Diagnostics
		params, d = m.params(ctx)
		diags.Append(d...)
		if diags.HasError() {
			return false, diags
		}
	}
	// The configuration only counts as persisted if both files are exactly
	// as the resource writes them, so that any manual change shows up as
	// persist changing to true and is reverted by the next apply.
	persisted := bytes.Equal(loadData, renderModulesLoad(name))
	if len(params) > 0 {
		persisted = persisted && bytes.Equal(optData, renderModprobeOptions(name, params))
	} else {
		persisted = persisted && optData == nil
	}
	m.ID = types.StringValue(name)
	m.Persist = types.BoolValue(persisted)
	return false, diags
}

// params returns the configured parameters; nil if there are none.
func (m *kernelModuleModel) params(ctx context.Context) (map[string]string, diag.Diagnostics) {
	if m.Parameters.IsNull() || m.Parameters.IsUnknown() {
		return nil, nil
	}
	var p map[string]string
	diags := m.Parameters.ElementsAs(ctx, &p, false)
	if len(p) == 0 {
		p = nil
	}
	return p, diags
}
