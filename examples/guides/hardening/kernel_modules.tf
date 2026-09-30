# File systems and network protocols the host never needs. "install <name>
# /bin/false" makes every attempt to load the module fail, including
# automatic loading when a program opens such a socket or file system;
# "blacklist" alone only stops loading by alias.
locals {
  disabled_modules = ["cramfs", "freevxfs", "hfs", "hfsplus", "jffs2", "dccp", "sctp", "rds", "tipc"]
}

resource "sysutils_file" "disabled_modules" {
  path    = "/etc/modprobe.d/90-disabled.conf"
  content = join("", [for m in local.disabled_modules : "install ${m} /bin/false\nblacklist ${m}\n"])
  mode    = "0644"
  owner   = "root"
  group   = "root"
}

# Pass bridged traffic (containers, VMs) through the firewall, too. The
# module is loaded now and listed in /etc/modules-load.d, which systemd
# processes before it applies sysctl.d at boot.
resource "sysutils_kernel_module" "br_netfilter" {
  name = "br_netfilter"
}

resource "sysutils_sysctl" "bridge_filtering" {
  for_each = toset(["net.bridge.bridge-nf-call-iptables", "net.bridge.bridge-nf-call-ip6tables"])

  name  = each.key
  value = "1"
  file  = local.sysctl_file

  # The parameters only exist while the module is loaded.
  depends_on = [sysutils_kernel_module.br_netfilter]
}
