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
