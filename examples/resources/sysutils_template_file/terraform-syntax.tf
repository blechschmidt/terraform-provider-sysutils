# Render a template file in Terraform's template syntax, like templatefile(),
# but on the provider side: the template is checked during plan and the file
# is only rewritten when the rendered content changes.
resource "sysutils_template_file" "hosts" {
  path     = "/etc/hosts.cluster"
  syntax   = "terraform"
  template = file("${path.module}/templates/hosts.tftpl")

  vars = {
    domain = "cluster.internal"
    nodes = {
      node1 = "10.0.1.1"
      node2 = "10.0.1.2"
    }
  }
}
