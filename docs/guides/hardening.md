---
page_title: "Cookbook: Hardening a host"
subcategory: "Cookbook"
description: |-
  Apply a hardening baseline: kernel parameters, disabled kernel modules, SSH settings, private directories and an audit of system directory permissions.
---

# Cookbook: Hardening a host

Hardening baselines such as the CIS benchmarks are mostly long lists of small settings: a kernel parameter here, a disabled module there, a file that must not be readable by others. Each one is easy to set by hand and just as easy to lose, for example to a package upgrade, a colleague debugging something, or a new image. Managed with Terraform, every setting has a resource, and `terraform plan` reports any that no longer holds.

This recipe uses [`sysutils_sysctl`](../resources/sysctl.md), [`sysutils_kernel_module`](../resources/kernel_module.md), [`sysutils_file`](../resources/file.md), [`sysutils_service`](../resources/service.md), [`sysutils_directory`](../resources/directory.md) and the [`sysutils_directory` data source](../data-sources/directory.md). Apply it as root. The complete configuration is in [`examples/guides/hardening`](https://github.com/blechschmidt/terraform-provider-sysutils/tree/main/examples/guides/hardening).

~> **Test on a machine you can reach another way first.** The SSH settings below disable password and root logins. Before you apply them to a remote host, make sure you can log in with a key as a user with sudo rights (see [Bootstrapping a host](./bootstrap-host.md)). Otherwise, keep a console open.

```terraform
terraform {
  required_version = ">= 1.5"

  required_providers {
    sysutils = {
      source = "blechschmidt/sysutils"
    }
  }
}

provider "sysutils" {}
```

## Kernel parameters

```terraform
# Kernel parameters from common hardening baselines. All of them go into
# one file, which systemd-sysctl (or the sysctl init script) applies at boot.
locals {
  sysctl_file = "/etc/sysctl.d/90-hardening.conf"

  hardening_sysctls = {
    # Hide kernel addresses and the kernel log from unprivileged users.
    "kernel.kptr_restrict"  = "2"
    "kernel.dmesg_restrict" = "1"
    # No core dumps of setuid programs, and no link tricks in sticky
    # directories such as /tmp.
    "fs.suid_dumpable"       = "0"
    "fs.protected_symlinks"  = "1"
    "fs.protected_hardlinks" = "1"
    # Drop spoofed packets and ignore ICMP redirects and source routing.
    "net.ipv4.conf.all.rp_filter"           = "1"
    "net.ipv4.conf.all.accept_redirects"    = "0"
    "net.ipv4.conf.all.send_redirects"      = "0"
    "net.ipv4.conf.all.accept_source_route" = "0"
    "net.ipv6.conf.all.accept_redirects"    = "0"
    "net.ipv4.tcp_syncookies"               = "1"
  }
}

resource "sysutils_sysctl" "hardening" {
  for_each = local.hardening_sysctls

  name  = each.key
  value = each.value
  file  = local.sysctl_file
}
```

Each `sysutils_sysctl` writes the value to `/proc/sys` right away and adds a `name = value` line to `file`, so that it also holds after a reboot. Parameters from one baseline belong in one file of their own. You can then see what the baseline changed by reading one file, and a `99-` file from a package or an administrator can still override it. The resources share the file, and the provider [locks it](https://registry.terraform.io/providers/blechschmidt/sysutils/latest/docs#concurrency-and-locking) for every edit, so Terraform can apply all eleven in parallel without losing a line.

A plan reports a parameter changed at runtime (`sysctl -w`) or a line edited in the file, and apply restores both. Destroying a resource removes its line, but it doesn't reset the running kernel's value. That happens at the next reboot.

Check each parameter against what the host does before you apply it. For example, `rp_filter = 1` drops packets that arrive on an interface other than the one the reply would leave by. That breaks hosts with asymmetric routing, where `2` (loose mode) is the right value.

## Kernel modules

```terraform
# File systems and network protocols the host never needs. "install <name>
# /bin/false" makes every attempt to load the module fail, including
# automatic loading when a program opens such a socket or file system;
# "blacklist" alone only stops loading by alias.
locals {
  disabled_modules = ["cramfs", "freevxfs", "hfs", "hfsplus", "jffs2", "dccp", "sctp", "rds", "tipc"]
}

resource "sysutils_file" "disabled_modules" {
  path    = "/etc/modprobe.d/90-disabled.conf"
  content = join("", [for m in local.disabled_modules : "install ${m} /bin/false\nblacklist ${m}\n"])
  mode    = "0644"
  owner   = "root"
  group   = "root"
}

# Pass bridged traffic (containers, VMs) through the firewall, too. The
# module is loaded now and listed in /etc/modules-load.d, which systemd
# processes before it applies sysctl.d at boot.
resource "sysutils_kernel_module" "br_netfilter" {
  name = "br_netfilter"
}

resource "sysutils_sysctl" "bridge_filtering" {
  for_each = toset(["net.bridge.bridge-nf-call-iptables", "net.bridge.bridge-nf-call-ip6tables"])

  name  = each.key
  value = "1"
  file  = local.sysctl_file

  # The parameters only exist while the module is loaded.
  depends_on = [sysutils_kernel_module.br_netfilter]
}
```

`sysutils_kernel_module` *loads* modules, and cannot keep one from being loaded. A module you never want is therefore disabled with a `modprobe.d` file. Its `install ... /bin/false` lines make `modprobe` fail for it, even when the kernel requests the module on demand, such as when a program opens an SCTP socket. An already loaded module stays loaded until the next reboot or `modprobe -r`.

`br_netfilter` shows the opposite case: a module the hardening depends on. Without it, traffic between containers or VMs on a Linux bridge bypasses iptables and nftables completely. The module resource loads it now and lists it in `/etc/modules-load.d/br_netfilter.conf` for the next boot. The two `net.bridge.*` parameters only exist while the module is loaded, so they depend on it. At boot, systemd loads the modules before it applies `sysctl.d`, so the order holds there, too.

## SSH, private directories and file modes

```terraform
# SSH settings in a drop-in file that only root can read. sshd uses the
# first value it finds for a setting, and the stock sshd_config of Debian,
# Ubuntu and Fedora includes sshd_config.d/*.conf at the top, so these win.
resource "sysutils_file" "sshd_hardening" {
  path    = "/etc/ssh/sshd_config.d/90-hardening.conf"
  content = <<-EOT
    PermitRootLogin no
    PasswordAuthentication no
    KbdInteractiveAuthentication no
    X11Forwarding no
    MaxAuthTries 3
  EOT
  mode    = "0600"
  owner   = "root"
  group   = "root"
}

variable "ssh_service" {
  description = "Name of the SSH service: \"ssh\" on Debian and Ubuntu, \"sshd\" elsewhere."
  type        = string
  default     = "ssh"
}

# Restarting sshd applies the settings; open sessions are not affected.
resource "sysutils_service" "sshd" {
  name  = var.ssh_service
  state = "running"

  restart_on_change = {
    hardening = sysutils_file.sshd_hardening.content_sha256
  }
}

# A directory whose contents only root may read, whoever writes to it.
# recursive_mode and recursive_owner reset every file below it on each
# apply, and a plan reports files that do not match in
# nonconforming_entries.
resource "sysutils_directory" "backups" {
  path            = "/var/backups/host"
  mode            = "0700"
  owner           = "root"
  group           = "root"
  recursive_mode  = true
  file_mode       = "0600"
  recursive_owner = true
}

# Audit system directories without taking them over: a data source only
# reads them, and the check block turns a finding into a warning in every
# plan and apply.
data "sysutils_directory" "root_only" {
  for_each = toset(["/etc", "/etc/cron.d", "/etc/ssh", "/etc/systemd/system", "/root"])

  path = each.key
}

locals {
  # Mode digits 2, 3, 6 and 7 include the write permission.
  writable_digits = ["2", "3", "6", "7"]

  writable_by_others = [
    for path, dir in data.sysutils_directory.root_only : "${path} (${dir.owner}:${dir.group} ${dir.mode})"
    if dir.exists && (
      coalesce(dir.uid, 0) != 0 ||
      contains(local.writable_digits, substr(coalesce(dir.mode, "0755"), 2, 1)) ||
      contains(local.writable_digits, substr(coalesce(dir.mode, "0755"), 3, 1))
    )
  ]
}

check "root_only_writable" {
  assert {
    condition     = length(local.writable_by_others) == 0
    error_message = "Directories writable by users other than root: ${join(", ", local.writable_by_others)}."
  }
}
```

**Modes are part of the resource.** `sysutils_file` and `sysutils_directory` apply `owner`, `group` and `mode` when they create the file, check them on every refresh, and restore them on the next apply if someone changed them. Ownership is set before the mode, and both before any content is written. A secret is therefore never visible, even briefly, under a looser mode. See the [security model](https://registry.terraform.io/providers/blechschmidt/sysutils/latest/docs#security-model).

**Restart on change.** `restart_on_change` restarts `sshd` whenever the drop-in file's checksum changes, so the new settings take effect in the same apply. Established SSH sessions survive a restart of `sshd`. If you also use the [bootstrapping recipe](./bootstrap-host.md), manage the SSH service with only one `sysutils_service` resource: merge `restart_on_change` into the one that sets `enabled`, rather than having two resources for the same service.

**Recursive modes for directories others write to.** `/var/backups/host` is written by backup jobs, which may create files with any mode. With `recursive_mode` and `recursive_owner`, every apply resets all entries below it to `root:root`, `0700` for directories and `0600` for files. A plan shows how many entries don't match in `nonconforming_entries`. Symlinks inside the tree are never followed.

**Audit what you don't own.** Directories such as `/etc` and `/root` belong to the operating system, not to this configuration. A resource would take them over: `terraform destroy` would try to remove them. The data source only reads their mode and owner. The [`check` block](https://developer.hashicorp.com/terraform/language/checks) turns a directory that users other than root can write to into a warning in every `terraform plan` and `apply`, without failing the run:

```text
Warning: Check block assertion failed

  on file_modes.tf line 73, in check "root_only_writable":
  73:     condition     = length(local.writable_by_others) == 0
    ├────────────────
    │ local.writable_by_others is tuple with 1 element

Directories writable by users other than root: /etc/cron.d (root:root 0777).
```

Checks need Terraform 1.5 or later, or OpenTofu. The data source also stores the directories' entry names in the state, so keep it to directories with few entries.

## Firewall

A baseline usually also restricts inbound connections. [`sysutils_firewall_rule`](../resources/firewall_rule.md) adds one nftables or iptables rule per resource, in a table of its own, and never touches rules that other tools manage. Its page has examples for allowing SSH from one network only.
