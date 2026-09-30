# Kernel parameters from common hardening baselines. All of them go into
# one file, which systemd-sysctl (or the sysctl init script) applies at boot.
locals {
  sysctl_file = "/etc/sysctl.d/90-hardening.conf"

  hardening_sysctls = {
    # Hide kernel addresses and the kernel log from unprivileged users.
    "kernel.kptr_restrict"  = "2"
    "kernel.dmesg_restrict" = "1"
    # No core dumps of setuid programs, and no link tricks in sticky
    # directories such as /tmp.
    "fs.suid_dumpable"       = "0"
    "fs.protected_symlinks"  = "1"
    "fs.protected_hardlinks" = "1"
    # Drop spoofed packets and ignore ICMP redirects and source routing.
    "net.ipv4.conf.all.rp_filter"           = "1"
    "net.ipv4.conf.all.accept_redirects"    = "0"
    "net.ipv4.conf.all.send_redirects"      = "0"
    "net.ipv4.conf.all.accept_source_route" = "0"
    "net.ipv6.conf.all.accept_redirects"    = "0"
    "net.ipv4.tcp_syncookies"               = "1"
  }
}

resource "sysutils_sysctl" "hardening" {
  for_each = local.hardening_sysctls

  name  = each.key
  value = each.value
  file  = local.sysctl_file
}
