data "sysutils_sysctl" "ip_forward" {
  name = "net.ipv4.ip_forward"
}

output "ip_forward" {
  value = {
    exists          = data.sysutils_sysctl.ip_forward.exists
    value           = data.sysutils_sysctl.ip_forward.value           # "1"
    persisted_value = data.sysutils_sysctl.ip_forward.persisted_value # "1", or null
    persisted_file  = data.sysutils_sysctl.ip_forward.persisted_file  # "/etc/sysctl.d/99-terraform.conf"
  }
}
