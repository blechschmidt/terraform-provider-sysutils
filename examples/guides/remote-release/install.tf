resource "sysutils_archive_extract" "exporter" {
  source           = sysutils_remote_file.exporter.path
  destination      = "/opt/exporter/${var.exporter_version}"
  strip_components = 1

  lifecycle {
    create_before_destroy = true
  }
}

resource "sysutils_symlink" "exporter" {
  path   = "/usr/local/bin/exporter"
  target = "${sysutils_archive_extract.exporter.destination}/exporter"
}
