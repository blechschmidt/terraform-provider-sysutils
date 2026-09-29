---
page_title: "sysutils_directory Resource - terraform-provider-sysutils"
subcategory: ""
description: |-
  Manages a directory on the local filesystem, including its mode and ownership.
---

# sysutils_directory (Resource)

Ensures a directory exists at `path` with the requested `mode` and, optionally, `owner` / `group` (which require privileges). On update, mode and ownership are reconciled in place. On destroy, the directory is removed — but only if it is empty, unless `force_destroy` is set.

Use this resource when a directory's permissions matter in their own right — for example a service's data directory that must be owned by a dedicated user and not be world-readable. Files placed in it with [`sysutils_file`](./file.md) create missing parents themselves, but those implicit parents always get mode `0755` and root/current-user ownership, and they are never cleaned up on destroy.

If a directory already exists at `path` when the resource is created, it is adopted: its mode and ownership are changed to match the configuration and it becomes managed by Terraform (and will be removed on destroy).

## Example Usage

### A service directory with a config file inside it

```terraform
resource "sysutils_directory" "app" {
  path  = "/srv/app"
  owner = "appsvc"
  group = "appsvc"
  mode  = "0750"
}

resource "sysutils_file" "app_config" {
  # Referencing the directory's path makes Terraform create the directory
  # first and remove the file before the directory on destroy.
  path    = "${sysutils_directory.app.path}/app.conf"
  content = "listen = 127.0.0.1:8080\n"
  mode    = "0640"
  owner   = "appsvc"
  group   = "appsvc"
}
```

### A shared scratch directory with the sticky bit

```terraform
resource "sysutils_directory" "scratch" {
  path          = "/var/lib/scratch"
  mode          = "1777"
  force_destroy = true
}
```

## Schema

### Required

- `path` (String) — Absolute path of the directory. Must be in canonical form (no `.`/`..` segments, duplicate or trailing slashes) and must not be `/`. Changing this forces a new resource.

### Optional

- `mode` (String) — Octal mode with 3 or 4 digits, optionally with a leading zero, such as `"0755"`, `"750"` or `"1777"`. Setuid, setgid and sticky bits are supported. The mode is applied with an explicit `chmod`, so the process umask does not affect it. Defaults to `"0755"`.
- `owner` (String) — Username or numeric UID that should own the directory. Requires privileges to change. If unset, the owner assigned at creation (normally the user running Terraform) is kept.
- `group` (String) — Group name or numeric GID of the directory. Requires privileges to change. If unset, the group assigned at creation is kept.
- `create_parents` (Boolean) — Create missing parent directories, like `mkdir -p`. Parents created this way get mode `0755` (subject to the umask) and are not removed on destroy. When `false`, creation fails if the parent does not exist. Defaults to `true`.
- `force_destroy` (Boolean) — Delete the directory and everything in it on destroy. When `false`, destroying a non-empty directory fails with an error. Cannot be enabled for protected system directories such as `/etc`, `/usr` or `/home`. Defaults to `false`.

### Read-Only

- `id` (String) — Resource identifier (equal to `path`).

## Import

An existing directory can be imported using its absolute path:

```sh
terraform import sysutils_directory.app /srv/app
```

The directory's current mode, owner and group are read into state; `create_parents` and `force_destroy` are set to their defaults. The next plan shows any differences between the directory on disk and your configuration, and the following apply corrects them.

## Drift Detection

On refresh, the provider re-reads the directory's mode, owner and group. If any of them was changed outside Terraform, the next plan shows the difference and apply restores the configured value. If `owner` or `group` is not set in the configuration, its current value is recorded in state but no change is ever planned for it.

Equivalent spellings are not reported as drift: a configured mode of `"755"` matches `0755` on disk, and a configured numeric UID or GID matches the corresponding name.

If the directory has been deleted, it is removed from state and recreated on the next apply. If something other than a directory now exists at `path`, including a symlink to a directory, refresh fails with an error rather than replacing or following it.

## Caveats

- `force_destroy = true` deletes the whole tree, including files that are not managed by Terraform. Enable it only for directories whose contents are disposable. To destroy a non-empty directory, first set `force_destroy = true` and apply, then destroy — the setting that counts is the one stored in state.
- A symlink at `path` is never followed. If `path` is a symlink, even one pointing to a directory, create and refresh fail rather than changing the mode or ownership of whatever it points to. Manage the real directory path instead. Mode and ownership are changed through a descriptor opened with `O_NOFOLLOW`, so the directory cannot be swapped for a symlink between the check and the change.
- Symlinks in the *parent* components of `path` are followed for create, update and non-recursive destroy. They are refused for `force_destroy`: if any component of `path` is a symlink, the recursive destroy fails and nothing is deleted.
- Recursive deletion never follows symlinks inside the tree; they are unlinked, and what they point to is untouched. It never descends into a filesystem mounted below `path`. If it encounters a mount point, it stops with an error.
- If the directory has been replaced by a file or symlink by the time of destroy, the replacement is left untouched and a warning is emitted.
- See the provider's [security model](../index.md#security-model).
- The resource only works on Unix-like systems.
