# Read the distribution of an image tree rather than of the running host,
# which the sysutils_host data source describes.
data "sysutils_file" "image_os_release" {
  path = "/srv/images/web/etc/os-release"
}

locals {
  image_os = provider::sysutils::parse_os_release(data.sysutils_file.image_os_release.content)
}

output "image_is_debian_family" {
  value = local.image_os["ID"] == "debian" || contains(split(" ", lookup(local.image_os, "ID_LIKE", "")), "debian")
}
