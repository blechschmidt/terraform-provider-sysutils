# Only ensure this one entry, and leave entries that other tools or
# administrators add alone. Destroy removes only this entry.
resource "sysutils_file_acl" "backup_read" {
  path      = "/etc/app/app.conf"
  exclusive = false

  entries = [
    { type = "user", name = "backup", permissions = "r--" },
  ]
}
