resource "sysutils_user" "deploy" {
  name        = "deploy"
  shell       = "/bin/bash"
  create_home = true
}

# Adds the key from a .pub file, with the comment it has there, to
# ~deploy/.ssh/authorized_keys.
resource "sysutils_ssh_authorized_key" "alice" {
  user = sysutils_user.deploy.name
  key  = file("${path.module}/keys/alice.pub")
}

# A key that may only run the backup command, and only from one network:
#
#   from="10.0.0.0/8",command="/usr/local/bin/backup",restrict ssh-ed25519 AAAA... backup job
resource "sysutils_ssh_authorized_key" "backup" {
  user    = sysutils_user.deploy.name
  key     = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAJ7S2wHQgPlWq1EKEakfqLV8Jgwxo/Pcnay1t0ECiSB"
  comment = "backup job"
  options = [
    "from=\"10.0.0.0/8\"",
    "command=\"/usr/local/bin/backup\"",
    "restrict",
  ]
}
