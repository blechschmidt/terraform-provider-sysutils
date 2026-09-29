data "sysutils_file" "machine_id" {
  path = "/etc/machine-id"
}

output "machine_id" {
  value = trimspace(data.sysutils_file.machine_id.content)
}
