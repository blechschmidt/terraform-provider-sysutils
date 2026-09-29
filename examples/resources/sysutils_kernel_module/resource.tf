# Kubernetes nodes need bridged traffic to pass through iptables.
resource "sysutils_kernel_module" "br_netfilter" {
  name = "br_netfilter"
}

resource "sysutils_sysctl" "bridge_nf_call_iptables" {
  # The parameter only exists once the module is loaded.
  name  = "net.bridge.bridge-nf-call-iptables"
  value = "1"

  depends_on = [sysutils_kernel_module.br_netfilter]
}

# A module with parameters. They are passed to modprobe and written to
# /etc/modprobe.d/dummy.conf.
resource "sysutils_kernel_module" "dummy" {
  name = "dummy"
  parameters = {
    numdummies = "2"
  }
}

# Loaded now, but not at boot.
resource "sysutils_kernel_module" "wireguard" {
  name    = "wireguard"
  persist = false
}
