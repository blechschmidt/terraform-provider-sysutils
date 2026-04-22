package provider

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

func TestAccExec_basic(t *testing.T) {
	requireRoot(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "sysutils_exec" "echo" {
  command = ["/bin/sh", "-c", "echo hi; echo err 1>&2; exit 0"]
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("sysutils_exec.echo", "exit_code", "0"),
					resource.TestCheckResourceAttr("sysutils_exec.echo", "stdout", "hi\n"),
					resource.TestCheckResourceAttr("sysutils_exec.echo", "stderr", "err\n"),
				),
			},
		},
	})
}

func TestAccExec_nonzeroFails(t *testing.T) {
	requireRoot(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "sysutils_exec" "fail" {
  command = ["/bin/sh", "-c", "exit 3"]
}`,
				ExpectError: regexp.MustCompile(`non-zero status 3`),
			},
		},
	})
}

func TestAccExec_nonzeroCapturedWhenAllowed(t *testing.T) {
	requireRoot(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "sysutils_exec" "captured" {
  command         = ["/bin/sh", "-c", "echo out; echo err 1>&2; exit 7"]
  fail_on_nonzero = false
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("sysutils_exec.captured", "exit_code", "7"),
					resource.TestCheckResourceAttr("sysutils_exec.captured", "stdout", "out\n"),
					resource.TestCheckResourceAttr("sysutils_exec.captured", "stderr", "err\n"),
				),
			},
		},
	})
}

// Isolated environment: only the variables we set should be present.
func TestAccExec_isolatedEnvironment(t *testing.T) {
	requireRoot(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "sysutils_exec" "isolated" {
  command                    = ["/usr/bin/env"]
  inherit_parent_environment = false
  environment = {
    FOO = "bar"
    BAZ = "qux"
  }
}`,
				Check: checkExecStdoutLines("sysutils_exec.isolated", []string{"BAZ=qux", "FOO=bar"}),
			},
		},
	})
}

// Inherited environment with overrides: parent vars are present and specific
// keys are replaced by the environment map.
func TestAccExec_inheritedEnvironmentOverride(t *testing.T) {
	requireRoot(t)

	t.Setenv("TF_SYSUTILS_FROM_PARENT", "present")
	t.Setenv("TF_SYSUTILS_OVERRIDE_ME", "original")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "sysutils_exec" "inherited" {
  command = ["/usr/bin/env"]
  environment = {
    TF_SYSUTILS_OVERRIDE_ME = "replaced"
    TF_SYSUTILS_NEW         = "added"
  }
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestMatchResourceAttr("sysutils_exec.inherited", "stdout", regexp.MustCompile(`(?m)^TF_SYSUTILS_FROM_PARENT=present$`)),
					resource.TestMatchResourceAttr("sysutils_exec.inherited", "stdout", regexp.MustCompile(`(?m)^TF_SYSUTILS_OVERRIDE_ME=replaced$`)),
					resource.TestMatchResourceAttr("sysutils_exec.inherited", "stdout", regexp.MustCompile(`(?m)^TF_SYSUTILS_NEW=added$`)),
				),
			},
		},
	})
}

// checkExecStdoutLines asserts that the exec resource's stdout, once split on
// newlines and sorted, equals wantSorted exactly. Used to verify isolated
// environments (where parent env vars must be absent).
func checkExecStdoutLines(resourceName string, wantSorted []string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[resourceName]
		if !ok {
			return fmt.Errorf("resource %s not found in state", resourceName)
		}
		stdout := rs.Primary.Attributes["stdout"]
		lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
		sort.Strings(lines)
		if !equalStringSlices(lines, wantSorted) {
			return fmt.Errorf("stdout lines mismatch:\n got: %q\nwant: %q", lines, wantSorted)
		}
		return nil
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestAccExec_stdin(t *testing.T) {
	requireRoot(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "sysutils_exec" "cat" {
  command = ["/bin/cat"]
  stdin   = "piped-in\n"
}`,
				Check: resource.TestCheckResourceAttr("sysutils_exec.cat", "stdout", "piped-in\n"),
			},
		},
	})
}
