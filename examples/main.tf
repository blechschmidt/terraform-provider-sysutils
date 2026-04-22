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
output "isolated_stdout"  { value = sysutils_exec.isolated.stdout }
output "isolated_exit"    { value = sysutils_exec.isolated.exit_code }
