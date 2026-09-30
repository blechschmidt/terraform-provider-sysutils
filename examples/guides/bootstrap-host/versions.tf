terraform {
  # Actions, such as the sshd restart in sshd.tf, need Terraform 1.14.
  required_version = ">= 1.14"

  required_providers {
    sysutils = {
      source = "blechschmidt/sysutils"
    }
  }
}

provider "sysutils" {}
