variable "artifact_token" {
  description = "API token for the artifact repository."
  type        = string
  sensitive   = true
}

variable "agent_sha512" {
  description = "SHA-512 of the agent binary."
  type        = string
}

# A binary from a private artifact repository. The headers are sensitive and
# are sent to artifacts.example.com only, never to a host it redirects to.
resource "sysutils_remote_file" "agent" {
  url      = "https://artifacts.example.com/repository/tools/agent/3.1.0/agent-linux-amd64"
  path     = "/usr/local/bin/agent"
  checksum = "sha512:${var.agent_sha512}"
  mode     = "0755"

  headers = {
    Authorization = "Bearer ${var.artifact_token}"
  }

  timeout        = "5m"
  max_size_bytes = 200 * 1024 * 1024
}
