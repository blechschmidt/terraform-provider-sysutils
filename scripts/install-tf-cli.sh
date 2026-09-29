#!/bin/sh
# Install the Terraform or OpenTofu CLI into a directory, verifying the
# release checksum.
#
# Usage: install-tf-cli.sh <terraform|tofu> <version> [dest-dir]
#
# <version> is an exact release ("1.5.7"), a release prefix ("1.5" picks the
# newest 1.5.x) or "latest" (the newest stable release). Pre-releases are only
# installed when requested exactly.
set -eu

cli=${1:?usage: install-tf-cli.sh <terraform|tofu> <version> [dest-dir]}
want=${2:?usage: install-tf-cli.sh <terraform|tofu> <version> [dest-dir]}
dest=${3:-/usr/local/bin}

case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

case "$cli" in
	terraform)
		index_url=https://releases.hashicorp.com/terraform/index.json
		# Every release is listed as "version":"x.y.z".
		version_pattern='"version":"[0-9]+\.[0-9]+\.[0-9]+"'
		;;
	tofu)
		index_url=https://get.opentofu.org/tofu/api.json
		# Every release is listed as "id":"x.y.z".
		version_pattern='"id":"[0-9]+\.[0-9]+\.[0-9]+"'
		;;
	*) echo "unknown CLI \"$cli\"; want terraform or tofu" >&2; exit 1 ;;
esac

if printf '%s' "$want" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-.+)?$'; then
	version=$want
else
	case "$want" in
		latest) prefix='' ;;
		*) prefix=$(printf '%s.' "$want" | sed 's/\./\\./g') ;;
	esac
	version=$(curl -fsSL --retry 3 "$index_url" \
		| grep -Eo "$version_pattern" \
		| grep -Eo '[0-9]+\.[0-9]+\.[0-9]+' \
		| grep -E "^${prefix}" \
		| sort -uV | tail -n 1)
	if [ -z "$version" ]; then
		echo "no stable $cli release matches \"$want\"" >&2
		exit 1
	fi
fi

case "$cli" in
	terraform) base="https://releases.hashicorp.com/terraform/${version}" ;;
	tofu) base="https://github.com/opentofu/opentofu/releases/download/v${version}" ;;
esac
zip="${cli}_${version}_linux_${arch}.zip"
sums="${cli}_${version}_SHA256SUMS"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL --retry 3 -o "$tmp/$zip" "$base/$zip"
curl -fsSL --retry 3 -o "$tmp/$sums" "$base/$sums"
(cd "$tmp" && grep " ${zip}\$" "$sums" | sha256sum -c -)
unzip -o -q "$tmp/$zip" "$cli" -d "$dest"
chmod 0755 "$dest/$cli"
"$dest/$cli" version
