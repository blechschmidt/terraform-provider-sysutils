# Needs Terraform 1.11 or later. An ephemeral variable never reaches the
# plan or the state, and Terraform accepts it only in write-only arguments
# such as content_wo. Values from ephemeral resources work the same way.
variable "api_token" {
  type      = string
  ephemeral = true
}

variable "api_token_version" {
  description = "Increase to write a new api_token to the file."
  type        = number
  default     = 1
}

# The state holds neither the token nor the file's content, only its
# checksums. The file is written on create, when content_wo_version changes,
# and when it was changed outside Terraform.
resource "sysutils_file" "api_token" {
  path               = "/etc/app/token.env"
  content_wo         = "API_TOKEN=${var.api_token}\n"
  content_wo_version = var.api_token_version
  mode               = "0600"
}
