package provider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	tfresource "github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// TestAccExec_timeoutKillsCommand checks that a command running past its
// timeout fails the apply with a clear diagnostic, promptly, and that no
// resource is recorded.
func TestAccExec_timeoutKillsCommand(t *testing.T) {
	requireRoot(t)

	start := time.Now()
	tfresource.Test(t, tfresource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []tfresource.TestStep{
			{
				Config: `
resource "sysutils_exec" "slow" {
  command = ["/bin/sh", "-c", "sleep 60 & wait"]
  timeout = "1s"
}`,
				ExpectError: regexp.MustCompile(`Command\s+timed\s+out\s+after\s+1s[\s\S]*process\s+group\s+was\s+killed`),
			},
		},
	})
	if elapsed := time.Since(start); elapsed > 45*time.Second {
		t.Errorf("test took %s; the 1s timeout did not stop the command", elapsed)
	}
}

func TestAccExec_timeoutNotReached(t *testing.T) {
	requireRoot(t)

	tfresource.Test(t, tfresource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []tfresource.TestStep{
			{
				Config: `
resource "sysutils_exec" "fast" {
  command = ["/bin/echo", "done"]
  timeout = "1m"
}`,
				Check: tfresource.ComposeAggregateTestCheckFunc(
					tfresource.TestCheckResourceAttr("sysutils_exec.fast", "stdout", "done\n"),
					tfresource.TestCheckResourceAttr("sysutils_exec.fast", "timeout", "1m"),
				),
			},
		},
	})
}

// TestAccExec_timeoutUpdateInPlace checks that changing the timeout updates
// the resource without re-running the command.
func TestAccExec_timeoutUpdateInPlace(t *testing.T) {
	requireRoot(t)

	counter := filepath.Join(t.TempDir(), "runs")
	config := func(timeout string) string {
		return fmt.Sprintf(`
resource "sysutils_exec" "count" {
  command = ["/bin/sh", "-c", "echo run >> %s"]
  timeout = %q
}`, counter, timeout)
	}
	tfresource.Test(t, tfresource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []tfresource.TestStep{
			{
				Config: config("10s"),
				Check:  checkFileContent(counter, "run\n"),
			},
			{
				Config: config("20s"),
				ConfigPlanChecks: tfresource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("sysutils_exec.count", plancheck.ResourceActionUpdate),
					},
				},
				Check: tfresource.ComposeAggregateTestCheckFunc(
					checkFileContent(counter, "run\n"),
					tfresource.TestCheckResourceAttr("sysutils_exec.count", "timeout", "20s"),
					tfresource.TestCheckResourceAttr("sysutils_exec.count", "exit_code", "0"),
				),
			},
		},
	})
}

func TestAccExec_invalidTimeout(t *testing.T) {
	tfresource.UnitTest(t, tfresource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []tfresource.TestStep{
			{
				Config: `
resource "sysutils_exec" "bad" {
  command = ["/bin/true"]
  timeout = "30"
}`,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`Invalid\s+duration`),
			},
			{
				Config: `
resource "sysutils_exec" "bad" {
  command = ["/bin/true"]
  timeout = "-1s"
}`,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`must\s+be\s+greater\s+than\s+zero`),
			},
		},
	})
}

// TestAccExec_destroyCommand checks that destroy_command runs on destroy with
// the resource's environment and working directory, that a non-zero exit
// fails the destroy and keeps the resource, and that the command can then be
// fixed in place.
func TestAccExec_destroyCommand(t *testing.T) {
	requireRoot(t)

	// inherit_parent_environment = false must also hide this from the
	// destroy command.
	t.Setenv("TF_SYSUTILS_PARENT_ONLY", "leaked")
	workDir := t.TempDir()
	marker := filepath.Join(workDir, "destroyed")
	config := func(destroyScript string) string {
		return fmt.Sprintf(`
resource "sysutils_exec" "managed" {
  command                    = ["/bin/true"]
  destroy_command            = ["/bin/sh", "-c", %q]
  working_directory          = %q
  inherit_parent_environment = false
  environment = {
    GREETING = "goodbye"
  }
}`, destroyScript, workDir)
	}
	failing := config(`echo "cleanup failed in $PWD" >&2; exit 4`)
	// "$${" is HCL's escape for a literal "${".
	fixed := config(`echo "$GREETING from $PWD, parent=$${TF_SYSUTILS_PARENT_ONLY:-unset}" > destroyed`)

	tfresource.Test(t, tfresource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkFileContent(marker, fmt.Sprintf("goodbye from %s, parent=unset\n", workDir)),
		Steps: []tfresource.TestStep{
			{
				Config: failing,
				Check:  tfresource.TestCheckResourceAttr("sysutils_exec.managed", "exit_code", "0"),
			},
			{
				Config:      failing,
				Destroy:     true,
				ExpectError: regexp.MustCompile(`Destroy\s+command\s+exited\s+with\s+non-zero\s+status\s+4[\s\S]*cleanup\s+failed\s+in\s+` + regexp.QuoteMeta(workDir)),
			},
			{
				// The failed destroy kept the resource. Fixing destroy_command
				// is an in-place update that does not re-run command.
				Config: fixed,
				ConfigPlanChecks: tfresource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("sysutils_exec.managed", plancheck.ResourceActionUpdate),
					},
				},
				Check: checkFileAbsent(marker),
			},
		},
	})
}

