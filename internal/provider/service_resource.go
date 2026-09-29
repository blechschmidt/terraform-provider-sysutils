package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = (*serviceResource)(nil)
	_ resource.ResourceWithConfigure   = (*serviceResource)(nil)
	_ resource.ResourceWithImportState = (*serviceResource)(nil)
	_ resource.ResourceWithModifyPlan  = (*serviceResource)(nil)
)

const defaultServiceTimeout = "2m"

func NewServiceResource() resource.Resource { return &serviceResource{} }

type serviceResource struct {
	hostOnlyResource
	cfg *serviceConfig
}

type serviceModel struct {
	Name            types.String `tfsdk:"name"`
	Enabled         types.Bool   `tfsdk:"enabled"`
	State           types.String `tfsdk:"state"`
	RestartOnChange types.Map    `tfsdk:"restart_on_change"`
	Runlevel        types.String `tfsdk:"runlevel"`
	Timeout         types.String `tfsdk:"timeout"`
	InitSystem      types.String `tfsdk:"init_system"`
	ID              types.String `tfsdk:"id"`
}

func (r *serviceResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_service"
}

func (r *serviceResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages the runtime state of an existing service: whether it starts at boot and whether it is running, and restarts it when any value in `restart_on_change` changes. " +
			"Uses `systemctl` when systemd booted the host and `rc-service`/`rc-update` when OpenRC did, as on Alpine Linux. " +
			"The service itself must already exist; install it with `sysutils_package` or define it with `sysutils_systemd_unit`. " +
			"Destroying the resource leaves the service as it is. Requires root privileges.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Name of the service. With systemd, a unit name such as `\"nginx.service\"`, `\"nginx\"` (short for `nginx.service`), `\"getty@tty2.service\"` or `\"backup.timer\"`; aliases are resolved to the unit they name. " +
					"With OpenRC, the name of a script in `/etc/init.d`, such as `\"nginx\"`. Changing this forces a new resource.",
				Validators:    []validator.String{stringCheck("service name", validateServiceName)},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"enabled": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Whether the service starts at boot: `systemctl enable`/`disable`, or with OpenRC `rc-update add`/`del` in `runlevel`. " +
					"If unset, the enablement is not managed and the current value is reported. " +
					"With systemd, only `enabled` counts as enabled; `static`, `indirect`, `enabled-runtime` and `masked` units are reported as not enabled, and only units with an `[Install]` section can be enabled.",
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"state": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Whether the service should be `\"running\"` or `\"stopped\"`. " +
					"If unset, the service is not started or stopped and the current value is reported. " +
					"With systemd, a unit counts as running when `systemctl is-active` reports `active`, `reloading` or `refreshing`; with OpenRC, when `rc-service <name> status` reports `started`.",
				Validators:    []validator.String{stringvalidator.OneOf(serviceStateRunning, serviceStateStopped)},
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"restart_on_change": schema.MapAttribute{
				Optional:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Arbitrary map of values whose change restarts the service, like the `triggers_replace` of `terraform_data`, but updating in place. " +
					"Use it to restart the service when its configuration changes, for example with the `content_sha256` of a `sysutils_file`. " +
					"The values are not passed to the service. Only a running service is restarted, and never while `state` is `\"stopped\"`; a service started by the same apply is not restarted again. " +
					"Setting the map for the first time, including on create, does not restart the service; removing it does not either. " +
					"With systemd, `systemctl daemon-reload` runs before the restart, so that changed unit files and drop-ins take effect.",
			},
			"runlevel": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(defaultOpenRCRunlevel),
				MarkdownDescription: "OpenRC runlevel that `enabled` refers to. Defaults to `\"default\"`, the runlevel that `rc-update add` uses. " +
					"A service is enabled when it is in this runlevel, whatever other runlevels it is in. Ignored with systemd.",
				Validators: []validator.String{stringCheck("runlevel", validateRunlevel)},
			},
			"timeout": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(defaultServiceTimeout),
				MarkdownDescription: "Maximum time each `systemctl`, `rc-service` or `rc-update` invocation may take, as a Go duration such as `\"30s\"` or `\"5m\"`. " +
					"Starting, stopping and restarting wait for the service, so this should exceed its own start and stop timeouts. Defaults to `\"2m\"`.",
				Validators: []validator.String{positiveDuration()},
			},
			"init_system": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Init system that manages the service: `\"systemd\"` or `\"openrc\"`. " +
					"Detected on every plan: systemd if `/run/systemd/system` exists, which systemd creates when it is PID 1; otherwise OpenRC if `/run/openrc/softlevel` exists. " +
					"If neither exists, planning fails.",
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Resource identifier (equal to `name`).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *serviceResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	data, diags := providerDataFrom(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	if data != nil {
		r.cfg = data.service
		r.fsRoot = data.root
	}
}

