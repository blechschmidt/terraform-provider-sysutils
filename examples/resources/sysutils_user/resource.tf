# A system user that owns a long-running service.
resource "sysutils_user" "app" {
  name        = "appsvc"
  system      = true
  shell       = "/usr/sbin/nologin"
  create_home = false
  groups      = ["adm"]
}

# A regular user with a fixed UID and home directory.
resource "sysutils_user" "alice" {
  name        = "alice"
  uid         = 2001
  home        = "/home/alice"
  shell       = "/bin/bash"
  create_home = true
}
