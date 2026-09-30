package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

type sysutilsProvider struct {
	version string
	// systemd overrides how sysutils_systemd_unit reaches systemd. It is nil
	// in production and set by unit tests to a fake systemctl.
	systemd *systemdConfig
	// mount overrides the fstab path and mounter of sysutils_mount. It is nil
	// in production and set by tests to a temporary fstab or a fake mounter.
	mount *mountConfig
	// sysctl overrides /proc/sys for sysutils_sysctl. It is nil in
	// production and set by unit tests to a temporary directory.
	sysctl *sysctlConfig
	// kernelModule overrides the configuration directories and module
	// loader of sysutils_kernel_module. It is nil in production and set by
	// unit tests to temporary directories and a fake loader.
	kernelModule *kernelModuleConfig
	// cron overrides the cron.d directory and file owner of
	// sysutils_cron_job. It is nil in production and set by tests to a
	// temporary directory.
	cron *cronConfig
	// sudoers overrides the sudoers.d directory, file owner and visudo of
	// sysutils_sudoers. It is nil in production and set by tests to a
	// temporary directory or a fake visudo.
	sudoers *sudoersConfig
	// pkg overrides how sysutils_package reaches the package manager. It is
	// nil in production and set by unit tests to a fake package manager or
	// command runner.
	pkg *packageConfig
	// repo overrides the file owner and HTTP client of
	// sysutils_package_repository. It is nil in production and set by tests
	// to their own user and a client that trusts their TLS server.
	repo *repoConfig
	// service overrides how sysutils_service detects and reaches the init
	// system. It is nil in production and set by unit tests to a fake
	// command runner and a temporary /run.
	service *serviceConfig
	// sshKey overrides how sysutils_ssh_authorized_key looks up users. It
	// is nil in production and set by unit tests to their own account with a
	// temporary home directory.
	sshKey *sshKeyConfig
	// timezone overrides how sysutils_timezone detects and runs
	// timedatectl. It is nil in production and set by unit tests to a fake
	// command runner and a temporary /run.
	timezone *timezoneConfig
	// hostname overrides how sysutils_hostname detects and runs
	// hostnamectl and reads and sets the kernel hostname. It is nil in
	// production and set by unit tests to fakes.
	hostname *hostnameConfig
	// locale overrides how sysutils_locale lists and generates locales. It
	// is nil in production and set by unit tests to a fake command runner
	// and temporary directories.
	locale *localeConfig
	// firewall overrides how sysutils_firewall_rule runs nft and iptables.
	// It is nil in production and set by tests to a fake command runner, or
	// to one that runs the commands inside a network namespace.
	firewall *firewallConfig
	// swap overrides the fstab path and swap tools of sysutils_swap. It is
	// nil in production and set by tests to a temporary fstab or a fake
	// swap manager.
	swap *swapConfig
	// alternatives overrides how sysutils_alternatives finds and runs
	// update-alternatives or alternatives. It is nil in production and set
	// by unit tests to a fake command runner.
	alternatives *alternativesConfig
	// host overrides /proc, /etc/hosts, uname and the DNS lookup of the
	// sysutils_host data source. It is nil in production and set by unit
	// tests to fixtures.
	host *hostConfig
}

// providerData is passed to resources that implement
// resource.ResourceWithConfigure.
type providerData struct {
	systemd      *systemdConfig
	mount        *mountConfig
	sysctl       *sysctlConfig
	kernelModule *kernelModuleConfig
	cron         *cronConfig
	sudoers      *sudoersConfig
	pkg          *packageConfig
	repo         *repoConfig
	service      *serviceConfig
	sshKey       *sshKeyConfig
	timezone     *timezoneConfig
	hostname     *hostnameConfig
	locale       *localeConfig
	firewall     *firewallConfig
	swap         *swapConfig
	alternatives *alternativesConfig
	host         *hostConfig
	// root is the directory that the paths of the file, file line, template
	// file, directory, symlink, archive extract, cron job, sudoers, package repository, timezone, hostname, locale and swap resources and the file and
	// directory data sources are confined to, and that the host data source
	// reads os-release and looks for package managers in; see rootfs.go.
	root *fsRoot
}

