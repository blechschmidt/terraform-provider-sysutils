data "sysutils_sysctl" "conf_all" {
  prefix = "net.ipv4.conf.all"
}

# Parameters whose running value differs from the one set at boot, for
# example because someone changed them with "sysctl -w".
output "not_persisted" {
  value = {
    for key, value in data.sysutils_sysctl.conf_all.values :
    key => { running = value, at_boot = data.sysutils_sysctl.conf_all.persisted_values[key] }
    if contains(keys(data.sysutils_sysctl.conf_all.persisted_values), key) && data.sysutils_sysctl.conf_all.persisted_values[key] != value
  }
}
