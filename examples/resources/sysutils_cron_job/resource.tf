# Written to /etc/cron.d/backup:
#
#   # Managed by Terraform (sysutils_cron_job). Manual changes will be reverted.
#   # Nightly database backup; see the runbook.
#   MAILTO=ops@example.com
#   PATH=/usr/local/bin:/usr/bin:/bin
#   30 2 * * 1-5 backup pg_dumpall | gzip > /var/backups/db-$(date +\%F).sql.gz
resource "sysutils_cron_job" "backup" {
  name     = "backup"
  schedule = "30 2 * * 1-5"
  user     = "backup"
  # cron turns an unescaped % into a newline, so it is escaped as \% (written
  # "\\%" in HCL).
  command = "pg_dumpall | gzip > /var/backups/db-$(date +\\%F).sql.gz"
  comment = "Nightly database backup; see the runbook."

  environment = {
    MAILTO = "ops@example.com"
    PATH   = "/usr/local/bin:/usr/bin:/bin"
  }
}

# Runs as root, once at boot.
resource "sysutils_cron_job" "warm_cache" {
  name     = "warm-cache"
  schedule = "@reboot"
  command  = "/usr/local/bin/warm-cache >/dev/null 2>&1"
}
