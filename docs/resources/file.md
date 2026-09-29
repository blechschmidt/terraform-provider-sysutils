---
page_title: "sysutils_file Resource - terraform-provider-sysutils"
subcategory: ""
description: |-
  Writes a file to a specific location on the local filesystem.
---

# sysutils_file (Resource)

Writes a file at `path` with the given `content`. On create the provider also sets the requested `mode`, and optionally `owner` / `group` (which require privileges). On update, content, mode, and ownership are reconciled in-place. On destroy, the file is removed.

Parent directories are created with mode `0755` if they do not already exist. To control the mode or ownership of the containing directory, manage it with [`sysutils_directory`](./directory.md) and reference its `path`.

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

- `path` (String) — Absolute path where the file should be written. Must be in canonical form (no `.`/`..` segments, duplicate or trailing slashes) and must not be `/`. Changing this forces a new resource.
- `content` (String) — File contents. Must be valid UTF-8 text.

### Optional

- `mode` (String) — Octal mode with 3 or 4 digits, optionally with a leading zero, such as `"0644"`, `"600"` or `"4755"`. The mode is applied with an explicit `chmod`, so the process umask does not affect it. Defaults to `"0644"`.
- `owner` (String) — Username or numeric UID that should own the file. Requires privileges to change. If unset, the owner assigned at creation (normally the user running Terraform) is kept.
- `group` (String) — Group name or numeric GID of the file. Requires privileges to change. If unset, the group assigned at creation is kept.

### Read-Only

- `id` (String) — Resource identifier (equal to `path`).

## Import

An existing file can be imported using its absolute path:

```sh
terraform import sysutils_file.hello /etc/hello.conf
```

The file's current content, mode, owner and group are read into state. The next plan shows any differences from your configuration — typically the content — and the following apply overwrites the file to match. Only UTF-8 text files can be imported; importing a binary file fails.

## Drift Detection

On refresh, the provider re-reads the file's content, mode, owner and group. If any of them was changed outside Terraform, the next plan shows the difference and apply restores the configured value. If `owner` or `group` is not set in the configuration, its current value is recorded in state but no change is ever planned for it.

Equivalent spellings are not reported as drift: a configured mode of `"644"` matches `0644` on disk, and a configured numeric UID or GID matches the corresponding name.

If the file has been deleted, it is removed from state and recreated on the next apply. If something other than a regular file (such as a directory or a symlink) now exists at `path`, refresh fails with an error rather than replacing or following it.

## Caveats

- Content is stored verbatim in Terraform state. Do not use this resource for secrets unless your state backend is encrypted and access-controlled.
- The file is rewritten in place, not atomically; a process reading it during an apply may see partial content. Ownership and mode are applied before the new content is written.
- A symlink at `path` is never followed. If `path` is a symlink, create, update and refresh fail rather than writing to, or reading, whatever it points to. The file is opened with `O_NOFOLLOW`, and its mode and ownership are changed through that descriptor. If the path has been replaced by a symlink at destroy time, destroy removes only the symlink.
- FIFOs, sockets, devices and directories at `path` are refused as "not a regular file".
- Symlinks in the *parent* components of `path` are followed. Every ancestor directory must be writable only by trusted users; see the provider's [security model](../index.md#security-model).
- The resource only works on Unix-like systems.
