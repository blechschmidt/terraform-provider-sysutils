resource "sysutils_file" "nginx_site" {
  count = data.sysutils_directory.nginx_conf.exists ? 1 : 0

  path    = "${data.sysutils_directory.nginx_conf.path}/app.conf"
  content = "server { listen 8080; }\n"
  # Match the ownership of the directory we are writing into.
  owner = data.sysutils_directory.nginx_conf.owner
  group = data.sysutils_directory.nginx_conf.group
}
