# Every account whose login shell is bash ...
data "sysutils_users" "bash" {
  shell = "/bin/bash"
}

# ... gets a shared profile snippet in its home directory.
resource "sysutils_file" "bash_aliases" {
  for_each = {
    for u in data.sysutils_users.bash.users : u.name => u
    if u.uid >= 1000
  }

  path    = "${each.value.home}/.bash_aliases"
  content = "alias ll='ls -l'\n"
  owner   = each.key
  mode    = "0644"
}
