# Install nginx, keep it enabled and running, and restart it whenever its
# configuration changes. Works with systemd and with OpenRC (Alpine).
resource "sysutils_package" "nginx" {
  name = "nginx"
}

resource "sysutils_file" "nginx_conf" {
  path    = "/etc/nginx/conf.d/app.conf"
  content = "server { listen 8080; root /srv/www; }\n"

  depends_on = [sysutils_package.nginx]
}

resource "sysutils_service" "nginx" {
  name    = "nginx"
  enabled = true
  state   = "running"

  restart_on_change = {
    config = sysutils_file.nginx_conf.content_sha256
  }

  depends_on = [sysutils_package.nginx]
}

# Keep a service that a package enabled from running at boot, and stop it.
resource "sysutils_service" "avahi" {
  name    = "avahi-daemon.service"
  enabled = false
  state   = "stopped"
}

# Only report whether a service is running; change nothing.
resource "sysutils_service" "sshd" {
  name = "ssh.service"
}

output "sshd_running" {
  value = sysutils_service.sshd.state == "running"
}
