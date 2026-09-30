# The packages every host gets. The names are the same on Debian, Ubuntu,
# Fedora, RHEL and Alpine.
resource "sysutils_package" "base" {
  for_each = toset(["openssh-server", "sudo", "chrony", "curl"])

  name = each.key
  # Refresh the package index before installing, as a fresh image has none.
  # It is refreshed once per run, however many packages ask for it.
  update_cache = true
  # The host needs these even when this configuration no longer manages them.
  remove_on_destroy = false
}

# The service names differ between distributions: "ssh" and "chrony" on
# Debian and Ubuntu, "sshd" and "chronyd" on Fedora, RHEL and Alpine.
variable "services" {
  description = "Services to enable and start, by the package that installs them."
  type        = map(string)
  default = {
    openssh-server = "ssh"
    chrony         = "chrony"
  }
}

resource "sysutils_service" "base" {
  for_each = var.services

  name    = each.value
  enabled = true
  state   = "running"

  # A package's service only exists once the package is installed.
  depends_on = [sysutils_package.base]
}
