# A template unit. "%i" is replaced by the instance name, so
# queue@emails.service runs the worker for the "emails" queue.
resource "sysutils_systemd_unit" "queue" {
  name = "queue@.service"

  unit = {
    description = "Queue worker for %i"
  }

  service = {
    exec_start = ["/usr/local/bin/worker --queue %i"]
    restart    = "on-failure"
  }

  install = {
    wanted_by = ["multi-user.target"]
  }
}

# Instances are enabled and started with sysutils_service. Changing the
# template restarts the running instances.
resource "sysutils_service" "queue" {
  for_each = toset(["emails", "reports"])

  name    = "queue@${each.key}.service"
  enabled = true
  state   = "running"

  depends_on = [sysutils_systemd_unit.queue]
}
