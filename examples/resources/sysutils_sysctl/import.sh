# By name, persisted in /etc/sysctl.d/99-terraform.conf.
terraform import sysutils_sysctl.ip_forward net.ipv4.ip_forward

# By name and the sysctl.d file it is persisted in.
terraform import sysutils_sysctl.port_range net.ipv4.ip_local_port_range:/etc/sysctl.d/60-ports.conf
