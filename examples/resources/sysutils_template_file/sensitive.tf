variable "db_password" {
  type      = string
  sensitive = true
}

# Secrets go into sensitive_vars. The rendered content is then only exposed
# as the sensitive rendered_sensitive attribute, and never shown in plans.
resource "sysutils_template_file" "db_credentials" {
  path  = "/etc/app/db.env"
  mode  = "0600"
  owner = "app"

  template = <<-EOT
    DB_USER={{ .user }}
    DB_PASSWORD={{ .password }}
  EOT

  vars           = { user = "app" }
  sensitive_vars = { password = var.db_password }
}
