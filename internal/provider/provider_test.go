package provider

import (
	"os"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
)

var testAccProtoV6ProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
	"sysutils": providerserver.NewProtocol6WithError(New("test")()),
}

// requireRoot fails fast for tests that mutate system state.
// Acceptance tests are already gated by TF_ACC=1; this is an extra guard
// so TF_ACC=1 on a non-root workstation doesn't accidentally create users
// or overwrite files outside of the test container.
func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("integration test requires root; run inside the test container (make test-docker)")
	}
}
