# Unpack a release tarball into /opt/app. The tarball's top-level directory
# (app-1.4.2/) is stripped, and everything belongs to the service user.
resource "sysutils_archive_extract" "app" {
  source           = "${path.module}/files/app-1.4.2-linux-amd64.tar.gz"
  destination      = "/opt/app"
  strip_components = 1
  owner            = "appsvc"
  group            = "appsvc"
  directory_mode   = "0750"
}

resource "sysutils_symlink" "app_bin" {
  path   = "/usr/local/bin/app"
  target = "/opt/app/bin/app"

  # Create the link only after the binary is in place.
  depends_on = [sysutils_archive_extract.app]
}
