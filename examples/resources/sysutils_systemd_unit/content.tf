# The unit file as text, inline or from a file next to the configuration.
resource "sysutils_systemd_unit" "worker" {
  name  = "worker.service"
  state = "running"

  content = <<-EOT
    [Unit]
    Description=Queue worker

    [Service]
    ExecStart=/usr/local/bin/worker
    Restart=always
  EOT
}

resource "sysutils_systemd_unit" "cleanup" {
  name   = "cleanup.service"
  source = "${path.module}/units/cleanup.service"
}
