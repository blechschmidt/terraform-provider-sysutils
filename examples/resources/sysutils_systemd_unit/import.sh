# By unit name; the unit file must be in /etc/systemd/system.
terraform import sysutils_systemd_unit.app app.service

# Templates are imported by their name, too.
terraform import sysutils_systemd_unit.queue queue@.service
