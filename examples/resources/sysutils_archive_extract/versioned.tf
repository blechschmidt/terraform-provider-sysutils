# Keep each release in its own directory and switch between them with a
# symlink. Changing the version extracts the new release into a new
# directory, switches the link, and then removes the old release.
variable "app_version" {
  type    = string
  default = "1.4.2"
}

resource "sysutils_archive_extract" "release" {
  source           = "${path.module}/files/app-${var.app_version}.zip"
  destination      = "/opt/app/releases/${var.app_version}"
  strip_components = 1
  file_mode        = "0644"

  # Refuse unexpectedly large releases.
  max_size    = 268435456 # 256 MiB
  max_entries = 20000

  lifecycle {
    create_before_destroy = true
  }
}

resource "sysutils_symlink" "current" {
  path   = "/opt/app/current"
  target = sysutils_archive_extract.release.destination
}
