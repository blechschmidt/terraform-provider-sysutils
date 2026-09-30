# Every group that lists "deploy" as a member.
data "sysutils_groups" "deploy" {
  member = "deploy"
}

# Warn if the deploy user has been given administrator rights.
check "deploy_not_admin" {
  assert {
    condition     = length(setintersection(data.sysutils_groups.deploy.names, ["sudo", "wheel", "adm"])) == 0
    error_message = "The deploy user is a member of an administrator group: ${join(", ", data.sysutils_groups.deploy.names)}."
  }
}
