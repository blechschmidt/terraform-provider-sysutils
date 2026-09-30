#!/bin/sh
# Runs inside a stock distribution container (see testacc-distros.sh): installs
# the tools the acceptance tests drive, boots OpenRC on Alpine, and runs the
# static test binary mounted at /sysutils. Setup output goes to stderr; stdout
# carries only the test binary's test2json-framed output.
#
# Usage: testacc-distro-entrypoint.sh [test binary flags...]
set -eu

exec 3>&1 1>&2

# shellcheck source=/dev/null
. /etc/os-release
echo "Setting up $PRETTY_NAME"

# Package mirrors occasionally drop connections.
retry() {
	"$@" || { sleep 10; "$@"; } || { sleep 30; "$@"; }
}

case "$ID" in
debian | ubuntu)
	export DEBIAN_FRONTEND=noninteractive
	# acl, attr: setfacl/getfattr for the extended attribute tests.
	# locales, tzdata: locale-gen and zone files. iproute2, iptables,
	# nftables: the firewall rule tests. passwd: useradd/groupadd.
	# util-linux, mount: unshare/nsenter, mkswap/swapon and losetup.
	# sudo: visudo for the sudoers tests. logrotate: the logrotate -d check.
	retry apt-get update -q
	retry apt-get install -y -q --no-install-recommends \
		acl attr ca-certificates iproute2 iptables locales logrotate mount \
		nftables passwd sudo tzdata util-linux
	;;
alpine)
	# shadow: useradd/groupadd/gpasswd (busybox adduser is not enough).
	# util-linux-misc: unshare/nsenter with --net. musl has no locales, so
	# the locale tests skip (see ACC_ALLOWED_SKIPS in testacc-distros.sh).
	# sudo and logrotate are left out on purpose, so that the sudoers and
	# logrotate tests cover a host without visudo and logrotate.
	retry apk add --no-cache \
		acl attr ca-certificates iproute2 iptables ip6tables nftables \
		openrc busybox-openrc shadow tzdata util-linux util-linux-misc losetup
	# Boot OpenRC so that rc-service and rc-update work (the OpenRC service
	# tests skip unless /run/openrc/softlevel exists).
	openrc sysinit >/dev/null 2>&1 || true
	openrc default >/dev/null 2>&1 || true
	test -e /run/openrc/softlevel || { echo "OpenRC did not boot" >&2; exit 1; }
	;;
fedora | rhel | centos | almalinux | rocky)
	# glibc-locale-source: localedef input for the locale tests.
	# iptables-nft, nftables, iproute: the firewall rule tests.
	# shadow-utils: useradd. util-linux: unshare/nsenter, losetup, mkswap.
	# sudo: visudo for the sudoers tests. logrotate: the logrotate -d check.
	retry dnf install -y -q --setopt=install_weak_deps=False \
		acl attr ca-certificates glibc-locale-source iproute iptables-nft \
		logrotate nftables shadow-utils sudo tzdata util-linux
	dnf clean all -q
	;;
*)
	echo "unsupported distribution: $ID" >&2
	exit 1
	;;
esac

export TF_ACC=1
: "${TF_CLI:=terraform}"
export TF_ACC_TERRAFORM_PATH="/sysutils/$TF_CLI"
if [ "$TF_CLI" = tofu ]; then
	export TF_ACC_PROVIDER_HOST=registry.opentofu.org
fi
echo "Acceptance tests with $("$TF_ACC_TERRAFORM_PATH" version | head -n 1)"

# Run from a writable copy of the package directory's test files, as go test
# would. plugin-testing copies the working directory into its temporary
# working directories, so it must be small and must not be /.
cp -r /sysutils/tree /sysutils-work
cd /sysutils-work/internal/provider
exec /sysutils/provider.test -test.v=test2json -test.count=1 \
	-test.timeout "${ACC_TIMEOUT:-30m}" "$@" >&3
