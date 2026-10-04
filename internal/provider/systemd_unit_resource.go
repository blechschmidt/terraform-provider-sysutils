package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                   = (*systemdUnitResource)(nil)
	_ resource.ResourceWithConfigure      = (*systemdUnitResource)(nil)
	_ resource.ResourceWithImportState    = (*systemdUnitResource)(nil)
	_ resource.ResourceWithValidateConfig = (*systemdUnitResource)(nil)
	_ resource.ResourceWithModifyPlan     = (*systemdUnitResource)(nil)
)

const (
	// privateKeyPreexisting marks a unit that was active before Create
	// wrote its file.
	privateKeyPreexisting = "preexisting"
	defaultSystemdTimeout = "2m"
	// systemdUnitFileMode is the mode of newly written unit files.
	systemdUnitFileMode fs.FileMode = 0o644
	// maxUnitFileSize bounds how much of a unit file or source is read.
	maxUnitFileSize = 1 << 20
)

func NewSystemdUnitResource() resource.Resource { return &systemdUnitResource{} }

type systemdUnitResource struct {
	cfg *systemdConfig
}

type systemdUnitModel struct {
	Name types.String `tfsdk:"name"`
	unitContentModel
	Enabled         types.Bool   `tfsdk:"enabled"`
	State           types.String `tfsdk:"state"`
	RestartOnChange types.Bool   `tfsdk:"restart_on_change"`
	Timeout         types.String `tfsdk:"timeout"`
	Path            types.String `tfsdk:"path"`
	ID              types.String `tfsdk:"id"`
}

func (r *systemdUnitResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_systemd_unit"
}

func (r *systemdUnitResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	attrs := unitContentSchema(unitFileSpecs, "unit")
	attrs["name"] = schema.StringAttribute{
		Required: true,
		MarkdownDescription: "Unit name including its type suffix, such as `\"app.service\"` or `\"backup.timer\"`. " +
			"Supported suffixes are `.service`, `.socket`, `.device`, `.mount`, `.automount`, `.swap`, `.target`, `.path`, `.timer` and `.slice`; " +
			"scope units cannot be defined by unit files, only configured with [`sysutils_systemd_dropin`](./systemd_dropin.md). " +
			"Template units (`\"app@.service\"`) and instance-specific unit files (`\"app@one.service\"`) are supported. " +
			"A unit with this name must not already exist anywhere on the host. Changing this forces a new resource.",
		Validators:    []validator.String{unitName()},
		PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
	}
	attrs["enabled"] = schema.BoolAttribute{
		Optional: true,
		Computed: true,
		MarkdownDescription: "Whether the unit is enabled to start at boot (`systemctl enable`/`disable`). " +
			"If unset, the enablement is not managed and the current value is reported. " +
			"Only units with an `[Install]` section can be enabled, and templates only if it sets `DefaultInstance=`.",
		PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
	}
	attrs["state"] = schema.StringAttribute{
		Optional: true,
		Computed: true,
		MarkdownDescription: "Whether the unit should be `\"running\"` or `\"stopped\"` (`systemctl start`/`stop`). " +
			"If unset, the unit is not started or stopped and the current value is reported. " +
			"A unit counts as running when `systemctl is-active` reports `active`, `reloading` or `refreshing`. " +
			"Templates cannot be started, so this must be unset for them and is always null; start instances with [`sysutils_service`](./service.md).",
		Validators:    []validator.String{stringvalidator.OneOf(unitStateRunning, unitStateStopped)},
		PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
	}
	attrs["restart_on_change"] = schema.BoolAttribute{
		Optional: true,
		Computed: true,
		Default:  booldefault.StaticBool(true),
		MarkdownDescription: "Whether to restart the unit (`systemctl try-restart`) when its unit file changes and it is running, so that the change takes effect. " +
			"For a template, its running instances are restarted. " +
			"Changes that systemd does not see, such as comments or formatting, never restart. Never applies while `state` is `\"stopped\"`. Defaults to `true`.",
	}
	attrs["timeout"] = schema.StringAttribute{
		Optional: true,
		Computed: true,
		Default:  stringdefault.StaticString(defaultSystemdTimeout),
		MarkdownDescription: "Maximum time each `systemctl` invocation may take, as a Go duration such as `\"30s\"` or `\"5m\"`. " +
			"`systemctl start` and `stop` wait for the unit to finish starting or stopping, so this should exceed the unit's own start and stop timeouts. Defaults to `\"2m\"`.",
		Validators: []validator.String{positiveDuration()},
	}
	attrs["path"] = schema.StringAttribute{
		Computed:            true,
		MarkdownDescription: "Path of the unit file, `/etc/systemd/system/<name>`.",
	}
	attrs["id"] = schema.StringAttribute{
		Computed:            true,
		MarkdownDescription: "Resource identifier (equal to `name`).",
		PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a systemd unit of any type: writes its unit file to `/etc/systemd/system`, runs `systemctl daemon-reload` when the file changes, and optionally enables and starts or stops the unit. " +
			"The unit file is given either as text (`content` or `source`) or as attributes: every section is an attribute (`unit`, `service`, `socket`, `timer`, ..., `install`) and every directive of systemd a nested attribute. " +
			"On destroy the unit is stopped and disabled, and its file is removed. Requires root privileges and a host booted with systemd.",
		Attributes: attrs,
	}
}

