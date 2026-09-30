# Allow SSH from the office network only, and reject it from everywhere else.
resource "sysutils_firewall_rule" "ssh_office" {
  name              = "ssh-office"
  family            = "ipv4"
  chain             = "input"
  protocol          = "tcp"
  source            = "192.0.2.0/24"
  destination_ports = ["22"]
  action            = "accept"
  comment           = "SSH from the office"
}

resource "sysutils_firewall_rule" "ssh_others" {
  name              = "ssh-others"
  chain             = "input"
  protocol          = "tcp"
  destination_ports = ["22"]
  action            = "reject"

  # Rules are evaluated in order, and new rules are added at the end, so the
  # reject is created after the accept.
  depends_on = [sysutils_firewall_rule.ssh_office]
}

# Drop traffic to a range of ports on one interface, for IPv4 and IPv6
# ("inet" is the default family).
resource "sysutils_firewall_rule" "block_ephemeral" {
  name              = "block-ephemeral-eth0"
  chain             = "input"
  protocol          = "udp"
  in_interface      = "eth0"
  destination_ports = ["30000-32767"]
  action            = "drop"
}

# Keep routed traffic from a container network away from a private range.
resource "sysutils_firewall_rule" "no_lan_from_containers" {
  name         = "no-lan-from-containers"
  family       = "ipv4"
  chain        = "forward"
  in_interface = "docker0"
  destination  = "10.0.0.0/8"
  action       = "reject"
}

# Force the iptables backend, for hosts managed by iptables-based tools.
resource "sysutils_firewall_rule" "icmpv6" {
  name     = "icmpv6"
  backend  = "iptables"
  family   = "ipv6"
  chain    = "input"
  protocol = "icmpv6"
  action   = "accept"
}
