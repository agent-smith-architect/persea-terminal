#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
DEPLOY_DIR=$(cd -- "$SCRIPT_DIR/.." && pwd -P)
command -v rg >/dev/null || { printf 'hermetic deployment tests require ripgrep (rg)\n' >&2; exit 1; }
PT_FIXTURE_FRONT_USER=termop_q7
PT_FIXTURE_REALM_USER=tmuxbot_k4
PT_FIXTURE_GROUP=termshare_x9
PT_FIXTURE_FRONT_UID=$(id -u)
PT_FIXTURE_REALM_UID=42002
PT_FIXTURE_GROUP_GID=42003
PT_FIXTURE_OPERATOR=operator-q7@example.invalid
export PT_FIXTURE_FRONT_USER PT_FIXTURE_REALM_USER PT_FIXTURE_GROUP
export PT_FIXTURE_FRONT_UID PT_FIXTURE_REALM_UID PT_FIXTURE_GROUP_GID PT_FIXTURE_OPERATOR
TMP=$(mktemp -d "${TMPDIR:-/tmp}/ptd.XXXXXXXX")
MOCK_BIN="$TMP/mock-bin"
FAKE_STATE="$TMP/fake-state"
DENIED_SOCKET="$TMP/denied.sock"
BYTE_SOCKET="$TMP/denied-byte.sock"
SERVER_PIDS=()
ROOT_UID_MUTANT=
STAGE_PATH_MUTANT=
OWNERSHIP_MUTANT=
OWNERSHIP_FIXTURE=
OWNERSHIP_SENTINEL=
PATH_BUDGET_MUTANT=
LANG_MUTANT=
LC_ALL_MUTANT=
tests=0

fail() { printf 'not ok %d - %s\n' "$((tests + 1))" "$*" >&2; exit 1; }
pass() { tests=$((tests + 1)); printf 'ok %d - %s\n' "$tests" "$*"; }
cleanup() {
  local rc=$?
  trap - EXIT
  if [[ -f $FAKE_STATE/sidecar-server-pids ]]; then
    while IFS= read -r pid; do kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; done <"$FAKE_STATE/sidecar-server-pids"
  fi
  if ((${#SERVER_PIDS[@]})); then kill "${SERVER_PIDS[@]}" 2>/dev/null || true; wait "${SERVER_PIDS[@]}" 2>/dev/null || true; fi
  if [[ -n $ROOT_UID_MUTANT ]]; then /usr/bin/sudo -n /bin/rm -rf -- "$ROOT_UID_MUTANT" 2>/dev/null || true; fi
  if [[ -n $STAGE_PATH_MUTANT ]]; then rm -f -- "$STAGE_PATH_MUTANT" 2>/dev/null || true; fi
  if [[ -n $OWNERSHIP_FIXTURE ]]; then /usr/bin/sudo -n /bin/rm -rf -- "$OWNERSHIP_FIXTURE" 2>/dev/null || true; fi
  if [[ -n $OWNERSHIP_SENTINEL ]]; then /usr/bin/sudo -n /bin/rm -f -- "$OWNERSHIP_SENTINEL" 2>/dev/null || true; fi
  if [[ -n $OWNERSHIP_MUTANT ]]; then rm -f -- "$OWNERSHIP_MUTANT" 2>/dev/null || true; fi
  if [[ -n $PATH_BUDGET_MUTANT ]]; then rm -f -- "$PATH_BUDGET_MUTANT" 2>/dev/null || true; fi
  if [[ -n $LANG_MUTANT ]]; then rm -f -- "$LANG_MUTANT" 2>/dev/null || true; fi
  if [[ -n $LC_ALL_MUTANT ]]; then rm -f -- "$LC_ALL_MUTANT" 2>/dev/null || true; fi
  chmod -R u+w -- "$TMP" 2>/dev/null || true
  rm -rf -- "$TMP"
  exit "$rc"
}
trap cleanup EXIT

mkdir -m 0700 "$MOCK_BIN" "$FAKE_STATE"
umask >"$MOCK_BIN/build-umask"
cat >"$MOCK_BIN/npm" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
real=$(id -ru)
effective=$(id -u)
lang=${LANG-<unset>}
lc_all=${LC_ALL-<unset>}
tmpdir=${TMPDIR-<unset>}
[[ $(umask) == $(cat "$(dirname -- "$0")/build-umask") ]] || {
  printf 'fake npm build umask differs from the invoking shell\n' >&2
  exit 98
}
printf 'npm real=%s effective=%s LANG=%s LC_ALL=%s TMPDIR=%s args=%s\n' "$real" "$effective" "$lang" "$lc_all" "$tmpdir" "$*" >>"$FAKE_STATE/build-uid.log"
[[ $real == "$PERSEA_EXPECT_BUILD_UID" && $effective == "$PERSEA_EXPECT_BUILD_UID" ]] || {
  printf 'fake npm UID witness: real=%s effective=%s expected=%s\n' "$real" "$effective" "$PERSEA_EXPECT_BUILD_UID" >&2
  exit 97
}
[[ $lang == C.UTF-8 && $lc_all == C.UTF-8 ]] || {
  printf 'fake npm locale witness: LANG=%s LC_ALL=%s expected=C.UTF-8/C.UTF-8\n' "$lang" "$lc_all" >&2
  exit 98
}
frozen_socket_suffix='/TestIngressSocketStateAndCleanupFailClosedreplacement_cleanup4294967295/001/front.sock'
socket_path_bytes=$((${#tmpdir} + ${#frozen_socket_suffix}))
((socket_path_bytes <= 107)) || {
  printf 'fake npm path-budget witness: TMPDIR=%s total=%s limit=107\n' "$tmpdir" "$socket_path_bytes" >&2
  exit 98
}
if [[ ${1:-} == run && ${2:-} == build ]]; then
  mkdir -p dist
  printf '<div id="app"></div>\n' >dist/index.html
  printf 'console.log("fresh build")\n' >dist/app.js
  printf 'body{}\n' >dist/app.css
  printf '.xterm{}\n' >dist/xterm.css
  printf 'synthetic license notice\n' >dist/THIRD_PARTY_NOTICES.txt
  for asset in app.js app.css xterm.css; do gzip -n -c "dist/$asset" >"dist/$asset.gz"; done
  # Installable browser shell. The fake build emits them because install.sh
  # requires the current tree's UI payload to be complete and regular.
  printf '{"start_url":"/?resume=1"}\n' >dist/manifest.webmanifest
  printf 'icon-192\n' >dist/icon-192.png
  printf 'icon-512\n' >dist/icon-512.png
  printf 'apple-touch-icon\n' >dist/apple-touch-icon.png
  if [[ ${FAKE_STAGE_PLANT:-0} == 1 ]]; then
    mkdir -p "$FAKE_STATE/stage-plant-target"
    if ln -s -- "$FAKE_STATE/stage-plant-target" ../../../stage 2>/dev/null; then
      printf 'created\n' >"$FAKE_STATE/root-stage-plant"
    else
      printf 'denied\n' >"$FAKE_STATE/root-stage-plant"
    fi
    ln -s -- "$FAKE_STATE/stage-plant-target" ../../stage
    printf 'created\n' >"$FAKE_STATE/builder-stage-plant"
  fi
fi
SH
cat >"$MOCK_BIN/go" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
real=$(id -ru)
effective=$(id -u)
lang=${LANG-<unset>}
lc_all=${LC_ALL-<unset>}
tmpdir=${TMPDIR-<unset>}
printf 'go real=%s effective=%s LANG=%s LC_ALL=%s TMPDIR=%s args=%s\n' "$real" "$effective" "$lang" "$lc_all" "$tmpdir" "$*" >>"$FAKE_STATE/build-uid.log"
[[ $real == "$PERSEA_EXPECT_BUILD_UID" && $effective == "$PERSEA_EXPECT_BUILD_UID" ]] || {
  printf 'fake go UID witness: real=%s effective=%s expected=%s\n' "$real" "$effective" "$PERSEA_EXPECT_BUILD_UID" >&2
  exit 97
}
[[ $lang == C.UTF-8 && $lc_all == C.UTF-8 ]] || {
  printf 'fake go locale witness: LANG=%s LC_ALL=%s expected=C.UTF-8/C.UTF-8\n' "$lang" "$lc_all" >&2
  exit 98
}
frozen_socket_suffix='/TestIngressSocketStateAndCleanupFailClosedreplacement_cleanup4294967295/001/front.sock'
socket_path_bytes=$((${#tmpdir} + ${#frozen_socket_suffix}))
((socket_path_bytes <= 107)) || {
  printf 'fake go path-budget witness: TMPDIR=%s total=%s limit=107\n' "$tmpdir" "$socket_path_bytes" >&2
  exit 98
}
if [[ ${1:-} == build || " $* " == *' build '* ]]; then
  output=
  while (($#)); do [[ $1 == -o ]] && { output=$2; break; }; shift; done
  [[ -n $output ]]
  printf '#!/usr/bin/env bash\nexit 0\n' >"$output"
  chmod 0755 "$output"
fi
SH
cat >"$MOCK_BIN/node" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
real=$(id -ru)
effective=$(id -u)
printf 'node real=%s effective=%s args=%s\n' "$real" "$effective" "$*" >>"$FAKE_STATE/build-uid.log"
[[ $real == "$PERSEA_EXPECT_BUILD_UID" && $effective == "$PERSEA_EXPECT_BUILD_UID" ]] || {
  printf 'fake node UID witness: real=%s effective=%s expected=%s\n' "$real" "$effective" "$PERSEA_EXPECT_BUILD_UID" >&2
  exit 97
}
SH
cat >"$MOCK_BIN/id" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
if [[ -n ${PT_FIXTURE_FRONT_USER:-} && -n ${PT_FIXTURE_REALM_USER:-} ]]; then
  case "${1:-} ${2:-}" in
    "-u $PT_FIXTURE_FRONT_USER") printf '%s\n' "${PT_ID_FRONT_OVERRIDE:-$PT_FIXTURE_FRONT_UID}"; exit ;;
    "-u $PT_FIXTURE_REALM_USER") printf '%s\n' "${PT_ID_REALM_OVERRIDE:-$PT_FIXTURE_REALM_UID}"; exit ;;
    "-g $PT_FIXTURE_FRONT_USER"|"-g $PT_FIXTURE_REALM_USER") printf '%s\n' "$PT_FIXTURE_GROUP_GID"; exit ;;
    "-nG $PT_FIXTURE_FRONT_USER"|"-nG $PT_FIXTURE_REALM_USER") printf '%s\n' "$PT_FIXTURE_GROUP"; exit ;;
  esac
fi
exec /usr/bin/id "$@"
SH
cat >"$MOCK_BIN/getent" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
if [[ ${1:-} == group && ${2:-} == "$PT_FIXTURE_GROUP" ]]; then
  printf '%s:x:%s:%s,%s\n' "$PT_FIXTURE_GROUP" "${PT_GROUP_GID_OVERRIDE:-$PT_FIXTURE_GROUP_GID}" "$PT_FIXTURE_FRONT_USER" "$PT_FIXTURE_REALM_USER"
else
  exec /usr/bin/getent "$@"
fi
SH
cat >"$MOCK_BIN/systemd-analyze" <<'SH'
#!/usr/bin/env bash
exit 0
SH
cat >"$MOCK_BIN/systemctl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$FAKE_STATE/systemctl.log"
pid_for_unit() {
  case $1 in
    persea-terminal-broker-desk-a7.service) printf '1101\n' ;;
    persea-terminal-broker-lab-k4.service) printf '1102\n' ;;
    persea-terminal-broker-ops-m9.service) printf '1105\n' ;;
    persea-terminal-front.service) printf '1103\n' ;;
    persea-terminal-tailscaled.service) printf '1104\n' ;;
    *) exit 1 ;;
  esac
}
uid_for_unit() {
  case $1 in
    persea-terminal-broker-desk-a7.service|persea-terminal-front.service) printf '%s\n' "$PT_FIXTURE_FRONT_UID" ;;
    persea-terminal-broker-lab-k4.service) printf '%s\n' "$PT_FIXTURE_REALM_UID" ;;
    persea-terminal-broker-ops-m9.service) printf '%s\n' "$PT_FIXTURE_FRONT_UID" ;;
    persea-terminal-tailscaled.service) id -u ;;
    *) exit 1 ;;
  esac
}
activate_unit() {
  local action=$1 unit=$2 pid target was_running=0
  if [[ ( ${FAKE_FAIL_START:-0} == 1 && $unit == persea-terminal-front.service ) || ${FAKE_FAIL_START:-0} == "$unit" ]]; then exit 1; fi
  if [[ ${FAKE_FAIL_START_ONCE:-0} == "$unit" && ! -e $FAKE_STATE/fail-start-once-$unit ]]; then
    touch "$FAKE_STATE/fail-start-once-$unit"
    exit 1
  fi
  mkdir -p "$FAKE_STATE/active" "$FAKE_STATE/proc"
  [[ -e $FAKE_STATE/active/$unit ]] && was_running=1
  touch "$FAKE_STATE/active/$unit"
  pid=$(pid_for_unit "$unit")
  mkdir -p "$FAKE_STATE/proc/$pid"
  uid_for_unit "$unit" >"$FAKE_STATE/proc/$pid/uid"
  if [[ $unit == persea-terminal-tailscaled.service ]]; then
    target="$PERSEA_DEPLOY_ROOT/usr/sbin/tailscaled"
  else
    target=$(readlink -f -- "$PERSEA_DEPLOY_ROOT/opt/persea-terminal/current/bin/persea-terminal")
  fi
  if [[ $action == restart || $was_running == 0 || ! -L $FAKE_STATE/proc/$pid/exe ]]; then ln -sfn -- "$target" "$FAKE_STATE/proc/$pid/exe"; fi
  if [[ $unit == persea-terminal-tailscaled.service ]]; then
    runtime="$PERSEA_DEPLOY_ROOT/run/persea-terminal-tailscale"
    state="$PERSEA_DEPLOY_ROOT/var/lib/persea-terminal-tailscale"
    socket="$runtime/tailscaled.sock"
    mkdir -m 0700 -p "$runtime" "$state"
    if [[ ! -S $socket ]]; then
      /usr/bin/python3 -c 'import os,select,socket,sys; p=sys.argv[1]; s=socket.socket(socket.AF_UNIX); s.bind(p); os.chmod(p,0o666); s.listen(8); [(lambda c:(c.close()))(s.accept()[0]) for _ in iter(lambda: select.select([s],[],[]), None)]' "$socket" </dev/null >/dev/null 2>&1 &
      server_pid=$!
      printf '%s\n' "$server_pid" >>"$FAKE_STATE/sidecar-server-pids"
      for _ in {1..100}; do [[ -S $socket ]] && break; sleep 0.01; done
    fi
  fi
  if [[ ${FAKE_SIGNAL_START_UNIT:-0} == "$unit" && ! -e $FAKE_STATE/signal-start-$unit ]]; then
    touch "$FAKE_STATE/signal-start-$unit"
    kill -s "${FAKE_SIGNAL_KIND:-TERM}" "$PPID"
  fi
}
deactivate_sidecar() {
  rm -f "$FAKE_STATE/active/persea-terminal-tailscaled.service"
  if [[ -f $FAKE_STATE/sidecar-server-pids ]]; then
    while IFS= read -r pid; do kill "$pid" 2>/dev/null || true; done <"$FAKE_STATE/sidecar-server-pids"
    : >"$FAKE_STATE/sidecar-server-pids"
  fi
  rm -f -- "$PERSEA_DEPLOY_ROOT/run/persea-terminal-tailscale/tailscaled.sock"
  rmdir -- "$PERSEA_DEPLOY_ROOT/run/persea-terminal-tailscale" 2>/dev/null || true
}
case ${1:-} in
  is-active)
    unit=${*: -1}
    [[ ${FAKE_INACTIVE:-0} != 1 && -e $FAKE_STATE/active/$unit ]]
    ;;
  is-enabled)
    unit=${*: -1}
    [[ -e $FAKE_STATE/enabled/$unit ]]
    ;;
  show)
    unit=$2
    if [[ $* == *'--property=FragmentPath'* ]]; then
      if [[ ${FAKE_FRAGMENT_DRIFT:-0} == 1 ]]; then
        printf '/wrong/%s\n' "$unit"
      else
        printf '%s/etc/systemd/system/%s\n' "$PERSEA_DEPLOY_ROOT" "$unit"
      fi
    elif [[ $* == *'--property=MainPID'* ]]; then
      if [[ -e $FAKE_STATE/active/$unit ]]; then pid_for_unit "$unit"; else printf '0\n'; fi
    elif [[ $* == *'--property=MemoryHigh'* ]]; then
      [[ $unit =~ ^persea-terminal-broker-(desk-a7|lab-k4|ops-m9)\.service$ ]] || exit 1
      [[ ${FAKE_UNIFIED_PROPERTY_DRIFT:-0} == high ]] && printf '1\n' || printf '891289600\n'
    elif [[ $* == *'--property=MemoryMax'* ]]; then
      [[ $unit =~ ^persea-terminal-broker-(desk-a7|lab-k4|ops-m9)\.service$ ]] || exit 1
      [[ ${FAKE_UNIFIED_PROPERTY_DRIFT:-0} == max ]] && printf '1\n' || printf '1073741824\n'
    elif [[ $* == *'--property=LimitCORE'* ]]; then
      [[ $unit =~ ^persea-terminal-broker-(desk-a7|lab-k4|ops-m9)\.service$ ]] || exit 1
      [[ ${FAKE_UNIFIED_PROPERTY_DRIFT:-0} == core ]] && printf 'infinity\n' || printf '0\n'
    else
      exit 1
    fi
    ;;
  start)
    shift
    for unit in "$@"; do [[ $unit == *.service ]] && activate_unit start "$unit"; done
    ;;
  restart)
    shift
    for unit in "$@"; do [[ $unit == *.service ]] && activate_unit restart "$unit"; done
    ;;
  enable)
    mkdir -p "$FAKE_STATE/enabled"; shift
    for unit in "$@"; do [[ $unit == *.service ]] && touch "$FAKE_STATE/enabled/$unit"; done
    ;;
  disable)
    shift
    for unit in "$@"; do [[ $unit == *.service ]] && rm -f "$FAKE_STATE/enabled/$unit"; done
    ;;
  stop)
    shift
    for unit in "$@"; do
      [[ $unit == *.service ]] && rm -f "$FAKE_STATE/active/$unit"
      if [[ $unit == persea-terminal-front.service && -e $FAKE_STATE/active/persea-terminal-tailscaled.service ]]; then deactivate_sidecar; fi
      if [[ $unit == persea-terminal-tailscaled.service ]]; then deactivate_sidecar; fi
      if [[ ${FAKE_SIGNAL_STOP_UNIT:-0} == "$unit" && ! -e $FAKE_STATE/signal-stop-$unit ]]; then
        touch "$FAKE_STATE/signal-stop-$unit"
        kill -s "${FAKE_SIGNAL_KIND:-TERM}" "$PPID"
      fi
    done
    ;;
  daemon-reload)
    if [[ -n ${FAKE_SIGNAL_DAEMON_RELOAD:-} && ! -e $FAKE_STATE/daemon-signal-sent ]]; then
      touch "$FAKE_STATE/daemon-signal-sent"
      kill -s "$FAKE_SIGNAL_DAEMON_RELOAD" "$PPID"
    fi
    ;;
