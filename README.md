# terraform-provider-sysutils

A Terraform provider for basic Linux system-administration primitives: files, lines in files, templated files, directories, symlinks, local users and groups, systemd units, and commands. It is meant for bootstrapping hosts where a full configuration-management system would be overkill, and for the last mile of host setup that other providers don't cover.

The provider acts on the machine Terraform runs on. It has no remote-execution mode.

## Feature matrix

| Resource | Manages | Import | Drift detection | Requires root |
|----------|---------|:------:|-----------------|---------------|
| [`sysutils_file`](./docs/resources/file.md) | A file from text, base64 or a local source file; exposes checksums | Yes, by path | Content, mode, owner, group | Only to set `owner`/`group` or write to system paths |
| [`sysutils_file_line`](./docs/resources/file_line.md) | One line or a marker-delimited block in an existing file, like Ansible's `lineinfile`/`blockinfile` | Yes, by `path:line` or `path:marker` | Line or block missing or changed | Only for files you can't otherwise write, such as `/etc/hosts` |
| [`sysutils_template_file`](./docs/resources/template_file.md) | A file rendered from a Go or Terraform-syntax template and variables; the template is checked at plan time | Yes, by path | Content (by checksum), mode, owner, group | Only to set `owner`/`group` or write to system paths |
| [`sysutils_directory`](./docs/resources/directory.md) | A directory, optionally with recursive ownership and modes | Yes, by path | Mode, owner, group; with the recursive options, anywhere in the tree | Only to set `owner`/`group` or write to system paths |
| [`sysutils_symlink`](./docs/resources/symlink.md) | A symbolic link, switched atomically | Yes, by path | Target, owner, group of the link | Only to set `owner`/`group` or write to system paths |
| [`sysutils_user`](./docs/resources/user.md) | A local user via `useradd`/`usermod`/`userdel` | Yes, by name | uid, primary gid, home, shell; supplementary groups when `groups` is set | Yes |
| [`sysutils_group`](./docs/resources/group.md) | A local group and its members via `groupadd`/`groupmod`/`gpasswd`/`groupdel` | Yes, by name | gid; members when `members` is set | Yes |
| [`sysutils_systemd_unit`](./docs/resources/systemd_unit.md) | A systemd unit file in `/etc/systemd/system`, whether the unit is enabled and whether it is running | Yes, by unit name | Unit file content, enabled, running or stopped | Yes, and systemd as PID 1 |
| [`sysutils_exec`](./docs/resources/exec.md) | A command run at create (and optionally destroy) time, with its exit code and output | No | No: results are recorded once; use `triggers` to re-run | Only if the command needs it |

| Data source | Reads | Requires root |
|-------------|-------|---------------|
| [`sysutils_file`](./docs/data-sources/file.md) | A file's contents, checksums, size, mode, ownership and modification time | Only for files you can't otherwise read |
| [`sysutils_directory`](./docs/data-sources/directory.md) | Whether a directory exists, its mode, ownership and entry names | Only for directories you can't otherwise list |
| [`sysutils_user`](./docs/data-sources/user.md) | A user by name or uid: uid, gid, home, shell, comment and supplementary groups | No |
| [`sysutils_group`](./docs/data-sources/group.md) | A group by name or gid: gid and members | No |

Data sources are read on every plan, so they always reflect the current state of the host.

**Drift detection** means that `terraform plan` compares the host with the state and shows changes made outside Terraform, and the next `terraform apply` reverts them. If a managed object is deleted outside Terraform, it is removed from state and recreated on the next apply.

**Import** lets you adopt objects that already exist. After `terraform import`, the next plan lists every difference between the host and your configuration without changing anything. The import format for each resource is on its documentation page.

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

The only provider argument is the optional `root_dir`. It makes the file, file line, template file, directory and symlink resources and the file and directory data sources work inside a directory tree, as if in a chroot, for example to build a container root filesystem:

