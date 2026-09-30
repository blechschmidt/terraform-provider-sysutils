# Keep a resolver configuration that something else would overwrite, such
# as a DHCP client, as written: with the immutable flag, nobody, not even
# root, can change, replace or remove the file.
resource "sysutils_file" "resolv_conf" {
  path    = "/etc/resolv.conf"
  content = "nameserver 10.0.0.53\nsearch internal\n"
}

resource "sysutils_file_attributes" "resolv_conf" {
  path       = sysutils_file.resolv_conf.path
  attributes = ["i"]
}

# An audit log that can only be appended to, and a cache directory that
# backups with dump skip and whose files get no access time updates.
resource "sysutils_file_attributes" "audit_log" {
  path       = "/var/log/app/audit.log"
  attributes = ["a"]
}

resource "sysutils_directory" "cache" {
  path = "/var/cache/app"
}

resource "sysutils_file_attributes" "cache" {
  path       = sysutils_directory.cache.path
  attributes = ["d", "A"]
}
