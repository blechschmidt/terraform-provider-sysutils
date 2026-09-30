package provider

import (
	"context"
	"errors"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = (*sysctlDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*sysctlDataSource)(nil)
)

func NewSysctlDataSource() datasource.DataSource { return &sysctlDataSource{} }

type sysctlDataSource struct {
	rootedDataSource
	cfg *sysctlConfig
}

type sysctlDataSourceModel struct {
	Name            types.String `tfsdk:"name"`
	Prefix          types.String `tfsdk:"prefix"`
	Exists          types.Bool   `tfsdk:"exists"`
	Value           types.String `tfsdk:"value"`
	PersistedValue  types.String `tfsdk:"persisted_value"`
	PersistedFile   types.String `tfsdk:"persisted_file"`
	Values          types.Map    `tfsdk:"values"`
	PersistedValues types.Map    `tfsdk:"persisted_values"`
	ID              types.String `tfsdk:"id"`
}

func (d *sysctlDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_sysctl"
}

// rootDirLiveNote is appended to the description of the attributes that
// describe the running kernel.
const rootDirLiveNote = " Null when the provider's `root_dir` is set: the running kernel is not part of the tree below it."

func (d *sysctlDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads kernel parameters from `/proc/sys`, either one parameter (`name`) or all parameters below a prefix (`prefix`), together with the values they are set to at boot. " +
			"The boot values come from the files `systemd-sysctl` and `sysctl --system` apply: the `*.conf` files in `/etc/sysctl.d`, `/run/sysctl.d`, `/usr/local/lib/sysctl.d`, `/usr/lib/sysctl.d` and `/lib/sysctl.d`, " +
			"sorted by file name, where a file in an earlier directory hides one of the same name in a later one, and then `/etc/sysctl.conf`; the last assignment of a key wins, and a file that is a symlink to `/dev/null` sets nothing. " +
			"This includes the files `sysutils_sysctl` writes. Glob patterns in keys, such as `net.ipv4.conf.*.rp_filter`, are not expanded. " +
			"Nothing on the host changes and no root privileges are needed. " +
			"With the provider's `root_dir` set, the boot values are read from the files below it, and the attributes describing the running kernel are null.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Key of one kernel parameter in the dotted form of `sysctl(8)`, such as `\"net.ipv4.ip_forward\"`, as in `sysutils_sysctl`. " +
					"A `.` inside a component, as in the interface name `eth0.100`, is written `/`: `\"net.ipv4.conf.eth0/100.forwarding\"`. " +
					"Exactly one of `name` and `prefix` must be set.",
				Validators: []validator.String{
					stringCheck("name", validateSysctlName),
					stringvalidator.ExactlyOneOf(path.MatchRoot("name"), path.MatchRoot("prefix")),
				},
			},
			"prefix": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Key of a group of kernel parameters, such as `\"vm\"` or `\"net.ipv4.conf.all\"`, to read every parameter below it, as `sysctl -a` does. " +
					"Matches whole components only: `\"net.ipv4.conf.all\"` does not include `net.ipv4.conf.all_squash`. A key of a single parameter selects just that parameter.",
				Validators: []validator.String{stringCheck("prefix", validateSysctlPrefix)},
			},
			"exists": schema.BoolAttribute{
				Computed: true,
				MarkdownDescription: "With `name`: whether the running kernel has the parameter. It does not when the module providing it is not loaded or the network interface it belongs to does not exist. " +
					"Null with `prefix`." + rootDirLiveNote,
			},
			"value": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "With `name`: the parameter's current value, with the trailing newline removed and runs of white space, such as the tabs between the fields of `net.ipv4.ip_local_port_range`, collapsed to one space. " +
					"Null if the parameter does not exist, and with `prefix`." + rootDirLiveNote,
			},
			"persisted_value": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "With `name`: the value the parameter is set to at boot, with white space normalized like `value`. Null if no configuration file sets it, and with `prefix`.",
			},
			"persisted_file": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "With `name`: the configuration file whose assignment of the parameter takes effect at boot, such as `\"/etc/sysctl.d/99-terraform.conf\"`. Null if `persisted_value` is.",
			},
			"values": schema.MapAttribute{
				ElementType: types.StringType,
				Computed:    true,
				MarkdownDescription: "With `prefix`: the current value of every parameter below it, by key, normalized like `value`. " +
					"Parameters that cannot be read, such as write-only ones, are left out, and so are file systems mounted inside `/proc/sys`, such as `binfmt_misc`. " +
					"Empty if the prefix does not exist; null with `name`." + rootDirLiveNote,
			},
			"persisted_values": schema.MapAttribute{
				ElementType: types.StringType,
				Computed:    true,
				MarkdownDescription: "With `prefix`: the value every key below it is set to at boot, by key as written in the configuration files, normalized like `value`. " +
					"Includes keys the running kernel does not have. Null with `name`.",
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Data source identifier (equal to `name` or `prefix`).",
			},
		},
	}
}

func (d *sysctlDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	data, diags := providerDataFrom(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	if data != nil {
		d.cfg = data.sysctl
		d.fsRoot = data.root
	}
}

func (d *sysctlDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var m sysctlDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	root := d.root()
	live := root.isHost()
	m.Exists, m.Value, m.PersistedValue, m.PersistedFile = types.BoolNull(), types.StringNull(), types.StringNull(), types.StringNull()
	m.Values, m.PersistedValues = types.MapNull(types.StringType), types.MapNull(types.StringType)

	persisted, err := readPersistedSysctl(root)
	if err != nil {
		resp.Diagnostics.AddError("Reading sysctl configuration", capitalize(err.Error())+".")
		return
	}

	if !m.Name.IsNull() {
		name := m.Name.ValueString()
		m.ID = types.StringValue(name)
		if live {
			v, err := readSysctl(d.cfg.root(), name)
			switch {
			case errors.Is(err, errSysctlNotFound):
				m.Exists = types.BoolValue(false)
			case err != nil:
				resp.Diagnostics.AddAttributeError(path.Root("name"), "Reading kernel parameter", capitalize(err.Error())+".")
				return
			default:
				m.Exists, m.Value = types.BoolValue(true), types.StringValue(v)
			}
		}
		if p, ok := persisted[name]; ok {
			m.PersistedValue, m.PersistedFile = types.StringValue(p.value), types.StringValue(p.file)
		}
		resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
		return
	}

	prefix := m.Prefix.ValueString()
	m.ID = types.StringValue(prefix)
	if live {
		values, err := listSysctl(d.cfg.root(), prefix)
		if errors.Is(err, errSysctlNotFound) {
			values, err = map[string]string{}, nil
		}
		if err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("prefix"), "Reading kernel parameters", capitalize(err.Error())+".")
			return
		}
		mv, diags := types.MapValueFrom(ctx, types.StringType, values)
		resp.Diagnostics.Append(diags...)
		m.Values = mv
	}
	below := map[string]string{}
	for key, p := range persisted {
		if key == prefix || strings.HasPrefix(key, prefix+".") {
			below[key] = p.value
		}
	}
	pv, diags := types.MapValueFrom(ctx, types.StringType, below)
	resp.Diagnostics.Append(diags...)
	m.PersistedValues = pv
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}
