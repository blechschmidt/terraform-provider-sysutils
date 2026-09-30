# Changelog

All notable changes to this provider are listed here. Versions follow [semantic versioning](https://semver.org/). The resource and data source pages under [`docs/`](./docs) describe each feature in full.

## Unreleased

### New data sources

- [`sysutils_package`](./docs/data-sources/package.md) reads whether an OS package is installed, its `version` and `architecture`, and the `available_version` the local package index offers (apt's candidate, or the newest version in the repositories with dnf, yum and apk), with the backends and detection of the `sysutils_package` resource. It only runs read-only queries and needs no root; dnf and yum query their metadata in cache-only mode, so that it isn't downloaded when it has expired. `refresh_cache = true` refreshes the index first, taking the provider's package-manager lock, at most once per provider run.
- [`sysutils_service`](./docs/data-sources/service.md) reads whether a service `exists`, is `enabled` and is `running`, its primary `unit` name and the init system's own `enabled_state` and `active_state`, with the systemd and OpenRC backends of the `sysutils_service` resource. It needs no root. Where neither systemd nor OpenRC runs, as in most containers or with SysV init, it still succeeds: `init_system` reports what runs (`sysvinit` or null), `supported` is false and the service attributes are null.
- Both refuse to read when the provider's `root_dir` is set, since they describe the running host.

### Tests

- The [upgrade tests](./README.md#upgrade-tests) now cover every resource: `sysutils_firewall_rule` and `sysutils_swap`, the two that v1.1.0's `upgrade_local_acc_test.go` left out, are upgraded from v1.1.0 on the Terraform Registry (`upgrade_acc_test.go`) and from a local baseline build (`upgrade_local_acc_test.go`, with `SYSUTILS_UPGRADE_FROM_REF`). The firewall tests run Terraform, and with it the baseline provider, inside a private network namespace; the swap tests use `persist = false` and leave `/etc/fstab` alone.
- The registry upgrade tests now run the same checks as the local ones: besides an empty plan, every stored attribute must keep its value, and a later plan without a refresh must be empty too.
- `make testacc-docker`, and with it the `acceptance` CI job, now runs the swap acceptance tests instead of skipping them: the container gets an unconfined seccomp profile, which Docker's default blocks `swapon` with, and a volume on `/tmp` for the swap files. A skipped swap test now fails the run, except for the tests on loop devices, which a container doesn't have.

## 1.1.0 (2026-09-30)

Everything below is new since v1.0.1, which shipped only `sysutils_file`, `sysutils_user` and `sysutils_exec`.

### Upgrading from v1.0.1

State written by v1.0.1 keeps working. After upgrading, the first `terraform plan` of an unchanged configuration is empty: no resource is replaced and no `sysutils_exec` command runs again. The [upgrade tests](./README.md#upgrade-tests) check this:

- `internal/provider/upgrade_acc_test.go` applies `sysutils_file` and `sysutils_exec` with v1.0.1 downloaded from the Terraform Registry, then plans with the current build and requires an empty plan.
- `internal/provider/upgrade_local_acc_test.go` does the same for `sysutils_file`, `sysutils_user`, `sysutils_exec` and every newer resource except `sysutils_firewall_rule` and `sysutils_swap`, with a baseline built from an earlier commit. In CI (`SYSUTILS_UPGRADE_FROM_REF=auto`), the baseline of the three v1.0.1 resources is the merge base with `main`, which is the v1.0.1 commit. It also checks that no stored attribute changes its value and that a later plan without a refresh is empty.

Some behaviour did change, and a few configurations that v1.0.1 accepted are now refused. Read the notes on the three existing resources below before upgrading.

### Behaviour changes to existing resources

#### `sysutils_file`

- **Breaking:** symlinks, FIFOs, devices and other non-regular files at `path` are refused instead of followed. The file is opened with `O_NOFOLLOW`, and all changes go through that descriptor. To manage the target of a symlink, set `path` to the target, or manage the link with `sysutils_symlink`.
- **Breaking:** `path` must be an absolute, clean path, and `mode` must be 3 or 4 octal digits (for example `"0644"`, `"600"` or `"4755"`). Both are checked at plan time.
- `content` is now optional. Exactly one of `content`, `sensitive_content` (new, hidden in plans), `content_base64` (new, binary data) or `source` (new, copies a local file) must be set. Configurations that set `content` are unchanged.
- `mode` now defaults to `"0644"` in the schema and is enforced on every apply. If `mode` is not set, a mode changed outside Terraform is set back to `0644`. v1.0.1 set `0644` only on create. The mode is applied with an explicit `chmod`, so the umask has no effect, and setuid/setgid bits survive ownership changes.
- `owner` and `group` accept numeric IDs and are now also computed. If not set, they record the file's current owner and group but don't change them. If set, changes made outside Terraform show up as drift and are set back.
- New computed attributes `content_sha256` and `content_md5` hold checksums, known at plan time. Content drift is detected by checksum, and for `content` the plan shows a text diff of what is on disk.
- Import by path: `terraform import sysutils_file.x /etc/x.conf`.
- With the new provider argument `root_dir`, `path` is resolved inside that directory.

#### `sysutils_user`

- `useradd`, `usermod`, `userdel` and the group commands now run one at a time within the provider process. Parallel applies no longer fail with `cannot lock /etc/passwd`.
- The login shell is read from `getent passwd`, and the provider checks that the entry it gets back really has the configured name. `getent` treats an all-digit name as a UID, so a user named `1000` used to be read as whoever has UID 1000. Names starting with `-` are refused.
- Supplementary groups are read in sorted order. When `groups` is not set, membership is neither compared nor changed, as before.
- The schema is unchanged: no attributes were added or removed.

#### `sysutils_exec`

- The schema is now version 1. A state upgrader fills in the new attributes, so v1.0.1 resources are neither replaced nor run again.
- New attributes: `timeout`, `destroy_command`, `sensitive_output` (output goes to `sensitive_stdout`/`sensitive_stderr` and is left out of error messages), `max_output_bytes`, and the computed `stdout_sha256`, `stderr_sha256` and `truncated`. Changing `timeout` or `destroy_command` updates the resource in place without re-running `command`. Changing any other input still replaces it.
- **Behaviour change:** at most `max_output_bytes` (1 MiB by default) of each output stream is stored in state. Longer output is cut off with a marker line, and `truncated` is set to `true`. The `*_sha256` attributes always hash the complete streams.
- **Behaviour change:** each command runs in its own process group. When `timeout` expires, or Terraform is interrupted, the whole group gets `SIGKILL`, and a timeout always fails, even with `fail_on_nonzero = false`.
- **Behaviour change:** if the command exits but background processes it started keep its stdout or stderr open for more than 5 seconds, the apply now fails instead of hanging. Redirect the output of daemons started from `command`, for example with `>/dev/null 2>&1`.

### New resources

- `sysutils_directory`: a directory with mode, owner and group, optionally applied recursively like `chmod -R`/`chown -R`, with drift detection anywhere in the tree; import by path.
- `sysutils_symlink`: a symbolic link, switched atomically; import by path.
- `sysutils_group`: a local group and, optionally, its complete member list, with `groupadd`/`groupmod`/`gpasswd`/`groupdel`; import by name.
- `sysutils_file_line`: one line or a marker-delimited block in an existing file, like Ansible's `lineinfile`/`blockinfile`; import by `path:line` or `path:marker`.
- `sysutils_template_file`: a file rendered from a Go or Terraform-syntax template, checked at plan time; import by path.
- `sysutils_ini_value`: one `key = value` setting in a section of an INI-style file, like Ansible's `ini_file`.
- `sysutils_hosts_entry`: one IP-to-hostnames line of `/etc/hosts`.
- `sysutils_archive_extract`: a local `.tar`, `.tar.gz`, `.tar.xz` or `.zip` archive extracted atomically, with path-traversal, link and size checks.
- `sysutils_systemd_unit`: a unit file in `/etc/systemd/system`, and whether the unit is enabled and running.
- `sysutils_service`: whether an existing service is enabled and running, with systemd or OpenRC.
- `sysutils_mount`: a mount and its `/etc/fstab` entry.
- `sysutils_swap`: a swap file or partition, formatted with `mkswap`, enabled with `swapon`, optionally recorded in `/etc/fstab`.
- `sysutils_sysctl`: a kernel parameter in `/proc/sys` and its `sysctl.d` entry.
- `sysutils_kernel_module`: a loaded kernel module, its parameters, and its `modules-load.d`/`modprobe.d` files.
- `sysutils_cron_job`: a cron job in its own `/etc/cron.d` file.
- `sysutils_sudoers`: a sudo drop-in in `/etc/sudoers.d`, from content or structured rules, checked with `visudo` before it is installed.
- `sysutils_ssh_authorized_key`: one key in a user's `~/.ssh/authorized_keys`, like Ansible's `authorized_key`.
- `sysutils_firewall_rule`: one rule in the `filter` table, with nftables or iptables/ip6tables.
- `sysutils_package`: an OS package installed, kept at the latest version, pinned or removed, with apt, dnf, yum or apk.
- `sysutils_package_repository`: an apt, dnf/yum or apk repository and its OpenPGP signing key.
- `sysutils_alternatives`: the selection of an `update-alternatives`/`alternatives` link group.
- `sysutils_hostname`: the static and kernel hostname, optionally the pretty hostname and a `127.0.1.1` line in `/etc/hosts`.
- `sysutils_timezone`: the system time zone.
- `sysutils_locale`: the system locale, optionally generating missing locales.

### New data sources

- `sysutils_file`: a file's contents, checksums, size, mode, ownership and modification time.
- `sysutils_directory`: whether a directory exists, its mode, ownership and entry names.
- `sysutils_user`: a user by name or UID.
- `sysutils_group`: a group by name or GID, with its members.
- `sysutils_host`: host facts for conditionals, such as hostname and FQDN, distribution (from `os-release`), kernel release, architecture, CPU count, memory, init system, package manager and firewall backend.

### Provider

- New optional provider argument `root_dir`. The file-writing resources and the file and directory data sources treat that directory as `/`, like a chroot, for example to build a container root filesystem. Absolute symlinks are resolved inside it, and links that lead out of it are refused.
- Edits to shared files, package manager runs and firewall changes are serialised with in-process mutexes and `flock(2)`, both within one provider process and between concurrent Terraform runs on the same host.
- Release builds for `linux/amd64`, `linux/arm64`, `linux/386` and `linux/arm` (v1.0.1 had `amd64` and `arm64` only). Fixed a build failure on 32-bit platforms in `sysutils_swap`.

### Documentation and testing

- Documentation is generated with tfplugindocs, and CI fails if it is out of date. There are cookbook guides for bootstrapping a host, deploying an application, hardening, service accounts and `root_dir`, and CI validates every example with `terraform validate`.
- CI runs the acceptance tests as root in containers with Terraform 1.5, the latest Terraform and the latest OpenTofu, and in stock Debian, Alpine and Fedora containers. It also applies `examples/complete` end to end with Terraform and OpenTofu, and runs the upgrade tests described above.

## 1.0.1

- Add the MIT license.

## 1.0.0

- First release: `sysutils_file`, `sysutils_user` and `sysutils_exec`.
