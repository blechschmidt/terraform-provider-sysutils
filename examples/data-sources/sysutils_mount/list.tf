# Every NFS mount, whether or not it is in /etc/fstab.
data "sysutils_mount" "nfs" {
  fstypes = ["nfs", "nfs4"]
}

output "nfs_mounts" {
  value = {
    for m in data.sysutils_mount.nfs.mounts : m.path => m.source
  }
}

# Mounts made by hand that a reboot would lose.
output "not_in_fstab" {
  value = [
    for m in data.sysutils_mount.nfs.mounts : m.path if !m.in_fstab
  ]
}
