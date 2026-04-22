---
page_title: "sysutils_file Resource - terraform-provider-sysutils"
subcategory: ""
description: |-
  Writes a file to a specific location on the local filesystem.
---

# sysutils_file (Resource)

Writes a file at `path` with the given `content`. On create the provider also sets the requested `mode`, and optionally `owner` / `group` (which require privileges). On update, content, mode, and ownership are reconciled in-place. On destroy, the file is removed.

Parent directories are created with mode `0755` if they do not already exist.

## Example Usage

```terraform
resource "sysutils_file" "hello" {
  path    = "/etc/hello.conf"
  content = "greeting = hi\n"
  mode    = "0644"
  owner   = "root"
  group   = "root"
}
```

## Schema

### Required

- `path` (String) — Absolute path where the file should be written. Changing this forces a new resource.
- `content` (String) — File contents.

### Optional

- `mode` (String) — Octal file mode such as `"0644"` or `"0600"`. Defaults to `"0644"` on create.
- `owner` (String) — Username that should own the file. Requires privileges to change.
- `group` (String) — Group that should own the file. Requires privileges to change.

### Read-Only

- `id` (String) — Resource identifier (equal to `path`).

## Import

An existing file can be imported using its absolute path:

```sh
terraform import sysutils_file.hello /etc/hello.conf
```

After import, the next plan will show the file's current contents being adopted into state.

## Drift Detection

On refresh, the provider re-reads the file and updates `content` and `mode` in state. If the file has been deleted out of band it is removed from state and will be recreated on the next apply. Ownership drift is only reported for the attributes you have explicitly set (`owner` / `group`); attributes left null are ignored to avoid spurious diffs against host defaults.

## Caveats

- Content is stored verbatim in Terraform state. Do not use this resource for secrets unless your state backend is encrypted and access-controlled.
- The resource only works on Unix-like systems.
