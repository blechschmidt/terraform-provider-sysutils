variable "exporter_sha256" {
  description = "SHA-256 of the release tarball, as published next to it."
  type        = string
}

# Download a release tarball, pinned by its published checksum. A download
# that does not match is discarded before it reaches path.
resource "sysutils_remote_file" "exporter" {
  url      = "https://downloads.example.com/exporter/v1.8.2/exporter-1.8.2.linux-amd64.tar.gz"
  path     = "/var/cache/downloads/exporter-1.8.2.linux-amd64.tar.gz"
  checksum = "sha256:${var.exporter_sha256}"
  mode     = "0644"
  owner    = "root"
  group    = "root"
}
