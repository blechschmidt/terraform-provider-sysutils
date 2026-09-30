# By name, looking in every available backend.
terraform import sysutils_firewall_rule.ssh_office ssh-office

# With an explicit backend: <backend>:<name>.
terraform import sysutils_firewall_rule.icmpv6 iptables:icmpv6
