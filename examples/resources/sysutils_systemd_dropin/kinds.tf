# Scope units have no unit files; drop-ins are the only way to configure
# them, here the scopes of user sessions started by systemd-logind.
resource "sysutils_systemd_dropin" "session_limits" {
  unit_name = "session-.scope"
  name      = "50-limits"

  scope = {
    runtime_max_sec = "12h"
  }
}

# A drop-in for every service on the host: /etc/systemd/system/service.d.
resource "sysutils_systemd_dropin" "all_services" {
  unit_name = "service"
  name      = "10-timeouts"

  service = {
    timeout_stop_sec = "30s"
  }
}

# A drop-in written as text.
resource "sysutils_systemd_dropin" "ssh_text" {
  unit_name = "ssh.service"
  name      = "override"
  content   = <<-EOT
    [Service]
    Environment=SSHD_OPTS=-o LogLevel=VERBOSE
  EOT
}
