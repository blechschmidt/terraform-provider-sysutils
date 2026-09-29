data "sysutils_file" "ca_cert" {
  path = "/etc/ssl/private/ca.der"
}

resource "sysutils_file" "ca_copy" {
  path           = "/opt/app/ca.der"
  content_base64 = data.sysutils_file.ca_cert.content_base64
  mode           = data.sysutils_file.ca_cert.mode
  owner          = data.sysutils_file.ca_cert.owner
  group          = data.sysutils_file.ca_cert.group
}

resource "sysutils_exec" "reload" {
  command = ["systemctl", "restart", "app"]
  triggers = {
    ca = data.sysutils_file.ca_cert.content_sha256
  }
}
