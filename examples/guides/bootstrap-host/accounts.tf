# Administrators: user name => OpenSSH public key.
locals {
  admins = {
    alice = file("${path.module}/keys/alice.pub")
  }
}

# Members of "ops" may use sudo.
resource "sysutils_group" "ops" {
  name = "ops"
}

resource "sysutils_user" "admin" {
  for_each = local.admins

  name        = each.key
  shell       = "/bin/bash"
  create_home = true
  groups      = [sysutils_group.ops.name]
}

resource "sysutils_ssh_authorized_key" "admin" {
  for_each = local.admins

  # Referencing the user creates the account and its home directory first.
  user = sysutils_user.admin[each.key].name
  key  = each.value
}

# The accounts have no password, so sudo must not ask for one. sudo ignores
# files in /etc/sudoers.d that others can write to; 0440 is the convention.
resource "sysutils_file" "sudoers_ops" {
  path    = "/etc/sudoers.d/ops"
  content = "%${sysutils_group.ops.name} ALL=(ALL:ALL) NOPASSWD: ALL\n"
  mode    = "0440"
  owner   = "root"
  group   = "root"

  depends_on = [sysutils_package.base]
}
