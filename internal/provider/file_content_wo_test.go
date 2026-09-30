package provider

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // Only to check that the state lacks it.
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
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
// file below stateDir (see harnessStateDir), in plain text or in any
// base64-encoded field (the private state is stored base64-encoded). If
// written is not empty, it is the content last written from content_wo:
// its SHA-256 and MD5 checksums, which would reveal a low-entropy secret,
// must not occur either, and the resource's private state must hold an
// argon2id record.
func checkStateHasNoPlaintext(stateDir, written string, secrets ...string) resource.TestCheckFunc {
	if written != "" {
		sha, md := sha256.Sum256([]byte(written)), md5.Sum([]byte(written)) //nolint:gosec
		secrets = append(slices.Clone(secrets),
			hex.EncodeToString(sha[:]), hex.EncodeToString(md[:]),
			base64.StdEncoding.EncodeToString(sha[:]), base64.StdEncoding.EncodeToString(md[:]),
			base64.RawStdEncoding.EncodeToString(sha[:]), base64.RawStdEncoding.EncodeToString(md[:]))
	}
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
				if bytes.Contains(bytes.ToLower(h), bytes.ToLower([]byte(s))) {
					return fmt.Errorf("state contains the plaintext or checksum %q:\n%s", s, h)
				}
			}
		}
		if written != "" && !bytes.Contains(private, []byte(`"argon2id"`)) {
			return fmt.Errorf("private state of sysutils_file has no argon2id record:\n%s", private)
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
	prior := func(version types.Int64) *fileModel {
		return &fileModel{ContentWOVersion: version}
	}
	one, two := types.Int64Value(1), types.Int64Value(2)
	rec, drifted := &contentWORecord{}, &contentWORecord{Drift: true}
	tests := []struct {
		name    string
		prior   *fileModel
		version types.Int64
		rec     *contentWORecord
		want    bool
	}{
		{"unchanged", prior(one), one, rec, false},
		{"unchanged without version", prior(types.Int64Null()), types.Int64Null(), rec, false},
		{"version changed", prior(one), two, rec, true},
		{"version added", prior(types.Int64Null()), one, rec, true},
		{"version removed", prior(one), types.Int64Null(), rec, true},
		{"version unknown", prior(one), types.Int64Unknown(), rec, true},
		{"nothing recorded", prior(one), one, nil, true},
		{"drift", prior(one), one, drifted, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := contentWONeedsWrite(tt.prior, tt.version, tt.rec); got != tt.want {
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

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		TerraformVersionChecks:   writeOnlyVersionChecks,
		CheckDestroy:             checkPathGone(target),
		Steps: []resource.TestStep{
			{
				// Create: the file is written. The checksums of a
				// write-only value are never stored, as they would reveal
				// a low-entropy secret.
				Config: woConfig(secret1, 1, "0600"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(testFileWOResource, plancheck.ResourceActionCreate),
					plancheck.ExpectKnownValue(testFileWOResource, tfjsonpath.New("content_sha256"), knownvalue.Null()),
					plancheck.ExpectKnownValue(testFileWOResource, tfjsonpath.New("content_md5"), knownvalue.Null()),
				}},
				ConfigStateChecks: []statecheck.StateCheck{
					checkNull("content_wo"), checkNull("content"), checkNull("sensitive_content"), checkNull("content_base64"),
					checkNull("content_sha256"), checkNull("content_md5"),
					statecheck.ExpectKnownValue(testFileWOResource, tfjsonpath.New("content_wo_version"), knownvalue.Int64Exact(1)),
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(target, secret1),
					checkFileMode(target, 0o600),
					checkStateHasNoPlaintext(stateDir, secret1, allSecrets...),
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
				ConfigStateChecks: []statecheck.StateCheck{checkNull("content_wo"), checkNull("content_sha256"), checkNull("content_md5")},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(target, secret2),
					checkStateHasNoPlaintext(stateDir, secret2, allSecrets...),
				),
			},
			{
				// Drift: refresh finds that the file no longer has the
				// content recorded in private state, so it is rewritten.
				PreConfig: func() { mustWrite(t, target, "TOKEN=tampered\n") },
				Config:    woConfig(secret2, 2, "0600"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(testFileWOResource, plancheck.ResourceActionUpdate),
					plancheck.ExpectUnknownValue(testFileWOResource, tfjsonpath.New("content_sha256")),
				}},
				ConfigStateChecks: []statecheck.StateCheck{checkNull("content_sha256"), checkNull("content_md5")},
				Check:             checkFileContent(target, secret2),
			},
			{
				// Changing only the mode leaves the content alone, even
				// though content_wo differs.
				Config: woConfig(secret3, 2, "0640"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(testFileWOResource, plancheck.ResourceActionUpdate),
					plancheck.ExpectKnownValue(testFileWOResource, tfjsonpath.New("content_sha256"), knownvalue.Null()),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(target, secret2),
					checkFileMode(target, 0o640),
					checkStateHasNoPlaintext(stateDir, secret2, allSecrets...),
				),
			},
			{
				// Ephemeral values are accepted.
				Config:          ephemeralConfig(3),
				ConfigVariables: config.Variables{"secret": config.StringVariable(secret3)},
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(testFileWOResource, plancheck.ResourceActionUpdate),
				}},
				ConfigStateChecks: []statecheck.StateCheck{checkNull("content_wo"), checkNull("content_sha256"), checkNull("content_md5")},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(target, secret3),
					checkFileMode(target, 0o600),
					checkStateHasNoPlaintext(stateDir, secret3, allSecrets...),
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
				ConfigStateChecks: []statecheck.StateCheck{checkNull("content"), checkNull("content_sha256"), checkNull("content_md5")},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFileContent(target, secret1),
					checkStateHasNoPlaintext(stateDir, secret1, allSecrets...),
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
					statecheck.ExpectKnownValue(testFileWOResource, tfjsonpath.New("content_sha256"), knownvalue.Null()),
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

// mapPrivate is an in-memory private state.
type mapPrivate map[string][]byte

func (p mapPrivate) GetKey(_ context.Context, key string) ([]byte, diag.Diagnostics) {
	return p[key], nil
}

func (p mapPrivate) SetKey(_ context.Context, key string, value []byte) diag.Diagnostics {
	if len(value) == 0 {
		delete(p, key)
	} else {
		p[key] = value
	}
	return nil
}

// The record of written content must not let whoever reads the state test
// guesses of the secret cheaply: it holds neither checksum, only a salted
// argon2id hash, and the same content gets a different record each time.
func TestContentWORecord(t *testing.T) {
	ctx := context.Background()
	const secret = "hunter2"
	sum := sha256Hex([]byte(secret))
	rec, err := newContentWORecord(sum)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.matches(sum) {
		t.Error("record does not match the content it was made from")
	}
	if rec.matches(sha256Hex([]byte("hunter3"))) {
		t.Error("record matches other content")
	}
	if (&contentWORecord{Drift: true}).matches(sum) {
		t.Error("empty record matches")
	}
	again, err := newContentWORecord(sum)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(rec.Salt, again.Salt) || bytes.Equal(rec.Hash, again.Hash) {
		t.Error("two records of the same content share salt or hash")
	}

	p := mapPrivate{fileContentWOLegacyPrivateKey: []byte(`"` + sum + `"`)}
	if diags := storeContentWORecord(ctx, p, rec); diags.HasError() {
		t.Fatal(diags)
	}
	if _, ok := p[fileContentWOLegacyPrivateKey]; ok {
		t.Error("storing a record kept the legacy plain checksum")
	}
	raw := string(p[fileContentWOPrivateKey])
	md := md5Hex([]byte(secret))
	for _, leak := range []string{secret, sum, md, base64.StdEncoding.EncodeToString(mustHex(t, sum))} {
		if strings.Contains(strings.ToLower(raw), strings.ToLower(leak)) {
			t.Errorf("private state %s contains %q", raw, leak)
		}
	}
	loaded, diags := loadContentWORecord(ctx, p)
	if diags.HasError() || loaded == nil || !loaded.matches(sum) || loaded.Drift {
		t.Errorf("loadContentWORecord = %+v, %v; want the stored record", loaded, diags)
	}
	if diags := storeContentWORecord(ctx, p, nil); diags.HasError() || len(p) != 0 {
		t.Errorf("removing the record left %v (%v)", p, diags)
	}

	// Development builds before v1.3.0 kept the plain checksum; such a
	// record counts as drift, so that the next apply replaces it.
	legacy, diags := loadContentWORecord(ctx, mapPrivate{fileContentWOLegacyPrivateKey: []byte(`"` + sum + `"`)})
	if diags.HasError() || legacy == nil || !legacy.Drift {
		t.Errorf("legacy record loaded as %+v, %v; want drift", legacy, diags)
	}
	if none, diags := loadContentWORecord(ctx, mapPrivate{}); none != nil || diags.HasError() {
		t.Errorf("empty private state loaded as %+v, %v", none, diags)
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A refresh of a resource that stores no content attribute uses content_wo,
// even if its private state has no record (it was lost, or written by a
// development build under another key): the file's content must not be read
// into the state, which is only done for a resource being imported.
func TestFileReadWithoutContentWORecord(t *testing.T) {
	ctx := context.Background()
	const secret = "TOKEN=wo-lost-record-7a3f\n"
	target := filepath.Join(t.TempDir(), "secret.env")
	if err := os.WriteFile(target, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, err := testAccProtoV6ProviderFactories["sysutils"]()
	if err != nil {
		t.Fatal(err)
	}
	schemas, err := srv.GetProviderSchema(ctx, &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	objType, ok := schemas.ResourceSchemas["sysutils_file"].ValueType().(tftypes.Object)
	if !ok {
		t.Fatal("sysutils_file schema is not an object")
	}

	read := func(t *testing.T, set map[string]any) map[string]tftypes.Value {
		t.Helper()
		vals := map[string]tftypes.Value{}
		for name, typ := range objType.AttributeTypes {
			vals[name] = tftypes.NewValue(typ, set[name])
		}
		dv, err := tfprotov6.NewDynamicValue(objType, tftypes.NewValue(objType, vals))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := srv.ReadResource(ctx, &tfprotov6.ReadResourceRequest{TypeName: "sysutils_file", CurrentState: &dv})
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range resp.Diagnostics {
			if d.Severity == tfprotov6.DiagnosticSeverityError {
				t.Fatalf("ReadResource: %s: %s", d.Summary, d.Detail)
			}
		}
		if bytes.Contains(resp.Private, []byte("wo-lost-record")) {
			t.Errorf("private state contains the secret: %s", resp.Private)
		}
		state, err := resp.NewState.Unmarshal(objType)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]tftypes.Value
		if err := state.As(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	t.Run("content_wo", func(t *testing.T) {
		got := read(t, map[string]any{"path": target, "id": target, "mode": "0600", "content_wo_version": big.NewFloat(1)})
		for _, attr := range []string{"content", "content_base64", "sensitive_content", "content_sha256", "content_md5"} {
			if !got[attr].IsNull() {
				t.Errorf("%s = %v, want null", attr, got[attr])
			}
		}
	})
	t.Run("import", func(t *testing.T) {
		// Right after import only path and id are known; the content is
		// read, as documented.
		got := read(t, map[string]any{"path": target, "id": target})
		if !got["content"].Equal(tftypes.NewValue(tftypes.String, secret)) {
			t.Errorf("content = %v, want the file's text", got["content"])
		}
	})
}
