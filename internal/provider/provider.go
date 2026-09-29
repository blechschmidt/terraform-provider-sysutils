package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
)

type sysutilsProvider struct {
	version string
	// systemd overrides how sysutils_systemd_unit reaches systemd. It is nil
	// in production and set by unit tests to a fake systemctl.
	systemd *systemdConfig
}

// providerData is passed to resources that implement
// resource.ResourceWithConfigure.
type providerData struct {
	systemd *systemdConfig
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
		MarkdownDescription: "The `sysutils` provider exposes a small set of primitives for host-level administration from Terraform: files, directories, symlinks, local users and groups, systemd units, and command execution. " +
			"The provider takes no configuration arguments.",
	}
}

func (p *sysutilsProvider) Configure(_ context.Context, _ provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	resp.ResourceData = &providerData{systemd: p.systemd}
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
