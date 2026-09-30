---
page_title: "Provisioning a service account and its files"
subcategory: "Cookbook"
description: |-
  End-to-end example: a service user and group, a directory tree with the right ownership, a configuration file and a symlink to the active release.
---

# Provisioning a service account and its files

Most services need the same groundwork before they can start: an unprivileged account, a place for their code, configuration, state and logs, and permissions that let the service read what it needs without being able to rewrite it. This guide builds that groundwork for a hypothetical service called `myapp` in one configuration. It shows how the resources fit together and why the ownership and modes are chosen the way they are.

The configuration must be applied as root: it creates an account and assigns ownership to it.

## What gets created

| Path or object | Owner:group | Mode | Why |
|----------------|-------------|------|-----|
| group `myapp` | | | Primary group of the service; grants read access to code and configuration. |
| user `myapp` | | | System account with no login shell and no password. |
| `/opt/myapp`, `/opt/myapp/releases` | `root:myapp` | `0750` | Code. The service can read and execute it but not modify it. |
| `/opt/myapp/current` | | symlink | Stable path to the active release, swapped atomically on upgrade. |
| `/etc/myapp` | `root:myapp` | `0750` | Configuration directory, not readable by other users. |
| `/etc/myapp/myapp.toml` | `root:myapp` | `0640` | Configuration. Owned by root so a compromised service cannot change it. |
| `/var/lib/myapp` | `myapp:myapp` | `0700` | Home and state directory; only the service can access it. |
| `/var/log/myapp` | `myapp:myapp` | `2750` | Logs. The setgid bit gives new log files the `myapp` group, so members of the group can read them. |
| group `myapp-admins` | | | Operators of the service. |
| `/etc/sudoers.d/50-myapp-admins` | `root:root` | `0440` | Lets operators restart the service and read its logs through sudo, and nothing else. |

## Configuration

```terraform
terraform {
  required_providers {
    sysutils = {
      source = "blechschmidt/sysutils"
    }
  }
}

provider "sysutils" {}

variable "release" {
  description = "Version under /opt/myapp/releases that /opt/myapp/current points at."
  type        = string
  default     = "1.4.2"
}

# 1. Identity: a system group and a system user whose primary group it is.
#    Referencing the gid makes Terraform create the group before the user and
#    delete it after the user.
resource "sysutils_group" "myapp" {
  name   = "myapp"
  system = true
}

resource "sysutils_user" "myapp" {
  name        = "myapp"
  gid         = sysutils_group.myapp.gid
  system      = true
  comment     = "myapp service account"
  home        = "/var/lib/myapp"
  shell       = "/usr/sbin/nologin"
  create_home = false # the directory is managed below
}

# 2. Directory tree. Code and configuration belong to root and are only
#    readable by the service's group; state and logs belong to the service.
resource "sysutils_directory" "install" {
  path  = "/opt/myapp"
  owner = "root"
  group = sysutils_group.myapp.name
  mode  = "0750"
}

# Releases are unpacked here by your deployment tooling, one subdirectory
# per version. Their contents are not managed by Terraform.
resource "sysutils_directory" "releases" {
  path  = "${sysutils_directory.install.path}/releases"
  owner = "root"
  group = sysutils_group.myapp.name
  mode  = "0750"
}

resource "sysutils_directory" "config" {
  path  = "/etc/myapp"
  owner = "root"
  group = sysutils_group.myapp.name
  mode  = "0750"
}

resource "sysutils_directory" "state" {
  path  = sysutils_user.myapp.home
  owner = sysutils_user.myapp.name
  group = sysutils_group.myapp.name
  mode  = "0700"

  # Keep ownership correct even for files the service or an operator
  # created by hand, and report drift anywhere in the tree.
  recursive_owner = true

  # The service writes its own data here; remove it along with the account.
  force_destroy = true
}

resource "sysutils_directory" "logs" {
  path  = "/var/log/myapp"
  owner = sysutils_user.myapp.name
  group = sysutils_group.myapp.name
  mode  = "2750" # setgid: new log files inherit the myapp group
}

# 3. Configuration file: owned by root so the service cannot rewrite it,
#    readable by the service through its group.
resource "sysutils_file" "config" {
  path    = "${sysutils_directory.config.path}/myapp.toml"
  owner   = "root"
  group   = sysutils_group.myapp.name
  mode    = "0640"
  content = <<-EOT
    listen    = "127.0.0.1:8080"
    data_dir  = "${sysutils_directory.state.path}"
    log_dir   = "${sysutils_directory.logs.path}"
  EOT
}

# 4. Stable path for the active release. Changing var.release swaps the link
#    atomically, so the service never sees a missing or half-updated path.
#    The target does not have to exist yet.
resource "sysutils_symlink" "current" {
  path   = "${sysutils_directory.install.path}/current"
  target = "releases/${var.release}" # relative to /opt/myapp
}

output "uid" {
  value = sysutils_user.myapp.uid
}

output "config_sha256" {
  value = sysutils_file.config.content_sha256
}
```

