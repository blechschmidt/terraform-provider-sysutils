data "sysutils_mount" "backup" {
  path = "/mnt/backup"
}

# Only write the backup job's configuration once the backup disk is
# mounted, so that it never fills the root file system instead.
resource "sysutils_file" "backup_target" {
  count = data.sysutils_mount.backup.mounted ? 1 : 0

  path    = "/etc/backup/target"
  content = "/mnt/backup\n"
}