func (r *systemdUnitResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *systemdUnitResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var config systemdUnitModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	typ := ""
	if !config.Name.IsUnknown() && validateUnitName(config.Name.ValueString()) == nil {
		typ = unitTypeOf(config.Name.ValueString())
		if isTemplateUnit(config.Name.ValueString()) && !config.State.IsNull() {
			resp.Diagnostics.AddAttributeError(path.Root("state"), "Templates cannot be started",
				fmt.Sprintf("%s is a template unit, which cannot be started or stopped itself. Leave state unset and manage instances such as %s with sysutils_service.",
					config.Name.ValueString(), strings.Replace(config.Name.ValueString(), "@.", "@example.", 1)))
		}
	}
	resp.Diagnostics.Append(validateUnitContentConfig(unitFileSpecs, typ, &config.unitContentModel, nil)...)
}

// ModifyPlan sets path from the name, and content, content_sha256 and the
// sections from the desired unit file. Refresh records the file on disk, so
// out-of-band edits and changes to a source file show up as a planned update.
func (r *systemdUnitResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // Destroy.
	}
	var config, plan systemdUnitModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	typ, template := "", false
	plan.Path = types.StringUnknown()
	if !config.Name.IsUnknown() {
		name := config.Name.ValueString()
		plan.Path = types.StringValue(r.unitPath(name))
		typ, template = unitTypeOf(name), isTemplateUnit(name)
	}
	if err := planUnitContent(unitFileSpecs, typ, &config.unitContentModel, nil, &plan.unitContentModel, nil); err != nil {
		resp.Diagnostics.AddAttributeError(unitContentAttribute(&config.unitContentModel), "Unable to read unit file contents", capitalize(err.Error())+".")
		return
	}

	// A content change can re-enable or restart the unit, which may change
	// enabled and state when they are not managed. Their prior values then
	// no longer predict the result.
	if !req.State.Raw.IsNull() {
		var prior types.String
		resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("content_sha256"), &prior)...)
		if resp.Diagnostics.HasError() {
			return
		}
		if !plan.ContentSHA256.Equal(prior) {
			if config.Enabled.IsNull() {
				plan.Enabled = types.BoolUnknown()
			}
			if config.State.IsNull() {
				plan.State = types.StringUnknown()
			}
		}
	}
	if template {
		plan.State = types.StringNull()
	}
	resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
}

