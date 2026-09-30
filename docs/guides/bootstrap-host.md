---
page_title: "Cookbook: Bootstrapping a host"
subcategory: "Cookbook"
description: |-
  Turn a fresh machine into one your team can administer: base packages and services, a service restarted when its configuration changes, administrator accounts, their SSH keys and sudo rights.
---

# Cookbook: Bootstrapping a host

A freshly installed machine or cloud image has none of what your team expects. It lacks your base packages and running services configured your way, your administrators' accounts, their SSH keys and their sudo rights. This recipe sets all of that up in one configuration of about 110 lines, which you apply once to each new host and then again whenever the list of administrators changes. It uses [`sysutils_package`](../resources/package.md), the [`sysutils_host`](../data-sources/host.md) data source, [`sysutils_service`](../resources/service.md), [`sysutils_file_line`](../resources/file_line.md), the [`sysutils_service` action](../actions/service.md), [`sysutils_group`](../resources/group.md), [`sysutils_user`](../resources/user.md), [`sysutils_ssh_authorized_key`](../resources/ssh_authorized_key.md) and [`sysutils_file`](../resources/file.md).

Apply it as root on the host itself. The provider acts on the machine Terraform runs on, so run Terraform there, for example from cloud-init or over SSH as part of your provisioning. The complete configuration is in [`examples/guides/bootstrap-host`](https://github.com/blechschmidt/terraform-provider-sysutils/tree/main/examples/guides/bootstrap-host). CI validates it against the provider, like every snippet on this page.

## Provider setup

```terraform
terraform {
  # Actions, such as the sshd restart in sshd.tf, need Terraform 1.14.
  required_version = ">= 1.14"

  required_providers {
    sysutils = {
      source = "blechschmidt/sysutils"
    }
  }
}

provider "sysutils" {}
```

The configuration needs Terraform 1.14 or later for the [action](https://developer.hashicorp.com/terraform/language/block/action) that restarts chrony when its configuration changes. On older Terraform versions and on OpenTofu, which has no actions, use `restart_on_change` instead, as described [below](#without-actions).

## Base packages and services

```terraform
# The packages every host gets. The names are the same on Debian, Ubuntu,
# Fedora, RHEL and Alpine.
resource "sysutils_package" "base" {
  for_each = toset(["openssh-server", "sudo", "chrony", "curl"])

  name = each.key
  # Refresh the package index before installing, as a fresh image has none.
  # It is refreshed once per run, however many packages ask for it.
  update_cache = true
  # The host needs these even when this configuration no longer manages them.
  remove_on_destroy = false
}

# The service names differ between distributions: "ssh" and "chrony" on
# Debian and Ubuntu, "sshd" and "chronyd" on Fedora, RHEL and Alpine.
variable "services" {
  description = "Services to enable and start, by the package that installs them."
  type        = map(string)
  default = {
    openssh-server = "ssh"
    chrony         = "chrony"
  }
}

resource "sysutils_service" "base" {
  for_each = var.services

  name    = each.value
  enabled = true
  state   = "running"

  # A package's service only exists once the package is installed.
  depends_on = [sysutils_package.base]
}
```

**Why `update_cache`:** container and cloud images often ship without a package index, and then `apt-get install` fails with "unable to locate package". With `update_cache = true` the index is refreshed during apply, before the first installation. It happens only once per run, however many resources set it, and never during `terraform plan`, which leaves the host alone.

**Why `remove_on_destroy = false`:** by default, destroying a `sysutils_package` removes the package, and apt and dnf then also remove everything that depends on it. The base packages must outlive this configuration, and some of them, such as `openssh-server`, were probably already installed before Terraform touched the host. Only let destroy remove packages that you installed for one purpose and want gone with it.

**Why a variable for services:** packages have the same names on Debian, Ubuntu, Fedora, RHEL and Alpine, but their services do not. Set `services` for your distribution, for example `-var 'services={"openssh-server"="sshd","chrony"="chronyd"}'` on Fedora. `sysutils_service` detects whether systemd or OpenRC runs the host and uses `systemctl` or `rc-service`/`rc-update`. Destroying it changes nothing: the service keeps running.

`depends_on` is needed here because nothing in the service's arguments refers to the package. Without it, Terraform could try to start `chrony` before it is installed, and the apply would fail with "service not found".

## Distribution-specific packages

```terraform
# Some packages have different names on each distribution. Read the host's
# distribution from /etc/os-release and pick the names for its family.
data "sysutils_host" "this" {}

locals {
  # The distribution and those it derives from, such as
  # ["ubuntu", "debian"] or ["rocky", "rhel", "centos", "fedora"].
  distro_lineage = concat([data.sysutils_host.this.os_id], data.sysutils_host.this.os_id_like)

  distro_family = (
    contains(local.distro_lineage, "debian") ? "debian" :
    contains(local.distro_lineage, "fedora") || contains(local.distro_lineage, "rhel") ? "redhat" :
    contains(local.distro_lineage, "alpine") ? "alpine" :
    data.sysutils_host.this.os_id
  )

  # dig and a cron daemon, by distribution family.
  distro_packages = {
    debian = ["bind9-dnsutils", "cron"]
    redhat = ["bind-utils", "cronie"]
    alpine = ["bind-tools", "cronie"]
  }
}

resource "sysutils_package" "distro" {
  # Indexing rather than lookup() makes an unsupported distribution fail
  # the plan instead of silently installing nothing.
  for_each = toset(local.distro_packages[local.distro_family])

  name              = each.key
  update_cache      = true
  remove_on_destroy = false
}
```

**Why a distribution family:** some packages are named differently on each distribution. `dig` comes in `bind9-dnsutils` on Debian and Ubuntu, `bind-utils` on Fedora and RHEL and `bind-tools` on Alpine. The [`sysutils_host`](../data-sources/host.md) data source reads `/etc/os-release`. `os_id` is the distribution (`ubuntu`) and `os_id_like` the ones it derives from (`["debian"]`), so checking both matches Ubuntu, Linux Mint and Raspberry Pi OS as `debian`, and Rocky Linux and AlmaLinux as `redhat`.

**Why indexing instead of `lookup()`:** on a distribution the map doesn't list, `local.distro_packages[local.distro_family]` fails the plan with "The given key does not identify an element in this collection value". `lookup()` with an empty default would silently install nothing instead.

`sysutils_host` also reports `init_system` and `package_manager`, which you can use the same way, for example to create a systemd unit only where systemd is PID 1.

## Restarting a service when its configuration changes

```terraform
# The NTP pool every host synchronises with, instead of the distribution's.
variable "ntp_pool" {
  description = "NTP pool for chrony, such as a pool.ntp.org zone or your own time servers' DNS name."
  type        = string
  default     = "europe.pool.ntp.org"
}

locals {
  # chrony's configuration file, by distribution family.
  chrony_conf = {
    debian = "/etc/chrony/chrony.conf"
    redhat = "/etc/chrony.conf"
    alpine = "/etc/chrony/chrony.conf"
  }
}

# Replace the distribution's pool line, and restart chrony whenever the line
# is added or changed, so that the new pool is used right away.
resource "sysutils_file_line" "ntp_pool" {
  path   = local.chrony_conf[local.distro_family]
  line   = "pool ${var.ntp_pool} iburst"
  regexp = "^pool "

  # The file comes with the chrony package.
  depends_on = [sysutils_package.base]

  lifecycle {
    action_trigger {
      events  = [after_create, after_update]
      actions = [action.sysutils_service.restart_chrony]
    }
  }
}

action "sysutils_service" "restart_chrony" {
  config {
    # Referencing the service resource orders the restart after chrony has
    # been enabled and started.
    name   = sysutils_service.base["chrony"].name
    action = "restart"
  }
}
```

**The pattern.** A resource writes a service's configuration, and its `action_trigger` invokes the [`sysutils_service` action](../actions/service.md) after the resource is created (`after_create`) or changed (`after_update`). The plan lists the restart under "Actions to be invoked after this change", and the apply runs it right after writing the line. When the line is already as configured, nothing is changed and chrony is not restarted. The same works with a whole file written by [`sysutils_file`](../resources/file.md) or a template rendered by [`sysutils_template_file`](../resources/template_file.md); list several resources' triggers against the same action to restart once for each file that changed.

**Why `sysutils_file_line`:** the configuration file belongs to the chrony package, which puts its other settings (the drift file, the key file, logging) in different places on each distribution. Replacing only the `pool` line keeps those. `regexp` selects the line to replace, and `line` itself matches it, so that the next apply finds nothing to change. Destroying the resource removes the line, so keep it as long as chrony should use your pool.

**Why reference the service resource:** the action's `name` comes from `sysutils_service.base["chrony"]`, not from `var.services`. Terraform therefore invokes the action only after that resource has enabled and started chrony. Otherwise the first apply could restart chrony, and so start it, before the service resource has checked it, or try to restart it before the package is installed.

**Restart or reload.** `action = "restart"` stops and starts the service, starting it if it was stopped, and fails the apply if it doesn't come up again. With systemd, it runs `systemctl daemon-reload` first, so the pattern also works for unit files and drop-ins written by `sysutils_file`. For services that can apply a new configuration without a restart, such as nginx or sshd, `action = "reload"` avoids the interruption.

**If the restart fails,** for example because of a typo in the pool name that chrony rejects, the line has been written and its new value saved, so the next plan finds nothing to change and does not restart chrony again. Fix the configuration, or run the action on its own once the cause is fixed:

```shell
terraform apply -invoke=action.sysutils_service.restart_chrony
```

### Without actions

On Terraform before 1.14 and on OpenTofu, remove the `lifecycle` block and the `action` block, and let the service resource restart chrony instead: add `restart_on_change = { ntp_pool = sysutils_file_line.ntp_pool.line }` to `sysutils_service.base`, limited to chrony with a conditional expression. `restart_on_change` restarts the service whenever a value in the map changes, but, unlike the action, not when the line is first written.

## Administrators

```terraform
# Administrators: user name => OpenSSH public key.
locals {
  admins = {
    alice = file("${path.module}/keys/alice.pub")
  }
}

# Members of "ops" may use sudo.
resource "sysutils_group" "ops" {
  name = "ops"
}

resource "sysutils_user" "admin" {
  for_each = local.admins

  name        = each.key
  shell       = "/bin/bash"
  create_home = true
  groups      = [sysutils_group.ops.name]
}

resource "sysutils_ssh_authorized_key" "admin" {
  for_each = local.admins

  # Referencing the user creates the account and its home directory first.
  user = sysutils_user.admin[each.key].name
  key  = each.value
}

# The accounts have no password, so sudo must not ask for one. sudo ignores
# files in /etc/sudoers.d that others can write to; 0440 is the convention.
resource "sysutils_file" "sudoers_ops" {
  path    = "/etc/sudoers.d/ops"
  content = "%${sysutils_group.ops.name} ALL=(ALL:ALL) NOPASSWD: ALL\n"
  mode    = "0440"
  owner   = "root"
  group   = "root"

  depends_on = [sysutils_package.base]
}
```

**One map drives everything.** Adding a person to `local.admins` creates their account, adds them to `ops` and installs their key. Removing them deletes the key, the account and its home directory (`userdel`). Resources with `for_each` are tracked by the map key, so removing one administrator doesn't touch the others.

**Group membership lives on the user.** `groups` on `sysutils_user` sets the user's supplementary groups. Leave `members` of `sysutils_group.ops` unset: if both resources manage the membership, they undo each other's changes on every apply. Membership changes take effect at the user's next login.

**Keys are added, not the file replaced.** `sysutils_ssh_authorized_key` manages one line of `~alice/.ssh/authorized_keys` and leaves any other keys in it alone. It creates `~/.ssh` with mode `0700` and the file with mode `0600`, owned by the user, as `sshd`'s `StrictModes` requires. It never follows a symlink inside the home directory, which the user controls. To restrict a key, for example to a source network, set `options = ["from=\"10.0.0.0/8\""]`.

**sudo without passwords.** `sysutils_user` doesn't manage passwords, so the accounts have none and can only log in with their key. The sudoers rule therefore uses `NOPASSWD`. If your policy requires passwords for sudo, set them outside Terraform and remove `NOPASSWD:`. The file depends on the packages so that `/etc/sudoers.d` exists when it is written. A syntax error in a sudoers file disables sudo for everyone, so check a new rule with `visudo -cf` before you change it here.

## Applying it

```shell
terraform init
terraform plan -out=bootstrap.tfplan
terraform apply bootstrap.tfplan
```

Keep the state (`terraform.tfstate`) with the host's other records, or in a [remote backend](https://developer.hashicorp.com/terraform/language/backend). A plan later tells you what changed on the host since. A key someone removed by hand, a package someone uninstalled or a service someone stopped shows up as a planned change, and apply puts it back. See [Plans, `root_dir` and locking](./root-dir-plans-and-locking.md) for using plans as a drift report.

## Next steps

- [Harden the host](./hardening.md): kernel parameters, disabled kernel modules, SSH settings and file modes.
- [Deploy an application](./app-deployment.md) onto it.
- Add a repository before installing packages from it with [`sysutils_package_repository`](../resources/package_repository.md), and reference it from `sysutils_package` so that it is added first.
