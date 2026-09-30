#!/usr/bin/env bash
# Check that the links in docs/ work on the Terraform Registry, not only on
# GitHub. The Registry serves docs/resources/file.md at
# .../latest/docs/resources/file, strips ".md" from link targets and resolves
# them against the page URL. So:
#
#  1. docs/index.md is served at .../latest/docs (no trailing slash), where
#     "./resources/file.md" becomes .../latest/resources/file, a 404. The
#     index page may only use absolute URLs and in-page anchors.
#  2. .../latest/docs/index redirects to .../latest/docs and drops the
#     anchor, so no page may link to index.md relatively.
#  3. Other pages may only use relative links of the forms "./page.md" and
#     "../(resources|data-sources|functions|guides)/page.md", each
#     optionally with an anchor, and the linked page must exist.
set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
cd "$repo"
fail=0

# Prints "file:line:target" for every markdown link target in docs/ that is
# neither absolute nor a pure in-page anchor.
relative_links() {
	grep -rnoE '\]\([^)]+\)' docs | sed -E 's/\]\((.*)\)$/\1/' |
		grep -vE ':(https?://|mailto:|#)'
}

while IFS= read -r entry; do
	file=${entry%%:*}
	rest=${entry#*:}
	target=${rest#*:}
	path=${target%%#*}
	if [ "$file" = docs/index.md ]; then
		echo "$file: relative link $target breaks on the Registry; use https://registry.terraform.io/providers/blechschmidt/sysutils/latest/docs/..." >&2
		fail=1
		continue
	fi
	case $path in
	../index.md | ./index.md | index.md)
		echo "$file: link $target drops its anchor on the Registry; use https://registry.terraform.io/providers/blechschmidt/sysutils/latest/docs#..." >&2
		fail=1
		continue
		;;
	esac
	if ! [[ $path =~ ^(\./[a-z0-9_-]+|\.\./(resources|data-sources|functions|guides)/[a-z0-9_-]+)\.md$ ]]; then
		echo "$file: unsupported relative link $target" >&2
		fail=1
		continue
	fi
	if [ ! -f "$(dirname "$file")/$path" ]; then
		echo "$file: link $target points to a missing page" >&2
		fail=1
	fi
done < <(relative_links)

if [ "$fail" -ne 0 ]; then
	echo 'Fix the links in templates/ (or the schema descriptions) and run "make docs".' >&2
	exit 1
fi
echo 'docs/ links OK'
