# By the name of the drop-in in /etc/systemd/journald.conf.d, without the
# ".conf" suffix. Its [Journal] settings are read into the attributes.
terraform import sysutils_journald_config.retention 90-retention
