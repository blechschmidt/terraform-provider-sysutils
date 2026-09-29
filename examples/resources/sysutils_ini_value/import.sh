# <path>:<section>:<key>
terraform import sysutils_ini_value.memory_limit '/etc/php/8.2/fpm/php.ini:PHP:memory_limit'

# Global keys, before the first section header, have an empty section.
terraform import sysutils_ini_value.editorconfig_root '/srv/app/.editorconfig::root'
