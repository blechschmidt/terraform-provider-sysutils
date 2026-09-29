# Find out which account owns uid 1000, typically the first regular user.
data "sysutils_user" "first" {
  uid = 1000
}

output "first_user" {
  value = {
    name   = data.sysutils_user.first.name
    shell  = data.sysutils_user.first.shell
    groups = data.sysutils_user.first.groups
    # The GECOS field is usually "Full Name,Room,Work phone,Home phone".
    full_name = split(",", data.sysutils_user.first.comment)[0]
  }
}
