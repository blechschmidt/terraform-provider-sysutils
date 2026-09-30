# Keep the journal on disk, so that logs survive a reboot, but bounded in
# size and age. Settings that are not set keep their defaults.
resource "sysutils_journald_config" "hardening" {
  name              = "60-hardening"
  storage           = "persistent"
  compress          = true
  system_max_use    = "2G"
  max_retention_sec = "3month"

  # Needs systemd; set it to false when building an image with root_dir.
  restart = true
}

# sudo's own log of every command run through it, readable only by root.
resource "sysutils_sudoers" "logfile" {
  name    = "10-logfile"
  content = "Defaults logfile=\"/var/log/sudo.log\"\n"
}

# Rotate that log weekly and keep a year of it. Re-created with the same
# restrictive mode after each rotation.
resource "sysutils_logrotate" "sudo" {
  name         = "sudo-log"
  paths        = ["/var/log/sudo.log"]
  frequency    = "weekly"
  rotate       = 52
  compress     = true
  missingok    = true
  notifempty   = true
  create_mode  = "0600"
  create_owner = "root"
  create_group = "root"
}
