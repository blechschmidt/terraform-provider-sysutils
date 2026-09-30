package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"github.com/hashicorp/terraform-plugin-framework/resource"
)

// TestSchemaMarkdownDescriptions ensures that every resource, data source,
// ephemeral resource, action and attribute has a MarkdownDescription. The docs under
// docs/ are generated from these descriptions by tfplugindocs, so a missing
// one would show up as an undocumented attribute.
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

	for _, newEphemeral := range p.EphemeralResources(ctx) {
		e := newEphemeral()
		var meta ephemeral.MetadataResponse
		e.Metadata(ctx, ephemeral.MetadataRequest{ProviderTypeName: "sysutils"}, &meta)
		var resp ephemeral.SchemaResponse
		e.Schema(ctx, ephemeral.SchemaRequest{}, &resp)

		if resp.Schema.MarkdownDescription == "" {
			t.Errorf("ephemeral resource %s: missing MarkdownDescription", meta.TypeName)
		}
		for name, attr := range resp.Schema.Attributes {
			if attr.GetMarkdownDescription() == "" {
				t.Errorf("ephemeral resource %s: attribute %q is missing a MarkdownDescription", meta.TypeName, name)
			}
		}
	}

	for _, newAction := range p.Actions(ctx) {
		a := newAction()
		var meta action.MetadataResponse
		a.Metadata(ctx, action.MetadataRequest{ProviderTypeName: "sysutils"}, &meta)
		var resp action.SchemaResponse
		a.Schema(ctx, action.SchemaRequest{}, &resp)

		if resp.Schema.MarkdownDescription == "" {
			t.Errorf("action %s: missing MarkdownDescription", meta.TypeName)
		}
		for name, attr := range resp.Schema.Attributes {
			if attr.GetMarkdownDescription() == "" {
				t.Errorf("action %s: attribute %q is missing a MarkdownDescription", meta.TypeName, name)
			}
		}
		if diags := resp.Schema.ValidateImplementation(ctx); diags.HasError() {
			t.Errorf("action %s: invalid schema: %v", meta.TypeName, diags)
		}
	}
}
