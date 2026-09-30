package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource                   = (*packageDataSource)(nil)
	_ datasource.DataSourceWithConfigure      = (*packageDataSource)(nil)
	_ datasource.DataSourceWithValidateConfig = (*packageDataSource)(nil)
)

func NewPackageDataSource() datasource.DataSource { return &packageDataSource{} }

type packageDataSource struct {
	hostOnlyDataSource
	cfg *packageConfig
}

type packageDataSourceModel struct {
	Name             types.String `tfsdk:"name"`
	Manager          types.String `tfsdk:"manager"`
	RefreshCache     types.Bool   `tfsdk:"refresh_cache"`
	PackageManager   types.String `tfsdk:"package_manager"`
	Installed        types.Bool   `tfsdk:"installed"`
	Version          types.String `tfsdk:"version"`
	Architecture     types.String `tfsdk:"architecture"`
	AvailableVersion types.String `tfsdk:"available_version"`
	ID               types.String `tfsdk:"id"`
}

func (d *packageDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_package"
}

func (d *packageDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads whether an OS package is installed, its version and architecture, and the version the local package index offers, with the same package manager backends as `sysutils_package`: `apt` on Debian and Ubuntu, `dnf` or `yum` on Fedora and RHEL-like distributions, `apk` on Alpine. " +
			"Only read-only queries run (`dpkg-query` and `apt-cache policy`, `rpm -q` and `repoquery -C`, `apk info`, `apk list` and `apk policy`), so nothing on the host changes and no root privileges are needed, unless `refresh_cache` is set.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Name of the package, such as `\"nginx\"`. The same names as in `sysutils_package` are accepted: 1 to 128 letters, digits, `.`, `_`, `+` or `-`, starting with a letter or digit; with apt, lowercase and at least 2 characters. " +
					"Architecture qualifiers, version constraints, globs, file paths and virtual `provides` are not accepted.",
				Validators: []validator.String{stringCheck("package name", validatePackageName)},
			},
			"manager": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Package manager to query: `\"apt\"`, `\"dnf\"`, `\"yum\"` or `\"apk\"`. " +
					"`\"auto\"` or unset uses the first of them whose tools are installed, in that order, as `sysutils_package` does. Reading fails if the manager is not installed.",
				Validators: []validator.String{stringvalidator.OneOf(packageManagerAuto, packageManagerApt, packageManagerDnf, packageManagerYum, packageManagerApk)},
			},
			"refresh_cache": schema.BoolAttribute{
				Optional: true,
				MarkdownDescription: "Refresh the package index (`apt-get update`, `dnf makecache`, `yum makecache`, `apk update`) before reading `available_version`. " +
					"This changes the host, needs root privileges and network access to the repositories, and happens whenever the data source is read, including during `terraform plan`. " +
					"The refresh waits for, and holds, the provider's package-manager lock, which `sysutils_package` also takes, and happens at most once per provider run, shared with `update_cache` of `sysutils_package`. Defaults to `false`.",
			},
			"package_manager": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Package manager that was queried: `\"apt\"`, `\"dnf\"`, `\"yum\"` or `\"apk\"`.",
			},
			"installed": schema.BoolAttribute{
				Computed: true,
				MarkdownDescription: "Whether the package is installed, according to the package database. " +
					"With apt, packages that are only half-installed or whose configuration files remain after removal are not installed.",
			},
			"version": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Installed version, in the form the package database reports and `version` of `sysutils_package` accepts: " +
					"`[epoch:]upstream[-revision]` for apt, `[epoch:]version-release` for dnf and yum, `version-rN` for apk. Null if the package is not installed.",
			},
			"architecture": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Architecture of the installed package, in the package manager's terms: such as `\"amd64\"` or `\"all\"` with apt, `\"x86_64\"` or `\"noarch\"` with dnf and yum, `\"x86_64\"` or `\"noarch\"` with apk. " +
					"With several architectures installed, that of the first one the package database lists. Null if the package is not installed.",
			},
			"available_version": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Version that installing or upgrading the package would select from the local package index: apt's candidate as `apt-cache policy` shows it, which honours pinning and may be the installed version; " +
					"with dnf and yum the newest version in the enabled repositories, from the downloaded metadata (`repoquery -C`); with apk the newest version in a repository without a tag. " +
					"Null if the package is in no repository, or if the index has not been downloaded, as in fresh container images; set `refresh_cache` to download it.",
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Data source identifier (equal to `name`).",
			},
		},
	}
}

