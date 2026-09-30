# Name the host web1.example.com: /etc/hostname, the kernel hostname and,
# on systemd hosts, systemd-hostnamed all report it. The pretty hostname is
# what hostnamectl shows, and /etc/hosts gets
# "127.0.1.1 web1.example.com web1" so that the name resolves without DNS.
resource "sysutils_hostname" "this" {
  hostname           = "web1.example.com"
  pretty_hostname    = "Web server 1"
  manage_hosts_entry = true
}

# Put back the previous hostname when the resource is destroyed, for
# example on a shared test machine.
# resource "sysutils_hostname" "this" {
#   hostname           = "ci-runner-7"
#   restore_on_destroy = true
# }
