# A data disk, mounted now and at boot.
resource "sysutils_directory" "data" {
  path = "/srv/data"
}

resource "sysutils_mount" "data" {
  path    = sysutils_directory.data.path
  device  = "UUID=3e6be9de-8139-11d1-9106-a43f08d823a6"
  fstype  = "ext4"
  options = ["noatime", "nodev", "nofail"]
  pass    = 2
}

# A size-limited scratch area in memory that is not persisted in /etc/fstab.
resource "sysutils_directory" "scratch" {
  path = "/srv/scratch"
}

resource "sysutils_mount" "scratch" {
  path    = sysutils_directory.scratch.path
  device  = "tmpfs"
  fstype  = "tmpfs"
  options = ["size=256m", "mode=1777", "nosuid", "nodev"]
  persist = false
}

# A read-only bind mount of the data disk's exports.
resource "sysutils_directory" "exports" {
  path = "/srv/exports"
}

resource "sysutils_mount" "exports" {
  path    = sysutils_directory.exports.path
  device  = "${sysutils_mount.data.path}/exports"
  fstype  = "none"
  options = ["bind", "ro"]
}

# An NFS share that is only recorded in /etc/fstab and mounted on first
# access by systemd's automounter.
resource "sysutils_mount" "archive" {
  path    = "/mnt/archive"
  device  = "nas.internal:/export/archive"
  fstype  = "nfs"
  options = ["noauto", "x-systemd.automount", "_netdev"]
  mounted = false
}
