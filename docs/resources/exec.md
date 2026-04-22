---
page_title: "sysutils_exec Resource - terraform-provider-sysutils"
subcategory: ""
description: |-
  Executes a command and captures exit code, stdout, and stderr.
---

# sysutils_exec (Resource)

Runs a command when the resource is created and again whenever any input attribute changes. The exit code, standard output, and standard error are recorded as computed attributes you can reference from other resources or outputs.

Every input attribute is marked `RequiresReplace`, so changing `command`, `environment`, `triggers`, or any other argument destroys the resource and recreates it (which re-runs the command). This is the same pattern used by `null_resource`.

The resource has no meaningful destroy action — the command already ran at create time, and deleting the resource simply drops the captured results from state.

## Example Usage

### Run a command using the provider's environment

```terraform
resource "sysutils_exec" "version" {
  command = ["/usr/local/bin/my-tool", "--version"]
}

output "tool_version" {
  value = sysutils_exec.version.stdout
}
```

### Run a command with a completely custom environment

Setting `inherit_parent_environment = false` means the child process sees only the variables from the `environment` map — not `PATH`, not `HOME`, nothing else:

```terraform
resource "sysutils_exec" "isolated" {
  command                    = ["/usr/bin/env"]
  inherit_parent_environment = false
  environment = {
    FOO = "bar"
    BAZ = "qux"
  }
}
```

### Force a re-run using triggers

```terraform
resource "sysutils_exec" "migrate" {
  command = ["/usr/local/bin/my-migrate", "--apply"]
  triggers = {
    schema_version = "3"
  }
}
```

Bumping `schema_version` causes the next apply to destroy and recreate the resource, re-executing the command.

### Pipe data into stdin and tolerate non-zero exits

```terraform
resource "sysutils_exec" "probe" {
  command         = ["/bin/sh", "-c", "cat; grep -q needle || exit 2"]
  stdin           = "haystack\nhaystack\n"
  fail_on_nonzero = false
}

output "probe_exit"   { value = sysutils_exec.probe.exit_code }
output "probe_stderr" { value = sysutils_exec.probe.stderr }
```

## Schema

### Required

- `command` (List of String) — Command and arguments as a list. Element `0` is the executable. Example: `["/bin/sh", "-c", "echo hi"]`.

### Optional

- `environment` (Map of String) — Environment variables to pass to the child.
- `inherit_parent_environment` (Boolean) — If `true` (default), the child process starts from the Terraform provider's environment and the keys in `environment` override specific values. If `false`, only the `environment` map is used.
- `working_directory` (String) — Working directory for the child process.
- `stdin` (String) — Data to pipe into the command's standard input.
- `triggers` (Map of String) — Arbitrary map whose changes force re-execution. The values are not passed to the command; they exist solely to invalidate the resource.
- `fail_on_nonzero` (Boolean) — If `true` (default), a non-zero exit code causes the apply to fail with the captured stderr in the diagnostic. If `false`, the exit code is recorded and the apply continues.

### Read-Only

- `exit_code` (Number) — Exit code returned by the command.
- `stdout` (String) — Captured standard output.
- `stderr` (String) — Captured standard error.
- `id` (String) — Opaque resource identifier.

## Caveats

- The command runs on the machine Terraform is executing on, not on any remote host. Use a provisioner or a dedicated SSH/WinRM provider for remote execution.
- `stdout` and `stderr` are stored in Terraform state. Avoid printing secrets. If you must, use a state backend with encryption-at-rest.
- Because every input forces replacement, Terraform will report a diff whenever any attribute changes — there is no in-place update.
