terraform {
  required_providers {
    sysutils = {
      source = "blechschmidt/sysutils"
    }
  }
}

provider "sysutils" {}

resource "sysutils_file" "motd" {
  path    = "/etc/motd"
  content = "Welcome.\n"
  mode    = "0644"
}
