#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib.sh"

target=
activate_local=0
while (($#)); do
  case $1 in
    --to-release) [[ $# -ge 2 ]] || persea_die 'missing --to-release value'; target=$2; shift 2 ;;
    --activate-local) activate_local=1; shift ;;
    *) persea_die "unknown argument: $1" ;;
  esac
done
persea_init_root
persea_require_root
if [[ $PERSEA_HERMETIC == 1 ]]; then persea_require_hermetic_mocks systemctl; fi

install_root=$(persea_path "$PERSEA_INSTALL_ROOT")
unit_root=$(persea_path "$PERSEA_UNIT_ROOT")
live_host=$PERSEA_HOST_MANIFEST_PATH
[[ -d $install_root && ! -L $install_root ]] || persea_die 'installed root is missing or unsafe'
[[ -L $install_root/current ]] || persea_die 'current is not an exact symlink'
current=$(readlink -- "$install_root/current")
[[ $current == releases/* && $current != *'..'* ]] || persea_die 'current target is ambiguous'
current_release="$install_root/$current"
persea_verify_release "$current_release"
cmp -s "$live_host" "$current_release/config/host.json" || persea_die 'live host manifest does not match the current release'
persea_assert_installed_units "$current_release" "$unit_root"
current_units=()
persea_read_release_inventory "$current_release" current_units

old_previous_present=0
old_previous=
if [[ -e $install_root/previous || -L $install_root/previous ]]; then
  [[ -L $install_root/previous ]] || persea_die 'previous is not an exact symlink'
  old_previous=$(readlink -- "$install_root/previous")
  [[ $old_previous == releases/* && $old_previous != *'..'* && -d $install_root/$old_previous && ! -L $install_root/$old_previous ]] || persea_die 'previous target is ambiguous'
  old_previous_present=1
fi
if [[ -z $target ]]; then
  [[ -L $install_root/previous ]] || persea_die 'no previous release is recorded'
  target=$(readlink -- "$install_root/previous")
fi
[[ $target == releases/* && $target != *'..'* ]] || persea_die 'rollback target must be a relative releases/ path'
[[ $target != "$current" ]] || persea_die 'rollback target is already current'
target_release="$install_root/$target"
persea_verify_release "$target_release"
target_units=()
persea_read_release_inventory "$target_release" target_units
persea_load_release_manifest "$target_release"
persea_verify_host_accounts
target_broker_units=("${PERSEA_BROKER_UNITS[@]}")

declare -A current_set=() target_set=() union_seen=() was_active_set=()
for unit in "${current_units[@]}"; do current_set[$unit]=1; done
for unit in "${target_units[@]}"; do target_set[$unit]=1; done
union_units=()
for unit in "${current_units[@]}" "${target_units[@]}"; do
  [[ -n ${union_seen[$unit]+x} ]] && continue
  union_seen[$unit]=1
  union_units+=("$unit")
done
current_only=()
target_only=()
union_brokers=()
current_broker_units=()
for unit in "${current_units[@]}"; do [[ -n ${target_set[$unit]+x} ]] || current_only+=("$unit"); done
for unit in "${target_units[@]}"; do [[ -n ${current_set[$unit]+x} ]] || target_only+=("$unit"); done
for unit in "${union_units[@]}"; do [[ $unit == persea-terminal-front.service ]] || union_brokers+=("$unit"); done
for unit in "${current_units[@]}"; do [[ $unit == persea-terminal-front.service ]] || current_broker_units+=("$unit"); done
stop_order=(persea-terminal-front.service "${union_brokers[@]}")

was_enabled=()
was_active=()
for unit in "${union_units[@]}"; do
  if systemctl is-enabled --quiet "$unit" 2>/dev/null; then was_enabled+=("$unit"); fi
  if systemctl is-active --quiet "$unit" 2>/dev/null; then was_active+=("$unit"); was_active_set[$unit]=1; fi
done

# The sidecar is a reverse-dependent lifecycle, not part of a release's
# application-unit inventory. Snapshot both intents before the first front
# stop: stopping the front can stop the sidecar, while enablement survives.
sidecar_unit=persea-terminal-tailscaled.service
sidecar_was_active=0
sidecar_was_enabled=0
if systemctl is-active --quiet "$sidecar_unit" 2>/dev/null; then sidecar_was_active=1; fi
if systemctl is-enabled --quiet "$sidecar_unit" 2>/dev/null; then sidecar_was_enabled=1; fi
sidecar_managed=$((sidecar_was_active || sidecar_was_enabled))
if ((activate_local && sidecar_was_enabled && !sidecar_was_active)); then
  persea_die 'enabled Persea Terminal sidecar is inactive; refusing activated rollback before mutation'
fi
start_sidecar() {
  systemctl start "$sidecar_unit" || {
    printf 'persea-terminal deploy: sidecar recovery start failed: %s\n' "$sidecar_unit" >&2
    return 1
  }
}

work=$(mktemp -d /tmp/ptrb.XXXXXXXX)
chmod 0700 "$work"
cp -- "$live_host" "$work/host.json"
temporary_targets=()
cleanup() {
  local rc=$? path
  for path in "${temporary_targets[@]}"; do
    [[ $path == "$install_root"/.* || $path == "$unit_root"/.* || $path == "$(dirname -- "$live_host")"/.* ]] && rm -f -- "$path"
  done
  rm -rf -- "$work"
  exit "$rc"
}
trap cleanup EXIT

restoration_in_progress=0
restore_source() {
  local rc=$1 cleanup_rc=0 host_restore
  trap - ERR INT TERM HUP
  if ((restoration_in_progress)); then exit "$rc"; fi
  restoration_in_progress=1
  set +e
  systemctl stop "${stop_order[@]}" || cleanup_rc=1
  systemctl disable "${union_units[@]}" || cleanup_rc=1
  persea_install_release_units "$current_release" "$unit_root" || cleanup_rc=1
  persea_remove_installed_units "$unit_root" "${target_only[@]}" || cleanup_rc=1
  host_restore="$(dirname -- "$live_host")/.host.json.rollback.$$"
  if [[ $PERSEA_HERMETIC == 1 ]]; then
    install -m 0600 "$work/host.json" "$host_restore" || cleanup_rc=1
  else
    install -o root -g root -m 0600 "$work/host.json" "$host_restore" || cleanup_rc=1
  fi
  mv -Tf -- "$host_restore" "$live_host" || cleanup_rc=1
  ln -s -- "$current" "$install_root/.current.rollback.$$" &&
    mv -Tf -- "$install_root/.current.rollback.$$" "$install_root/current" || cleanup_rc=1
  if ((old_previous_present)); then
    ln -s -- "$old_previous" "$install_root/.previous.rollback.$$" &&
      mv -Tf -- "$install_root/.previous.rollback.$$" "$install_root/previous" || cleanup_rc=1
  else
    rm -f -- "$install_root/previous" || cleanup_rc=1
  fi
  systemctl daemon-reload || cleanup_rc=1
  ((${#was_enabled[@]} == 0)) || systemctl enable "${was_enabled[@]}" || cleanup_rc=1
  if ((sidecar_managed)); then
    if ((sidecar_was_enabled)); then systemctl enable "$sidecar_unit" || cleanup_rc=1; else systemctl disable "$sidecar_unit" || cleanup_rc=1; fi
  fi
  for unit in "${current_broker_units[@]}"; do
    [[ -n ${was_active_set[$unit]+x} ]] || continue
    systemctl start "$unit" || cleanup_rc=1
  done
  if [[ -n ${was_active_set[persea-terminal-front.service]+x} ]]; then systemctl start persea-terminal-front.service || cleanup_rc=1; fi
  if ((sidecar_was_active)); then start_sidecar || cleanup_rc=1; fi
  "$SCRIPT_DIR/verify.sh" >/dev/null || cleanup_rc=1
  if ((activate_local)); then
    if ((sidecar_was_active)); then
      "$SCRIPT_DIR/verify.sh" --require-active --require-sidecar >/dev/null || cleanup_rc=1
    else
      "$SCRIPT_DIR/verify.sh" --require-active >/dev/null || cleanup_rc=1
    fi
  fi
  if ((cleanup_rc)); then printf 'persea-terminal deploy: rollback source restoration failed; sidecar recovery or verification is incomplete\n' >&2; fi
  exit "$rc"
}

trap 'restore_source $?' ERR
trap 'restore_source 130' INT
trap 'restore_source 143' TERM
trap 'restore_source 129' HUP

systemctl stop "${stop_order[@]}"
systemctl disable "${union_units[@]}"
persea_install_release_units "$target_release" "$unit_root"
persea_remove_installed_units "$unit_root" "${current_only[@]}"
host_new="$(dirname -- "$live_host")/.host.json.new.$$"
temporary_targets+=("$host_new")
if [[ $PERSEA_HERMETIC == 1 ]]; then
  install -m 0600 "$target_release/config/host.json" "$host_new"
else
  install -o root -g root -m 0600 "$target_release/config/host.json" "$host_new"
fi
mv -Tf -- "$host_new" "$live_host"
ln -s -- "$target" "$install_root/.current.rollback.$$"
temporary_targets+=("$install_root/.current.rollback.$$")
mv -Tf -- "$install_root/.current.rollback.$$" "$install_root/current"
ln -s -- "$current" "$install_root/.previous.rollback.$$"
temporary_targets+=("$install_root/.previous.rollback.$$")
mv -Tf -- "$install_root/.previous.rollback.$$" "$install_root/previous"
systemctl daemon-reload
"$SCRIPT_DIR/verify.sh" >/dev/null
if ((activate_local)); then
  for unit in "${target_broker_units[@]}"; do systemctl start "$unit"; done
  systemctl start persea-terminal-front.service
  systemctl enable "${target_units[@]}"
  if ((sidecar_managed)); then
    if ((sidecar_was_enabled)); then systemctl enable "$sidecar_unit"; else systemctl disable "$sidecar_unit"; fi
  fi
  if ((sidecar_was_active)); then
    start_sidecar
    "$SCRIPT_DIR/verify.sh" --require-active --require-sidecar >/dev/null
  else
    "$SCRIPT_DIR/verify.sh" --require-active >/dev/null
  fi
fi
trap - ERR INT TERM HUP
printf 'CURRENT=%s\nRECOVERABLE_RELEASE=%s\nALIAS_STORE_PRESERVED=%s\nACTIVATION_EVIDENCE_PRESERVED=%s\n' "$target" "$current" "$(persea_path "$PERSEA_STATE_ROOT/aliases.json")" "$(persea_path "$PERSEA_EVIDENCE_ROOT")"
