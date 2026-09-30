# 5. Operators: members of myapp-admins may restart the service and read its
#    status and logs as root, without a password, and nothing else. visudo
#    checks the file before it is installed.
resource "sysutils_group" "myapp_admins" {
  name = "myapp-admins"
}

resource "sysutils_sudoers" "myapp_admins" {
  name = "50-myapp-admins"

  rules = [{
    users    = ["%${sysutils_group.myapp_admins.name}"]
    runas    = "root"
    nopasswd = true
    commands = [
      "/usr/bin/systemctl restart myapp.service",
      "/usr/bin/systemctl status myapp.service",
      "/usr/bin/journalctl -u myapp.service",
    ]
  }]
}
