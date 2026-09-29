resource "sysutils_directory" "app" {
  path  = "/srv/app"
  owner = "appsvc"
  group = "appsvc"
  mode  = "0750"
}

resource "sysutils_file" "app_config" {
  # Referencing the directory's path makes Terraform create the directory
  # first and remove the file before the directory on destroy.
  path    = "${sysutils_directory.app.path}/app.conf"
  content = "listen = 127.0.0.1:8080\n"
  mode    = "0640"
  owner   = "appsvc"
  group   = "appsvc"
}
