# Extract a plugin bundle into an existing directory that holds other files.
# Files the bundle shares with the directory are replaced only with
# overwrite = true; destroy removes exactly the files that were extracted.
resource "sysutils_archive_extract" "plugins" {
  source      = "${path.module}/files/plugins.tar.xz"
  destination = "/usr/local/lib/app"
  overwrite   = true
}

output "installed_plugin_files" {
  value = sysutils_archive_extract.plugins.files
}
