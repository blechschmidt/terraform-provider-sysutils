# Map a private address to a fully qualified name and a short alias.
resource "sysutils_hosts_entry" "db" {
  ip        = "10.0.0.5"
  hostnames = ["db.internal.example.com", "db"]
  comment   = "primary database"
}

# IPv6 works the same way.
resource "sysutils_hosts_entry" "db_v6" {
  ip        = "fd00::5"
  hostnames = ["db.internal.example.com", "db"]
}

# Point a name at loopback. The distribution's "127.0.0.1 localhost" line
# has another canonical name and is left alone.
resource "sysutils_hosts_entry" "app_local" {
  ip        = "127.0.0.1"
  hostnames = ["app.local"]
}

# A hosts file other than /etc/hosts, such as one served by dnsmasq's
# addn-hosts option.
resource "sysutils_hosts_entry" "printer" {
  path      = "/etc/dnsmasq.hosts"
  ip        = "192.168.1.20"
  hostnames = ["printer.lan", "printer"]
}
