# A tmpfs mounted on first access by an automount unit. The names of mount
# and automount units must be the escaped mount point (systemd-escape --path).
resource "sysutils_systemd_unit" "scratch_mount" {
  name = "srv-scratch.mount"

  mount = {
    what    = "tmpfs"
    where   = "/srv/scratch"
    type    = "tmpfs"
    options = "size=512m,mode=1777"
  }
}

resource "sysutils_systemd_unit" "scratch_automount" {
  name    = "srv-scratch.automount"
  enabled = true
  state   = "running"

  automount = {
    where            = "/srv/scratch"
    timeout_idle_sec = "10min"
  }

  install = {
    wanted_by = ["local-fs.target"]
  }

  depends_on = [sysutils_systemd_unit.scratch_mount]
}

# A slice that caps the memory and CPU of every unit placed in it.
resource "sysutils_systemd_unit" "batch_slice" {
  name = "batch.slice"

  slice = {
    memory_max = "2G"
    cpu_quota  = "150%"
  }
}
