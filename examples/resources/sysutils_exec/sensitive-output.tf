resource "sysutils_exec" "api_token" {
  command          = ["/usr/local/bin/issue-token", "--scope", "deploy"]
  sensitive_output = true
}

# The token is redacted in plans and CLI output. stdout and stderr are null.
output "api_token" {
  value     = trimspace(sysutils_exec.api_token.sensitive_stdout)
  sensitive = true
}

# The hash is not sensitive and can be used to detect that the token changed.
output "api_token_sha256" {
  value = sysutils_exec.api_token.stdout_sha256
}
