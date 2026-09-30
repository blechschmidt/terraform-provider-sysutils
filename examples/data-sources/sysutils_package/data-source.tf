data "sysutils_package" "nginx" {
  name = "nginx"
}

output "nginx" {
  value = {
    installed    = data.sysutils_package.nginx.installed
    version      = data.sysutils_package.nginx.version      # null if not installed
    architecture = data.sysutils_package.nginx.architecture # "amd64", "x86_64", "noarch", ...
    available    = data.sysutils_package.nginx.available_version
    manager      = data.sysutils_package.nginx.package_manager # "apt", "dnf", "yum" or "apk"
  }
}
