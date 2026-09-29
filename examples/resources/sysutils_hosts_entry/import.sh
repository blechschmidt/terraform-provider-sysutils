# <path>:<ip>; the first line of the file that maps the address is imported.
terraform import sysutils_hosts_entry.db '/etc/hosts:10.0.0.5'

# The path ends at the first colon, so an IPv6 address needs no quoting.
terraform import sysutils_hosts_entry.db_v6 '/etc/hosts:fd00::5'
