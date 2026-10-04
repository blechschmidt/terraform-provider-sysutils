package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

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
	_ resource.Resource                   = (*systemdDropInResource)(nil)
	_ resource.ResourceWithConfigure      = (*systemdDropInResource)(nil)
	_ resource.ResourceWithImportState    = (*systemdDropInResource)(nil)
	_ resource.ResourceWithValidateConfig = (*systemdDropInResource)(nil)
	_ resource.ResourceWithModifyPlan     = (*systemdDropInResource)(nil)
)

func NewSystemdDropInResource() resource.Resource { return &systemdDropInResource{} }

type systemdDropInResource struct {
	cfg *systemdConfig
}

type systemdDropInModel struct {
	UnitName types.String `tfsdk:"unit_name"`
	Name     types.String `tfsdk:"name"`
	unitContentModel
	Scope           types.Object `tfsdk:"scope"`
	RestartOnChange types.Bool   `tfsdk:"restart_on_change"`
	Timeout         types.String `tfsdk:"timeout"`
	Path            types.String `tfsdk:"path"`
	ID              types.String `tfsdk:"id"`
}

// unitTypeNames are the unit types, which also name the directories of
// drop-ins for every unit of a type, such as /etc/systemd/system/service.d.
var unitTypeNames = []string{"service", "socket", "device", "mount", "automount", "swap", "target", "path", "timer", "slice", "scope"}

// validateDropInUnit checks the unit_name of a drop-in: a unit name of any
// type (including templates, instances and prefixes such as "app-.service")
// or a unit type for all units of that type.
func validateDropInUnit(s string) error {
	for _, t := range unitTypeNames {
		if s == t {
			return nil
		}
	}
	if err := validateUnitNameWith(s, dropInUnitSuffixes); err != nil {
		return fmt.Errorf("%w, or a unit type such as \"service\"", err)
	}
	return nil
}

var dropInNamePattern = regexp.MustCompile(`^[A-Za-z0-9_@:+-][A-Za-z0-9_@:+.-]*$`)

// validateDropInName checks the name of a drop-in file without ".conf".
func validateDropInName(s string) error {
	if len(s) > 200 {
		return fmt.Errorf("drop-in name %q is longer than 200 characters", s)
	}
	if !dropInNamePattern.MatchString(s) {
		return fmt.Errorf("drop-in name %q must consist of ASCII letters, digits, \"_\", \"@\", \":\", \"+\", \"-\" and \".\", and must not start with \".\"", s)
	}
	return nil
}

// dropInRestartable reports whether a change to a drop-in of unitName
// restarts the unit. Restarting is only meaningful for single units of the
// types that run something; drop-ins for templates, all units of a type,
// prefixes, targets, slices, scopes and devices take effect without it or
// cannot be restarted.
func dropInRestartable(unitName string) bool {
	if isTemplateUnit(unitName) || !strings.Contains(unitName, ".") {
		return false
	}
	i := strings.LastIndexByte(unitName, '.')
	if strings.HasSuffix(unitName[:i], "-") {
		return false // A prefix drop-in such as app-.service.d.
	}
	switch unitTypeOf(unitName) {
	case "service", "socket", "mount", "automount", "swap", "timer", "path":
		return true
	}
	return false
}

func (r *systemdDropInResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_systemd_dropin"
}

