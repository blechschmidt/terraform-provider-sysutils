#!/usr/bin/env bash
# Run the full test suite, including acceptance tests, as root inside the
# disposable test container (see Dockerfile.test and "make testacc-docker").
#
# Besides failing on test failures, this fails if any test was skipped for a
# reason other than those matched by ACC_ALLOWED_SKIPS (an extended regular
# expression), so that the root-only user, group, chown and file_line tests
# can't silently stop running.
set -euo pipefail

: "${TF_CLI:=terraform}"
# Allowed by default: the systemd and OpenRC tests (no init system in the
# container; testacc-distros.sh boots OpenRC on Alpine), the kernel_module
# and sysctl tests (no CAP_SYS_MODULE and a read-only /proc/sys in a
# container), the swap tests (swapon is not permitted in a container),
# and the local-baseline upgrade tests
# unless SYSUTILS_UPGRADE_FROM_REF is set, as it is in CI.
: "${ACC_ALLOWED_SKIPS:=systemd is not PID 1|OpenRC did not boot this host|is not in any release yet|CAP_SYS_MODULE is not in the effective capability set|the kernel does not allow setting [^ ]+ here|swapon is not permitted here|SYSUTILS_UPGRADE_FROM_REF is not set}"
export ACC_ALLOWED_SKIPS

if [ "$(id -u)" -ne 0 ]; then
	echo "testacc-container.sh must run as root" >&2
	exit 1
fi

cli_path=$(command -v "$TF_CLI")
export TF_ACC=1
export TF_ACC_TERRAFORM_PATH="$cli_path"
if [ "$TF_CLI" = tofu ]; then
	# OpenTofu resolves unqualified provider names against its own registry,
	# so the test framework must register the in-process provider there.
	export TF_ACC_PROVIDER_HOST=registry.opentofu.org
fi

echo "Acceptance tests with $("$cli_path" version | head -n 1) at $cli_path"

report=$(mktemp)
status=0
# test2json events are printed as plain test output and kept in $report for
# the skip check below.
go test -json -count=1 -timeout "${ACC_TIMEOUT:-30m}" "$@" ./... | tee "$report" | scripts/acc-report.sh print || status=$?

scripts/acc-report.sh check-skips "$report" || status=$?
rm -f "$report"
exit "$status"
