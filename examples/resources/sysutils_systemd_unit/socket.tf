# Socket activation: systemd listens on the port and starts the service on
# the first connection. Only the socket is enabled and started.
resource "sysutils_systemd_unit" "echo_service" {
  name = "echo.service"

  unit = {
    requires = ["echo.socket"]
  }

  service = {
    exec_start = ["/usr/local/bin/echo-server"]
    # The listening socket is passed as file descriptor 3.
    non_blocking = true
  }
}

resource "sysutils_systemd_unit" "echo_socket" {
  name    = "echo.socket"
  enabled = true
  state   = "running"

  socket = {
    listen_stream = ["127.0.0.1:7000", "[::1]:7000"]
    backlog       = 128
  }

  install = {
    wanted_by = ["sockets.target"]
  }

  depends_on = [sysutils_systemd_unit.echo_service]
}
