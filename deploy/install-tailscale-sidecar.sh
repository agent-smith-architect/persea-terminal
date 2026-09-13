#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib.sh"

(($# == 0)) || persea_die 'install-tailscale-sidecar.sh takes no arguments'
persea_init_root
persea_require_root
if [[ $PERSEA_HERMETIC == 1 ]]; then persea_require_hermetic_mocks tailscale systemctl persea-process-metadata; fi
for command in tailscale systemctl install stat readlink cp mv cmp mktemp python3; do
  command -v "$command" >/dev/null || persea_die "required command is missing: $command"
done
persea_require_tailscale_cli
persea_require_tailscaled

install_root=$(persea_path "$PERSEA_INSTALL_ROOT")
unit_root=$(persea_path "$PERSEA_UNIT_ROOT")
[[ -d $install_root && ! -L $install_root && -L $install_root/current ]] || persea_die 'installed root/current is unsafe'
current_target=$(readlink -- "$install_root/current")
[[ $current_target == releases/* && $current_target != *'..'* ]] || persea_die 'current target is ambiguous'
release="$install_root/$current_target"
persea_verify_release "$release"
release_unit="$release/units/$PERSEA_SIDECAR_UNIT"
[[ -f $release_unit && ! -L $release_unit ]] || persea_die 'current immutable release lacks the sidecar unit'
persea_require_safe_directory "$(dirname -- "$unit_root")"
persea_require_safe_directory "$unit_root"
installed_unit="$unit_root/$PERSEA_SIDECAR_UNIT"

work=$(mktemp -d "${TMPDIR:-/tmp}/persea-terminal-sidecar-install.XXXXXXXX")
cleanup_work() {
  local rc=$?
  rm -f -- "$unit_root/.${PERSEA_SIDECAR_UNIT}.new.$$"
  rm -rf -- "$work"
  exit "$rc"
}
trap cleanup_work EXIT
persea_capture_main_serve "$work/main-before.json"
printf '%s\n' "$current_target" >"$work/current-before"

prior_present=0
if [[ -e $installed_unit || -L $installed_unit ]]; then
  [[ -f $installed_unit && ! -L $installed_unit ]] || persea_die 'prior sidecar unit is unsafe'
  read -r prior_owner prior_mode < <(stat -Lc '%u %a' -- "$installed_unit")
  expected_owner=$(persea_expected_root_owner)
  [[ $prior_owner == "$expected_owner" && $prior_mode == 444 ]] || persea_die 'prior sidecar unit metadata is unsafe'
  cp -- "$installed_unit" "$work/prior-unit"
  prior_present=1
fi
prior_enabled=0
prior_active=0
systemctl is-enabled --quiet "$PERSEA_SIDECAR_UNIT" 2>/dev/null && prior_enabled=1
systemctl is-active --quiet "$PERSEA_SIDECAR_UNIT" 2>/dev/null && prior_active=1

mutation_armed=0
restoration_in_progress=0
restore_sidecar() {
  local rc=$1 cleanup_rc=0 unit_new="$unit_root/.${PERSEA_SIDECAR_UNIT}.rollback.$$" restored_enabled restored_active
  trap - ERR INT TERM HUP
  if ((restoration_in_progress)); then exit "$rc"; fi
  restoration_in_progress=1
  set +e
  if ((mutation_armed)); then
    systemctl stop "$PERSEA_SIDECAR_UNIT" || cleanup_rc=1
    systemctl disable "$PERSEA_SIDECAR_UNIT" || cleanup_rc=1
    if ((prior_present)); then
      if [[ $PERSEA_HERMETIC == 1 ]]; then
        install -m 0444 "$work/prior-unit" "$unit_new" || cleanup_rc=1
      else
        install -o root -g root -m 0444 "$work/prior-unit" "$unit_new" || cleanup_rc=1
      fi
      mv -Tf -- "$unit_new" "$installed_unit" || cleanup_rc=1
    else
      rm -f -- "$installed_unit" || cleanup_rc=1
    fi
    systemctl daemon-reload || cleanup_rc=1
    ((prior_enabled == 0)) || systemctl enable "$PERSEA_SIDECAR_UNIT" || cleanup_rc=1
    ((prior_active == 0)) || systemctl start "$PERSEA_SIDECAR_UNIT" || cleanup_rc=1
    if ((prior_present)); then
      [[ -f $installed_unit && ! -L $installed_unit ]] && cmp -s "$work/prior-unit" "$installed_unit" || cleanup_rc=1
    else
      [[ ! -e $installed_unit && ! -L $installed_unit ]] || cleanup_rc=1
    fi
    restored_enabled=0
    restored_active=0
    systemctl is-enabled --quiet "$PERSEA_SIDECAR_UNIT" 2>/dev/null && restored_enabled=1
    systemctl is-active --quiet "$PERSEA_SIDECAR_UNIT" 2>/dev/null && restored_active=1
    [[ $restored_enabled == "$prior_enabled" && $restored_active == "$prior_active" ]] || cleanup_rc=1
    [[ $(readlink -- "$install_root/current" 2>/dev/null) == "$(<"$work/current-before")" ]] || cleanup_rc=1
  fi
  persea_capture_main_serve "$work/main-after-rollback.json" || cleanup_rc=1
  persea_assert_main_serve_unchanged "$work/main-before.json" "$work/main-after-rollback.json" || cleanup_rc=1
  if ((cleanup_rc)); then printf 'persea-terminal deploy: sidecar install-state restoration verification failed\n' >&2; fi
  exit "$rc"
}

mutation_armed=1
trap 'restore_sidecar $?' ERR
trap 'restore_sidecar 130' INT
trap 'restore_sidecar 143' TERM
trap 'restore_sidecar 129' HUP
unit_new="$unit_root/.${PERSEA_SIDECAR_UNIT}.new.$$"
if [[ $PERSEA_HERMETIC == 1 ]]; then
  install -m 0444 "$release_unit" "$unit_new"
else
  install -o root -g root -m 0444 "$release_unit" "$unit_new"
fi
mv -Tf -- "$unit_new" "$installed_unit"
systemctl daemon-reload
if ((prior_active)); then systemctl restart "$PERSEA_SIDECAR_UNIT"; else systemctl start "$PERSEA_SIDECAR_UNIT"; fi
systemctl enable "$PERSEA_SIDECAR_UNIT"
"$SCRIPT_DIR/verify.sh" --require-sidecar >/dev/null
[[ $(readlink -- "$install_root/current") == "$current_target" ]] || persea_die 'current release pointer changed during sidecar install'
persea_capture_main_serve "$work/main-after.json"
persea_assert_main_serve_unchanged "$work/main-before.json" "$work/main-after.json"
trap - ERR INT TERM HUP
mutation_armed=0
printf 'SIDECAR_UNIT=%s\nSIDECAR_STATE=%s\nLOGIN_REQUIRED=run %s/login-tailscale-sidecar.sh\n' \
  "$installed_unit" "$(persea_path "$PERSEA_SIDECAR_STATE_DIR")" "$SCRIPT_DIR"