esac
SH
cat >"$MOCK_BIN/persea-process-metadata" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
unit=$1 pid=$2 expected_binary=$3
uid=$(<"$FAKE_STATE/proc/$pid/uid")
identity=$(stat -Lc '%d:%i' -- "$FAKE_STATE/proc/$pid/exe")
[[ -f $expected_binary ]]
if [[ ${FAKE_PID_EXEC_DRIFT:-} == "$unit" ]]; then identity=0:0; fi
if [[ ${FAKE_PID_UID_DRIFT:-} == "$unit" ]]; then uid=999; fi
printf '%s:%s\n' "$uid" "$identity"
SH
cat >"$MOCK_BIN/persea-runtime-metadata" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
path=$1
case $path in
  */run/persea-terminal) value=$PT_FIXTURE_FRONT_UID:$PT_FIXTURE_GROUP_GID ;;
  */run/persea-terminal/front.sock) value=$PT_FIXTURE_FRONT_UID:$PT_FIXTURE_GROUP_GID; realm=front ;;
  */run/persea-terminal-desk-a7) value=$PT_FIXTURE_FRONT_UID:$PT_FIXTURE_GROUP_GID ;;
  */run/persea-terminal-desk-a7/broker.sock) value=$PT_FIXTURE_FRONT_UID:$PT_FIXTURE_GROUP_GID; realm=local ;;
  */run/persea-terminal-lab-k4) value=$PT_FIXTURE_REALM_UID:$PT_FIXTURE_GROUP_GID ;;
  */run/persea-terminal-lab-k4/broker.sock) value=$PT_FIXTURE_REALM_UID:$PT_FIXTURE_GROUP_GID; realm=remote ;;
  */run/persea-terminal-ops-m9) value=$PT_FIXTURE_FRONT_UID:$PT_FIXTURE_GROUP_GID ;;
  */run/persea-terminal-ops-m9/broker.sock) value=$PT_FIXTURE_FRONT_UID:$PT_FIXTURE_GROUP_GID; realm=ops ;;
  *) exit 1 ;;
esac
value=$value:$(stat -Lc '%a' -- "$path")
if [[ -n ${realm:-} && ${FAKE_SOCKET_DRIFT:-} == "$realm" ]]; then value=${value%:*}:666; fi
printf '%s\n' "$value"
SH
cat >"$MOCK_BIN/persea-unified-runtime-metadata" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
unit=$1 runtime_dir=$3
case $unit in
  persea-terminal-broker-desk-a7.service|persea-terminal-broker-ops-m9.service) uid=$PT_FIXTURE_FRONT_UID ;;
  persea-terminal-broker-lab-k4.service) uid=$PT_FIXTURE_REALM_UID ;;
  *) exit 1 ;;
