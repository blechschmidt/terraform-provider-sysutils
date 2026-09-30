data "sysutils_host" "facts" {}

locals {
  # True on Debian and on distributions derived from it, such as Ubuntu.
  debian_like = contains(
    concat([data.sysutils_host.facts.os_id], data.sysutils_host.facts.os_id_like),
    "debian",
  )
}

# Only Debian-like systems read /etc/default/grub.d; skip it elsewhere.
resource "sysutils_file" "grub_console" {
  count = local.debian_like ? 1 : 0

  path    = "/etc/default/grub.d/50-console.cfg"
  content = "GRUB_TERMINAL=console\n"
}

# Size a cache by the host's memory: a quarter of it, in MiB.
resource "sysutils_file" "cache_config" {
  path    = "/etc/app/cache.conf"
  content = "cache_mb = ${floor(data.sysutils_host.facts.memory_total_bytes / 4 / 1048576)}\n"
}

# Start the unit only where systemd runs, not in containers.
resource "sysutils_service" "app" {
  count = data.sysutils_host.facts.init_system == "systemd" ? 1 : 0

  name  = "app"
  state = "running"
}
