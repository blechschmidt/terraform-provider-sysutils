resource "sysutils_exec" "migrate" {
  command = ["/usr/local/bin/my-migrate", "--apply"]
  triggers = {
    schema_version = "3"
  }
}
