package provider

// Provider-defined actions (Terraform 1.14 and later) that operate on
// services: sysutils_service restarts, reloads, starts or stops a service
// through the serviceManager backends of sysutils_service, and
// sysutils_systemd_daemon_reload reloads systemd's configuration. They are
// invoked from action_trigger blocks in a resource's lifecycle, for example
// after a configuration file changed, or with "terraform apply -invoke".
// They change the running host, never state.

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/action/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Values of the action attribute of the sysutils_service action.
const (
	serviceActionRestart = "restart"
	serviceActionReload  = "reload"
	serviceActionStart   = "start"
	serviceActionStop    = "stop"
)

var (
	_ action.ActionWithConfigure  = (*serviceAction)(nil)
	_ action.ActionWithModifyPlan = (*serviceAction)(nil)
	_ action.ActionWithConfigure  = (*systemdDaemonReloadAction)(nil)
	_ action.ActionWithModifyPlan = (*systemdDaemonReloadAction)(nil)
)

// hostAction is embedded by actions that change the running host. Like
// hostOnlyResource, it refuses to plan them when root_dir is set.
type hostAction struct {
	fsRoot *fsRoot
	cfg    *serviceConfig
}

func (h *hostAction) Configure(_ context.Context, req action.ConfigureRequest, resp *action.ConfigureResponse) {
	data, diags := providerDataFrom(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	if data != nil {
		h.fsRoot = data.root
		h.cfg = data.service
	}
}

// checkHost adds an error to diags if root_dir is set.
func (h *hostAction) checkHost(diags *diag.Diagnostics) {
	if h.fsRoot.isHost() {
		return
	}
	diags.AddError("Not supported with root_dir",
		fmt.Sprintf("This action changes the running host, not files below root_dir, so it cannot be used with root_dir = %q. "+
			"Invoke it through a separate provider configuration without root_dir, for example through a provider alias.", h.fsRoot.String()))
}

// actionTimeout returns the configured timeout, or defaultServiceTimeout.
func actionTimeout(v types.String, diags *diag.Diagnostics) time.Duration {
	t := v.ValueString()
	if v.IsNull() || v.IsUnknown() || t == "" {
		t = defaultServiceTimeout
	}
	if err := validateDuration(t); err != nil {
		diags.AddAttributeError(path.Root("timeout"), "Invalid timeout", capitalize(err.Error())+".")
		return 0
	}
	return mustParseDuration(t)
}

func actionTimeoutAttribute() schema.StringAttribute {
	return schema.StringAttribute{
		Optional: true,
		MarkdownDescription: "Maximum time each `systemctl` or `rc-service` invocation may take, as a Go duration such as `\"30s\"` or `\"5m\"`. " +
			"Starting, stopping and restarting wait for the service, so this should exceed its own start and stop timeouts. Defaults to `\"2m\"`.",
		Validators: []validator.String{positiveDuration()},
	}
}

func NewServiceAction() action.Action { return &serviceAction{} }

type serviceAction struct {
	hostAction
}

type serviceActionModel struct {
	Name    types.String `tfsdk:"name"`
	Action  types.String `tfsdk:"action"`
	Timeout types.String `tfsdk:"timeout"`
}

func (a *serviceAction) Metadata(_ context.Context, req action.MetadataRequest, resp *action.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_service"
}

func (a *serviceAction) Schema(_ context.Context, _ action.SchemaRequest, resp *action.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Restarts, reloads, starts or stops a service when invoked, typically from an `action_trigger` after its configuration file changed. " +
			"Uses `systemctl` when systemd booted the host and `rc-service` when OpenRC did, like the `sysutils_service` resource. " +
			"The service must exist when the action runs. Nothing is stored in the state. Requires root privileges and Terraform 1.14 or later.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Name of the service. With systemd, a unit name such as `\"nginx.service\"` or `\"nginx\"` (short for `nginx.service`); aliases are resolved to the unit they name, and template units such as `\"getty@.service\"` can only be named by instance. " +
					"With OpenRC, the name of a script in `/etc/init.d`, such as `\"nginx\"`.",
				Validators: []validator.String{stringCheck("service name", validateServiceName)},
			},
			"action": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "What to do: `\"restart\"` restarts the service, starting it if it is stopped; `\"reload\"` asks the running service to reload its configuration without restarting, and fails if it is stopped or cannot reload; " +
					"`\"start\"` starts it unless it is running; `\"stop\"` stops it unless it is stopped. " +
					"With systemd, `restart` runs `systemctl daemon-reload` first, so that changed unit files and drop-ins take effect, and `start` and `reload` do so when systemd reports that the unit's file changed.",
				Validators: []validator.String{stringvalidator.OneOf(serviceActionRestart, serviceActionReload, serviceActionStart, serviceActionStop)},
			},
			"timeout": actionTimeoutAttribute(),
		},
	}
}

