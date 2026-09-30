# Regular login users: uids from 1000 up (the usual UID_MIN in
# /etc/login.defs), excluding the "nobody" account.
data "sysutils_users" "humans" {
  uid_min = 1000
  uid_max = 60000
}

output "human_users" {
  value = data.sysutils_users.humans.names
}

output "human_homes" {
  value = { for u in data.sysutils_users.humans.users : u.name => u.home }
}
