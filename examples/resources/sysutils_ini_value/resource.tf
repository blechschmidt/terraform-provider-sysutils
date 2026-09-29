# Set a key in a section of php.ini, keeping every comment and other setting.
resource "sysutils_ini_value" "memory_limit" {
  path    = "/etc/php/8.2/fpm/php.ini"
  section = "PHP"
  key     = "memory_limit"
  value   = "512M"
}

# A systemd drop-in: the file and its [Service] section are created if needed.
resource "sysutils_ini_value" "app_restart" {
  path      = "/etc/systemd/system/app.service.d/restart.conf"
  section   = "Service"
  key       = "Restart"
  value     = "on-failure"
  separator = "="
  create    = true
}

# A git config subsection.
resource "sysutils_ini_value" "origin_url" {
  path    = "/srv/app/.git/config"
  section = "remote \"origin\""
  key     = "url"
  value   = "https://github.com/example/app.git"
}

# sshd_config has no sections and separates key and value with a space.
resource "sysutils_ini_value" "no_root_login" {
  path      = "/etc/ssh/sshd_config"
  key       = "PermitRootLogin"
  value     = "no"
  separator = " "
}

# Make sure a key is not set at all.
resource "sysutils_ini_value" "no_display_errors" {
  path    = "/etc/php/8.2/fpm/php.ini"
  section = "PHP"
  key     = "display_errors"
  state   = "absent"
}
