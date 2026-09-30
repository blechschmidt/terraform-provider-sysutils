data "sysutils_service" "ssh" {
  # "ssh" on Debian and Ubuntu; systemd resolves the alias "sshd" to it.
  name = "sshd"
}

output "ssh" {
  value = {
    init_system = data.sysutils_service.ssh.init_system # "systemd", "openrc", "sysvinit" or null
    exists      = data.sysutils_service.ssh.exists
    unit        = data.sysutils_service.ssh.unit    # "ssh.service"
    enabled     = data.sysutils_service.ssh.enabled # starts at boot
    running     = data.sysutils_service.ssh.running
    active      = data.sysutils_service.ssh.active_state # "active", "failed", ...
  }
}
