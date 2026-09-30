resource "sysutils_directory" "releases" {
  path = "/opt/myapp/releases"
  mode = "0755"
}

# Each version is unpacked into a directory of its own. The code belongs to
# root, so the service cannot modify it. The tarball's top-level directory
# (myapp-1.4.2/) is stripped.
resource "sysutils_archive_extract" "release" {
  source           = "${path.module}/files/myapp-${var.app_version}.tar.gz"
  destination      = "${sysutils_directory.releases.path}/${var.app_version}"
  strip_components = 1

  # On an upgrade, extract the new version and switch the symlink to it
  # before the old version is removed.
  lifecycle {
    create_before_destroy = true
  }
}

# The stable path the unit runs the service from. Switching it to a new
# release is a single rename.
resource "sysutils_symlink" "current" {
  path   = "/opt/myapp/current"
  target = sysutils_archive_extract.release.destination
}
