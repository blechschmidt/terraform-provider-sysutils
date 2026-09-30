package provider

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/hashicorp/go-version"
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
		t.Skip("integration test requires root; run inside the test container (make testacc-docker)")
	}
}

// skipDashImportID is the SkipFunc of import steps whose ID starts with "-".
// OpenTofu 1.13 and later parse flags after the positional arguments, and
// terraform-exec does not end the flags of "import" with "--", so such an ID
// is rejected as an unknown flag and never reaches the provider. Terraform
// passes it on, so the CI jobs with Terraform still run these steps.
func skipDashImportID() (bool, error) {
	cli := os.Getenv("TF_ACC_TERRAFORM_PATH")
	if cli == "" {
		p, err := exec.LookPath("terraform")
		if err != nil {
			// plugin-testing may install Terraform itself, which passes the ID.
			return false, nil
		}
		cli = p
	}
	out, err := exec.Command(cli, "version").Output()
	if err != nil {
		return false, fmt.Errorf("%s version: %w", cli, err)
	}
	first, _, _ := strings.Cut(string(out), "\n")
	v, ok := strings.CutPrefix(first, "OpenTofu v")
	if !ok {
		return false, nil
	}
	tofu, err := version.NewVersion(strings.TrimSpace(v))
	if err != nil {
		return false, fmt.Errorf("parsing %q: %w", first, err)
	}
	return !tofu.LessThan(version.Must(version.NewVersion("1.13.0"))), nil
}
