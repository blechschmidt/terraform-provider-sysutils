# Groups whose name starts with "app-", for example "app-web" and "app-db".
data "sysutils_groups" "app" {
  name_regex = "^app-"
}

output "app_groups" {
  value = { for g in data.sysutils_groups.app.groups : g.name => g.gid }
}
