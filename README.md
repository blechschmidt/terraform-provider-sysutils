# terraform-provider-sysutils

A Terraform provider for basic Linux system-administration primitives: managing files, directories and symlinks, managing local users, and executing commands. Useful for bootstrapping hosts where a full configuration-management system would be overkill.

## Features

### Resources

- `sysutils_file` — write a file with content, mode, and optional owner/group. Supports import and detects drift.
- `sysutils_directory` — manage a directory with mode and optional owner/group; optionally delete it recursively on destroy. Supports import and detects drift.
- `sysutils_symlink` — manage a symbolic link, switching its target atomically, with optional owner/group of the link itself. Supports import and detects drift.
- `sysutils_user` — create, update, and delete local users via `useradd`/`usermod`/`userdel`.
- `sysutils_exec` — run a command (passed as a string array) with either the provider's environment or a custom one, capturing exit code, stdout, and stderr.

### Data sources

- `sysutils_directory` — read a directory's existence, mode, ownership and entry names without managing it.

## Installation

```terraform
terraform {
  required_providers {
    sysutils = {
      source = "blechschmidt/sysutils"
    }
  }
}

provider "sysutils" {}
```

## Quick example

```terraform
resource "sysutils_directory" "app" {
  path  = "/srv/app"
  mode  = "0750"
  owner = "root"
  group = "root"
}

resource "sysutils_file" "app_config" {
  path    = "${sysutils_directory.app.path}/app.conf"
  content = "listen = 127.0.0.1:8080\n"
  mode    = "0640"
}

resource "sysutils_symlink" "app_config_link" {
  path   = "/etc/app.conf"
  target = sysutils_file.app_config.path
}

resource "sysutils_file" "hello" {
  path    = "/etc/hello.conf"
  content = "greeting = hi\n"
  mode    = "0644"
  owner   = "root"
  group   = "root"
}

resource "sysutils_user" "app" {
  name        = "appsvc"
  system      = true
  shell       = "/usr/sbin/nologin"
  create_home = false
  groups      = ["adm"]
}

resource "sysutils_exec" "migrate" {
  command = ["/usr/local/bin/my-migrate", "--apply"]
  environment = {
    DATABASE_URL = "postgres://localhost/app"
  }
  triggers = {
    # Bump to force a re-run.
    schema_version = "3"
  }
}

output "migration_stdout" { value = sysutils_exec.migrate.stdout }
output "migration_exit"   { value = sysutils_exec.migrate.exit_code }
```

## Documentation

- [Provider overview](./docs/index.md)
- [`sysutils_file`](./docs/resources/file.md)
- [`sysutils_directory`](./docs/resources/directory.md)
- [`sysutils_symlink`](./docs/resources/symlink.md)
- [`sysutils_user`](./docs/resources/user.md)
- [`sysutils_exec`](./docs/resources/exec.md)
- [`sysutils_directory` data source](./docs/data-sources/directory.md)
- [Examples](./examples)

## Requirements

- Linux (the user and exec resources depend on `useradd`/`usermod`/`userdel` and POSIX semantics).
- Root privileges for most operations — setting file ownership, creating users, and writing to system paths all require it.
- Terraform >= 1.5.

## Development

```sh
make build        # build the provider binary
make install      # install into ~/.terraform.d/plugins for local use
make test         # unit tests (skip acceptance; no host mutation)
make testacc      # acceptance tests against the host (requires root)
make test-docker  # acceptance tests inside a throwaway container (safe)
make lint         # golangci-lint
make coverage     # generate coverage.html
```

The acceptance tests create real users, write real files, and execute real commands. They are gated by `TF_ACC=1` **and** a root-EUID check, and the default path for running them is `make test-docker`, which builds `Dockerfile.test` and runs everything inside a disposable container so the host is never touched.

## Releasing

Tag a commit matching `v*` and push the tag; the `release` GitHub workflow uses goreleaser to build, sign (GPG), and publish the Terraform Registry manifest. The repo must have `GPG_PRIVATE_KEY` and `PASSPHRASE` configured as Actions secrets.

## License

MIT. See [LICENSE](./LICENSE).
