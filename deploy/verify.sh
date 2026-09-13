#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib.sh"

require_active=0
require_sidecar=0
while (($#)); do
  case $1 in
    --require-active) require_active=1; shift ;;
    --require-sidecar) require_sidecar=1; shift ;;
    *) persea_die "unknown argument: $1" ;;
  esac
done
persea_init_root
if [[ $PERSEA_HERMETIC == 1 ]]; then
  persea_require_hermetic_mocks systemctl systemd-analyze
  if ((require_active)); then
    persea_require_hermetic_mocks persea-runtime-metadata persea-process-metadata persea-unified-runtime-metadata
  fi
  if ((require_sidecar)); then persea_require_hermetic_mocks tailscale persea-process-metadata; fi
fi
if ((require_sidecar)); then persea_require_root; fi
install_root=$(persea_path "$PERSEA_INSTALL_ROOT")
unit_root=$(persea_path "$PERSEA_UNIT_ROOT")
[[ -d $install_root && ! -L $install_root && -L $install_root/current ]] || persea_die 'installed root/current is unsafe'
target=$(readlink -- "$install_root/current")
[[ $target == releases/* && $target != *'..'* ]] || persea_die 'current target is ambiguous'
release="$install_root/$target"
[[ -d $release && ! -L $release ]] || persea_die 'current release is unsafe'
persea_verify_release "$release"
cmp -s "$PERSEA_HOST_MANIFEST_PATH" "$release/config/host.json" || persea_die 'installed host manifest drift; run the guarded installer'
persea_verify_host_accounts
persea_assert_installed_units "$release" "$unit_root"
declare -a unified_realm_indexes=()
declare -a unified_runtime_dirs=()
for index in "${!PERSEA_REALM_IDS[@]}"; do
  realm_id=${PERSEA_REALM_IDS[index]}
  unit=${PERSEA_BROKER_UNITS[index]}
  config="$release/config/broker-$realm_id.json"
  mapfile -t unified_fields < <(python3 - "$config" <<'PY'
import json, sys
value = json.load(open(sys.argv[1], encoding="utf-8"))
dev = value.get("unified_terminal_dev")
if dev is not None:
    assert dev.get("enabled") is True
    print(dev["runtime_dir"])
PY
  )
  if ((${#unified_fields[@]} == 0)); then
    ! grep -q -E '^(TemporaryFileSystem=.*unified-journal|MemoryHigh=|MemoryMax=|LimitCORE=)' "$release/units/$unit" ||
      persea_die "unconfigured broker has unified retention controls: $unit"
    continue
  fi
  ((${#unified_fields[@]} == 1)) || persea_die "ambiguous unified runtime config: $unit"
  runtime_dir=${unified_fields[0]}
  [[ $runtime_dir == "/run/persea-terminal-$realm_id/unified-journal" ]] ||
    persea_die "unified runtime path is not derived from its realm: $unit"
  grep -Fxq "TemporaryFileSystem=$runtime_dir:rw,nosuid,nodev,noexec,noswap,size=96M,nr_inodes=256,mode=0700,uid=${PERSEA_REALM_UIDS[index]},gid=$PERSEA_GROUP_GID" "$release/units/$unit" ||
    persea_die "unified journal filesystem controls drift: $unit"
  grep -Fxq 'MemoryHigh=850M' "$release/units/$unit" || persea_die "unified broker MemoryHigh drift: $unit"
  grep -Fxq 'MemoryMax=1G' "$release/units/$unit" || persea_die "unified broker MemoryMax drift: $unit"
  grep -Fxq 'LimitCORE=0' "$release/units/$unit" || persea_die "unified broker core limit drift: $unit"
  unified_realm_indexes+=("$index")
  unified_runtime_dirs+=("$runtime_dir")
done
# Image staging coherence: a staging-enabled broker config and its unit's
# ReadWritePaths must agree on the realm directory under the release's
# staging root (front.staging_root override or the default), because a fresh
# deploy is only sufficient when both were rendered together. Existence of the
# directories themselves is enforced fail-closed at runtime: a missing
# ReadWritePaths target fails broker unit start, and the broker's startup
# probe refuses a directory with the wrong owner or mode.
release_staging_root=$(persea_release_staging_root "$release")
for index in "${!PERSEA_REALM_IDS[@]}"; do
  realm_id=${PERSEA_REALM_IDS[index]}
  unit=${PERSEA_BROKER_UNITS[index]}
  config="$release/config/broker-$realm_id.json"
  mapfile -t staging_fields < <(python3 - "$config" <<'PY'
import json, sys
value = json.load(open(sys.argv[1], encoding="utf-8"))
staging = value.get("image_staging_dir")
if staging is not None:
    print(staging)
PY
  )
  if ((${#staging_fields[@]} == 0)); then
    ! grep -q -E '^ReadWritePaths=' "$release/units/$unit" ||
      persea_die "unconfigured broker opens ReadWritePaths: $unit"
    continue
  fi
  ((${#staging_fields[@]} == 1)) || persea_die "ambiguous image staging config: $unit"
  staging_dir=${staging_fields[0]}
  [[ $staging_dir == "$release_staging_root/$realm_id" ]] ||
    persea_die "image staging path is not derived from its realm: $unit"
  grep -Fxq "ReadWritePaths=$staging_dir" "$release/units/$unit" ||
    persea_die "image staging ReadWritePaths drift: $unit"
done
# The front never touches staged files; brokers own them. The front's state
# directory keeps the alias store's exact-0700 invariant regardless of image
# staging, and the front unit must carry no image directives at all — a 0711
# rendering here is the 2026-08-20 activation failure.
grep -Fxq 'StateDirectoryMode=0700' "$release/units/persea-terminal-front.service" ||
  persea_die 'front StateDirectoryMode must stay exactly 0700'
! grep -qF -- "$release_staging_root" "$release/units/persea-terminal-front.service" ||
  persea_die 'front unit must not reference the image staging root'
# The preferences and snippet stores (optional in older releases) must sit
# beside the alias store inside that same 0700 state directory.
persea_verify_front_store_paths "$release" "$release_staging_root"
if ((require_active && ${#unified_realm_indexes[@]} > 0)) && [[ $PERSEA_HERMETIC != 1 ]]; then
  persea_require_root
fi
for unit in "${PERSEA_UNITS[@]}"; do
  ! grep -n -E '^PrivateTmp=' "$release/units/$unit" >/dev/null || persea_die 'broker/front units must not use PrivateTmp'
  grep -Fxq 'RestrictAddressFamilies=AF_UNIX' "$release/units/$unit" || persea_die "application unit is not AF_UNIX-only: $unit"
done
if [[ $PERSEA_HERMETIC == 1 ]]; then
  systemd-analyze verify "$release"/units/*.service >/dev/null
else
  "$SCRIPT_DIR/tests/systemd-unit-test.sh" >/dev/null
fi
if ((require_active)); then
  binary_identity=$(stat -Lc '%d:%i' -- "$release/bin/persea-terminal")
  declare -A expected_uid=()
  for index in "${!PERSEA_BROKER_UNITS[@]}"; do
    expected_uid[${PERSEA_BROKER_UNITS[index]}]=${PERSEA_REALM_UIDS[index]}
  done
  expected_uid[persea-terminal-front.service]=$PERSEA_FRONT_UID
  for unit in "${PERSEA_UNITS[@]}"; do
    systemctl is-active --quiet "$unit" || persea_die "inactive unit: $unit"
    fragment=$(systemctl show "$unit" --property=FragmentPath --value)
    [[ $fragment == "$unit_root/$unit" ]] || persea_die "active unit fragment drift: $unit"
    main_pid=$(systemctl show "$unit" --property=MainPID --value)
    [[ $main_pid =~ ^[1-9][0-9]*$ ]] || persea_die "active unit has no MainPID: $unit"
    if [[ $PERSEA_HERMETIC == 1 ]]; then
      process_metadata=$(persea-process-metadata "$unit" "$main_pid" "$release/bin/persea-terminal")
    else
      [[ -d /proc/$main_pid ]] || persea_die "MainPID disappeared: $unit"
      process_metadata="$(stat -Lc '%u' -- "/proc/$main_pid"):$(stat -Lc '%d:%i' -- "/proc/$main_pid/exe")"
    fi
    accepted_identity=0
    [[ $process_metadata == "${expected_uid[$unit]}:$binary_identity" ]] && accepted_identity=1
    if ((accepted_identity == 0)) && [[ $unit == persea-terminal-broker-*.service ]]; then
      realm_id=${unit#persea-terminal-broker-}; realm_id=${realm_id%.service}
      for candidate in "$install_root"/releases/*; do
        [[ -d $candidate && ! -L $candidate && -f $candidate/bin/persea-terminal && ! -L $candidate/bin/persea-terminal ]] || continue
        cmp -s "$candidate/bin/persea-terminal" "$release/bin/persea-terminal" || continue
        cmp -s "$candidate/units/$unit" "$release/units/$unit" || continue
        cmp -s "$candidate/config/broker-$realm_id.json" "$release/config/broker-$realm_id.json" || continue
        read -r candidate_owner candidate_mode < <(stat -Lc '%u %a' -- "$candidate/bin/persea-terminal")
        [[ $candidate_owner == $(persea_expected_root_owner) && $candidate_mode == 555 ]] || continue
        candidate_identity=$(stat -Lc '%d:%i' -- "$candidate/bin/persea-terminal")
        if [[ $process_metadata == "${expected_uid[$unit]}:$candidate_identity" ]]; then
          accepted_identity=1
          break
        fi
      done
    fi
    ((accepted_identity)) || persea_die "MainPID UID/executable drift: $unit"
  done

  runtime_boundaries=(
    "dir:$(persea_path /run/persea-terminal):$PERSEA_FRONT_UID:$PERSEA_GROUP_GID:700"
    "socket:$(persea_path "$PERSEA_FRONT_SOCKET"):$PERSEA_FRONT_UID:$PERSEA_GROUP_GID:600"
  )
  for index in "${!PERSEA_REALM_IDS[@]}"; do
    runtime_mode=2710
    socket_mode=660
    if [[ ${PERSEA_REALM_USERS[index]} == "$PERSEA_FRONT_USER" && ${PERSEA_REALM_UIDS[index]} == "$PERSEA_FRONT_UID" ]]; then
      runtime_mode=700
      socket_mode=600
    fi
    runtime_boundaries+=(
      "dir:$(persea_path "/run/persea-terminal-${PERSEA_REALM_IDS[index]}"):${PERSEA_REALM_UIDS[index]}:$PERSEA_GROUP_GID:$runtime_mode"
      "socket:$(persea_path "${PERSEA_BROKER_SOCKETS[index]}"):${PERSEA_REALM_UIDS[index]}:$PERSEA_GROUP_GID:$socket_mode"
    )
  done
  for boundary in "${runtime_boundaries[@]}"; do
    IFS=: read -r kind path expected_runtime_uid expected_runtime_gid expected_runtime_mode <<<"$boundary"
    if [[ $kind == dir ]]; then
      [[ -d $path && ! -L $path ]] || persea_die "runtime directory is missing or unsafe: $path"
    else
      [[ -S $path && ! -L $path ]] || persea_die "runtime socket is missing or unsafe: $path"
    fi
    if [[ $PERSEA_HERMETIC == 1 ]]; then
      runtime_metadata=$(persea-runtime-metadata "$path")
    else
      runtime_metadata=$(stat -Lc '%u:%g:%a' -- "$path")
    fi
    [[ $runtime_metadata == "$expected_runtime_uid:$expected_runtime_gid:$expected_runtime_mode" ]] || persea_die "runtime boundary metadata drift: $path"
  done
  for unified_index in "${!unified_realm_indexes[@]}"; do
    index=${unified_realm_indexes[unified_index]}
    runtime_dir=${unified_runtime_dirs[unified_index]}
    unit=${PERSEA_BROKER_UNITS[index]}
    main_pid=$(systemctl show "$unit" --property=MainPID --value)
    [[ $(systemctl show "$unit" --property=MemoryHigh --value) == 891289600 ]] || persea_die "active MemoryHigh drift: $unit"
    [[ $(systemctl show "$unit" --property=MemoryMax --value) == 1073741824 ]] || persea_die "active MemoryMax drift: $unit"
    [[ $(systemctl show "$unit" --property=LimitCORE --value) == 0 ]] || persea_die "active hard core limit drift: $unit"
    if [[ $PERSEA_HERMETIC == 1 ]]; then
      unified_metadata=$(persea-unified-runtime-metadata "$unit" "$main_pid" "$runtime_dir")
    else
      core_limits=$(awk '$1 == "Max" && $2 == "core" && $3 == "file" && $4 == "size" {print $5 ":" $6}' "/proc/$main_pid/limits")
      [[ $core_limits == 0:0 ]] || persea_die "active process core limits drift: $unit"
      mount_record=$(nsenter --target "$main_pid" --mount -- findmnt --noheadings --target "$runtime_dir" --output FSTYPE,OPTIONS)
      read -r mount_type mount_options <<<"$mount_record"
      owner_mode=$(nsenter --target "$main_pid" --mount -- stat -Lc '%u:%g:%a' -- "$runtime_dir")
      read -r block_size block_count inode_count < <(nsenter --target "$main_pid" --mount -- stat -fc '%S %b %c' -- "$runtime_dir")
      unified_metadata="0:0:$mount_type:$owner_mode:$((block_size * block_count)):$inode_count:$mount_options"
    fi
    IFS=: read -r core_soft core_hard mount_type mount_uid mount_gid mount_mode mount_bytes mount_inodes mount_options <<<"$unified_metadata"
    [[ $core_soft == 0 && $core_hard == 0 ]] || persea_die "active process core limits drift: $unit"
    [[ $mount_type == tmpfs && $mount_uid == "${PERSEA_REALM_UIDS[index]}" && $mount_gid == "$PERSEA_GROUP_GID" && $mount_mode == 700 ]] ||
      persea_die "unified journal mount identity drift: $unit"
    [[ $mount_bytes == 100663296 && $mount_inodes == 256 ]] || persea_die "unified journal mount capacity drift: $unit"
    for option in rw nosuid nodev noexec noswap size=98304k nr_inodes=256 mode=700 "uid=${PERSEA_REALM_UIDS[index]}" "gid=$PERSEA_GROUP_GID"; do
      [[ ,$mount_options, == *,$option,* ]] || persea_die "unified journal mount option drift ($option): $unit"
    done
  done
fi
if ((require_sidecar)); then
  sidecar_unit="$release/units/$PERSEA_SIDECAR_UNIT"
  installed_sidecar="$unit_root/$PERSEA_SIDECAR_UNIT"
  [[ -f $sidecar_unit && ! -L $sidecar_unit ]] || persea_die 'current release lacks the sidecar unit'
  [[ -f $installed_sidecar && ! -L $installed_sidecar ]] || persea_die 'installed sidecar unit is missing or unsafe'
  cmp -s "$sidecar_unit" "$installed_sidecar" || persea_die 'installed sidecar unit drift'
  read -r sidecar_owner sidecar_mode < <(stat -Lc '%u %a' -- "$installed_sidecar")
  expected_owner=0; [[ $PERSEA_HERMETIC == 1 ]] && expected_owner=$(id -u)
  [[ $sidecar_owner == "$expected_owner" && $sidecar_mode == 444 ]] || persea_die 'installed sidecar unit metadata drift'
  persea_require_tailscale_cli
  persea_require_tailscaled
  work=$(mktemp -d "${TMPDIR:-/tmp}/persea-terminal-verify-sidecar.XXXXXXXX")
  cleanup_verify_sidecar() { rm -rf -- "$work"; }
  trap cleanup_verify_sidecar EXIT
  persea_capture_main_serve "$work/main-before.json"
  persea_assert_sidecar_runtime
  persea_sidecar_tailscale status --json >"$work/sidecar-status.json"
  python3 - "$work/sidecar-status.json" <<'PY'
import json, sys
status = json.load(open(sys.argv[1], encoding="utf-8"))
assert isinstance(status, dict), "sidecar status is not a JSON object"
state = status.get("BackendState")
assert state in ("NeedsLogin", "Running", "Starting"), f"unexpected sidecar backend state: {state}"
PY
  persea_capture_main_serve "$work/main-after.json"
  persea_assert_main_serve_unchanged "$work/main-before.json" "$work/main-after.json"
  cleanup_verify_sidecar
  trap - EXIT
fi
printf 'VERIFIED_RELEASE=%s\nACTIVE_REQUIRED=%s\nSIDECAR_REQUIRED=%s\n' "$release" "$require_active" "$require_sidecar"