// ModifyPlan refuses to plan with root_dir, detects the init system, and
// marks unmanaged values unknown when the apply may change them.
func (r *serviceResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	r.hostOnlyResource.ModifyPlan(ctx, req, resp)
	if resp.Diagnostics.HasError() || req.Plan.Raw.IsNull() {
		return
	}
	var config, plan serviceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	kind, err := r.cfg.detect()
	if err != nil {
		resp.Diagnostics.AddError("No supported init system", capitalize(err.Error())+".")
		return
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("init_system"), kind)...)
	if !config.Name.IsUnknown() {
		if err := validateServiceNameFor(kind, config.Name.ValueString()); err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("name"), "Invalid service name", capitalize(err.Error())+".")
			return
		}
	}

	if req.State.Raw.IsNull() {
		return // Create: unmanaged values are unknown already.
	}
	var prior serviceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Enablement is looked up in a different place now, so the prior
	// value no longer predicts it.
	if config.Enabled.IsNull() && (prior.InitSystem.ValueString() != kind || !prior.Runlevel.Equal(plan.Runlevel)) {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("enabled"), types.BoolUnknown())...)
	}
	// A restart can fail and leave the service stopped.
	if config.State.IsNull() && triggersChanged(prior.RestartOnChange, plan.RestartOnChange) {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("state"), types.StringUnknown())...)
	}
}

// triggersChanged reports whether restart_on_change changed from prior to
// planned in a way that requires a restart. Setting the map for the first
// time or removing it does not: there is no previous value to differ from.
// An unknown map may change.
func triggersChanged(prior, planned types.Map) bool {
	if prior.IsNull() || planned.IsNull() {
		return false
	}
	if planned.IsUnknown() || prior.IsUnknown() {
		return true
	}
	return !prior.Equal(planned)
}