type providerModel struct {
	RootDir types.String `tfsdk:"root_dir"`
}

func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &sysutilsProvider{version: version}
	}
}

func (p *sysutilsProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "sysutils"
	resp.Version = p.version
}

func (p *sysutilsProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The `sysutils` provider exposes a small set of primitives for host-level administration from Terraform: files, `/etc/hosts` entries, directories, symlinks, archives, local users and groups, systemd units and services, mounts, swap, kernel parameters and modules, the hostname, time zone and locale, cron jobs, sudo rules, SSH authorized keys, firewall rules, OS packages and package repositories, alternatives links, and command execution, and reports host facts such as the distribution for use in conditionals. " +
			"All arguments are optional.",
		Attributes: map[string]schema.Attribute{
			"root_dir": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Directory that every managed path is relative to, as if the provider ran in a chroot there. " +
					"With `root_dir = \"/srv/rootfs\"`, a `sysutils_file` with `path = \"/etc/hosts\"` writes `/srv/rootfs/etc/hosts`. " +
					"Use it to build a container or OS image root filesystem tree. " +
					"Applies to the `sysutils_file`, `sysutils_file_line`, `sysutils_ini_value`, `sysutils_hosts_entry`, `sysutils_template_file`, `sysutils_directory`, `sysutils_symlink`, `sysutils_archive_extract` (its `destination`), `sysutils_cron_job`, `sysutils_sudoers`, `sysutils_package_repository`, `sysutils_timezone`, `sysutils_hostname` (which then only writes files and leaves the kernel hostname alone), `sysutils_locale` and `sysutils_swap` resources and the `sysutils_file` and `sysutils_directory` data sources. " +
					"The `sysutils_host` data source reads `os-release` and looks for the package manager below it, but reports the running host's kernel, names, hardware, init system and firewall. " +
					"`sysutils_mount`, `sysutils_sysctl`, `sysutils_kernel_module`, `sysutils_service`, `sysutils_package`, `sysutils_ssh_authorized_key`, `sysutils_firewall_rule` and `sysutils_alternatives` change the running host and refuse to plan when `root_dir` is set, as does `sysutils_package_repository` with `refresh_cache = true`, `sysutils_locale` with `generate = true` and `sysutils_swap` with `enabled = true` or a block device. " +
					"`path` attributes, ids and import ids keep the path inside the root. " +
					"Symlinks inside the root are resolved as they would be in a chroot: absolute link targets are relative to `root_dir`, and a link that leads above `root_dir` is an error, so no symlink in the tree can make the provider act outside it. " +
					"Must be an absolute path in canonical form; symlinks in `root_dir` itself are followed. It must exist when a resource or data source is read or applied. " +
					"Must be known at plan time. Defaults to `\"/\"`, the host's root directory.",
				Validators: []validator.String{absolutePathOrRoot()},
			},
		},
	}
}

func (p *sysutilsProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	root := hostRoot
	switch {
	case cfg.RootDir.IsUnknown():
		// Operating on the host root until the value is known would be
		// wrong, and silently so; refuse instead.
		resp.Diagnostics.AddAttributeError(path.Root("root_dir"), "Unknown root_dir",
			"The value of root_dir must be known when planning. Do not derive it from attributes of resources that have not been created yet.")
		return
	case !cfg.RootDir.IsNull():
		var err error
		if root, err = newFSRoot(cfg.RootDir.ValueString()); err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("root_dir"), "Invalid root_dir", capitalize(err.Error())+".")
			return
		}
	}
	data := &providerData{systemd: p.systemd, mount: p.mount, sysctl: p.sysctl, kernelModule: p.kernelModule, cron: p.cron, sudoers: p.sudoers, pkg: p.pkg, repo: p.repo, service: p.service, sshKey: p.sshKey, timezone: p.timezone, hostname: p.hostname, locale: p.locale, firewall: p.firewall, swap: p.swap, alternatives: p.alternatives, host: p.host, root: root}
	resp.ResourceData = data
	resp.DataSourceData = data
}

