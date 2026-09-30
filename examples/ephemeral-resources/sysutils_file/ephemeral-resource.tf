# Needs Terraform 1.11 or later, or OpenTofu 1.11 or later, to pass the
# value to a write-only argument. The secret file must belong to root (or
# the user running Terraform) and must not be writable by its group or
# others, and so must every directory on the way to it.
ephemeral "sysutils_file" "db_password" {
  path = "/etc/app/secrets/db-password"
}

# The password is read during plan and apply, written to the file, and
# never stored in the plan or the state. Increase content_wo_version to
# write a changed password.
resource "sysutils_file" "app_env" {
  path               = "/etc/app/app.env"
  content_wo         = "DB_PASSWORD=${trimspace(ephemeral.sysutils_file.db_password.content)}\n"
  content_wo_version = 1
  mode               = "0600"
}
