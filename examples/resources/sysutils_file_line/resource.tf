# Add a static host entry to /etc/hosts. Destroy removes only this line.
resource "sysutils_file_line" "db_host" {
  path = "/etc/hosts"
  line = "10.0.0.5 db.internal db"
}

# Replace the (possibly commented-out) PermitRootLogin setting in place, so
# that it stays in the global section. If no line matches, the line is added
# after the last line matching insert_after, or at the end of the file.
resource "sysutils_file_line" "sshd_root_login" {
  path         = "/etc/ssh/sshd_config"
  regexp       = "^#?PermitRootLogin\\s"
  line         = "PermitRootLogin no"
  insert_after = "^#?Port\\s"
}

# Reload sshd whenever the managed line changes.
resource "sysutils_exec" "reload_sshd" {
  command = ["systemctl", "reload", "ssh"]
  triggers = {
    line = sysutils_file_line.sshd_root_login.line
  }
}
