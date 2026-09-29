# Installed if missing. On a fresh image, refresh the package index first.
resource "sysutils_package" "nginx" {
  name         = "nginx"
  update_cache = true
}

# Kept at the newest version in the package index.
resource "sysutils_package" "ca_certificates" {
  name  = "ca-certificates"
  state = "latest"
}

# Pinned to an exact version: upgrades made outside Terraform are reverted.
resource "sysutils_package" "tree" {
  name    = "tree"
  version = "2.1.1-2ubuntu3"
  manager = "apt"
}

# Removed if installed.
resource "sysutils_package" "telnet" {
  name  = "telnet"
  state = "absent"
}

# Installed before Terraform managed it: keep it when the resource is
# destroyed.
resource "sysutils_package" "openssh_server" {
  name              = "openssh-server"
  remove_on_destroy = false
}

output "nginx_version" {
  value = sysutils_package.nginx.installed_version
}
