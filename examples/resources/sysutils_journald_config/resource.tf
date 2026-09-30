# Keep the journal on disk, capped at 1 GiB and one month, and restart
# journald so that it applies the settings right away. Writes
# /etc/systemd/journald.conf.d/90-retention.conf.
resource "sysutils_journald_config" "retention" {
  name              = "90-retention"
  storage           = "persistent"
  system_max_use    = "1G"
  max_retention_sec = "1month"
  compress          = true
  forward_to_syslog = false

  # Other settings of the [Journal] section of journald.conf(5).
  extra = {
    RateLimitIntervalSec = "30s"
    RateLimitBurst       = "10000"
  }

  restart = true
}
