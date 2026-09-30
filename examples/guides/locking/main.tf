# Nine resources that edit /etc/hosts. Terraform applies them in parallel;
# the provider serialises the edits, so none of them is lost.
locals {
  cluster = {
    "10.0.1.1" = "node1"
    "10.0.1.2" = "node2"
    "10.0.1.3" = "node3"
    "10.0.1.4" = "node4"
    "10.0.1.5" = "node5"
    "10.0.1.6" = "node6"
    "10.0.1.7" = "node7"
    "10.0.1.8" = "node8"
    "10.0.1.9" = "node9"
  }
}

resource "sysutils_hosts_entry" "cluster" {
  for_each = local.cluster

  ip        = each.key
  hostnames = ["${each.value}.cluster.internal", each.value]
  comment   = "cluster node"
}

# Each package is installed by its own resource, but only one package
# manager command runs at a time: apt, dnf, yum and apk fail rather than
# wait when another instance holds their lock.
resource "sysutils_package" "tools" {
  for_each = toset(["jq", "tree", "htop"])

  name = each.key
}
