# Needs Terraform 1.11 or later, or OpenTofu 1.11 or later, to pass the
# value to a write-only argument. The command runs during every plan and
# apply that needs the value, so it must only read.
ephemeral "sysutils_exec" "api_token" {
  command = ["/usr/bin/vault", "kv", "get", "-field=token", "secret/app"]
  environment = {
    VAULT_ADDR = "https://vault.example.com:8200"
  }
  timeout = "30s"
}

# The token is never stored in the plan or the state. Increase
# content_wo_version to write a rotated token.
resource "sysutils_file" "api_token" {
  path               = "/etc/app/token"
  content_wo         = trimspace(ephemeral.sysutils_exec.api_token.stdout)
  content_wo_version = 1
  mode               = "0600"
}
