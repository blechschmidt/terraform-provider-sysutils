data "sysutils_service" "firewalld" {
  name = "firewalld"
}

# Stop and disable firewalld where it is installed, so that it does not
# replace the rules of sysutils_firewall_rule. "== true" also handles hosts
# without a supported init system, where exists is null.
resource "sysutils_service" "firewalld" {
  count = data.sysutils_service.firewalld.exists == true ? 1 : 0

  name    = "firewalld"
  enabled = false
  state   = "stopped"
}
