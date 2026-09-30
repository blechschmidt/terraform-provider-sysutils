# Without a published checksum, allow_unverified accepts whatever the server
# returns. The file is downloaded again when url changes or the local copy is
# modified; force_redownload = true would fetch it on every apply.
resource "sysutils_remote_file" "ca_bundle" {
  url              = "https://pki.example.com/internal-ca.pem"
  path             = "/usr/local/share/ca-certificates/internal-ca.crt"
  allow_unverified = true
}