func (r *systemdDropInResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	attrs := unitContentSchema(unitSectionSpecs, "drop-in")
	attrs["unit_name"] = schema.StringAttribute{
		Required: true,
		MarkdownDescription: "The unit that the drop-in configures, including its type suffix, such as `\"ssh.service\"`. " +
			"Any unit type is supported, including `.scope` and `.device` units, templates (`\"getty@.service\"`) and single instances (`\"getty@tty1.service\"`). " +
			"A name ending in a dash, such as `\"app-.service\"`, configures every unit whose name starts with that prefix, and a bare unit type such as `\"service\"` every unit of the type. " +
			"Changing this forces a new resource.",
		Validators:    []validator.String{stringCheck("unit name", validateDropInUnit)},
		PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
	}
	attrs["name"] = schema.StringAttribute{
		Required: true,
		MarkdownDescription: "Name of the drop-in file without the `.conf` suffix, such as `\"override\"` or `\"50-limits\"`. " +
			"systemd applies the drop-ins of a unit in the order of their file names. Changing this forces a new resource.",
		Validators:    []validator.String{stringCheck("drop-in name", validateDropInName)},
		PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
	}
	attrs["restart_on_change"] = schema.BoolAttribute{
		Optional: true,
		Computed: true,
		Default:  booldefault.StaticBool(true),
		MarkdownDescription: "Whether to restart the unit (`systemctl try-restart`) when the drop-in changes or is removed and the unit is running, so that the change takes effect. " +
			"Only applies to single `.service`, `.socket`, `.mount`, `.automount`, `.swap`, `.timer` and `.path` units, not to templates, prefixes or unit types. " +
			"Changes that systemd does not see, such as comments or formatting, never restart. Defaults to `true`.",
	}
	attrs["timeout"] = schema.StringAttribute{
		Optional: true,
		Computed: true,
		Default:  stringdefault.StaticString(defaultSystemdTimeout),
		MarkdownDescription: "Maximum time each `systemctl` invocation may take, as a Go duration such as `\"30s\"` or `\"5m\"`. " +
			"`systemctl try-restart` waits for the unit to restart, so this should exceed the unit's own start and stop timeouts. Defaults to `\"2m\"`.",
		Validators: []validator.String{positiveDuration()},
	}
	attrs["path"] = schema.StringAttribute{
		Computed:            true,
		MarkdownDescription: "Path of the drop-in file, `/etc/systemd/system/<unit_name>.d/<name>.conf`.",
	}
	attrs["id"] = schema.StringAttribute{
		Computed:            true,
		MarkdownDescription: "Resource identifier, `<unit_name>/<name>`.",
		PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a drop-in file that changes the configuration of a systemd unit, `/etc/systemd/system/<unit_name>.d/<name>.conf`, and runs `systemctl daemon-reload` when it changes. " +
			"The drop-in is given either as text (`content` or `source`) or as attributes: every section is an attribute (`unit`, `service`, `socket`, `scope`, ..., `install`) and every directive of systemd a nested attribute. " +
			"Requires root privileges and a host booted with systemd.",
		Attributes: attrs,
	}
}

func (r *systemdDropInResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return // Not configured yet, e.g. during validation.
	}
	data, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("Expected *providerData, got %T.", req.ProviderData))
		return
	}
	r.cfg = data.systemd
}

func (r *systemdDropInResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var config systemdDropInModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	typ := ""
	if !config.UnitName.IsUnknown() && validateDropInUnit(config.UnitName.ValueString()) == nil {
		typ = unitTypeOf(config.UnitName.ValueString())
	}
	resp.Diagnostics.Append(validateUnitContentConfig(unitSectionSpecs, typ, &config.unitContentModel, &config.Scope)...)
}

func (r *systemdDropInResource) dropInPath(unitName, name string) string {
	return filepath.Join(r.cfg.dir(), unitName+".d", name+".conf")
}

func (r *systemdDropInResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // Destroy.
	}
	var config, plan systemdDropInModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	typ := ""
	plan.Path, plan.ID = types.StringUnknown(), types.StringUnknown()
	if !config.UnitName.IsUnknown() && !config.Name.IsUnknown() {
		unitName, name := config.UnitName.ValueString(), config.Name.ValueString()
		typ = unitTypeOf(unitName)
		plan.Path = types.StringValue(r.dropInPath(unitName, name))
		plan.ID = types.StringValue(unitName + "/" + name)
	}
	if err := planUnitContent(unitSectionSpecs, typ, &config.unitContentModel, &config.Scope, &plan.unitContentModel, &plan.Scope); err != nil {
		resp.Diagnostics.AddAttributeError(unitContentAttribute(&config.unitContentModel), "Unable to read drop-in contents", capitalize(err.Error())+".")
		return
	}
	resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
}

func (r *systemdDropInResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	r.apply(ctx, req.Plan, req.Config, &resp.State, &resp.Diagnostics, true)
}

func (r *systemdDropInResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	r.apply(ctx, req.Plan, req.Config, &resp.State, &resp.Diagnostics, false)
}

// planConfigGetter is the part of tfsdk.Plan and tfsdk.Config used by apply.
type planConfigGetter interface {
	Get(ctx context.Context, target any) diag.Diagnostics
}

