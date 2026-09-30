data "sysutils_package" "docker" {
  name = "docker-ce"
}

# Configure the Docker daemon only on hosts that have Docker installed,
# without installing it anywhere.
resource "sysutils_file" "docker_daemon" {
  count = data.sysutils_package.docker.installed ? 1 : 0

  path    = "/etc/docker/daemon.json"
  content = jsonencode({ "log-driver" = "journald" })
}
