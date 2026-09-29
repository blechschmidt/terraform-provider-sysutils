data "sysutils_group" "docker" {
  name = "docker"
}

output "docker_gid" {
  value = data.sysutils_group.docker.gid
}

output "docker_users" {
  value = data.sysutils_group.docker.members
}