### Letting operators restart the service

Operators need to restart the service and read its logs, but not a root shell. A [`sysutils_sudoers`](../resources/sudoers.md) drop-in grants exactly those commands to a group of their own:

```terraform
# 5. Operators: members of myapp-admins may restart the service and read its
#    status and logs as root, without a password, and nothing else. visudo
#    checks the file before it is installed.
resource "sysutils_group" "myapp_admins" {
  name = "myapp-admins"
}

resource "sysutils_sudoers" "myapp_admins" {
  name = "50-myapp-admins"

  rules = [{
    users    = ["%${sysutils_group.myapp_admins.name}"]
    runas    = "root"
    nopasswd = true
    commands = [
      "/usr/bin/systemctl restart myapp.service",
      "/usr/bin/systemctl status myapp.service",
      "/usr/bin/journalctl -u myapp.service",
    ]
  }]
}
```

Before the file is renamed into place, `visudo -cf` checks it, so a mistake fails the apply with visudo's message instead of breaking sudo for everyone. The name `50-myapp-admins` contains no `.`: sudo silently skips files in `/etc/sudoers.d` whose names do, and the resource refuses such names at plan time. Add operators with a [`sysutils_group`](../resources/group.md) `members` list or `sysutils_user` `groups`.

## How it works

**Ordering comes from references.** The user refers to `sysutils_group.myapp.gid`, the directories refer to the user and group names, and the file and symlink refer to directory paths. Terraform therefore creates the group, then the user, then the directories, then the file, and destroys them in reverse order. There is no need for `depends_on`.

**The home directory is managed as a directory, not by `useradd`.** `create_home = false` stops `useradd` from creating `/var/lib/myapp` with its default mode and a copy of `/etc/skel`. `sysutils_directory.state` creates it instead, with an explicit mode. Its `path` refers to `sysutils_user.myapp.home`, so the two cannot drift apart.

**Ownership is enforced across the state directory.** `recursive_owner = true` gives every entry below `/var/lib/myapp` the directory's owner and group, like `chown -R`. Files an operator created there as root are reported at the next plan, as a change to `nonconforming_entries`, and fixed by the next apply. `force_destroy = true` lets `terraform destroy` remove the directory even though the service has written data into it. Leave it out if that data must outlive the account.

**Intermediate directories are created on demand.** `/opt/myapp/releases` is managed explicitly because its permissions matter. Missing parents of any managed path are created with mode `0755` and are not removed on destroy.

**Upgrading is a one-variable change.** Your deployment tooling unpacks each version into `/opt/myapp/releases/<version>`. Running `terraform apply -var release=1.5.0` then updates `sysutils_symlink.current` in place. The link is replaced with a single `rename`, so the service never sees a missing or half-updated `/opt/myapp/current`. The target does not have to exist when the link is created.

## Checking for drift

Run `terraform plan` at any time to compare the host with the configuration. For example, after someone runs `chmod 644 /etc/myapp/myapp.toml` and drops a root-owned file into `/var/lib/myapp`, the plan shows:

```text
  ~ resource "sysutils_directory" "state" {
      ~ nonconforming_entries = 1 -> 0
  ~ resource "sysutils_file" "config" {
      ~ mode           = "0644" -> "0640"
Plan: 0 to add, 2 to change, 0 to destroy.
```

`terraform apply` restores the configured state. Changes to the configuration file's content or owner, to the group's gid, and to the user's uid, shell, home or primary group are detected the same way.

## Adopting an existing installation

If the account and directories already exist, import them instead of recreating them. Each resource is imported by its name or path:

```shell
terraform import sysutils_group.myapp myapp
terraform import sysutils_user.myapp myapp
terraform import sysutils_directory.install /opt/myapp
terraform import sysutils_directory.releases /opt/myapp/releases
terraform import sysutils_directory.config /etc/myapp
terraform import sysutils_directory.state /var/lib/myapp
terraform import sysutils_directory.logs /var/log/myapp
terraform import sysutils_file.config /etc/myapp/myapp.toml
terraform import sysutils_symlink.current /opt/myapp/current
terraform import sysutils_group.myapp_admins myapp-admins
terraform import sysutils_sudoers.myapp_admins 50-myapp-admins
```

Then run `terraform plan`. It lists every difference between what is on disk and this configuration, and nothing is changed until you apply. Options that describe how Terraform manages a resource rather than what is on disk, such as `force_destroy` and `recursive_owner`, are set to their defaults on import, and the group's `system` flag cannot be read back from `/etc/group`, so expect them to show up as in-place updates.

## Next steps

- Restart or reload the service when its configuration changes with a [`sysutils_exec`](../resources/exec.md) resource whose `triggers` include `sysutils_file.config.content_sha256`.
- Add the service's host entries or a single setting to a shared file such as `/etc/hosts` with [`sysutils_file_line`](../resources/file_line.md).
- Read the provider's [security model](../index.md#security-model) before managing paths inside directories that other users can write to.