func (r *systemdUnitResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan, config systemdUnitModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := plan.Name.ValueString()
	// Config validation already enforces this; re-check as defense in depth,
	// since the name becomes a file name.
	if err := validateUnitName(name); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("name"), "Invalid unit name", capitalize(err.Error())+".")
		return
	}
	sc, err := r.systemctl(&plan)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("timeout"), "Invalid timeout", capitalize(err.Error())+".")
		return
	}
	data, _, err := desiredUnitBytes(unitFileSpecs, &config.unitContentModel, nil, false)
	if err != nil {
		resp.Diagnostics.AddAttributeError(unitContentAttribute(&config.unitContentModel), "Unable to read unit file contents", capitalize(err.Error())+".")
		return
	}

	target := r.unitPath(name)
	preexisting, err := r.checkUnitAbsent(ctx, sc, name, target)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("name"), "Unit already exists", capitalize(err.Error())+".")
		return
	}
	if preexisting {
		// Remembered so that destroy leaves the unit running, as it was
		// before the resource added its file.
		resp.Diagnostics.Append(resp.Private.SetKey(ctx, privateKeyPreexisting, []byte(`true`))...)
	}
	if err := replaceFileAtomic(target, data, nil, systemdUnitFileMode); err != nil {
		resp.Diagnostics.AddError("Writing unit file", err.Error())
		return
	}

	// The unit file exists from here on. Whatever happens, record it, so a
	// failure taints the resource instead of orphaning the file.
	resp.Diagnostics.Append(r.converge(ctx, sc, &plan, &config, true)...)
	resp.Diagnostics.Append(r.saveRefreshed(ctx, sc, &plan, &resp.State)...)
}

func (r *systemdUnitResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state systemdUnitModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	sc, err := r.systemctl(&state)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("timeout"), "Invalid timeout", capitalize(err.Error())+".")
		return
	}
	found, diags := r.refresh(ctx, sc, &state)
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

func (r *systemdUnitResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, config systemdUnitModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	sc, err := r.systemctl(&plan)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("timeout"), "Invalid timeout", capitalize(err.Error())+".")
		return
	}
	resp.Diagnostics.Append(r.converge(ctx, sc, &plan, &config, false)...)
	resp.Diagnostics.Append(r.saveRefreshed(ctx, sc, &plan, &resp.State)...)
}

func (r *systemdUnitResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state systemdUnitModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.Name.ValueString()
	// State is not validated by the schema; never act on an invalid name.
	if err := validateUnitName(name); err != nil {
		resp.Diagnostics.AddError("Refusing to remove unit", capitalize(err.Error())+".")
		return
	}
	sc, err := r.systemctl(&state)
	if err != nil {
		// A timeout that no longer parses must not block destroy.
		sc = systemctl{run: r.cfg.runner(), timeout: mustParseDuration(defaultSystemdTimeout)}
	}
	target := r.unitPath(name)

	info, err := os.Lstat(target)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		resp.Diagnostics.AddError("Reading unit file", err.Error())
		return
	}
	if exists && !info.Mode().IsRegular() {
		resp.Diagnostics.AddWarning("Unit file left in place",
			fmt.Sprintf("%q is no longer a regular file, so it was not created by this resource. The unit was not stopped, disabled or removed.", target))
		return
	}

	// Only stop a unit that is (still) defined by our file, never a vendor
	// unit of the same name that became visible after our file was deleted
	// out of band. For a template, that applies to each of its instances.
	loaded, err := r.unitsLoadedFrom(ctx, sc, name, target)
	if err != nil {
		resp.Diagnostics.AddError("Querying unit", err.Error())
		return
	}
	toStop := loaded
	if exists && !isTemplateUnit(name) {
		toStop = []string{name}
	}
	if unitTypeOf(name) == "device" {
		toStop = nil // Device units follow the kernel's devices; they are never stopped.
	}
	if pre, d := req.Private.GetKey(ctx, privateKeyPreexisting); d.HasError() || string(pre) == "true" {
		// The unit was active before the resource added its file, such as
		// a mount made outside systemd; leave it as it was.
		toStop = nil
	}
	for _, u := range toStop {
		if err := stopUnit(ctx, sc, u); err != nil {
			resp.Diagnostics.AddError("Stopping unit", err.Error())
			return
		}
	}
	if !exists {
		if len(loaded) > 0 {
			if err := sc.do(ctx, "daemon-reload"); err != nil {
				resp.Diagnostics.AddError("Reloading systemd", err.Error())
			}
		}
		return
	}

	if err := disableUnit(ctx, sc, name); err != nil {
		resp.Diagnostics.AddError("Disabling unit", err.Error())
		return
	}

	// unlink never follows symlinks and never removes directories.
	if err := syscall.Unlink(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
		resp.Diagnostics.AddError("Removing unit file", explainImmutable(&fs.PathError{Op: "unlink", Path: target, Err: err}, filepath.Dir(target)).Error())
		return
	}
	if err := sc.do(ctx, "daemon-reload"); err != nil {
		resp.Diagnostics.AddError("Reloading systemd", err.Error())
	}
}

