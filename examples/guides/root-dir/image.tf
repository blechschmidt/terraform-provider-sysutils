resource "sysutils_file" "hostname" {
  provider = sysutils.image

  path    = "/etc/hostname"
  content = "web\n"
  mode    = "0644"
  # Numeric IDs: names would be looked up in the host's /etc/passwd, not in
  # the image's.
  owner = "0"
  group = "0"

  # root_dir must exist before anything below it is read or written.
  depends_on = [sysutils_directory.rootfs]
}

resource "sysutils_hosts_entry" "db" {
  provider = sysutils.image

  ip        = "10.0.0.5"
  hostnames = ["db.internal", "db"]

  depends_on = [sysutils_directory.rootfs]
}

resource "sysutils_cron_job" "logrotate" {
  provider = sysutils.image

  name     = "logrotate-hourly"
  schedule = "0 * * * *"
  command  = "/usr/sbin/logrotate /etc/logrotate.conf"

  depends_on = [sysutils_directory.rootfs]
}

# An absolute target is resolved inside the image: this link is correct
# when the image boots, and never points at the host's zoneinfo.
resource "sysutils_symlink" "localtime" {
  provider = sysutils.image

  path   = "/etc/localtime"
  target = "/usr/share/zoneinfo/Europe/Berlin"

  depends_on = [sysutils_directory.rootfs]
}
