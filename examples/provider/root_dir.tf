# Build a container root filesystem in /srv/images/web/rootfs. Every path
# below is inside that directory: /etc/nginx/nginx.conf is written to
# /srv/images/web/rootfs/etc/nginx/nginx.conf.
provider "sysutils" {
  alias    = "image"
  root_dir = "/srv/images/web/rootfs"
}

resource "sysutils_directory" "nginx_conf" {
  provider = sysutils.image

  path = "/etc/nginx"
  # Use numeric IDs: names are looked up in the host's user database, not
  # in the image's /etc/passwd.
  owner = "0"
  group = "0"
}

resource "sysutils_file" "nginx_conf" {
  provider = sysutils.image

  path    = "${sysutils_directory.nginx_conf.path}/nginx.conf"
  content = "worker_processes auto;\n"
}

# An absolute target is resolved inside the image, like in a chroot.
resource "sysutils_symlink" "localtime" {
  provider = sysutils.image

  path   = "/etc/localtime"
  target = "/usr/share/zoneinfo/UTC"
}
