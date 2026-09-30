data "sysutils_file" "fstab" {
  path = "/etc/fstab"
}

locals {
  # Blank lines and comments parse to null and are skipped.
  fstab = [
    for line in split("\n", data.sysutils_file.fstab.content) :
    provider::sysutils::parse_fstab_line(line)
    if provider::sysutils::parse_fstab_line(line) != null
  ]
}

output "read_only_mount_points" {
  value = [for e in local.fstab : e.mount_point if contains(e.options, "ro")]
}

output "fstab_by_mount_point" {
  value = { for e in local.fstab : e.mount_point => e }
}
