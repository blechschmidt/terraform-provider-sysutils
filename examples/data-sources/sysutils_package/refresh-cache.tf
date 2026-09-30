# Refresh the package index while planning, then report whether the index
# offers another version of openssl than the installed one. Needs root.
data "sysutils_package" "openssl" {
  name          = "openssl"
  refresh_cache = true
}

output "openssl_update_available" {
  value = (
    data.sysutils_package.openssl.installed &&
    data.sysutils_package.openssl.available_version != null &&
    data.sysutils_package.openssl.available_version != data.sysutils_package.openssl.version
  )
}
