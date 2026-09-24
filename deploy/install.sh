#!/usr/bin/env bash
# Inner Bash processes intentionally expand their own positional parameters.
# shellcheck disable=SC2016
set -Eeuo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
PROJECT_DIR=$(cd -- "$SCRIPT_DIR/.." && pwd -P)
REPO_ROOT=$(git -C "$PROJECT_DIR" rev-parse --show-toplevel)
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib.sh"
[[ $REPO_ROOT == "$PROJECT_DIR" ]] || persea_die 'installer must run from the standalone repository root'

activate_local=0
keep_releases=${PERSEA_KEEP_RELEASES:-5}
while (($#)); do
  case $1 in
    --activate-local) activate_local=1; shift ;;
    --keep-releases) [[ $# -ge 2 ]] || persea_die 'missing --keep-releases value'; keep_releases=$2; shift 2 ;;
    --no-prune) keep_releases=0; shift ;;
    *) persea_die "unknown argument: $1" ;;
  esac
done
persea_validate_keep_releases "$keep_releases"

persea_init_root locked public
persea_require_root
new_units=("${PERSEA_UNITS[@]}")
new_broker_units=("${PERSEA_BROKER_UNITS[@]}")
if [[ $PERSEA_HERMETIC == 1 ]]; then
  persea_require_hermetic_mocks tailscale systemd-analyze systemctl
fi

required=(git tar clang install stat readlink ln mv mkdir cp sha256sum id getent tailscale systemctl systemd-analyze python3)
for command in "${required[@]}"; do
  command -v "$command" >/dev/null || persea_die "required command is missing: $command"
done
[[ $PERSEA_HERMETIC == 1 || -x /usr/sbin/runuser ]] || persea_die 'required command is missing: /usr/sbin/runuser'

head=$(git -C "$REPO_ROOT" rev-parse HEAD)
clean=$(git -C "$REPO_ROOT" status --porcelain --untracked-files=all)
if [[ $PERSEA_HERMETIC == 1 ]]; then
  head=${PERSEA_TEST_HEAD:-$head}
  [[ ${PERSEA_TEST_CLEAN:-0} == 1 ]] && clean=
fi
[[ $head =~ ^[0-9a-f]{40}$ ]] || persea_die 'invalid source commit'
[[ -z $clean ]] || persea_die 'source worktree is dirty or uncommitted'

persea_verify_host_accounts
build_gid=$(id -g "$PERSEA_FRONT_USER")

