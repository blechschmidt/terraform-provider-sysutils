data "sysutils_mount" "data" {
  path = "/srv/data"
}

output "data_mount" {
  value = {
    mounted       = data.sysutils_mount.data.mounted
    source        = data.sysutils_mount.data.source  # "/dev/sdb1"
    fstype        = data.sysutils_mount.data.fstype  # "ext4"
    options       = data.sysutils_mount.data.options # ["rw", "noatime"]
    read_only     = data.sysutils_mount.data.read_only
    in_fstab      = data.sysutils_mount.data.in_fstab
    fstab_options = data.sysutils_mount.data.fstab_options # ["defaults", "noatime"]
  }
}
