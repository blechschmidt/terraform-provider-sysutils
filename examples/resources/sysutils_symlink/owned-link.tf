resource "sysutils_symlink" "config" {
  path   = "/home/alice/.app.conf"
  target = "/etc/app/alice.conf"
  owner  = "alice"
  group  = "alice"
}
