resource "sysutils_exec" "package_list" {
  command = ["/usr/bin/dpkg-query", "-W", "-f", "$${Package} $${Version}\n"]

  # Keep at most 64 KiB of each stream in state.
  max_output_bytes = 65536
}

output "package_list_truncated" {
  value = sysutils_exec.package_list.truncated
}

# The hash covers the complete output, even when it was truncated in state.
output "package_list_sha256" {
  value = sysutils_exec.package_list.stdout_sha256
}
