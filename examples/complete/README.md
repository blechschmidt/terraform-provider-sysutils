# Complete example: a service host

This stack provisions everything a small service needs on a Linux host:

| Resource | What it does |
|----------|--------------|
| `sysutils_group.service`, `sysutils_user.service` | System group and user the service runs as (on the host). |
| `sysutils_directory.root` | The sandbox directory that stands in for the host's root filesystem (`var.root_dir`). |
| `sysutils_directory.config`, `.state`, `.logs`, `.cron_d` | The service's directory tree, with modes and ownership. |
| `sysutils_template_file.config` | The service's configuration, rendered from `templates/service.conf.tmpl`. |
| `sysutils_cron_job.cleanup` | A nightly job that deletes old logs. |
| `sysutils_file_line.hosts` | An `/etc/hosts` entry for the service. |
| `sysutils_sysctl.somaxconn` | `net.core.somaxconn`, persisted in `/etc/sysctl.d/90-<name>.conf` (on the host). |
| `sysutils_systemd_unit.service` | The service's unit, enabled and running (on the host). Its `EnvironmentFile` is the rendered configuration, and it restarts when the configuration changes. |

Every resource that supports the provider's `root_dir` uses an aliased provider with `root_dir = var.root_dir`, so the files, directories, cron job and hosts entry are written below the sandbox instead of the host's `/etc` and `/var`. Users, groups, systemd units and kernel parameters always act on the host. `sysutils_sysctl` refuses to plan with `root_dir` set, so it uses the default provider.

The default `net.core.somaxconn` value, `4096`, is the kernel's own default since Linux 5.4, so applying the stack does not change the running kernel on current systems. Set `manage_systemd = false` on hosts without systemd as PID 1, such as containers.

## Usage

Run as root:

```sh
terraform init
terraform apply
terraform plan   # No changes: the stack is idempotent
terraform destroy
```

`make e2e` in the repository root does this with the provider built from the working tree, and checks the results; see `scripts/e2e-complete.sh`.
