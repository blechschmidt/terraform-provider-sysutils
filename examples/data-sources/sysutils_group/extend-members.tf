# Look up the group that owns /var/log files by gid ...
data "sysutils_group" "adm" {
  gid = 4
}

# ... and adopt it with one extra member, keeping the existing ones.
# Import it first with: terraform import sysutils_group.adm adm
resource "sysutils_group" "adm" {
  name    = data.sysutils_group.adm.name
  gid     = data.sysutils_group.adm.gid
  members = setunion(data.sysutils_group.adm.members, ["deploy"])
}
