#!/bin/sh
# Prints the CHANGELOG.md section of one version, without its heading, for
# use as the GitHub release notes: scripts/release-notes.sh 1.2.0
# Fails if CHANGELOG.md has no "## <version>" section, so that a release
# can't be published before its changelog entry is written.
set -eu
version=${1#v}
changelog=${2:-CHANGELOG.md}
notes=$(awk -v v="$version" '
	/^## / {
		if (found) exit
		if ($2 == v) { found = 1; next }
	}
	found { print }
' "$changelog")
if [ -z "$(printf '%s' "$notes" | tr -d '[:space:]')" ]; then
	echo "$changelog has no section for version $version" >&2
	exit 1
fi
printf '%s\n' "$notes"
