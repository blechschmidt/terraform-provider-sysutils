data "sysutils_directory" "releases" {
  path = "/opt/app/releases"
}

locals {
  # entries is null when the directory does not exist.
  releases = data.sysutils_directory.releases.exists ? data.sysutils_directory.releases.entries : []
}

output "latest_release" {
  # Entries are sorted lexically (byte order), so "v10" sorts before "v9".
  value = length(local.releases) > 0 ? local.releases[length(local.releases) - 1] : null
}
