#!/usr/bin/env bash
# Run the provider test suite, including acceptance tests, as root inside
# stock distribution containers, to cover the distribution-specific code
# paths (apt/dnf/apk, OpenRC, glibc vs musl locales, tzdata layouts,
# iptables/nftables builds) that the Debian-based testacc-docker image
# cannot.
#
# The static test binary and the CLI are built/downloaded once on the host
# and mounted read-only into each container, where
# testacc-distro-entrypoint.sh installs the needed tools and runs them. The
# run fails on test failures and on skips not allowed for that distribution.
#
# Usage: testacc-distros.sh [--bundle DIR] [--build-only] [image...]
#   --bundle DIR   keep the bundle (test binary, CLI, test files, entrypoint)
#                  in DIR, and reuse it if DIR/provider.test exists. CI builds
#                  it once and runs each distribution in a job of its own.
#   --build-only   build the bundle and exit.
#   image...       the images to test in (default: $DISTROS or the CI matrix)
# Environment:
#   TF_CLI, TF_CLI_VERSION  CLI to test with (default: terraform latest)
#   ACC_RUN                 -test.run pattern (default: all tests)
#   ACC_EXTRA_ALLOWED_SKIPS extra skip reasons to allow, for local runs
set -euo pipefail

cd "$(dirname "$0")/.."

: "${TF_CLI:=terraform}"
: "${TF_CLI_VERSION:=latest}"
bundle=
build_only=
while [ $# -gt 0 ]; do
	case "$1" in
	--bundle) bundle=${2:?--bundle needs a directory}; shift 2 ;;
	--build-only) build_only=1; shift ;;
	--) shift; break ;;
	-*) echo "unknown option $1" >&2; exit 2 ;;
	*) break ;;
	esac
done
read -r -a images <<<"${*:-${DISTROS:-debian:stable alpine:latest fedora:latest}}"

# Skips allowed on every distribution: no systemd as PID 1, no module loading
# or writable /proc/sys, and no swapon in a container, and the upgrade tests,
# which need go and git. Whether su's PAM session runs pam_limits depends on
# the distribution (Debian ships it commented out).
common_skips='the su PAM session does not use pam_limits on this host|systemd is not PID 1|is not in any release yet|CAP_SYS_MODULE is not in the effective capability set|the kernel does not allow setting [^ ]+ here|swapon is not permitted here|SYSUTILS_UPGRADE_FROM_REF is not set'

# allowed_skips IMAGE prints the skip reasons allowed on IMAGE on top of
# common_skips.
allowed_skips() {
	case "$1" in
	alpine*)
		# musl has no locale database: no locale, locale-gen or localedef.
		# Alpine has no alternatives system (update-alternatives or
		# chkconfig alternatives). The apt-only package test. sudo is not
		# installed, so the tests that need visudo skip. OpenRC runs, so the
		# service data source test for hosts without a supported init
		# system skips.
		echo 'neither locale-gen nor localedef is installed|the locale command was not found|no alternatives tool|package manager is apk, not apt|visudo is not installed|openrc is running, which the data source supports'
		;;
	fedora* | *rhel* | *centos* | *almalinux* | *rockylinux*)
		# Only the Alpine container boots OpenRC. The apt-only package test.
		echo 'OpenRC did not boot this host|package manager is (dnf|yum), not apt'
		;;
	*)
		echo 'OpenRC did not boot this host'
		;;
	esac
}

if [ -n "$bundle" ]; then
	mkdir -p "$bundle"
	out=$(cd "$bundle" && pwd)
else
	out=$(mktemp -d)
	trap 'rm -rf "$out"' EXIT
fi
chmod 0755 "$out"

if [ ! -e "$out/provider.test" ]; then
	echo "Building the static test binary"
	CGO_ENABLED=0 TF_ACC=1 go test -c -o "$out/provider.test" ./internal/provider
	scripts/install-tf-cli.sh "$TF_CLI" "$TF_CLI_VERSION" "$out" >/dev/null
	cp scripts/testacc-distro-entrypoint.sh "$out/entrypoint.sh"
	# The files the tests read relative to the package directory, as go test
	# runs them: testdata/ and ../../examples/.
	mkdir -p "$out/tree/internal/provider"
	cp -r examples "$out/tree/"
	cp -r internal/provider/testdata "$out/tree/internal/provider/"
else
	echo "Reusing the bundle in $out"
	# Artifact uploads do not keep file modes.
	chmod 0755 "$out/provider.test" "$out/$TF_CLI"
fi
if [ -n "$build_only" ]; then
	exit 0
fi

run_args=()
if [ -n "${ACC_RUN:-}" ]; then
	run_args=(-test.run "$ACC_RUN")
fi

failed=()
for image in "${images[@]}"; do
	echo
	echo "=== $image ($TF_CLI $TF_CLI_VERSION)"
	report=$(mktemp)
	status=0
	# SYS_ADMIN and an unconfined AppArmor profile let the tests mount a
	# tmpfs and create network namespaces inside the container; NET_ADMIN
	# lets the firewall tests change the firewall of those namespaces. The
	# host's mounts and firewall are not affected. --init: the test binary
	# must not be PID 1, which would leave the daemons that OpenRC's
	# start-stop-daemon orphans unreaped, so that stopping them never
	# completes ("process refused to stop"). Everything runs as root, so
	# no-new-privileges costs nothing and keeps setuid programs in the image
	# from gaining more. No credentials, sockets or host paths other than
	# the read-only bundle are passed in.
	docker run --rm --init --pull missing \
		--cap-add SYS_ADMIN --cap-add NET_ADMIN --security-opt apparmor=unconfined \
		--security-opt no-new-privileges \
		-e TF_CLI="$TF_CLI" -e ACC_TIMEOUT -e SYSUTILS_ACC_PACKAGE \
		-v "$out:/sysutils:ro" \
		"$image" /bin/sh /sysutils/entrypoint.sh "${run_args[@]}" \
		| go tool test2json -t -p github.com/blechschmidt/terraform-provider-sysutils/internal/provider \
		| tee "$report" | scripts/acc-report.sh print || status=$?
	extra=$(allowed_skips "$image")
	ACC_ALLOWED_SKIPS="$common_skips${extra:+|$extra}${ACC_EXTRA_ALLOWED_SKIPS:+|$ACC_EXTRA_ALLOWED_SKIPS}" \
		scripts/acc-report.sh check-skips "$report" || status=$?
	rm -f "$report"
	if [ "$status" -ne 0 ]; then
		failed+=("$image")
	fi
done

echo
if [ "${#failed[@]}" -ne 0 ]; then
	echo "FAIL: ${failed[*]}"
	exit 1
fi
echo "PASS: ${images[*]}"
