# Change a unit that a package installed, without replacing its unit file:
# /etc/systemd/system/nginx.service.d/50-limits.conf. nginx is restarted
# when the drop-in changes.
resource "sysutils_systemd_dropin" "nginx_limits" {
  unit_name = "nginx.service"
  name      = "50-limits"

  service = {
    limit_nofile = "65536"
    memory_max   = "1G"
    restart      = "on-failure"
    restart_sec  = "5s"
  }
}

# Replace the command of a unit: an empty assignment first clears the
# vendor's ExecStart=.
resource "sysutils_systemd_dropin" "getty_autologin" {
  unit_name = "getty@tty1.service"
  name      = "autologin"

  service = {
    exec_start = ["", "-/sbin/agetty --autologin admin --noclear %I $TERM"]
  }
}
