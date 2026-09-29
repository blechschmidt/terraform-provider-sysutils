# Render a configuration file with Go text/template syntax. {{ }} does not
# clash with Terraform's own ${} interpolation, so the template can be
# written inline in a heredoc.
resource "sysutils_template_file" "app_config" {
  path  = "/etc/app/app.conf"
  mode  = "0640"
  owner = "root"
  group = "app"

  template = <<-EOT
    listen = {{ .listen }}:{{ .port }}
    log_level = {{ .log_level | default "info" }}
    {{ range .upstreams -}}
    upstream = {{ . }}
    {{ end -}}
  EOT

  vars = {
    listen    = "0.0.0.0"
    port      = 8080
    log_level = ""
    upstreams = ["10.0.0.11:9000", "10.0.0.12:9000"]
  }
}

# Restart the service whenever the rendered content changes.
resource "sysutils_exec" "restart_app" {
  command = ["systemctl", "restart", "app"]
  triggers = {
    config = sysutils_template_file.app_config.content_sha256
  }
}