validate_build_tool() {
  local label=$1 candidate=$2 allowed_root=${3:-} canonical cursor mode
  [[ $candidate == /* ]] || persea_die "$label tool candidate is not absolute: $candidate"
  [[ $candidate != "$REPO_ROOT" && $candidate != "$REPO_ROOT/"* ]] || persea_die "$label tool candidate is repository-resident: $candidate"
  [[ -f $candidate && -x $candidate ]] || persea_die "$label tool candidate is missing or unexecutable: $candidate"
  canonical=$(readlink -f -- "$candidate") || persea_die "cannot resolve $label tool candidate: $candidate"
  if [[ -n $allowed_root ]]; then
    [[ $canonical == "$allowed_root/"* ]] || persea_die "$label tool candidate escapes its fixed prefix: $candidate -> $canonical"
  fi
  [[ $canonical != "$REPO_ROOT" && $canonical != "$REPO_ROOT/"* ]] || persea_die "$label tool candidate resolves into the repository: $candidate"
  [[ -f $canonical && -x $canonical ]] || persea_die "$label tool target is missing or unexecutable: $canonical"
  if [[ -z $allowed_root ]]; then
    for cursor in "$(dirname -- "$candidate")" "$(dirname -- "$canonical")"; do
      while :; do
        [[ -d $cursor && ! -L $cursor ]] || persea_die "$label tool ancestor is unsafe: $cursor"
        mode=$(stat -c '%a' -- "$cursor") || persea_die "cannot stat $label tool ancestor: $cursor"
        (( (8#$mode & 0022) == 0 )) || persea_die "$label tool ancestor is group/other-writable: $cursor"
        [[ $cursor == / ]] && break
        cursor=$(dirname -- "$cursor")
      done
    done
  fi
  if [[ $PERSEA_HERMETIC != 1 ]]; then
    /usr/sbin/runuser -u "$PERSEA_FRONT_USER" -- /usr/bin/test -x "$candidate" ||
      persea_die "$label tool candidate is not executable by UID $PERSEA_FRONT_UID: $candidate"
    /usr/sbin/runuser -u "$PERSEA_FRONT_USER" -- /usr/bin/test -x "$canonical" ||
      persea_die "$label tool target is not executable by UID $PERSEA_FRONT_UID: $canonical"
  fi
  printf '%s\n' "$candidate"
}

if [[ $PERSEA_HERMETIC == 1 ]]; then
  toolchain_root=${PERSEA_TEST_TOOLCHAIN_ROOT:-$PERSEA_DEPLOY_MOCK_BIN}
  [[ $toolchain_root == /* && -d $toolchain_root && ! -L $toolchain_root ]] || persea_die 'hermetic toolchain root is unsafe'
  npm_tool=$(validate_build_tool npm "${PERSEA_TEST_NPM_CANDIDATE:-$toolchain_root/npm}" "$toolchain_root")
  node_tool=$(validate_build_tool node "${PERSEA_TEST_NODE_CANDIDATE:-$toolchain_root/node}" "$toolchain_root")
  go_tool=$(validate_build_tool go "${PERSEA_TEST_GO_CANDIDATE:-$toolchain_root/go}" "$toolchain_root")
else
  npm_candidate=${PERSEA_NPM_BIN:-$(command -v npm || true)}
  node_candidate=${PERSEA_NODE_BIN:-$(command -v node || true)}
  go_candidate=${PERSEA_GO_BIN:-$(command -v go || true)}
  npm_tool=$(validate_build_tool npm "$npm_candidate")
  node_tool=$(validate_build_tool node "$node_candidate")
  go_tool=$(validate_build_tool go "$go_candidate")
fi

persea_require_tailscale_cli

install_root=$(persea_path "$PERSEA_INSTALL_ROOT")
release_root="$install_root/releases"
unit_root=$(persea_path "$PERSEA_UNIT_ROOT")
persea_require_safe_directory "$(dirname -- "$install_root")"
persea_require_safe_directory "$install_root" 1
if [[ -e $install_root/current || -L $install_root/current ]]; then
  [[ -L $install_root/current ]] || persea_die 'unsafe pre-existing current target'
  old_target=$(readlink -- "$install_root/current")
  [[ $old_target == releases/* && $old_target != *'..'* && -d $install_root/$old_target && ! -L $install_root/$old_target ]] || persea_die 'unsafe current symlink target'
else
  old_target=
fi
old_previous_target=
old_previous_present=0
if [[ -e $install_root/previous || -L $install_root/previous ]]; then
  [[ -L $install_root/previous ]] || persea_die 'unsafe pre-existing previous target'
  old_previous_target=$(readlink -- "$install_root/previous")
  [[ $old_previous_target == releases/* && $old_previous_target != *'..'* && -d $install_root/$old_previous_target && ! -L $install_root/$old_previous_target ]] || persea_die 'unsafe previous symlink target'
  old_previous_present=1
fi
persea_require_safe_directory "$(dirname -- "$unit_root")"
if [[ -n $old_target ]]; then
  old_release="$install_root/$old_target"
  persea_require_public_release "$old_release"
  persea_verify_release "$old_release"
  persea_assert_installed_units "$old_release" "$unit_root"
  persea_read_release_inventory "$old_release" old_units
else
  old_units=()
  ((old_previous_present == 0)) || persea_die 'previous release exists without a current package'
  persea_assert_no_installed_units "$unit_root"
fi

declare -A transition_seen=()
transition_units=()
for unit in "${old_units[@]}" "${new_units[@]}"; do
  [[ -n ${transition_seen[$unit]+x} ]] && continue
  transition_seen[$unit]=1
  transition_units+=("$unit")
done

was_enabled=()
was_active=()
declare -A was_active_set=()
for unit in "${transition_units[@]}"; do
  if systemctl is-enabled --quiet "$unit" 2>/dev/null; then was_enabled+=("$unit"); fi
  if systemctl is-active --quiet "$unit" 2>/dev/null; then was_active+=("$unit"); was_active_set[$unit]=1; fi
done
if ((activate_local == 0 && (${#was_enabled[@]} > 0 || ${#was_active[@]} > 0))); then
  persea_die 'managed units are active or enabled; rerun with --activate-local'
fi

build_root=$(mktemp -d /tmp/pt.XXXXXXXX)
release_new=
temporary_targets=()
cleanup_build() {
  local rc=$1 target
  if [[ -n $release_new && $release_new == "$release_root"/.*.new."$$" && -d $release_new && ! -L $release_new ]]; then
    chmod -R u+w -- "$release_new" 2>/dev/null || true
    rm -rf -- "$release_new"
  fi
  for target in "${temporary_targets[@]}"; do
    [[ $target == "$install_root"/.* || $target == "$unit_root"/.* ]] && rm -f -- "$target"
  done
  # Revoke mode 0511 can still be active when a build command fails. Restore
  # write permission only on the root-owned parent; never chmod recursively
  # across builder-controlled entries or their possible out-of-tree hardlinks.
  chmod 0700 -- "$build_root" 2>/dev/null || rc=1
  rm -rf -- "$build_root" || rc=1
  exit "$rc"
}
trap 'cleanup_build $?' EXIT
build_workspace="$build_root/w"
src="$build_workspace/src"
build_output="$build_workspace/output"
build_binary="$build_output/persea-terminal"
build_tmp="$build_workspace/t"
# Go's longest frozen test socket suffix includes the sanitized subtest name,
# a maximal ten-digit temporary-directory discriminator, and /001/front.sock.
test_socket_suffix='/TestIngressSocketStateAndCleanupFailClosedreplacement_cleanup4294967295/001/front.sock'
test_socket_path_bytes=$((${#build_tmp} + ${#test_socket_suffix}))
((test_socket_path_bytes <= 107)) || persea_die "build TMPDIR exceeds Unix socket path budget: $test_socket_path_bytes > 107"
mkdir -m 0700 -- "$build_workspace" "$src" "$build_workspace/home" "$build_tmp" "$build_workspace/cache" "$build_output"
git -C "$REPO_ROOT" archive HEAD | tar -x -C "$src"
if [[ $PERSEA_HERMETIC != 1 ]]; then
  chown -R "$PERSEA_FRONT_UID:$build_gid" -- "$build_workspace"
fi
chmod 0511 -- "$build_root"

build_path="$(dirname -- "$node_tool"):$(dirname -- "$npm_tool"):$(dirname -- "$go_tool"):/usr/bin:/bin"
run_build() {
  local -a build_env=(
    "PATH=$build_path"
    "LANG=C.UTF-8"
    "LC_ALL=C.UTF-8"
    "HOME=$build_workspace/home"
    "TMPDIR=$build_tmp"
    "XDG_CACHE_HOME=$build_workspace/cache"
    "NPM_CONFIG_CACHE=$build_workspace/cache/npm"
    "PLAYWRIGHT_BROWSERS_PATH=$build_workspace/cache/playwright"
    "GOCACHE=$build_workspace/cache/go-build"
    "GOMODCACHE=$build_workspace/cache/go-mod"
    "PERSEA_EXPECT_BUILD_UID=$PERSEA_FRONT_UID"
  )
  if [[ $PERSEA_HERMETIC == 1 ]]; then
    build_env+=("FAKE_STATE=${FAKE_STATE:-}" "FAKE_STAGE_PLANT=${FAKE_STAGE_PLANT:-0}")
    /usr/bin/env -i "${build_env[@]}" /bin/bash -c '
      real=$(/usr/bin/id -ru)
      effective=$(/usr/bin/id -u)
      if [[ $real != "$PERSEA_EXPECT_BUILD_UID" || $effective != "$PERSEA_EXPECT_BUILD_UID" ]]; then
        printf "persea-terminal deploy: build UID witness: real=%s effective=%s expected=%s\n" "$real" "$effective" "$PERSEA_EXPECT_BUILD_UID" >&2
        exit 97
      fi
      exec "$@"
    ' persea-build "$@"
  else
    /usr/sbin/runuser -u "$PERSEA_FRONT_USER" -- /usr/bin/env -i "${build_env[@]}" /bin/bash -c '
      real=$(/usr/bin/id -ru)
      effective=$(/usr/bin/id -u)
      if [[ $real != "$PERSEA_EXPECT_BUILD_UID" || $effective != "$PERSEA_EXPECT_BUILD_UID" ]]; then
        printf "persea-terminal deploy: build UID witness: real=%s effective=%s expected=%s\n" "$real" "$effective" "$PERSEA_EXPECT_BUILD_UID" >&2
        exit 97
      fi
      exec "$@"
    ' persea-build "$@"
  fi
}

run_build /bin/bash -c 'cd "$1/ui" && "$2" ci && "$2" exec -- playwright install chromium && "$2" run typecheck && "$2" test && "$2" run build' persea-ui "$src" "$npm_tool"
run_build /bin/bash -c 'cd "$1" && "$2" test ./...' persea-go-test "$src" "$go_tool"
run_build /bin/bash -c 'cd "$1" && CGO_ENABLED=1 CC=/usr/bin/clang "$2" test -race -timeout 30m ./internal/... ./cmd/...' persea-go-race "$src" "$go_tool"
run_build /bin/bash -c 'cd "$1" && CGO_ENABLED=1 CC=/usr/bin/clang "$2" build -trimpath -o "$3" ./cmd/persea-terminal' persea-go-build "$src" "$go_tool" "$build_binary"

# Revoke the build UID's pathname access before root validates or imports artifacts.
chmod 0700 -- "$build_root"
new_release_ui_assets=(index.html app.js app.css xterm.css app.js.gz app.css.gz xterm.css.gz THIRD_PARTY_NOTICES.txt manifest.webmanifest icon-192.png icon-512.png apple-touch-icon.png)
for asset in "${new_release_ui_assets[@]}"; do
  [[ -f $src/ui/dist/$asset && ! -L $src/ui/dist/$asset ]] || persea_die "fresh UI build is missing regular $asset"
done
[[ -f $build_binary && ! -L $build_binary ]] || persea_die 'fresh Go build is missing a regular binary'
[[ $(stat -Lc '%h' -- "$build_binary") == 1 ]] || persea_die 'fresh Go binary has an unsafe link count'

# The final stage did not exist while the configured build UID could traverse the build root.
stage="$build_root/stage"
[[ ! -e $stage && ! -L $stage ]] || persea_die 'root stage target already exists'
mkdir -m 0700 -- "$stage" "$stage/bin" "$stage/ui" "$stage/libexec" "$stage/config" "$stage/units"
cp -- "$build_binary" "$stage/bin/persea-terminal"
for asset in "${new_release_ui_assets[@]}"; do
  cp -- "$src/ui/dist/$asset" "$stage/ui/$asset"
done
cp -- "$SCRIPT_DIR/probe-unix.py" "$stage/$PERSEA_PROBE_HELPER"
cp -- "$SCRIPT_DIR/host-config.py" "$stage/libexec/host-config.py"
rendered="$build_root/rendered-host"
persea_generate_candidate "$rendered" "$PERSEA_HOST_MANIFEST_PATH"
cp -a -- "$rendered/config/." "$stage/config/"
cp -a -- "$rendered/units/." "$stage/units/"

finalize_release_stage() {
  local source_stage=$1
  chmod 0555 "$source_stage/bin/persea-terminal"
  chmod 0444 "$source_stage/ui"/* "$source_stage/config"/* "$source_stage/units"/* "$source_stage/libexec"/*
  (cd "$source_stage" && while IFS= read -r path; do sha256sum "$path"; done < <(find bin libexec ui config units -type f -printf '%p\n' | LC_ALL=C sort) >MANIFEST.sha256)
  chmod 0444 "$source_stage/MANIFEST.sha256"
}

import_release_stage() {
  local source_stage=$1 release_name=$2 output_name=$3 target
  target="$release_root/$release_name"
  if [[ -e $target ]]; then
    [[ -d $target && ! -L $target ]] || persea_die 'release target is unsafe'
    (cd "$target" && sha256sum -c MANIFEST.sha256 >/dev/null) || persea_die 'existing release manifest mismatch'
    cmp -s "$source_stage/MANIFEST.sha256" "$target/MANIFEST.sha256" || persea_die 'existing release content disagrees with its immutable identity'
    persea_verify_release_metadata "$target"
  else
    if [[ ! -e $install_root ]]; then
      if [[ $PERSEA_HERMETIC == 1 ]]; then install -d -m 0755 "$install_root"; else install -d -o root -g root -m 0755 "$install_root"; fi
    fi
    persea_require_safe_directory "$install_root"
    if [[ ! -e $release_root ]]; then
      if [[ $PERSEA_HERMETIC == 1 ]]; then install -d -m 0755 "$release_root"; else install -d -o root -g root -m 0755 "$release_root"; fi
    fi
    persea_require_safe_directory "$release_root"
    release_new="$release_root/.${release_name}.new.$$"
    [[ ! -e $release_new ]] || persea_die 'release staging target already exists'
    mkdir -m 0700 -- "$release_new"
    cp -a -- "$source_stage/." "$release_new/"
    if [[ $PERSEA_HERMETIC != 1 ]]; then chown -R root:root -- "$release_new"; fi
    find "$release_new" -type d -exec chmod 0555 {} +
    chmod 0555 "$release_new/bin/persea-terminal"
    find "$release_new" -type f ! -path "$release_new/bin/persea-terminal" -exec chmod 0444 {} +
    mv -T -- "$release_new" "$target"
    release_new=
    persea_verify_release_metadata "$target"
  fi
  printf -v "$output_name" '%s' "$target"
}

finalize_release_stage "$stage"

host_hash=${PERSEA_HOST_MANIFEST_SHA256:0:16}
release_id="$head-$host_hash"
release_dir=
import_release_stage "$stage" "$release_id" release_dir

declare -A old_unit_set=() new_unit_set=()
for unit in "${old_units[@]}"; do old_unit_set[$unit]=1; done
for unit in "${new_units[@]}"; do new_unit_set[$unit]=1; done
removed_units=()
for unit in "${old_units[@]}"; do
  [[ -n ${new_unit_set[$unit]+x} ]] || removed_units+=("$unit")
done
changed_broker_units=()
unchanged_broker_units=()
for unit in "${new_broker_units[@]}"; do
  realm_id=${unit#persea-terminal-broker-}; realm_id=${realm_id%.service}
  if [[ -n $old_target && -n ${old_unit_set[$unit]+x} ]] &&
    cmp -s "$install_root/$old_target/bin/persea-terminal" "$release_dir/bin/persea-terminal" &&
    cmp -s "$install_root/$old_target/units/$unit" "$release_dir/units/$unit" &&
    cmp -s "$install_root/$old_target/config/broker-$realm_id.json" "$release_dir/config/broker-$realm_id.json"; then
    unchanged_broker_units+=("$unit")
  else
    changed_broker_units+=("$unit")
  fi
done
if [[ ! -e $unit_root ]]; then
  if [[ $PERSEA_HERMETIC == 1 ]]; then install -d -m 0755 "$unit_root"; else install -d -o root -g root -m 0755 "$unit_root"; fi
fi
persea_require_safe_directory "$unit_root"
transition_brokers=()
new_only_units=()
for unit in "${transition_units[@]}"; do
  [[ $unit == persea-terminal-front.service ]] || transition_brokers+=("$unit")
done
for unit in "${new_units[@]}"; do
  [[ -n ${old_unit_set[$unit]+x} ]] || new_only_units+=("$unit")
done
restore_stop_order=(persea-terminal-front.service "${transition_brokers[@]}")
mutation_armed=0
restoration_in_progress=0
restore_install_state() {
  local rc=$1 cleanup_rc=0
  trap - ERR INT TERM HUP
  if ((restoration_in_progress)); then exit "$rc"; fi
  restoration_in_progress=1
  set +e
  if ((mutation_armed)); then
    if ((activate_local)); then
      systemctl stop "${restore_stop_order[@]}" || cleanup_rc=1
      systemctl disable "${transition_units[@]}" || cleanup_rc=1
    fi
    if [[ -n $old_target ]]; then
      persea_install_release_units "$install_root/$old_target" "$unit_root" || cleanup_rc=1
      persea_remove_installed_units "$unit_root" "${new_only_units[@]}" || cleanup_rc=1
    else
      persea_remove_installed_units "$unit_root" "${transition_units[@]}" || cleanup_rc=1
    fi
    if [[ -n $old_target ]]; then
      ln -s -- "$old_target" "$install_root/.current.rollback.$$" &&
        mv -Tf -- "$install_root/.current.rollback.$$" "$install_root/current" || cleanup_rc=1
    else
      rm -f -- "$install_root/current" || cleanup_rc=1
    fi
    if ((old_previous_present)); then
      ln -s -- "$old_previous_target" "$install_root/.previous.rollback.$$" &&
        mv -Tf -- "$install_root/.previous.rollback.$$" "$install_root/previous" || cleanup_rc=1
    else
      rm -f -- "$install_root/previous" || cleanup_rc=1
    fi
    systemctl daemon-reload || cleanup_rc=1
    if ((activate_local)); then
      ((${#was_enabled[@]} == 0)) || systemctl enable "${was_enabled[@]}" || cleanup_rc=1
      for unit in "${old_units[@]}"; do
        [[ -n ${was_active_set[$unit]+x} ]] || continue
        systemctl start "$unit" || cleanup_rc=1
      done
    fi
    if [[ -n $old_target ]]; then
      persea_assert_installed_units "$install_root/$old_target" "$unit_root" || cleanup_rc=1
    else
      persea_assert_no_installed_units "$unit_root" || cleanup_rc=1
    fi
  fi
  if ((cleanup_rc)); then printf 'persea-terminal deploy: install-state restoration verification failed\n' >&2; fi
  exit "$rc"
}

trap 'restore_install_state $?' ERR
trap 'restore_install_state 130' INT
trap 'restore_install_state 143' TERM
trap 'restore_install_state 129' HUP
mutation_armed=1

# Image staging directories. The staging-enabled broker units carry a plain
# ReadWritePaths= on the realm directory, so the directories must exist before
# any such unit starts — a missing one fails unit start (fail-closed), never
# silently disables staging. The root comes from the release itself
# (front.staging_root override or the default), its own tree outside the
# front's state directory (which the alias store requires to stay exactly
# 0700), root-owned and traversal-only (0711); each realm directory is
# realm-owned 0700, which the broker's own startup probe then re-asserts.
# Idempotent: install -d leaves an existing directory's contents alone, and a
# repeat run only re-applies owner and mode.
staging_root=$(persea_path "$(persea_release_staging_root "$release_dir")")
for index in "${!PERSEA_REALM_IDS[@]}"; do
  realm_id=${PERSEA_REALM_IDS[index]}
  grep -q '"image_staging_dir"' "$release_dir/config/broker-$realm_id.json" || continue
  if [[ ! -d $staging_root ]]; then
    if [[ $PERSEA_HERMETIC == 1 ]]; then install -d -m 0711 "$staging_root"; else install -d -o root -g root -m 0711 "$staging_root"; fi
  fi
  persea_require_safe_directory "$staging_root"
  if [[ $PERSEA_HERMETIC == 1 ]]; then
    install -d -m 0700 "$staging_root/$realm_id"
  else
    install -d -o "${PERSEA_REALM_USERS[index]}" -g "$PERSEA_GROUP" -m 0700 "$staging_root/$realm_id"
  fi
done

if ((activate_local && ${#removed_units[@]} > 0)); then
  systemctl stop persea-terminal-front.service
  systemctl stop "${removed_units[@]}"
  systemctl disable "${removed_units[@]}"
fi
persea_install_release_units "$release_dir" "$unit_root"
persea_remove_installed_units "$unit_root" "${removed_units[@]}"

new_target="releases/$release_id"
current_new="$install_root/.current.new.$$"
temporary_targets+=("$current_new")
ln -s -- "$new_target" "$current_new"
if [[ -n $old_target && $old_target != "$new_target" ]]; then
  previous_new="$install_root/.previous.new.$$"
  temporary_targets+=("$previous_new")
  ln -s -- "$old_target" "$previous_new"
  mv -Tf -- "$previous_new" "$install_root/previous"
fi
mv -Tf -- "$current_new" "$install_root/current"
systemctl daemon-reload

if ((activate_local)); then
  for unit in "${new_broker_units[@]}"; do
    if [[ -n ${was_active_set[$unit]+x} ]]; then
      unchanged=0
      for prior_unchanged in "${unchanged_broker_units[@]}"; do [[ $prior_unchanged == "$unit" ]] && unchanged=1; done
      ((unchanged)) && continue
      if [[ $PERSEA_HERMETIC == 1 && ${PERSEA_TEST_START_ONLY_MUTANT:-0} == 1 ]]; then
        systemctl start "$unit"
      else
        systemctl restart "$unit"
      fi
    else
      systemctl start "$unit"
    fi
  done
  if [[ -n ${was_active_set[persea-terminal-front.service]+x} && ${#removed_units[@]} == 0 ]]; then
    if [[ $PERSEA_HERMETIC == 1 && ${PERSEA_TEST_START_ONLY_MUTANT:-0} == 1 ]]; then
      systemctl start persea-terminal-front.service
    else
      systemctl restart persea-terminal-front.service
    fi
  else
    systemctl start persea-terminal-front.service
  fi
  ((${#removed_units[@]} == 0)) || systemctl disable "${removed_units[@]}"
  systemctl enable "${new_units[@]}"
  "$SCRIPT_DIR/verify.sh" --require-active >/dev/null
else
  "$SCRIPT_DIR/verify.sh" >/dev/null
fi
trap - ERR INT TERM HUP
mutation_armed=0

persea_prune_releases "$keep_releases"
printf 'RELEASE=%s\nCURRENT=%s\nLOCAL_ACTIVATION=%s\n' "$release_dir" "$new_target" "$activate_local"
