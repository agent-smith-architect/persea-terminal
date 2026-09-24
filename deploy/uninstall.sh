#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib.sh"

# Used by lib.sh when re-executing under the deployment lock.
# shellcheck disable=SC2034
PERSEA_DEPLOY_ARGUMENTS=("$@")
(($# == 0)) || persea_die 'uninstall.sh takes no arguments'
persea_init_root locked
persea_require_root
if [[ $PERSEA_HERMETIC == 1 ]]; then persea_require_hermetic_mocks systemctl; fi
install_root=$(persea_path "$PERSEA_INSTALL_ROOT")
unit_root=$(persea_path "$PERSEA_UNIT_ROOT")
[[ -d $install_root && ! -L $install_root ]] || persea_die 'installed root is missing or unsafe'
current_target=
for link in current previous; do
  if [[ -e $install_root/$link || -L $install_root/$link ]]; then
    [[ -L $install_root/$link ]] || persea_die "unsafe $link target"
    target=$(readlink -- "$install_root/$link")
    [[ $target == releases/* && $target != *'..'* ]] || persea_die "ambiguous $link target"
    [[ $link != current ]] || current_target=$target
  fi
done
[[ -n $current_target ]] || persea_die 'current release is missing'
current_release="$install_root/$current_target"
persea_verify_release "$current_release"
persea_read_release_inventory "$current_release" current_units
broker_units=("${current_units[@]:0:${#current_units[@]}-1}")
systemctl stop persea-terminal-front.service "${broker_units[@]}"
systemctl disable "${current_units[@]}"
for unit in "${current_units[@]}"; do
  [[ ! -e $unit_root/$unit || ( -f $unit_root/$unit && ! -L $unit_root/$unit ) ]] || persea_die "unsafe installed unit: $unit"
done
persea_remove_installed_units "$unit_root" "${current_units[@]}"
rm -f -- "$install_root/current" "$install_root/previous"
systemctl daemon-reload
# Staged operator images are ephemeral content on a 6-hour TTL; unlike the
# alias store they carry no identity worth preserving across an uninstall.
# The root is the one the current release was rendered against.
staging_root=$(persea_path "$(persea_release_staging_root "$current_release")")
staged_images_removed=0
if [[ -d $staging_root && ! -L $staging_root ]]; then
  rm -rf -- "$staging_root"
  staged_images_removed=1
fi
printf 'RELEASES_PRESERVED=%s\nALIAS_STORE_PRESERVED=%s\nACTIVATION_EVIDENCE_PRESERVED=%s\nSTAGED_IMAGES_REMOVED=%s\n' "$install_root/releases" "$(persea_path "$PERSEA_STATE_ROOT/aliases.json")" "$(persea_path "$PERSEA_EVIDENCE_ROOT")" "$staged_images_removed"
