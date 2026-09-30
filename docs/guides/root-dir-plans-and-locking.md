---
page_title: "Cookbook: Image trees, dry runs and parallel applies"
subcategory: "Cookbook"
description: |-
  Configure a directory tree such as a container root filesystem with root_dir, use plans as a dry run and drift report, and rely on the provider's locking when many resources edit the same files.
---

# Cookbook: Image trees, dry runs and parallel applies

This page covers three provider behaviours that apply to all resources rather than one:

- [`root_dir`](#configuring-an-image-tree-with-root_dir) points the provider at a directory tree instead of the host.
- [Plans](#plans-as-a-dry-run-and-drift-report) are the provider's check mode: they show what apply would change, without changing anything.
- [Locking](#parallel-applies-and-locking) lets Terraform apply many resources that edit the same file at once.

The reference for all three is the [provider page](../index.md).

## Configuring an image tree with `root_dir`

Container images, VM images and chroots are often assembled in a directory on a build machine. With `root_dir`, the file resources treat that directory as `/`. The configuration then uses the paths the image will have, such as `/etc/hostname`, and the provider writes them below the directory.

```terraform
terraform {
  required_version = ">= 1.5"

  required_providers {
    sysutils = {
      source = "blechschmidt/sysutils"
    }
  }
}

variable "rootfs" {
  description = "Root file system of the image being built, such as an unpacked base image."
  type        = string
  default     = "/srv/images/web/rootfs"
}

# The host itself. Only used to create the image's root directory.
provider "sysutils" {}

# Everything inside the image. "/etc/hostname" means <rootfs>/etc/hostname,
# and symlinks in the tree are resolved inside it, as in a chroot.
provider "sysutils" {
  alias    = "image"
  root_dir = var.rootfs
}

resource "sysutils_directory" "rootfs" {
  path = var.rootfs
  mode = "0755"
  # The tree is a build artifact: destroy deletes it with everything in it,
  # including the files and directories the resources below leave behind.
  force_destroy = true
}
```

```terraform
resource "sysutils_file" "hostname" {
  provider = sysutils.image

  path    = "/etc/hostname"
  content = "web\n"
  mode    = "0644"
  # Numeric IDs: names would be looked up in the host's /etc/passwd, not in
  # the image's.
  owner = "0"
  group = "0"

  # root_dir must exist before anything below it is read or written.
  depends_on = [sysutils_directory.rootfs]
}

resource "sysutils_hosts_entry" "db" {
  provider = sysutils.image

  ip        = "10.0.0.5"
  hostnames = ["db.internal", "db"]

  depends_on = [sysutils_directory.rootfs]
}

resource "sysutils_cron_job" "logrotate" {
  provider = sysutils.image

  name     = "logrotate-hourly"
  schedule = "0 * * * *"
  command  = "/usr/sbin/logrotate /etc/logrotate.conf"

  depends_on = [sysutils_directory.rootfs]
}

# An absolute target is resolved inside the image: this link is correct
# when the image boots, and never points at the host's zoneinfo.
resource "sysutils_symlink" "localtime" {
  provider = sysutils.image

  path   = "/etc/localtime"
  target = "/usr/share/zoneinfo/Europe/Berlin"

  depends_on = [sysutils_directory.rootfs]
}
```

**Two provider configurations.** The default, unrooted configuration creates the image's root directory on the host. The aliased configuration `sysutils.image` manages everything inside it. `root_dir` must be known when Terraform plans, so it comes from a variable rather than from the directory resource, and `depends_on` makes sure the directory exists before anything is written into it. `force_destroy` lets `terraform destroy` delete the tree, which still contains what the resources leave behind, such as the image's `/etc/hosts` and `/etc/cron.d`.

**Symlinks stay inside the tree.** An unpacked image is full of symlinks, and many are absolute, such as `/etc/localtime -> /usr/share/zoneinfo/UTC`. A normal path lookup would follow them to the build machine's own files. Below `root_dir`, the provider resolves each path component itself, like `chroot`: an absolute link target is taken relative to `root_dir`, and a link that climbs above it is an error. A malicious or broken image therefore can't make the provider write to the build machine.

**Names are the host's.** `owner` and `group` names are looked up in the build machine's `/etc/passwd` and `/etc/group`, not in the image's, so the example uses numeric IDs.

**Only resources that write files can use `root_dir`.** Resources that change the running system, such as mounts, sysctls, kernel modules, services, packages, SSH keys, firewall rules and alternatives, refuse to plan with `root_dir` set, so that an image configuration never changes the build machine by mistake. Users, groups, systemd units and commands always act on the host. The [provider page](../index.md#root-directory) lists every resource and the exceptions.

## Plans as a dry run and drift report

The provider never changes the host during `terraform plan`. Refresh only reads, data sources only read, and checks that need the host, such as whether a template renders or a time zone is installed, don't modify it either. `terraform plan` is therefore this provider's equivalent of Ansible's `--check --diff`, and it's safe to run as often as you like.

The file resources are built so that the plan shows the change itself, not just "update in place":

- `sysutils_file` and `sysutils_template_file` read the file's actual content during refresh, so the plan shows a line diff from what is on disk to what apply will write.
- `sysutils_file_line`, `sysutils_ini_value` and `sysutils_hosts_entry` show the managed line, value or hostnames changing from what is in the file.
- Sensitive content is hidden, and the plan shows only the change of `content_sha256`.

A few ways to use plans:

```shell
# What would apply change? Save the plan, review it, and apply exactly that.
terraform plan -out=host.tfplan
terraform apply host.tfplan

# Drift report for monitoring or CI: exit code 0 means the host matches the
# configuration, 2 means it has drifted (1 is an error).
terraform plan -detailed-exitcode

# Only update the state from the host, without planning configuration changes.
terraform plan -refresh-only
```

`-detailed-exitcode` is a good nightly job: a key someone added by hand, a service someone stopped, a changed sysctl, or a file with the wrong mode all make it exit with `2`, and the plan shows which. [`check` blocks](https://developer.hashicorp.com/terraform/language/checks), as in the [hardening recipe](./hardening.md#ssh-private-directories-and-file-modes), add conditions on things that no resource manages, which are reported as warnings.

Two limits apply. `sysutils_exec` records its command's result once and doesn't compare it with the host later, so it never reports drift. Use `triggers` to run it again. And a plan can only predict so much: apply can still fail, for example if a package's post-install script fails.

## Parallel applies and locking

```terraform
# Nine resources that edit /etc/hosts. Terraform applies them in parallel;
# the provider serialises the edits, so none of them is lost.
locals {
  cluster = {
    "10.0.1.1" = "node1"
    "10.0.1.2" = "node2"
    "10.0.1.3" = "node3"
    "10.0.1.4" = "node4"
    "10.0.1.5" = "node5"
    "10.0.1.6" = "node6"
    "10.0.1.7" = "node7"
    "10.0.1.8" = "node8"
    "10.0.1.9" = "node9"
  }
}

resource "sysutils_hosts_entry" "cluster" {
  for_each = local.cluster

  ip        = each.key
  hostnames = ["${each.value}.cluster.internal", each.value]
  comment   = "cluster node"
}

# Each package is installed by its own resource, but only one package
# manager command runs at a time: apt, dnf, yum and apk fail rather than
# wait when another instance holds their lock.
resource "sysutils_package" "tools" {
  for_each = toset(["jq", "tree", "htop"])

  name = each.key
}
```

Terraform applies up to 10 resources at a time (`-parallelism`). All nine `sysutils_hosts_entry` resources above edit `/etc/hosts`. Each edit reads the file, changes its line and writes the file back, so two edits running at once would lose one of the changes. The provider prevents this:

- **Edits of the same file are serialised.** Every resource that edits a shared file, such as `/etc/hosts`, `/etc/fstab`, a `sysctl.d` file, `authorized_keys` or an INI file, locks the file from before it reads it until after it has written it. Edits of different files still run in parallel. The lock follows the real path, so `/var/run/x` and `/run/x` share a lock, and with `root_dir` the lock is on the path on the host.
- **Package manager commands run one at a time.** All `sysutils_package`, `sysutils_package_repository` and `sysutils_alternatives` operations share one lock, because apt, dnf, yum and apk fail rather than wait for each other, and package scripts run the alternatives tools. Many packages can be declared separately, but they install one after another.
- **Firewall changes run one at a time,** so that no rule is added or removed based on an outdated list of rules.
- **Locks work across processes.** They use `flock(2)` on files in `/run/terraform-provider-sysutils` (for root), so two Terraform runs on one host, or two provider configurations in one run, also wait for each other.

You don't need `depends_on` or `-parallelism=1` to order resources that edit the same file. Use `depends_on` only when the order matters for the result, as with firewall rules, which are evaluated in the order they were added.

Programs other than the provider don't take these locks. If an editor or another tool changes a file between the provider reading and writing it, the provider notices before replacing the file, and the apply fails with "file was modified by another process" rather than overwriting the other change. Run apply again. The [provider page](../index.md#concurrency-and-locking) has the details, including how long each lock is waited for.
