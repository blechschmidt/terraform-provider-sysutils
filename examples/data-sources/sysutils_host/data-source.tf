data "sysutils_host" "this" {}

output "host" {
  value = {
    fqdn            = data.sysutils_host.this.fqdn
    distribution    = data.sysutils_host.this.os_pretty_name # "Debian GNU/Linux 12 (bookworm)"
    kernel          = data.sysutils_host.this.kernel_release
    architecture    = data.sysutils_host.this.architecture # "x86_64", "aarch64"
    cpus            = data.sysutils_host.this.cpu_count
    memory_gib      = floor(data.sysutils_host.this.memory_total_bytes / 1073741824)
    init_system     = data.sysutils_host.this.init_system     # "systemd", "openrc", "sysvinit" or null
    package_manager = data.sysutils_host.this.package_manager # "apt", "dnf", "yum", "apk" or null
  }
}
