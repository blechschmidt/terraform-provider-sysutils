# Written to /etc/sudoers.d/50-deploy:
#
#   # Managed by Terraform (sysutils_sudoers). Manual changes will be reverted.
#   %deploy ALL = (root) NOPASSWD: /usr/bin/systemctl restart app.service, /usr/bin/systemctl reload nginx.service
#   alice ALL = (ALL:ALL) ALL
resource "sysutils_sudoers" "deploy" {
  name = "50-deploy"

  rules = [
    {
      users    = ["%deploy"]
      runas    = "root"
      nopasswd = true
      commands = [
        "/usr/bin/systemctl restart app.service",
        "/usr/bin/systemctl reload nginx.service",
      ]
    },
    {
      users    = ["alice"]
      runas    = "ALL:ALL"
      commands = ["ALL"]
    },
  ]
}

# Anything sudoers can express, written verbatim. visudo checks the file
# before it is installed, so a typo fails the apply instead of breaking sudo.
resource "sysutils_sudoers" "backup" {
  name    = "60-backup"
  content = <<-EOT
    Defaults:backup !requiretty, env_keep += "BORG_PASSPHRASE"
    Cmnd_Alias BACKUP = /usr/bin/rsync, /usr/bin/borg
    backup ALL = (root) NOPASSWD: BACKUP
  EOT
}
