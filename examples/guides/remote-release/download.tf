locals {
  exporter_release = "exporter-${var.exporter_version}.linux-amd64"
}

resource "sysutils_directory" "downloads" {
  path = "/var/cache/exporter"
  mode = "0700"
}

# The tarball is verified before it is renamed into place, so a truncated or
# tampered download never reaches the extraction below.
resource "sysutils_remote_file" "exporter" {
  url      = "https://downloads.example.com/exporter/v${var.exporter_version}/${local.exporter_release}.tar.gz"
  path     = "${sysutils_directory.downloads.path}/${local.exporter_release}.tar.gz"
  checksum = "sha256:${var.exporter_sha256}"
}