func (r *systemdDropInResource) apply(ctx context.Context, planGetter, configGetter planConfigGetter, state stateSetter, diags *diag.Diagnostics, create bool) {
	var plan, config systemdDropInModel
	diags.Append(planGetter.Get(ctx, &plan)...)
	diags.Append(configGetter.Get(ctx, &config)...)
	if diags.HasError() {
		return
	}
	unitName, name := plan.UnitName.ValueString(), plan.Name.ValueString()
	// Config validation already enforces these; re-check as defense in
	// depth, since both become part of a path.
	if err := validateDropInUnit(unitName); err != nil {
		diags.AddAttributeError(path.Root("unit_name"), "Invalid unit name", capitalize(err.Error())+".")
		return
	}
	if err := validateDropInName(name); err != nil {
		diags.AddAttributeError(path.Root("name"), "Invalid drop-in name", capitalize(err.Error())+".")
		return
	}
	sc, err := systemctlWithTimeout(r.cfg, plan.Timeout)
	if err != nil {
		diags.AddAttributeError(path.Root("timeout"), "Invalid timeout", capitalize(err.Error())+".")
		return
	}
	data, _, err := desiredUnitBytes(unitSectionSpecs, &config.unitContentModel, &config.Scope, false)
	if err != nil {
		diags.AddAttributeError(unitContentAttribute(&config.unitContentModel), "Unable to read drop-in contents", capitalize(err.Error())+".")
		return
	}

	target := r.dropInPath(unitName, name)
	spec := r.fileSpec()
	current, snap, err := spec.read(target)
	if err != nil {
		diags.AddError("Reading drop-in", err.Error())
		return
	}
	changed := create || snap == nil || !bytes.Equal(current, data)
	semantic := create || snap == nil || !unitFilesEquivalent(current, data)
	if changed {
		if err := spec.write(target, data, create, nil); err != nil {
			if errors.Is(err, errDropInExists) {
				diags.AddAttributeError(path.Root("name"), "Drop-in already exists",
					fmt.Sprintf("%s already exists. Import it with \"terraform import\" and the ID %q instead, or remove the file.", target, unitName+"/"+name))
			} else {
				diags.AddError("Writing drop-in", err.Error())
			}
			return
		}
	}

	// The file exists from here on. Whatever happens, record it, so a
	// failure taints the resource instead of orphaning the file.
	if changed {
		diags.Append(r.reload(ctx, sc, unitName, plan.RestartOnChange.ValueBool() && semantic)...)
	}
	planned := plan.ContentSHA256
	found, d := r.refresh(ctx, &plan)
	diags.Append(d...)
	if d.HasError() {
		return
	}
	if !found {
		diags.AddError("Drop-in missing after write", fmt.Sprintf("%q disappeared while the drop-in was being written.", target))
		return
	}
	diags.Append(state.Set(ctx, &plan)...)
	if !planned.Equal(plan.ContentSHA256) && !planned.IsUnknown() {
		diags.AddAttributeError(unitContentAttribute(&config.unitContentModel), "Content changed during apply",
			fmt.Sprintf("The drop-in %q has SHA-256 %s, but the plan expected %s. "+
				"The source file was probably modified between plan and apply; run terraform apply again.", target, plan.ContentSHA256.ValueString(), planned.ValueString()))
	}
}

func (r *systemdDropInResource) fileSpec() dropInSpec {
	uid, gid := processOwner()
	return dropInSpec{mode: systemdUnitFileMode, dirMode: 0o755, maxSize: maxUnitFileSize, uid: uid, gid: gid}
}

// reload runs daemon-reload, checks that the unit still loads, and restarts
// it if restart is set, it is running and dropInRestartable allows it.
func (r *systemdDropInResource) reload(ctx context.Context, sc systemctl, unitName string, restart bool) diag.Diagnostics {
	var diags diag.Diagnostics
	if err := sc.do(ctx, "daemon-reload"); err != nil {
		diags.AddError("Reloading systemd", err.Error())
		return diags
	}
	if !strings.Contains(unitName, ".") {
		return diags // A drop-in for a whole unit type.
	}
	probe := unitName
	if isTemplateUnit(unitName) {
		probe = strings.Replace(unitName, "@.", "@sysutils-load-check.", 1)
	}
	props, err := sc.show(ctx, probe, "LoadState")
	if err != nil {
		diags.AddError("Querying unit", err.Error())
		return diags
	}
	// A drop-in may be written before its unit is installed, so "not-found"
	// is fine; "bad-setting" and "error" are not.
	if state := props["LoadState"]; state == "bad-setting" || state == "error" {
		diags.AddError("Unit failed to load",
			fmt.Sprintf("After daemon-reload, systemd reports load state %q for %s. Check the drop-in; `systemctl status %s` and the journal show the reason.", state, probe, probe))
		return diags
	}
	if !restart || !dropInRestartable(unitName) {
		return diags
	}
	active, err := sc.isActive(ctx, unitName)
	if err != nil {
		diags.AddError("Querying unit", err.Error())
		return diags
	}
	if unitRunState(active) == unitStateRunning {
		if err := sc.do(ctx, "try-restart", "--", unitName); err != nil {
			diags.AddError("Restarting unit", err.Error())
		}
	}
	return diags
}

