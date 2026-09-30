terraform {
  required_version = ">= 1.5"

  required_providers {
    sysutils = {
      source = "blechschmidt/sysutils"
    }
  }
}

variable "rootfs" {
  description = "Root file system of the image being built, such as an unpacked base image."
  type        = string
  default     = "/srv/images/web/rootfs"
}

# The host itself. Only used to create the image's root directory.
provider "sysutils" {}

# Everything inside the image. "/etc/hostname" means <rootfs>/etc/hostname,
# and symlinks in the tree are resolved inside it, as in a chroot.
provider "sysutils" {
  alias    = "image"
  root_dir = var.rootfs
}

resource "sysutils_directory" "rootfs" {
  path = var.rootfs
  mode = "0755"
  # The tree is a build artifact: destroy deletes it with everything in it,
  # including the files and directories the resources below leave behind.
  force_destroy = true
}
