variable "service_name" {
  description = "Name of the service. Used for the user, the group, the directories, the systemd unit and the cron job."
  type        = string
  default     = "demoapp"
}

variable "root_dir" {
  description = "Sandbox directory that stands in for the service host's root filesystem. Everything that supports root_dir is written below it. It is created, and removed again on destroy."
  type        = string
  default     = "/var/tmp/sysutils-complete/rootfs"
}

variable "listen_port" {
  description = "Port the service listens on; rendered into its configuration."
  type        = number
  default     = 8080
}

variable "log_level" {
  description = "Log level of the service; rendered into its configuration."
  type        = string
  default     = "info"
}

variable "manage_systemd" {
  description = "Whether to install and start the systemd unit. Set to false on hosts where systemd is not PID 1, such as most containers."
  type        = bool
  default     = true
}

variable "somaxconn" {
  description = "Value for net.core.somaxconn. The default, 4096, is also the kernel's default since Linux 5.4."
  type        = string
  default     = "4096"
}
