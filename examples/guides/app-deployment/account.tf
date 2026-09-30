resource "sysutils_group" "myapp" {
  name   = "myapp"
  system = true
}

resource "sysutils_user" "myapp" {
  name        = "myapp"
  gid         = sysutils_group.myapp.gid
  system      = true
  shell       = "/usr/sbin/nologin"
  home        = "/var/lib/myapp"
  create_home = false
}

# The only place the service may write to.
resource "sysutils_directory" "state" {
  path  = sysutils_user.myapp.home
  mode  = "0700"
  owner = sysutils_user.myapp.name
  group = sysutils_group.myapp.name
}
