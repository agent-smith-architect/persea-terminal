#!/usr/bin/env bash
# shellcheck disable=SC2034

# Shared, side-effect-free constants and candidate generation for Slice 2.4.
# Executable entry points source this file; it must not be run directly.

# Tailscale compatibility policy: a minimum version floor plus a capability
# probe of the installed binaries, never an exact version/commit pin. The host
# package auto-updates and the sidecar unit executes the system tailscaled, so
# a pin can only block deploys; it cannot protect the running sidecar. The
# floor is the release the ingress slice was validated against. Newer releases
# are accepted when every flag/subcommand below is still present in the
# installed binary's help output. See deploy/README.md "Tailscale
# compatibility policy".
PERSEA_TS_MIN_VERSION=1.102.2

# Required capability entries: "<binary>|<help path>|<kind>|<token>"
#   binary     cli (the tailscale CLI) or daemon (tailscaled)
#   help path  subcommand words whose --help output is parsed; empty = top level
#   kind       flag (an option line), subcommand (a listed command),
#              or text (a literal substring of the help text)
# Each group names where the deploy slice depends on the capability.
PERSEA_TS_REQUIRED_CAPABILITIES=(
  # sidecar unit ExecStart, rendered by deploy/host-config.py sidecar_unit()
  'daemon||flag|-state'
  'daemon||flag|-statedir'
  'daemon||flag|-socket'
  'daemon||flag|-tun'
  'daemon||text|userspace-networking'
  'daemon||flag|-port'
  # every CLI call: persea_tailscale_command pins the LocalAPI socket explicitly
  'cli||flag|--socket'
  # status --json readbacks: activate/deactivate/login/verify scripts
  'cli||subcommand|status'
  'cli|status|flag|--json'
  # Service endpoints and service-scoped rollback: activate-service.sh,
  # deactivate-service.sh, uninstall-tailscale-sidecar.sh; serve status --json
  # is also the main-node preservation readback (persea_capture_main_serve)
  'cli||subcommand|serve'
  'cli|serve|flag|--service'
  'cli|serve|flag|--https'
  'cli|serve|flag|--http'
  'cli|serve|flag|--yes'
  'cli|serve|subcommand|status'
  'cli|serve status|flag|--json'
  'cli|serve|subcommand|drain'
  'cli|serve|subcommand|clear'
  # debug prefs: AdvertiseTags/AdvertiseServices readback in activate-service.sh
  'cli|debug|subcommand|prefs'
  # sidecar enrolment: login-tailscale-sidecar.sh
  'cli||subcommand|up'
  'cli|up|flag|--hostname'
  'cli|up|flag|--advertise-tags'
)
PERSEA_HOST_CONFIG=/etc/persea-terminal/host.json
PERSEA_FRONT_SOCKET=/run/persea-terminal/front.sock
PERSEA_TS_CLI=/usr/bin/tailscale
PERSEA_TS_DAEMON=/usr/sbin/tailscaled
PERSEA_MAIN_TS_SOCKET=/run/tailscale/tailscaled.sock
PERSEA_SIDECAR_UNIT=persea-terminal-tailscaled.service
PERSEA_SIDECAR_STATE_DIR=/var/lib/persea-terminal-tailscale
PERSEA_SIDECAR_STATE_FILE=/var/lib/persea-terminal-tailscale/tailscaled.state
PERSEA_SIDECAR_RUNTIME_DIR=/run/persea-terminal-tailscale
PERSEA_SIDECAR_SOCKET=/run/persea-terminal-tailscale/tailscaled.sock
PERSEA_INSTALL_ROOT=/opt/persea-terminal
PERSEA_STATE_ROOT=/var/lib/persea-terminal
PERSEA_EVIDENCE_ROOT=/var/lib/persea-terminal-deployments
PERSEA_IMAGE_STAGING_ROOT_DEFAULT=/var/lib/persea-terminal-staging
PERSEA_UNIT_ROOT=/etc/systemd/system
PERSEA_PROBE_HELPER=libexec/probe-unix.py

PERSEA_REALM_IDS=()
PERSEA_REALM_DISPLAY_NAMES=()
PERSEA_REALM_USERS=()
PERSEA_REALM_UIDS=()
PERSEA_BROKER_UNITS=()
PERSEA_BROKER_SOCKETS=()
PERSEA_BROKER_CONFIGS=()
PERSEA_UNITS=()
PERSEA_RELEASE_UNITS=()

persea_die() {
  printf 'persea-terminal deploy: %s\n' "$*" >&2
  exit 1
}

