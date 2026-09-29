resource "sysutils_exec" "register" {
  command         = ["/usr/local/bin/inventory", "register", "--host", "web-1"]
  destroy_command = ["/usr/local/bin/inventory", "deregister", "--host", "web-1"]

  # Both commands run in this directory with this environment.
  working_directory = "/var/lib/inventory"
  environment = {
    INVENTORY_URL = "https://inventory.example.com"
  }
  timeout = "30s"
}
