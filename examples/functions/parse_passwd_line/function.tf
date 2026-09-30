data "sysutils_file" "passwd" {
  path = "/etc/passwd"
}

locals {
  # Blank lines and comments parse to null and are skipped.
  accounts = [
    for line in split("\n", data.sysutils_file.passwd.content) :
    provider::sysutils::parse_passwd_line(line)
    if provider::sysutils::parse_passwd_line(line) != null
  ]
}

output "login_users" {
  value = sort([
    for a in local.accounts : a.name
    if a.uid >= 1000 && !endswith(a.shell, "nologin") && !endswith(a.shell, "false")
  ])
}
