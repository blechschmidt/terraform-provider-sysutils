# With root_dir, the os_* attributes and package_manager describe the image
# tree. The attributes listed in live_facts still describe the machine that
# builds it.
provider "sysutils" {
  alias    = "image"
  root_dir = "/srv/images/web"
}

data "sysutils_host" "image" {
  provider = sysutils.image
}

output "image_distribution" {
  value = "${data.sysutils_host.image.os_id} ${data.sysutils_host.image.os_version_id}"
}
