package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/resource"
)

// TestResourceDocsHaveImportExamples checks that every resource with
// ImportState has examples/resources/<type>/import.sh, which tfplugindocs
// renders as the Import section of the resource's page, and that resources
// without import support don't advertise one.
func TestResourceDocsHaveImportExamples(t *testing.T) {
	p := New("test")()
	var meta provider.MetadataResponse
	p.Metadata(context.Background(), provider.MetadataRequest{}, &meta)

	for _, newResource := range p.Resources(context.Background()) {
		r := newResource()
		var resp resource.MetadataResponse
		r.Metadata(context.Background(), resource.MetadataRequest{ProviderTypeName: meta.TypeName}, &resp)

		importFile := filepath.Join("..", "..", "examples", "resources", resp.TypeName, "import.sh")
		_, err := os.Stat(importFile)
		_, importable := r.(resource.ResourceWithImportState)
		switch {
		case importable && err != nil:
			t.Errorf("%s supports import but %s is missing: %v", resp.TypeName, importFile, err)
		case !importable && err == nil:
			t.Errorf("%s does not support import but %s exists", resp.TypeName, importFile)
		}
	}
}
