resource "sysutils_directory" "scratch" {
  path          = "/var/lib/scratch"
  mode          = "1777"
  force_destroy = true
}
