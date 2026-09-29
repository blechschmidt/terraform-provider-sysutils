data "sysutils_user" "www" {
  name = "www-data"
}

# Resolve the name of the user's primary group.
data "sysutils_group" "www" {
  gid = data.sysutils_user.www.gid
}

# Give a directory to the web server user, whatever its uid is on this host.
resource "sysutils_directory" "webroot" {
  path  = "/srv/www"
  owner = data.sysutils_user.www.name
  group = data.sysutils_group.www.name
  mode  = "0750"
}

output "www_home" {
  value = data.sysutils_user.www.home
}