func (r *serviceResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan, config serviceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	mgr, diags := r.manager(&plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Nothing is created, so a failure leaves nothing to record: the next
	// apply retries from whatever state the service is in.
	resp.Diagnostics.Append(r.converge(ctx, mgr, &plan, &config, false)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.saveRefreshed(ctx, mgr, &plan, &resp.State)...)
}

func (r *serviceResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state serviceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	mgr, diags := r.manager(&state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	found, diags := r.refresh(ctx, mgr, &state)
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

func (r *serviceResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, config, prior serviceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	mgr, diags := r.manager(&plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	restart := triggersChanged(prior.RestartOnChange, plan.RestartOnChange)
	diags = r.converge(ctx, mgr, &plan, &config, restart)
	if diags.HasError() {
		// The framework keeps the prior state after an error, including
		// the old restart_on_change, so the next apply retries a failed
		// restart. Record what the service looks like now, though.
		resp.Diagnostics.Append(diags...)
		if found, rdiags := r.refresh(ctx, mgr, &prior); found && !rdiags.HasError() {
			resp.Diagnostics.Append(resp.State.Set(ctx, &prior)...)
		}
		return
	}
	resp.Diagnostics.Append(diags...)
	resp.Diagnostics.Append(r.saveRefreshed(ctx, mgr, &plan, &resp.State)...)
}

// Delete leaves the service as it is: the resource only manages runtime
// state, and a service that outlives its resource is the less surprising
// outcome.
func (r *serviceResource) Delete(context.Context, resource.DeleteRequest, *resource.DeleteResponse) {}

// ImportState imports a service by name. Its enablement and state are
// filled in by the subsequent Read.
func (r *serviceResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if err := validateServiceName(req.ID); err != nil {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("Expected a service name: %s.", err))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("runlevel"), defaultOpenRCRunlevel)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("timeout"), defaultServiceTimeout)...)
}

// manager detects the init system and returns its backend, configured with
// m's timeout and runlevel.
func (r *serviceResource) manager(m *serviceModel) (serviceManager, diag.Diagnostics) {
	var diags diag.Diagnostics
	t := m.Timeout.ValueString()
	if t == "" {
		t = defaultServiceTimeout
	}
	if err := validateDuration(t); err != nil {
		diags.AddAttributeError(path.Root("timeout"), "Invalid timeout", capitalize(err.Error())+".")
		return nil, diags
	}
	level := m.Runlevel.ValueString()
	if level == "" {
		level = defaultOpenRCRunlevel
	}
	if err := validateRunlevel(level); err != nil {
		diags.AddAttributeError(path.Root("runlevel"), "Invalid runlevel", capitalize(err.Error())+".")
		return nil, diags
	}
	kind, err := r.cfg.detect()
	if err != nil {
		diags.AddError("No supported init system", capitalize(err.Error())+".")
		return nil, diags
	}
	// State is not validated by the schema; never pass an invalid name to
	// the init system.
	if err := validateServiceNameFor(kind, m.Name.ValueString()); err != nil {
		diags.AddAttributeError(path.Root("name"), "Invalid service name", capitalize(err.Error())+".")
		return nil, diags
	}
	mgr, err := r.cfg.manager(kind, mustParseDuration(t), level)
	if err != nil {
		diags.AddError("No supported init system", capitalize(err.Error())+".")
		return nil, diags
	}
	return mgr, diags
}

// converge brings the service in line with config: it enables or disables
// it and starts or stops it, as far as config sets enabled and state, and
// restarts it if restart is true and it was already running. Every change
// is verified by reading the status back.
func (r *serviceResource) converge(ctx context.Context, mgr serviceManager, m, config *serviceModel, restart bool) diag.Diagnostics {
	var diags diag.Diagnostics
	name := m.Name.ValueString()

	st, err := mgr.Status(ctx, name)
	if err != nil {
		diags.AddError("Querying service", err.Error())
		return diags
	}
	if !st.Found {
		diags.AddAttributeError(path.Root("name"), "Service not found", serviceNotFoundDetail(mgr.Kind(), name))
		return diags
	}
	unit := st.Unit

	if !config.Enabled.IsNull() && config.Enabled.ValueBool() != st.Enabled {
		want := config.Enabled.ValueBool()
		if err := mgr.SetEnabled(ctx, unit, want); err != nil {
			diags.AddError("Changing service enablement", err.Error())
			return diags
		}
		now, err := mgr.Status(ctx, name)
		if err != nil {
			diags.AddError("Querying service", err.Error())
			return diags
		}
		if now.Enabled != want {
			detail := fmt.Sprintf("The %s command succeeded, but %s still reports %s for %s.", verbFor(want, "enable", "disable"), mgr.Kind(), now.Detail, unit)
			if want {
				detail += " " + mgr.EnableHint(unit)
			}
			diags.AddError("Changing service enablement", detail)
			return diags
		}
	}

	started := false
	if !config.State.IsNull() {
		want := config.State.ValueString() == serviceStateRunning
		if want != st.Running {
			if err := mgr.SetRunning(ctx, unit, want); err != nil {
				diags.AddError("Changing service state", err.Error())
				return diags
			}
			diags.Append(checkRunning(ctx, mgr, name, unit, want, verbFor(want, "start", "stop"))...)
			if diags.HasError() {
				return diags
			}
			started = want
		}
	}

	if restart && st.Running && !started && config.State.ValueString() != serviceStateStopped {
		if err := mgr.Restart(ctx, unit); err != nil {
			diags.AddError("Restarting service", err.Error())
			return diags
		}
		diags.Append(checkRunning(ctx, mgr, name, unit, true, "restart")...)
	}
	return diags
}

// checkRunning verifies that the service is running (or not) after verb.
func checkRunning(ctx context.Context, mgr serviceManager, name, unit string, want bool, verb string) diag.Diagnostics {
	var diags diag.Diagnostics
	now, err := mgr.Status(ctx, name)
	if err != nil {
		diags.AddError("Querying service", err.Error())
		return diags
	}
	if now.Running != want {
		detail := fmt.Sprintf("The %s command succeeded, but %s reports %s for %s.", verb, mgr.Kind(), now.Detail, unit)
		if want {
			detail += " " + mgr.StartHint(unit)
		}
		diags.AddError("Changing service state", detail)
	}
	return diags
}

func verbFor(b bool, ifTrue, ifFalse string) string {
	if b {
		return ifTrue
	}
	return ifFalse
}

func serviceNotFoundDetail(kind, name string) string {
	switch kind {
	case initSystemOpenRC:
		return fmt.Sprintf("OpenRC has no service named %q: rc-service --exists found no script for it in /etc/init.d. Install the package that provides it first.", name)
	default:
		return fmt.Sprintf("systemd has no unit named %q (load state \"not-found\"). Install the package that provides it, or define it with sysutils_systemd_unit and reference that resource's name so that it is created first.", name)
	}
}

// saveRefreshed re-reads the service into m and stores it in state. It is
// used after changes, so a missing service is an error rather than a
// removal.
func (r *serviceResource) saveRefreshed(ctx context.Context, mgr serviceManager, m *serviceModel, state stateSetter) diag.Diagnostics {
	found, diags := r.refresh(ctx, mgr, m)
	if diags.HasError() {
		return diags
	}
	if !found {
		diags.AddAttributeError(path.Root("name"), "Service disappeared", serviceNotFoundDetail(mgr.Kind(), m.Name.ValueString()))
		return diags
	}
	diags.Append(state.Set(ctx, m)...)
	return diags
}

// refresh reads the service's enablement and state into m. It reports
// found=false if the init system knows no such service.
func (r *serviceResource) refresh(ctx context.Context, mgr serviceManager, m *serviceModel) (bool, diag.Diagnostics) {
	var diags diag.Diagnostics
	st, err := mgr.Status(ctx, m.Name.ValueString())
	if err != nil {
		diags.AddError("Querying service", err.Error())
		return false, diags
	}
	if !st.Found {
		return false, diags
	}
	m.Enabled = types.BoolValue(st.Enabled)
	m.State = types.StringValue(verbFor(st.Running, serviceStateRunning, serviceStateStopped))
	m.InitSystem = types.StringValue(mgr.Kind())
	m.ID = types.StringValue(m.Name.ValueString())
	if m.Runlevel.IsNull() || m.Runlevel.ValueString() == "" {
		m.Runlevel = types.StringValue(defaultOpenRCRunlevel)
	}
	if m.Timeout.IsNull() || m.Timeout.ValueString() == "" {
		m.Timeout = types.StringValue(defaultServiceTimeout)
	}
	return true, diags
}
