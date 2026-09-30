package provider

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/config"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"
)

// Write-only attributes need Terraform 1.11 (OpenTofu 1.11) or later.
var writeOnlyVersionChecks = []tfversion.TerraformVersionCheck{tfversion.SkipBelow(tfversion.Version1_11_0)}

const testFileWOResource = "sysutils_file.test"

// harnessStateDir makes plugin-testing create its working directories below
// a new directory and returns it, so that checks can inspect the raw state
// file, private state included; see readHarnessState.
func harnessStateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TF_ACC_TEMP_DIR", dir)
	return dir
}

// readHarnessState returns the raw terraform.tfstate below dir, which must
// be the only one.
func readHarnessState(dir string) ([]byte, error) {
	var found []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == "terraform.tfstate" {
			found = append(found, p)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if len(found) != 1 {
		return nil, fmt.Errorf("found %d state files below %s, want 1: %v", len(found), dir, found)
	}
	return os.ReadFile(found[0])
}

// checkStateHasNoPlaintext fails if any of secrets occurs in the raw state
// file below stateDir (see harnessStateDir), in plain text or in any base64-encoded field (the
// private state is stored base64-encoded). If wantPrivateSHA is not empty,
// the resource's private state must record it as the checksum of the
// written content.
func checkStateHasNoPlaintext(stateDir, wantPrivateSHA string, secrets ...string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		raw, err := readHarnessState(stateDir)
		if err != nil {
			return fmt.Errorf("reading state: %w", err)
		}
		var st struct {
			Resources []struct {
				Type      string `json:"type"`
				Instances []struct {
					Private string `json:"private"`
				} `json:"instances"`
			} `json:"resources"`
		}
		if err := json.Unmarshal(raw, &st); err != nil {
			return fmt.Errorf("decoding state: %w", err)
		}
		haystacks := [][]byte{raw}
		var private []byte
		for _, r := range st.Resources {
			for _, inst := range r.Instances {
				decoded := decodeBase64Recursively([]byte(inst.Private))
				haystacks = append(haystacks, decoded...)
				if r.Type == "sysutils_file" {
					private = bytes.Join(decoded, []byte("\n"))
				}
			}
		}
		for _, h := range haystacks {
			for _, s := range secrets {
				if bytes.Contains(h, []byte(s)) {
					return fmt.Errorf("state contains the plaintext %q:\n%s", s, h)
				}
			}
		}
		if wantPrivateSHA != "" && !bytes.Contains(private, []byte(wantPrivateSHA)) {
			return fmt.Errorf("private state of sysutils_file does not record checksum %s:\n%s", wantPrivateSHA, private)
		}
		return nil
	}
}

// decodeBase64Recursively returns b and everything that decodes from it as
// base64, directly or in the string values of JSON objects, recursively.
func decodeBase64Recursively(b []byte) [][]byte {
	out := [][]byte{b}
	if dec, err := base64.StdEncoding.DecodeString(string(b)); err == nil && len(dec) > 0 {
		out = append(out, decodeBase64Recursively(dec)...)
	}
	var obj map[string]any
	if json.Unmarshal(b, &obj) == nil {
		for _, v := range obj {
			if s, ok := v.(string); ok {
				out = append(out, decodeBase64Recursively([]byte(s))...)
			}
		}
	}
	return out
}

