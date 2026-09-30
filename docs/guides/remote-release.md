---
page_title: "Cookbook: Installing a release from a URL"
subcategory: "Cookbook"
description: |-
  Download a release tarball over https, verify its published checksum, unpack it into a versioned directory and link its binary into the PATH; upgrade by changing two variables.
---

# Cookbook: Installing a release from a URL

Many tools are published as release tarballs with a checksum file next to them. This recipe installs one, `exporter`, the way you would by hand with `curl`, `sha256sum -c` and `tar`, but so that every step is checked and repeatable:

- the tarball is downloaded and verified against its published SHA-256 checksum;
- it is unpacked into a directory of its own for each version;
- a symlink in `/usr/local/bin` points to the active version's binary.

It uses [`sysutils_remote_file`](../resources/remote_file.md), [`sysutils_archive_extract`](../resources/archive_extract.md), [`sysutils_symlink`](../resources/symlink.md) and [`sysutils_directory`](../resources/directory.md), and needs root to write to `/opt` and `/usr/local/bin`. The complete configuration is in [`examples/guides/remote-release`](https://github.com/blechschmidt/terraform-provider-sysutils/tree/main/examples/guides/remote-release); replace `downloads.example.com` with the tool's release URL.

```terraform
terraform {
  required_version = ">= 1.5"

  required_providers {
    sysutils = {
      source = "blechschmidt/sysutils"
    }
  }
}

provider "sysutils" {}
```

```terraform
variable "exporter_version" {
  description = "Release to install, such as 1.8.2."
  type        = string
}

variable "exporter_sha256" {
  description = "SHA-256 of the release tarball, from the release's sha256sums.txt."
  type        = string
}
```

## Download

```terraform
locals {
  exporter_release = "exporter-${var.exporter_version}.linux-amd64"
}

resource "sysutils_directory" "downloads" {
  path = "/var/cache/exporter"
  mode = "0700"
}

# The tarball is verified before it is renamed into place, so a truncated or
# tampered download never reaches the extraction below.
resource "sysutils_remote_file" "exporter" {
  url      = "https://downloads.example.com/exporter/v${var.exporter_version}/${local.exporter_release}.tar.gz"
  path     = "${sysutils_directory.downloads.path}/${local.exporter_release}.tar.gz"
  checksum = "sha256:${var.exporter_sha256}"
}
```

**Take the checksum from the publisher, not from the download.** Copy it from the release's `sha256sums.txt` or release notes into `exporter_sha256`, ideally after checking that file's signature. A checksum computed from a file you just downloaded only proves that the download did not change since.

**Nothing unverified reaches the disk.** The tarball is streamed into a temporary file with mode `0600` next to `path`, and renamed into place only when its size and checksum match. A truncated download, a mismatch, a response larger than `max_size_bytes` (1 GiB by default) or a redirect from https to plain http fails the apply and leaves nothing behind.

**The URL is not polled.** Refresh hashes the local file instead of contacting the server, so plans work offline and don't depend on the server's availability. If the tarball is modified or deleted, the plan shows it and apply downloads it again.

## Install

```terraform
resource "sysutils_archive_extract" "exporter" {
  source           = sysutils_remote_file.exporter.path
  destination      = "/opt/exporter/${var.exporter_version}"
  strip_components = 1

  lifecycle {
    create_before_destroy = true
  }
}

resource "sysutils_symlink" "exporter" {
  path   = "/usr/local/bin/exporter"
  target = "${sysutils_archive_extract.exporter.destination}/exporter"
}
```

`source` refers to the download's `path`, so the tarball is verified before it is extracted. `sysutils_archive_extract` checks the archive itself too: entries that would land outside `destination` and archives that expand beyond `max_size` are refused. It reads the tarball during every plan; when the download is still to come, its checksum is simply unknown until apply.

## Upgrading

```shell
terraform apply -var exporter_version=1.9.0 -var exporter_sha256=<checksum from sha256sums.txt>
```

The new version changes the download's `path`, so the new tarball is downloaded and verified next to the old one. `create_before_destroy` on the extraction unpacks the new release, switches the symlink with a single rename, and only then removes the old release's files. A wrong checksum stops the upgrade before anything is extracted, and the old version stays active.

If a publisher replaces a release file under the same URL, the checksum no longer matches and the apply fails instead of installing different code under a known version, which is exactly what the checksum is for. For files that change without a new URL and have no checksum, such as a CA bundle, see `allow_unverified` and `force_redownload` on the [`sysutils_remote_file`](../resources/remote_file.md) page.
