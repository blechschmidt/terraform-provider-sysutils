# Run a small web service as the "appsvc" user, start it now and at boot.
# Every section of the unit file is an attribute, and every directive a
# nested attribute in snake case. Directives that may be repeated are lists.
resource "sysutils_systemd_unit" "app" {
  name    = "app.service"
  enabled = true
  state   = "running"

  unit = {
    description = "Example application"
    after       = ["network-online.target"]
    wants       = ["network-online.target"]
  }

  service = {
    user        = "appsvc"
    exec_start  = ["/usr/bin/python3 -m http.server 8080 --bind 127.0.0.1"]
    environment = ["PYTHONUNBUFFERED=1"]
    restart     = "on-failure"

    # Sandboxing, see systemd.exec(5).
    no_new_privileges = true
    protect_system    = "strict"
    protect_home      = true
    private_tmp       = true
  }

  install = {
    wanted_by = ["multi-user.target"]
  }
}

# A oneshot service triggered by a timer. The service itself is neither
# enabled nor started; the timer is.
resource "sysutils_systemd_unit" "backup" {
  name = "backup.service"

  service = {
    type       = "oneshot"
    exec_start = ["/usr/local/bin/backup --all"]
  }
}

resource "sysutils_systemd_unit" "backup_timer" {
  name    = "backup.timer"
  enabled = true
  state   = "running"

  timer = {
    on_calendar = ["daily"]
    persistent  = true
    unit        = sysutils_systemd_unit.backup.name
  }

  install = {
    wanted_by = ["timers.target"]
  }
}