// ImportState imports a unit by name. Its content, enablement and state are
// filled in by the subsequent Read.
func (r *systemdUnitResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if err := validateUnitName(req.ID); err != nil {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("Expected a unit name: %s.", err))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("restart_on_change"), true)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("timeout"), defaultSystemdTimeout)...)
}

func (r *systemdUnitResource) unitPath(name string) string {
	return filepath.Join(r.cfg.dir(), name)
}

// systemctl returns a systemctl client using m's timeout.
func (r *systemdUnitResource) systemctl(m *systemdUnitModel) (systemctl, error) {
	return systemctlWithTimeout(r.cfg, m.Timeout)
}

// systemctlWithTimeout returns a systemctl client for cfg with the given
// timeout attribute, which defaults to defaultSystemdTimeout.
func systemctlWithTimeout(cfg *systemdConfig, timeout types.String) (systemctl, error) {
	t := timeout.ValueString()
	if t == "" {
		t = defaultSystemdTimeout
	}
	if err := validateDuration(t); err != nil {
		return systemctl{}, err
	}
	return systemctl{run: cfg.runner(), timeout: mustParseDuration(t)}, nil
}

func mustParseDuration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		panic(err)
	}
	return d
}

// checkUnitAbsent fails if a unit file already exists at target, or if
// systemd already knows a unit called name from anywhere else. A file in
// /etc/systemd/system silently overrides a vendor unit of the same name, and
// destroying the resource would then stop and disable that unit.
//
// preexisting reports a device, mount, swap or slice unit that systemd has
// without a unit file, to which the file only adds settings.
func (r *systemdUnitResource) checkUnitAbsent(ctx context.Context, sc systemctl, name, target string) (preexisting bool, err error) {
	if _, err := os.Lstat(target); err == nil {
		return false, fmt.Errorf("%q already exists; import it with terraform import to manage it", target)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if isTemplateUnit(name) {
		// systemd loads instances, never templates, so ask for unit files.
		state, err := sc.isEnabled(ctx, name)
		if err != nil {
			return false, err
		}
		if state != "not-found" {
			return false, fmt.Errorf("a template unit named %q already exists in systemd (unit file state %q), "+
				"which a file in %s would override. To change an existing unit, manage a drop-in with sysutils_systemd_dropin instead",
				name, state, r.cfg.dir())
		}
		return false, nil
	}
	props, err := sc.show(ctx, name, "LoadState", "FragmentPath", "ActiveState")
	if err != nil {
		return false, err
	}
	// Device, mount and swap units also exist without a unit file, for the
	// kernel's devices, mounts and swap areas, and systemd creates any slice
	// on demand. A unit file adds settings to them rather than overriding
	// another file.
	if props["FragmentPath"] == "" && props["LoadState"] == "loaded" {
		switch unitTypeOf(name) {
		case "device", "mount", "swap", "slice":
			return props["ActiveState"] != "inactive", nil
		}
	}
	// A unit still loaded from target itself is left over from a unit file
	// that was deleted without a daemon-reload; it is ours to recreate.
	if state := props["LoadState"]; state != "not-found" && props["FragmentPath"] != target {
		where := props["FragmentPath"]
		if where == "" {
			where = "an unknown location"
		}
		return false, fmt.Errorf("a unit named %q already exists in systemd (load state %q, defined in %s), "+
			"which a file in %s would override. To change an existing unit, manage a drop-in (%s.d/*.conf) with sysutils_systemd_dropin instead",
			name, state, where, r.cfg.dir(), target)
	}
	return false, nil
}

// unitsLoadedFrom returns the units that systemd currently has loaded from
// the unit file target: name itself, or for a template, its instances.
func (r *systemdUnitResource) unitsLoadedFrom(ctx context.Context, sc systemctl, name, target string) ([]string, error) {
	pattern := name
	if isTemplateUnit(name) {
		i := strings.LastIndexByte(name, '.')
		pattern = name[:i] + "*" + name[i:]
	}
	units, err := sc.showAll(ctx, pattern, "Id", "FragmentPath")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, u := range units {
		if u["FragmentPath"] == target && u["Id"] != "" {
			out = append(out, u["Id"])
		}
	}
	return out, nil
}

// checkLoaded fails unless systemd could load the unit after a
// daemon-reload. A template is checked through an instance of it.
func checkLoaded(ctx context.Context, sc systemctl, name string) diag.Diagnostics {
	var diags diag.Diagnostics
	probe := name
	if isTemplateUnit(name) {
		probe = strings.Replace(name, "@.", "@sysutils-load-check.", 1)
	}
	props, err := sc.show(ctx, probe, "LoadState")
	if err != nil {
		diags.AddError("Querying unit", err.Error())
		return diags
	}
	if state := props["LoadState"]; state != "loaded" {
		diags.AddError("Unit failed to load",
			fmt.Sprintf("After daemon-reload, systemd reports load state %q for %s. Check the unit file; `systemctl status %s` and the journal show the reason.", state, probe, probe))
	}
	return diags
}

// converge brings the unit on the host in line with m: it writes the unit
// file, reloads systemd if the file changed, and applies enabled and state.
// Only enabled and state set in config are enforced; the plan may carry the
// prior values of unmanaged ones. created is true if the file was just
// written by Create.
func (r *systemdUnitResource) converge(ctx context.Context, sc systemctl, m, config *systemdUnitModel, created bool) diag.Diagnostics {
	var diags diag.Diagnostics
	name := m.Name.ValueString()

	changed, semantic := created, created
	if !created {
		var err error
		changed, semantic, err = r.writeUnitFile(m, config)
		if err != nil {
			diags.AddError("Writing unit file", err.Error())
			return diags
		}
	}

	if changed {
		if err := sc.do(ctx, "daemon-reload"); err != nil {
			diags.AddError("Reloading systemd", err.Error())
			return diags
		}
		if diags.Append(checkLoaded(ctx, sc, name)...); diags.HasError() {
			return diags
		}
	}

	enabled, err := sc.isEnabled(ctx, name)
	if err != nil {
		diags.AddError("Querying unit", err.Error())
		return diags
	}

	if semantic && !created {
		// Re-enabling recreates the [Install] symlinks, which may have
		// changed along with the file.
		if unitFileEnabled(enabled) && (config.Enabled.IsNull() || config.Enabled.ValueBool()) {
			if err := sc.do(ctx, "reenable", "--", name); err != nil {
				diags.AddError("Re-enabling unit", err.Error())
				return diags
			}
		}
		if m.RestartOnChange.ValueBool() && config.State.ValueString() != unitStateStopped {
			if diags.Append(r.restartRunning(ctx, sc, name)...); diags.HasError() {
				return diags
			}
		}
	}

	if !config.Enabled.IsNull() && config.Enabled.ValueBool() != unitFileEnabled(enabled) {
		diags.Append(setUnitEnabled(ctx, sc, name, config.Enabled.ValueBool())...)
		if diags.HasError() {
			return diags
		}
	}
	if !config.State.IsNull() {
		active, err := sc.isActive(ctx, name)
		if err != nil {
			diags.AddError("Querying unit", err.Error())
			return diags
		}
		if config.State.ValueString() != unitRunState(active) {
			diags.Append(setUnitRunState(ctx, sc, name, config.State.ValueString())...)
		}
	}
	return diags
}

// restartRunning restarts the unit if it is running, or for a template,
// each running instance that uses it.
func (r *systemdUnitResource) restartRunning(ctx context.Context, sc systemctl, name string) diag.Diagnostics {
	var diags diag.Diagnostics
	units := []string{name}
	if isTemplateUnit(name) {
		var err error
		if units, err = r.unitsLoadedFrom(ctx, sc, name, r.unitPath(name)); err != nil {
			diags.AddError("Querying unit", err.Error())
			return diags
		}
	}
	for _, u := range units {
		active, err := sc.isActive(ctx, u)
		if err != nil {
			diags.AddError("Querying unit", err.Error())
			return diags
		}
		if unitRunState(active) != unitStateRunning {
			continue
		}
		if err := sc.do(ctx, "try-restart", "--", u); err != nil {
			diags.AddError("Restarting unit", err.Error())
			return diags
		}
	}
	return diags
}

// writeUnitFile writes the desired content to the unit file if it differs
// from what is on disk. It reports whether it wrote the file, and whether
// systemd reads the new file differently from the old one. The file is
// replaced atomically and keeps its owner and mode.
func (r *systemdUnitResource) writeUnitFile(m, config *systemdUnitModel) (changed, semantic bool, err error) {
	data, _, err := desiredUnitBytes(unitFileSpecs, &config.unitContentModel, nil, false)
	if err != nil {
		return false, false, err
	}
	target := r.unitPath(m.Name.ValueString())
	return writeUnitFileAt(target, data)
}

// writeUnitFileAt replaces target with data unless it already holds data.
func writeUnitFileAt(target string, data []byte) (changed, semantic bool, err error) {
	unlock, err := lockFileForEdit(target)
	if err != nil {
		return false, false, err
	}
	defer unlock()
	current, snap, err := readRegularFileNoFollow(target, maxUnitFileSize)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Deleted since the last refresh; recreate it.
		snap = nil
		semantic = true
	case err != nil:
		return false, false, err
	case bytes.Equal(current, data):
		return false, false, nil
	default:
		semantic = !unitFilesEquivalent(current, data)
	}
	if err := replaceFileAtomic(target, data, snap, systemdUnitFileMode); err != nil {
		return false, false, err
	}
	return true, semantic, nil
}

