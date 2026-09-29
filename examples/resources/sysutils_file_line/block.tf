# Keep a block of cluster nodes in /etc/hosts, between marker comments:
#
#   # BEGIN cluster nodes
#   10.0.1.1 node1
#   ...
#   # END cluster nodes
locals {
  nodes = {
    node1 = "10.0.1.1"
    node2 = "10.0.1.2"
    node3 = "10.0.1.3"
  }
}

resource "sysutils_file_line" "cluster_hosts" {
  path   = "/etc/hosts"
  marker = "# {mark} cluster nodes"
  block  = join("", [for name, ip in local.nodes : "${ip} ${name}\n"])
}

# Create the file if it does not exist yet, and put the block at the top.
resource "sysutils_file_line" "profile_path" {
  path          = "/etc/profile.d/app.sh"
  create        = true
  insert_before = "BOF"
  block         = <<-EOT
    export APP_HOME=/opt/app
    export PATH="$APP_HOME/bin:$PATH"
  EOT
}
