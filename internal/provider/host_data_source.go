package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = (*hostDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*hostDataSource)(nil)
)

func NewHostDataSource() datasource.DataSource { return &hostDataSource{} }

type hostDataSource struct {
	data *providerData
}

type hostDataSourceModel struct {
	ID                types.String `tfsdk:"id"`
	RootDir           types.String `tfsdk:"root_dir"`
	Hostname          types.String `tfsdk:"hostname"`
	FQDN              types.String `tfsdk:"fqdn"`
	OSID              types.String `tfsdk:"os_id"`
	OSIDLike          types.List   `tfsdk:"os_id_like"`
	OSVersionID       types.String `tfsdk:"os_version_id"`
	OSVersionCodename types.String `tfsdk:"os_version_codename"`
	OSPrettyName      types.String `tfsdk:"os_pretty_name"`
	OSRelease         types.Map    `tfsdk:"os_release"`
	KernelRelease     types.String `tfsdk:"kernel_release"`
	Architecture      types.String `tfsdk:"architecture"`
	CPUCount          types.Int64  `tfsdk:"cpu_count"`
	MemoryTotalBytes  types.Int64  `tfsdk:"memory_total_bytes"`
	InitSystem        types.String `tfsdk:"init_system"`
	PackageManager    types.String `tfsdk:"package_manager"`
	FirewallBackend   types.String `tfsdk:"firewall_backend"`
	LiveFacts         types.List   `tfsdk:"live_facts"`
}

func (d *hostDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_host"
}

// liveNote is appended to the description of every attribute listed in
// liveHostFacts.
const liveNote = " **Live:** always describes the running host, also when the provider's `root_dir` is set."

func (d *hostDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	str := func(desc string) schema.StringAttribute {
		return schema.StringAttribute{Computed: true, MarkdownDescription: desc}
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reports facts about the host: its names, its distribution from `os-release`, the kernel release and architecture, CPU count and memory, and the init system, package manager and firewall backend that the provider's resources would use. " +
			"Use it in conditionals, for example to choose package names by distribution. It takes no arguments.",
		Attributes: map[string]schema.Attribute{
			"id": str("Data source identifier: the root directory the file-based facts were read from, `\"/\"` unless the provider's `root_dir` is set."),
			"root_dir": str("The provider's `root_dir`, or `\"/\"` if it is not set. " +
				"The `os_*` attributes and `package_manager` describe the tree below it; the attributes listed in `live_facts` describe the running host."),
			"hostname": str("Host name of the running system, as `uname -n` reports it." + liveNote),
			"fqdn": str("Fully qualified domain name, found as `hostname -f` finds it: `hostname` if it contains a dot; " +
				"otherwise the canonical (first) name of the first `/etc/hosts` line that lists `hostname`, if that contains a dot; " +
				"otherwise the name DNS returns for `hostname` (with the resolver's search domains), if the lookup succeeds within 3 seconds and the name contains a dot; " +
				"otherwise `hostname`." + liveNote),
			"os_id": str("`ID` from `os-release`: a lower-case identifier of the distribution, such as `debian`, `ubuntu`, `alpine` or `fedora`. " +
				"`linux` if the file has no `ID`, as `os-release(5)` specifies. " +
				"`os-release` is `/etc/os-release`, or `/usr/lib/os-release` if that does not exist, below the provider's `root_dir`. " +
				"Null, like the other `os_*` attributes, if neither exists; the data source then also returns a warning."),
			"os_id_like": schema.ListAttribute{
				Computed:    true,
				ElementType: types.StringType,
				MarkdownDescription: "`ID_LIKE` from `os-release` as a list: the IDs of the distributions this one derives from, closest first, " +
					"such as `[\"debian\"]` on Ubuntu or `[\"rhel\", \"centos\", \"fedora\"]` on Rocky Linux. Empty if not set. " +
					"Check `contains(concat([os_id], os_id_like), \"debian\")` to match a distribution and its derivatives.",
			},
			"os_version_id": str("`VERSION_ID` from `os-release`, such as `12`, `24.04`, `3.20.3` or `40`. Null if not set, as on rolling releases and Debian testing."),
			"os_version_codename": str("`VERSION_CODENAME` from `os-release`, such as `bookworm` or `noble`, or `UBUNTU_CODENAME` if only that is set. " +
				"Null if neither is set, as on Alpine and Fedora."),
			"os_pretty_name": str("`PRETTY_NAME` from `os-release`, such as `Debian GNU/Linux 12 (bookworm)`. `Linux` if the file has no `PRETTY_NAME`."),
			"os_release": schema.MapAttribute{
				Computed:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Every field of `os-release`, by name, with quotes and escapes removed, for fields without an attribute of their own, " +
					"such as `NAME`, `VERSION`, `VARIANT_ID` or `SUPPORT_END`. Unlike `os_id` and `os_pretty_name`, it has no defaults.",
			},
			"kernel_release": str("Release of the running kernel, as `uname -r` reports it, such as `6.8.0-45-generic`." + liveNote),
			"architecture":   str("Machine hardware name of the running kernel, as `uname -m` reports it, such as `x86_64` or `aarch64`." + liveNote),
			"cpu_count": schema.Int64Attribute{
				Computed: true,
				MarkdownDescription: "Number of CPUs the provider can run on, as `nproc` reports it. " +
					"CPU affinity (`taskset`, cpusets) lowers it; CPU quotas of containers do not." + liveNote,
			},
			"memory_total_bytes": schema.Int64Attribute{
				Computed: true,
				MarkdownDescription: "Total usable RAM in bytes, `MemTotal` of `/proc/meminfo`. " +
					"In a container this is usually the host's memory, not the container's limit." + liveNote,
			},
			"init_system": str("Init system that booted the host: `systemd` if `/run/systemd/system` exists, otherwise `openrc` if `/run/openrc/softlevel` exists, otherwise `sysvinit` if PID 1 is called `init` (SysV or BusyBox init). " +
				"Null if none of these is the case, as in most containers. " +
				"The detection is the same as that of `sysutils_service`, which manages services with `systemd` and `openrc`." + liveNote),
			"package_manager": str("Package manager that `sysutils_package` with `manager = \"auto\"` would use: the first of `apt`, `dnf`, `yum` and `apk` whose tools are installed. " +
				"On the host the provider's `PATH` is searched; below `root_dir`, `/usr/local/sbin`, `/usr/local/bin`, `/usr/sbin`, `/usr/bin`, `/sbin` and `/bin` of the tree are. " +
				"Null if none is found."),
			"firewall_backend": str("Firewall backend that `sysutils_firewall_rule` with `backend = \"auto\"` would use: `nftables` if the `nft` command works, otherwise `iptables` if `iptables` or `ip6tables` is installed. " +
				"Null if neither is available. `nft` needs root to list the ruleset, so run as another user this reports `iptables` or null." + liveNote),
			"live_facts": schema.ListAttribute{
				Computed:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Names of the attributes that come from the running kernel and system rather than from files below `root_dir`: " +
					"`hostname`, `fqdn`, `kernel_release`, `architecture`, `cpu_count`, `memory_total_bytes`, `init_system` and `firewall_backend`. " +
					"When `root_dir` is set, these describe the machine building the tree, not the tree.",
			},
		},
	}
}

