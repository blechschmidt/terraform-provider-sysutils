terraform {
  required_providers {
    sysutils = {
      source = "registry.terraform.io/blechschmidt/sysutils"
    }
  }
}

provider "sysutils" {}

resource "sysutils_file" "hello" {
  path    = "/tmp/hello.txt"
  content = "Hello from Terraform.\n"
  mode    = "0644"
}

resource "sysutils_user" "app" {
  name        = "appsvc"
  shell       = "/usr/sbin/nologin"
  system      = true
  create_home = false
  groups      = ["adm"]
}

# A private directory owned by the service user, with its config file inside.
# Referencing the user's attributes makes Terraform create the user first.
resource "sysutils_directory" "app_data" {
  path  = "/tmp/sysutils-example/app"
  owner = sysutils_user.app.name
  group = tostring(sysutils_user.app.gid)
  mode  = "0750"
  # Allow `terraform destroy` to remove files the service wrote here too.
  force_destroy = true
}

# Referencing the directory's path orders creation (directory first) and
# destruction (file first) correctly.
resource "sysutils_file" "app_config" {
  path    = "${sysutils_directory.app_data.path}/app.conf"
  content = "listen = 127.0.0.1:8080\n"
  mode    = "0640"
  owner   = sysutils_directory.app_data.owner
  group   = sysutils_directory.app_data.group
}

# A stable path pointing at the config file. Changing `target` swaps the link
# atomically.
resource "sysutils_symlink" "app_config_current" {
  path       = "/tmp/sysutils-example/current.conf"
  target     = "app/app.conf" # relative to the link's directory
  depends_on = [sysutils_file.app_config]
}

# Inspect a directory without managing it. A missing directory is not an
# error: `exists` is false and the other attributes are null.
data "sysutils_directory" "tmp" {
  path = "/tmp"
}

# Run a command using the provider process's environment plus an override.
resource "sysutils_exec" "inherited" {
  command = ["/bin/sh", "-c", "echo PATH=$PATH EXTRA=$EXTRA"]
  environment = {
    EXTRA = "inherited-plus-extra"
  }
  triggers = {
    # Change this value to force a re-run.
    version = "1"
  }
}

# Run a command with a completely custom environment (no inheritance).
resource "sysutils_exec" "isolated" {
  command                    = ["/usr/bin/env"]
  inherit_parent_environment = false
  environment = {
    FOO = "bar"
    BAZ = "qux"
  }
}

output "inherited_stdout" { value = sysutils_exec.inherited.stdout }
output "isolated_stdout" { value = sysutils_exec.isolated.stdout }
output "isolated_exit" { value = sysutils_exec.isolated.exit_code }

output "tmp_mode" { value = data.sysutils_directory.tmp.mode }
output "tmp_exists" { value = data.sysutils_directory.tmp.exists }
