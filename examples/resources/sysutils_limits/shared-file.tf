# All limits of one application in a file of its own, one resource per
# entry. Other lines in the file, such as comments added by hand, are kept.
locals {
  app_limits = {
    "soft-nofile" = { type = "soft", item = "nofile", value = "16384" }
    "hard-nofile" = { type = "hard", item = "nofile", value = "65536" }
    "both-nproc"  = { type = "-", item = "nproc", value = "8192" }
  }
}

resource "sysutils_limits" "app" {
  for_each = local.app_limits

  domain = "app"
  type   = each.value.type
  item   = each.value.item
  value  = each.value.value
  file   = "80-app.conf"
}