esac
realm=${unit#persea-terminal-broker-}
realm=${realm%.service}
[[ $runtime_dir == /run/persea-terminal-$realm/unified-journal ]]
bytes=100663296
[[ ${FAKE_UNIFIED_RUNTIME_DRIFT:-0} == capacity ]] && bytes=1
printf '0:0:tmpfs:%s:%s:700:%s:256:rw,nosuid,nodev,noexec,relatime,size=98304k,nr_inodes=256,mode=700,uid=%s,gid=%s,noswap\n' \
  "$uid" "$PT_FIXTURE_GROUP_GID" "$bytes" "$uid" "$PT_FIXTURE_GROUP_GID"
SH
cat >"$MOCK_BIN/runuser" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[[ $1 == -u && -n $2 && $2 != root && $3 == -- ]]
shift 3
[[ $1 == python3 && $2 == */releases/*/libexec/probe-unix.py && -r $2 ]]
args=("$@")
for ((i = 0; i < ${#args[@]}; i++)); do
  if [[ ${args[$i]} == --socket ]]; then args[$((i + 1))]=$FAKE_DENIED_SOCKET; fi
done
exec "${args[@]}"
SH
cat >"$MOCK_BIN/tailscale" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
socket=
if [[ ${1:-} == --socket=* ]]; then socket=${1#--socket=}; shift; fi
main_socket="$PERSEA_DEPLOY_ROOT/run/tailscale/tailscaled.sock"
sidecar_socket="$PERSEA_DEPLOY_ROOT/run/persea-terminal-tailscale/tailscaled.sock"
case $socket in
  "$main_socket") endpoint=main ;;
  "$sidecar_socket") endpoint=sidecar ;;
  '') endpoint=default ;;
  *) printf 'unexpected fake Tailscale socket: %s\n' "$socket" >&2; exit 1 ;;
esac
printf 'CALL %s %s\n' "$endpoint" "$*" >>"$FAKE_STATE/tailscale-calls.log"
if [[ " $* " == *' --help '* ]]; then
  # Offline help, like the real CLI: no daemon is contacted. FAKE_TS_OMIT='<path>:<token>'
  # drops one flag/subcommand line from the help of one path ('' path = top level).
  help_args="$*"; help_path=${help_args%% --help*}; [[ $help_path != --help ]] || help_path=
  case $help_path in
    '') help=$'USAGE\n  tailscale [flags] <subcommand> [command flags]\n\nSUBCOMMANDS\n  up           Connect to Tailscale, logging in if needed\n  down         Disconnect from Tailscale\n  status       Show state of tailscaled and its connections\n  serve        Serve content and local servers on your tailnet\n  version      Print Tailscale version\n\nFLAGS\n  --socket value\n    \tpath to tailscaled socket (default /var/run/tailscale/tailscaled.sock)' ;;
    status) help=$'USAGE\n  tailscale status [--active] [--web] [--json]\n\nFLAGS\n  --active, --active=false\n    \tfilter output to only peers with active sessions (default false)\n  --json, --json=false\n    \toutput in JSON format (WARNING: format subject to change) (default false)' ;;
    serve) help=$'USAGE\n  tailscale serve status [--json]\n  tailscale serve --service=<svc-name> [flags] <target>\n\nSUBCOMMANDS\n  status      View current serve configuration\n  reset       Reset current serve config\n  drain       Drain a service from the current node\n  clear       Remove all config for a service\n\nFLAGS\n  --bg, --bg=false\n    \tRun the command as a background process\n  --http value\n    \tExpose an HTTP server at the specified port\n  --https value\n    \tExpose an HTTPS server at the specified port (default mode)\n  --service value\n    \tServe for a service with distinct virtual IP instead on node itself.\n  --yes, --yes=false\n    \tUpdate without interactive prompts (default false)' ;;
    'serve status') help=$'USAGE\n  tailscale serve status [--json]\n\nFLAGS\n  --json, --json=false\n    \toutput in JSON format (default false)' ;;
    debug) help=$'USAGE\n  tailscale debug <debug flags | subcommand>\n\nSUBCOMMANDS\n  prefs                  Print prefs\n  clear-netmap-cache     Remove and discard cached network maps (if any)' ;;
    up) help=$'USAGE\n  tailscale up [flags]\n\nFLAGS\n  --advertise-tags value\n    \tcomma-separated ACL tags to request\n  --hostname value\n    \thostname to use instead of the one provided by the OS\n  --json, --json=false\n    \toutput in JSON format (default false)\n  --reset, --reset=false\n    \treset unspecified settings to their default values (default false)' ;;
    *) printf 'fake tailscale: no help fixture for path: %s\n' "$help_path" >&2; exit 1 ;;
  esac
  omit_token=
  if [[ -n ${FAKE_TS_OMIT:-} && ${FAKE_TS_OMIT%%:*} == "$help_path" ]]; then omit_token=${FAKE_TS_OMIT#*:}; fi
  awk -v omit="$omit_token" '{ first=$1; sub(/[,=].*$/, "", first); if (omit != "" && first == omit) next; print }' <<<"$help"
  exit 0
fi
suffix=${FAKE_SUFFIX:-testing-f8.ts.net}
host="terminal.$suffix"
if [[ $endpoint == main || $endpoint == default ]]; then
  case "${1:-} ${2:-}" in
    version*)
      printf '%s\n  tailscale commit: 9329c3677031109ff6d0b80abee0cddc8f35ff6f\n  long version: %s-t9329c3677-ga522f65e9\n' "${FAKE_TS_VERSION:-1.102.3}" "${FAKE_TS_VERSION:-1.102.3}"
      ;;
    'serve status')
      drift=0
      if [[ ${FAKE_MAIN_DRIFT:-0} == 1 ]]; then
        count=0; [[ ! -f $FAKE_STATE/main-serve-count ]] || count=$(<"$FAKE_STATE/main-serve-count")
        count=$((count + 1)); printf '%s\n' "$count" >"$FAKE_STATE/main-serve-count"
        ((count >= ${FAKE_MAIN_DRIFT_AT:-2})) && drift=1
      fi
      if ((drift)); then printf '{"Main":"DRIFT"}\n'; else printf '{"Main":"main-preserved","Routes":["/browse-persea","/other"]}\n'; fi
      ;;
    'status --json')
      printf '{"BackendState":"Running","Self":{"DNSName":"console.testing-f8.ts.net.","Tags":null},"MagicDNSSuffix":"testing-f8.ts.net"}\n'
      ;;
    'debug prefs')
      printf '{"AdvertiseTags":null,"AdvertiseServices":[]}\n'
      ;;
    *)
      printf 'MAIN_MUTATE %s\n' "$*" >>"$FAKE_STATE/tailscale.log"
      exit 91
      ;;
  esac
  exit 0
fi
case "${1:-} ${2:-}" in
  version*)
    printf '%s\n  tailscale commit: 9329c3677031109ff6d0b80abee0cddc8f35ff6f\n  long version: %s-t9329c3677-ga522f65e9\n' "${FAKE_TS_VERSION:-1.102.3}" "${FAKE_TS_VERSION:-1.102.3}"
    ;;
  'status --json')
    if [[ ${FAKE_NO_TAG:-0} == 1 ]]; then
      tags='[]'
    elif [[ ${FAKE_EXTRA_TAG:-0} == 1 ]]; then
      tags='["tag:terminal-service","tag:extra"]'
    elif [[ ${FAKE_WRONG_TAG:-0} == 1 ]]; then
      tags='["tag:wrong"]'
    else
      tags='["tag:terminal-service"]'
    fi
    if [[ -e $FAKE_STATE/service-https && -e $FAKE_STATE/service-http && ${FAKE_SERVICE_MISSING:-0} != 1 ]]; then capmap='{"https://tailscale.com/cap/service-host":[{"svc:terminal":["100.100.100.100"]}]}'; else capmap='{}'; fi
    dns="service-host.$suffix"; [[ ${FAKE_PROTECTED_IDENTITY:-0} != 1 ]] || dns=console.testing-f8.ts.net
    printf '{"BackendState":"%s","Self":{"DNSName":"%s.","Tags":%s,"CapMap":%s},"MagicDNSSuffix":"%s","CurrentTailnet":{"MagicDNSSuffix":"%s"}}\n' "${FAKE_BACKEND_STATE:-Running}" "$dns" "$tags" "$capmap" "$suffix" "$suffix"
    ;;
  'debug prefs')
    if [[ -e $FAKE_STATE/service-https && -e $FAKE_STATE/service-http && ${FAKE_UNAPPROVED:-0} != 1 ]]; then services='["svc:terminal"]'; else services='[]'; fi
    printf '{"AdvertiseTags":["tag:terminal-service"],"AdvertiseServices":%s}\n' "$services"
    ;;
  'serve status')
    if [[ ${FAKE_REPLACE_EVIDENCE:-0} == 1 && ! -e $FAKE_STATE/evidence-replaced ]]; then
      evidence="$PERSEA_DEPLOY_ROOT/var/lib/persea-terminal-deployments"
      mv -- "$evidence" "$evidence.replaced"
      mkdir -m 0700 -- "$evidence"
      touch "$FAKE_STATE/evidence-replaced"
    fi
    if [[ -e $FAKE_STATE/service-https && -e $FAKE_STATE/service-http ]]; then
      target='unix:/run/persea-terminal/front.sock'
      [[ ${FAKE_BAD_READBACK:-0} != 1 ]] || target='tcp://127.0.0.1:9'
      printf '{"TCP":{"7373":{"HTTPS":true}},"Web":{"node.example:7373":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:7373"}}}},"Services":{"svc:terminal":{"TCP":{"80":{"HTTP":true},"443":{"HTTPS":true}},"Web":{"%s:80":{"Handlers":{"/":{"Proxy":"%s"}}},"%s:443":{"Handlers":{"/":{"Proxy":"%s"}}}}}}}\n' "$host" "$target" "$host" "$target"
    else
      printf '{"TCP":{"7373":{"HTTPS":true}},"Web":{"node.example:7373":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:7373"}}}}}\n'
    fi
    ;;
  'serve drain')
    printf 'MUTATE drain %s\n' "${3:-}" >>"$FAKE_STATE/tailscale.log"
    ;;
  'serve clear')
    printf 'MUTATE clear %s\n' "${3:-}" >>"$FAKE_STATE/tailscale.log"
    rm -f "$FAKE_STATE/service-active" "$FAKE_STATE/service-https" "$FAKE_STATE/service-http"
    ;;
  serve\ --service=*)
    printf 'MUTATE %s\n' "$*" >>"$FAKE_STATE/tailscale.log"
    [[ ${FAKE_SERVE_FAIL:-0} != 1 ]] || exit 1
    if [[ $* == *'--https=443'* ]]; then touch "$FAKE_STATE/service-https"; fi
    if [[ $* == *'--http=80'* ]]; then
      [[ ${FAKE_HTTP_SERVE_FAIL:-0} != 1 ]] || exit 1
      touch "$FAKE_STATE/service-http"
    fi
    if [[ -e $FAKE_STATE/service-https && -e $FAKE_STATE/service-http ]]; then touch "$FAKE_STATE/service-active"; fi
    if [[ -n ${FAKE_SIGNAL_AFTER_SERVE:-} ]]; then kill -s "$FAKE_SIGNAL_AFTER_SERVE" "$PPID"; fi
    ;;
  up\ --hostname=terminal-sidecar)
    [[ $* == 'up --hostname=terminal-sidecar --advertise-tags=tag:terminal-service' ]] || exit 1
    [[ ${FAKE_LOGIN_FAIL:-0} != 1 ]] || exit 1
    printf 'LOGIN %s\n' "$*" >>"$FAKE_STATE/tailscale.log"
    ;;
  *) printf 'unexpected fake tailscale invocation: %s\n' "$*" >&2; exit 1 ;;
esac
SH
chmod 0755 "$MOCK_BIN"/*

SCRUBBED_BIN="$TMP/scrubbed-root-bin"
mkdir -m 0700 "$SCRUBBED_BIN"
for command in tailscale systemctl systemd-analyze id getent; do
  cp -- "$MOCK_BIN/$command" "$SCRUBBED_BIN/$command"
done
chmod 0755 "$SCRUBBED_BIN"/*

new_root() {
  local root
  root=$(mktemp -d "$TMP/root.XXXXXXXX")
  mkdir -p "$root/opt" "$root/etc/systemd" "$root/etc/persea-terminal" "$root/var/lib/persea-terminal" "$root/run/tailscale" "$root/usr/sbin"
  chmod 0755 "$root/opt" "$root/etc" "$root/etc/systemd" "$root/etc/persea-terminal" "$root/var" "$root/var/lib" "$root/run"
  chmod 0700 "$root/var/lib/persea-terminal"
  python3 - "$root/etc/persea-terminal/host.json" <<'PY'
import json, os, sys
manifest = {
    "version": 1,
    "front": {"user": os.environ["PT_FIXTURE_FRONT_USER"], "uid": int(os.environ["PT_FIXTURE_FRONT_UID"]), "group": os.environ["PT_FIXTURE_GROUP"], "gid": int(os.environ["PT_FIXTURE_GROUP_GID"])},
    "ingress": {"canonical_host": "terminal.testing-f8.ts.net", "operator_login": os.environ["PT_FIXTURE_OPERATOR"], "max_connections": 64},
    "tailscale": {"service": "svc:terminal", "sidecar_hostname": "terminal-sidecar", "sidecar_tag": "tag:terminal-service", "tailnet_suffix": "testing-f8.ts.net", "protected_main_dns_name": "console.testing-f8.ts.net"},
    "realms": [
        {
            "id": "desk-a7",
            "display_name": "Operator Q7",
            "user": os.environ["PT_FIXTURE_FRONT_USER"],
            "uid": int(os.environ["PT_FIXTURE_FRONT_UID"]),
            "servers": [{"label": "default", "socket_name": "default"}],
            "session_create": {
                "enabled": True,
                "servers": ["default"],
                "name_pattern": "^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$",
                "max_sessions": 8,
                "start_directory": "/srv/example-project",
            },
            "unified_terminal_dev": {
                "enabled": True,
                "server": "default",
                "session": "unified-dev",
                "observer_session": "observer-main",
            },
        },
        {
            "id": "lab-k4", "display_name": "Automation K4",
            "user": os.environ["PT_FIXTURE_REALM_USER"], "uid": int(os.environ["PT_FIXTURE_REALM_UID"]),
            "servers": [{"label": "default", "socket_name": "default"}],
            "session_create": {
                "enabled": True, "servers": ["default"],
                "name_pattern": "^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$",
                "max_sessions": 8, "start_directory": "/srv/example-project",
            },
            "unified_terminal_dev": {
                "enabled": True, "server": "default", "session": "unified-dev",
                "observer_session": "observer-lab",
            },
        },
    ],
}
with open(sys.argv[1], "w", encoding="utf-8") as handle:
    json.dump(manifest, handle, separators=(",", ":"))
    handle.write("\n")
os.chmod(sys.argv[1], 0o600)
PY
  cat >"$root/usr/sbin/tailscaled" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
version=${FAKE_TSD_VERSION:-1.102.3}
case ${1:-} in
  --version)
    printf '%s\n  tailscale commit: 9329c3677031109ff6d0b80abee0cddc8f35ff6f\n  long version: %s-t9329c3677-ga522f65e9\n' "$version" "$version"
    ;;
  --help|-help|-h)
    # Go flag-package shape: usage on stderr, exit 0. FAKE_TSD_OMIT='<flag>' drops one
    # flag line; a non-flag token drops the description line that mentions it.
    help=$'Usage of tailscaled:\n  -port value\n    \tUDP port to listen on for WireGuard and peer-to-peer traffic; 0 means automatically select (default 0)\n  -socket string\n    \tpath of the service unix socket (default "/var/run/tailscale/tailscaled.sock")\n  -state string\n    \tabsolute path of state file; use \'mem:\' to not store state and register as an ephemeral node\n  -statedir string\n    \tpath to directory for storage of config state, TLS certs, temporary incoming Taildrop files, etc.\n  -tun string\n    \ttunnel interface name; use "userspace-networking" (beta) to not use TUN (default "tailscale0")\n  -verbose int\n    \tlog verbosity level; 0 is default, 1 or higher are increasingly verbose'
    awk -v omit="${FAKE_TSD_OMIT:-}" 'BEGIN { flag = (substr(omit, 1, 1) == "-") } { first=$1; sub(/[,=].*$/, "", first); if (omit != "" && flag && first == omit) next; if (omit != "" && !flag && index($0, omit)) next; print }' <<<"$help" >&2
    ;;
  *) printf 'fake tailscaled: unexpected invocation: %s\n' "$*" >&2; exit 1 ;;
esac
SH
  chmod 0755 "$root/usr/sbin/tailscaled"
  printf '%s\n' "$root"
}

hermetic_env=(
  "PATH=$MOCK_BIN:/usr/bin:/bin"
  "FAKE_STATE=$FAKE_STATE"
  "FAKE_DENIED_SOCKET=$DENIED_SOCKET"
  "PT_FIXTURE_FRONT_USER=$PT_FIXTURE_FRONT_USER"
  "PT_FIXTURE_REALM_USER=$PT_FIXTURE_REALM_USER"
  "PT_FIXTURE_GROUP=$PT_FIXTURE_GROUP"
  "PT_FIXTURE_FRONT_UID=$PT_FIXTURE_FRONT_UID"
  "PT_FIXTURE_REALM_UID=$PT_FIXTURE_REALM_UID"
  "PT_FIXTURE_GROUP_GID=$PT_FIXTURE_GROUP_GID"
  "PT_FIXTURE_OPERATOR=$PT_FIXTURE_OPERATOR"
  "PERSEA_DEPLOY_HERMETIC=1"
  "PERSEA_DEPLOY_MOCK_BIN=$MOCK_BIN"
  "PERSEA_TEST_EUID=0"
  "PERSEA_TEST_CLEAN=1"
)

build_root_snapshot() {
  find /tmp -maxdepth 1 -type d \( -name 'pt.????????' -o -name 'persea-terminal-install.????????' \) -printf '%f\n' 2>/dev/null | LC_ALL=C sort
}
build_roots_before=$(build_root_snapshot)

/usr/bin/python3 - "$DENIED_SOCKET" "$BYTE_SOCKET" <<'PY' &
import os, socket, sys
servers = []
for path in sys.argv[1:]:
    try: os.unlink(path)
    except FileNotFoundError: pass
    server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    server.bind(path)
    os.chmod(path, 0o600)
    server.listen(16)
    servers.append(server)
while True:
    readable, _, _ = __import__("select").select(servers, [], [])
    for server in readable:
        conn, _ = server.accept()
        try:
            conn.recv(8192)
            if server is servers[1]:
                conn.sendall(b"X")
        except OSError:
            pass
        conn.close()
PY
SERVER_PIDS+=("$!")
for _ in {1..100}; do [[ -S $DENIED_SOCKET && -S $BYTE_SOCKET ]] && break; sleep 0.02; done
[[ -S $DENIED_SOCKET && -S $BYTE_SOCKET ]] || fail 'denied-probe sockets did not start'

peer_probe_discriminator() {
  local helper=$1
  "$helper" --socket "$DENIED_SOCKET" --host terminal.testing-f8.ts.net --login "$PT_FIXTURE_OPERATOR" --expect-denied >/dev/null 2>&1 &&
    ! "$helper" --socket "$TMP/missing.sock" --host terminal.testing-f8.ts.net --login "$PT_FIXTURE_OPERATOR" --expect-denied >/dev/null 2>&1 &&
    ! "$helper" --socket "$BYTE_SOCKET" --host terminal.testing-f8.ts.net --login "$PT_FIXTURE_OPERATOR" --expect-denied >/dev/null 2>&1
}
peer_probe_discriminator "$DEPLOY_DIR/probe-unix.py" || fail 'strict zero-byte denied-probe baseline failed'
connect_mutant="$TMP/probe-connect-failure-success.py"
cp -- "$DEPLOY_DIR/probe-unix.py" "$connect_mutant"
sed -i '0,/return 1/s//return 0/' "$connect_mutant"
chmod 0755 "$connect_mutant"
if peer_probe_discriminator "$connect_mutant"; then fail 'connect-failure-as-success mutant survived'; fi
pass 'peer probe rejects connect-failure-as-success mutant'

byte_mutant="$TMP/probe-any-byte-success.py"
cp -- "$DEPLOY_DIR/probe-unix.py" "$byte_mutant"
sed -i 's/return 1  # Any byte disproves/return 0  # MUTANT: any byte accepted/' "$byte_mutant"
chmod 0755 "$byte_mutant"
if peer_probe_discriminator "$byte_mutant"; then fail 'any-byte-as-denial mutant survived'; fi
pass 'peer probe rejects any-byte-as-denial mutant'

zero_failure_mutant="$TMP/probe-zero-byte-failure.py"
cp -- "$DEPLOY_DIR/probe-unix.py" "$zero_failure_mutant"
sed -i 's/return 0  # Exact zero-byte/return 1  # MUTANT: exact zero-byte rejected/' "$zero_failure_mutant"
chmod 0755 "$zero_failure_mutant"
if peer_probe_discriminator "$zero_failure_mutant"; then fail 'zero-byte-as-failure mutant survived'; fi
pass 'peer probe accepts exact zero-byte EOF/reset and rejects zero-byte-as-failure mutant'

rootless_marker="$TMP/rootless-marker"
if "$DEPLOY_DIR/install.sh" >"$rootless_marker" 2>&1; then fail 'production rootless execution was accepted'; fi
[[ ! -e /opt/persea-terminal/current ]] || true
pass 'production invocation refuses rootless execution before writes'

expect_preflight_failure() {
  local label=$1; shift
  local root
  root=$(new_root)
  if env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$root" "$@" "$DEPLOY_DIR/install.sh" >"$TMP/failure.out" 2>&1; then
    fail "$label was accepted"
  fi
  [[ ! -e $root/opt/persea-terminal ]] || fail "$label wrote installation state"
  pass "$label fails before installation writes"
}

expect_preflight_failure 'wrong front UID' PT_ID_FRONT_OVERRIDE=42991
expect_preflight_failure 'wrong second-realm UID' PT_ID_REALM_OVERRIDE=42992
expect_preflight_failure 'wrong shared group GID' PT_GROUP_GID_OVERRIDE=42993
expect_preflight_failure 'below-floor Tailscale CLI' FAKE_TS_VERSION=1.102.1
grep -Fq 'tailscale CLI 1.102.1 is below the supported Tailscale floor 1.102.2' "$TMP/failure.out" || fail 'below-floor refusal did not name the floor'
expect_preflight_failure 'unparseable Tailscale CLI version' FAKE_TS_VERSION=unstable
grep -Fq 'tailscale CLI reports an unparseable version: unstable' "$TMP/failure.out" || fail 'unparseable-version refusal did not name the reported version'
expect_preflight_failure 'Tailscale CLI lacking serve --service' FAKE_TS_OMIT='serve:--service'
grep -Fq 'tailscale serve --help lacks required flag --service' "$TMP/failure.out" || fail 'missing-flag refusal did not name the flag'
expect_preflight_failure 'Tailscale CLI lacking serve status --json' FAKE_TS_OMIT='serve status:--json'
grep -Fq 'tailscale serve status --help lacks required flag --json' "$TMP/failure.out" || fail 'missing nested flag refusal did not name the flag'
expect_preflight_failure 'Tailscale CLI lacking the debug prefs subcommand' FAKE_TS_OMIT='debug:prefs'
grep -Fq 'tailscale debug --help lacks required subcommand prefs' "$TMP/failure.out" || fail 'missing-subcommand refusal did not name the subcommand'
expect_preflight_failure 'Tailscale CLI lacking the global --socket flag' FAKE_TS_OMIT=':--socket'
grep -Fq 'tailscale --help lacks required flag --socket' "$TMP/failure.out" || fail 'missing global flag refusal did not name the flag'

expect_preflight_success() {
  local label=$1; shift
  local root
  root=$(new_root)
  env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$root" "$@" "$DEPLOY_DIR/install.sh" >"$TMP/success.out" 2>&1 || fail "$label was refused: $(tail -n 1 "$TMP/success.out")"
  [[ -L $root/opt/persea-terminal/current ]] || fail "$label did not install a release"
  pass "$label passes the Tailscale floor and capability probe"
}

expect_preflight_success 'newer Tailscale patch release 1.102.9' FAKE_TS_VERSION=1.102.9
expect_preflight_success 'newer Tailscale minor release 1.104.0' FAKE_TS_VERSION=1.104.0
expect_preflight_success 'Tailscale pre-release build 1.105.1-dev20261001' FAKE_TS_VERSION=1.105.1-dev20261001

SCRUBBED_ROOT=$(new_root)
: >"$FAKE_STATE/build-uid.log"
rm -f -- "$FAKE_STATE/root-stage-plant" "$FAKE_STATE/builder-stage-plant"
ambient_tmp="$TMP/operator-controlled-tmp"
mkdir -m 0700 -- "$ambient_tmp"
env "${hermetic_env[@]}" \
  "PATH=$SCRUBBED_BIN:/usr/bin:/bin" \
  "TMPDIR=$ambient_tmp" \
  "PERSEA_DEPLOY_MOCK_BIN=$SCRUBBED_BIN" \
  "PERSEA_TEST_TOOLCHAIN_ROOT=$MOCK_BIN" \
  "FAKE_STAGE_PLANT=1" \
  "PERSEA_DEPLOY_ROOT=$SCRUBBED_ROOT" \
  "$DEPLOY_DIR/install.sh" >"$TMP/scrubbed-path-install.out"
[[ $(wc -l <"$FAKE_STATE/build-uid.log") == 8 ]] || fail 'scrubbed-PATH install did not run all eight npm/Go invocations'
! grep -q -v "real=$PT_FIXTURE_FRONT_UID effective=$PT_FIXTURE_FRONT_UID" "$FAKE_STATE/build-uid.log" || fail 'scrubbed-PATH install ran a toolchain process outside the configured build UID'
[[ $(grep -c '^npm ' "$FAKE_STATE/build-uid.log") == 5 && $(grep -c '^go ' "$FAKE_STATE/build-uid.log") == 3 ]] || fail 'scrubbed-PATH install toolchain ledger drifted'
pass 'scrubbed root PATH resolves an explicit toolchain and runs every npm/Go process as the configured build UID'

[[ $(grep -c ' LANG=C.UTF-8 LC_ALL=C.UTF-8 ' "$FAKE_STATE/build-uid.log") == 8 ]] || fail 'not all eight build invocations received both fixed UTF-8 locale variables'
mapfile -t build_tmpdirs < <(sed -n 's/^.* TMPDIR=\([^ ]*\) args=.*$/\1/p' "$FAKE_STATE/build-uid.log")
[[ ${#build_tmpdirs[@]} == 8 ]] || fail 'build TMPDIR ledger did not contain eight entries'
[[ $(printf '%s\n' "${build_tmpdirs[@]}" | sort -u | wc -l) == 1 ]] || fail 'build invocations did not share one private TMPDIR'
short_build_tmp=${build_tmpdirs[0]}
[[ $short_build_tmp == /tmp/pt.????????/w/t ]] || fail "build TMPDIR is not the fixed short private shape: $short_build_tmp"
[[ $short_build_tmp != "$ambient_tmp"* ]] || fail 'installer inherited the operator TMPDIR'
frozen_socket_suffix='/TestIngressSocketStateAndCleanupFailClosedreplacement_cleanup4294967295/001/front.sock'
[[ $((${#short_build_tmp} + ${#frozen_socket_suffix})) == 107 ]] || fail 'frozen worst-case Unix-socket path is not exactly within the 107-byte budget'
pass 'all eight build invocations receive fixed UTF-8 locale and one non-inherited 107-byte-safe private TMPDIR'

PATH_BUDGET_MUTANT="$DEPLOY_DIR/.install-old-build-root-mutant.$$"
cp -- "$DEPLOY_DIR/install.sh" "$PATH_BUDGET_MUTANT"
sed -i 's|mktemp -d /tmp/pt.XXXXXXXX|mktemp -d /tmp/persea-terminal-install.XXXXXXXX|' "$PATH_BUDGET_MUTANT"
# shellcheck disable=SC2016
grep -Fqx 'build_root=$(mktemp -d /tmp/persea-terminal-install.XXXXXXXX)' "$PATH_BUDGET_MUTANT" || fail 'old-build-root mutant rewrite did not apply'
path_mutant_root=$(new_root)
if env "${hermetic_env[@]}" "PERSEA_TEST_TOOLCHAIN_ROOT=$MOCK_BIN" "PERSEA_DEPLOY_ROOT=$path_mutant_root" \
  "$PATH_BUDGET_MUTANT" >"$TMP/path-budget-mutant.out" 2>&1; then
  fail 'old long build-root template mutant survived the path-budget witness'
fi
grep -Fq 'persea-terminal deploy: build TMPDIR exceeds Unix socket path budget:' "$TMP/path-budget-mutant.out" || fail 'old-build-root mutant lacked the positive path-budget witness'
[[ ! -e $path_mutant_root/opt/persea-terminal ]] || fail 'old-build-root mutant wrote installation state'
rm -f -- "$PATH_BUDGET_MUTANT"
PATH_BUDGET_MUTANT=
pass 'old long build-root template mutant fails the positive path-budget witness before installation'

locale_mutant_discriminator() {
  local label=$1 source=$2 witness=$3 root
  root=$(new_root)
  if env "${hermetic_env[@]}" "PERSEA_TEST_TOOLCHAIN_ROOT=$MOCK_BIN" "PERSEA_DEPLOY_ROOT=$root" \
    "$source" >"$TMP/$label-mutant.out" 2>&1; then
    fail "$label locale mutant survived the positive locale witness"
  fi
  grep -Fq "$witness" "$TMP/$label-mutant.out" || fail "$label locale mutant lacked its positive locale witness"
  [[ ! -e $root/opt/persea-terminal ]] || fail "$label locale mutant wrote installation state"
}

LANG_MUTANT="$DEPLOY_DIR/.install-missing-lang-mutant.$$"
cp -- "$DEPLOY_DIR/install.sh" "$LANG_MUTANT"
sed -i '/^    "LANG=C.UTF-8"$/d' "$LANG_MUTANT"
! grep -Fqx '    "LANG=C.UTF-8"' "$LANG_MUTANT" || fail 'LANG mutant rewrite did not apply'
locale_mutant_discriminator lang "$LANG_MUTANT" 'fake npm locale witness: LANG=<unset> LC_ALL=C.UTF-8 expected=C.UTF-8/C.UTF-8'
rm -f -- "$LANG_MUTANT"
LANG_MUTANT=
pass 'removing fixed LANG fails the positive locale witness before installation'

LC_ALL_MUTANT="$DEPLOY_DIR/.install-missing-lc-all-mutant.$$"
cp -- "$DEPLOY_DIR/install.sh" "$LC_ALL_MUTANT"
sed -i '/^    "LC_ALL=C.UTF-8"$/d' "$LC_ALL_MUTANT"
! grep -Fqx '    "LC_ALL=C.UTF-8"' "$LC_ALL_MUTANT" || fail 'LC_ALL mutant rewrite did not apply'
locale_mutant_discriminator lc-all "$LC_ALL_MUTANT" 'fake npm locale witness: LANG=C.UTF-8 LC_ALL=<unset> expected=C.UTF-8/C.UTF-8'
rm -f -- "$LC_ALL_MUTANT"
LC_ALL_MUTANT=
pass 'removing fixed LC_ALL fails the positive locale witness before installation'

grep -Fxq denied "$FAKE_STATE/root-stage-plant" || fail 'fake npm created the root-owned final stage during the build'
grep -Fxq created "$FAKE_STATE/builder-stage-plant" || fail 'stage-path plant setup did not create its builder-tree symlink'
STAGE_PATH_MUTANT="$DEPLOY_DIR/.install-stage-path-mutant.$$"
cp -- "$DEPLOY_DIR/install.sh" "$STAGE_PATH_MUTANT"
# These literals are the exact source text the mutant must replace and produce.
# shellcheck disable=SC2016
sed -i 's|stage="$build_root/stage"|stage="$build_workspace/stage"|' "$STAGE_PATH_MUTANT"
# shellcheck disable=SC2016
grep -Fqx 'stage="$build_workspace/stage"' "$STAGE_PATH_MUTANT" || fail 'stage-path mutant rewrite did not apply'
stage_mutant_root=$(new_root)
rm -f -- "$FAKE_STATE/root-stage-plant" "$FAKE_STATE/builder-stage-plant"
if env "${hermetic_env[@]}" \
  "PERSEA_TEST_TOOLCHAIN_ROOT=$MOCK_BIN" \
  "FAKE_STAGE_PLANT=1" \
  "PERSEA_DEPLOY_ROOT=$stage_mutant_root" \
  "$STAGE_PATH_MUTANT" >"$TMP/stage-path-mutant.out" 2>&1; then
  fail 'builder-owned final-stage mutant survived the path plant'
fi
grep -Fxq 'persea-terminal deploy: root stage target already exists' "$TMP/stage-path-mutant.out" || fail 'builder-owned final-stage mutant lacked the planted-stage witness'
[[ ! -e $stage_mutant_root/opt/persea-terminal ]] || fail 'builder-owned final-stage mutant wrote installation state'
rm -f -- "$STAGE_PATH_MUTANT"
STAGE_PATH_MUTANT=
pass 'root stage is unreachable during build and a builder-owned-stage mutant fails on the planted symlink'

ownership_boundary_discriminator() {
  local source=$1 gid before_meta before_hash after_meta after_hash result=0
  local workspace artifact stage
  OWNERSHIP_FIXTURE=$(mktemp -d "$TMP/ownership-model.XXXXXXXX")
  OWNERSHIP_SENTINEL=$(mktemp "$TMP/ownership-sentinel.XXXXXXXX")
  printf 'persea-terminal ownership boundary sentinel\n' >"$OWNERSHIP_SENTINEL"
  chmod 0640 -- "$OWNERSHIP_SENTINEL"
  [[ $(id -u) == "$PT_FIXTURE_FRONT_UID" ]] || return 1
  gid=$(id -g)
  before_meta=$(stat -Lc '%u:%g:%a:%s' -- "$OWNERSHIP_SENTINEL") || return 1
  before_hash=$(/usr/bin/sudo -n /usr/bin/sha256sum -- "$OWNERSHIP_SENTINEL") || return 1
  before_hash=${before_hash%% *}

  workspace="$OWNERSHIP_FIXTURE/workspace"
  artifact="$workspace/output/persea-terminal"
  stage="$OWNERSHIP_FIXTURE/stage"
  /usr/bin/sudo -n /bin/chown root:root -- "$OWNERSHIP_FIXTURE" || return 1
  /usr/bin/sudo -n /bin/chmod 0700 -- "$OWNERSHIP_FIXTURE" || return 1
  /usr/bin/sudo -n /usr/bin/install -d -o "$PT_FIXTURE_FRONT_UID" -g "$gid" -m 0700 "$workspace" "$workspace/output" || return 1
  /usr/bin/sudo -n /bin/chmod 0511 -- "$OWNERSHIP_FIXTURE" || return 1
  /usr/bin/sudo -n -u "#$PT_FIXTURE_FRONT_UID" /usr/bin/install -m 0755 /bin/true "$artifact" || return 1
  /usr/bin/sudo -n -u "#$PT_FIXTURE_FRONT_UID" /bin/ln -- "$OWNERSHIP_SENTINEL" "$workspace/out-of-tree-hardlink" || return 1

  /usr/bin/sudo -n /bin/chmod 0700 -- "$OWNERSHIP_FIXTURE" || return 1
  # shellcheck disable=SC2016
  if grep -Fq 'chown -R root:root -- "$build_workspace"' "$source"; then
    /usr/bin/sudo -n /bin/chown -R root:root -- "$workspace" || return 1
  fi
  /usr/bin/sudo -n /usr/bin/test -f "$artifact" || return 1
  /usr/bin/sudo -n /usr/bin/test ! -L "$artifact" || return 1
  [[ $(/usr/bin/sudo -n /usr/bin/stat -Lc '%h' -- "$artifact") == 1 ]] || return 1
  /usr/bin/sudo -n /usr/bin/install -d -o root -g root -m 0700 "$stage" "$stage/bin" || return 1
  /usr/bin/sudo -n /bin/cp -- "$artifact" "$stage/bin/persea-terminal" || return 1
  /usr/bin/sudo -n /bin/rm -rf -- "$OWNERSHIP_FIXTURE" || return 1
  OWNERSHIP_FIXTURE=

  after_meta=$(stat -Lc '%u:%g:%a:%s' -- "$OWNERSHIP_SENTINEL") || return 1
  after_hash=$(/usr/bin/sudo -n /usr/bin/sha256sum -- "$OWNERSHIP_SENTINEL") || return 1
  after_hash=${after_hash%% *}
  [[ $after_meta == "$before_meta" && $after_hash == "$before_hash" ]] || result=1
  /usr/bin/sudo -n /bin/rm -f -- "$OWNERSHIP_SENTINEL" || return 1
  OWNERSHIP_SENTINEL=
  return "$result"
}

ownership_boundary_discriminator "$DEPLOY_DIR/install.sh" || fail 'candidate changed the UID/GID/mode/content of an out-of-tree build-UID hardlink sentinel'
OWNERSHIP_MUTANT="$TMP/install-recursive-chown-mutant"
cp -- "$DEPLOY_DIR/install.sh" "$OWNERSHIP_MUTANT"
# shellcheck disable=SC2016
sed -i '/^chmod 0700 -- "$build_root"$/a\
if [[ $PERSEA_HERMETIC != 1 ]]; then\
  chown -R root:root -- "$build_workspace"\
fi' "$OWNERSHIP_MUTANT"
# shellcheck disable=SC2016
grep -Fq 'chown -R root:root -- "$build_workspace"' "$OWNERSHIP_MUTANT" || fail 'recursive-ownership mutant rewrite did not apply'
if ownership_boundary_discriminator "$OWNERSHIP_MUTANT"; then
  fail 'recursive root-ownership mutant preserved the out-of-tree hardlink sentinel'
fi
rm -f -- "$OWNERSHIP_MUTANT"
OWNERSHIP_MUTANT=
pass 'root imports validated artifacts without changing an out-of-tree hardlink sentinel and rejects recursive ownership mutation'

expect_toolchain_failure() {
  local label=$1 witness=$2 root
  shift 2
  root=$(new_root)
  if env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$root" "$@" "$DEPLOY_DIR/install.sh" >"$TMP/toolchain-failure.out" 2>&1; then
    fail "$label toolchain candidate was accepted"
  fi
  grep -Fq -- "$witness" "$TMP/toolchain-failure.out" || fail "$label toolchain failure lacked its exact witness"
  [[ ! -e $root/opt/persea-terminal ]] || fail "$label toolchain failure wrote installation state"
  pass "$label toolchain candidate fails before installation writes with an exact witness"
}

unexecutable_go="$MOCK_BIN/unexecutable-go"
printf '#!/usr/bin/env bash\nexit 0\n' >"$unexecutable_go"
chmod 0644 "$unexecutable_go"
escaped_node="$MOCK_BIN/escaped-node"
ln -s -- /bin/true "$escaped_node"
expect_toolchain_failure 'relative npm' 'npm tool candidate is not absolute: npm' PERSEA_TEST_NPM_CANDIDATE=npm
expect_toolchain_failure 'missing npm' 'npm tool candidate is missing or unexecutable' "PERSEA_TEST_NPM_CANDIDATE=$MOCK_BIN/missing-npm"
expect_toolchain_failure 'unexecutable Go' 'go tool candidate is missing or unexecutable' "PERSEA_TEST_GO_CANDIDATE=$unexecutable_go"
expect_toolchain_failure 'repository-resident npm' 'npm tool candidate is repository-resident' "PERSEA_TEST_NPM_CANDIDATE=$DEPLOY_DIR/install.sh"
expect_toolchain_failure 'symlink-escaped node' 'node tool candidate escapes its fixed prefix' "PERSEA_TEST_NODE_CANDIDATE=$escaped_node"

ROOT_UID_MUTANT=$(new_root)
/usr/bin/sudo -n /bin/chown -R root:root -- "$ROOT_UID_MUTANT"
root_uid_status=0
# The invoking UID intentionally owns the captured discriminator output.
# shellcheck disable=SC2024
/usr/bin/sudo -n /usr/bin/env "${hermetic_env[@]}" \
  "PERSEA_DEPLOY_ROOT=$ROOT_UID_MUTANT" \
  "$DEPLOY_DIR/install.sh" >"$TMP/root-uid-mutant.out" 2>&1 || root_uid_status=$?
root_uid_install_present=0
/usr/bin/sudo -n /usr/bin/test -e "$ROOT_UID_MUTANT/opt/persea-terminal" && root_uid_install_present=1
/usr/bin/sudo -n /bin/rm -rf -- "$ROOT_UID_MUTANT"
ROOT_UID_MUTANT=
[[ $root_uid_status == 97 ]] || fail "direct-root toolchain mutant returned $root_uid_status instead of 97"
grep -Fxq "persea-terminal deploy: build UID witness: real=0 effective=0 expected=$PT_FIXTURE_FRONT_UID" "$TMP/root-uid-mutant.out" || fail 'direct-root toolchain mutant lacked the 0/0 UID witness'
((root_uid_install_present == 0)) || fail 'direct-root toolchain mutant wrote installation state'
pass 'direct-root toolchain mutant fails before installation writes with real/effective UID 0 witness'

candidate_a="$TMP/candidate-a"
candidate_b="$TMP/candidate-b"
"$DEPLOY_DIR/generate-candidate.sh" --host-config "$DEPLOY_DIR/host.example.json" --output "$candidate_a" >/dev/null
"$DEPLOY_DIR/generate-candidate.sh" --host-config "$DEPLOY_DIR/host.example.json" --output "$candidate_b" >/dev/null
diff -ru "$candidate_a" "$candidate_b" >/dev/null || fail 'candidate generation is not deterministic'
pass 'candidate generation is deterministic'

/usr/bin/python3 - "$DEPLOY_DIR/../README.md" "$DEPLOY_DIR/host.example.json" <<'PY' || fail 'production README host-manifest example is invalid'
import json, sys
text = open(sys.argv[1], encoding="utf-8").read()
manifest = json.load(open(sys.argv[2], encoding="utf-8"))
assert "/etc/persea-terminal/host.json" in text
assert "deploy/host.example.json" in text
assert len(manifest["realms"]) == 2
assert manifest["front"]["user"] != manifest["realms"][1]["user"]
PY
pass 'production README routes operators through the strict portable host manifest'

FIRST_FAIL_ROOT=$(new_root)
mkdir -m 0755 "$FIRST_FAIL_ROOT/etc/systemd/system"
printf '[Unit]\nDescription=unrelated fixture\n' >"$FIRST_FAIL_ROOT/etc/systemd/system/unrelated.service"
rm -rf -- "$FAKE_STATE/active" "$FAKE_STATE/enabled" "$FAKE_STATE/proc"
if env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$FIRST_FAIL_ROOT" FAKE_FAIL_START=1 "$DEPLOY_DIR/install.sh" --activate-local >"$TMP/first-install-failure.out" 2>&1; then
  fail 'first-install activation failure was accepted'
fi
[[ ! -e $FIRST_FAIL_ROOT/opt/persea-terminal/current && ! -e $FIRST_FAIL_ROOT/opt/persea-terminal/previous ]] || fail 'first-install failure retained package pointers'
for unit in persea-terminal-broker-desk-a7.service persea-terminal-broker-lab-k4.service persea-terminal-front.service; do
  [[ ! -e $FIRST_FAIL_ROOT/etc/systemd/system/$unit ]] || fail "first-install failure retained $unit"
done
[[ -f $FIRST_FAIL_ROOT/etc/systemd/system/unrelated.service ]] || fail 'first-install failure removed an unrelated unit'
[[ ! -d $FAKE_STATE/enabled || -z $(find "$FAKE_STATE/enabled" -type f -print -quit) ]] || fail 'first-install failure left units enabled'
pass 'first-install-unit-removal discriminator removes only new units and package pointers'

ROOT=$(new_root)
env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" "$DEPLOY_DIR/install.sh" >"$TMP/install.out"
current_before=$(readlink -- "$ROOT/opt/persea-terminal/current")
[[ $current_before == releases/* ]] || fail 'current link is not release-relative'
env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" "$DEPLOY_DIR/verify.sh" >/dev/null
pass 'fresh hermetic install has exact configs, manifest, units, and bindings'
python3 - "$ROOT/opt/persea-terminal/current/config/front.json" <<'PY' || fail 'rendered front config does not place the preferences and snippet stores beside the alias store'
import json, os, sys
front = json.load(open(sys.argv[1], encoding="utf-8"))
home = os.path.dirname(front["alias_store_path"])
assert home == "/var/lib/persea-terminal", home
assert front["preferences_store_path"] == home + "/preferences.json", front
assert front["snippet_store_path"] == home + "/snippets.json", front
assert front["workspace_store_path"] == home + "/workspaces.json", front
assert front["keyboard_preferences_store_path"] == home + "/keyboard-v1.json", front
PY
pass 'rendered front config places the preferences and snippet stores beside the alias store'
[[ $(wc -l <"$ROOT/opt/persea-terminal/$current_before/MANIFEST.sha256") == 25 ]] || fail 'immutable release manifest does not cover the exact portable payload inventory'
# The installable shell is release payload, not a runtime download. Each
# asset must be a regular file whose bytes the release manifest covers.
for pwa_asset in ui/manifest.webmanifest ui/icon-192.png ui/icon-512.png ui/apple-touch-icon.png; do
  [[ -f "$ROOT/opt/persea-terminal/$current_before/$pwa_asset" && ! -L "$ROOT/opt/persea-terminal/$current_before/$pwa_asset" ]] ||
    fail "release is missing a regular $pwa_asset"
  grep -qE "^[0-9a-f]{64}  $pwa_asset\$" "$ROOT/opt/persea-terminal/$current_before/MANIFEST.sha256" ||
    fail "release manifest does not cover $pwa_asset"
done
pass 'release manifest covers the installable shell assets as regular files'
[[ $(find "$ROOT/opt/persea-terminal/$current_before/units" -maxdepth 1 -type f -name '*.service' | wc -l) == 4 ]] || fail 'immutable release does not contain four unit artifacts'
[[ ! -e $ROOT/etc/systemd/system/persea-terminal-tailscaled.service ]] || fail 'application install implicitly installed the sidecar unit'
[[ $(find "$ROOT/etc/systemd/system" -maxdepth 1 -type f -name 'persea-terminal-*.service' | wc -l) == 3 ]] || fail 'application install no longer controls exactly three units'
pass 'manifest contains four units while the application lifecycle installs exactly three'

release_path="$ROOT/opt/persea-terminal/$current_before"
chmod 0644 "$release_path/config/front.json"
if env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" "$DEPLOY_DIR/verify.sh" >"$TMP/writable-release.out" 2>&1; then fail 'writable release artifact was accepted'; fi
chmod 0444 "$release_path/config/front.json"
pass 'writable release artifact is rejected'

release_hardlink="$TMP/release-hardlink-q6"
ln -- "$release_path/config/front.json" "$release_hardlink"
if env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" "$DEPLOY_DIR/verify.sh" >"$TMP/hardlinked-release.out" 2>&1; then
  fail 'hardlinked immutable release artifact was accepted'
fi
grep -Fq 'release file has an unsafe link count' "$TMP/hardlinked-release.out" || fail 'hardlinked release rejection lacked its exact witness'
rm -f -- "$release_hardlink"
pass 'immutable release verification rejects out-of-tree hardlink aliases'

residue_unit="$ROOT/etc/systemd/system/persea-terminal-broker-residue-z9.service"
printf '[Unit]\nDescription=synthetic crash residue\n' >"$residue_unit"
chmod 0444 "$residue_unit"
if env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" "$DEPLOY_DIR/verify.sh" >"$TMP/untracked-unit-residue.out" 2>&1; then
  fail 'untracked package-unit residue was accepted'
fi
grep -Fq 'unexpected installed application unit: persea-terminal-broker-residue-z9.service' "$TMP/untracked-unit-residue.out" ||
  fail 'untracked package-unit rejection lacked its exact witness'
rm -f -- "$residue_unit"
pass 'exact installed-unit inventory rejects crash-residue broker units'

digest_before=$(find "$ROOT/opt/persea-terminal" "$ROOT/etc/systemd/system" -type f -print0 | sort -z | xargs -0 sha256sum | sha256sum)
env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" "$DEPLOY_DIR/install.sh" >/dev/null
digest_after=$(find "$ROOT/opt/persea-terminal" "$ROOT/etc/systemd/system" -type f -print0 | sort -z | xargs -0 sha256sum | sha256sum)
[[ $digest_before == "$digest_after" && $(readlink -- "$ROOT/opt/persea-terminal/current") == "$current_before" ]] || fail 'repeated install drifted'
pass 'repeated install is deterministic and idempotent'

# Each required public-release marker must fail closed before lifecycle writes.
for marker in config/host.json config/resolved-host.json config/managed-units MANIFEST.sha256; do
  chmod u+w -- "$(dirname -- "$release_path/$marker")"
  mv -- "$release_path/$marker" "$TMP/public-release-marker"
  : >"$FAKE_STATE/systemctl.log"
  if env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" "$DEPLOY_DIR/install.sh" >"$TMP/unsupported-release.out" 2>&1; then
    fail "unsupported release without $marker was accepted"
  fi
  grep -Fq 'install predates the first public release' "$TMP/unsupported-release.out" || fail 'unsupported layout lacked its refusal message'
  [[ $(readlink -- "$ROOT/opt/persea-terminal/current") == "$current_before" ]] || fail 'unsupported layout changed current'
  [[ ! -s $FAKE_STATE/systemctl.log ]] || fail 'unsupported layout reached lifecycle commands'
  mv -- "$TMP/public-release-marker" "$release_path/$marker"
  chmod 0555 -- "$(dirname -- "$release_path/$marker")"
done
pass 'unsupported release shapes are refused before lifecycle mutation'

python3 "$SCRIPT_DIR/release-retention-test.py" "$release_path" "$TMP"
pass 'release retention preserves protected releases and refuses ambiguous or unsafe trees'

# Isolate integration checks from the active-unit scenarios below.
RETENTION_ROOT=$(new_root)
RETENTION_STATE="$TMP/retention-state"
mkdir -m 0700 "$RETENTION_STATE"
retention_env=("${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$RETENTION_ROOT" "FAKE_STATE=$RETENTION_STATE")
retention_count() { find "$RETENTION_ROOT/opt/persea-terminal/releases" -mindepth 1 -maxdepth 1 -type d | wc -l; }
for number in 1 2 3 4 5 6; do
  printf -v retention_head '%040d' "$number"
  env "${retention_env[@]}" "PERSEA_TEST_HEAD=$retention_head" "$DEPLOY_DIR/install.sh" --no-prune >/dev/null
done
[[ $(retention_count) == 6 ]] || fail 'disable switch pruned releases'
env "${retention_env[@]}" "$DEPLOY_DIR/rollback.sh" --keep-releases 20 >/dev/null
[[ $(retention_count) == 6 ]] || fail 'limit above count removed releases'
env "${retention_env[@]}" "$DEPLOY_DIR/rollback.sh" --no-prune >/dev/null
[[ $(retention_count) == 6 ]] || fail 'rollback disable switch pruned releases'
env "${retention_env[@]}" "$DEPLOY_DIR/rollback.sh" >/dev/null
[[ $(retention_count) == 5 ]] || fail 'rollback did not apply default retention'
env "${retention_env[@]}" PERSEA_KEEP_RELEASES=2 "$DEPLOY_DIR/rollback.sh" --keep-releases 3 >/dev/null
[[ $(retention_count) == 3 ]] || fail 'rollback flag did not override environment'
env "${retention_env[@]}" PERSEA_KEEP_RELEASES=2 "$DEPLOY_DIR/install.sh" >/dev/null
[[ $(retention_count) == 2 ]] || fail 'install did not apply environment retention'
env "${retention_env[@]}" "$DEPLOY_DIR/verify.sh" >/dev/null
retention_previous=$(readlink -- "$RETENTION_ROOT/opt/persea-terminal/previous")
mkdir "$RETENTION_ROOT/opt/persea-terminal/releases/foreign"
env "${retention_env[@]}" "$DEPLOY_DIR/install.sh" --keep-releases 2 >"$TMP/retention-skip.out" 2>&1
grep -Fq 'warning: release retention:' "$TMP/retention-skip.out" || fail 'foreign entry did not warn'
[[ $(retention_count) == 3 ]] || fail 'foreign entry did not skip pruning'
[[ $(readlink -- "$RETENTION_ROOT/opt/persea-terminal/previous") == "$retention_previous" ]] || fail 'reinstall lost the rollback target'
rmdir "$RETENTION_ROOT/opt/persea-terminal/releases/foreign"
for invalid in 1 -1 02 text; do
  if env "${retention_env[@]}" "$DEPLOY_DIR/install.sh" --keep-releases "$invalid" >"$TMP/retention-invalid.out" 2>&1; then
    fail "invalid retention limit $invalid was accepted"
  fi
  grep -Fq 'keep-releases must be an integer' "$TMP/retention-invalid.out" || fail 'invalid limit lacked its witness'
done
pass 'install and rollback enforce retention limits, defaults, override and disable options'

for app_unit in persea-terminal-broker-desk-a7.service persea-terminal-broker-lab-k4.service persea-terminal-front.service; do
  ! grep -n -E '^PrivateTmp=' "$candidate_a/units/$app_unit" >/dev/null || fail 'application unit uses PrivateTmp'
  grep -Fxq 'RestrictAddressFamilies=AF_UNIX' "$candidate_a/units/$app_unit" || fail 'application unit address-family hardening drifted'
done
[[ $(grep -R -h -c '^RestrictAddressFamilies=AF_UNIX$' "$candidate_a/units" | awk '{s+=$1} END{print s+0}') == 3 ]] || fail 'unit address-family hardening drifted'
sidecar_unit="$candidate_a/units/persea-terminal-tailscaled.service"
grep -Fxq 'Requires=persea-terminal-front.service' "$sidecar_unit" || fail 'sidecar does not require the front unit'
grep -Fxq 'After=persea-terminal-front.service network-online.target' "$sidecar_unit" || fail 'sidecar ordering drifted'
grep -Fxq 'User=root' "$sidecar_unit" || fail 'sidecar is not root-owned at runtime'
grep -Fxq 'ExecStart=/usr/sbin/tailscaled --state=/var/lib/persea-terminal-tailscale/tailscaled.state --statedir=/var/lib/persea-terminal-tailscale --socket=/run/persea-terminal-tailscale/tailscaled.sock --tun=userspace-networking --port=0' "$sidecar_unit" || fail 'sidecar daemon boundary drifted'
grep -Fxq 'InaccessiblePaths=/run/tailscale /var/lib/tailscale' "$sidecar_unit" || fail 'sidecar can access protected main-daemon paths'
! rg -n -g '*.sh' 'tmux[[:space:]].*(list-sessions|list-windows|list-panes)|/tmp/tmux-[^/]*/\*' "$DEPLOY_DIR" >/dev/null || fail 'deployment discovers arbitrary tmux state'
! rg -n -g '*.sh' -g '!**/hermetic-deploy-test.sh' 'tailscale serve reset' "$DEPLOY_DIR" >/dev/null || fail 'global Tailscale reset is present'
[[ $(rg -n -g '*.sh' -g '!**/hermetic-deploy-test.sh' -F '"$cli" "--socket=$socket" "$@"' "$DEPLOY_DIR" | wc -l) == 1 ]] || fail 'explicit-socket command construction is not centralized exactly once'
! rg -n -g '*.sh' -g '!**/hermetic-deploy-test.sh' '(^|[;&|[:space:]()])tailscale[[:space:]]+(serve|status|debug|up|down|logout|version)([[:space:]]|$)|--reset([=[:space:]]|$)' "$DEPLOY_DIR" >/dev/null || fail 'production script contains a bare/default-socket or reset Tailscale operation'
pass 'unit hardening and explicit tmux/service scope are static invariants'

mkdir -m 0700 "$ROOT/run/persea-terminal" "$ROOT/run/persea-terminal-desk-a7"
mkdir -m 2710 "$ROOT/run/persea-terminal-lab-k4"
socket="$ROOT/run/persea-terminal/front.sock"
POSITIVE_COUNT="$TMP/positive-probe-count"
: >"$POSITIVE_COUNT"
/usr/bin/python3 - "$socket" "$POSITIVE_COUNT" <<'PY' &
import os, socket, sys
path = sys.argv[1]
count_path = sys.argv[2]
try: os.unlink(path)
except FileNotFoundError: pass
server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
server.bind(path)
os.chmod(path, 0o600)
server.listen(16)
while True:
    conn, _ = server.accept()
    with open(count_path, "a", encoding="ascii") as handle:
        handle.write("1\n")
    try:
        conn.recv(8192)
        conn.sendall(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}")
    except OSError:
        pass
    conn.close()
PY
SERVER_PIDS+=("$!")
for _ in {1..100}; do [[ -S $socket ]] && break; sleep 0.02; done
[[ -S $socket && $(stat -Lc '%u:%a' "$socket") == "$PT_FIXTURE_FRONT_UID:600" ]] || fail 'fake protected front socket did not start'

/usr/bin/python3 - "$ROOT/run/persea-terminal-desk-a7/broker.sock" "$ROOT/run/persea-terminal-lab-k4/broker.sock" <<'PY'
import os, socket, sys
for index, path in enumerate(sys.argv[1:]):
    try: os.unlink(path)
    except FileNotFoundError: pass
    server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    server.bind(path)
    os.chmod(path, 0o600 if index == 0 else 0o660)
    server.close()
PY

rm -rf -- "$FAKE_STATE/active" "$FAKE_STATE/enabled" "$FAKE_STATE/proc"
if env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" PERSEA_TEST_HEAD=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb FAKE_FAIL_START=1 "$DEPLOY_DIR/install.sh" --activate-local >"$TMP/failed-install.out" 2>&1; then
  fail 'injected activation failure was accepted'
fi
[[ $(readlink -- "$ROOT/opt/persea-terminal/current") == "$current_before" ]] || fail 'failed install replaced current'
[[ ! -d $FAKE_STATE/enabled || -z $(find "$FAKE_STATE/enabled" -type f -print -quit) ]] || fail 'failed install left units enabled'
pass 'failed activated install restores current and exact prior inactive/enabled state'

env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" "$DEPLOY_DIR/install.sh" --activate-local >/dev/null
env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" "$DEPLOY_DIR/verify.sh" --require-active >/dev/null
pass 'explicit local activation starts all three units with exact runtime readback'

active_current_before=$(readlink -- "$ROOT/opt/persea-terminal/current")
active_desk_exe_before=$(stat -Lc '%d:%i' "$FAKE_STATE/proc/1101/exe")
if env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" PERSEA_TEST_HEAD=ffffffffffffffffffffffffffffffffffffffff \
  "$DEPLOY_DIR/install.sh" >"$TMP/nonactivating-active-install.out" 2>&1; then
  fail 'non-activating install over active managed units was accepted'
fi
grep -Fq 'managed units are active or enabled; rerun with --activate-local' "$TMP/nonactivating-active-install.out" ||
  fail 'non-activating active-install rejection lacked its exact witness'
[[ $(readlink -- "$ROOT/opt/persea-terminal/current") == "$active_current_before" ]] || fail 'non-activating active-install rejection changed current'
[[ $(stat -Lc '%d:%i' "$FAKE_STATE/proc/1101/exe") == "$active_desk_exe_before" ]] || fail 'non-activating active-install rejection changed a broker process'
[[ -z $(find "$ROOT/opt/persea-terminal/releases" -maxdepth 1 -type d -name 'ffffffffffffffffffffffffffffffffffffffff-*' -print -quit) ]] ||
  fail 'non-activating active-install rejection imported a release before refusing'
pass 'active or enabled deployments require explicit activation before every install write'

: >"$FAKE_STATE/systemctl.log"
desk_broker_exe_before=$(stat -Lc '%d:%i' "$FAKE_STATE/proc/1101/exe")
lab_broker_exe_before=$(stat -Lc '%d:%i' "$FAKE_STATE/proc/1102/exe")
env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" PERSEA_TEST_HEAD=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb "$DEPLOY_DIR/install.sh" --activate-local >/dev/null
upgraded_target=$(readlink -- "$ROOT/opt/persea-terminal/current")
[[ $upgraded_target == releases/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-* ]] || fail 'active upgrade did not select the new release'
mapfile -t restart_lines < <(grep '^restart persea-terminal-' "$FAKE_STATE/systemctl.log")
[[ ${#restart_lines[@]} == 1 && ${restart_lines[0]} == 'restart persea-terminal-front.service' ]] || fail 'manifest-equivalent upgrade restarted an unchanged broker'
[[ $(stat -Lc '%d:%i' "$FAKE_STATE/proc/1101/exe") == "$desk_broker_exe_before" ]] || fail 'first unchanged broker executable identity drifted'
[[ $(stat -Lc '%d:%i' "$FAKE_STATE/proc/1102/exe") == "$lab_broker_exe_before" ]] || fail 'second unchanged broker executable identity drifted'
pass 'active upgrade preserves unchanged broker PIDs and restarts only the front'

variant_unit=persea-terminal-front.service
upgraded_release="$ROOT/opt/persea-terminal/$upgraded_target"
upgraded_unit_hash=$(sha256sum "$upgraded_release/units/$variant_unit" | awk '{print $1}')

if env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" PERSEA_TEST_HEAD=eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee FAKE_PID_EXEC_DRIFT=persea-terminal-front.service "$DEPLOY_DIR/install.sh" --activate-local >"$TMP/versioned-unit-failure.out" 2>&1; then
  fail 'versioned-unit activated-install failure was accepted'
fi
[[ $(readlink -- "$ROOT/opt/persea-terminal/current") == "$upgraded_target" ]] || fail 'versioned-unit activated-install failure did not restore current'
[[ $(sha256sum "$ROOT/etc/systemd/system/$variant_unit" | awk '{print $1}') == "$upgraded_unit_hash" ]] || fail 'versioned-unit activated-install failure did not restore old unit bytes'
env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" "$DEPLOY_DIR/verify.sh" --require-active >/dev/null
pass 'failed activated install restores exact prior immutable unit bytes'

rm -f "$FAKE_STATE/daemon-signal-sent"
interrupted_status=0
env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" PERSEA_TEST_HEAD=dddddddddddddddddddddddddddddddddddddddd FAKE_SIGNAL_DAEMON_RELOAD=TERM "$DEPLOY_DIR/install.sh" --activate-local >"$TMP/interrupted-install.out" 2>&1 || interrupted_status=$?
[[ $interrupted_status == 143 ]] || fail "interrupted activated install returned $interrupted_status instead of 143"
[[ $(readlink -- "$ROOT/opt/persea-terminal/current") == "$upgraded_target" ]] || fail 'interrupted activated install did not restore current'
[[ $(sha256sum "$ROOT/etc/systemd/system/$variant_unit" | awk '{print $1}') == "$upgraded_unit_hash" ]] || fail 'interrupted activated install did not restore versioned unit bytes'
for unit in persea-terminal-broker-desk-a7.service persea-terminal-broker-lab-k4.service persea-terminal-front.service; do
  [[ -e $FAKE_STATE/active/$unit && -e $FAKE_STATE/enabled/$unit ]] || fail "interrupted activated install did not restore prior state for $unit"
done
env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" "$DEPLOY_DIR/verify.sh" --require-active >/dev/null
pass 'signal-interrupted activated install restores pointers, versioned units, and prior active/enabled state'

expect_active_verify_failure() {
  local label=$1; shift
  if env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" "$@" "$DEPLOY_DIR/verify.sh" --require-active >"$TMP/verify-drift.out" 2>&1; then
    fail "$label drift was accepted"
  fi
  pass "$label drift is rejected by exact active verification"
}
expect_active_verify_failure 'unit fragment' FAKE_FRAGMENT_DRIFT=1
expect_active_verify_failure 'MainPID executable' FAKE_PID_EXEC_DRIFT=persea-terminal-front.service
expect_active_verify_failure 'front socket boundary' FAKE_SOCKET_DRIFT=front
expect_active_verify_failure 'local broker socket boundary' FAKE_SOCKET_DRIFT=local
expect_active_verify_failure 'cross-UID broker socket boundary' FAKE_SOCKET_DRIFT=remote
expect_active_verify_failure 'unified broker cgroup property' FAKE_UNIFIED_PROPERTY_DRIFT=high
expect_active_verify_failure 'unified broker hard core property' FAKE_UNIFIED_PROPERTY_DRIFT=core
expect_active_verify_failure 'unified journal mount capacity' FAKE_UNIFIED_RUNTIME_DRIFT=capacity

if env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" PERSEA_TEST_HEAD=cccccccccccccccccccccccccccccccccccccccc PERSEA_TEST_START_ONLY_MUTANT=1 "$DEPLOY_DIR/install.sh" --activate-local >"$TMP/start-only-mutant.out" 2>&1; then
  fail 'active-upgrade start-only mutant survived'
fi
[[ $(readlink -- "$ROOT/opt/persea-terminal/current") == "$upgraded_target" ]] || fail 'start-only mutant did not restore the prior current release'
env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" "$DEPLOY_DIR/verify.sh" --require-active >/dev/null
pass 'active-upgrade start-only mutant fails and restores prior active release'

rollback_release="$ROOT/opt/persea-terminal/$current_before"
rollback_unit_hash=$(sha256sum "$rollback_release/units/$variant_unit" | awk '{print $1}')
env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" "$DEPLOY_DIR/rollback.sh" --activate-local >/dev/null
[[ $(readlink -- "$ROOT/opt/persea-terminal/current") == "$current_before" ]] || fail 'explicit rollback did not switch to the target release'
[[ $(sha256sum "$ROOT/etc/systemd/system/$variant_unit" | awk '{print $1}') == "$rollback_unit_hash" ]] || fail 'explicit rollback did not install target release unit bytes'
[[ $(readlink -- "$ROOT/opt/persea-terminal/previous") == "$upgraded_target" ]] || fail 'explicit rollback did not preserve the displaced release'
env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" "$DEPLOY_DIR/verify.sh" --require-active >/dev/null
pass 'explicit rollback installs the exact immutable target unit bytes'

topology_host="$ROOT/etc/persea-terminal/host.json"
topology_original="$TMP/topology-original-host.json"
cp -- "$topology_host" "$topology_original"
desk_before=$(stat -Lc '%d:%i' "$FAKE_STATE/proc/1101/exe")
lab_before=$(stat -Lc '%d:%i' "$FAKE_STATE/proc/1102/exe")

python3 - "$topology_host" reorder <<'PY'
import json, sys
path, action = sys.argv[1:]
value = json.load(open(path, encoding="utf-8"))
if action == "reorder":
    value["realms"].reverse()
with open(path, "w", encoding="utf-8") as handle:
    json.dump(value, handle, separators=(",", ":"))
    handle.write("\n")
PY
: >"$FAKE_STATE/systemctl.log"
env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" PERSEA_TEST_HEAD=1111111111111111111111111111111111111111 "$DEPLOY_DIR/install.sh" --activate-local >/dev/null
! grep -Eq '^(start|restart) persea-terminal-broker-' "$FAKE_STATE/systemctl.log" || fail 'realm reorder restarted an unchanged broker'
grep -Fxq 'restart persea-terminal-front.service' "$FAKE_STATE/systemctl.log" || fail 'realm reorder did not restart the front'
[[ $(stat -Lc '%d:%i' "$FAKE_STATE/proc/1101/exe") == "$desk_before" && $(stat -Lc '%d:%i' "$FAKE_STATE/proc/1102/exe") == "$lab_before" ]] || fail 'realm reorder changed a broker executable identity'
python3 - "$ROOT/opt/persea-terminal/current/config/front.json" <<'PY'
import json, sys
front = json.load(open(sys.argv[1], encoding="utf-8"))
assert [realm["name"] for realm in front["realms"]] == ["lab-k4", "desk-a7"]
PY
pass 'realm reorder restarts only the front and preserves both broker PIDs'

python3 - "$topology_host" <<'PY'
import json, sys
path = sys.argv[1]
value = json.load(open(path, encoding="utf-8"))
next(realm for realm in value["realms"] if realm["id"] == "desk-a7")["display_name"] = "Operator Q7 renamed"
with open(path, "w", encoding="utf-8") as handle:
    json.dump(value, handle, separators=(",", ":"))
    handle.write("\n")
PY
: >"$FAKE_STATE/systemctl.log"
env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" PERSEA_TEST_HEAD=2222222222222222222222222222222222222222 "$DEPLOY_DIR/install.sh" --activate-local >/dev/null
! grep -Eq '^(start|restart) persea-terminal-broker-' "$FAKE_STATE/systemctl.log" || fail 'display-only change restarted an unchanged broker'
grep -Fxq 'restart persea-terminal-front.service' "$FAKE_STATE/systemctl.log" || fail 'display-only change did not restart the front'
[[ $(stat -Lc '%d:%i' "$FAKE_STATE/proc/1101/exe") == "$desk_before" && $(stat -Lc '%d:%i' "$FAKE_STATE/proc/1102/exe") == "$lab_before" ]] || fail 'display-only change altered a broker executable identity'
pass 'display-only change restarts only the front and preserves both broker PIDs'

python3 - "$topology_host" <<'PY'
import json, sys
path = sys.argv[1]
value = json.load(open(path, encoding="utf-8"))
next(realm for realm in value["realms"] if realm["id"] == "desk-a7")["servers"] = [{"label": "default", "socket_name": "alternate-a8"}]
with open(path, "w", encoding="utf-8") as handle:
    json.dump(value, handle, separators=(",", ":"))
    handle.write("\n")
PY
: >"$FAKE_STATE/systemctl.log"
env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" PERSEA_TEST_HEAD=3333333333333333333333333333333333333333 "$DEPLOY_DIR/install.sh" --activate-local >/dev/null
grep -Fxq 'restart persea-terminal-broker-desk-a7.service' "$FAKE_STATE/systemctl.log" || fail 'changed realm broker was not restarted'
! grep -Eq '^(start|restart) persea-terminal-broker-lab-k4.service$' "$FAKE_STATE/systemctl.log" || fail 'unchanged realm broker was restarted'
grep -Fxq 'restart persea-terminal-front.service' "$FAKE_STATE/systemctl.log" || fail 'one-realm change did not restart the front'
[[ $(stat -Lc '%d:%i' "$FAKE_STATE/proc/1102/exe") == "$lab_before" ]] || fail 'one-realm change altered the other broker executable identity'
pass 'one-realm selector change restarts only that broker and the front'

ops_runtime="$ROOT/run/persea-terminal-ops-m9"
ops_broker_socket="$ops_runtime/broker.sock"
ops_tmux_socket="$TMP/tmux-ops-m9.sock"
mkdir -m 0700 -- "$ops_runtime"
python3 - "$ops_broker_socket" "$ops_tmux_socket" <<'PY' &
import os, select, socket, sys
servers = []
for path in sys.argv[1:]:
    server = socket.socket(socket.AF_UNIX)
    server.bind(path)
    os.chmod(path, 0o600)
    server.listen(8)
    servers.append(server)
while True:
    readable, _, _ = select.select(servers, [], [])
    for server in readable:
        server.accept()[0].close()
PY
SERVER_PIDS+=("$!")
for _ in {1..100}; do [[ -S $ops_broker_socket && -S $ops_tmux_socket ]] && break; sleep 0.01; done
[[ -S $ops_broker_socket && -S $ops_tmux_socket ]] || fail 'add-realm socket sentinels did not start'
ops_tmux_inode=$(stat -Lc '%d:%i' "$ops_tmux_socket")
python3 - "$topology_host" "$ops_tmux_socket" <<'PY'
import json, os, sys
path, tmux_socket = sys.argv[1:]
value = json.load(open(path, encoding="utf-8"))
value["realms"].append({
    "id": "ops-m9",
    "display_name": "Operations M9",
    "user": os.environ["PT_FIXTURE_FRONT_USER"],
    "uid": int(os.environ["PT_FIXTURE_FRONT_UID"]),
    "servers": [{"label": "isolated", "socket_path": tmux_socket}],
    "session_create": {
        "enabled": True, "servers": ["isolated"],
        "name_pattern": "^[a-z][a-z0-9-]{0,20}$",
        "max_sessions": 8, "start_directory": "/srv/example-project",
    },
    "unified_terminal_dev": {
        "enabled": True, "server": "isolated", "session": "unified-dev",
        "observer_session": "observer-ops",
    },
})
with open(path, "w", encoding="utf-8") as handle:
    json.dump(value, handle, separators=(",", ":"))
    handle.write("\n")
PY
: >"$FAKE_STATE/systemctl.log"
env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" PERSEA_TEST_HEAD=4444444444444444444444444444444444444444 "$DEPLOY_DIR/install.sh" --activate-local >/dev/null
grep -Fxq 'start persea-terminal-broker-ops-m9.service' "$FAKE_STATE/systemctl.log" || fail 'added realm broker was not started'
! grep -Eq '^(start|restart) persea-terminal-broker-(desk-a7|lab-k4).service$' "$FAKE_STATE/systemctl.log" || fail 'adding a realm restarted an existing broker'
grep -Fxq 'restart persea-terminal-front.service' "$FAKE_STATE/systemctl.log" || fail 'adding a realm did not restart the front'
[[ -f $ROOT/etc/systemd/system/persea-terminal-broker-ops-m9.service ]] || fail 'added realm unit was not installed'
[[ $(stat -Lc '%d:%i' "$ops_tmux_socket") == "$ops_tmux_inode" ]] || fail 'adding a realm changed its configured tmux socket'
env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" "$DEPLOY_DIR/verify.sh" --require-active >/dev/null
pass 'adding a realm starts only the new broker and restarts the front'

cp -- "$topology_original" "$topology_host"
chmod 0600 "$topology_host"
: >"$FAKE_STATE/systemctl.log"
env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" PERSEA_TEST_HEAD=5555555555555555555555555555555555555555 "$DEPLOY_DIR/install.sh" --activate-local >/dev/null
grep -Fxq 'stop persea-terminal-front.service' "$FAKE_STATE/systemctl.log" || fail 'realm removal did not stop the front first'
grep -Fxq 'stop persea-terminal-broker-ops-m9.service' "$FAKE_STATE/systemctl.log" || fail 'realm removal did not stop the removed broker'
[[ ! -e $ROOT/etc/systemd/system/persea-terminal-broker-ops-m9.service ]] || fail 'removed realm unit remains installed'
[[ -S $ops_tmux_socket && $(stat -Lc '%d:%i' "$ops_tmux_socket") == "$ops_tmux_inode" ]] || fail 'realm removal touched the configured tmux socket'
env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" "$DEPLOY_DIR/verify.sh" --require-active >/dev/null
topology_current=$(readlink -- "$ROOT/opt/persea-terminal/current")
pass 'removing a realm cleans only its broker unit and preserves the tmux selector'

: >"$FAKE_STATE/tailscale.log"
: >"$FAKE_STATE/tailscale-calls.log"
env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" FAKE_BACKEND_STATE=NeedsLogin "$DEPLOY_DIR/install-tailscale-sidecar.sh" >/dev/null
env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" "$DEPLOY_DIR/verify.sh" --require-sidecar >/dev/null
[[ -f $ROOT/etc/systemd/system/persea-terminal-tailscaled.service && -e $FAKE_STATE/active/persea-terminal-tailscaled.service && -e $FAKE_STATE/enabled/persea-terminal-tailscaled.service ]] || fail 'sidecar install did not install, start, and enable only its exact unit'
cmp -s "$ROOT/opt/persea-terminal/$topology_current/units/persea-terminal-tailscaled.service" "$ROOT/etc/systemd/system/persea-terminal-tailscaled.service" || fail 'installed sidecar unit differs from current immutable release'
[[ $(find "$ROOT/etc/systemd/system" -maxdepth 1 -type f -name 'persea-terminal-*.service' | wc -l) == 4 ]] || fail 'explicit sidecar install did not produce exactly four package units'
! grep -q '^MAIN_MUTATE ' "$FAKE_STATE/tailscale.log" || fail 'sidecar install reached a main-daemon mutation sentinel'
pass 'explicit sidecar installer preserves the three-unit application lifecycle and verifies NeedsLogin-capable runtime'

# rollback..F5D: rollback owns the reverse-dependent sidecar lifecycle on
# success, restoration, signals, and terminal sidecar failures.
f5_source=$(readlink -- "$ROOT/opt/persea-terminal/current")
f5_target=$(readlink -- "$ROOT/opt/persea-terminal/previous")
f5_source_front_hash=$(sha256sum "$ROOT/etc/systemd/system/persea-terminal-front.service" | awk '{print $1}')

assert_f5_source_restored() {
  [[ $(readlink -- "$ROOT/opt/persea-terminal/current") == "$f5_source" ]] || fail 'rollback restoration changed the source pointer'
  [[ $(sha256sum "$ROOT/etc/systemd/system/persea-terminal-front.service" | awk '{print $1}') == "$f5_source_front_hash" ]] || fail 'rollback restoration changed source unit bytes'
  [[ -e $FAKE_STATE/active/persea-terminal-tailscaled.service && -e $FAKE_STATE/enabled/persea-terminal-tailscaled.service ]] || fail 'rollback restoration lost sidecar active/enabled intent'
  env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" "$DEPLOY_DIR/verify.sh" --require-active --require-sidecar >/dev/null || fail 'rollback restored source failed sidecar-required verification'
}

assert_f5_start_order() {
  local log=$1 first_broker last_broker front sidecar
  first_broker=$(grep -n '^start persea-terminal-broker-' "$log" | head -1 | cut -d: -f1)
  last_broker=$(grep -n '^start persea-terminal-broker-' "$log" | tail -1 | cut -d: -f1)
  front=$(grep -n '^start persea-terminal-front.service$' "$log" | tail -1 | cut -d: -f1)
  sidecar=$(grep -n '^start persea-terminal-tailscaled.service$' "$log" | tail -1 | cut -d: -f1)
  [[ -n $first_broker && -n $last_broker && -n $front && -n $sidecar && $last_broker -lt $front && $front -lt $sidecar ]] ||
    fail "rollback start order is not brokers -> front -> sidecar: $(tr '\n' ';' <"$log")"
}

: >"$FAKE_STATE/systemctl.log"
env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" "$DEPLOY_DIR/rollback.sh" --activate-local >"$TMP/rollback-normal.out"
[[ $(readlink -- "$ROOT/opt/persea-terminal/current") == "$f5_target" ]] || fail 'rollback did not select rollback target'
[[ -e $FAKE_STATE/active/persea-terminal-tailscaled.service && -e $FAKE_STATE/enabled/persea-terminal-tailscaled.service ]] || fail 'rollback did not restore sidecar lifecycle'
assert_f5_start_order "$FAKE_STATE/systemctl.log"
env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" "$DEPLOY_DIR/verify.sh" --require-active --require-sidecar >/dev/null || fail 'rollback final verification failed'
grep -Fq "CURRENT=$f5_target" "$TMP/rollback-normal.out" || fail 'rollback omitted its verified success receipt'
env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" "$DEPLOY_DIR/rollback.sh" --to-release "$f5_source" --activate-local >/dev/null
assert_f5_source_restored
pass 'rollback normal rollback restores sidecar after front and verifies it'

rm -f -- "$FAKE_STATE/fail-start-once-persea-terminal-front.service"
: >"$FAKE_STATE/systemctl.log"
if env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" FAKE_FAIL_START_ONCE=persea-terminal-front.service \
  "$DEPLOY_DIR/rollback.sh" --activate-local >"$TMP/rollback-error.out" 2>&1; then
  fail 'rollback accepted an injected post-stop activation failure'
fi
assert_f5_source_restored
assert_f5_start_order "$FAKE_STATE/systemctl.log"
! grep -q '^CURRENT=' "$TMP/rollback-error.out" || fail 'rollback printed a success receipt after restoration'
pass 'rollback error restoration restores pointers, units, front, and sidecar intent'

run_f5_signal() {
  local label=$1 expected=$2; shift 2
  rm -f -- "$FAKE_STATE"/signal-stop-* "$FAKE_STATE"/signal-start-* "$FAKE_STATE/daemon-signal-sent"
  : >"$FAKE_STATE/systemctl.log"
  local status=0
  env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" "$@" "$DEPLOY_DIR/rollback.sh" --activate-local >"$TMP/rollback-signal-$label.out" 2>&1 || status=$?
  [[ $status == "$expected" ]] || fail "rollback $label returned $status instead of $expected"
  assert_f5_source_restored
  ! grep -q '^CURRENT=' "$TMP/rollback-signal-$label.out" || fail "rollback $label printed a success receipt"
  [[ -z $(find "$ROOT/opt/persea-terminal" "$ROOT/etc/persea-terminal" -maxdepth 1 -name '.*rollback.*' -print -quit) ]] || fail "rollback $label left a temporary rollback target"
}
run_f5_signal pre_pointer 130 FAKE_SIGNAL_STOP_UNIT=persea-terminal-front.service FAKE_SIGNAL_KIND=INT
run_f5_signal post_pointer 143 FAKE_SIGNAL_DAEMON_RELOAD=TERM
run_f5_signal post_front_start 129 FAKE_SIGNAL_START_UNIT=persea-terminal-front.service FAKE_SIGNAL_KIND=HUP
pass 'rollback INT TERM and HUP restore sidecar state before returning signal status'

rm -f -- "$FAKE_STATE/fail-start-once-persea-terminal-tailscaled.service"
if env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" FAKE_FAIL_START_ONCE=persea-terminal-tailscaled.service \
  "$DEPLOY_DIR/rollback.sh" --activate-local >"$TMP/rollback-sidecar-once.out" 2>&1; then
  fail 'rollback accepted a sidecar start failure'
fi
assert_f5_source_restored
grep -Fqi 'sidecar recovery' "$TMP/rollback-sidecar-once.out" || fail 'rollback sidecar start failure was not named'
! grep -q '^CURRENT=' "$TMP/rollback-sidecar-once.out" || fail 'rollback printed success after sidecar start failure'

if env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" FAKE_PID_EXEC_DRIFT=persea-terminal-tailscaled.service \
  "$DEPLOY_DIR/rollback.sh" --activate-local >"$TMP/rollback-sidecar-verify.out" 2>&1; then
  fail 'rollback accepted sidecar verification drift'
fi
[[ $(readlink -- "$ROOT/opt/persea-terminal/current") == "$f5_source" ]] || fail 'rollback verifier failure did not restore source pointer'
[[ -e $FAKE_STATE/active/persea-terminal-tailscaled.service ]] || fail 'rollback verifier failure left sidecar inactive'
grep -Fqi 'sidecar' "$TMP/rollback-sidecar-verify.out" || fail 'rollback verifier failure did not name the sidecar'
! grep -q '^CURRENT=' "$TMP/rollback-sidecar-verify.out" || fail 'rollback verifier failure printed success'
pass 'rollback sidecar start and verification failures are terminal and visible'

sidecar_version_hash=$(sha256sum "$ROOT/etc/systemd/system/persea-terminal-tailscaled.service" | awk '{print $1}')
if env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" FAKE_TSD_VERSION=1.102.1 "$DEPLOY_DIR/install-tailscale-sidecar.sh" >"$TMP/sidecar-version-fail.out" 2>&1; then
  fail 'below-floor tailscaled daemon was accepted'
fi
grep -Fq 'tailscaled daemon 1.102.1 is below the supported Tailscale floor 1.102.2' "$TMP/sidecar-version-fail.out" || fail 'below-floor daemon refusal did not name the floor'
if env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" FAKE_TSD_OMIT=-tun "$DEPLOY_DIR/install-tailscale-sidecar.sh" >"$TMP/sidecar-capability-fail.out" 2>&1; then
  fail 'tailscaled lacking --tun was accepted'
fi
grep -Fq 'tailscaled --help lacks required flag -tun' "$TMP/sidecar-capability-fail.out" || fail 'missing daemon flag refusal did not name the flag'
if env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" FAKE_TSD_OMIT=userspace-networking "$DEPLOY_DIR/install-tailscale-sidecar.sh" >"$TMP/sidecar-userspace-fail.out" 2>&1; then
  fail 'tailscaled without userspace-networking was accepted'
fi
grep -Fq 'tailscaled --help lacks required text userspace-networking' "$TMP/sidecar-userspace-fail.out" || fail 'missing userspace-networking refusal did not name the capability'
[[ $(sha256sum "$ROOT/etc/systemd/system/persea-terminal-tailscaled.service" | awk '{print $1}') == "$sidecar_version_hash" && -e $FAKE_STATE/active/persea-terminal-tailscaled.service ]] || fail 'tailscaled preflight mutated sidecar state'
pass 'tailscaled floor and capability preflight fail before sidecar mutation and name the missing capability'

env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" FAKE_TS_VERSION=1.104.0 FAKE_TSD_VERSION=1.104.0 "$DEPLOY_DIR/verify.sh" --require-sidecar >/dev/null || fail 'newer Tailscale minor release was refused by sidecar verification'
env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" FAKE_TS_VERSION=1.102.3 FAKE_TSD_VERSION=1.102.9 "$DEPLOY_DIR/verify.sh" --require-sidecar >/dev/null || fail 'newer tailscaled patch release was refused by sidecar verification'
pass 'newer Tailscale CLI and daemon releases pass the floor and capability probe during sidecar verification'

: >"$FAKE_STATE/tailscale.log"
env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" "$DEPLOY_DIR/login-tailscale-sidecar.sh" >/dev/null
grep -Fxq 'LOGIN up --hostname=terminal-sidecar --advertise-tags=tag:terminal-service' "$FAKE_STATE/tailscale.log" || fail 'sidecar login did not use exact hostname and sole tag'
! grep -q '^MAIN_MUTATE ' "$FAKE_STATE/tailscale.log" || fail 'sidecar login reached a main-daemon mutation sentinel'
pass 'interactive login is isolated to the sidecar socket with exact hostname and sole tag'

sidecar_socket="$ROOT/run/persea-terminal-tailscale/tailscaled.sock"
sidecar_runtime="$ROOT/run/persea-terminal-tailscale"
sidecar_state="$ROOT/var/lib/persea-terminal-tailscale"

expect_sidecar_boundary_failure() {
  local label=$1; shift
  : >"$FAKE_STATE/tailscale.log"
  if env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" "$@" "$DEPLOY_DIR/activate-service.sh" >"$TMP/sidecar-boundary-fail.out" 2>&1; then
    fail "$label sidecar boundary was accepted"
  fi
  ! grep -q -E '^(MUTATE|MAIN_MUTATE) ' "$FAKE_STATE/tailscale.log" || fail "$label sidecar boundary reached a Tailscale mutation sentinel"
  pass "$label sidecar boundary fails before every Tailscale mutation"
}

expect_sidecar_boundary_failure 'extra hosting tag' FAKE_EXTRA_TAG=1
expect_sidecar_boundary_failure 'wrong hosting tag' FAKE_WRONG_TAG=1
expect_sidecar_boundary_failure 'protected main-node hosting identity' FAKE_PROTECTED_IDENTITY=1
expect_sidecar_boundary_failure 'wrong sidecar daemon' FAKE_PID_EXEC_DRIFT=persea-terminal-tailscaled.service

chmod 0777 "$sidecar_socket"
expect_sidecar_boundary_failure 'drifted-mode sidecar socket'
chmod 0666 "$sidecar_socket"
chmod 0755 "$sidecar_runtime"
expect_sidecar_boundary_failure 'world-traversable sidecar runtime'
chmod 0700 "$sidecar_runtime"
chmod 0755 "$sidecar_state"
expect_sidecar_boundary_failure 'world-traversable sidecar state'
chmod 0700 "$sidecar_state"

rm -f -- "$sidecar_socket"
expect_sidecar_boundary_failure 'missing sidecar socket'
env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" systemctl restart persea-terminal-tailscaled.service
rm -f -- "$sidecar_socket"
printf 'not a socket\n' >"$sidecar_socket"
chmod 0666 "$sidecar_socket"
expect_sidecar_boundary_failure 'ordinary-file sidecar endpoint'
rm -f -- "$sidecar_socket"
env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" systemctl restart persea-terminal-tailscaled.service
rm -f -- "$sidecar_socket"
ln -s -- "$socket" "$sidecar_socket"
expect_sidecar_boundary_failure 'symlinked sidecar endpoint'
rm -f -- "$sidecar_socket"
env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" systemctl restart persea-terminal-tailscaled.service
/usr/bin/sudo -n chown root:root "$sidecar_socket"
expect_sidecar_boundary_failure 'wrong-owner sidecar socket'
/usr/bin/sudo -n chown "$(id -u):$(id -g)" "$sidecar_socket"

default_mutant="$TMP/default-socket-mutant"
cp -a -- "$DEPLOY_DIR" "$default_mutant"
chmod u+w "$default_mutant/lib.sh"
# shellcheck disable=SC2016
sed -i 's/"$cli" "--socket=$socket" "$@"/"$cli" "$@"/' "$default_mutant/lib.sh"
# shellcheck disable=SC2016
grep -Fxq '  "$cli" "$@"' "$default_mutant/lib.sh" || fail 'default-socket mutant rewrite did not apply'
: >"$FAKE_STATE/tailscale.log"
: >"$FAKE_STATE/tailscale-calls.log"
if env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" "$default_mutant/activate-service.sh" >"$TMP/default-socket-activate.out" 2>&1; then
  fail 'default-socket activation mutant was accepted'
fi
grep -Fxq 'CALL default status --json' "$FAKE_STATE/tailscale-calls.log" || fail 'default-socket mutant did not reach the distinct protected main-node identity sentinel'
! grep -q -E '^(MUTATE|MAIN_MUTATE) ' "$FAKE_STATE/tailscale.log" || fail 'default-socket activation mutant changed mock state'
pass 'omitted-socket activation mutant selects distinct protected main-node identity and fails before mutation'

: >"$FAKE_STATE/tailscale.log"
: >"$FAKE_STATE/tailscale-calls.log"
if env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" "$default_mutant/login-tailscale-sidecar.sh" >"$TMP/default-socket-login.out" 2>&1; then
  fail 'default-socket login mutant was accepted'
fi
grep -Fxq 'CALL default status --json' "$FAKE_STATE/tailscale-calls.log" || fail 'default-socket login mutant did not reach the protected-main identity sentinel'
! grep -q -E '^(LOGIN|MUTATE|MAIN_MUTATE) ' "$FAKE_STATE/tailscale.log" || fail 'default-socket login mutant reached enrollment or mutation'
pass 'omitted-socket login mutant rejects protected main node before enrollment'

reset_mutant_dir="$TMP/reset-login-mutant"
cp -a -- "$DEPLOY_DIR" "$reset_mutant_dir"
reset_mutant="$reset_mutant_dir/login-tailscale-sidecar.sh"
chmod u+w "$reset_mutant"
sed -i 's/persea_sidecar_tailscale up /persea_sidecar_tailscale up --reset /' "$reset_mutant"
grep -Fq 'persea_sidecar_tailscale up --reset ' "$reset_mutant" || fail 'reset login mutant rewrite did not apply'
: >"$FAKE_STATE/tailscale.log"
if env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" bash "$reset_mutant" >"$TMP/reset-login.out" 2>&1; then
  fail 'reset login mutant was accepted'
fi
! grep -q -E '^(LOGIN|MAIN_MUTATE) ' "$FAKE_STATE/tailscale.log" || fail 'reset login mutant enrolled or reached main state'
pass 'reset-bearing login mutant fails the exact enrollment discriminator'

prior_sidecar_hash=$(sha256sum "$ROOT/etc/systemd/system/persea-terminal-tailscaled.service" | awk '{print $1}')
prior_current=$(readlink -- "$ROOT/opt/persea-terminal/current")
if env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" FAKE_PID_EXEC_DRIFT=persea-terminal-tailscaled.service "$DEPLOY_DIR/install-tailscale-sidecar.sh" >"$TMP/sidecar-install-rollback.out" 2>&1; then
  fail 'sidecar install readback failure was accepted'
fi
[[ $(sha256sum "$ROOT/etc/systemd/system/persea-terminal-tailscaled.service" | awk '{print $1}') == "$prior_sidecar_hash" ]] || fail 'sidecar install rollback changed prior unit bytes'
[[ $(readlink -- "$ROOT/opt/persea-terminal/current") == "$prior_current" && -e $FAKE_STATE/active/persea-terminal-tailscaled.service && -e $FAKE_STATE/enabled/persea-terminal-tailscaled.service ]] || fail 'sidecar install rollback did not restore pointer/active/enabled state'
pass 'sidecar install readback failure restores exact unit and lifecycle state'

rm -f -- "$FAKE_STATE/daemon-signal-sent"
sidecar_signal_status=0
env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" FAKE_SIGNAL_DAEMON_RELOAD=TERM "$DEPLOY_DIR/install-tailscale-sidecar.sh" >"$TMP/sidecar-install-signal.out" 2>&1 || sidecar_signal_status=$?
[[ $sidecar_signal_status == 143 ]] || fail "signal-interrupted sidecar install returned $sidecar_signal_status instead of 143"
[[ $(sha256sum "$ROOT/etc/systemd/system/persea-terminal-tailscaled.service" | awk '{print $1}') == "$prior_sidecar_hash" && -e $FAKE_STATE/active/persea-terminal-tailscaled.service && -e $FAKE_STATE/enabled/persea-terminal-tailscaled.service ]] || fail 'signal-interrupted sidecar install did not restore exact unit lifecycle'
[[ $(readlink -- "$ROOT/opt/persea-terminal/current") == "$prior_current" ]] || fail 'signal-interrupted sidecar install changed current release pointer'
pass 'signal-interrupted sidecar install preserves signal status and restores exact lifecycle'

rm -f -- "$FAKE_STATE/main-serve-count" "$FAKE_STATE/service-active" "$FAKE_STATE/service-https" "$FAKE_STATE/service-http"
: >"$FAKE_STATE/tailscale.log"
if env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" FAKE_MAIN_DRIFT=1 FAKE_MAIN_DRIFT_AT=5 "$DEPLOY_DIR/activate-service.sh" >"$TMP/main-drift-activation.out" 2>&1; then
  fail 'main Serve drift during activation was accepted'
fi
grep -Fxq 'MUTATE serve --service=svc:terminal --https=443 --yes unix:/run/persea-terminal/front.sock' "$FAKE_STATE/tailscale.log" || fail 'main-drift activation did not reach the intended post-mutation discriminator'
grep -Fxq 'MUTATE serve --service=svc:terminal --http=80 --yes unix:/run/persea-terminal/front.sock' "$FAKE_STATE/tailscale.log" || fail 'main-drift activation did not configure the HTTP redirect endpoint'
grep -Fxq 'MUTATE drain svc:terminal' "$FAKE_STATE/tailscale.log" || fail 'main-drift activation did not drain exact Service'
grep -Fxq 'MUTATE clear svc:terminal' "$FAKE_STATE/tailscale.log" || fail 'main-drift activation did not clear exact Service'
[[ ! -e $FAKE_STATE/service-active ]] || fail 'main-drift activation left target Service state'
! grep -q '^MAIN_MUTATE ' "$FAKE_STATE/tailscale.log" || fail 'main-drift activation attempted to repair the protected main node'
pass 'main Serve drift after activation triggers only sidecar Service rollback and no repair'

rm -f -- "$FAKE_STATE/main-serve-count" "$FAKE_STATE/service-active" "$FAKE_STATE/service-https" "$FAKE_STATE/service-http"
: >"$FAKE_STATE/tailscale.log"
if env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" FAKE_BAD_READBACK=1 FAKE_MAIN_DRIFT=1 FAKE_MAIN_DRIFT_AT=5 "$DEPLOY_DIR/activate-service.sh" >"$TMP/main-drift-rollback.out" 2>&1; then
  fail 'main Serve drift during activation rollback was accepted'
fi
grep -Fxq 'MUTATE drain svc:terminal' "$FAKE_STATE/tailscale.log" || fail 'rollback-drift activation did not drain exact Service'
grep -Fxq 'MUTATE clear svc:terminal' "$FAKE_STATE/tailscale.log" || fail 'rollback-drift activation did not clear exact Service'
[[ ! -e $FAKE_STATE/service-active ]] || fail 'rollback-drift activation left target Service state'
! grep -q '^MAIN_MUTATE ' "$FAKE_STATE/tailscale.log" || fail 'rollback-drift activation attempted to repair the protected main node'
pass 'main Serve drift detected inside activation rollback never triggers protected main-node repair'

rm -f -- "$FAKE_STATE/main-serve-count"
: >"$FAKE_STATE/tailscale.log"
if env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" FAKE_MAIN_DRIFT=1 FAKE_MAIN_DRIFT_AT=4 "$DEPLOY_DIR/install-tailscale-sidecar.sh" >"$TMP/main-drift-install.out" 2>&1; then
  fail 'main Serve drift during sidecar install was accepted'
fi
[[ $(sha256sum "$ROOT/etc/systemd/system/persea-terminal-tailscaled.service" | awk '{print $1}') == "$prior_sidecar_hash" && -e $FAKE_STATE/active/persea-terminal-tailscaled.service && -e $FAKE_STATE/enabled/persea-terminal-tailscaled.service ]] || fail 'main-drift install did not restore exact prior unit lifecycle'
[[ $(readlink -- "$ROOT/opt/persea-terminal/current") == "$prior_current" ]] || fail 'main-drift install changed current release pointer'
! grep -q '^MAIN_MUTATE ' "$FAKE_STATE/tailscale.log" || fail 'main-drift install attempted to repair the protected main node'
pass 'main Serve drift during sidecar install is detected with exact lifecycle restoration'

rm -f -- "$FAKE_STATE/main-serve-count"
: >"$FAKE_STATE/tailscale.log"
if env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" FAKE_MAIN_DRIFT=1 FAKE_MAIN_DRIFT_AT=2 "$DEPLOY_DIR/uninstall-tailscale-sidecar.sh" >"$TMP/main-drift-uninstall.out" 2>&1; then
  fail 'main Serve drift during sidecar uninstall was accepted'
fi
[[ -f $ROOT/etc/systemd/system/persea-terminal-tailscaled.service && -e $FAKE_STATE/active/persea-terminal-tailscaled.service && -e $FAKE_STATE/enabled/persea-terminal-tailscaled.service && -d $sidecar_state ]] || fail 'main-drift uninstall did not restore unit/lifecycle/state'
! grep -q '^MAIN_MUTATE ' "$FAKE_STATE/tailscale.log" || fail 'main-drift uninstall attempted to repair the protected main node'
pass 'main Serve drift during sidecar uninstall is detected with recoverable state preserved'

rm -f -- "$FAKE_STATE/main-serve-count" "$FAKE_STATE/service-active" "$FAKE_STATE/service-https" "$FAKE_STATE/service-http"
: >"$FAKE_STATE/tailscale.log"
env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" "$DEPLOY_DIR/activate-service.sh" >/dev/null
[[ -e $FAKE_STATE/service-active ]] || fail 'deactivation drift setup did not activate the target Service'
rm -f -- "$FAKE_STATE/main-serve-count"
: >"$FAKE_STATE/tailscale.log"
if env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" FAKE_MAIN_DRIFT=1 FAKE_MAIN_DRIFT_AT=4 "$DEPLOY_DIR/deactivate-service.sh" >"$TMP/main-drift-deactivate.out" 2>&1; then
  fail 'main Serve drift during deactivation was accepted'
fi
grep -Fxq 'MUTATE drain svc:terminal' "$FAKE_STATE/tailscale.log" || fail 'main-drift deactivation did not drain exact Service'
grep -Fxq 'MUTATE clear svc:terminal' "$FAKE_STATE/tailscale.log" || fail 'main-drift deactivation did not clear exact Service'
[[ ! -e $FAKE_STATE/service-active ]] || fail 'main-drift deactivation left target Service state'
! grep -q '^MAIN_MUTATE ' "$FAKE_STATE/tailscale.log" || fail 'main-drift deactivation attempted to repair the protected main node'
pass 'main Serve drift during deactivation is detected after exact Service clearing'

expect_no_tailscale_mutation() {
  local label=$1; shift
  : >"$FAKE_STATE/tailscale.log"
  rm -f "$FAKE_STATE/service-active" "$FAKE_STATE/service-https" "$FAKE_STATE/service-http"
  if env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" "$@" "$DEPLOY_DIR/activate-service.sh" >"$TMP/activate-fail.out" 2>&1; then
    fail "$label activation was accepted"
  fi
  ! grep -q '^MUTATE ' "$FAKE_STATE/tailscale.log" || fail "$label reached a Tailscale mutation"
  pass "$label prevents every Tailscale mutation"
}

expect_no_tailscale_mutation 'missing hosting tag' FAKE_NO_TAG=1
expect_no_tailscale_mutation 'wrong Service FQDN' FAKE_SUFFIX=wrong-tailnet.ts.net
chmod 0666 "$socket"
expect_no_tailscale_mutation 'unsafe front socket'
chmod 0600 "$socket"
expect_no_tailscale_mutation 'inactive local unit' FAKE_INACTIVE=1
expect_no_tailscale_mutation 'drifted MainPID executable' FAKE_PID_EXEC_DRIFT=persea-terminal-front.service
expect_no_tailscale_mutation 'drifted local broker socket' FAKE_SOCKET_DRIFT=local
expect_no_tailscale_mutation 'drifted cross-UID broker socket' FAKE_SOCKET_DRIFT=remote

evidence_root="$ROOT/var/lib/persea-terminal-deployments"
rm -rf -- "$evidence_root" "$evidence_root.replaced"
chmod 0777 "$ROOT/var"
expect_no_tailscale_mutation 'unsafe evidence ancestor'
chmod 0755 "$ROOT/var"
mkdir -m 0700 "$TMP/evidence-symlink-target"
ln -s -- "$TMP/evidence-symlink-target" "$evidence_root"
expect_no_tailscale_mutation 'symlink evidence root'
rm -f -- "$evidence_root"
mkdir -m 0755 "$evidence_root"
expect_no_tailscale_mutation 'wrong-mode evidence root'
chmod 0700 "$evidence_root"
expect_no_tailscale_mutation 'wrong-owner evidence root' PERSEA_TEST_EVIDENCE_OWNER=999
rm -f "$FAKE_STATE/evidence-replaced"
expect_no_tailscale_mutation 'replaced evidence root' FAKE_REPLACE_EVIDENCE=1
rm -rf -- "$evidence_root" "$evidence_root.replaced"

expect_post_mutation_rollback() {
  local label=$1; shift
  : >"$FAKE_STATE/tailscale.log"
  rm -f "$FAKE_STATE/service-active" "$FAKE_STATE/service-https" "$FAKE_STATE/service-http"
  if env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" "$@" "$DEPLOY_DIR/activate-service.sh" >"$TMP/post-fail.out" 2>&1; then
    fail "$label activation was accepted"
  fi
  grep -Fxq 'MUTATE serve --service=svc:terminal --https=443 --yes unix:/run/persea-terminal/front.sock' "$FAKE_STATE/tailscale.log" || fail "$label did not initialize HTTPS"
  grep -Fxq 'MUTATE serve --service=svc:terminal --http=80 --yes unix:/run/persea-terminal/front.sock' "$FAKE_STATE/tailscale.log" || fail "$label did not initialize the HTTP redirect endpoint"
  grep -Fxq 'MUTATE drain svc:terminal' "$FAKE_STATE/tailscale.log" || fail "$label did not drain exact Service"
  grep -Fxq 'MUTATE clear svc:terminal' "$FAKE_STATE/tailscale.log" || fail "$label did not clear exact Service"
  ! grep -q 'reset' "$FAKE_STATE/tailscale.log" || fail "$label used global reset"
  pass "$label rolls back only the target Service after bounded post-command failure"
}

expect_post_mutation_rollback 'undefined Service' FAKE_SERVICE_MISSING=1
expect_post_mutation_rollback 'unapproved Service' FAKE_UNAPPROVED=1
expect_post_mutation_rollback 'HTTP endpoint command failure' FAKE_HTTP_SERVE_FAIL=1

: >"$FAKE_STATE/tailscale.log"
rm -f "$FAKE_STATE/service-active" "$FAKE_STATE/service-https" "$FAKE_STATE/service-http"
signal_status=0
env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" FAKE_SIGNAL_AFTER_SERVE=TERM "$DEPLOY_DIR/activate-service.sh" >"$TMP/signal-activation.out" 2>&1 || signal_status=$?
[[ $signal_status == 143 ]] || fail "signal-interrupted activation returned $signal_status instead of 143"
mapfile -t signal_mutations <"$FAKE_STATE/tailscale.log"
[[ ${signal_mutations[0]:-} == 'MUTATE serve --service=svc:terminal --https=443 --yes unix:/run/persea-terminal/front.sock' ]] || fail 'signal discriminator missed the sole Service mutation'
[[ ${signal_mutations[1]:-} == 'MUTATE drain svc:terminal' && ${signal_mutations[2]:-} == 'MUTATE clear svc:terminal' ]] || fail 'signal discriminator did not drain then clear exact Service'
[[ ${#signal_mutations[@]} == 3 ]] || fail 'signal discriminator made extra Tailscale mutations'
signal_evidence=$(find "$evidence_root" -mindepth 1 -maxdepth 1 -type d -printf '%T@ %p\n' | sort -nr | sed -n '1s/^[^ ]* //p')
[[ -n $signal_evidence && -f $signal_evidence/node-serve-before.json && -f $signal_evidence/node-serve-after-rollback.json ]] || fail 'signal discriminator lacks rollback evidence'
cmp -s "$signal_evidence/node-serve-before.json" "$signal_evidence/node-serve-after-rollback.json" || fail 'signal discriminator did not restore raw Serve state'
[[ ! -e $FAKE_STATE/service-active ]] || fail 'signal discriminator left target Service active'
! grep -q 'reset' "$FAKE_STATE/tailscale.log" || fail 'signal discriminator used global reset'
pass 'signal-interruption discriminator rolls back exact Service and preserves signal exit status'

: >"$FAKE_STATE/tailscale.log"
rm -f "$FAKE_STATE/service-active" "$FAKE_STATE/service-https" "$FAKE_STATE/service-http"
positive_before=$(wc -l <"$POSITIVE_COUNT")
env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" "$DEPLOY_DIR/activate-service.sh" >"$TMP/activate.out"
positive_after=$(wc -l <"$POSITIVE_COUNT")
grep -Fxq 'MUTATE serve --service=svc:terminal --https=443 --yes unix:/run/persea-terminal/front.sock' "$FAKE_STATE/tailscale.log" || fail 'activation did not target exactly one Service'
grep -Fxq 'MUTATE serve --service=svc:terminal --http=80 --yes unix:/run/persea-terminal/front.sock' "$FAKE_STATE/tailscale.log" || fail 'activation did not configure the HTTP redirect endpoint'
[[ $(grep -c '^MUTATE ' "$FAKE_STATE/tailscale.log") == 2 ]] || fail 'activation did not make exactly the two endpoint-scoped Service mutations'
[[ $((positive_after - positive_before)) == 2 ]] || fail 'activation did not run root-positive probes before and after UID denial'
pass 'fresh-host activation uses positive/zero-byte-negative/positive probes and exact HTTPS plus redirect endpoint mutations'

activation_evidence=$(sed -n 's/^EVIDENCE=//p' "$TMP/activate.out")
[[ -n $activation_evidence && -f $activation_evidence/tailscale-version.json && ! -L $activation_evidence/tailscale-version.json ]] || fail 'activation did not record the validated Tailscale version'
[[ $(stat -Lc '%a' -- "$activation_evidence/tailscale-version.json") == 400 ]] || fail 'Tailscale version evidence is not read-only'
python3 - "$activation_evidence/tailscale-version.json" <<'PY'
import json, sys
record = json.load(open(sys.argv[1], encoding="utf-8"))
assert record["min_version"] == "1.102.2", record
assert record["cli"] == {"version": "1.102.3", "long_version": "1.102.3-t9329c3677-ga522f65e9"}, record
assert record["daemon"] == {"version": "1.102.3", "long_version": "1.102.3-t9329c3677-ga522f65e9"}, record
probed = {(entry["binary"], entry["help_path"], entry["kind"], entry["token"]) for entry in record["capabilities_verified"]}
for expected in (("daemon", "", "flag", "-tun"), ("daemon", "", "text", "userspace-networking"), ("cli", "", "flag", "--socket"), ("cli", "serve", "flag", "--service"), ("cli", "serve status", "flag", "--json"), ("cli", "debug", "subcommand", "prefs"), ("cli", "up", "flag", "--advertise-tags")):
    assert expected in probed, expected
PY
pass 'activation evidence records the validated Tailscale CLI/daemon versions and the probed capabilities without gating on them'

: >"$FAKE_STATE/tailscale.log"
rm -f "$FAKE_STATE/service-active" "$FAKE_STATE/service-https" "$FAKE_STATE/service-http"
if env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" FAKE_BAD_READBACK=1 "$DEPLOY_DIR/activate-service.sh" >"$TMP/bad-readback.out" 2>&1; then
  fail 'bad service readback was accepted'
fi
grep -Fxq 'MUTATE drain svc:terminal' "$FAKE_STATE/tailscale.log" || fail 'failed activation did not drain exact Service'
grep -Fxq 'MUTATE clear svc:terminal' "$FAKE_STATE/tailscale.log" || fail 'failed activation did not clear exact Service'
! grep -q 'reset' "$FAKE_STATE/tailscale.log" || fail 'failed activation used global reset'
pass 'failed activation performs service-scoped rollback with Node Serve preservation'

: >"$FAKE_STATE/tailscale.log"
touch "$FAKE_STATE/service-active" "$FAKE_STATE/service-https" "$FAKE_STATE/service-http"
env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" "$DEPLOY_DIR/deactivate-service.sh" >/dev/null
[[ $(sed -n '1p' "$FAKE_STATE/tailscale.log") == 'MUTATE drain svc:terminal' && $(sed -n '2p' "$FAKE_STATE/tailscale.log") == 'MUTATE clear svc:terminal' ]] || fail 'deactivation is not drain then clear'
! grep -q 'reset' "$FAKE_STATE/tailscale.log" || fail 'deactivation used global reset'
pass 'deactivation is exact service-scoped drain then clear'

: >"$FAKE_STATE/tailscale.log"
env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" "$DEPLOY_DIR/uninstall-tailscale-sidecar.sh" >"$TMP/sidecar-uninstall-preserve.out"
[[ ! -e $ROOT/etc/systemd/system/persea-terminal-tailscaled.service && ! -e $FAKE_STATE/active/persea-terminal-tailscaled.service && ! -e $FAKE_STATE/enabled/persea-terminal-tailscaled.service ]] || fail 'default sidecar uninstall retained unit lifecycle state'
[[ -d $sidecar_state && ! -L $sidecar_state ]] || fail 'default sidecar uninstall did not preserve identity state'
grep -Fq 'SIDECAR_STATE=preserved:' "$TMP/sidecar-uninstall-preserve.out" || fail 'default sidecar uninstall did not report recoverability'
! grep -q '^MAIN_MUTATE ' "$FAKE_STATE/tailscale.log" || fail 'default sidecar uninstall attempted to mutate the protected main node'
pass 'default sidecar uninstall removes exact unit and preserves recoverable identity state'

env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" "$DEPLOY_DIR/install-tailscale-sidecar.sh" >/dev/null
: >"$FAKE_STATE/tailscale.log"
env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" "$DEPLOY_DIR/uninstall-tailscale-sidecar.sh" --purge-state >"$TMP/sidecar-uninstall-purge.out"
[[ ! -e $sidecar_state && ! -L $sidecar_state ]] || fail 'explicit sidecar state purge retained identity state'
grep -Fq 'SIDECAR_STATE=purged:' "$TMP/sidecar-uninstall-purge.out" || fail 'explicit sidecar state purge did not report destruction'
! grep -q '^MAIN_MUTATE ' "$FAKE_STATE/tailscale.log" || fail 'purging sidecar uninstall attempted to mutate the protected main node'
pass 'explicit sidecar purge occurs only after Service clearing and unit stop'

alias_store="$ROOT/var/lib/persea-terminal/aliases.json"
preferences_store="$ROOT/var/lib/persea-terminal/preferences.json"
snippet_store="$ROOT/var/lib/persea-terminal/snippets.json"
workspace_store="$ROOT/var/lib/persea-terminal/workspaces.json"
workspace_store_sentinel="$TMP/workspace-store-sentinel.json"
printf '{}\n' >"$alias_store"
printf '{}\n' >"$preferences_store"
printf '{}\n' >"$snippet_store"
printf '{"version":1,"workspaces":[{"sentinel":"preserve-me"}]}\n' >"$workspace_store_sentinel"
install -m 0600 "$workspace_store_sentinel" "$workspace_store"
workspace_store_identity=$(stat -Lc '%a:%u:%g' -- "$workspace_store")
workspace_store_preserved() {
  [[ -f $workspace_store && ! -L $workspace_store ]] &&
    cmp -s -- "$workspace_store_sentinel" "$workspace_store" &&
    [[ $(stat -Lc '%a:%u:%g' -- "$workspace_store") == "$workspace_store_identity" ]]
}
env "${hermetic_env[@]}" "PERSEA_DEPLOY_ROOT=$ROOT" "$DEPLOY_DIR/uninstall.sh" >/dev/null
[[ -f $alias_store && -f $preferences_store && -f $snippet_store && -d $ROOT/opt/persea-terminal/releases && -d $evidence_root && ! -e $ROOT/opt/persea-terminal/current ]] || fail 'uninstall removed recoverable state/evidence or retained current'
workspace_store_preserved || fail 'uninstall changed workspace-store bytes, mode, ownership, type, or presence'
workspace_store_backup="$TMP/workspace-store-delete-mutant.json"
mv -- "$workspace_store" "$workspace_store_backup"
if workspace_store_preserved; then
  fail 'delete-workspace-store mutant survived the preservation gate'
fi
mv -- "$workspace_store_backup" "$workspace_store"
workspace_store_preserved || fail 'workspace-store mutant restoration changed the preservation witness'
pass 'target-bound uninstall preserves releases, alias/preferences/snippet/workspace state, and root activation evidence'
pass 'delete-workspace-store mutant is rejected by the exact workspace preservation witness'

[[ $(build_root_snapshot) == "$build_roots_before" ]] || fail 'installer or build-environment mutants left private build-root residue'
pass 'candidate and all disposable build-environment mutants leave zero private build-root residue'

printf '1..%d\n' "$tests"
