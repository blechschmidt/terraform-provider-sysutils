#!/usr/bin/env bash
# Check that every example configuration is valid, so that the HCL shown in
# docs/ (which tfplugindocs copies from examples/) and in README.md works:
#
#  1. terraform fmt -check on examples/.
#  2. terraform validate on each example directory, with the provider built
#     from this checkout (dev_overrides), so that the schema, attribute
#     validators and ValidateConfig checks of the current code are applied.
#     The directories are examples/, examples/provider, examples/complete,
#     examples/guides/* and every examples/resources/* and
#     examples/data-sources/* directory. A directory without a
#     required_providers block (most resource examples) gets one for
#     blechschmidt/sysutils; without it Terraform would look for
#     hashicorp/sysutils.
#  3. No fenced terraform or hcl code in templates/: pages must pull their
#     HCL from examples/ with tffile, so that step 2 covers it.
#  4. Every ```terraform block in README.md must appear verbatim in a file
#     below examples/.
#
# Nothing is planned or applied, so the host is not changed and root is not
# needed. validate needs no init when every provider is overridden, so no
# network access is needed either. Uses the terraform CLI (TF_CLI), not tofu:
# tofu init fails with dev_overrides for a provider its registry lacks.
set -euo pipefail

: "${TF_CLI:=terraform}"
repo=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

failed=0
fail() {
	echo "FAIL: $*" >&2
	failed=1
}

echo "=== terraform fmt -check examples/"
"$TF_CLI" fmt -recursive -check -diff "$repo/examples" || fail "examples/ is not formatted; run \"make docs\" or \"terraform fmt -recursive examples/\""

echo "=== Build provider"
mkdir -p "$work/bin"
(cd "$repo" && go build -o "$work/bin/terraform-provider-sysutils" .)
cat >"$work/cli.tfrc" <<TFRC
provider_installation {
  dev_overrides {
    "blechschmidt/sysutils" = "$work/bin"
  }
  direct {}
}
TFRC
export TF_CLI_CONFIG_FILE="$work/cli.tfrc" TF_IN_AUTOMATION=1 CHECKPOINT_DISABLE=1

provider_block='terraform {
  required_providers {
    sysutils = {
      source = "blechschmidt/sysutils"
    }
  }
}'

dirs=(examples examples/provider examples/complete)
for d in "$repo"/examples/guides/*/ "$repo"/examples/resources/*/ "$repo"/examples/data-sources/*/; do
	d=${d%/}
	dirs+=("${d#"$repo"/}")
done

n=0
for d in "${dirs[@]}"; do
	# Validate a copy, so that no .terraform directory is left behind.
	# Subdirectories are copied too (templates, keys, files); for examples/
	# itself, which contains all the others, only its own files are.
	dest="$work/validate/$n"
	n=$((n + 1))
	mkdir -p "$dest"
	if [ "$d" = examples ]; then
		find "$repo/examples" -maxdepth 1 -type f -exec cp {} "$dest/" \;
	else
		cp -r "$repo/$d/." "$dest/"
	fi
	if ! grep -qs 'required_providers' "$dest"/*.tf; then
		printf '%s\n' "$provider_block" >"$dest/zz_required_providers.tf"
	fi
	if out=$("$TF_CLI" -chdir="$dest" validate -no-color 2>&1); then
		echo "ok   $d"
	else
		echo "FAIL $d"
		# Drop the dev_overrides warning, which is expected: skip from its
		# heading to the next diagnostic.
		printf '%s\n' "$out" | awk '
			/^(Error|Warning): / { skip = /Provider development overrides/ }
			!skip { print "     " $0 }
		'
		failed=1
	fi
done

echo "=== No inline HCL in templates/"
if grep -rnE '^[[:space:]]*```[[:space:]]*(terraform|hcl|tf)[[:space:]]*$' "$repo/templates"; then
	fail "templates/ contains fenced terraform/hcl code; move it to a file below examples/ and include it with {{ tffile \"...\" }}"
fi

echo "=== README.md terraform blocks come from examples/"
# Extract each ```terraform ... ``` block of README.md into its own file.
mkdir -p "$work/readme"
awk -v dir="$work/readme" '
	/^```terraform[[:space:]]*$/ { n++; file = sprintf("%s/%03d.tf", dir, n); inblock = 1; start = NR; next }
	inblock && /^```[[:space:]]*$/ { inblock = 0; print start > (file ".line"); close(file ".line"); close(file); next }
	inblock { print > file }
' "$repo/README.md"
for block in "$work"/readme/*.tf; do
	[ -e "$block" ] || continue
	line=$(cat "$block.line")
	found=false
	while IFS= read -r -d '' f; do
		if python3 - "$block" "$f" <<'PY'; then
import sys
block = open(sys.argv[1]).read()
sys.exit(0 if block in open(sys.argv[2]).read() else 1)
PY
			found=true
			echo "ok   README.md:$line (${f#"$repo"/})"
			break
		fi
	done < <(find "$repo/examples" -name '*.tf' -print0 | sort -z)
	if [ "$found" = false ]; then
		fail "README.md:$line: the terraform block is not an excerpt of any file below examples/; copy it from one, or add it to one"
	fi
done

if [ "$failed" -ne 0 ]; then
	echo "examples check failed" >&2
	exit 1
fi
echo "PASS: all examples are formatted and valid"