func TestContentWONeedsWrite(t *testing.T) {
	const sum = "abc"
	prior := func(version types.Int64, sha types.String) *fileModel {
		return &fileModel{ContentWOVersion: version, ContentSHA256: sha}
	}
	one, two := types.Int64Value(1), types.Int64Value(2)
	tests := []struct {
		name    string
		prior   *fileModel
		version types.Int64
		written string
		want    bool
	}{
		{"unchanged", prior(one, types.StringValue(sum)), one, sum, false},
		{"unchanged without version", prior(types.Int64Null(), types.StringValue(sum)), types.Int64Null(), sum, false},
		{"version changed", prior(one, types.StringValue(sum)), two, sum, true},
		{"version added", prior(types.Int64Null(), types.StringValue(sum)), one, sum, true},
		{"version removed", prior(one, types.StringValue(sum)), types.Int64Null(), sum, true},
		{"version unknown", prior(one, types.StringValue(sum)), types.Int64Unknown(), sum, true},
		{"nothing recorded", prior(one, types.StringValue(sum)), one, "", true},
		{"drift", prior(one, types.StringValue("def")), one, sum, true},
		{"no checksum in state", prior(one, types.StringNull()), one, sum, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := contentWONeedsWrite(tt.prior, tt.version, tt.written); got != tt.want {
				t.Errorf("contentWONeedsWrite = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAccFile_contentWO(t *testing.T) {
	target := filepath.Join(t.TempDir(), "secret.env")
	stateDir := harnessStateDir(t)
	const (
		secret1 = "TOKEN=wo-secret-one-5b1e\n"
		secret2 = "TOKEN=wo-secret-two-9c4d\n"
		secret3 = "TOKEN=wo-secret-three-e27a\n"
		plain   = "not a secret\n"
	)
	allSecrets := []string{"wo-secret-one-5b1e", "wo-secret-two-9c4d", "wo-secret-three-e27a"}

	woConfig := func(content string, version int, mode string) string {
		return fmt.Sprintf(`
resource "sysutils_file" "test" {
  path               = %q
  content_wo         = %q
  content_wo_version = %d
  mode               = %q
}
`, target, content, version, mode)
	}
	// The same, with the secret in an ephemeral variable, which Terraform
	// accepts only in write-only arguments.
	ephemeralConfig := func(version int) string {
		return fmt.Sprintf(`
variable "secret" {
  type      = string
  ephemeral = true
}

resource "sysutils_file" "test" {
  path               = %q
  content_wo         = var.secret
  content_wo_version = %d
  mode               = "0600"
}
`, target, version)
	}
	checkNull := func(attr string) statecheck.StateCheck {
		return statecheck.ExpectKnownValue(testFileWOResource, tfjsonpath.New(attr), knownvalue.Null())
	}
	checkSHA := func(content string) statecheck.StateCheck {
		return statecheck.ExpectKnownValue(testFileWOResource, tfjsonpath.New("content_sha256"),
			knownvalue.StringExact(sha256Hex([]byte(content))))
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		TerraformVersionChecks:   writeOnlyVersionChecks,
		CheckDestroy:             checkPathGone(target),
		Steps: []resource.TestStep{
			{
				// Create: the file is written, and the checksums are known
				// only after apply.
				Config: woConfig(secret1, 1, "0600"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(testFileWOResource, plancheck.ResourceActionCreate),
					plancheck.ExpectUnknownValue(testFileWOResource, tfjsonpath.New("content_sha256")),
				}},
				ConfigStateChecks: []statecheck.StateCheck{
					checkNull("content_wo"), checkNull("content"), checkNull("sensitive_content"), checkNull("content_base64"),
					checkSHA(secret1),
					statecheck.ExpectKnownValue(testFileWOResource, tfjsonpath.New("content_wo_version"), knownvalue.Int64Exact(1)),
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(target, secret1),
					checkFileMode(target, 0o600),
					checkStateHasNoPlaintext(stateDir, sha256Hex([]byte(secret1)), allSecrets...),
				),
			},
			{
				// A changed value with the same version is not written:
				// Terraform keeps no trace of the old value to compare.
				Config: woConfig(secret2, 1, "0600"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectEmptyPlan(),
				}},
				Check: checkFileContent(target, secret1),
			},
			{
				// A new version writes the current value.
				Config: woConfig(secret2, 2, "0600"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(testFileWOResource, plancheck.ResourceActionUpdate),
					plancheck.ExpectUnknownValue(testFileWOResource, tfjsonpath.New("content_sha256")),
				}},
				ConfigStateChecks: []statecheck.StateCheck{checkNull("content_wo"), checkSHA(secret2)},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(target, secret2),
					checkStateHasNoPlaintext(stateDir, sha256Hex([]byte(secret2)), allSecrets...),
				),
			},
			{
				// Drift: the file no longer has the checksum recorded in
				// private state, so it is rewritten.
				PreConfig: func() { mustWrite(t, target, "TOKEN=tampered\n") },
				Config:    woConfig(secret2, 2, "0600"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(testFileWOResource, plancheck.ResourceActionUpdate),
				}},
				ConfigStateChecks: []statecheck.StateCheck{checkSHA(secret2)},
				Check:             checkFileContent(target, secret2),
			},
			{
				// Changing only the mode leaves the content alone, even
				// though content_wo differs.
				Config: woConfig(secret3, 2, "0640"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(testFileWOResource, plancheck.ResourceActionUpdate),
					plancheck.ExpectKnownValue(testFileWOResource, tfjsonpath.New("content_sha256"),
						knownvalue.StringExact(sha256Hex([]byte(secret2)))),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(target, secret2),
					checkFileMode(target, 0o640),
					checkStateHasNoPlaintext(stateDir, sha256Hex([]byte(secret2)), allSecrets...),
				),
			},
			{
				// Ephemeral values are accepted.
				Config:          ephemeralConfig(3),
				ConfigVariables: config.Variables{"secret": config.StringVariable(secret3)},
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(testFileWOResource, plancheck.ResourceActionUpdate),
				}},
				ConfigStateChecks: []statecheck.StateCheck{checkNull("content_wo"), checkSHA(secret3)},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(target, secret3),
					checkFileMode(target, 0o600),
					checkStateHasNoPlaintext(stateDir, sha256Hex([]byte(secret3)), allSecrets...),
				),
			},
			{
				// Switching to content stores that content and forgets
				// the recorded checksum.
				Config: fmt.Sprintf(`
resource "sysutils_file" "test" {
  path    = %q
  content = %q
  mode    = "0600"
}
`, target, plain),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(testFileWOResource, tfjsonpath.New("content"), knownvalue.StringExact(plain)),
					checkNull("content_wo_version"),
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(target, plain),
					checkStateHasNoPlaintext(stateDir, "", allSecrets...),
					checkPrivateLacks(stateDir, "content_wo_sha256"),
				),
			},
			{
				// Switching back writes content_wo, whatever the version,
				// and removes content from the state.
				Config: woConfig(secret1, 3, "0600"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(testFileWOResource, plancheck.ResourceActionUpdate),
				}},
				ConfigStateChecks: []statecheck.StateCheck{checkNull("content"), checkSHA(secret1)},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(target, secret1),
					checkStateHasNoPlaintext(stateDir, sha256Hex([]byte(secret1)), allSecrets...),
				),
			},
		},
	})
}

