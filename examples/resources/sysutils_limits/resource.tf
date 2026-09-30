# Let the postgres user open up to 65536 files. Both entries go to
# /etc/security/limits.d/90-terraform-user-postgres.conf, the default file
# for the domain.
resource "sysutils_limits" "postgres_nofile_soft" {
  domain = "postgres"
  type   = "soft"
  item   = "nofile"
  value  = "65536"
}

resource "sysutils_limits" "postgres_nofile_hard" {
  domain = "postgres"
  type   = "hard"
  item   = "nofile"
  value  = "65536"
}

# Let members of the "audio" group lock memory without limit, for both the
# soft and the hard limit ("-").
resource "sysutils_limits" "audio_memlock" {
  domain = "@audio"
  type   = "-"
  item   = "memlock"
  value  = "unlimited"
}

# No core dumps for anyone, in /etc/security/limits.d/90-terraform-default.conf.
resource "sysutils_limits" "no_core_dumps" {
  domain = "*"
  type   = "hard"
  item   = "core"
  value  = "0"
}

# Regular users, by UID range, get at most 4096 processes.
resource "sysutils_limits" "users_nproc" {
  domain = "1000:"
  type   = "hard"
  item   = "nproc"
  value  = "4096"
}
