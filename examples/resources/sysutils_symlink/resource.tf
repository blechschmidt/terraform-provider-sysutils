resource "sysutils_directory" "release" {
  path = "/opt/app/releases/v2"
}

resource "sysutils_symlink" "current" {
  path = "/opt/app/current"
  # Relative targets are resolved against the directory containing the link,
  # so this points to /opt/app/releases/v2 and survives moving /opt/app.
  target = "releases/v2"

  depends_on = [sysutils_directory.release]
}