func (d *packageDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	data, diags := providerDataFrom(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	if data != nil {
		d.cfg = data.pkg
		d.fsRoot = data.root
	}
}

// ValidateConfig checks the name against the stricter rules of an explicitly
// configured manager; with "auto" they are checked once the manager is known.
func (d *packageDataSource) ValidateConfig(ctx context.Context, req datasource.ValidateConfigRequest, resp *datasource.ValidateConfigResponse) {
	var m packageDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if m.Manager.IsNull() || m.Manager.IsUnknown() || m.Manager.ValueString() == packageManagerAuto {
		return
	}
	if !m.Name.IsNull() && !m.Name.IsUnknown() && validatePackageName(m.Name.ValueString()) == nil {
		if err := validatePackageNameFor(m.Manager.ValueString(), m.Name.ValueString()); err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("name"), "Invalid package name", capitalize(err.Error())+".")
		}
	}
}

func (d *packageDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	d.checkHostOnly(&resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	var m packageDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	details, kind, diags := d.inspect(ctx, &m)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	m.ID = types.StringValue(m.Name.ValueString())
	m.PackageManager = types.StringValue(kind)
	m.Installed = types.BoolValue(details.Installed)
	m.Version = optionalString(details.Installed, details.Version)
	m.Architecture = optionalString(details.Installed, details.Architecture)
	m.AvailableVersion = optionalString(true, details.Candidate)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

// optionalString returns s as a value, or null if ok is false or s is
// empty.
func optionalString(ok bool, s string) types.String {
	if !ok || s == "" {
		return types.StringNull()
	}
	return types.StringValue(s)
}

// inspect resolves the package manager and reads the package with it,
// refreshing the index first if refresh_cache is set. It returns the
// manager's kind.
func (d *packageDataSource) inspect(ctx context.Context, m *packageDataSourceModel) (packageDetails, string, diag.Diagnostics) {
	var diags diag.Diagnostics
	name := m.Name.ValueString()
	if err := validatePackageName(name); err != nil {
		diags.AddAttributeError(path.Root("name"), "Invalid package name", capitalize(err.Error())+".")
		return packageDetails{}, "", diags
	}
	kind := packageManagerAuto
	if !m.Manager.IsNull() {
		kind = m.Manager.ValueString()
	}
	cfg := d.cfg.orDefault()
	mgr, err := cfg.resolve(kind)
	if err != nil {
		diags.AddAttributeError(path.Root("manager"), "Package manager not available", capitalize(err.Error())+".")
		return packageDetails{}, "", diags
	}
	if err := validatePackageNameFor(mgr.Kind(), name); err != nil {
		diags.AddAttributeError(path.Root("name"), "Invalid package name", capitalize(err.Error())+".")
		return packageDetails{}, "", diags
	}

	if m.RefreshCache.ValueBool() {
		// The lock serialises the refresh with the changes of
		// sysutils_package and guards cfg's record of refreshed indexes.
		// The package is read while it is still held, so that the result
		// matches the refreshed index.
		unlock, err := lockPackageManager(ctx)
		if err != nil {
			diags.AddError("Locking package manager", capitalize(err.Error())+".")
			return packageDetails{}, "", diags
		}
		defer unlock()
		if err := cfg.updateCacheOnce(ctx, mgr); err != nil {
			diags.AddAttributeError(path.Root("refresh_cache"), "Updating package index", capitalize(err.Error())+".")
			return packageDetails{}, "", diags
		}
	}
	details, err := mgr.Inspect(ctx, name)
	if err != nil {
		diags.AddError("Querying package", fmt.Sprintf("Reading package %s with %s: %s.", name, mgr.Kind(), err))
		return packageDetails{}, "", diags
	}
	return details, mgr.Kind(), diags
}
