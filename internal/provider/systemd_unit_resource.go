package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/hashicorp/terraform-plugin-framework-validators/resourcevalidator"
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
	_ resource.Resource                     = (*systemdUnitResource)(nil)
	_ resource.ResourceWithConfigure        = (*systemdUnitResource)(nil)
	_ resource.ResourceWithImportState      = (*systemdUnitResource)(nil)
	_ resource.ResourceWithConfigValidators = (*systemdUnitResource)(nil)
	_ resource.ResourceWithModifyPlan       = (*systemdUnitResource)(nil)
)

const (
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
	Name            types.String `tfsdk:"name"`
	Content         types.String `tfsdk:"content"`
	Source          types.String `tfsdk:"source"`
	ContentSHA256   types.String `tfsdk:"content_sha256"`
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
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a systemd unit: writes its unit file to `/etc/systemd/system`, runs `systemctl daemon-reload` when the file changes, and optionally enables and starts or stops the unit. " +
			"On destroy the unit is stopped and disabled, and its file is removed. Requires root privileges and a host booted with systemd.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Unit name including its type suffix, such as `\"app.service\"` or `\"backup.timer\"`. " +
					"Supported suffixes are `.service`, `.socket`, `.target`, `.timer`, `.path`, `.mount`, `.automount`, `.swap` and `.slice`. " +
					"Template and instance units (names containing `@`) are not supported. " +
					"A unit with this name must not already exist anywhere on the host. Changing this forces a new resource.",
				Validators:    []validator.String{unitName()},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"content": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Contents of the unit file. Exactly one of `content` and `source` must be set.",
			},
			"source": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Path to a local file whose contents are used as the unit file. Exactly one of `content` and `source` must be set. " +
					"Relative paths are resolved against Terraform's working directory; prefer `${path.module}/...`. " +
					"The source is hashed during every plan, so a change to its contents plans an update.",
				Validators: []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"content_sha256": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Hex-encoded SHA-256 checksum of the unit file. Known at plan time, so other resources can use it to react to changes of the unit.",
			},
			"enabled": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Whether the unit is enabled to start at boot (`systemctl enable`/`disable`). " +
					"If unset, the enablement is not managed and the current value is reported. " +
					"Only units with an `[Install]` section can be enabled.",
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"state": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Whether the unit should be `\"running\"` or `\"stopped\"` (`systemctl start`/`stop`). " +
					"If unset, the unit is not started or stopped and the current value is reported. " +
					"A unit counts as running when `systemctl is-active` reports `active`, `reloading` or `refreshing`.",
				Validators:    []validator.String{stringvalidator.OneOf(unitStateRunning, unitStateStopped)},
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"restart_on_change": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
				MarkdownDescription: "Whether to restart the unit (`systemctl try-restart`) when its unit file changes and it is running, so that the change takes effect. " +
					"Never applies while `state` is `\"stopped\"`. Defaults to `true`.",
			},
			"timeout": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(defaultSystemdTimeout),
				MarkdownDescription: "Maximum time each `systemctl` invocation may take, as a Go duration such as `\"30s\"` or `\"5m\"`. " +
					"`systemctl start` and `stop` wait for the unit to finish starting or stopping, so this should exceed the unit's own start and stop timeouts. Defaults to `\"2m\"`.",
				Validators: []validator.String{positiveDuration()},
			},
			"path": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Path of the unit file, `/etc/systemd/system/<name>`.",
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Resource identifier (equal to `name`).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
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

func (r *systemdUnitResource) ConfigValidators(_ context.Context) []resource.ConfigValidator {
	return []resource.ConfigValidator{
		resourcevalidator.ExactlyOneOf(path.MatchRoot("content"), path.MatchRoot("source")),
	}
}