func setUnitEnabled(ctx context.Context, sc systemctl, name string, enable bool) diag.Diagnostics {
	var diags diag.Diagnostics
	verb := "disable"
	if enable {
		verb = "enable"
	}
	if err := sc.do(ctx, verb, "--", name); err != nil {
		diags.AddError("Changing unit enablement", err.Error())
		return diags
	}
	state, err := sc.isEnabled(ctx, name)
	if err != nil {
		diags.AddError("Querying unit", err.Error())
		return diags
	}
	if unitFileEnabled(state) != enable {
		detail := fmt.Sprintf("systemctl %s succeeded, but systemctl is-enabled reports %q for %s.", verb, state, name)
		if enable {
			detail += " Only units with an [Install] section that sets WantedBy=, RequiredBy= or Alias= can be enabled."
		}
		diags.AddError("Changing unit enablement", detail)
	}
	return diags
}

func setUnitRunState(ctx context.Context, sc systemctl, name, want string) diag.Diagnostics {
	var diags diag.Diagnostics
	verb := "stop"
	if want == unitStateRunning {
		verb = "start"
	}
	if err := sc.do(ctx, verb, "--", name); err != nil {
		diags.AddError("Changing unit state", err.Error())
		return diags
	}
	active, err := sc.isActive(ctx, name)
	if err != nil {
		diags.AddError("Querying unit", err.Error())
		return diags
	}
	if got := unitRunState(active); got != want {
		detail := fmt.Sprintf("systemctl %s succeeded, but systemctl is-active reports %q for %s.", verb, active, name)
		if want == unitStateRunning {
			detail += " The unit may have exited right after starting; a Type=oneshot service needs RemainAfterExit=yes to stay active. `systemctl status " + name + "` and the journal show more."
		}
		diags.AddError("Changing unit state", detail)
	}
	return diags
}

