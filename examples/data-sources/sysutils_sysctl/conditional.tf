data "sysutils_sysctl" "conntrack_max" {
  name = "net.netfilter.nf_conntrack_max"
}

# nf_conntrack_max only exists while the nf_conntrack module is loaded.
# "== true" also handles a provider with root_dir, where exists is null.
resource "sysutils_sysctl" "conntrack_max" {
  count = data.sysutils_sysctl.conntrack_max.exists == true ? 1 : 0

  name  = "net.netfilter.nf_conntrack_max"
  value = "262144"
}
