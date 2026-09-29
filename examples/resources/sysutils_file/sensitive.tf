variable "db_password" {
  type      = string
  sensitive = true
}

# The plan shows `sensitive_content = (sensitive value)`; changes to the
# file, including edits made outside Terraform, appear only as changes to
# content_sha256 and content_md5.
resource "sysutils_file" "db_credentials" {
  path              = "/etc/app/db.env"
  sensitive_content = "DB_PASSWORD=${var.db_password}\n"
  mode              = "0600"
}
