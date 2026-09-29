data "sysutils_directory" "nginx_conf" {
  path = "/etc/nginx/conf.d"
}

output "nginx_installed" {
  value = data.sysutils_directory.nginx_conf.exists
}

output "nginx_conf_owner" {
  value = data.sysutils_directory.nginx_conf.owner
}
