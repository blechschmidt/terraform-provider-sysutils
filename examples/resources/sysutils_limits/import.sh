# By <path>:<domain>:<type>:<item>, where path is the drop-in file in
# /etc/security/limits.d.
terraform import sysutils_limits.postgres_nofile_soft /etc/security/limits.d/90-terraform-user-postgres.conf:postgres:soft:nofile

# Domains may contain colons, as UID and GID ranges do.
terraform import sysutils_limits.users_nproc /etc/security/limits.d/90-terraform-uid-1000.conf:1000::hard:nproc
