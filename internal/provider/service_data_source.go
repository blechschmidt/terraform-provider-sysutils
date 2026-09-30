package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = (*serviceDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*serviceDataSource)(nil)
)

func NewServiceDataSource() datasource.DataSource { return &serviceDataSource{} }

type serviceDataSource struct {
	hostOnlyDataSource
	cfg *serviceConfig
}

type serviceDataSourceModel struct {
	Name         types.String `tfsdk:"name"`
	Runlevel     types.String `tfsdk:"runlevel"`
	InitSystem   types.String `tfsdk:"init_system"`
	Supported    types.Bool   `tfsdk:"supported"`
	Exists       types.Bool   `tfsdk:"exists"`
	Unit         types.String `tfsdk:"unit"`
	Enabled      types.Bool   `tfsdk:"enabled"`
	EnabledState types.String `tfsdk:"enabled_state"`
	Running      types.Bool   `tfsdk:"running"`
	ActiveState  types.String `tfsdk:"active_state"`
	ID           types.String `tfsdk:"id"`
}

func (d *serviceDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_service"
}

// supportedNote is appended to the description of every attribute that is
// null where the init system is not supported.
const supportedNote = " Null if `supported` is false."

func (d *serviceDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads whether a service exists, starts at boot and is running, with the same init system backends as `sysutils_service`: `systemctl` when systemd booted the host, `rc-service`/`rc-update` when OpenRC did, as on Alpine Linux. " +
			"Only status queries run, so nothing on the host changes and no root privileges are needed. " +
			"Where neither systemd nor OpenRC runs, as in most containers or with SysV init, reading still succeeds: `init_system` says what runs instead and `supported` is false.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Name of the service, as in `sysutils_service`. With systemd, a unit name such as `\"nginx.service\"`, `\"nginx\"` (short for `nginx.service`), `\"getty@tty2.service\"` or `\"backup.timer\"`; aliases are resolved to the unit they name. " +
					"With OpenRC, the name of a script in `/etc/init.d`, such as `\"nginx\"`.",
				Validators: []validator.String{stringCheck("service name", validateServiceName)},
			},
			"runlevel": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "OpenRC runlevel that `enabled` refers to. Defaults to `\"default\"`, the runlevel that `rc-update add` uses. Ignored with systemd.",
				Validators:          []validator.String{stringCheck("runlevel", validateRunlevel)},
			},
			"init_system": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Init system that booted the host, detected like `init_system` of the `sysutils_host` data source: `\"systemd\"` if `/run/systemd/system` exists, otherwise `\"openrc\"` if `/run/openrc/softlevel` exists, otherwise `\"sysvinit\"` if PID 1 is `init`. " +
					"Null if none of them runs, as in most containers.",
			},
			"supported": schema.BoolAttribute{
				Computed: true,
				MarkdownDescription: "Whether `init_system` is one that the data source and `sysutils_service` support: `\"systemd\"` or `\"openrc\"`. " +
					"If false, the attributes describing the service are null.",
			},
			"exists": schema.BoolAttribute{
				Computed: true,
				MarkdownDescription: "Whether the init system knows the service: with systemd, whether a unit of that name is loaded or has a unit file (a load state other than `not-found`); with OpenRC, whether `rc-service --exists` finds a script in `/etc/init.d`." +
					supportedNote,
			},
			"unit": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Name the init system knows the service by: with systemd the unit's primary name, with its type suffix added and aliases resolved, such as `\"ssh.service\"` for `name = \"sshd\"`; with OpenRC `name` itself. " +
					"Null if the service does not exist." + supportedNote,
			},
			"enabled": schema.BoolAttribute{
				Computed: true,
				MarkdownDescription: "Whether the service starts at boot, as `enabled` of `sysutils_service` reports it: with systemd, only the `is-enabled` state `enabled` counts; with OpenRC, whether the service is in `runlevel`. " +
					"False if the service does not exist." + supportedNote,
			},
			"enabled_state": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The init system's own word for the enablement: what `systemctl is-enabled` prints, such as `\"enabled\"`, `\"disabled\"`, `\"static\"`, `\"indirect\"`, `\"enabled-runtime\"` or `\"masked\"`; with OpenRC `\"enabled\"` or `\"disabled\"`. " +
					"Null if the service does not exist." + supportedNote,
			},
			"running": schema.BoolAttribute{
				Computed: true,
				MarkdownDescription: "Whether the service is running, as `state` of `sysutils_service` reports it: with systemd, when `systemctl is-active` reports `active`, `reloading` or `refreshing`; with OpenRC, when `rc-service <name> status` reports `started`. " +
					"False if the service does not exist." + supportedNote,
			},
			"active_state": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The init system's own word for the run state: what `systemctl is-active` prints, such as `\"active\"`, `\"inactive\"`, `\"activating\"` or `\"failed\"`; with OpenRC the status `rc-service <name> status` reports, such as `\"started\"`, `\"stopped\"` or `\"crashed\"`. " +
					"Null if the service does not exist." + supportedNote,
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Data source identifier (equal to `name`).",
			},
		},
	}
}

func (d *serviceDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	data, diags := providerDataFrom(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	if data != nil {
		d.cfg = data.service
		d.fsRoot = data.root
	}
}

func (d *serviceDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	d.checkHostOnly(&resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	var m serviceDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := m.Name.ValueString()
	m.ID = types.StringValue(name)
	m.Exists, m.Enabled, m.Running = types.BoolNull(), types.BoolNull(), types.BoolNull()
	m.Unit, m.EnabledState, m.ActiveState = types.StringNull(), types.StringNull(), types.StringNull()

	probed := d.cfg.probe().kind
	m.InitSystem = optionalString(true, probed)
	m.Supported = types.BoolValue(probed == initSystemSystemd || probed == initSystemOpenRC)
	if !m.Supported.ValueBool() {
		resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
		return
	}

	// detect checks that the init system's tools are installed, and
	// explains why not.
	kind, err := d.cfg.detect()
	if err != nil {
		resp.Diagnostics.AddError("Init system tools not available", capitalize(err.Error())+".")
		return
	}
	m.InitSystem = types.StringValue(kind)
	if err := validateServiceNameFor(kind, name); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("name"), "Invalid service name", capitalize(err.Error())+".")
		return
	}
	level := defaultOpenRCRunlevel
	if !m.Runlevel.IsNull() {
		level = m.Runlevel.ValueString()
	}
	mgr, err := d.cfg.manager(kind, mustParseDuration(defaultServiceTimeout), level)
	if err != nil {
		resp.Diagnostics.AddError("No supported init system", capitalize(err.Error())+".")
		return
	}
	st, err := mgr.Status(ctx, name)
	if err != nil {
		resp.Diagnostics.AddError("Querying service", err.Error())
		return
	}
	m.Exists = types.BoolValue(st.Found)
	m.Enabled = types.BoolValue(st.Found && st.Enabled)
	m.Running = types.BoolValue(st.Found && st.Running)
	if st.Found {
		m.Unit = types.StringValue(st.Unit)
		m.EnabledState = optionalString(true, st.EnabledState)
		m.ActiveState = optionalString(true, st.ActiveState)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}