persea_init_root() {
  PERSEA_HERMETIC=${PERSEA_DEPLOY_HERMETIC:-0}
  PERSEA_ROOT_PREFIX=${PERSEA_DEPLOY_ROOT:-}
  if [[ $PERSEA_HERMETIC == 1 ]]; then
    [[ $PERSEA_ROOT_PREFIX == /* && $PERSEA_ROOT_PREFIX != / ]] || persea_die 'hermetic root must be an absolute non-root path'
    [[ -d $PERSEA_ROOT_PREFIX && ! -L $PERSEA_ROOT_PREFIX ]] || persea_die 'hermetic root must be an existing real directory'
    PERSEA_ROOT_PREFIX=$(cd -- "$PERSEA_ROOT_PREFIX" && pwd -P)
  else
    [[ -z $PERSEA_ROOT_PREFIX ]] || persea_die 'alternate roots require PERSEA_DEPLOY_HERMETIC=1'
  fi
  persea_load_host_manifest
}

persea_load_host_manifest() {
  local manifest production=0 manifest_dir owner mode
  if [[ $PERSEA_HERMETIC == 1 ]]; then
    manifest=${PERSEA_DEPLOY_HOST_CONFIG:-$(persea_path "$PERSEA_HOST_CONFIG")}
    [[ $manifest == /* ]] || persea_die 'hermetic host manifest path must be absolute'
  else
    [[ -z ${PERSEA_DEPLOY_HOST_CONFIG:-} ]] || persea_die 'production host manifest path cannot be overridden'
    manifest=$PERSEA_HOST_CONFIG
    production=1
    manifest_dir=${manifest%/*}
    [[ -d $manifest_dir && ! -L $manifest_dir ]] || persea_die 'production host manifest directory is missing or unsafe'
    read -r owner mode < <(stat -Lc '%u %a' -- "$manifest_dir")
    [[ $owner == 0 && $((8#$mode & 0022)) == 0 ]] || persea_die 'production host manifest directory owner/mode is unsafe'
  fi
  persea_load_manifest_records "$manifest" "$production"
}

# Selects the generator that speaks for a release, into
# PERSEA_RELEASE_GENERATOR. A release that bundles libexec/host-config.py is
# judged by that copy, hash-pinned against the release's own MANIFEST.sha256
# entry immediately before execution; release self-verification and
# target-release record loading MUST both go through this one selection, so a
# release verified by its own renderer can never be re-parsed by the current
# tree's schema. The rules are:
#   - bundled generator present: pin it or die — a copy that is missing from
#     the checksum manifest, duplicated there, or whose bytes disagree is
#     tampering, never a fallback;
#   - bundled generator genuinely absent (pre-bundling release): fall back to
#     the current source tree's generator, exactly as release verification
#     does for the same shape.
# The current tree's generator otherwise serves only the current operator
# manifest (persea_load_host_manifest).
persea_select_release_generator() {
  local release=$1 hash relative manifest_hash='' generator_hash
  PERSEA_RELEASE_GENERATOR="$SCRIPT_DIR/host-config.py"
  [[ -e $release/libexec/host-config.py || -L $release/libexec/host-config.py ]] || return 0
  [[ -f $release/libexec/host-config.py && ! -L $release/libexec/host-config.py ]] || persea_die 'release bundled generator is unsafe'
  [[ -f $release/MANIFEST.sha256 && ! -L $release/MANIFEST.sha256 ]] || persea_die 'release checksum manifest is missing or unsafe'
  while read -r hash relative; do
    [[ $relative == libexec/host-config.py ]] || continue
    [[ -z $manifest_hash ]] || persea_die 'duplicate release manifest entry: libexec/host-config.py'
    manifest_hash=$hash
  done <"$release/MANIFEST.sha256"
  [[ $manifest_hash =~ ^[0-9a-f]{64}$ ]] || persea_die 'release bundled generator is missing from the checksum manifest'
  read -r generator_hash _ < <(sha256sum "$release/libexec/host-config.py")
  [[ $generator_hash == "$manifest_hash" ]] || persea_die 'release bundled generator disagrees with the checksum manifest'
  PERSEA_RELEASE_GENERATOR="$release/libexec/host-config.py"
}

persea_load_release_manifest() {
  local release=$1
  [[ -f $release/config/host.json && ! -L $release/config/host.json ]] || persea_die 'release host snapshot is missing or unsafe'
  persea_select_release_generator "$release"
  persea_load_manifest_records "$release/config/host.json" 0 "$PERSEA_RELEASE_GENERATOR"
}

# The NUL-delimited stream between a generator's `records` subcommand and this
# loader is a versioned deploy ABI: the stream leads with its version record
# and this loader refuses versions it does not speak. A target release's
# stream comes from that release's own pinned generator, so an installed
# release keeps speaking the schema it shipped with; any future stream change
# must arrive as a new version record that this loader learns to accept
# alongside every version an installed release may still emit.
persea_load_manifest_records() {
  local manifest=$1 production=$2 generator=${3:-$SCRIPT_DIR/host-config.py} index=0 tag key value digest='' record_count
  local -a command records
  command=(python3 "$generator" records "$manifest")
  ((production == 0)) || command+=(--production)
  records=()
  mapfile -d '' -t records < <("${command[@]}")
  ((${#records[@]} >= 3)) || persea_die 'host manifest parser returned an incomplete record stream'
  [[ ${records[index]} == V1 ]] || persea_die 'host manifest parser returned an unknown record version'
  ((index += 1))

  PERSEA_REALM_IDS=()
  PERSEA_REALM_DISPLAY_NAMES=()
  PERSEA_REALM_USERS=()
  PERSEA_REALM_UIDS=()
  PERSEA_BROKER_UNITS=()
  PERSEA_BROKER_SOCKETS=()
  PERSEA_BROKER_CONFIGS=()
  while ((index < ${#records[@]})); do
    tag=${records[index]}; ((index += 1))
    case $tag in
      SCALAR)
        ((index + 1 < ${#records[@]})) || persea_die 'truncated host manifest scalar record'
        key=${records[index]}; value=${records[index + 1]}; ((index += 2))
        case $key in
          PERSEA_FRONT_USER|PERSEA_FRONT_UID|PERSEA_GROUP|PERSEA_GROUP_GID|PERSEA_OPERATOR|PERSEA_MAX_CONNECTIONS|PERSEA_SERVICE|PERSEA_SIDECAR_HOSTNAME|PERSEA_SIDECAR_TAG|PERSEA_TAILNET_SUFFIX|PERSEA_CANONICAL_SERVICE_FQDN|PERSEA_PROTECTED_MAIN_DNS_NAME)
            printf -v "$key" '%s' "$value"
            ;;
          *) persea_die "unknown host manifest scalar: $key" ;;
        esac
        ;;
      REALM)
        ((index + 6 < ${#records[@]})) || persea_die 'truncated host manifest realm record'
        PERSEA_REALM_IDS+=("${records[index]}")
        PERSEA_REALM_DISPLAY_NAMES+=("${records[index + 1]}")
        PERSEA_REALM_USERS+=("${records[index + 2]}")
        PERSEA_REALM_UIDS+=("${records[index + 3]}")
        PERSEA_BROKER_UNITS+=("${records[index + 4]}")
        PERSEA_BROKER_SOCKETS+=("${records[index + 5]}")
        PERSEA_BROKER_CONFIGS+=("${records[index + 6]}")
        ((index += 7))
        ;;
      END)
        ((index < ${#records[@]})) || persea_die 'truncated host manifest end record'
        digest=${records[index]}; ((index += 1))
        if [[ ! $digest =~ ^[0-9a-f]{64}$ ]] || ((index != ${#records[@]})); then
          persea_die 'invalid host manifest terminal record'
        fi
        break
        ;;
      *) persea_die "unknown host manifest record: $tag" ;;
    esac
  done
  [[ -n $digest ]] || persea_die 'host manifest record stream lacks a terminal digest'
  record_count=${#PERSEA_REALM_IDS[@]}
  ((record_count >= 1 && record_count == ${#PERSEA_REALM_USERS[@]} && record_count == ${#PERSEA_BROKER_UNITS[@]})) ||
    persea_die 'host manifest realm records are inconsistent'
  PERSEA_HOST_MANIFEST_PATH=$manifest
  PERSEA_HOST_MANIFEST_SHA256=$digest
  PERSEA_UNITS=("${PERSEA_BROKER_UNITS[@]}" persea-terminal-front.service)
  PERSEA_RELEASE_UNITS=("${PERSEA_UNITS[@]}" "$PERSEA_SIDECAR_UNIT")
}

persea_verify_host_accounts() {
  local group_row group_gid user actual_uid expected_uid index
  group_row=$(getent group "$PERSEA_GROUP" || true)
  group_gid=${group_row#*:*:}; group_gid=${group_gid%%:*}
  [[ $group_gid == "$PERSEA_GROUP_GID" ]] || persea_die 'shared group/GID mismatch'
  for index in "${!PERSEA_REALM_USERS[@]}"; do
    user=${PERSEA_REALM_USERS[index]}
    expected_uid=${PERSEA_REALM_UIDS[index]}
    actual_uid=$(id -u "$user" 2>/dev/null || true)
    [[ $actual_uid == "$expected_uid" ]] || persea_die "realm user/UID mismatch: ${PERSEA_REALM_IDS[index]}"
    id -nG "$user" | tr ' ' '\n' | grep -Fxq "$PERSEA_GROUP" || persea_die "$user is not a member of $PERSEA_GROUP"
  done
  actual_uid=$(id -u "$PERSEA_FRONT_USER" 2>/dev/null || true)
  [[ $actual_uid == "$PERSEA_FRONT_UID" ]] || persea_die 'front user/UID mismatch'
  id -nG "$PERSEA_FRONT_USER" | tr ' ' '\n' | grep -Fxq "$PERSEA_GROUP" || persea_die "$PERSEA_FRONT_USER is not a member of $PERSEA_GROUP"
}

# The effective image staging root for a rendered release: the front config's
# image_staging_root (manifest front.staging_root) or the frozen default.
# Deriving it from the release keeps installer, verifier, and uninstaller on
# the exact root the units and broker configs were rendered against.
persea_release_staging_root() {
  local release=$1
  [[ -f $release/config/front.json ]] || persea_die "release front config missing: $release"
  # A release's staging root must be derived from that release's OWN artifacts,
  # never from the current tree's default: an older release rendered before the
  # root was parametrized (or before a default change) carries the truth in its
  # broker configs, and judging it against a newer default reports legitimate
  # version drift as incoherence — the 2026-08-21 activation failure.
  python3 - "$release/config/front.json" "$PERSEA_IMAGE_STAGING_ROOT_DEFAULT" "$release"/config/broker-*.json <<'PY'
import json, os, sys
front = json.load(open(sys.argv[1], encoding="utf-8"))
root = front.get("image_staging_root")
if not root:
    parents = set()
    for path in sys.argv[3:]:
        if not os.path.isfile(path):
            continue
        staging = json.load(open(path, encoding="utf-8")).get("image_staging_dir")
        if staging is not None:
            parents.add(os.path.dirname(staging))
    if len(parents) > 1:
        raise SystemExit("ambiguous image staging root across broker configs")
    root = parents.pop() if parents else sys.argv[2]
if not root.startswith("/") or root == "/" or root.endswith("/"):
    raise SystemExit("invalid image staging root")
print(root)
PY
}

# The front's durable stores must all live in its 0700 state directory. The
# alias store path is required; the other store paths were
# added later and are optional in the front config, so a release rendered
# before they existed carries neither and passes — a release is judged only by
# its own artifacts. When present, each must be a clean absolute path, a
# sibling of the alias store, distinct from every other store file, and never
# under the release's image staging root (broker-owned, outside the front's
# confinement).
persea_verify_front_store_paths() {
  local release=$1 staging_root=$2
  [[ -f $release/config/front.json ]] || persea_die "release front config missing: $release"
  python3 - "$release/config/front.json" "$staging_root" <<'PY' || persea_die 'front store paths are incoherent'
import json, os, sys
front = json.load(open(sys.argv[1], encoding="utf-8"))
staging_root = sys.argv[2]
alias = front.get("alias_store_path")
if not isinstance(alias, str) or not alias.startswith("/") or os.path.normpath(alias) != alias:
    raise SystemExit("alias_store_path is not a clean absolute path")
home = os.path.dirname(alias)
seen = {alias}
for key in ("preferences_store_path", "snippet_store_path", "workspace_store_path", "keyboard_preferences_store_path"):
    value = front.get(key)
    if value is None:
        continue
    if not isinstance(value, str) or not value.startswith("/") or os.path.normpath(value) != value:
        raise SystemExit(f"{key} is not a clean absolute path")
    if os.path.dirname(value) != home:
        raise SystemExit(f"{key} is not a sibling of the alias store")
    if value in seen:
        raise SystemExit(f"{key} reuses another store file")
    if value == staging_root or value.startswith(staging_root + "/"):
        raise SystemExit(f"{key} lives under the image staging root")
    seen.add(value)
PY
}

persea_path() {
  local absolute=$1
  [[ $absolute == /* ]] || persea_die "internal non-absolute path: $absolute"
  printf '%s%s\n' "$PERSEA_ROOT_PREFIX" "$absolute"
}

persea_effective_uid() {
  if [[ $PERSEA_HERMETIC == 1 && -n ${PERSEA_TEST_EUID:-} ]]; then
    printf '%s\n' "$PERSEA_TEST_EUID"
  else
    id -u
  fi
}

persea_require_root() {
  [[ $(persea_effective_uid) == 0 ]] || persea_die 'root execution is required'
}

persea_require_hermetic_mocks() {
  local command resolved mock_root
  [[ $PERSEA_HERMETIC == 1 ]] || return 0
  mock_root=${PERSEA_DEPLOY_MOCK_BIN:-}
  [[ $mock_root == /* && -d $mock_root && ! -L $mock_root ]] || persea_die 'hermetic execution requires a real PERSEA_DEPLOY_MOCK_BIN directory'
  mock_root=$(cd -- "$mock_root" && pwd -P)
  for command in "$@"; do
    resolved=$(command -v "$command" 2>/dev/null || true)
    [[ $resolved == "$mock_root/"* ]] || persea_die "hermetic command is not mock-bound: $command"
  done
}

persea_tailscale_command() {
  local endpoint=$1 cli socket
  shift
  case $endpoint in
    main) socket=$PERSEA_MAIN_TS_SOCKET ;;
    sidecar) socket=$PERSEA_SIDECAR_SOCKET ;;
    *) persea_die "internal unknown Tailscale endpoint: $endpoint" ;;
  esac
  if [[ $PERSEA_HERMETIC == 1 ]]; then
    cli=$(command -v tailscale 2>/dev/null || true)
    [[ -n $cli ]] || persea_die 'hermetic Tailscale CLI mock is missing'
    socket=$(persea_path "$socket")
  else
    cli=$PERSEA_TS_CLI
    [[ -f $cli && ! -L $cli && -x $cli ]] || persea_die "exact Tailscale CLI is missing or unsafe: $cli"
  fi
  "$cli" "--socket=$socket" "$@"
}

persea_main_tailscale() {
  persea_tailscale_command main "$@"
}

persea_sidecar_tailscale() {
  persea_tailscale_command sidecar "$@"
}

persea_tailscaled_path() {
  local daemon=$PERSEA_TS_DAEMON
  [[ $PERSEA_HERMETIC == 1 ]] && daemon=$(persea_path "$PERSEA_TS_DAEMON")
  [[ -f $daemon && ! -L $daemon && -x $daemon ]] || persea_die "exact tailscaled daemon is missing or unsafe: $daemon"
  printf '%s\n' "$daemon"
}

# Reduce a reported version ("1.102.3", "1.103.5-dev20260901") to MAJOR.MINOR.PATCH.
persea_tailscale_version_core() {
  local core=${1%%[!0-9.]*}
  [[ $core =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || return 1
  printf '%s\n' "$core"
}

# True when MAJOR.MINOR.PATCH $1 is at least $2, comparing numerically per component.
persea_tailscale_version_at_least() {
  local -a have want
  local index
  IFS=. read -r -a have <<<"$1"
  IFS=. read -r -a want <<<"$2"
  for index in 0 1 2; do
    if ((10#${have[index]} > 10#${want[index]})); then return 0; fi
    if ((10#${have[index]} < 10#${want[index]})); then return 1; fi
  done
  return 0
}

persea_require_tailscale_floor() {
  local label=$1 reported=$2 core
  core=$(persea_tailscale_version_core "$reported") || persea_die "$label reports an unparseable version: ${reported:-<empty>}"
  persea_tailscale_version_at_least "$core" "$PERSEA_TS_MIN_VERSION" ||
    persea_die "$label $core is below the supported Tailscale floor $PERSEA_TS_MIN_VERSION"
  printf '%s\n' "$core"
}

persea_tailscale_capability_label() {
  local binary=$1 path=$2
  case $binary in
    cli) printf 'tailscale%s --help\n' "${path:+ $path}" ;;
    daemon) printf 'tailscaled --help\n' ;;
    *) persea_die "internal unknown Tailscale capability binary: $binary" ;;
  esac
}

# Help output is parsed offline: every --help form exits without contacting
# a daemon, so the probe never reaches a LocalAPI socket. Exit status is
# ignored on purpose; a binary that cannot print its help fails the token
# checks and is refused with the missing capability named.
persea_tailscale_help_text() {
  local binary=$1 path=$2 daemon
  local -a words=()
  [[ -z $path ]] || read -r -a words <<<"$path"
  case $binary in
    cli) persea_main_tailscale "${words[@]}" --help 2>&1 || true ;;
    daemon) daemon=$(persea_tailscaled_path); "$daemon" --help 2>&1 || true ;;
    *) persea_die "internal unknown Tailscale capability binary: $binary" ;;
  esac
}

persea_probe_tailscale_capabilities() {
  local binary=$1 entry entry_binary path kind token help cache_key present
  local -A help_cache=()
  for entry in "${PERSEA_TS_REQUIRED_CAPABILITIES[@]}"; do
    IFS='|' read -r entry_binary path kind token <<<"$entry"
    [[ $entry_binary == "$binary" ]] || continue
    cache_key=${path:-top}
    if [[ -z ${help_cache[$cache_key]+set} ]]; then
      help_cache[$cache_key]=$(persea_tailscale_help_text "$binary" "$path")
    fi
    help=${help_cache[$cache_key]}
    present=0
    case $kind in
      flag) grep -Eq -- "^[[:space:]]+${token}([[:space:],=]|\$)" <<<"$help" && present=1 ;;
      subcommand) grep -Eq -- "^[[:space:]]+${token}[[:space:]]" <<<"$help" && present=1 ;;
      text) grep -Fq -- "$token" <<<"$help" && present=1 ;;
      *) persea_die "internal unknown Tailscale capability kind: $kind" ;;
    esac
    ((present)) || persea_die "$(persea_tailscale_capability_label "$binary" "$path") lacks required $kind $token; the installed Tailscale is not supported"
  done
}

# Floor + capability probe of the tailscale CLI. Records the validated
# versions in PERSEA_TS_CLI_VERSION / PERSEA_TS_CLI_LONG_VERSION.
persea_require_tailscale_cli() {
  local output
  output=$(persea_main_tailscale version)
  PERSEA_TS_CLI_VERSION=$(persea_require_tailscale_floor 'tailscale CLI' "$(sed -n '1p' <<<"$output" | tr -d '[:space:]')")
  PERSEA_TS_CLI_LONG_VERSION=$(sed -n 's/^[[:space:]]*long version: //p' <<<"$output" | tr -d '[:space:]')
  persea_probe_tailscale_capabilities cli
}

# Floor + capability probe of the tailscaled daemon the sidecar unit executes.
# Records the validated versions in PERSEA_TSD_VERSION / PERSEA_TSD_LONG_VERSION.
persea_require_tailscaled() {
  local daemon output
  daemon=$(persea_tailscaled_path)
  output=$("$daemon" --version)
  PERSEA_TSD_VERSION=$(persea_require_tailscale_floor 'tailscaled daemon' "$(sed -n '1p' <<<"$output" | tr -d '[:space:]')")
  PERSEA_TSD_LONG_VERSION=$(sed -n 's/^[[:space:]]*long version: //p' <<<"$output" | tr -d '[:space:]')
  persea_probe_tailscale_capabilities daemon
}

# Record which Tailscale the deploy step validated, for incident readback.
# Informational only: nothing gates on this file.
persea_write_tailscale_evidence() {
  local output=$1
  [[ -n ${PERSEA_TS_CLI_VERSION:-} && -n ${PERSEA_TSD_VERSION:-} ]] ||
    persea_die 'internal: Tailscale CLI and daemon must be validated before recording evidence'
  python3 - "$output" "$PERSEA_TS_MIN_VERSION" "$PERSEA_TS_CLI_VERSION" "$PERSEA_TS_CLI_LONG_VERSION" \
    "$PERSEA_TSD_VERSION" "$PERSEA_TSD_LONG_VERSION" "${PERSEA_TS_REQUIRED_CAPABILITIES[@]}" <<'PY'
import json, sys
output, floor, cli_version, cli_long, daemon_version, daemon_long, *entries = sys.argv[1:]
capabilities = []
for entry in entries:
    binary, path, kind, token = entry.split("|")
    capabilities.append({"binary": binary, "help_path": path, "kind": kind, "token": token})
record = {
    "min_version": floor,
    "cli": {"version": cli_version, "long_version": cli_long},
    "daemon": {"version": daemon_version, "long_version": daemon_long},
    "capabilities_verified": capabilities,
}
with open(output, "w", encoding="utf-8") as handle:
    json.dump(record, handle, separators=(",", ":"), sort_keys=True)
    handle.write("\n")
PY
}

persea_assert_root_private_directory() {
  local path=$1 label=$2 expected_owner expected_group metadata
  [[ -d $path && ! -L $path ]] || persea_die "$label is missing or unsafe: $path"
  expected_owner=$(persea_expected_root_owner)
  expected_group=0
  [[ $PERSEA_HERMETIC == 1 ]] && expected_group=$(id -g)
  metadata=$(stat -Lc '%u:%g:%a' -- "$path") || persea_die "cannot stat $label: $path"
  [[ $metadata == "$expected_owner:$expected_group:700" ]] || persea_die "$label owner/group/mode mismatch: $path"
}

persea_assert_sidecar_runtime() {
  local unit_root runtime state socket daemon expected_owner expected_group metadata fragment main_pid daemon_identity process_metadata
  unit_root=$(persea_path "$PERSEA_UNIT_ROOT")
  runtime=$(persea_path "$PERSEA_SIDECAR_RUNTIME_DIR")
  state=$(persea_path "$PERSEA_SIDECAR_STATE_DIR")
  socket=$(persea_path "$PERSEA_SIDECAR_SOCKET")
  daemon=$PERSEA_TS_DAEMON
  [[ $PERSEA_HERMETIC == 1 ]] && daemon=$(persea_path "$PERSEA_TS_DAEMON")
  persea_assert_root_private_directory "$runtime" 'sidecar runtime directory'
  persea_assert_root_private_directory "$state" 'sidecar state directory'
  [[ -S $socket && ! -L $socket ]] || persea_die "sidecar control socket is missing or unsafe: $socket"
  expected_owner=$(persea_expected_root_owner)
  expected_group=0
  [[ $PERSEA_HERMETIC == 1 ]] && expected_group=$(id -g)
  metadata=$(stat -Lc '%u:%g:%a' -- "$socket") || persea_die 'cannot stat sidecar control socket'
  # tailscaled 1.98.9 explicitly chmods its Linux LocalAPI socket to 0666.
  # The exact root-owned 0700 parent is the confidentiality boundary.
  [[ $metadata == "$expected_owner:$expected_group:666" ]] || persea_die 'sidecar control socket owner/group/mode mismatch'
  [[ -f $daemon && ! -L $daemon && -x $daemon ]] || persea_die "exact tailscaled daemon is missing or unsafe: $daemon"
  systemctl is-active --quiet "$PERSEA_SIDECAR_UNIT" || persea_die 'sidecar unit is inactive'
  fragment=$(systemctl show "$PERSEA_SIDECAR_UNIT" --property=FragmentPath --value)
  [[ $fragment == "$unit_root/$PERSEA_SIDECAR_UNIT" ]] || persea_die 'sidecar unit fragment drift'
  main_pid=$(systemctl show "$PERSEA_SIDECAR_UNIT" --property=MainPID --value)
  [[ $main_pid =~ ^[1-9][0-9]*$ ]] || persea_die 'sidecar unit has no MainPID'
  daemon_identity=$(stat -Lc '%d:%i' -- "$daemon")
  if [[ $PERSEA_HERMETIC == 1 ]]; then
    process_metadata=$(persea-process-metadata "$PERSEA_SIDECAR_UNIT" "$main_pid" "$daemon")
  else
    [[ -d /proc/$main_pid ]] || persea_die 'sidecar MainPID disappeared'
    process_metadata="$(stat -Lc '%u' -- "/proc/$main_pid"):$(stat -Lc '%d:%i' -- "/proc/$main_pid/exe")"
  fi
  [[ $process_metadata == "$expected_owner:$daemon_identity" ]] || persea_die 'sidecar MainPID UID/executable drift'
}

persea_capture_main_serve() {
  local output=$1
  persea_main_tailscale serve status --json >"$output"
}

persea_assert_main_serve_unchanged() {
  local before=$1 after=$2
  cmp -s "$before" "$after" || {
    printf 'persea-terminal deploy: protected main-node Serve state drifted; refusing automatic repair\n' >&2
    return 1
  }
}

persea_validate_host() {
  local host=$1 label
  [[ -n $host && $host == "${host,,}" && ${#host} -le 253 ]] || return 1
  [[ $host != localhost && $host != *.localhost && $host != 127.* && $host != ::1 ]] || return 1
  [[ $host != "$host":* && $host != *"/"* && $host != *"\\"* && $host != *"@"* && $host != *" "* ]] || return 1
  [[ $host == "$PERSEA_CANONICAL_SERVICE_FQDN" && $host != "$PERSEA_PROTECTED_MAIN_DNS_NAME" ]] || return 1
  IFS=. read -r -a labels <<<"$host"
  for label in "${labels[@]}"; do
    [[ $label =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$ ]] || return 1
  done
}

persea_require_safe_directory() {
  local path=$1 allow_missing=${2:-0} info owner mode
  if [[ ! -e $path ]]; then
    [[ $allow_missing == 1 ]] || persea_die "required directory is missing: $path"
    return 0
  fi
  [[ -d $path && ! -L $path ]] || persea_die "unsafe non-directory target: $path"
  info=$(stat -Lc '%u %a' -- "$path") || persea_die "cannot stat target: $path"
  read -r owner mode <<<"$info"
  if [[ $PERSEA_HERMETIC == 1 ]]; then
    [[ $owner == $(id -u) ]] || persea_die "hermetic target has wrong owner: $path"
  else
    [[ $owner == 0 ]] || persea_die "target is not root-owned: $path"
  fi
  (( (8#$mode & 0022) == 0 )) || persea_die "target is group/other writable: $path"
}

persea_expected_root_owner() {
  if [[ $PERSEA_HERMETIC == 1 ]]; then
    id -u
  else
    printf '0\n'
  fi
}

persea_evidence_metadata() {
  local path=$1 metadata
  metadata=$(stat -Lc '%u:%g:%a:%d:%i' -- "$path") || {
    printf 'persea-terminal deploy: cannot stat deployment evidence root: %s\n' "$path" >&2
    return 1
  }
  if [[ $PERSEA_HERMETIC == 1 && -n ${PERSEA_TEST_EVIDENCE_OWNER:-} ]]; then
    metadata=${PERSEA_TEST_EVIDENCE_OWNER}:${metadata#*:}
  fi
  printf '%s\n' "$metadata"
}

persea_assert_evidence_root() {
  local path=$1 identity=$2 expected_owner metadata owner group mode device inode expected_group parent parent_owner parent_mode
  local -a ancestors
  expected_owner=$(persea_expected_root_owner)
  expected_group=0
  [[ $PERSEA_HERMETIC == 1 ]] && expected_group=$(id -g)
  ancestors=("$(persea_path /var)" "$(dirname -- "$path")")
  for parent in "${ancestors[@]}"; do
    [[ -d $parent && ! -L $parent ]] || {
      printf 'persea-terminal deploy: deployment evidence ancestor is unsafe: %s\n' "$parent" >&2
      return 1
    }
    read -r parent_owner parent_mode < <(stat -Lc '%u %a' -- "$parent") || return 1
    [[ $parent_owner == "$expected_owner" && $((8#$parent_mode & 0022)) == 0 ]] || {
      printf 'persea-terminal deploy: deployment evidence ancestor owner/mode mismatch: %s\n' "$parent" >&2
      return 1
    }
  done
  [[ -d $path && ! -L $path ]] || {
    printf 'persea-terminal deploy: deployment evidence root is not a real directory\n' >&2
    return 1
  }
  metadata=$(persea_evidence_metadata "$path") || return 1
  IFS=: read -r owner group mode device inode <<<"$metadata"
  [[ $owner == "$expected_owner" && $group == "$expected_group" && $mode == 700 ]] || {
    printf 'persea-terminal deploy: deployment evidence root owner/group/mode mismatch\n' >&2
    return 1
  }
  [[ $device:$inode == "$identity" ]] || {
    printf 'persea-terminal deploy: deployment evidence root inode changed\n' >&2
    return 1
  }
}

persea_prepare_evidence_root() {
  local path expected_owner metadata owner group mode device inode expected_group
  path=$(persea_path "$PERSEA_EVIDENCE_ROOT")
  persea_require_safe_directory "$(persea_path /var)"
  persea_require_safe_directory "$(dirname -- "$path")"
  if [[ ! -e $path ]]; then
    if [[ $PERSEA_HERMETIC == 1 ]]; then
      install -d -m 0700 "$path"
    else
      install -d -o root -g root -m 0700 "$path"
    fi
  fi
  [[ -d $path && ! -L $path ]] || persea_die 'deployment evidence root is not a real directory'
  expected_owner=$(persea_expected_root_owner)
  expected_group=0
  [[ $PERSEA_HERMETIC == 1 ]] && expected_group=$(id -g)
  metadata=$(persea_evidence_metadata "$path")
  IFS=: read -r owner group mode device inode <<<"$metadata"
  [[ $owner == "$expected_owner" && $group == "$expected_group" && $mode == 700 ]] || persea_die 'deployment evidence root owner/group/mode mismatch'
  printf '%s %s:%s\n' "$path" "$device" "$inode"
}

persea_verify_release_metadata() {
  local release=$1 allow_legacy=${2:-0} expected_owner expected_group path mode owner group links count hash relative
  local -a manifest_paths
  local -A seen_paths
  [[ -d $release && ! -L $release ]] || persea_die "release is not a real directory: $release"
  expected_owner=0
  expected_group=0
  if [[ $PERSEA_HERMETIC == 1 ]]; then
    expected_owner=$(id -u)
    expected_group=$(id -g)
  fi
  if find "$release" -type l -o \( ! -type d ! -type f \) | grep -q .; then
    persea_die "release contains a symlink or special file: $release"
  fi
  [[ -f $release/MANIFEST.sha256 && ! -L $release/MANIFEST.sha256 ]] || persea_die 'release checksum manifest is missing or unsafe'
  manifest_paths=()
  while read -r hash relative; do
    [[ $hash =~ ^[0-9a-f]{64}$ && $relative =~ ^[A-Za-z0-9._/-]+$ && $relative != /* && $relative != *'..'* && $relative != *//* ]] ||
      persea_die "unsafe release manifest entry: $relative"
    [[ -z ${seen_paths[$relative]+x} ]] || persea_die "duplicate release manifest entry: $relative"
    seen_paths[$relative]=1
    manifest_paths+=("$relative")
    [[ -f $release/$relative && ! -L $release/$relative ]] || persea_die "release manifest target is missing or unsafe: $relative"
  done <"$release/MANIFEST.sha256"
  ((${#manifest_paths[@]} >= 1)) || persea_die 'release checksum manifest is empty'
  count=$(find "$release" -type f | wc -l)
  [[ $count == $((${#manifest_paths[@]} + 1)) ]] || persea_die "release file inventory mismatch: $count"
  while IFS= read -r -d '' path; do
    read -r owner group mode < <(stat -Lc '%u %g %a' -- "$path")
    [[ $owner == "$expected_owner" && $group == "$expected_group" && $mode == 555 ]] || persea_die "release directory metadata mismatch: $path"
  done < <(find "$release" -type d -print0)
  while IFS= read -r -d '' path; do
    read -r owner group mode links < <(stat -Lc '%u %g %a %h' -- "$path")
    [[ $links == 1 ]] || persea_die "release file has an unsafe link count: $path"
    if [[ $path == "$release/bin/persea-terminal" ]]; then
      [[ $owner == "$expected_owner" && $group == "$expected_group" && $mode == 555 ]] || persea_die "release binary metadata mismatch: $path"
    else
      [[ $owner == "$expected_owner" && $group == "$expected_group" && $mode == 444 ]] || persea_die "release file metadata mismatch: $path"
    fi
  done < <(find "$release" -type f -print0)
  if [[ -f $release/config/host.json && ! -L $release/config/host.json ]]; then
    # A release must be verified by the generator that produced it: a later
    # renderer legitimately emits different bytes from the same snapshot, and
    # re-rendering an old release with the current generator would report that
    # drift as tampering. persea_select_release_generator pins the bundled
    # copy against the checksum manifest immediately before it runs, and
    # record loading uses the identical selection.
    persea_select_release_generator "$release"
    python3 "$PERSEA_RELEASE_GENERATOR" verify-release "$release/config/host.json" "$release" ||
      persea_die 'release host snapshot and generated files disagree'
  else
    [[ $allow_legacy == 1 ]] || persea_die 'release host snapshot is missing'
  fi
}

persea_verify_release() {
  local release=$1
  [[ -d $release && ! -L $release ]] || persea_die "release is not a real directory: $release"
  (cd "$release" && sha256sum -c MANIFEST.sha256 >/dev/null) || persea_die "release manifest mismatch: $release"
  persea_verify_release_metadata "$release"
}

persea_verify_legacy_release() {
  local release=$1 relative
  local -a required=(
    bin/persea-terminal
    ui/index.html ui/app.js ui/app.css ui/xterm.css
    libexec/probe-unix.py
    config/front.json
    units/persea-terminal-front.service units/persea-terminal-tailscaled.service
  )
  [[ -d $release && ! -L $release && ! -e $release/config/host.json && ! -L $release/config/host.json ]] ||
    persea_die 'legacy release shape is ambiguous'
  (cd "$release" && sha256sum -c MANIFEST.sha256 >/dev/null) || persea_die "legacy release manifest mismatch: $release"
  persea_verify_release_metadata "$release" 1
  for relative in "${required[@]}"; do
    [[ -f $release/$relative && ! -L $release/$relative ]] || persea_die "legacy release payload is missing or unsafe: $relative"
  done
}

persea_validate_managed_unit_name() {
  local unit=$1
  [[ $unit == persea-terminal-front.service || $unit =~ ^persea-terminal-broker-[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?\.service$ ]]
}

persea_read_release_inventory() {
  local release=$1 output_name=$2 unit
  local -n output=$output_name
  local -A seen
  output=()
  [[ -f $release/config/managed-units && ! -L $release/config/managed-units ]] || persea_die 'release managed-unit inventory is missing or unsafe'
  while IFS= read -r unit; do
    [[ -n $unit ]] || persea_die 'release managed-unit inventory contains a blank entry'
    persea_validate_managed_unit_name "$unit" || persea_die "unsafe managed unit name: $unit"
    [[ -z ${seen[$unit]+x} ]] || persea_die "duplicate managed unit: $unit"
    seen[$unit]=1
    output+=("$unit")
    [[ -f $release/units/$unit && ! -L $release/units/$unit ]] || persea_die "managed unit is missing from release: $unit"
  done <"$release/config/managed-units"
  ((${#output[@]} >= 2 && ${#output[@]} <= 33)) || persea_die 'managed-unit inventory has an invalid size'
  [[ ${output[-1]} == persea-terminal-front.service ]] || persea_die 'managed-unit inventory must end with the front unit'
  for unit in "${output[@]:0:${#output[@]}-1}"; do
    [[ $unit == persea-terminal-broker-*.service ]] || persea_die 'broker units must precede the front unit'
  done
}

persea_assert_installed_units() {
  local release=$1 unit_root=$2 unit path expected_owner expected_group owner group mode actual_count=0
  local -a release_units
  local -A release_set actual_set
  persea_require_safe_directory "$unit_root"
  persea_read_release_inventory "$release" release_units
  for unit in "${release_units[@]}"; do release_set[$unit]=1; done
  while IFS= read -r -d '' path; do
    unit=${path##*/}
    persea_validate_managed_unit_name "$unit" || persea_die "unsafe installed application-unit name: $unit"
    [[ -z ${actual_set[$unit]+x} ]] || persea_die "duplicate installed application unit: $unit"
    actual_set[$unit]=1
    ((actual_count += 1))
    [[ -n ${release_set[$unit]+x} ]] || persea_die "unexpected installed application unit: $unit"
  done < <(find "$unit_root" -mindepth 1 -maxdepth 1 \( -name 'persea-terminal-broker-*.service' -o -name 'persea-terminal-front.service' \) -print0)
  [[ $actual_count == "${#release_units[@]}" ]] || persea_die 'installed application-unit inventory does not match the current release'
  expected_owner=$(persea_expected_root_owner)
  expected_group=0
  [[ $PERSEA_HERMETIC == 1 ]] && expected_group=$(id -g)
  for unit in "${release_units[@]}"; do
    [[ -f $unit_root/$unit && ! -L $unit_root/$unit ]] || persea_die "installed unit is missing or unsafe: $unit"
    cmp -s "$release/units/$unit" "$unit_root/$unit" || persea_die "installed unit does not match current release: $unit"
    read -r owner group mode < <(stat -Lc '%u %g %a' -- "$unit_root/$unit")
    [[ $owner == "$expected_owner" && $group == "$expected_group" && $mode == 444 ]] || persea_die "installed unit metadata mismatch: $unit"
  done
}

persea_assert_no_installed_units() {
  local unit_root=$1 found
  [[ -e $unit_root ]] || return 0
  persea_require_safe_directory "$unit_root"
  found=$(find "$unit_root" -maxdepth 1 \( -name 'persea-terminal-broker-*.service' -o -name 'persea-terminal-front.service' \) -print -quit)
  [[ -z $found ]] || persea_die "package unit exists without a current release: ${found##*/}"
}

persea_install_release_units() {
  local release=$1 unit_root=$2 unit unit_new
  local -a release_units
  persea_require_safe_directory "$unit_root"
  persea_read_release_inventory "$release" release_units
  for unit in "${release_units[@]}"; do
    unit_new="$unit_root/.${unit}.new.$$"
    temporary_targets+=("$unit_new")
    if [[ $PERSEA_HERMETIC == 1 ]]; then
      install -m 0444 "$release/units/$unit" "$unit_new"
    else
      install -o root -g root -m 0444 "$release/units/$unit" "$unit_new"
    fi
  done
  for unit in "${release_units[@]}"; do
    mv -Tf -- "$unit_root/.${unit}.new.$$" "$unit_root/$unit"
  done
}

persea_assert_legacy_installed_units() {
  local release=$1 unit_root=$2 unit expected_owner expected_group owner group mode
  shift 2
  persea_require_safe_directory "$unit_root"
  expected_owner=$(persea_expected_root_owner)
  expected_group=0
  [[ $PERSEA_HERMETIC == 1 ]] && expected_group=$(id -g)
  for unit in "$@"; do
    persea_validate_managed_unit_name "$unit" || persea_die "unsafe legacy managed unit: $unit"
    [[ -f $release/units/$unit && ! -L $release/units/$unit ]] || persea_die "legacy release unit is missing: $unit"
    [[ -f $unit_root/$unit && ! -L $unit_root/$unit ]] || persea_die "legacy installed unit is missing or unsafe: $unit"
    cmp -s "$release/units/$unit" "$unit_root/$unit" || persea_die "legacy installed unit drift: $unit"
    read -r owner group mode < <(stat -Lc '%u %g %a' -- "$unit_root/$unit")
    [[ $owner == "$expected_owner" && $group == "$expected_group" && $mode == 444 ]] || persea_die "legacy installed unit metadata drift: $unit"
  done
}

persea_install_legacy_units() {
  local release=$1 unit_root=$2 unit unit_new
  shift 2
  persea_require_safe_directory "$unit_root"
  for unit in "$@"; do
    persea_validate_managed_unit_name "$unit" || persea_die "unsafe legacy managed unit: $unit"
    unit_new="$unit_root/.${unit}.new.$$"
    temporary_targets+=("$unit_new")
    if [[ $PERSEA_HERMETIC == 1 ]]; then
      install -m 0444 "$release/units/$unit" "$unit_new"
    else
      install -o root -g root -m 0444 "$release/units/$unit" "$unit_new"
    fi
  done
  for unit in "$@"; do mv -Tf -- "$unit_root/.${unit}.new.$$" "$unit_root/$unit"; done
}

persea_remove_installed_units() {
  local unit_root=$1 unit
  shift
  [[ -d $unit_root && ! -L $unit_root ]] || return 0
  for unit in "$@"; do
    persea_validate_managed_unit_name "$unit" || persea_die "refusing unsafe managed-unit removal: $unit"
    rm -f -- "$unit_root/$unit"
  done
}

persea_generate_candidate() {
  local candidate_output=$1 manifest=$2
  [[ $manifest == /* ]] || persea_die 'candidate host manifest path must be absolute'
  [[ ! -e $candidate_output ]] || persea_die "candidate output already exists: $candidate_output"
  python3 "$SCRIPT_DIR/host-config.py" render "$manifest" "$candidate_output" || persea_die 'host manifest candidate generation failed'
  chmod 0600 "$candidate_output"/config/*.json "$candidate_output"/units/*.service
}

persea_assert_unit_name() {
  local candidate=$1 unit
  for unit in "${PERSEA_UNITS[@]}"; do
    [[ $candidate == "$unit" ]] && return 0
  done
  return 1
}
