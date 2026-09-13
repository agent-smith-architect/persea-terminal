#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib.sh"
export PERSEA_HERMETIC=0
TARGET=/etc/persea-terminal/host.json
INSTALL_ROOT=/opt/persea-terminal
UNIT_ROOT=/etc/systemd/system
TAILSCALE=/usr/bin/tailscale
MAIN_SOCKET=/run/tailscale/tailscaled.sock
SIDECAR_SOCKET=/run/persea-terminal-tailscale/tailscaled.sock

die() { printf 'persea-terminal legacy migration: %s\n' "$*" >&2; exit 1; }

(($# == 0)) || die 'migrate-legacy-host.sh takes no arguments'
[[ $(id -u) == 0 ]] || die 'root execution is required'
for command in python3 sha256sum stat readlink systemctl install mv cmp sync mktemp; do
  command -v "$command" >/dev/null || die "required command is missing: $command"
done
[[ -x $TAILSCALE && ! -L $TAILSCALE ]] || die 'exact Tailscale CLI is missing or unsafe'
[[ ! -e $TARGET && ! -L $TARGET ]] || die 'host manifest already exists; refusing overwrite'
[[ -d $INSTALL_ROOT && ! -L $INSTALL_ROOT && -L $INSTALL_ROOT/current ]] || die 'legacy current release is missing or unsafe'
current=$(readlink -- "$INSTALL_ROOT/current")
[[ $current == releases/* && $current != *'..'* ]] || die 'legacy current target is ambiguous'
release="$INSTALL_ROOT/$current"
[[ -d $release && ! -L $release ]] || die 'legacy current release is unsafe'
persea_verify_legacy_release "$release"
[[ -d $UNIT_ROOT && ! -L $UNIT_ROOT ]] || die 'systemd unit root is missing or unsafe'

work=$(mktemp -d /tmp/ptmh.XXXXXXXX)
chmod 0700 "$work"
target_new=
cleanup() {
  local rc=$?
  trap - EXIT
  [[ -z $target_new ]] || rm -f -- "$target_new"
  rm -rf -- "$work"
  exit "$rc"
}
trap cleanup EXIT

"$TAILSCALE" "--socket=$MAIN_SOCKET" serve status --json >"$work/main-serve-before.json"
"$TAILSCALE" "--socket=$SIDECAR_SOCKET" serve status --json >"$work/sidecar-serve-before.json"
"$TAILSCALE" "--socket=$MAIN_SOCKET" status --json >"$work/main-status.json"
"$TAILSCALE" "--socket=$SIDECAR_SOCKET" status --json >"$work/sidecar-status.json"
"$TAILSCALE" "--socket=$SIDECAR_SOCKET" debug prefs >"$work/sidecar-prefs.json"

python3 "$SCRIPT_DIR/legacy-migrate.py" \
  --release "$release" \
  --unit-root "$UNIT_ROOT" \
  --main-status "$work/main-status.json" \
  --sidecar-status "$work/sidecar-status.json" \
  --sidecar-prefs "$work/sidecar-prefs.json" \
  --output "$work/host.json" || die 'legacy state is ambiguous or inconsistent'
python3 "$SCRIPT_DIR/host-config.py" validate "$work/host.json" >/dev/null || die 'derived host manifest failed strict validation'

python3 - "$work/host.json" >"$work/managed-units" <<'PY'
import json, sys
value = json.load(open(sys.argv[1], encoding="utf-8"))
for realm in value["realms"]:
    print(f"persea-terminal-broker-{realm['id']}.service")
print("persea-terminal-front.service")
PY
while IFS= read -r unit; do
  [[ -f $UNIT_ROOT/$unit && ! -L $UNIT_ROOT/$unit ]] || die "installed unit is missing or unsafe: $unit"
  read -r owner group mode < <(stat -Lc '%u %g %a' -- "$UNIT_ROOT/$unit")
  [[ $owner == 0 && $group == 0 && $mode == 444 ]] || die "installed unit metadata drift: $unit"
  systemctl is-active --quiet "$unit" || die "legacy application unit is inactive: $unit"
  fragment=$(systemctl show "$unit" --property=FragmentPath --value)
  [[ $fragment == "$UNIT_ROOT/$unit" ]] || die "legacy application unit fragment drift: $unit"
done <"$work/managed-units"
sidecar_unit=persea-terminal-tailscaled.service
[[ -f $UNIT_ROOT/$sidecar_unit && ! -L $UNIT_ROOT/$sidecar_unit ]] || die 'installed Tailscale sidecar unit is missing or unsafe'
cmp -s "$release/units/$sidecar_unit" "$UNIT_ROOT/$sidecar_unit" || die 'installed Tailscale sidecar unit differs from the legacy release'
read -r owner group mode < <(stat -Lc '%u %g %a' -- "$UNIT_ROOT/$sidecar_unit")
[[ $owner == 0 && $group == 0 && $mode == 444 ]] || die 'installed Tailscale sidecar unit metadata drift'
systemctl is-active --quiet "$sidecar_unit" || die 'legacy Tailscale sidecar is inactive'
fragment=$(systemctl show "$sidecar_unit" --property=FragmentPath --value)
[[ $fragment == "$UNIT_ROOT/$sidecar_unit" ]] || die 'legacy Tailscale sidecar unit fragment drift'

"$TAILSCALE" "--socket=$MAIN_SOCKET" serve status --json >"$work/main-serve-after.json"
"$TAILSCALE" "--socket=$SIDECAR_SOCKET" serve status --json >"$work/sidecar-serve-after.json"
cmp -s "$work/main-serve-before.json" "$work/main-serve-after.json" || die 'protected main-node Serve state changed during readback'
cmp -s "$work/sidecar-serve-before.json" "$work/sidecar-serve-after.json" || die 'sidecar Serve state changed during readback'

target_dir=${TARGET%/*}
if [[ -e $target_dir ]]; then
  [[ -d $target_dir && ! -L $target_dir ]] || die 'host manifest directory is unsafe'
  read -r owner mode < <(stat -Lc '%u %a' -- "$target_dir")
  [[ $owner == 0 && $((8#$mode & 0022)) == 0 ]] || die 'host manifest directory owner/mode is unsafe'
else
  install -d -o root -g root -m 0755 "$target_dir"
fi
target_new="$target_dir/.host.json.new.$$"
[[ ! -e $target_new && ! -L $target_new ]] || die 'host manifest staging target already exists'
install -o root -g root -m 0600 "$work/host.json" "$target_new"
mv -Tf -- "$target_new" "$TARGET"
target_new=
sync -f "$TARGET"
sync -f "$target_dir"
python3 "$SCRIPT_DIR/host-config.py" validate "$TARGET" --production >/dev/null || die 'installed host manifest metadata/readback failed'
digest=$(python3 "$SCRIPT_DIR/host-config.py" digest "$TARGET" --production)
printf 'HOST_CONFIG=%s\nSHA256=%s\nSOURCE_RELEASE=%s\n' "$TARGET" "$digest" "$current"
