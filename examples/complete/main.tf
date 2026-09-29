# A service host, end to end: the service's group and user, its directory
# tree, a rendered configuration file, a systemd unit that runs it, a kernel
# parameter it needs, a cron job that cleans up after it and an entry in
# /etc/hosts.
#
# Everything that supports the provider's root_dir is written into a sandbox
# directory (var.root_dir) instead of the host's root filesystem, so the stack
# can be applied and destroyed on any Linux machine without touching its /etc.
# Users, groups, systemd units and kernel parameters have no such sandbox: they
# act on the host. Run as root.

locals {
  name = var.service_name

  # Paths inside the sandbox, as the service sees them.
  config_dir = "/etc/${local.name}"
  state_dir  = "/var/lib/${local.name}"
  log_dir    = "/var/log/${local.name}"
}

# Host provider: users, groups, the systemd unit, the kernel parameter and the
# sandbox directory itself.
provider "sysutils" {}

# Sandbox provider: every path is relative to var.root_dir.
provider "sysutils" {
  alias    = "sandbox"
  root_dir = var.root_dir
}

# ---------------------------------------------------------------------------
# Host: sandbox, group and user
# ---------------------------------------------------------------------------

resource "sysutils_directory" "root" {
  path  = var.root_dir
  mode  = "0755"
  owner = "root"
  group = "root"

  # The resources below create directories of their own (such as /etc and
  # /var inside the sandbox) that no resource manages. Remove them with the
  # sandbox on destroy.
  force_destroy = true
}

resource "sysutils_group" "service" {
  name   = local.name
  system = true
}

resource "sysutils_user" "service" {
  name    = local.name
  gid     = sysutils_group.service.gid
  system  = true
  home    = local.state_dir
  shell   = "/usr/sbin/nologin"
  comment = "${local.name} service account"
}

# ---------------------------------------------------------------------------
# Sandbox: directories, configuration, cron job, /etc/hosts entry
# ---------------------------------------------------------------------------

resource "sysutils_directory" "config" {
  provider = sysutils.sandbox

  path  = local.config_dir
  mode  = "0750"
  owner = "root"
  group = sysutils_group.service.name

  depends_on = [sysutils_directory.root]
}

resource "sysutils_directory" "state" {
  provider = sysutils.sandbox

  path  = local.state_dir
  mode  = "0750"
  owner = sysutils_user.service.name
  group = sysutils_group.service.name

  depends_on = [sysutils_directory.root]
}

resource "sysutils_directory" "logs" {
  provider = sysutils.sandbox

  path  = local.log_dir
  mode  = "0750"
  owner = sysutils_user.service.name
  group = sysutils_group.service.name

  # Log files written by the service are removed with the directory.
  force_destroy = true

  depends_on = [sysutils_directory.root]
}

resource "sysutils_template_file" "config" {
  provider = sysutils.sandbox

  path     = "${sysutils_directory.config.path}/${local.name}.conf"
  template = file("${path.module}/templates/service.conf.tmpl")
  syntax   = "terraform"
  vars = {
    name      = local.name
    port      = var.listen_port
    log_level = var.log_level
    state_dir = local.state_dir
    log_dir   = local.log_dir
  }

  mode  = "0640"
  owner = "root"
  group = sysutils_group.service.name
}

resource "sysutils_directory" "cron_d" {
  provider = sysutils.sandbox

  path = "/etc/cron.d"
  mode = "0755"

  depends_on = [sysutils_directory.root]
}

resource "sysutils_cron_job" "cleanup" {
  provider = sysutils.sandbox

  name     = "${local.name}-cleanup"
  schedule = "30 3 * * *"
  user     = sysutils_user.service.name
  command  = "find ${local.log_dir} -name '*.log' -mtime +14 -delete"
  comment  = "Removes ${local.name} logs older than two weeks."
  environment = {
    MAILTO = ""
  }

  depends_on = [sysutils_directory.cron_d, sysutils_directory.logs]
}

resource "sysutils_file_line" "hosts" {
  provider = sysutils.sandbox

  path   = "/etc/hosts"
  line   = "127.0.0.1 ${local.name}.internal"
  regexp = "\\s${replace(local.name, ".", "\\.")}\\.internal$"
  create = true

  depends_on = [sysutils_directory.root]
}

# ---------------------------------------------------------------------------
# Host: kernel parameter and systemd unit
# ---------------------------------------------------------------------------

resource "sysutils_sysctl" "somaxconn" {
  name  = "net.core.somaxconn"
  value = var.somaxconn
  file  = "/etc/sysctl.d/90-${local.name}.conf"
}

resource "sysutils_systemd_unit" "service" {
  count = var.manage_systemd ? 1 : 0

  name = "${local.name}.service"
  content = templatefile("${path.module}/templates/service.unit.tmpl", {
    name = local.name
    user = sysutils_user.service.name
    # The unit runs on the host, so it refers to the configuration by its
    # host path, inside the sandbox.
    config_file = "${sysutils_directory.root.path}${sysutils_template_file.config.path}"
    # Changes the unit file, and so restarts the service, whenever the
    # configuration changes.
    config_sha256 = sysutils_template_file.config.content_sha256
  })
  enabled = true
  state   = "running"

  depends_on = [sysutils_sysctl.somaxconn, sysutils_directory.state]
}
