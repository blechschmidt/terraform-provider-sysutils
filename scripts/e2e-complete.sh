#!/usr/bin/env bash
# End-to-end test of examples/complete with the locally built provider:
# init, plan, apply, a second plan that must be empty (the stack is
# idempotent), destroy, and checks that the host is left as it was.
#
# Runs as root, because the stack creates a user, a group, a systemd unit and
# a sysctl.d file on the host. Everything else is written below the sandbox
# directory TF_VAR_root_dir. Set TF_CLI=tofu to use OpenTofu. The systemd unit
# is skipped automatically if systemd is not PID 1 (TF_VAR_manage_systemd
# overrides this).
set -euo pipefail

: "${TF_CLI:=terraform}"
: "${TF_VAR_service_name:=sysutilse2e}"
: "${TF_VAR_root_dir:=/var/tmp/sysutils-e2e/rootfs}"
export TF_VAR_service_name TF_VAR_root_dir
if [ -z "${TF_VAR_manage_systemd:-}" ]; then
	if [ "$(cat /proc/1/comm 2>/dev/null)" = systemd ]; then
		export TF_VAR_manage_systemd=true
	else
		export TF_VAR_manage_systemd=false
	fi
fi
name=$TF_VAR_service_name
root=$TF_VAR_root_dir

if [ "$(id -u)" -ne 0 ]; then
	echo "e2e-complete.sh must run as root" >&2
	exit 1
fi

repo=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)

step() { printf '\n=== %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

# Nothing may be left over from an earlier run: the resources refuse to take
# over existing users, units and sysctl entries.
getent passwd "$name" >/dev/null && fail "user $name already exists"
getent group "$name" >/dev/null && fail "group $name already exists"
[ ! -e "$root" ] || fail "$root already exists"
[ ! -e "/etc/systemd/system/$name.service" ] || fail "/etc/systemd/system/$name.service already exists"
[ ! -e "/etc/sysctl.d/90-$name.conf" ] || fail "/etc/sysctl.d/90-$name.conf already exists"

step "Build provider"
mkdir -p "$work/bin"
(cd "$repo" && go build -o "$work/bin/terraform-provider-sysutils" .)

# dev_overrides makes the CLI use the provider just built instead of the
# registry release, for every configuration in this process.
cat >"$work/cli.tfrc" <<TFRC
provider_installation {
  dev_overrides {
    "blechschmidt/sysutils" = "$work/bin"
  }
  direct {}
}
TFRC
export TF_CLI_CONFIG_FILE="$work/cli.tfrc"
export TF_IN_AUTOMATION=1 CHECKPOINT_DISABLE=1

# Work on a copy, so that no state or lock file is left in the repository.
cp -r "$repo/examples/complete" "$work/stack"
tf() { "$TF_CLI" -chdir="$work/stack" "$@"; }

"$TF_CLI" version
echo "service_name=$name root_dir=$root manage_systemd=$TF_VAR_manage_systemd"

destroyed=false
cleanup() {
	# Tear down whatever was created if a later step failed.
	if [ "$destroyed" = false ] && [ -f "$work/stack/terraform.tfstate" ]; then
		step "Destroy after failure"
		tf destroy -auto-approve -input=false -no-color || true
	fi
	rm -rf "$work"
}
trap cleanup EXIT

step "Init"
if [ "$(basename "$TF_CLI")" = tofu ]; then
	# OpenTofu's init still looks up overridden providers in its registry,
	# which does not have this one, and fails. With dev_overrides and no
	# other providers, init is not needed.
	echo "skipped for OpenTofu"
else
	tf init -input=false -no-color
fi

step "Validate"
tf validate -no-color

step "Plan"
tf plan -input=false -no-color -out="$work/plan"

step "Apply"
tf apply -input=false -no-color "$work/plan"

step "Check the host"
conf="$root/etc/$name/$name.conf"
[ -f "$conf" ] || fail "$conf was not written"
grep -qx "SERVICE_NAME=$name" "$conf" || fail "$conf has unexpected content"
[ "$(stat -c '%a %U %G' "$conf")" = "640 root $name" ] || fail "$conf: mode/owner $(stat -c '%a %U %G' "$conf")"
[ "$(stat -c '%a %U %G' "$root/var/lib/$name")" = "750 $name $name" ] || fail "state dir: $(stat -c '%a %U %G' "$root/var/lib/$name")"
grep -q "^30 3 \* \* \* $name find " "$root/etc/cron.d/$name-cleanup" || fail "cron job not written"
grep -qx "127.0.0.1 $name.internal" "$root/etc/hosts" || fail "hosts entry not written"
grep -qx "net.core.somaxconn = ${TF_VAR_somaxconn:-4096}" "/etc/sysctl.d/90-$name.conf" || fail "sysctl entry not written"
getent passwd "$name" >/dev/null || fail "user $name was not created"
[ ! -e "/etc/cron.d/$name-cleanup" ] || fail "cron job escaped the sandbox"
if [ "$TF_VAR_manage_systemd" = true ]; then
	systemctl is-active --quiet "$name.service" || fail "$name.service is not running"
	systemctl is-enabled --quiet "$name.service" || fail "$name.service is not enabled"
fi
echo "ok"

step "Plan again (must be empty)"
code=0
tf plan -input=false -no-color -detailed-exitcode || code=$?
case $code in
0) echo "ok: no changes" ;;
2) fail "the second plan is not empty: the stack is not idempotent" ;;
*) fail "the second plan failed with exit code $code" ;;
esac

step "Destroy"
tf destroy -auto-approve -input=false -no-color
destroyed=true

step "Check the host is clean"
[ ! -e "$root" ] || fail "$root still exists"
getent passwd "$name" >/dev/null && fail "user $name still exists"
getent group "$name" >/dev/null && fail "group $name still exists"
[ ! -e "/etc/sysctl.d/90-$name.conf" ] || fail "/etc/sysctl.d/90-$name.conf still exists"
[ ! -e "/etc/systemd/system/$name.service" ] || fail "unit file still exists"
echo "ok"

printf '\nPASS: examples/complete applied, re-planned empty and destroyed\n'
