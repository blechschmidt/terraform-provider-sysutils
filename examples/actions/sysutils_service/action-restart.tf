# Restart a service when any of several files change. Each file triggers the
# same action; Terraform invokes it once per changed file.
resource "sysutils_file" "app_config" {
  for_each = {
    "app.env"  = "LISTEN=127.0.0.1:9000\n"
    "app.toml" = "[log]\nlevel = \"info\"\n"
  }

  path    = "/etc/app/${each.key}"
  content = each.value
  mode    = "0640"

  lifecycle {
    action_trigger {
      events  = [after_create, after_update]
      actions = [action.sysutils_service.restart_app]
    }
  }
}

action "sysutils_service" "restart_app" {
  config {
    name    = "app.service"
    action  = "restart"
    timeout = "5m"
  }
}
