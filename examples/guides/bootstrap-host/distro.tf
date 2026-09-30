# Some packages have different names on each distribution. Read the host's
# distribution from /etc/os-release and pick the names for its family.
data "sysutils_host" "this" {}

locals {
  # The distribution and those it derives from, such as
  # ["ubuntu", "debian"] or ["rocky", "rhel", "centos", "fedora"].
  distro_lineage = concat([data.sysutils_host.this.os_id], data.sysutils_host.this.os_id_like)

  distro_family = (
    contains(local.distro_lineage, "debian") ? "debian" :
    contains(local.distro_lineage, "fedora") || contains(local.distro_lineage, "rhel") ? "redhat" :
    contains(local.distro_lineage, "alpine") ? "alpine" :
    data.sysutils_host.this.os_id
  )

  # dig and a cron daemon, by distribution family.
  distro_packages = {
    debian = ["bind9-dnsutils", "cron"]
    redhat = ["bind-utils", "cronie"]
    alpine = ["bind-tools", "cronie"]
  }
}

resource "sysutils_package" "distro" {
  # Indexing rather than lookup() makes an unsupported distribution fail
  # the plan instead of silently installing nothing.
  for_each = toset(local.distro_packages[local.distro_family])

  name              = each.key
  update_cache      = true
  remove_on_destroy = false
}
