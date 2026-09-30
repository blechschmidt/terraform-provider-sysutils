resource "sysutils_systemd_unit" "app" {
  name   = "app.service"
  source = "${path.module}/app.service"
}

resource "sysutils_service" "app" {
  name    = sysutils_systemd_unit.app.name
  enabled = true
  state   = "running"
}