// checkPrivateLacks fails if the decoded private state of any resource in
// the state below stateDir contains key.
func checkPrivateLacks(stateDir, key string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		raw, err := readHarnessState(stateDir)
		if err != nil {
			return err
		}
		var st struct {
			Resources []struct {
				Instances []struct {
					Private string `json:"private"`
				} `json:"instances"`
			} `json:"resources"`
		}
		if err := json.Unmarshal(raw, &st); err != nil {
			return err
		}
		for _, r := range st.Resources {
			for _, inst := range r.Instances {
				for _, h := range decodeBase64Recursively([]byte(inst.Private)) {
					if strings.Contains(string(h), key) {
						return fmt.Errorf("private state still has %q: %s", key, h)
					}
				}
			}
		}
		return nil
	}
}

// A file that was never written from content_wo, such as an imported one,
// has no recorded checksum, so it is written on the first apply even though
// its content_wo_version does not change.
func TestAccFile_contentWOImport(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "imported.env")
	mustWrite(t, target, "old\n")
	const secret = "TOKEN=wo-imported-31f0\n"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		TerraformVersionChecks:   writeOnlyVersionChecks,
		CheckDestroy:             checkPathGone(target),
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
import {
  to = sysutils_file.test
  id = %q
}

resource "sysutils_file" "test" {
  path       = %q
  content_wo = %q
  mode       = "0644"
}
`, target, target, secret),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(testFileWOResource, plancheck.ResourceActionUpdate),
				}},
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(testFileWOResource, tfjsonpath.New("content"), knownvalue.Null()),
					statecheck.ExpectKnownValue(testFileWOResource, tfjsonpath.New("content_sha256"),
						knownvalue.StringExact(sha256Hex([]byte(secret)))),
				},
				Check: checkFileContent(target, secret),
			},
		},
	})
}

func TestAccFile_contentWOConflicts(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "f")
	src := filepath.Join(dir, "src")
	mustWrite(t, src, "x")

	conflict := regexp.MustCompile(`(?s)Invalid Attribute Combination.*content_wo`)
	step := func(extra string, want *regexp.Regexp) resource.TestStep {
		return resource.TestStep{
			Config: fmt.Sprintf(`
resource "sysutils_file" "test" {
  path = %q
  %s
}
`, target, extra),
			PlanOnly:    true,
			ExpectError: want,
		}
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		TerraformVersionChecks:   writeOnlyVersionChecks,
		Steps: []resource.TestStep{
			step(`content_wo = "a"
  content = "b"`, conflict),
			step(`content_wo = "a"
  sensitive_content = "b"`, conflict),
			step(`content_wo = "a"
  content_base64 = "Yg=="`, conflict),
			step(fmt.Sprintf(`content_wo = "a"
  source = %q`, src), conflict),
			step(`content = "a"
  content_wo_version = 1`, regexp.MustCompile(`(?s)Attribute "content_wo" must be specified when\s+"content_wo_version" is\s+specified`)),
		},
	})
}

// Terraform before 1.11 does not know write-only attributes; the provider
// rejects a value for content_wo rather than storing it in the state.
func TestAccFile_contentWOUnsupportedTerraform(t *testing.T) {
	target := filepath.Join(t.TempDir(), "f")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		TerraformVersionChecks:   []tfversion.TerraformVersionCheck{tfversion.SkipAbove(tfversion.Version1_10_0)},
		Steps: []resource.TestStep{{
			Config: fmt.Sprintf(`
resource "sysutils_file" "test" {
  path       = %q
  content_wo = "a"
}
`, target),
			ExpectError: regexp.MustCompile(`Write-only attributes are only supported in\s+Terraform 1.11 and later`),
		}},
	})
}
