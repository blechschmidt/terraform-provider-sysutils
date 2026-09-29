terraform {
  required_providers {
    sysutils = {
      source = "blechschmidt/sysutils"
    }
  }
}

provider "sysutils" {}

variable "release" {
  description = "Version under /opt/myapp/releases that /opt/myapp/current points at."
  type        = string
  default     = "1.4.2"
}

# 1. Identity: a system group and a system user whose primary group it is.
#    Referencing the gid makes Terraform create the group before the user and
#    delete it after the user.
resource "sysutils_group" "myapp" {
  name   = "myapp"
  system = true
}

resource "sysutils_user" "myapp" {
  name        = "myapp"
  gid         = sysutils_group.myapp.gid
  system      = true
  comment     = "myapp service account"
  home        = "/var/lib/myapp"
  shell       = "/usr/sbin/nologin"
  create_home = false # the directory is managed below
}

# 2. Directory tree. Code and configuration belong to root and are only
#    readable by the service's group; state and logs belong to the service.
resource "sysutils_directory" "install" {
  path  = "/opt/myapp"
  owner = "root"
  group = sysutils_group.myapp.name
  mode  = "0750"
}

# Releases are unpacked here by your deployment tooling, one subdirectory
# per version. Their contents are not managed by Terraform.
resource "sysutils_directory" "releases" {
  path  = "${sysutils_directory.install.path}/releases"
  owner = "root"
  group = sysutils_group.myapp.name
  mode  = "0750"
}

resource "sysutils_directory" "config" {
  path  = "/etc/myapp"
  owner = "root"
  group = sysutils_group.myapp.name
  mode  = "0750"
}

resource "sysutils_directory" "state" {
  path  = sysutils_user.myapp.home
  owner = sysutils_user.myapp.name
  group = sysutils_group.myapp.name
  mode  = "0700"

  # Keep ownership correct even for files the service or an operator
  # created by hand, and report drift anywhere in the tree.
  recursive_owner = true

  # The service writes its own data here; remove it along with the account.
  force_destroy = true
}

resource "sysutils_directory" "logs" {
  path  = "/var/log/myapp"
  owner = sysutils_user.myapp.name
  group = sysutils_group.myapp.name
  mode  = "2750" # setgid: new log files inherit the myapp group
}

# 3. Configuration file: owned by root so the service cannot rewrite it,
#    readable by the service through its group.
resource "sysutils_file" "config" {
  path    = "${sysutils_directory.config.path}/myapp.toml"
  owner   = "root"
  group   = sysutils_group.myapp.name
  mode    = "0640"
  content = <<-EOT
    listen    = "127.0.0.1:8080"
    data_dir  = "${sysutils_directory.state.path}"
    log_dir   = "${sysutils_directory.logs.path}"
  EOT
}

# 4. Stable path for the active release. Changing var.release swaps the link
#    atomically, so the service never sees a missing or half-updated path.
#    The target does not have to exist yet.
resource "sysutils_symlink" "current" {
  path   = "${sysutils_directory.install.path}/current"
  target = "releases/${var.release}" # relative to /opt/myapp
}

output "uid" {
  value = sysutils_user.myapp.uid
}

output "config_sha256" {
  value = sysutils_file.config.content_sha256
}
