# Rotate the logs of myapp daily, keep two weeks of them compressed, and
# tell the service to reopen its log files afterwards. Writes
# /etc/logrotate.d/myapp, checked with "logrotate -d" first.
resource "sysutils_logrotate" "myapp" {
  name          = "myapp"
  paths         = ["/var/log/myapp/*.log"]
  frequency     = "daily"
  rotate        = 14
  compress      = true
  delaycompress = true
  missingok     = true
  notifempty    = true
  sharedscripts = true

  # A fresh, empty log file right after rotation.
  create_mode  = "0640"
  create_owner = "myapp"
  create_group = "adm"

  postrotate = <<-EOT
    systemctl kill --signal=HUP --kill-whom=main myapp.service
  EOT

  # Anything the typed attributes don't cover, one directive per line.
  extra_directives = [
    "maxsize 100M",
    "dateext",
    "su myapp adm",
  ]
}
