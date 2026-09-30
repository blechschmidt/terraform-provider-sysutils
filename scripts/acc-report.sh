#!/usr/bin/env bash
# Helpers for test2json reports of acceptance-test runs, shared by
# testacc-container.sh and testacc-distros.sh.
#
#   acc-report.sh print               test2json events on stdin -> plain test output
#   acc-report.sh check-skips REPORT  fail if a test in REPORT was skipped for a
#                                     reason not matched by ACC_ALLOWED_SKIPS (an
#                                     extended regular expression)
set -euo pipefail

case "${1:-}" in
print)
	awk '
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
	}'
	;;
check-skips)
	report=${2:?usage: acc-report.sh check-skips REPORT}
	: "${ACC_ALLOWED_SKIPS:?ACC_ALLOWED_SKIPS is not set}"
	# Collect the output of every skipped test and print the unexpected ones.
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
	if [ -n "$unexpected" ]; then
		echo
		echo "FAIL: tests were skipped unexpectedly (allowed: /$ACC_ALLOWED_SKIPS/):"
		printf '%s\n' "$unexpected"
		exit 1
	fi
	;;
*)
	echo "usage: acc-report.sh print | check-skips REPORT" >&2
	exit 2
	;;
esac
