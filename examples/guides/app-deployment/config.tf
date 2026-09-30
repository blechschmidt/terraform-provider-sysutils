# Readable by the service, writable only by root.
resource "sysutils_directory" "config" {
  path  = "/etc/myapp"
  mode  = "0750"
  owner = "root"
  group = sysutils_group.myapp.name
}

# The template is rendered and checked during plan, so the plan shows the
# exact change to the file. Go template syntax does not clash with
# Terraform's ${} interpolation.
resource "sysutils_template_file" "config" {
  path  = "${sysutils_directory.config.path}/myapp.toml"
  mode  = "0640"
  owner = "root"
  group = sysutils_group.myapp.name

  template = <<-EOT
    listen = "127.0.0.1:{{ .port }}"
    data_dir = "{{ .data_dir }}"
    {{ range .upstreams -}}
    [[upstream]]
    address = "{{ . }}"
    {{ end -}}
  EOT

  vars = {
    port      = var.listen_port
    data_dir  = sysutils_directory.state.path
    upstreams = ["10.0.0.11:9000", "10.0.0.12:9000"]
  }
}
