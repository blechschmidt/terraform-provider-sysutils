# Override a packaged unit with a drop-in written by sysutils_file, then make
# systemd load it and restart the service with it. The actions run in the
# order they are listed.
resource "sysutils_file" "nginx_limits" {
  path    = "/etc/systemd/system/nginx.service.d/limits.conf"
  content = <<-EOT
    [Service]
    LimitNOFILE=65536
  EOT
  mode    = "0644"

  lifecycle {
    action_trigger {
      events  = [after_create, after_update]
      actions = [action.sysutils_systemd_daemon_reload.this, action.sysutils_service.restart_nginx]
    }
  }
}

action "sysutils_systemd_daemon_reload" "this" {}

action "sysutils_service" "restart_nginx" {
  config {
    name   = "nginx.service"
    action = "restart"
  }
}