// ModifyPlan refuses to plan with root_dir and fails the plan if no
// supported init system runs the host or the name is invalid for it. The
// service itself need not exist yet: the same apply may install it.
func (a *serviceAction) ModifyPlan(ctx context.Context, req action.ModifyPlanRequest, resp *action.ModifyPlanResponse) {
	a.checkHost(&resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	var config serviceActionModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	kind, err := a.cfg.detect()
	if err != nil {
		resp.Diagnostics.AddError("No supported init system", capitalize(err.Error())+".")
		return
	}
	if !config.Name.IsUnknown() {
		if err := validateServiceNameFor(kind, config.Name.ValueString()); err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("name"), "Invalid service name", capitalize(err.Error())+".")
		}
	}
}

func (a *serviceAction) Invoke(ctx context.Context, req action.InvokeRequest, resp *action.InvokeResponse) {
	a.checkHost(&resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	var config serviceActionModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	timeout := actionTimeout(config.Timeout, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	name, verb := config.Name.ValueString(), config.Action.ValueString()

	kind, err := a.cfg.detect()
	if err != nil {
		resp.Diagnostics.AddError("No supported init system", capitalize(err.Error())+".")
		return
	}
	// The init system may differ from the one seen during planning, and
	// its stricter name rules must hold before the name reaches it.
	if err := validateServiceNameFor(kind, name); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("name"), "Invalid service name", capitalize(err.Error())+".")
		return
	}
	mgr, err := a.cfg.manager(kind, timeout, defaultOpenRCRunlevel)
	if err != nil {
		resp.Diagnostics.AddError("No supported init system", capitalize(err.Error())+".")
		return
	}
	resp.Diagnostics.Append(invokeServiceAction(ctx, mgr, name, verb, resp.SendProgress)...)
}

// invokeServiceAction performs verb on the service name and verifies the
// result by reading its status back.
func invokeServiceAction(ctx context.Context, mgr serviceManager, name, verb string, progress func(action.InvokeProgressEvent)) diag.Diagnostics {
	var diags diag.Diagnostics
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
	report := func(msg string) {
		if progress != nil {
			progress(action.InvokeProgressEvent{Message: msg})
		}
	}

	switch verb {
	case serviceActionRestart:
		report(fmt.Sprintf("Restarting %s (%s)", unit, mgr.Kind()))
		err = mgr.Restart(ctx, unit)
	case serviceActionReload:
		if !st.Running {
			diags.AddAttributeError(path.Root("action"), "Service not running",
				fmt.Sprintf("%s cannot be reloaded because it is not running (%s). Use action = %q to start it if it is stopped.", unit, st.Detail, serviceActionRestart))
			return diags
		}
		report(fmt.Sprintf("Reloading %s (%s)", unit, mgr.Kind()))
		err = mgr.Reload(ctx, unit)
	case serviceActionStart:
		if st.Running {
			report(fmt.Sprintf("%s is already running", unit))
			return diags
		}
		report(fmt.Sprintf("Starting %s (%s)", unit, mgr.Kind()))
		err = mgr.SetRunning(ctx, unit, true)
	case serviceActionStop:
		if !st.Running {
			report(fmt.Sprintf("%s is already stopped", unit))
			return diags
		}
		report(fmt.Sprintf("Stopping %s (%s)", unit, mgr.Kind()))
		err = mgr.SetRunning(ctx, unit, false)
	default:
		diags.AddAttributeError(path.Root("action"), "Invalid action",
			fmt.Sprintf("action must be %q, %q, %q or %q, got %q.", serviceActionRestart, serviceActionReload, serviceActionStart, serviceActionStop, verb))
		return diags
	}
	if err != nil {
		diags.AddError(fmt.Sprintf("Service %s failed", verb), err.Error())
		return diags
	}
	// A reload leaves the run state alone; the others must reach theirs.
	if verb == serviceActionReload {
		return diags
	}
	diags.Append(checkRunning(ctx, mgr, name, unit, verb != serviceActionStop, verb)...)
	if !diags.HasError() {
		report(fmt.Sprintf("%s: %s done", unit, verb))
	}
	return diags
}