func (d *hostDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	data, diags := providerDataFrom(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	d.data = data
}

func (d *hostDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	src := hostFactSources{root: hostRoot}
	if d.data != nil {
		src = hostFactSources{root: d.data.root, host: d.data.host, service: d.data.service, pkg: d.data.pkg, firewall: d.data.firewall}
		if src.root == nil {
			src.root = hostRoot
		}
	}
	facts, warnings, err := collectHostFacts(ctx, src)
	if err != nil {
		resp.Diagnostics.AddError("Cannot read host facts", capitalize(err.Error())+".")
		return
	}
	for _, w := range warnings {
		resp.Diagnostics.AddWarning("No os-release file", w)
	}

	state, diags := hostFactsModel(ctx, src.root, facts)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// hostFactsModel converts facts to the data source's state. Empty strings
// become null.
func hostFactsModel(ctx context.Context, root *fsRoot, f hostFacts) (hostDataSourceModel, diag.Diagnostics) {
	var diags diag.Diagnostics
	optional := func(s string) types.String {
		if s == "" {
			return types.StringNull()
		}
		return types.StringValue(s)
	}
	m := hostDataSourceModel{
		ID:                types.StringValue(root.String()),
		RootDir:           types.StringValue(root.String()),
		Hostname:          optional(f.hostname),
		FQDN:              optional(f.fqdn),
		OSID:              types.StringNull(),
		OSIDLike:          types.ListNull(types.StringType),
		OSVersionID:       types.StringNull(),
		OSVersionCodename: types.StringNull(),
		OSPrettyName:      types.StringNull(),
		OSRelease:         types.MapNull(types.StringType),
		KernelRelease:     optional(f.kernel.release),
		Architecture:      optional(f.kernel.machine),
		CPUCount:          types.Int64Value(int64(f.cpuCount)),
		MemoryTotalBytes:  types.Int64Value(f.memTotal),
		InitSystem:        optional(f.initSystem),
		PackageManager:    optional(f.packageManager),
		FirewallBackend:   optional(f.firewallBackend),
	}
	var d diag.Diagnostics
	m.LiveFacts, d = types.ListValueFrom(ctx, types.StringType, liveHostFacts)
	diags.Append(d...)
	if r := f.osRelease; r != nil {
		m.OSID = types.StringValue(r.id())
		m.OSVersionID = optional(r["VERSION_ID"])
		m.OSVersionCodename = optional(r.versionCodename())
		m.OSPrettyName = types.StringValue(r.prettyName())
		m.OSIDLike, d = types.ListValueFrom(ctx, types.StringType, r.idLike())
		diags.Append(d...)
		m.OSRelease, d = types.MapValueFrom(ctx, types.StringType, map[string]string(r))
		diags.Append(d...)
	}
	return m, diags
}
