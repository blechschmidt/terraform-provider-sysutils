# /etc/os-release is usually a symlink to /usr/lib/os-release.
data "sysutils_file" "os_release" {
  path            = "/etc/os-release"
  follow_symlinks = true
}
