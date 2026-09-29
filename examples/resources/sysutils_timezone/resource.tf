# Run the host on Berlin time. /etc/localtime becomes a symlink to
# /usr/share/zoneinfo/Europe/Berlin, and /etc/timezone names the zone on
# Debian, Ubuntu and Alpine.
resource "sysutils_timezone" "this" {
  timezone = "Europe/Berlin"
}

# Put back the previous time zone when the resource is destroyed, for
# example on a shared test machine.
# resource "sysutils_timezone" "this" {
#   timezone           = "UTC"
#   restore_on_destroy = true
# }