```terraform
provider "sysutils" {
  root_dir = "/srv/images/web/rootfs" # "/etc/hosts" means /srv/images/web/rootfs/etc/hosts
}
```

Symlinks in the tree are resolved inside it: absolute link targets are relative to `root_dir`, and a link that leads above `root_dir` is an error. See [Root Directory](./docs/index.md#root-directory).

### Requirements

- Linux. The user and group resources call the `shadow-utils`/`passwd` tools; the file resources rely on Linux-specific system calls.
- Terraform >= 1.5, or OpenTofu. CI runs the acceptance tests against Terraform 1.5, the latest Terraform and the latest OpenTofu.
- Root privileges for the operations marked in the feature matrix. The provider never elevates privileges itself; run Terraform as root, or with `sudo`, when you need them.

## Quick example

```terraform
resource "sysutils_group" "app" {
  name   = "appsvc"
  system = true
}

resource "sysutils_user" "app" {
  name        = "appsvc"
  gid         = sysutils_group.app.gid
  system      = true
  shell       = "/usr/sbin/nologin"
  create_home = false
}

resource "sysutils_directory" "app" {
  path  = "/srv/app"
  mode  = "0750"
  owner = "root"
  group = sysutils_group.app.name
}

resource "sysutils_file" "app_config" {
  path    = "${sysutils_directory.app.path}/app.conf"
  content = "listen = 127.0.0.1:8080\n"
  mode    = "0640"
  owner   = "root"
  group   = sysutils_group.app.name
}

resource "sysutils_symlink" "app_config_link" {
  path   = "/etc/app.conf"
  target = sysutils_file.app_config.path
}

resource "sysutils_file_line" "app_host" {
  path = "/etc/hosts"
  line = "10.0.0.5 db.internal db"
}

resource "sysutils_exec" "reload" {
  command = ["/usr/bin/systemctl", "reload-or-restart", "app"]
  triggers = {
    # Re-run whenever the configuration changes.
    config = sysutils_file.app_config.content_sha256
  }
}
```

The [service account guide](./docs/guides/service-account.md) walks through a complete version of this setup, including drift detection, upgrades via symlink and importing an existing installation.

## Documentation

- [Provider overview and security model](./docs/index.md)
- [Guide: provisioning a service account and its files](./docs/guides/service-account.md)
- Resources: [`sysutils_file`](./docs/resources/file.md), [`sysutils_file_line`](./docs/resources/file_line.md), [`sysutils_template_file`](./docs/resources/template_file.md), [`sysutils_directory`](./docs/resources/directory.md), [`sysutils_symlink`](./docs/resources/symlink.md), [`sysutils_user`](./docs/resources/user.md), [`sysutils_group`](./docs/resources/group.md), [`sysutils_systemd_unit`](./docs/resources/systemd_unit.md), [`sysutils_exec`](./docs/resources/exec.md)
- Data sources: [`sysutils_file`](./docs/data-sources/file.md), [`sysutils_directory`](./docs/data-sources/directory.md), [`sysutils_user`](./docs/data-sources/user.md), [`sysutils_group`](./docs/data-sources/group.md)
- [Examples](./examples)

## Security considerations

The provider usually runs as root and changes system state directly, so it is worth knowing where its guarantees end. The [security model](./docs/index.md#security-model) in the provider documentation has the full details.

### Symlink handling

A local user who can write to a directory above a managed path could try to plant a symlink there, so that a root-run Terraform writes, `chmod`s or `chown`s a different file, such as `/etc/shadow`.

- The managed path itself is never followed if it is a symlink. `sysutils_file`, `sysutils_file_line`, `sysutils_template_file` and `sysutils_directory` open it with `O_NOFOLLOW` and fail rather than write through a link. Mode and ownership are changed through the open descriptor, so the path can't be swapped between check and change. `sysutils_symlink` changes the link itself with `lchown`, never its target.
- The `sysutils_file` data source refuses to read through a symlink unless `follow_symlinks = true`.
- Recursive operations (`force_destroy`, `recursive_owner`, `recursive_mode`) never follow symlinks inside the tree and never cross into another mounted filesystem. `force_destroy` also refuses to run if any component of the path is a symlink.
- Symlinks in the *parent* components of a path are followed for ordinary operations, so paths under `/var/run` and similar keep working. **Every ancestor directory of a managed path must be writable only by trusted users.** Avoid managing paths inside world-writable directories such as `/tmp` as root.
- With `root_dir` set, the provider resolves every path component itself, treats absolute link targets as relative to `root_dir`, and rejects any symlink that leads above it, so an untrusted tree (for example an unpacked image) cannot redirect it to the host.

### Content in state

Terraform state holds every attribute in plain text, and so does anything that has read access to your state backend.

- `sysutils_file` stores `content`, `sensitive_content` and `content_base64` verbatim. `sensitive_content` is hidden in plans, which show only the change of `content_sha256`. For `source`, only the path and checksums are stored.
- The `sysutils_file` data source stores the file's contents. Don't point it at secrets.
- `sysutils_file_line` stores the managed line or block.
- `sysutils_template_file` stores the template, its variables and the rendered content. With `sensitive_vars`, the rendered content goes into the sensitive `rendered_sensitive` attribute, so it's hidden in plans, but it is still in state.
- `sysutils_systemd_unit` stores the unit file's contents.

Don't manage secrets with these resources unless your state backend encrypts data at rest and access to it is restricted. Marking a value `sensitive` only hides it in plans and CLI output; it is still in state.

### Exec output in state

`sysutils_exec` records its command, `stdin`, `environment` and the captured `stdout` and `stderr` in state.

- `sensitive_output = true` moves the output into `sensitive_stdout` and `sensitive_stderr`, which are redacted in plans, CLI output and error diagnostics, but are still stored in state.
- `max_output_bytes` (1 MiB by default) caps how much output is stored, which keeps state small for chatty commands. `stdout_sha256` and `stderr_sha256` always cover the complete output and are never sensitive.
- To keep a secret out of state altogether, have the command read it from a file or secret store itself instead of passing it through Terraform.

### Running as root

- Every command in `sysutils_exec` runs with the privileges of the user running Terraform. Treat a configuration that contains `sysutils_exec` like a shell script run as that user, and review changes to it accordingly.
- `force_destroy = true` on `sysutils_directory` deletes everything in the tree, including files Terraform doesn't manage. `/`, a fixed list of critical system directories and paths containing symlinks are refused, but anything else is deleted when you ask for it.
- `sysutils_user` and `sysutils_group` change `/etc/passwd`, `/etc/group` and `/etc/shadow`. Changing a group's gid doesn't re-own existing files, and changing a user's uid re-owns only the files in their home directory. Fix ownership elsewhere yourself.
- Diagnostics contain paths and operating-system errors, never file content.
- Run Terraform as an unprivileged user when the configuration doesn't need root, for example when it only manages files in your own directories.

## Development

Building requires Go (see `go.mod` for the version). Acceptance tests and doc generation also need the `terraform` CLI.

| Command | What it does |
|---------|--------------|
| `make build` | Build the provider binary `terraform-provider-sysutils` in the repository root. |
| `make install` | Build and copy the binary into `~/.terraform.d/plugins/` so a local Terraform configuration can use it. |
| `make test` | Run unit tests. Acceptance tests are skipped, so the host isn't touched. |
| `make testacc-docker` | Run the full suite, including acceptance tests, as root inside a disposable container. **Use this to run acceptance tests.** Select the CLI with `TF_CLI=terraform\|tofu` and `TF_CLI_VERSION=<version>\|<prefix>\|latest`. |
| `make testacc-docker-matrix` | Run `testacc-docker` for every CLI in the CI matrix: Terraform 1.5, Terraform latest and OpenTofu latest. |
| `make testacc` | Run the acceptance tests directly on the host (requires root). |
| `make lint` | Run `golangci-lint`. |
| `make coverage` | Write a test-coverage report to `coverage.html`. |
| `make docs` | Format the examples and regenerate `docs/`. |
| `make docs-check` | Fail if `docs/` is out of date or the examples aren't formatted. Run in CI. |

### Acceptance tests

The acceptance tests create real users and groups, write real files under `/tmp` and `/etc`, and run real commands. They run only when `TF_ACC=1` is set **and** the process is root, so a plain `go test ./...` never changes the host.

`make testacc-docker` builds `Dockerfile.test`, a Debian image with Go, the `passwd`, `acl` and `attr` tools and a Terraform or OpenTofu CLI, and runs `scripts/testacc-container.sh` inside it as root. That script runs `go test ./... -count=1` with `TF_ACC=1` against the installed CLI. The container is removed afterwards, so the host's users, groups and files are never modified. Only run `make testacc` on a machine you can afford to change, such as a throwaway VM. (`make test-docker` is an alias for `make testacc-docker`.)

```sh
make testacc-docker                                           # latest Terraform
make testacc-docker TF_CLI=terraform TF_CLI_VERSION=1.5       # newest Terraform 1.5.x
make testacc-docker TF_CLI=tofu TF_CLI_VERSION=latest         # latest OpenTofu
make testacc-docker TF_CLI=tofu TF_CLI_VERSION=1.12.6         # an exact release
make testacc-docker-matrix                                    # all CLIs tested in CI
```

`scripts/install-tf-cli.sh` downloads the CLI and checks it against the release's `SHA256SUMS`. The container gets `CAP_SYS_ADMIN` and no AppArmor profile, so that the tests can mount a tmpfs in the container's own mount namespace.

The run fails not only when a test fails, but also when a test is skipped. Only the systemd tests may be skipped, because a container has no systemd as PID 1. This way a broken container setup can't silently turn the root-only user, group, chown and file_line tests into skips. Set `ACC_ALLOWED_SKIPS` to an extended regular expression to allow other skip messages. Extra arguments to the script are passed to `go test`, for example:

```sh
docker run --rm --cap-add SYS_ADMIN --security-opt apparmor=unconfined \
  terraform-provider-sysutils-testacc:terraform-latest \
  scripts/testacc-container.sh -run 'TestAccUser'
```

CI runs `make testacc-docker` for each CLI in its matrix (Terraform 1.5, latest Terraform, latest OpenTofu) in the `acceptance` job of `.github/workflows/test.yaml`.

To try a local build against a Terraform configuration, build it and point Terraform at it with a [`dev_overrides`](https://developer.hashicorp.com/terraform/cli/config/config-file#development-overrides-for-provider-developers) block in `~/.terraformrc`:

```hcl
provider_installation {
  dev_overrides {
    "blechschmidt/sysutils" = "/path/to/terraform-provider-sysutils"
  }
  direct {}
}
```

### Documentation

Everything under `docs/` is generated by [terraform-plugin-docs](https://github.com/hashicorp/terraform-plugin-docs); don't edit it by hand. `make docs` builds it from:

- the `MarkdownDescription` of each schema attribute, in `internal/provider/*.go`;
- the page layouts and prose in `templates/`, including the guides in `templates/guides/`;
- the example configurations in `examples/`: `resource.tf`, `data-source.tf` and other `.tf` files are pulled into the pages by the templates, and each resource's `import.sh` becomes its Import section.

Change those sources, run `make docs`, and commit the regenerated `docs/` together with your change. CI runs `make docs-check`, which fails when the committed `docs/` doesn't match. `TestResourceDocsHaveImportExamples` fails if a resource supports import but has no `examples/resources/<name>/import.sh`.

## Releasing

Tag a commit matching `v*` and push the tag. The `release` GitHub workflow uses goreleaser to build, sign (GPG) and publish the release, including the Terraform Registry manifest. The repository needs `GPG_PRIVATE_KEY` and `PASSPHRASE` configured as Actions secrets.

## License

MIT. See [LICENSE](./LICENSE).
