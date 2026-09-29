# Route packets between interfaces, now and after every reboot. The entry
# goes to /etc/sysctl.d/99-terraform.conf.
resource "sysutils_sysctl" "ip_forward" {
  name  = "net.ipv4.ip_forward"
  value = "1"
}

# A multi-value parameter, persisted in a file of its own.
resource "sysutils_sysctl" "port_range" {
  name  = "net.ipv4.ip_local_port_range"
  value = "1024 65535"
  file  = "/etc/sysctl.d/60-ports.conf"
}

# A per-interface parameter. The "/" stands for the "." in the name of the
# VLAN interface eth0.100.
resource "sysutils_sysctl" "vlan_rp_filter" {
  name  = "net.ipv4.conf.eth0/100.rp_filter"
  value = "2"
}

# Only for the running kernel; nothing is persisted.
resource "sysutils_sysctl" "swappiness" {
  name    = "vm.swappiness"
  value   = "10"
  persist = false
}
