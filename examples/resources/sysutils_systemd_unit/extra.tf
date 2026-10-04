# Directives that the provider does not know yet go into "extra" of their
# section, and custom sections into "extra_sections". An empty string
# writes an empty assignment, which resets a list such as ExecStart=.
resource "sysutils_systemd_unit" "custom" {
  name = "custom.service"

  unit = {
    description = "Uses directives of a newer systemd"
    extra = {
      "X-Owner" = ["platform-team"]
    }
  }

  service = {
    exec_start = ["/usr/local/bin/custom"]
    extra = {
      NewDirective = ["value"]
    }
  }

  extra_sections = {
    "X-Inventory" = {
      Tier = ["backend"]
    }
  }
}
