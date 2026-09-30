# Read the account databases of an unpacked image instead of the host's.
provider "sysutils" {
  alias    = "image"
  root_dir = "/srv/images/web/rootfs"
}

data "sysutils_groups" "image_system" {
  provider = sysutils.image
  gid_max  = 999
}

output "image_system_groups" {
  value = data.sysutils_groups.image_system.names
}