func NewSystemdDaemonReloadAction() action.Action { return &systemdDaemonReloadAction{} }

type systemdDaemonReloadAction struct {
	hostAction
}

type systemdDaemonReloadModel struct {
	Timeout types.String `tfsdk:"timeout"`
}

func (a *systemdDaemonReloadAction) Metadata(_ context.Context, req action.MetadataRequest, resp *action.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_systemd_daemon_reload"
}

func (a *systemdDaemonReloadAction) Schema(_ context.Context, _ action.SchemaRequest, resp *action.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Runs `systemctl daemon-reload` when invoked, so that systemd rereads its unit files, drop-ins and generators. " +
			"Invoke it from an `action_trigger` after writing a unit file or drop-in with a resource that does not reload systemd itself, such as `sysutils_file`. " +
			"Fails unless systemd booted the host. Nothing is stored in the state. Requires root privileges and Terraform 1.14 or later.",
		Attributes: map[string]schema.Attribute{
			"timeout": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Maximum time `systemctl daemon-reload` may take, as a Go duration such as `\"30s\"` or `\"5m\"`. Defaults to `\"2m\"`.",
				Validators:          []validator.String{positiveDuration()},
			},
		},
	}
}

// ModifyPlan refuses to plan with root_dir or without systemd.
func (a *systemdDaemonReloadAction) ModifyPlan(_ context.Context, _ action.ModifyPlanRequest, resp *action.ModifyPlanResponse) {
	a.checkHost(&resp.Diagnostics)
	if !resp.Diagnostics.HasError() {
		a.requireSystemd(&resp.Diagnostics)
	}
}

func (a *systemdDaemonReloadAction) requireSystemd(diags *diag.Diagnostics) {
	kind, err := a.cfg.detect()
	switch {
	case err != nil:
		diags.AddError("systemd is not running", capitalize(err.Error())+".")
	case kind != initSystemSystemd:
		diags.AddError("systemd is not running",
			fmt.Sprintf("sysutils_systemd_daemon_reload needs systemd, but %s manages services on this host. "+
				"To share a configuration between hosts with different init systems, set condition in the action_trigger block, for example to data.sysutils_host.this.init_system == \"systemd\".", kind))
	}
}

func (a *systemdDaemonReloadAction) Invoke(ctx context.Context, req action.InvokeRequest, resp *action.InvokeResponse) {
	a.checkHost(&resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	var config systemdDaemonReloadModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	timeout := actionTimeout(config.Timeout, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	a.requireSystemd(&resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if resp.SendProgress != nil {
		resp.SendProgress(action.InvokeProgressEvent{Message: "Reloading the systemd manager configuration"})
	}
	sc := systemctl{run: a.cfg.runner(), timeout: timeout}
	if err := sc.do(ctx, "daemon-reload"); err != nil {
		resp.Diagnostics.AddError("systemd daemon-reload failed", err.Error())
	}
}
