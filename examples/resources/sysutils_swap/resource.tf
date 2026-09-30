# A 2 GiB swap file, enabled now and at boot.
resource "sysutils_swap" "file" {
  path     = "/swapfile"
  size_mib = 2048
}

# A swap partition, preferred over the swap file. The partition must be
# blank or already formatted as swap; set force = true to format a
# partition that still holds a file system.
resource "sysutils_swap" "partition" {
  path     = "/dev/disk/by-partuuid/0f3c7a1e-02"
  priority = 10
}

# A swap file for an image being built below root_dir: created and added
# to the image's /etc/fstab, but not enabled on the build host.
provider "sysutils" {
  alias    = "image"
  root_dir = "/srv/images/web/rootfs"
}

resource "sysutils_swap" "image" {
  provider = sysutils.image
  path     = "/swapfile"
  size_mib = 512
  enabled  = false
}
