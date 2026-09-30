variable "exporter_version" {
  description = "Release to install, such as 1.8.2."
  type        = string
}

variable "exporter_sha256" {
  description = "SHA-256 of the release tarball, from the release's sha256sums.txt."
  type        = string
}