// ModifyPlan sets path from the name and content_sha256 from the desired
// content. Refresh records the checksum of the file on disk, so out-of-band
// edits and changes to a source file show up as a planned update.
func (r *systemdUnitResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // Destroy.
	}
	var config systemdUnitModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	unitPath := types.StringUnknown()
	if !config.Name.IsUnknown() {
		unitPath = types.StringValue(r.unitPath(config.Name.ValueString()))
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("path"), unitPath)...)

	sha := types.StringUnknown()
	sums, known, err := desiredChecksums(&fileModel{Content: config.Content, Source: config.Source})
	if err != nil {
		resp.Diagnostics.AddAttributeError(unitContentAttribute(&config), "Unable to read unit file contents", capitalize(err.Error())+".")
		return
	}
	if known {
		sha = types.StringValue(sums.sha256)
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("content_sha256"), sha)...)

	// A content change can re-enable or restart the unit, which may change
	// enabled and state when they are not managed. Their prior values then
	// no longer predict the result.
	if req.State.Raw.IsNull() {
		return // Create: unmanaged values are unknown already.
	}
	var prior types.String
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("content_sha256"), &prior)...)
	if resp.Diagnostics.HasError() || sha.Equal(prior) {
		return
	}
	if config.Enabled.IsNull() {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("enabled"), types.BoolUnknown())...)
	}
	if config.State.IsNull() {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("state"), types.StringUnknown())...)
	}
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
	data, err := desiredUnitContent(&plan)
	if err != nil {
		resp.Diagnostics.AddAttributeError(unitContentAttribute(&plan), "Unable to read unit file contents", capitalize(err.Error())+".")
		return
	}

	target := r.unitPath(name)
	if err := r.checkUnitAbsent(ctx, sc, name, target); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("name"), "Unit already exists", capitalize(err.Error())+".")
		return
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
	// out of band.
	props, err := sc.show(ctx, name, "FragmentPath")
	if err != nil {
		resp.Diagnostics.AddError("Querying unit", err.Error())
		return
	}
	if exists || props["FragmentPath"] == target {
		if err := stopUnit(ctx, sc, name); err != nil {
			resp.Diagnostics.AddError("Stopping unit", err.Error())
			return
		}
	}
	if !exists {
		if props["FragmentPath"] == target {
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
		resp.Diagnostics.AddError("Removing unit file", (&fs.PathError{Op: "unlink", Path: target, Err: err}).Error())
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
	t := m.Timeout.ValueString()
	if t == "" {
		t = defaultSystemdTimeout
	}
	if err := validateDuration(t); err != nil {
		return systemctl{}, err
	}
	return systemctl{run: r.cfg.runner(), timeout: mustParseDuration(t)}, nil
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
func (r *systemdUnitResource) checkUnitAbsent(ctx context.Context, sc systemctl, name, target string) error {
	if _, err := os.Lstat(target); err == nil {
		return fmt.Errorf("%q already exists; import it with terraform import to manage it", target)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	props, err := sc.show(ctx, name, "LoadState", "FragmentPath")
	if err != nil {
		return err
	}
	// A unit still loaded from target itself is left over from a unit file
	// that was deleted without a daemon-reload; it is ours to recreate.
	if state := props["LoadState"]; state != "not-found" && props["FragmentPath"] != target {
		where := props["FragmentPath"]
		if where == "" {
			where = "an unknown location"
		}
		return fmt.Errorf("a unit named %q already exists in systemd (load state %q, defined in %s), "+
			"which a file in %s would override. To change an existing unit, manage a drop-in file (%s.d/*.conf) with sysutils_file instead",
			name, state, where, r.cfg.dir(), target)
	}
	return nil
}

// converge brings the unit on the host in line with m: it writes the unit
// file, reloads systemd if the file changed, and applies enabled and state.
// Only enabled and state set in config are enforced; the plan may carry the
// prior values of unmanaged ones. created is true if the file was just
// written by Create.
func (r *systemdUnitResource) converge(ctx context.Context, sc systemctl, m, config *systemdUnitModel, created bool) diag.Diagnostics {
	var diags diag.Diagnostics
	name := m.Name.ValueString()

	changed := created
	if !created {
		var err error
		changed, err = r.writeUnitFile(m)
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
		props, err := sc.show(ctx, name, "LoadState")
		if err != nil {
			diags.AddError("Querying unit", err.Error())
			return diags
		}
		if state := props["LoadState"]; state != "loaded" {
			diags.AddError("Unit failed to load",
				fmt.Sprintf("After daemon-reload, systemd reports load state %q for %s. Check the unit file; `systemctl status %s` and the journal show the reason.", state, name, name))
			return diags
		}
	}

	enabled, err := sc.isEnabled(ctx, name)
	if err != nil {
		diags.AddError("Querying unit", err.Error())
		return diags
	}
	active, err := sc.isActive(ctx, name)
	if err != nil {
		diags.AddError("Querying unit", err.Error())
		return diags
	}

	if changed && !created {
		// Re-enabling recreates the [Install] symlinks, which may have
		// changed along with the file.
		if unitFileEnabled(enabled) && (config.Enabled.IsNull() || config.Enabled.ValueBool()) {
			if err := sc.do(ctx, "reenable", "--", name); err != nil {
				diags.AddError("Re-enabling unit", err.Error())
				return diags
			}
		}
		if m.RestartOnChange.ValueBool() && config.State.ValueString() != unitStateStopped && unitRunState(active) == unitStateRunning {
			if err := sc.do(ctx, "try-restart", "--", name); err != nil {
				diags.AddError("Restarting unit", err.Error())
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
	if !config.State.IsNull() && config.State.ValueString() != unitRunState(active) {
		diags.Append(setUnitRunState(ctx, sc, name, config.State.ValueString())...)
	}
	return diags
}

// writeUnitFile writes the desired content to the unit file if it differs
// from what is on disk, and reports whether it did. The file is replaced
// atomically and keeps its owner and mode.
func (r *systemdUnitResource) writeUnitFile(m *systemdUnitModel) (bool, error) {
	data, err := desiredUnitContent(m)
	if err != nil {
		return false, err
	}
	target := r.unitPath(m.Name.ValueString())
	current, snap, err := readRegularFileNoFollow(target, maxUnitFileSize)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Deleted since the last refresh; recreate it.
		snap = nil
	case err != nil:
		return false, err
	case bytes.Equal(current, data):
		return false, nil
	}
	if err := replaceFileAtomic(target, data, snap, systemdUnitFileMode); err != nil {
		return false, err
	}
	return true, nil
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
		diags.AddAttributeError(unitContentAttribute(m), "Content changed during apply",
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
// With content set (or after import), content is set to the file's text so
// that drift shows as a readable diff; with source set, drift is detected by
// comparing content_sha256 with the checksum computed during plan.
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
	sums, err := checksumReader(bytes.NewReader(data))
	if err != nil {
		diags.AddError("Reading unit file", err.Error())
		return false, diags
	}
	m.ContentSHA256 = types.StringValue(sums.sha256)
	if m.Source.IsNull() && utf8.Valid(data) {
		m.Content = types.StringValue(string(data))
	}

	enabled, err := sc.isEnabled(ctx, name)
	if err != nil {
		diags.AddError("Querying unit", err.Error())
		return false, diags
	}
	active, err := sc.isActive(ctx, name)
	if err != nil {
		diags.AddError("Querying unit", err.Error())
		return false, diags
	}
	m.Enabled = types.BoolValue(unitFileEnabled(enabled))
	m.State = types.StringValue(unitRunState(active))
	m.Path = types.StringValue(target)
	m.ID = types.StringValue(name)
	return true, diags
}

// desiredUnitContent returns the unit file contents configured in m.
func desiredUnitContent(m *systemdUnitModel) ([]byte, error) {
	if !m.Content.IsNull() {
		return []byte(m.Content.ValueString()), nil
	}
	if m.Source.IsNull() {
		return nil, errors.New("one of content or source must be set")
	}
	src, err := openSource(m.Source.ValueString())
	if err != nil {
		return nil, err
	}
	defer func() { _ = src.Close() }()
	data, err := io.ReadAll(io.LimitReader(src, maxUnitFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("reading source: %w", err)
	}
	if len(data) > maxUnitFileSize {
		return nil, fmt.Errorf("source %q is larger than %d bytes", m.Source.ValueString(), maxUnitFileSize)
	}
	return data, nil
}

// unitContentAttribute returns the path of whichever content attribute is
// set in m, for attaching diagnostics.
func unitContentAttribute(m *systemdUnitModel) path.Path {
	if !m.Source.IsNull() {
		return path.Root("source")
	}
	return path.Root("content")
}