func (r *systemdDropInResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state systemdDropInModel
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

// refresh reads the drop-in into m. It reports found=false if the file does
// not exist.
func (r *systemdDropInResource) refresh(_ context.Context, m *systemdDropInModel) (bool, diag.Diagnostics) {
	var diags diag.Diagnostics
	unitName, name := m.UnitName.ValueString(), m.Name.ValueString()
	// State is not validated by the schema; never read an arbitrary file.
	if err := validateDropInUnit(unitName); err != nil {
		diags.AddError("Invalid state", capitalize(err.Error())+".")
		return false, diags
	}
	if err := validateDropInName(name); err != nil {
		diags.AddError("Invalid state", capitalize(err.Error())+".")
		return false, diags
	}
	target := r.dropInPath(unitName, name)
	data, snap, err := r.fileSpec().read(target)
	if err != nil {
		diags.AddError("Reading drop-in", err.Error())
		return false, diags
	}
	if snap == nil {
		return false, diags
	}
	refreshUnitContent(unitSectionSpecs, unitTypeOf(unitName), data, &m.unitContentModel, &m.Scope)
	m.Path = types.StringValue(target)
	m.ID = types.StringValue(unitName + "/" + name)
	return true, diags
}

func (r *systemdDropInResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state systemdDropInModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	unitName, name := state.UnitName.ValueString(), state.Name.ValueString()
	// State is not validated by the schema; never remove an arbitrary file.
	if err := validateDropInUnit(unitName); err != nil {
		resp.Diagnostics.AddError("Refusing to remove drop-in", capitalize(err.Error())+".")
		return
	}
	if err := validateDropInName(name); err != nil {
		resp.Diagnostics.AddError("Refusing to remove drop-in", capitalize(err.Error())+".")
		return
	}
	sc, err := systemctlWithTimeout(r.cfg, state.Timeout)
	if err != nil {
		// A timeout that no longer parses must not block destroy.
		sc = systemctl{run: r.cfg.runner(), timeout: mustParseDuration(defaultSystemdTimeout)}
	}
	target := r.dropInPath(unitName, name)
	removed, err := removeDropInFile(target)
	if err != nil {
		resp.Diagnostics.AddError("Removing drop-in", explainImmutable(err, filepath.Dir(target)).Error())
		return
	}
	if !removed {
		return
	}
	// Remove the drop-in directory if this was its last file.
	if err := syscall.Rmdir(filepath.Dir(target)); err != nil && !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) && !errors.Is(err, fs.ErrNotExist) {
		resp.Diagnostics.AddWarning("Drop-in directory left in place", fmt.Sprintf("Removing the empty directory %s: %s.", filepath.Dir(target), err))
	}
	resp.Diagnostics.Append(r.reload(ctx, sc, unitName, state.RestartOnChange.ValueBool())...)
}

// ImportState imports a drop-in by "<unit_name>/<name>". Its contents are
// filled in by the subsequent Read.
func (r *systemdDropInResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	unitName, name, ok := strings.Cut(req.ID, "/")
	if !ok {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("Expected <unit_name>/<name>, such as \"ssh.service/override\", got %q.", req.ID))
		return
	}
	name = strings.TrimSuffix(name, ".conf")
	if err := validateDropInUnit(unitName); err != nil {
		resp.Diagnostics.AddError("Invalid import ID", capitalize(err.Error())+".")
		return
	}
	if err := validateDropInName(name); err != nil {
		resp.Diagnostics.AddError("Invalid import ID", capitalize(err.Error())+".")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("unit_name"), unitName)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), name)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), unitName+"/"+name)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("restart_on_change"), true)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("timeout"), defaultSystemdTimeout)...)
}
