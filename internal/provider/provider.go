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
	// pkg overrides how sysutils_package reaches the package manager. It is
	// nil in production and set by unit tests to a fake package manager or
	// command runner.
	pkg *packageConfig
}

// providerData is passed to resources that implement
// resource.ResourceWithConfigure.
type providerData struct {
	systemd      *systemdConfig
	mount        *mountConfig
	sysctl       *sysctlConfig
	kernelModule *kernelModuleConfig
	cron         *cronConfig
	pkg          *packageConfig
	// root is the directory that the paths of the file, file line, template
	// file, directory, symlink and cron job resources and the file and
	// directory data sources are confined to; see rootfs.go.
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
		MarkdownDescription: "The `sysutils` provider exposes a small set of primitives for host-level administration from Terraform: files, directories, symlinks, local users and groups, systemd units, mounts, kernel parameters and modules, cron jobs, OS packages, and command execution. " +
			"All arguments are optional.",
		Attributes: map[string]schema.Attribute{
			"root_dir": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Directory that every managed path is relative to, as if the provider ran in a chroot there. " +
					"With `root_dir = \"/srv/rootfs\"`, a `sysutils_file` with `path = \"/etc/hosts\"` writes `/srv/rootfs/etc/hosts`. " +
					"Use it to build a container or OS image root filesystem tree. " +
					"Applies to the `sysutils_file`, `sysutils_file_line`, `sysutils_ini_value`, `sysutils_template_file`, `sysutils_directory`, `sysutils_symlink` and `sysutils_cron_job` resources and the `sysutils_file` and `sysutils_directory` data sources; " +
					"`sysutils_mount`, `sysutils_sysctl`, `sysutils_kernel_module` and `sysutils_package` change the running host and refuse to plan when `root_dir` is set. " +
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
	data := &providerData{systemd: p.systemd, mount: p.mount, sysctl: p.sysctl, kernelModule: p.kernelModule, cron: p.cron, pkg: p.pkg, root: root}
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

func (p *sysutilsProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewFileResource,
		NewFileLineResource,
		NewIniValueResource,
		NewTemplateFileResource,
		NewDirectoryResource,
		NewSymlinkResource,
		NewUserResource,
		NewGroupResource,
		NewExecResource,
		NewSystemdUnitResource,
		NewMountResource,
		NewSysctlResource,
		NewKernelModuleResource,
		NewCronJobResource,
		NewPackageResource,
	}
}

func (p *sysutilsProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewDirectoryDataSource,
		NewFileDataSource,
		NewUserDataSource,
		NewGroupDataSource,
	}
}
