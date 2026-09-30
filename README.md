# terraform-provider-sysutils

A Terraform provider for basic Linux system-administration primitives: files, lines in files, `/etc/hosts` entries, templated files, directories, symlinks, archives, local users and groups, systemd units and services, mounts, swap, kernel parameters and modules, the time zone and locale, cron jobs, SSH authorized keys, firewall rules, OS packages and package repositories, alternatives links, and commands. It is meant for bootstrapping hosts where a full configuration-management system would be overkill, and for the last mile of host setup that other providers don't cover.

The provider acts on the machine Terraform runs on. It has no remote-execution mode.

## Feature matrix

| Resource | Manages | Import | Drift detection | Requires root |
|----------|---------|:------:|-----------------|---------------|
| [`sysutils_file`](./docs/resources/file.md) | A file from text, base64 or a local source file; exposes checksums | Yes, by path | Content, mode, owner, group | Only to set `owner`/`group` or write to system paths |
| [`sysutils_file_line`](./docs/resources/file_line.md) | One line or a marker-delimited block in an existing file, like Ansible's `lineinfile`/`blockinfile` | Yes, by `path:line` or `path:marker` | Line or block missing or changed | Only for files you can't otherwise write, such as `/etc/hosts` |
| [`sysutils_ini_value`](./docs/resources/ini_value.md) | One `key = value` setting in a section of an INI-style file (systemd drop-ins, `php.ini`, git config, `sshd_config`), like Ansible's `ini_file`; creates a missing section | Yes, by `path:section:key` | Value changed, key removed, added (with `state = "absent"`) or duplicated | Only for files you can't otherwise write |
| [`sysutils_hosts_entry`](./docs/resources/hosts_entry.md) | One IP-to-hostnames line of `/etc/hosts` (or another hosts file), with an optional comment; keeps all other lines and the file's mode and owner, and refuses a hostname another line maps to a different address unless `allow_duplicate` is set | Yes, by `path:ip` | Hostnames or comment changed; line or file removed | Only for files you can't otherwise write, such as `/etc/hosts` |
| [`sysutils_template_file`](./docs/resources/template_file.md) | A file rendered from a Go or Terraform-syntax template and variables; the template is checked at plan time | Yes, by path | Content (by checksum), mode, owner, group | Only to set `owner`/`group` or write to system paths |
| [`sysutils_directory`](./docs/resources/directory.md) | A directory, optionally with recursive ownership and modes | Yes, by path | Mode, owner, group; with the recursive options, anywhere in the tree | Only to set `owner`/`group` or write to system paths |
| [`sysutils_symlink`](./docs/resources/symlink.md) | A symbolic link, switched atomically | Yes, by path | Target, owner, group of the link | Only to set `owner`/`group` or write to system paths |
| [`sysutils_archive_extract`](./docs/resources/archive_extract.md) | A local `.tar`, `.tar.gz`, `.tar.xz` or `.zip` archive extracted into a directory, atomically and with optional owner, group, modes and `strip_components`; refuses path traversal, escaping links and decompression bombs, and removes exactly the extracted files on destroy | No | Extracted files missing or changed (contents, type, symlink target, mode, owner, group); archive changed | Only to set `owner`/`group` or write to system paths |
| [`sysutils_user`](./docs/resources/user.md) | A local user via `useradd`/`usermod`/`userdel` | Yes, by name | uid, primary gid, home, shell; supplementary groups when `groups` is set | Yes |
| [`sysutils_group`](./docs/resources/group.md) | A local group and its members via `groupadd`/`groupmod`/`gpasswd`/`groupdel` | Yes, by name | gid; members when `members` is set | Yes |
| [`sysutils_systemd_unit`](./docs/resources/systemd_unit.md) | A systemd unit file in `/etc/systemd/system`, whether the unit is enabled and whether it is running | Yes, by unit name | Unit file content, enabled, running or stopped | Yes, and systemd as PID 1 |
| [`sysutils_service`](./docs/resources/service.md) | Whether an existing service starts at boot and is running, with `systemctl` when systemd is PID 1 or `rc-service`/`rc-update` with OpenRC (Alpine); restarts it when values in `restart_on_change` change | Yes, by name | Enabled or disabled, started or stopped outside Terraform; service removed | Yes, and systemd or OpenRC as init system |
| [`sysutils_mount`](./docs/resources/mount.md) | A file system mount and its `/etc/fstab` entry, like Ansible's `mount` module | Yes, by mount point | fstab entry changed or missing; unmounted, a different device or type mounted, or writable although `ro` is set | Yes |
| [`sysutils_swap`](./docs/resources/swap.md) | A swap file (allocated with `fallocate`, mode `0600`, owned by root) or an existing swap partition, formatted with `mkswap`, enabled with `swapon` with an optional priority, and its `/etc/fstab` entry; refuses to format devices holding a file system unless `force` is set, and deletes only swap files it created | Yes, by path | Disabled, enabled with another priority, resized, reformatted or deleted; fstab entry or its `pri=` option changed or missing | Yes, unless used with `root_dir` and `enabled = false` |
| [`sysutils_sysctl`](./docs/resources/sysctl.md) | A kernel parameter in `/proc/sys` and its `sysctl.d` entry, like `sysctl -w` or Ansible's `sysctl` module | Yes, by name or `name:file` | Running value changed; `sysctl.d` entry missing or changed | Yes |
| [`sysutils_kernel_module`](./docs/resources/kernel_module.md) | A kernel module loaded with `modprobe`, its parameters, and its `modules-load.d` and `modprobe.d` files | Yes, by name (must be loaded) | Module unloaded; configuration files missing or changed | Yes, with `CAP_SYS_MODULE` |
| [`sysutils_timezone`](./docs/resources/timezone.md) | The system time zone: `/etc/localtime` as a symlink into `/usr/share/zoneinfo` and, on Debian, Ubuntu and Alpine, `/etc/timezone`; uses `timedatectl` when systemd is PID 1; checks that the zone is installed; optionally restores the previous zone on destroy | Yes, as `system` | `/etc/localtime` pointing to another zone, replaced or missing; `/etc/timezone` naming another zone | Yes, unless used with `root_dir` on a tree you can write |
| [`sysutils_locale`](./docs/resources/locale.md) | The system locale: `LANG` and `LC_*` in `/etc/locale.conf` or `/etc/default/locale` (detected), keeping other lines; optionally compiles missing locales with `locale-gen` or `localedef`, and restores the previous values on destroy | Yes, as `system` | `LANG` or an `LC_*` variable changed, added or removed; file deleted | Yes, unless used with `root_dir` on a tree you can write |
| [`sysutils_ssh_authorized_key`](./docs/resources/ssh_authorized_key.md) | One public key, with its options and comment, in a user's `~/.ssh/authorized_keys`, like Ansible's `authorized_key`; creates `~/.ssh` (`0700`) and writes the file (`0600`) owned by the user, never following symlinks inside the home directory | Yes, by `user:fingerprint` | Key removed, duplicated, or its options or comment changed; file or `~/.ssh` deleted | Unless managing your own keys |
| [`sysutils_firewall_rule`](./docs/resources/firewall_rule.md) | One firewall rule in the `filter` table (family, chain, protocol, source and destination address, ports and port ranges, interfaces, accept/drop/reject), with nftables in the provider's own `inet terraform_sysutils` table or with iptables/ip6tables in the built-in chains (detected automatically); rules are tagged with a comment, never touching rules of other tools | Yes, by name or `<backend>:<name>` | Rule deleted or changed (ports, addresses, action and every other attribute); copies or changes the attributes can't express are replaced | Yes, or `CAP_NET_ADMIN` |
| [`sysutils_cron_job`](./docs/resources/cron_job.md) | A cron job in its own `/etc/cron.d` file, written with mode `0644` and root ownership, like Ansible's `cron` module with `cron_file` | Yes, by name | Schedule, user, command, environment, comment or any other content changed; mode or ownership changed; file missing | Yes |
| [`sysutils_package`](./docs/resources/package.md) | An OS package installed, kept at the newest version, pinned to an exact version or removed with `apt`, `dnf`, `yum` or `apk` (detected automatically), like Ansible's `package` module | Yes, by name (must be installed) | Package removed, installed, upgraded or downgraded outside Terraform; with `state = "latest"`, a newer version in the package index | Yes |
| [`sysutils_package_repository`](./docs/resources/package_repository.md) | An apt (deb822 `.sources`), dnf/yum (`.repo`) or apk repository, with its OpenPGP signing key given inline or fetched from a URL and stored in `/etc/apt/keyrings` or `/etc/pki/rpm-gpg`; optionally refreshes the package index, like Ansible's `deb822_repository` and `yum_repository` | Yes, by name or `<manager>:<name>` | Repository file or managed line edited or removed, signing key changed, removed or given another mode or owner | Yes |
| [`sysutils_alternatives`](./docs/resources/alternatives.md) | Which alternative a link group of `update-alternatives` (Debian, Ubuntu, SUSE) or `alternatives` (Fedora, RHEL) points to, pinned in manual mode; registers an alternative with `link` and `priority` if needed, and on destroy returns to automatic mode or unregisters it | Yes, by name | Another alternative selected, back in automatic mode, alternative or link group removed; with `link` and `priority`, master link or priority changed | Yes |
| [`sysutils_exec`](./docs/resources/exec.md) | A command run at create (and optionally destroy) time, with its exit code and output | No | No: results are recorded once; use `triggers` to re-run | Only if the command needs it |

