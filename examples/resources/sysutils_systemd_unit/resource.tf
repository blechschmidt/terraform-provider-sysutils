# Run a small web service as the "appsvc" user, start it now and at boot.
resource "sysutils_systemd_unit" "app" {
  name    = "app.service"
  enabled = true
  state   = "running"

  content = <<-EOT
    [Unit]
    Description=Example application
    After=network-online.target
    Wants=network-online.target

    [Service]
    User=appsvc
    ExecStart=/usr/bin/python3 -m http.server 8080 --bind 127.0.0.1
    Restart=on-failure

    [Install]
    WantedBy=multi-user.target
  EOT
}

# A oneshot service triggered by a timer. The service itself is neither
# enabled nor started; the timer is.
resource "sysutils_systemd_unit" "backup" {
  name   = "backup.service"
  source = "${path.module}/units/backup.service"
}

resource "sysutils_systemd_unit" "backup_timer" {
  name    = "backup.timer"
  enabled = true
  state   = "running"

  content = <<-EOT
    [Timer]
    OnCalendar=daily
    Persistent=true

    [Install]
    WantedBy=timers.target
  EOT

  # Make sure the service exists before the timer can fire.
  depends_on = [sysutils_systemd_unit.backup]
}
