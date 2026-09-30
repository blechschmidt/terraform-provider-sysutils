# Give the deploy user write access to one log file, and the monitoring
# group read access, without changing the file's owner or mode bits:
#
#   user::rw-
#   user:deploy:rw-
#   group::r--
#   group:monitoring:r--
#   mask::rw-
#   other::---
resource "sysutils_file_acl" "app_log" {
  path = "/var/log/app/app.log"

  entries = [
    { type = "user", name = "deploy", permissions = "rw-" },
    { type = "group", name = "monitoring", permissions = "r--" },
  ]
}

# A shared directory: the developers group can read and write it, and every
# file or directory created in it inherits the same access through the
# default ACL.
resource "sysutils_directory" "shared" {
  path  = "/srv/shared"
  mode  = "2770"
  group = "staff"
}

resource "sysutils_file_acl" "shared" {
  path = sysutils_directory.shared.path

  entries = [
    { type = "group", name = "developers", permissions = "rwx" },
  ]

  default_entries = [
    { type = "group", name = "developers", permissions = "rwx" },
    { type = "other", permissions = "---" },
  ]
}
