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
		Description: "Basic Linux system utility resources (files, users, command execution).",
	}
}

func (p *sysutilsProvider) Configure(_ context.Context, _ provider.ConfigureRequest, _ *provider.ConfigureResponse) {
}

func (p *sysutilsProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewFileResource,
		NewUserResource,
		NewExecResource,
	}
}

func (p *sysutilsProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return nil
}
