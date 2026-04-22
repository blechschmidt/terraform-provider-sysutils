---
page_title: "sysutils Provider"
subcategory: ""
description: |-
  Terraform provider for basic Linux system utility operations: writing files, managing users, and executing commands.
---

# sysutils Provider

The `sysutils` provider exposes three primitives for host-level administration from Terraform:

| Resource | Purpose |
|----------|---------|
| [`sysutils_file`](./resources/file.md) | Write a file with given content, mode, and ownership. |
| [`sysutils_user`](./resources/user.md) | Create, update, and delete local users via `useradd`/`usermod`/`userdel`. |
| [`sysutils_exec`](./resources/exec.md) | Run a command with a chosen environment, capturing exit code, stdout, and stderr. |

It is intended for small bootstrapping tasks where installing and configuring something like Ansible or a full configuration-management system would be overkill.

## Example Usage

```terraform
terraform {
  required_providers {
    sysutils = {
      source = "blechschmidt/sysutils"
    }
  }
}

provider "sysutils" {}

resource "sysutils_file" "motd" {
  path    = "/etc/motd"
  content = "Welcome.\n"
  mode    = "0644"
}
```

## Requirements

- Linux host (user and exec resources depend on POSIX semantics and the `shadow-utils`/`passwd` toolchain).
- Privileges sufficient to perform the operations. Writing to `/etc`, changing file ownership, and creating users all require root. The provider itself makes no attempt to elevate privileges — run Terraform as root (or via `sudo`) when needed.
- Terraform >= 1.5.

## Schema

The provider takes no configuration arguments.
