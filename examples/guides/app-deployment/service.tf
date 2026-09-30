# The unit file only. Whether the service is enabled and running is left to
# sysutils_service below, so that the two resources don't both manage it.
resource "sysutils_systemd_unit" "myapp" {
  name = "myapp.service"

  content = <<-EOT
    [Unit]
    Description=myapp
    After=network-online.target
    Wants=network-online.target

    [Service]
    User=${sysutils_user.myapp.name}
    Group=${sysutils_group.myapp.name}
    ExecStart=${sysutils_symlink.current.path}/bin/myapp --config ${sysutils_template_file.config.path}
    WorkingDirectory=${sysutils_directory.state.path}
    Restart=on-failure
    NoNewPrivileges=true
    ProtectSystem=strict
    ReadWritePaths=${sysutils_directory.state.path}

    [Install]
    WantedBy=multi-user.target
  EOT
}

resource "sysutils_service" "myapp" {
  name    = sysutils_systemd_unit.myapp.name
  enabled = true
  state   = "running"

  # Restart when a new release is switched to or the configuration changes.
  restart_on_change = {
    release = sysutils_symlink.current.target
    config  = sysutils_template_file.config.content_sha256
  }
}