| Data source | Reads | Requires root |
|-------------|-------|---------------|
| [`sysutils_file`](./docs/data-sources/file.md) | A file's contents, checksums, size, mode, ownership and modification time | Only for files you can't otherwise read |
| [`sysutils_directory`](./docs/data-sources/directory.md) | Whether a directory exists, its mode, ownership and entry names | Only for directories you can't otherwise list |
| [`sysutils_user`](./docs/data-sources/user.md) | A user by name or uid: uid, gid, home, shell, comment and supplementary groups | No |
| [`sysutils_group`](./docs/data-sources/group.md) | A group by name or gid: gid and members | No |
| [`sysutils_host`](./docs/data-sources/host.md) | Host facts for conditionals: hostname and FQDN, distribution from `os-release` (id, id_like, version, codename), kernel release, architecture, CPU count, memory, and the detected init system, package manager and firewall backend | No |

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

The only provider argument is the optional `root_dir`. With it, the resources that write files treat a directory tree as `/`, like a chroot, for example to build a container root filesystem. Resources that change the running system refuse to plan with it. See [Image trees, dry runs and parallel applies](./docs/guides/root-dir-plans-and-locking.md) for a worked example and [Root Directory](./docs/index.md#root-directory) for the details.

### Requirements

- Linux. The user and group resources call the `shadow-utils`/`passwd` tools; the file resources rely on Linux-specific system calls.
- Terraform >= 1.5, or OpenTofu. CI runs the acceptance tests against Terraform 1.5, the latest Terraform and the latest OpenTofu.
- Root privileges for the operations marked in the feature matrix. The provider never elevates privileges itself; run Terraform as root, or with `sudo`, when you need them.

## Quick example

Administrator accounts with their SSH keys, from the [bootstrapping recipe](./docs/guides/bootstrap-host.md):

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
```

## Cookbook

Each guide solves a common job end to end and explains why the configuration looks the way it does. Their configurations are in [`examples/guides`](./examples/guides).

- [Bootstrapping a host](./docs/guides/bootstrap-host.md): base packages and services, package names by distribution, administrator accounts, SSH keys and sudo.
- [Hardening a host](./docs/guides/hardening.md): kernel parameters, disabled kernel modules, SSH settings, file modes and a permission audit.
- [Deploying an application](./docs/guides/app-deployment.md): versioned releases from a tarball, a templated configuration, a systemd unit and restarts on change.
- [Image trees, dry runs and parallel applies](./docs/guides/root-dir-plans-and-locking.md): `root_dir`, plans as check mode and drift report, and how the provider locks shared files.
- [Provisioning a service account and its files](./docs/guides/service-account.md): directory ownership and modes, upgrades through a symlink, and importing an existing installation.

For a whole service host in one stack, see [`examples/complete`](./examples/complete).

## Documentation

- [Provider overview and security model](./docs/index.md)
- [Cookbook guides](#cookbook)
- Resources: [`sysutils_file`](./docs/resources/file.md), [`sysutils_file_line`](./docs/resources/file_line.md), [`sysutils_ini_value`](./docs/resources/ini_value.md), [`sysutils_hosts_entry`](./docs/resources/hosts_entry.md), [`sysutils_template_file`](./docs/resources/template_file.md), [`sysutils_directory`](./docs/resources/directory.md), [`sysutils_symlink`](./docs/resources/symlink.md), [`sysutils_archive_extract`](./docs/resources/archive_extract.md), [`sysutils_user`](./docs/resources/user.md), [`sysutils_group`](./docs/resources/group.md), [`sysutils_systemd_unit`](./docs/resources/systemd_unit.md), [`sysutils_service`](./docs/resources/service.md), [`sysutils_mount`](./docs/resources/mount.md), [`sysutils_swap`](./docs/resources/swap.md), [`sysutils_sysctl`](./docs/resources/sysctl.md), [`sysutils_kernel_module`](./docs/resources/kernel_module.md), [`sysutils_timezone`](./docs/resources/timezone.md), [`sysutils_locale`](./docs/resources/locale.md), [`sysutils_ssh_authorized_key`](./docs/resources/ssh_authorized_key.md), [`sysutils_cron_job`](./docs/resources/cron_job.md), [`sysutils_firewall_rule`](./docs/resources/firewall_rule.md), [`sysutils_package`](./docs/resources/package.md), [`sysutils_package_repository`](./docs/resources/package_repository.md), [`sysutils_alternatives`](./docs/resources/alternatives.md), [`sysutils_exec`](./docs/resources/exec.md)
- Data sources: [`sysutils_file`](./docs/data-sources/file.md), [`sysutils_directory`](./docs/data-sources/directory.md), [`sysutils_user`](./docs/data-sources/user.md), [`sysutils_group`](./docs/data-sources/group.md), [`sysutils_host`](./docs/data-sources/host.md)
- [Examples](./examples), including [`examples/complete`](./examples/complete), a whole service host in one stack

## Security considerations

The provider usually runs as root and changes system state directly, so it is worth knowing where its guarantees end. The [security model](./docs/index.md#security-model) in the provider documentation has the full details.

### Symlink handling

A local user who can write to a directory above a managed path could try to plant a symlink there, so that a root-run Terraform writes, `chmod`s or `chown`s a different file, such as `/etc/shadow`.

- The managed path itself is never followed if it is a symlink. `sysutils_file`, `sysutils_file_line`, `sysutils_ini_value`, `sysutils_hosts_entry`, `sysutils_template_file` and `sysutils_directory` open it with `O_NOFOLLOW` and fail rather than write through a link. Mode and ownership are changed through the open descriptor, so the path can't be swapped between check and change. `sysutils_symlink` changes the link itself with `lchown`, never its target.
- The `sysutils_file` data source refuses to read through a symlink unless `follow_symlinks = true`.
- Recursive operations (`force_destroy`, `recursive_owner`, `recursive_mode`) never follow symlinks inside the tree and never cross into another mounted filesystem. `force_destroy` also refuses to run if any component of the path is a symlink.
- Symlinks in the *parent* components of a path are followed for ordinary operations, so paths under `/var/run` and similar keep working. **Every ancestor directory of a managed path must be writable only by trusted users.** Avoid managing paths inside world-writable directories such as `/tmp` as root.
- With `root_dir` set, the provider resolves every path component itself, treats absolute link targets as relative to `root_dir`, and rejects any symlink that leads above it, so an untrusted tree (for example an unpacked image) cannot redirect it to the host.

### Content in state

Terraform state holds every attribute in plain text, and so does anything that has read access to your state backend.

- `sysutils_file` stores `content`, `sensitive_content` and `content_base64` verbatim. `sensitive_content` is hidden in plans, which show only the change of `content_sha256`. For `source`, only the path and checksums are stored.
- The `sysutils_file` data source stores the file's contents. Don't point it at secrets.
- `sysutils_file_line` stores the managed line or block, and `sysutils_ini_value` the managed value.
- `sysutils_template_file` stores the template, its variables and the rendered content. With `sensitive_vars`, the rendered content goes into the sensitive `rendered_sensitive` attribute, so it's hidden in plans, but it is still in state.
- `sysutils_systemd_unit` stores the unit file's contents.
- `sysutils_mount` stores its mount options, which also end up in the world-readable `/etc/fstab`. Use `credentials=` files instead of `password=` options for network shares.

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
- `sysutils_sysctl` and `sysutils_kernel_module` change the running kernel. A wrong parameter can cut the host off the network or weaken its hardening (for example `kernel.kptr_restrict` or `kernel.yama.ptrace_scope`), and changing a module's parameters unloads and reloads it. Sysctl names are validated so that they can only refer to files below `/proc/sys`, and module names and parameters are checked against strict patterns before `modprobe` sees them.
- `sysutils_cron_job` schedules commands that run as root by default. Its file is always given mode `0644` and root ownership before it is renamed into place, so no other user can change the job, and job names are restricted to `[A-Za-z0-9_-]` so that the file can only be created directly in `/etc/cron.d`.
- `sysutils_ssh_authorized_key` grants SSH access to an account. It edits a file in a directory the user controls, so it never follows a symlink inside the home directory, accesses `authorized_keys` only through the opened `~/.ssh` directory so that swapping it mid-way can't redirect the write, and refuses a file with several hard links, which could otherwise copy another file's contents into the user's file. Further lines with the same key are removed, so that a copy without the managed `from=` or `command=` restriction can't bypass it.
- `sysutils_package` installs software as root, and package installation scripts run as root. Only the repositories and keys configured on the host, including those added with `sysutils_package_repository`, are used. Package names and versions are checked against strict patterns and passed to the package manager as separate arguments, never through a shell, so they can't inject options or commands. Removing a package also removes the packages that depend on it (apt, dnf); set `remove_on_destroy = false` for packages the host can't do without.
- `sysutils_package_repository` decides where the host's packages come from, and a repository's key vouches for every package it serves. Keys are only fetched over `https` (or read from a local file), checked to be OpenPGP public keys, and bound to their repository with apt's `Signed-By` rather than trusted for every repository. Names, URIs and the other values are checked so that none can add a line, field or section to the file, and URIs with credentials are refused, since repository files are world-readable.
- `sysutils_alternatives` decides which program runs under a common name such as `java` or `editor`, possibly for every user and for root. With `link` and `priority` it registers any absolute path as an alternative, so treat those values like a `sysutils_symlink` in a system directory. Names and paths are checked against strict patterns (no leading `-`, absolute and canonical paths without white space) and passed as separate arguments, never through a shell. Its commands hold the package-manager lock, so they never overlap with a package install by the provider.
- Diagnostics contain paths and operating-system errors, never file content.
- Run Terraform as an unprivileged user when the configuration doesn't need root, for example when it only manages files in your own directories.

## Development

Building requires Go (see `go.mod` for the version). Acceptance tests and doc generation also need the `terraform` CLI.

| Command | What it does |
|---------|--------------|
| `make build` | Build the provider binary `terraform-provider-sysutils` in the repository root. |
| `make install` | Build and copy the binary into `~/.terraform.d/plugins/` so a local Terraform configuration can use it. |
| `make test` | Run unit tests. Acceptance tests are skipped, so the host isn't touched. |
| `make testacc-docker` | Run the full suite, including acceptance tests, as root inside a disposable container. **Use this to run acceptance tests.** Select the CLI with `TF_CLI=terraform\|tofu` and `TF_CLI_VERSION=<version>\|<prefix>\|latest`; add `SYSUTILS_UPGRADE_FROM_REF=auto` to also run the upgrade tests from a local baseline build ([upgrade tests](#upgrade-tests)). |
| `make testacc-docker-matrix` | Run `testacc-docker` for every CLI in the CI matrix: Terraform 1.5, Terraform latest and OpenTofu latest. |
| `make testacc` | Run the acceptance tests directly on the host (requires root). |
| `make e2e` | Apply [`examples/complete`](./examples/complete) with the local build, check that a second plan is empty, and destroy it. Changes the host (requires root); CI runs it on its runner VMs. |
| `make lint` | Run `golangci-lint`. |
| `make coverage` | Write a test-coverage report to `coverage.html`. |
| `make docs` | Format the examples and regenerate `docs/`. |
| `make docs-check` | Fail if `docs/` is out of date or the examples aren't formatted. Run in CI. |
| `make examples-check` | Run `terraform validate` on every example directory with the provider built from the checkout, and check that all HCL in the docs and in this README comes from `examples/`. Needs neither root nor network access. Run in CI. |

### Acceptance tests

The acceptance tests create real users and groups, write real files under `/tmp` and `/etc`, and run real commands. They run only when `TF_ACC=1` is set **and** the process is root, so a plain `go test ./...` never changes the host.

`make testacc-docker` builds `Dockerfile.test`, a Debian image with Go, the `passwd`, `acl` and `attr` tools and a Terraform or OpenTofu CLI, and runs `scripts/testacc-container.sh` inside it as root. That script runs `go test ./... -count=1` with `TF_ACC=1` against the installed CLI. The container is removed afterwards, so the host's users, groups and files are never modified. The container also gets `NET_ADMIN`: the `sysutils_firewall_rule` tests create a network namespace of their own with `unshare --net` and run `nft`, `iptables` and `ip6tables` inside it, so no firewall outside that namespace is changed, not even the container's. Without root, `unshare`, or the privileges to change a namespace's firewall, they are skipped. Only run `make testacc` on a machine you can afford to change, such as a throwaway VM. (`make test-docker` is an alias for `make testacc-docker`.)

```sh
make testacc-docker                                           # latest Terraform
make testacc-docker TF_CLI=terraform TF_CLI_VERSION=1.5       # newest Terraform 1.5.x
make testacc-docker TF_CLI=tofu TF_CLI_VERSION=latest         # latest OpenTofu
make testacc-docker TF_CLI=tofu TF_CLI_VERSION=1.12.6         # an exact release
make testacc-docker-matrix                                    # all CLIs tested in CI
```

`scripts/install-tf-cli.sh` downloads the CLI and checks it against the release's `SHA256SUMS`. The container gets `CAP_SYS_ADMIN` and no AppArmor profile, so that the tests can mount a tmpfs in the container's own mount namespace. It gets neither `CAP_SYS_MODULE` nor a writable `/proc/sys`, so the acceptance tests of `sysutils_kernel_module` and `sysutils_sysctl` skip themselves there; their unit tests use a fake `modprobe` and a fake `/proc/sys` and always run. On a root host or VM, the acceptance tests load and unload the `dummy` module and change `fs.lease-break-time`, restoring it afterwards, with configuration files in temporary directories. The acceptance tests of `sysutils_package` install, pin, upgrade and remove the small `tree` package (override with `SYSUTILS_ACC_PACKAGE`) with whichever of apt, dnf, yum or apk is installed, refreshing the package index first, so they need network access to the distribution's repositories. They skip themselves if the package is already installed, so that they never remove something the host needs; its unit tests use a fake package manager and scripted command output and always run. The acceptance test of `sysutils_package_repository` adds a repository to the host's real package manager and refreshes the index with it: the distribution's own backports suite, signed by its archive keyring, with apt; an empty local repository with dnf and yum; the tagged `edge/testing` repository with apk. It needs network access for apt and apk, and skips itself if its files exist already; its unit tests work below a temporary directory with a fake package manager and a local HTTPS server and always run. The acceptance tests of `sysutils_swap` create, enable, resize and remove small swap files in a temporary directory, with a temporary fstab, and format and enable loop devices (`losetup`); they skip themselves where `swapon` is not permitted or the temporary directory cannot hold swap files, as in the test container, whose `/tmp` is on overlayfs. Its unit tests use a fake `swapon` and always run. The acceptance tests of `sysutils_timezone` and `sysutils_locale` work below a temporary `root_dir`, except for three that change the real host and put it back afterwards: one sets the time zone (with `timedatectl` when systemd is PID 1) and restores it on destroy, one sets the locale to `C.UTF-8` and restores the locale file, and one compiles the `en_DK.UTF-8` locale with the real `locale-gen` or `localedef` into a temporary locale file, then restores `/etc/locale.gen` and deletes the compiled locale. The last skips itself if the locale is installed already; the test image installs `tzdata` and `locales` so that none of them skips. The acceptance tests of `sysutils_alternatives` register, select, change and remove a uniquely named link group of the host's real `update-alternatives` or `alternatives`, with its link and alternatives in a temporary directory, and remove the group with `--remove-all` when they end; they skip themselves where neither tool is installed, such as on Alpine. Its unit tests use a fake of both tools and a fake `update-alternatives` script and always run.

The run fails not only when a test fails, but also when a test is skipped. Only the systemd, kernel_module and sysctl tests may be skipped, because a container has no systemd as PID 1, no `CAP_SYS_MODULE` and a read-only `/proc/sys`, and so may the upgrade tests described below while their prerequisites are missing. This way a broken container setup can't silently turn the root-only user, group, chown and file_line tests into skips. Set `ACC_ALLOWED_SKIPS` to an extended regular expression to allow other skip messages. Extra arguments to the script are passed to `go test`, for example:

```sh
docker run --rm --cap-add SYS_ADMIN --security-opt apparmor=unconfined \
  terraform-provider-sysutils-testacc:terraform-latest \
  scripts/testacc-container.sh -run 'TestAccUser'
```

CI runs `make testacc-docker SYSUTILS_UPGRADE_FROM_REF=auto` for each CLI in its matrix (Terraform 1.5, latest Terraform, latest OpenTofu) in the `acceptance` job of `.github/workflows/test.yaml`.

#### Upgrade tests

`internal/provider/upgrade_acc_test.go` checks that state written by the last release still works with the current code. Each test applies a configuration with the released provider, downloaded from the Terraform Registry, then plans the same configuration with the current build and fails unless the plan is empty. This catches attributes added with a default or `RequiresReplace`, renamed or retyped attributes and missing state upgraders, which would otherwise make users' next plan replace their resources (for `sysutils_exec`: run the command again). If such a test fails because the schema had to change, bump the resource's schema `Version` and add a state upgrader, as `sysutils_exec` does in `exec_resource_upgrade.go`. The tests need network access to `registry.terraform.io`, also with OpenTofu.

They start from the release in `defaultUpgradeFromVersion`; `SYSUTILS_UPGRADE_FROM_VERSION` overrides it. `upgradeFirstRelease` records the first release of each tested resource. A resource that has not been released yet, such as `sysutils_directory`, has no state in the wild to be compatible with: its test skips itself with "is not in any release yet", which `scripts/testacc-container.sh` allows. After a release, bump `defaultUpgradeFromVersion` and fill in the release of any newly released resource.

`internal/provider/upgrade_local_acc_test.go` covers every resource, released or not, by upgrading from a provider built from an earlier commit of this repository instead of a release. Each test

1. builds the provider at the baseline commit (`git archive` into a temporary directory, then `go build`) and installs it as version `0.0.<number of commits>` into a temporary [filesystem mirror](https://developer.hashicorp.com/terraform/cli/config/config-file#filesystem_mirror), which a CLI configuration file passed in `TF_CLI_CONFIG_FILE` points Terraform or OpenTofu at (`dev_overrides` can't be used, because `tofu init` fails with it);
2. applies a configuration with that build and records the state;
3. plans the same configuration with the current code, fails unless the plan is empty, applies it, and fails if any attribute in the recorded state lost or changed its value (attributes the baseline didn't have may be added);
4. plans once more in a `PlanOnly` step, which fails unless the plans with and without a refresh are both empty. A plan without a refresh is only checked once the upgraded state has been stored: before that, Terraform plans an in-place update with no changed values for a resource whose schema has gained a sensitive attribute since the baseline;
5. destroys with the current code.

They run only if `SYSUTILS_UPGRADE_FROM_REF` is set, together with `TF_ACC=1` and root:

| `SYSUTILS_UPGRADE_FROM_REF` | Baseline of each resource |
|-----------------------------|---------------------------|
| unset | none; the tests skip themselves with "SYSUTILS_UPGRADE_FROM_REF is not set", which `scripts/testacc-container.sh` allows |
| `auto` | the merge base of `HEAD` and `main` (or `origin/main`) if the resource exists there, otherwise the commit that added the resource (`internal/provider/<name>_resource.go`) |
| any git ref, such as `v1.0.1` or `HEAD~5` | that commit if the resource exists there, otherwise the commit that added the resource |

Each baseline commit is built once per `go test` run, and the builds are deleted when it ends. The tests need `git`, the full history (a shallow clone has no merge base) and the Go toolchain, and network access if the baseline needs modules that aren't in the module cache. The `systemd_unit`, `mount`, `sysctl`, `kernel_module`, `cron_job` and `package` tests change the host just like the resources' own acceptance tests, and skip themselves under the same conditions. A baseline build can't use the fake fstab and package managers of the other tests, so they use the real ones: the `mount` test uses `persist = false` so that `/etc/fstab` is left alone, and the `cron_job` test writes a uniquely named file to `/etc/cron.d`. The `alternatives` test registers a uniquely named link group with the host's real alternatives tool, with its link and alternative in a temporary directory, and destroys it with `remove_on_destroy = true`, which removes the group again.

`make testacc-docker SYSUTILS_UPGRADE_FROM_REF=auto` passes the variable into the container and mounts the repository's `.git` directory read-only (it must be a directory, not a `git worktree` link file). The `acceptance` CI job runs it this way, with a full-history checkout. To run the tests directly on a root VM:

```sh
TF_ACC=1 SYSUTILS_UPGRADE_FROM_REF=auto go test ./internal/provider/ -run TestAccUpgradeLocal -v
TF_ACC=1 SYSUTILS_UPGRADE_FROM_REF=v1.0.1 go test ./internal/provider/ -run TestAccUpgradeLocal_file -v
```

If one of these tests fails because a change is not backward compatible, fix it as for the release-based tests above: make `Read` fill in new attributes, or bump the schema `Version` and add a state upgrader. A deliberate change to a stored value, such as the `id` format of `sysutils_file_line`, is listed in the test with `localUpgradeStepsChanging`.

#### End-to-end example

`make e2e` runs `scripts/e2e-complete.sh`: it builds the provider, points Terraform at it with `dev_overrides`, and runs `init`, `plan`, `apply`, a second `plan -detailed-exitcode` that must report no changes, and `destroy` on a copy of [`examples/complete`](./examples/complete). After apply and after destroy it checks the files, the user, the unit and the sysctl entry on the host. It creates the user and group `sysutilse2e`, the unit `sysutilse2e.service` and `/etc/sysctl.d/90-sysutilse2e.conf`, and writes everything else below `/var/tmp/sysutils-e2e/rootfs`; it refuses to start if any of these exist. The unit is left out if systemd is not PID 1. OpenTofu skips `init`, which fails with `dev_overrides` for a provider that is not in its registry. The `e2e` CI job runs it as root on the runner VM with the latest Terraform and the latest OpenTofu.

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

CI also runs `make examples-check` (`scripts/check-examples.sh`), so that no example in the docs is broken:

- It runs `terraform fmt -check` on `examples/`, then `terraform validate` on `examples/`, `examples/provider`, `examples/complete`, each `examples/guides/*` and each `examples/resources/*` and `examples/data-sources/*` directory. `validate` uses the provider built from the checkout through `dev_overrides`, so the current schema and its validators check every attribute. A directory without a `required_providers` block gets one for `blechschmidt/sysutils`. Nothing is planned or applied. With every provider overridden, `validate` needs no `terraform init`, so the check doesn't need network access either.
- It fails if a template in `templates/` contains a fenced `terraform` or `hcl` block. Put the configuration into a file below `examples/` and include it with `{{ tffile "examples/..." }}`, as the guides do.
- It fails if a `` ```terraform `` block in this README is not an exact excerpt of a file below `examples/`.

The files that examples read with `file()` or `filebase64()` must exist, since `validate` evaluates those functions. They are small stand-ins, such as `examples/resources/sysutils_ssh_authorized_key/keys/alice.pub`. Files that the provider only reads during plan, such as the `source` of `sysutils_file` and `sysutils_archive_extract`, may be missing.

## Releasing

Tag a commit matching `v*` and push the tag. Afterwards, bump `defaultUpgradeFromVersion` in `internal/provider/upgrade_acc_test.go` to the new release (see [upgrade tests](#upgrade-tests)). The `release` GitHub workflow uses goreleaser to build, sign (GPG) and publish the release, including the Terraform Registry manifest. The repository needs `GPG_PRIVATE_KEY` and `PASSPHRASE` configured as Actions secrets.

## License

MIT. See [LICENSE](./LICENSE).
