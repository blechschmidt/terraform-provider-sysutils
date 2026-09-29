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
}

// providerData is passed to resources that implement
// resource.ResourceWithConfigure.
type providerData struct {
	systemd *systemdConfig
	mount   *mountConfig
	sysctl  *sysctlConfig
	// root is the directory that the paths of the file, file line, template
	// file, directory and symlink resources and the file and directory data
	// sources are confined to; see rootfs.go.
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
		MarkdownDescription: "The `sysutils` provider exposes a small set of primitives for host-level administration from Terraform: files, directories, symlinks, local users and groups, systemd units, mounts, kernel parameters, and command execution. " +
			"All arguments are optional.",
		Attributes: map[string]schema.Attribute{
			"root_dir": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Directory that every managed path is relative to, as if the provider ran in a chroot there. " +
					"With `root_dir = \"/srv/rootfs\"`, a `sysutils_file` with `path = \"/etc/hosts\"` writes `/srv/rootfs/etc/hosts`. " +
					"Use it to build a container or OS image root filesystem tree. " +
					"Applies to the `sysutils_file`, `sysutils_file_line`, `sysutils_template_file`, `sysutils_directory` and `sysutils_symlink` resources and the `sysutils_file` and `sysutils_directory` data sources; " +
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
	data := &providerData{systemd: p.systemd, mount: p.mount, sysctl: p.sysctl, root: root}
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
		NewTemplateFileResource,
		NewDirectoryResource,
		NewSymlinkResource,
		NewUserResource,
		NewGroupResource,
		NewExecResource,
		NewSystemdUnitResource,
		NewMountResource,
		NewSysctlResource,
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
