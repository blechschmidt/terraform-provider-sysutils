# A binary key: content is null for files that are not valid UTF-8, so
# pass content_base64 on and decode it again.
ephemeral "sysutils_file" "tls_key" {
  path     = "/etc/app/secrets/tls.key.der"
  max_size = 65536
}

resource "sysutils_file" "tls_key" {
  path               = "/etc/nginx/tls/app.key.der"
  content_wo         = base64decode(ephemeral.sysutils_file.tls_key.content_base64)
  content_wo_version = 1
  mode               = "0600"
}
