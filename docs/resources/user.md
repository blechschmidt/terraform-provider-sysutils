---
page_title: "sysutils_user Resource - terraform-provider-sysutils"
subcategory: ""
description: |-
  Manages a local Linux user via useradd/usermod/userdel.
---

# sysutils_user (Resource)

Manages a local user account by shelling out to `useradd` (create), `usermod` (update), and `userdel` (delete). Requires root privileges. Password management is intentionally not supported — use a separate mechanism if you need it.

## Example Usage

```terraform
# A system user that owns a long-running service.
resource "sysutils_user" "app" {
  name        = "appsvc"
  system      = true
  shell       = "/usr/sbin/nologin"
  create_home = false
  groups      = ["adm"]
}

# A regular user with a fixed UID and home directory.
resource "sysutils_user" "alice" {
  name        = "alice"
  uid         = 2001
  home        = "/home/alice"
  shell       = "/bin/bash"
  create_home = true
}
```

## Schema

### Required

- `name` (String) — Username. Changing this forces a new resource.

### Optional

- `uid` (Number) — Numeric user ID. Assigned by the system if unset.
- `gid` (Number) — Primary numeric group ID.
- `home` (String) — Home directory path.
- `shell` (String) — Login shell (for example `/bin/bash` or `/usr/sbin/nologin`).
- `comment` (String) — Value for the GECOS comment field in `/etc/passwd`.
- `system` (Boolean) — Create as a system account (`useradd -r`). Only honored at create time.
- `create_home` (Boolean) — If `true`, pass `-m` to `useradd` to create the home directory. Defaults to `false` (i.e. `-M`).
- `groups` (Set of String) — Supplementary group names. Replaces the current supplementary-group set on update.

### Read-Only

- `id` (String) — Resource identifier (equal to `name`).

## Import

```sh
terraform import sysutils_user.alice alice
```

After import, the first plan will populate the computed attributes (`uid`, `gid`, `home`, `shell`) from the passwd database.

## Caveats

- Requires root privileges. Running as a non-root user will fail at `useradd`.
- Only manages supplementary groups when `groups` is set. If `groups` is null, the provider will not compare or reconcile membership — it will leave whatever is in `/etc/group` alone to avoid fighting other systems that manage group membership.
- Does not manage passwords, SSH keys, sudoers entries, or home-directory contents. Pair with `sysutils_file` if you need an `authorized_keys` file, for example.
