resource "sysutils_directory" "www" {
  path            = "/srv/www"
  owner           = "www-data"
  group           = "www-data"
  mode            = "0750" # the directory and every subdirectory
  file_mode       = "0640" # every regular file inside it
  recursive_owner = true   # like chown -R www-data:www-data
  recursive_mode  = true   # like chmod -R, with separate file/directory modes
}
