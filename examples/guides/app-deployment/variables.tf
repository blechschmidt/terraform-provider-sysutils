variable "app_version" {
  description = "Release to deploy. Your build puts its tarball at files/myapp-<version>.tar.gz."
  type        = string
  default     = "1.4.2"
}

variable "listen_port" {
  description = "TCP port the service listens on."
  type        = number
  default     = 8080
}