// stopUnit stops name if it is active or changing state, and clears a failed
// state so that systemd can forget the unit once its file is gone.
func stopUnit(ctx context.Context, sc systemctl, name string) error {
	active, err := sc.isActive(ctx, name)
	if err != nil {
		return err
	}
	switch active {
	case "inactive":
		return nil
	case "failed":
		return sc.do(ctx, "reset-failed", "--", name)
	default:
		return sc.do(ctx, "stop", "--", name)
	}
}

// disableUnit disables name if it is enabled, persistently or at runtime.
func disableUnit(ctx context.Context, sc systemctl, name string) error {
	enabled, err := sc.isEnabled(ctx, name)
	if err != nil {
		return err
	}
	switch enabled {
	case "enabled":
		return sc.do(ctx, "disable", "--", name)
	case "enabled-runtime":
		return sc.do(ctx, "disable", "--runtime", "--", name)
	default:
		return nil
	}
}

// saveRefreshed re-reads the unit into m and stores it in state. It is used
// after writes, so a missing unit file is an error rather than a removal.
//
// If the file's checksum differs from the one planned, which happens when a
// source file changes between plan and apply, the refreshed state is still
// stored but an error asks for another apply.
func (r *systemdUnitResource) saveRefreshed(ctx context.Context, sc systemctl, m *systemdUnitModel, state stateSetter) diag.Diagnostics {
	planned := m.ContentSHA256
	found, diags := r.refresh(ctx, sc, m)
	if diags.HasError() {
		return diags
	}
	if !found {
		diags.AddError("Unit file missing after write",
			fmt.Sprintf("%q disappeared while the unit was being configured.", r.unitPath(m.Name.ValueString())))
		return diags
	}
	diags.Append(state.Set(ctx, m)...)
	if !planned.IsUnknown() && !planned.IsNull() && !planned.Equal(m.ContentSHA256) {
		diags.AddAttributeError(unitContentAttribute(&m.unitContentModel), "Content changed during apply",
			fmt.Sprintf("The unit file %q has SHA-256 %s, but the plan expected %s. "+
				"The source file was probably modified between plan and apply; run terraform apply again.",
				r.unitPath(m.Name.ValueString()), m.ContentSHA256.ValueString(), planned.ValueString()))
	}
	return diags
}