// TestAccExec_destroyCommandFailureIgnored checks that fail_on_nonzero = false
// also lets a failing destroy_command through.
func TestAccExec_destroyCommandFailureIgnored(t *testing.T) {
	requireRoot(t)

	tfresource.Test(t, tfresource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []tfresource.TestStep{
			{
				Config: `
resource "sysutils_exec" "tolerant" {
  command         = ["/bin/true"]
  destroy_command = ["/bin/sh", "-c", "exit 9"]
  fail_on_nonzero = false
}`,
			},
		},
	})
}

// TestAccExec_destroyCommandTimeout checks that the timeout also applies to
// destroy_command.
func TestAccExec_destroyCommandTimeout(t *testing.T) {
	requireRoot(t)

	config := func(destroy string) string {
		return fmt.Sprintf(`
resource "sysutils_exec" "hang" {
  command         = ["/bin/true"]
  destroy_command = ["/bin/sh", "-c", %q]
  timeout         = "1s"
}`, destroy)
	}
	tfresource.Test(t, tfresource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []tfresource.TestStep{
			{Config: config("sleep 60")},
			{
				Config:      config("sleep 60"),
				Destroy:     true,
				ExpectError: regexp.MustCompile(`Destroy\s+command\s+timed\s+out\s+after\s+1s`),
			},
			// Let the final destroy succeed.
			{Config: config("true")},
		},
	})
}

// TestAccExec_sensitiveOutput checks that with sensitive_output the output
// never appears in a non-sensitive attribute, in state or in the plan diff.
func TestAccExec_sensitiveOutput(t *testing.T) {
	requireRoot(t)

	const secret = "s3cr3t-token-value"
	config := func(timeout string) string {
		return fmt.Sprintf(`
resource "sysutils_exec" "secret" {
  command          = ["/bin/sh", "-c", "echo %s; echo %s-err >&2"]
  sensitive_output = true
  timeout          = %q
}`, secret, secret, timeout)
	}
	const addr = "sysutils_exec.secret"
	tfresource.Test(t, tfresource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []tfresource.TestStep{
			{
				Config: config("10s"),
				ConfigPlanChecks: tfresource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectSensitiveValue(addr, tfjsonpath.New("sensitive_stdout")),
						plancheck.ExpectSensitiveValue(addr, tfjsonpath.New("sensitive_stderr")),
					},
				},
				Check: tfresource.ComposeAggregateTestCheckFunc(
					tfresource.TestCheckNoResourceAttr(addr, "stdout"),
					tfresource.TestCheckNoResourceAttr(addr, "stderr"),
					tfresource.TestCheckResourceAttr(addr, "sensitive_stdout", secret+"\n"),
					tfresource.TestCheckResourceAttr(addr, "sensitive_stderr", secret+"-err\n"),
					tfresource.TestCheckResourceAttr(addr, "stdout_sha256", sha256Hex([]byte(secret+"\n"))),
					tfresource.TestCheckResourceAttr(addr, "stderr_sha256", sha256Hex([]byte(secret+"-err\n"))),
					tfresource.TestCheckResourceAttr(addr, "truncated", "false"),
				),
			},
			{
				// An in-place update produces a diff that carries the prior
				// output; it must only appear under sensitive attributes.
				Config: config("20s"),
				ConfigPlanChecks: tfresource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(addr, plancheck.ResourceActionUpdate),
						plancheck.ExpectSensitiveValue(addr, tfjsonpath.New("sensitive_stdout")),
						plancheck.ExpectSensitiveValue(addr, tfjsonpath.New("sensitive_stderr")),
						plancheck.ExpectKnownValue(addr, tfjsonpath.New("stdout_sha256"), knownvalue.StringExact(sha256Hex([]byte(secret+"\n")))),
						expectSecretOnlyInSensitive{addr: addr, secret: secret},
					},
				},
			},
		},
	})
}

