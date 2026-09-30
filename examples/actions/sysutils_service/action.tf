# Reload nginx whenever its site configuration is created or changes. The
# action runs during apply, right after the file is written, and never when
# the file is unchanged.
resource "sysutils_file" "site" {
  path    = "/etc/nginx/conf.d/app.conf"
  content = <<-EOT
    server {
      listen 8080;
      root   /srv/app/public;
    }
  EOT
  mode    = "0644"

  lifecycle {
    action_trigger {
      events  = [after_create, after_update]
      actions = [action.sysutils_service.reload_nginx]
    }
  }
}

action "sysutils_service" "reload_nginx" {
  config {
    name   = "nginx"
    action = "reload"
  }
}
