# The NTP pool every host synchronises with, instead of the distribution's.
variable "ntp_pool" {
  description = "NTP pool for chrony, such as a pool.ntp.org zone or your own time servers' DNS name."
  type        = string
  default     = "europe.pool.ntp.org"
}

locals {
  # chrony's configuration file, by distribution family.
  chrony_conf = {
    debian = "/etc/chrony/chrony.conf"
    redhat = "/etc/chrony.conf"
    alpine = "/etc/chrony/chrony.conf"
  }
}

# Replace the distribution's pool line, and restart chrony whenever the line
# is added or changed, so that the new pool is used right away.
resource "sysutils_file_line" "ntp_pool" {
  path   = local.chrony_conf[local.distro_family]
  line   = "pool ${var.ntp_pool} iburst"
  regexp = "^pool "

  # The file comes with the chrony package.
  depends_on = [sysutils_package.base]

  lifecycle {
    action_trigger {
      events  = [after_create, after_update]
      actions = [action.sysutils_service.restart_chrony]
    }
  }
}

action "sysutils_service" "restart_chrony" {
  config {
    # Referencing the service resource orders the restart after chrony has
    # been enabled and started.
    name   = sysutils_service.base["chrony"].name
    action = "restart"
  }
}