// TestAccExec_sensitiveOutputFailureDiagnostic checks that a failing command
// with sensitive_output does not leak its output into the error.
func TestAccExec_sensitiveOutputFailureDiagnostic(t *testing.T) {
	requireRoot(t)

	tfresource.Test(t, tfresource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []tfresource.TestStep{
			{
				Config: `
resource "sysutils_exec" "secret" {
  command          = ["/bin/sh", "-c", "echo leaked-secret; exit 2"]
  sensitive_output = true
}`,
				// TestDescribeOutputSensitive checks that the output itself is
				// absent from this diagnostic.
				ExpectError: regexp.MustCompile(`non-zero\s+status\s+2[\s\S]*Output\s+withheld\s+because\s+sensitive_output\s+is\s+true\.\s+stdout_sha256:\s+` +
					sha256Hex([]byte("leaked-secret\n"))),
			},
		},
	})
}

// TestAccExec_truncation checks that output beyond max_output_bytes is cut,
// marked, and flagged, while the hash still covers the complete output.
func TestAccExec_truncation(t *testing.T) {
	requireRoot(t)

	full := strings.Repeat("0123456789", 10) + "\n"
	tfresource.Test(t, tfresource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []tfresource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "sysutils_exec" "big" {
  command          = ["/bin/sh", "-c", "printf '%%s' \"$0\"; echo short >&2", %q]
  max_output_bytes = 25
}

resource "sysutils_exec" "default_cap" {
  command = ["/bin/sh", "-c", "printf '%%s' \"$0\"", %q]
}`, full, full),
				Check: tfresource.ComposeAggregateTestCheckFunc(
					tfresource.TestCheckResourceAttr("sysutils_exec.big", "stdout",
						"0123456789012345678901234\n[sysutils_exec: output truncated, kept 25 of 101 bytes]\n"),
					tfresource.TestCheckResourceAttr("sysutils_exec.big", "stderr", "short\n"),
					tfresource.TestCheckResourceAttr("sysutils_exec.big", "truncated", "true"),
					tfresource.TestCheckResourceAttr("sysutils_exec.big", "stdout_sha256", sha256Hex([]byte(full))),
					tfresource.TestCheckResourceAttr("sysutils_exec.big", "stderr_sha256", sha256Hex([]byte("short\n"))),
					tfresource.TestCheckResourceAttr("sysutils_exec.default_cap", "stdout", full),
					tfresource.TestCheckResourceAttr("sysutils_exec.default_cap", "truncated", "false"),
					tfresource.TestCheckResourceAttr("sysutils_exec.default_cap", "max_output_bytes", "1048576"),
				),
			},
		},
	})
}

// TestAccExec_truncationDefaultCap checks the 1 MiB default with real volume.
func TestAccExec_truncationDefaultCap(t *testing.T) {
	requireRoot(t)

	tfresource.Test(t, tfresource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []tfresource.TestStep{
			{
				Config: `
resource "sysutils_exec" "huge" {
  command = ["/bin/sh", "-c", "head -c 3145728 /dev/zero | tr '\\0' 'x'"]
}`,
				Check: tfresource.ComposeAggregateTestCheckFunc(
					tfresource.TestCheckResourceAttr("sysutils_exec.huge", "truncated", "true"),
					tfresource.TestCheckResourceAttr("sysutils_exec.huge", "stdout",
						strings.Repeat("x", 1<<20)+"\n[sysutils_exec: output truncated, kept 1048576 of 3145728 bytes]\n"),
					tfresource.TestCheckResourceAttr("sysutils_exec.huge", "stdout_sha256", sha256Hex([]byte(strings.Repeat("x", 3<<20)))),
				),
			},
		},
	})
}

// TestExecStateUpgradeV0 checks that version 0 state gains the defaults a new
// resource would get, so the upgrade does not plan a replacement.
func TestExecStateUpgradeV0(t *testing.T) {
	ctx := context.Background()
	r := &execResource{}
	upgrader := r.UpgradeState(ctx)[0]

	priorType := upgrader.PriorSchema.Type().TerraformType(ctx)
	str := func(s string) tftypes.Value { return tftypes.NewValue(tftypes.String, s) }
	strList := tftypes.List{ElementType: tftypes.String}
	strMap := tftypes.Map{ElementType: tftypes.String}
	prior := tftypes.NewValue(priorType, map[string]tftypes.Value{
		"command":                    tftypes.NewValue(strList, []tftypes.Value{str("/bin/echo"), str("hi")}),
		"environment":                tftypes.NewValue(strMap, nil),
		"inherit_parent_environment": tftypes.NewValue(tftypes.Bool, true),
		"working_directory":          tftypes.NewValue(tftypes.String, nil),
		"stdin":                      tftypes.NewValue(tftypes.String, nil),
		"triggers":                   tftypes.NewValue(strMap, nil),
		"fail_on_nonzero":            tftypes.NewValue(tftypes.Bool, true),
		"exit_code":                  tftypes.NewValue(tftypes.Number, 0),
		"stdout":                     str("hi\n"),
		"stderr":                     str(""),
		"id":                         str("123-abc"),
	})

	var schemaResp resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)
	req := resource.UpgradeStateRequest{State: &tfsdk.State{Schema: *upgrader.PriorSchema, Raw: prior}}
	resp := resource.UpgradeStateResponse{State: tfsdk.State{Schema: schemaResp.Schema}}
	upgrader.StateUpgrader(ctx, req, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("upgrade diagnostics: %v", resp.Diagnostics)
	}

	var got execModel
	if diags := resp.State.Get(ctx, &got); diags.HasError() {
		t.Fatalf("reading upgraded state: %v", diags)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"stdout", got.Stdout.ValueString(), "hi\n"},
		{"id", got.ID.ValueString(), "123-abc"},
		{"sensitive_output", got.SensitiveOutput.ValueBool(), false},
		{"max_output_bytes", got.MaxOutputBytes.ValueInt64(), int64(defaultMaxOutputBytes)},
		{"truncated", got.Truncated.ValueBool(), false},
		{"timeout null", got.Timeout.IsNull(), true},
		{"destroy_command null", got.DestroyCommand.IsNull(), true},
		{"sensitive_stdout null", got.SensitiveStdout.IsNull(), true},
		{"stdout_sha256", got.StdoutSHA256.ValueString(), sha256Hex([]byte("hi\n"))},
		{"stderr_sha256", got.StderrSHA256.ValueString(), sha256Hex(nil)},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// expectSecretOnlyInSensitive is a plan check asserting that, in both the
// prior and planned values of a resource, any top-level attribute whose value
// contains secret is marked sensitive (and therefore redacted in the diff).
type expectSecretOnlyInSensitive struct {
	addr, secret string
}

func (e expectSecretOnlyInSensitive) CheckPlan(_ context.Context, req plancheck.CheckPlanRequest, resp *plancheck.CheckPlanResponse) {
	for _, rc := range req.Plan.ResourceChanges {
		if rc.Address != e.addr {
			continue
		}
		sides := []struct {
			name      string
			values    any
			sensitive any
		}{
			{"before", rc.Change.Before, rc.Change.BeforeSensitive},
			{"after", rc.Change.After, rc.Change.AfterSensitive},
		}
		found := false
		for _, side := range sides {
			values, _ := side.values.(map[string]any)
			sensitive, _ := side.sensitive.(map[string]any)
			for key, v := range values {
				s, ok := v.(string)
				if !ok || !strings.Contains(s, e.secret) {
					continue
				}
				found = true
				if marked, _ := sensitive[key].(bool); !marked {
					resp.Error = fmt.Errorf("%s: %s.%s contains the secret but is not sensitive", e.addr, side.name, key)
					return
				}
			}
		}
		if !found {
			resp.Error = fmt.Errorf("%s: secret not found in any attribute of the plan; the check is not exercising anything", e.addr)
		}
		return
	}
	resp.Error = fmt.Errorf("%s not found in plan", e.addr)
}

func checkFileAbsent(p string) tfresource.TestCheckFunc {
	return func(*terraform.State) error {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			return fmt.Errorf("%s exists (or cannot be checked: %v); want absent", p, err)
		}
		return nil
	}
}