// stateSetter is the part of tfsdk.State used by saveRefreshed.
type stateSetter interface {
	Set(ctx context.Context, val any) diag.Diagnostics
}

// refresh reads the unit file and the unit's enablement and active state into
// m. It reports found=false if the unit file does not exist.
//
// content is set to the file's text, so that drift shows as a readable diff,
// and the sections to the file as systemd reads it.
func (r *systemdUnitResource) refresh(ctx context.Context, sc systemctl, m *systemdUnitModel) (bool, diag.Diagnostics) {
	var diags diag.Diagnostics
	name := m.Name.ValueString()
	target := r.unitPath(name)

	data, _, err := readRegularFileNoFollow(target, maxUnitFileSize)
	if errors.Is(err, fs.ErrNotExist) {
		return false, diags
	}
	if err != nil {
		diags.AddError("Reading unit file", err.Error())
		return false, diags
	}
	refreshUnitContent(unitFileSpecs, unitTypeOf(name), data, &m.unitContentModel, nil)

	enabled, err := sc.isEnabled(ctx, name)
	if err != nil {
		diags.AddError("Querying unit", err.Error())
		return false, diags
	}
	m.Enabled = types.BoolValue(unitFileEnabled(enabled))
	m.State = types.StringNull()
	if !isTemplateUnit(name) {
		active, err := sc.isActive(ctx, name)
		if err != nil {
			diags.AddError("Querying unit", err.Error())
			return false, diags
		}
		m.State = types.StringValue(unitRunState(active))
	}
	m.Path = types.StringValue(target)
	m.ID = types.StringValue(name)
	return true, diags
}
