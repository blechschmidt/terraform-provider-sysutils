resource "sysutils_file" "hello" {
  path    = "/etc/hello.conf"
  content = "greeting = hi\n"
  mode    = "0644"
  owner   = "root"
  group   = "root"
}

# Binary content, e.g. from filebase64() or another resource.
resource "sysutils_file" "logo" {
  path           = "/srv/www/logo.png"
  content_base64 = filebase64("${path.module}/files/logo.png")
}

# Copy a local file. Editing files/app.jar triggers an update on the next plan.
resource "sysutils_file" "app" {
  path   = "/opt/app/app.jar"
  source = "${path.module}/files/app.jar"
  mode   = "0640"
}

# Restart a service whenever the file's contents change.
resource "sysutils_exec" "reload" {
  command = ["systemctl", "restart", "app"]
  triggers = {
    jar = sysutils_file.app.content_sha256
  }
}