// providerDataFrom converts the value passed to Configure of a resource or
// data source. It returns nil before the provider is configured, for
// example during validation.
func providerDataFrom(v any) (*providerData, diag.Diagnostics) {
	var diags diag.Diagnostics
	if v == nil {
		return nil, diags
	}
	data, ok := v.(*providerData)
	if !ok {
		diags.AddError("Unexpected provider data", fmt.Sprintf("Expected *providerData, got %T.", v))
		return nil, diags
	}
	return data, diags
}

// rootedResource is embedded by resources whose paths are confined to
// root_dir. It implements resource.ResourceWithConfigure.
type rootedResource struct {
	fsRoot *fsRoot
}

func (r *rootedResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	data, diags := providerDataFrom(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	if data != nil {
		r.fsRoot = data.root
	}
}

// root returns the configured root, or the host root before configuration.
func (r *rootedResource) root() *fsRoot {
	if r.fsRoot == nil {
		return hostRoot
	}
	return r.fsRoot
}

// hostOnlyResource is embedded by resources that act on the running host
// itself (its kernel or mount table) rather than on files in a tree, and so
// cannot honour root_dir. Its ModifyPlan refuses to plan their creation or
// update when root_dir is set: silently changing the host of a provider
// configured to build an image tree would defeat the purpose of root_dir.
// Destroy is still allowed, so that resources created before root_dir was
// set can be removed.
type hostOnlyResource struct {
	fsRoot *fsRoot
}

func (h *hostOnlyResource) ModifyPlan(_ context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() || h.fsRoot.isHost() {
		return
	}
	resp.Diagnostics.AddError("Not supported with root_dir",
		fmt.Sprintf("This resource changes the running host, not files below root_dir, so it cannot be used with root_dir = %q. "+
			"Manage it with a separate provider configuration without root_dir, for example through a provider alias.", h.fsRoot.String()))
}

// rootedDataSource is the data source counterpart of rootedResource.
type rootedDataSource struct {
	fsRoot *fsRoot
}

func (d *rootedDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	data, diags := providerDataFrom(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	if data != nil {
		d.fsRoot = data.root
	}
}

func (d *rootedDataSource) root() *fsRoot {
	if d.fsRoot == nil {
		return hostRoot
	}
	return d.fsRoot
}

// hostOnlyDataSource is the data source counterpart of hostOnlyResource:
// its check refuses to read when root_dir is set, since the data source
// would describe the running host rather than the tree below root_dir.
type hostOnlyDataSource struct {
	fsRoot *fsRoot
}

// checkHostOnly adds an error to diags if root_dir is set.
func (d *hostOnlyDataSource) checkHostOnly(diags *diag.Diagnostics) {
	if d.fsRoot.isHost() {
		return
	}
	diags.AddError("Not supported with root_dir",
		fmt.Sprintf("This data source reads the running host, not files below root_dir, so it cannot be used with root_dir = %q. "+
			"Read it through a separate provider configuration without root_dir, for example through a provider alias.", d.fsRoot.String()))
}

func (p *sysutilsProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewFileResource,
		NewRemoteFileResource,
		NewFileLineResource,
		NewIniValueResource,
		NewTemplateFileResource,
		NewDirectoryResource,
		NewSymlinkResource,
		NewArchiveExtractResource,
		NewUserResource,
		NewGroupResource,
		NewExecResource,
		NewSystemdUnitResource,
		NewServiceResource,
		NewMountResource,
		NewSwapResource,
		NewSysctlResource,
		NewKernelModuleResource,
		NewCronJobResource,
		NewSudoersResource,
		NewSSHAuthorizedKeyResource,
		NewHostsEntryResource,
		NewTimezoneResource,
		NewHostnameResource,
		NewLocaleResource,
		NewFirewallRuleResource,
		NewPackageResource,
		NewPackageRepositoryResource,
		NewAlternativesResource,
		NewFileACLResource,
		NewLimitsResource,
	}
}

func (p *sysutilsProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewDirectoryDataSource,
		NewFileDataSource,
		NewUserDataSource,
		NewGroupDataSource,
		NewHostDataSource,
		NewPackageDataSource,
		NewServiceDataSource,
	}
}
