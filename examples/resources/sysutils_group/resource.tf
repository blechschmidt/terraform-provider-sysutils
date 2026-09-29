# A system group for a service, with a fixed member list.
resource "sysutils_group" "app" {
  name    = "appsvc"
  system  = true
  members = ["root"]
}

# Use the group as a user's primary group. Referencing the gid makes
# Terraform create the group before the user and delete it afterwards.
resource "sysutils_user" "app" {
  name        = "appsvc"
  gid         = sysutils_group.app.gid
  system      = true
  shell       = "/usr/sbin/nologin"
  create_home = false
}

# A regular group with a fixed gid whose membership is managed elsewhere.
resource "sysutils_group" "developers" {
  name = "developers"
  gid  = 2100
}
