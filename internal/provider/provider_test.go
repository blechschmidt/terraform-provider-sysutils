package provider

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/hashicorp/go-version"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"
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
	tofu, err := openTofuVersion()
	if err != nil || tofu == nil {
		return false, err
	}
	return !tofu.LessThan(version.Must(version.NewVersion("1.13.0"))), nil
}

// openTofuVersion returns the version of the CLI the acceptance tests use
// (TF_ACC_TERRAFORM_PATH, or terraform in PATH) if it is OpenTofu, and nil
// if it is Terraform.
func openTofuVersion() (*version.Version, error) {
	cli := os.Getenv("TF_ACC_TERRAFORM_PATH")
	if cli == "" {
		p, err := exec.LookPath("terraform")
		if err != nil {
			// plugin-testing may install Terraform itself.
			return nil, nil
		}
		cli = p
	}
	out, err := exec.Command(cli, "version").Output()
	if err != nil {
		return nil, fmt.Errorf("%s version: %w", cli, err)
	}
	first, _, _ := strings.Cut(string(out), "\n")
	v, ok := strings.CutPrefix(first, "OpenTofu v")
	if !ok {
		return nil, nil
	}
	tofu, err := version.NewVersion(strings.TrimSpace(v))
	if err != nil {
		return nil, fmt.Errorf("parsing %q: %w", first, err)
	}
	return tofu, nil
}

// skipOpenTofuBelow is a TerraformVersionCheck that skips the test on
// OpenTofu before minimum, for features that OpenTofu added in a later
// version than Terraform. Its message matches that of tfversion.SkipBelow,
// which the acceptance scripts allow as a skip reason.
type skipOpenTofuBelow struct {
	minimum *version.Version
	feature string
}

func (s skipOpenTofuBelow) CheckTerraformVersion(_ context.Context, req tfversion.CheckTerraformVersionRequest, resp *tfversion.CheckTerraformVersionResponse) {
	tofu, err := openTofuVersion()
	switch {
	case err != nil:
		resp.Error = err
	case tofu != nil && tofu.LessThan(s.minimum):
		resp.Skip = fmt.Sprintf("Terraform CLI version %s is below minimum version %s: OpenTofu supports %s only from %s: skipping test",
			req.TerraformVersion, s.minimum, s.feature, s.minimum)
	}
}
