output "config_file" {
  description = "Host path of the rendered configuration file."
  value       = "${sysutils_directory.root.path}${sysutils_template_file.config.path}"
}

output "config_sha256" {
  description = "SHA-256 checksum of the rendered configuration."
  value       = sysutils_template_file.config.content_sha256
}

output "service_uid" {
  description = "UID of the service user."
  value       = sysutils_user.service.uid
}

output "service_gid" {
  description = "GID of the service group."
  value       = sysutils_group.service.gid
}

output "unit_path" {
  description = "Path of the systemd unit file, or null if systemd is not managed."
  value       = one(sysutils_systemd_unit.service[*].path)
}
