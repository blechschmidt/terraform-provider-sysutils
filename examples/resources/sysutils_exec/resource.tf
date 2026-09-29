resource "sysutils_exec" "version" {
  command = ["/usr/local/bin/my-tool", "--version"]
}

output "tool_version" {
  value = sysutils_exec.version.stdout
}
