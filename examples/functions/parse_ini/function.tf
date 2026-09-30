data "sysutils_file" "php_ini" {
  path = "/etc/php/8.2/fpm/php.ini"
}

locals {
  php = provider::sysutils::parse_ini(data.sysutils_file.php_ini.content)
}

# Keys before the first [section] header are in the section "".
output "php_memory_limit" {
  value = lookup(lookup(local.php, "PHP", {}), "memory_limit", null)
}
