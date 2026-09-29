# Docker's apt repository, with its key fetched from Docker and stored in
# /etc/apt/keyrings/docker.asc. The package index is refreshed after every
# change, so the package below can be installed right away.
resource "sysutils_package_repository" "docker" {
  name            = "docker"
  description     = "Docker CE"
  uris            = ["https://download.docker.com/linux/debian"]
  suites          = ["bookworm"]
  components      = ["stable"]
  architectures   = ["amd64"]
  signing_key_url = "https://download.docker.com/linux/debian/gpg"
  refresh_cache   = true
}

resource "sysutils_package" "docker" {
  name = "docker-ce"

  depends_on = [sysutils_package_repository.docker]
}

# A dnf repository whose key dnf fetches itself. dnf variables such as
# $releasever are passed through.
resource "sysutils_package_repository" "pgdg" {
  name            = "pgdg16"
  manager         = "dnf"
  description     = "PostgreSQL 16 for Fedora $releasever"
  uris            = ["https://download.postgresql.org/pub/repos/yum/16/fedora/fedora-$releasever-$basearch"]
  signing_key_url = "https://download.postgresql.org/pub/repos/yum/keys/PGDG-RPM-GPG-KEY-Fedora"
}

# A local flat apt repository, signed by a key kept next to the
# configuration.
resource "sysutils_package_repository" "local" {
  name        = "local"
  manager     = "apt"
  uris        = ["file:///srv/apt"]
  suites      = ["./"]
  signing_key = file("${path.module}/local-repo.asc")
}

# Alpine's edge/testing repository, tagged so that only packages asked for
# as "name@testing" come from it.
resource "sysutils_package_repository" "alpine_testing" {
  name    = "testing"
  manager = "apk"
  uris    = ["https://dl-cdn.alpinelinux.org/alpine/edge/testing"]
  tag     = "testing"
}
