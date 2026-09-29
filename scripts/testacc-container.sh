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
# Allowed by default: the systemd tests (no systemd as PID 1 in a
# container), the kernel_module and sysctl tests (no CAP_SYS_MODULE and a
# read-only /proc/sys in a container), and the local-baseline upgrade tests
# unless SYSUTILS_UPGRADE_FROM_REF is set, as it is in CI.
: "${ACC_ALLOWED_SKIPS:=systemd is not PID 1|is not in any release yet|CAP_SYS_MODULE is not in the effective capability set|the kernel does not allow setting [^ ]+ here|SYSUTILS_UPGRADE_FROM_REF is not set}"

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
go test -json -count=1 -timeout "${ACC_TIMEOUT:-30m}" "$@" ./... | tee "$report" | awk '
	/"Action":"output"/ {
		out = $0
		sub(/.*"Output":"/, "", out)
		sub(/"}$/, "", out)
		gsub(/\\\\/, "\001", out)
		gsub(/\\n/, "\n", out)
		gsub(/\\t/, "\t", out)
		gsub(/\\"/, "\"", out)
		gsub(/\\u003c/, "<", out)
		gsub(/\\u003e/, ">", out)
		gsub(/\\u0026/, "\\&", out)
		gsub(/\001/, "\\", out)
		printf "%s", out
		fflush()
	}' || status=$?

# Collect the output of every skipped test and fail on unexpected skips.
unexpected=$(awk -v allowed="$ACC_ALLOWED_SKIPS" '
	function field(name,   s) {
		s = $0
		if (!sub(".*\"" name "\":\"", "", s)) return ""
		sub(/".*/, "", s)
		return s
	}
	{
		test = field("Test")
		if (test == "") next
		key = field("Package") " " test
	}
	/"Action":"output"/ {
		out = $0
		sub(/.*"Output":"/, "", out)
		sub(/"}$/, "", out)
		if (out !~ /^(=== |--- |    --- )/) outputs[key] = outputs[key] out
	}
	/"Action":"skip"/ {
		if (outputs[key] !~ allowed) printf "%s:%s\n", key, outputs[key]
	}' "$report")
rm -f "$report"

if [ -n "$unexpected" ]; then
	echo
	echo "FAIL: tests were skipped unexpectedly (allowed: /$ACC_ALLOWED_SKIPS/):"
	printf '%s\n' "$unexpected"
	[ "$status" -ne 0 ] || status=1
fi
exit "$status"
