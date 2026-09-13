#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib.sh"

purge_state=0
while (($#)); do
  case $1 in
    --purge-state) purge_state=1; shift ;;
    *) persea_die "unknown argument: $1" ;;
  esac
done
persea_init_root
persea_require_root
if [[ $PERSEA_HERMETIC == 1 ]]; then persea_require_hermetic_mocks tailscale systemctl persea-process-metadata; fi
for command in tailscale systemctl install stat readlink cp mv cmp mktemp python3 find; do
  command -v "$command" >/dev/null || persea_die "required command is missing: $command"
done
persea_require_tailscale_cli
persea_require_tailscaled

install_root=$(persea_path "$PERSEA_INSTALL_ROOT")
unit_root=$(persea_path "$PERSEA_UNIT_ROOT")
installed_unit="$unit_root/$PERSEA_SIDECAR_UNIT"
socket=$(persea_path "$PERSEA_SIDECAR_SOCKET")
state=$(persea_path "$PERSEA_SIDECAR_STATE_DIR")
[[ -d $install_root && ! -L $install_root && -L $install_root/current ]] || persea_die 'installed root/current is unsafe'
current_target=$(readlink -- "$install_root/current")
[[ $current_target == releases/* && $current_target != *'..'* ]] || persea_die 'current target is ambiguous'
release="$install_root/$current_target"
persea_verify_release "$release"
[[ -f $release/units/$PERSEA_SIDECAR_UNIT && ! -L $release/units/$PERSEA_SIDECAR_UNIT ]] || persea_die 'current release lacks the sidecar unit'
[[ -f $installed_unit && ! -L $installed_unit ]] || persea_die 'installed sidecar unit is missing or unsafe'
cmp -s "$release/units/$PERSEA_SIDECAR_UNIT" "$installed_unit" || persea_die 'installed sidecar unit drift'

work=$(mktemp -d "${TMPDIR:-/tmp}/persea-terminal-sidecar-uninstall.XXXXXXXX")
cleanup_work() {
  local rc=$?
  rm -rf -- "$work"
  exit "$rc"
}
trap cleanup_work EXIT
cp -- "$installed_unit" "$work/prior-unit"
persea_capture_main_serve "$work/main-before.json"
prior_enabled=0
prior_active=0
systemctl is-enabled --quiet "$PERSEA_SIDECAR_UNIT" 2>/dev/null && prior_enabled=1
systemctl is-active --quiet "$PERSEA_SIDECAR_UNIT" 2>/dev/null && prior_active=1
((prior_active)) || persea_die 'sidecar must be active so exact Service absence can be proven before uninstall'
persea_assert_sidecar_runtime
persea_sidecar_tailscale serve status --json >"$work/serve-before.json"
service_present=$(python3 - "$work/serve-before.json" "$PERSEA_SERVICE" <<'PY'
import json, sys
config = json.load(open(sys.argv[1], encoding="utf-8"))
print("1" if sys.argv[2] in (config.get("Services") or {}) else "0")
PY
)

mutation_armed=0
restoration_in_progress=0
restore_uninstall() {
  local rc=$1 cleanup_rc=0 unit_new="$unit_root/.${PERSEA_SIDECAR_UNIT}.rollback.$$" restored_enabled restored_active
  trap - ERR INT TERM HUP
  if ((restoration_in_progress)); then exit "$rc"; fi
  restoration_in_progress=1
  set +e
  if ((mutation_armed)); then
    if [[ ! -f $installed_unit || -L $installed_unit ]] || ! cmp -s "$work/prior-unit" "$installed_unit"; then
      if [[ $PERSEA_HERMETIC == 1 ]]; then
        install -m 0444 "$work/prior-unit" "$unit_new" || cleanup_rc=1
      else
        install -o root -g root -m 0444 "$work/prior-unit" "$unit_new" || cleanup_rc=1
      fi
      mv -Tf -- "$unit_new" "$installed_unit" || cleanup_rc=1
    fi
    systemctl daemon-reload || cleanup_rc=1
    ((prior_enabled == 0)) || systemctl enable "$PERSEA_SIDECAR_UNIT" || cleanup_rc=1
    ((prior_active == 0)) || systemctl start "$PERSEA_SIDECAR_UNIT" || cleanup_rc=1
    [[ -f $installed_unit && ! -L $installed_unit ]] && cmp -s "$work/prior-unit" "$installed_unit" || cleanup_rc=1
    restored_enabled=0
    restored_active=0
    systemctl is-enabled --quiet "$PERSEA_SIDECAR_UNIT" 2>/dev/null && restored_enabled=1
    systemctl is-active --quiet "$PERSEA_SIDECAR_UNIT" 2>/dev/null && restored_active=1
    [[ $restored_enabled == "$prior_enabled" && $restored_active == "$prior_active" ]] || cleanup_rc=1
  fi
  persea_capture_main_serve "$work/main-after-rollback.json" || cleanup_rc=1
  persea_assert_main_serve_unchanged "$work/main-before.json" "$work/main-after-rollback.json" || cleanup_rc=1
  if ((cleanup_rc)); then printf 'persea-terminal deploy: sidecar uninstall-state restoration verification failed\n' >&2; fi
  exit "$rc"
}

mutation_armed=1
trap 'restore_uninstall $?' ERR
trap 'restore_uninstall 130' INT
trap 'restore_uninstall 143' TERM
trap 'restore_uninstall 129' HUP
if [[ $service_present == 1 ]]; then
  persea_sidecar_tailscale serve drain "$PERSEA_SERVICE"
  persea_sidecar_tailscale serve clear "$PERSEA_SERVICE"
fi
persea_sidecar_tailscale serve status --json >"$work/serve-after-clear.json"
python3 - "$work/serve-before.json" "$work/serve-after-clear.json" "$PERSEA_SERVICE" <<'PY'
import json, sys
before = json.load(open(sys.argv[1], encoding="utf-8"))
after = json.load(open(sys.argv[2], encoding="utf-8"))
service = sys.argv[3]
before_services = dict(before.pop("Services", {}) or {})
after_services = dict(after.pop("Services", {}) or {})
before_services.pop(service, None)
assert service not in after_services, "target Service remains configured"
assert before_services == after_services, "another sidecar Service changed"
assert before == after, "sidecar Serve state changed outside target Service"
PY
systemctl stop "$PERSEA_SIDECAR_UNIT"
systemctl disable "$PERSEA_SIDECAR_UNIT"
rm -f -- "$installed_unit"
systemctl daemon-reload
[[ ! -e $socket && ! -L $socket ]] || persea_die 'sidecar control socket remains after stop'
persea_capture_main_serve "$work/main-after.json"
persea_assert_main_serve_unchanged "$work/main-before.json" "$work/main-after.json"

state_result=preserved
if ((purge_state)); then
  persea_assert_root_private_directory "$state" 'sidecar state directory'
  unsafe_state_entry=$(find "$state" -xdev \( -type l -o \( ! -type d ! -type f \) \) -print -quit)
  [[ -z $unsafe_state_entry ]] || persea_die 'sidecar state contains a symlink or special file; refusing purge'
  find "$state" -xdev -depth -delete
  [[ ! -e $state && ! -L $state ]] || persea_die 'sidecar state purge was incomplete'
  state_result=purged
fi
trap - ERR INT TERM HUP
mutation_armed=0
printf 'SIDECAR_UNIT=removed\nSIDECAR_SERVICE=cleared\nSIDECAR_STATE=%s:%s\nRECOVERABILITY=%s\n' \
  "$state_result" "$state" "$([[ $state_result == preserved ]] && printf 'identity state retained for reinstall' || printf 'identity state irreversibly deleted by explicit --purge-state')"
