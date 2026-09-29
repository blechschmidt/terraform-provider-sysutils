package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/resource"
)

// TestSchemaMarkdownDescriptions ensures that every resource, data source and
// attribute has a MarkdownDescription. The docs under docs/ are generated
// from these descriptions by tfplugindocs, so a missing one would show up as
// an undocumented attribute.
func TestSchemaMarkdownDescriptions(t *testing.T) {
	ctx := context.Background()
	p := &sysutilsProvider{}

	for _, newResource := range p.Resources(ctx) {
		r := newResource()
		var meta resource.MetadataResponse
		r.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: "sysutils"}, &meta)
		var resp resource.SchemaResponse
		r.Schema(ctx, resource.SchemaRequest{}, &resp)

		if resp.Schema.MarkdownDescription == "" {
			t.Errorf("resource %s: missing MarkdownDescription", meta.TypeName)
		}
		for name, attr := range resp.Schema.Attributes {
			if attr.GetMarkdownDescription() == "" {
				t.Errorf("resource %s: attribute %q is missing a MarkdownDescription", meta.TypeName, name)
			}
		}
	}

	for _, newDataSource := range p.DataSources(ctx) {
		d := newDataSource()
		var meta datasource.MetadataResponse
		d.Metadata(ctx, datasource.MetadataRequest{ProviderTypeName: "sysutils"}, &meta)
		var resp datasource.SchemaResponse
		d.Schema(ctx, datasource.SchemaRequest{}, &resp)

		if resp.Schema.MarkdownDescription == "" {
			t.Errorf("data source %s: missing MarkdownDescription", meta.TypeName)
		}
		for name, attr := range resp.Schema.Attributes {
			if attr.GetMarkdownDescription() == "" {
				t.Errorf("data source %s: attribute %q is missing a MarkdownDescription", meta.TypeName, name)
			}
		}
	}
}
