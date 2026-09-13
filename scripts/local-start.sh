#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
PROJECT_DIR=$(cd -- "$SCRIPT_DIR/.." && pwd -P)
BIN=
ADAPTER_BIN=
SESSION=persea
RUNTIME_DIR=
TMUX_SOCKET=
BROKER_PID=
BROKER_START=
BROKER_EXE=
BROKER_PGID=
FRONT_PID=
FRONT_START=
FRONT_EXE=
FRONT_PGID=
ADAPTER_PID=
ADAPTER_START=
ADAPTER_EXE=
ADAPTER_PGID=

die() { printf '%s\n' "$1" >&2; return 1; }

# Process birth and process-group identity survive exec and remain readable for
# a non-dumpable broker. /proc/PID/exe is deliberately inaccessible in that case.
read_identity() {
  local pid=$1 line rest
  local -a fields
  [[ "$pid" =~ ^[1-9][0-9]*$ && -r "/proc/$pid/stat" ]] || return 1
  IFS= read -r line <"/proc/$pid/stat" || return 1
  [[ "$line" == *') '* ]] || return 1
  rest=${line##*) }
  read -r -a fields <<<"$rest"
  (( ${#fields[@]} > 19 )) || return 1
  ID_START=${fields[19]}
  ID_PGID=${fields[2]}
  [[ "$ID_START" =~ ^[1-9][0-9]*$ && "$ID_PGID" =~ ^[0-9]+$ ]]
}

matches() {
  read_identity "$1" && [[ "$ID_START" == "$2" && "$ID_PGID" == "$3" && "$ID_PGID" == "$1" ]]
}

record_service_identity() {
  local pid=$1 role=$2 expected_exe=$3 i initial_start initial_pgid
  read_identity "$pid" || die "cannot record $role process start ticks"
  initial_start=$ID_START; initial_pgid=$pid
  printf -v "${role^^}_START" '%s' "$initial_start"
  printf -v "${role^^}_EXE" '%s' "$expected_exe"
  printf -v "${role^^}_PGID" '%s' "$initial_pgid"
  for ((i=0; i<40; i++)); do
    if read_identity "$pid" && [[ "$ID_START" == "$initial_start" && "$ID_PGID" == "$initial_pgid" ]]; then
      RECORDED_START=$ID_START
      RECORDED_EXE=$expected_exe
      RECORDED_PGID=$ID_PGID
      return 0
    fi
    sleep 0.05
  done
  die "$role did not become a process-group leader with the recorded birth identity"
}

stop_owned() {
  local pid=$1 start=$2 exe=$3 pgid=$4 i
  [[ -n "$pid" && -n "$start" && -n "$exe" && -n "$pgid" ]] || return 0
  matches "$pid" "$start" "$pgid" || return 0
  kill -TERM -- "-$pgid"
  for ((i=0; i<40; i++)); do
    matches "$pid" "$start" "$pgid" || return 0
    sleep 0.05
  done
  matches "$pid" "$start" "$pgid" || return 0
  kill -KILL -- "-$pgid"
}

rollback() {
  local rc=$?
  trap - ERR INT TERM
  set +e
  stop_owned "$ADAPTER_PID" "$ADAPTER_START" "$ADAPTER_EXE" "$ADAPTER_PGID"
  stop_owned "$FRONT_PID" "$FRONT_START" "$FRONT_EXE" "$FRONT_PGID"
  stop_owned "$BROKER_PID" "$BROKER_START" "$BROKER_EXE" "$BROKER_PGID"
  if [[ -n "$TMUX_SOCKET" ]]; then
    timeout -k 2 5 tmux -S "$TMUX_SOCKET" kill-server >/dev/null 2>&1
  fi
  if [[ -n "$RUNTIME_DIR" && "$RUNTIME_DIR" == /tmp/persea-terminal.* && -d "$RUNTIME_DIR" && ! -L "$RUNTIME_DIR" ]]; then
    rm -rf -- "$RUNTIME_DIR"
  fi
  exit "$rc"
}

for command in npm go tmux node timeout setsid nohup ps; do
  command -v "$command" >/dev/null || die "required command is missing: $command"
done

(cd "$PROJECT_DIR/ui" && npm ci && npm run build)

old_umask=$(umask)
umask 077
RUNTIME_DIR=$(mktemp -d /tmp/persea-terminal.XXXXXXXXXX)
chmod 0700 "$RUNTIME_DIR"
umask "$old_umask"
TMUX_SOCKET="$RUNTIME_DIR/tmux.sock"
BIN="$RUNTIME_DIR/persea-terminal"
ADAPTER_BIN="$RUNTIME_DIR/persea-terminal-ingress-test"
BROKER_SOCKET="$RUNTIME_DIR/broker.sock"
FRONT_SOCKET="$RUNTIME_DIR/front.sock"
BROKER_CONFIG="$RUNTIME_DIR/broker.json"
FRONT_CONFIG="$RUNTIME_DIR/front.json"
STATE_FILE="$RUNTIME_DIR/state.env"
TMUX_CONFIG="$RUNTIME_DIR/tmux.conf"
trap rollback ERR INT TERM

(cd "$PROJECT_DIR" && go build -o "$BIN" ./cmd/persea-terminal)
(cd "$PROJECT_DIR" && go build -o "$ADAPTER_BIN" ./cmd/persea-terminal-ingress-test)
chmod 0700 "$BIN" "$ADAPTER_BIN"
mkdir -m 0700 "$RUNTIME_DIR/ui"
for bundle_file in index.html app.js app.css xterm.css app.js.gz app.css.gz xterm.css.gz THIRD_PARTY_NOTICES.txt manifest.webmanifest icon-192.png icon-512.png apple-touch-icon.png; do
  cp -- "$PROJECT_DIR/ui/dist/$bundle_file" "$RUNTIME_DIR/ui/$bundle_file"
done
chmod 0600 "$RUNTIME_DIR/ui/"*

printf '%s\n' \
  'set -g history-limit 10000' \
  'set -g status on' \
  'set -g status-left ""' \
  'set -g status-right ""' \
  'set -g status-interval 0' \
  'set -g mouse off' >"$TMUX_CONFIG"
chmod 0600 "$TMUX_CONFIG"

# The pane remains an ordinary interactive shell after deterministic history seeding.
# Expansion is intentionally deferred to that private pane shell.
# shellcheck disable=SC2016
PANE_COMMAND='i=1; while [ "$i" -le 2500 ]; do printf "PERSEA-HISTORY-%04d\\n" "$i"; i=$((i+1)); done; exec "${SHELL:-/bin/bash}" -i'
timeout -k 2 10 tmux -S "$TMUX_SOCKET" -f "$TMUX_CONFIG" new-session -d -s "$SESSION" -x 120 -y 40 "$PANE_COMMAND"
for _ in {1..100}; do
  HISTORY_SIZE=$(timeout -k 2 5 tmux -S "$TMUX_SOCKET" display-message -p -t "$SESSION:" -F '#{history_size}')
  (( HISTORY_SIZE >= 2000 )) && break
  sleep 0.05
done
(( HISTORY_SIZE >= 2000 )) || die "private tmux history did not seed"
[[ $(timeout -k 2 5 tmux -S "$TMUX_SOCKET" list-sessions -F '#{session_name}') == "$SESSION" ]] || die "private tmux realm does not contain exactly one session"

CALLER_UID=$(id -u)
OPERATOR_LOGIN=local-operator@example.test
PORT=$(node -e 'const n=require("node:net").createServer();n.listen(0,"127.0.0.1",()=>{console.log(n.address().port);n.close()})')
# PERSEA_HERMETIC_TLS=1 makes the disposable adapter terminate TLS with a
# certificate minted in memory for that run, so the page is served from an
# https loopback origin. WebKit refuses the front door's Secure CSRF cookie on
# any http origin, loopback included, so a WebKit browser gate needs this.
HERMETIC_TLS=${PERSEA_HERMETIC_TLS:-0}
[[ "$HERMETIC_TLS" == 0 || "$HERMETIC_TLS" == 1 ]] || die "PERSEA_HERMETIC_TLS must be 0 or 1"
SCHEME=http
INGRESS_TLS_FIELD=
ADAPTER_TLS_FLAG=()
if [[ "$HERMETIC_TLS" == 1 ]]; then
  SCHEME=https
  INGRESS_TLS_FIELD=',"hermetic_tls":true'
  ADAPTER_TLS_FLAG=(--tls)
fi
UNIFIED_OBSERVER=persea-local-observer
[[ "$SESSION" != "$UNIFIED_OBSERVER" ]] || UNIFIED_OBSERVER=persea-local-observer-2
mkdir -m 0700 "$RUNTIME_DIR/unified-journal"
printf '{"realm":"local","front_uid":%s,"servers":[{"label":"private","socket_path":"%s"}],"session_create":{"enabled":true,"servers":["private"]},"unified_terminal_dev":{"enabled":true,"server":"private","session":"%s","observer_session":"%s","runtime_dir":"%s"}}\n' "$CALLER_UID" "$TMUX_SOCKET" "$SESSION" "$UNIFIED_OBSERVER" "$RUNTIME_DIR/unified-journal" >"$BROKER_CONFIG"
printf '{"ingress":{"socket_path":"%s","peer_uid":%s,"canonical_host":"127.0.0.1:%s","operator_login":"%s","max_connections":64%s},"realms":[{"name":"local","socket":"%s","broker_uid":%s}],"alias_store_path":"%s","handle_ttl_seconds":60,"aliases":[{"alias":"Local","realm":"local","server":"private","session":"%s"}]}\n' "$FRONT_SOCKET" "$CALLER_UID" "$PORT" "$OPERATOR_LOGIN" "$INGRESS_TLS_FIELD" "$BROKER_SOCKET" "$CALLER_UID" "$RUNTIME_DIR/aliases.json" "$SESSION" >"$FRONT_CONFIG"
chmod 0600 "$BROKER_CONFIG" "$FRONT_CONFIG"

(cd "$PROJECT_DIR" && exec nohup setsid "$BIN" broker --socket "$BROKER_SOCKET" --config "$BROKER_CONFIG") >"$RUNTIME_DIR/broker.log" 2>&1 </dev/null &
BROKER_PID=$!
record_service_identity "$BROKER_PID" broker "$BIN"
BROKER_START=$RECORDED_START
BROKER_EXE=$RECORDED_EXE
BROKER_PGID=$RECORDED_PGID
for _ in {1..100}; do
  [[ -S "$BROKER_SOCKET" ]] && break
  matches "$BROKER_PID" "$BROKER_START" "$BROKER_PGID" || die "broker exited before readiness"
  sleep 0.05
done
[[ -S "$BROKER_SOCKET" ]] || die "broker socket was not ready"

(cd "$RUNTIME_DIR" && exec nohup setsid "$BIN" front --config "$FRONT_CONFIG" --static-dir ui) >"$RUNTIME_DIR/front.log" 2>&1 </dev/null &
FRONT_PID=$!
record_service_identity "$FRONT_PID" front "$BIN"
FRONT_START=$RECORDED_START
FRONT_EXE=$RECORDED_EXE
FRONT_PGID=$RECORDED_PGID
for _ in {1..100}; do
  [[ -S "$FRONT_SOCKET" ]] && break
  matches "$FRONT_PID" "$FRONT_START" "$FRONT_PGID" || die "front exited before socket readiness"
  sleep 0.05
done
[[ -S "$FRONT_SOCKET" ]] || die "front socket was not ready"

(cd "$RUNTIME_DIR" && exec nohup setsid "$ADAPTER_BIN" --listen "127.0.0.1:$PORT" --socket "$FRONT_SOCKET" --operator "$OPERATOR_LOGIN" "${ADAPTER_TLS_FLAG[@]}") >"$RUNTIME_DIR/adapter.log" 2>&1 </dev/null &
ADAPTER_PID=$!
record_service_identity "$ADAPTER_PID" adapter "$ADAPTER_BIN"
ADAPTER_START=$RECORDED_START
ADAPTER_EXE=$RECORDED_EXE
ADAPTER_PGID=$RECORDED_PGID

inventory_ready() {
  node - "$PORT" "$SCHEME" <<'NODE'
const port = Number(process.argv[2]);
const scheme = process.argv[3];
const http = require(scheme === 'https' ? 'node:https' : 'node:http');
// The adapter's certificate is minted per run and anchored nowhere, so the
// readiness probe cannot verify it. Only the loopback adapter is ever reached.
const options = {host: '127.0.0.1', port, path: '/api/inventory', headers: {Host: `127.0.0.1:${port}`}, timeout: 1000};
if (scheme === 'https') options.rejectUnauthorized = false;
const request = http.get(options, response => {
  let body = '';
  response.setEncoding('utf8');
  response.on('data', chunk => { body += chunk; });
  response.on('end', () => {
    try {
      const value = JSON.parse(body);
      const session = value?.realms?.[0]?.servers?.[0]?.sessions?.[0];
      const ready = response.statusCode === 200 && session?.name === 'persea';
      if (!ready) console.error(JSON.stringify({status: response.statusCode, session: session?.name, realms: value?.realms?.map(realm => ({error: realm.error, servers: realm.servers?.map(server => ({status: server.status, error: server.error, sessions: server.sessions?.length}))}))}));
      process.exit(ready ? 0 : 1);
    } catch (_) { process.exit(1); }
  });
});
request.on('timeout', () => request.destroy());
request.on('error', () => process.exit(1));
NODE
}

ready=0
for _ in {1..100}; do
  matches "$FRONT_PID" "$FRONT_START" "$FRONT_PGID" || die "front exited before inventory readiness"
  matches "$ADAPTER_PID" "$ADAPTER_START" "$ADAPTER_PGID" || die "adapter exited before inventory readiness"
  if inventory_ready 2>"$RUNTIME_DIR/readiness.log" && matches "$FRONT_PID" "$FRONT_START" "$FRONT_PGID" && matches "$ADAPTER_PID" "$ADAPTER_START" "$ADAPTER_PGID"; then ready=1; break; fi
  sleep 0.05
done
if (( ready != 1 )); then
  cat "$RUNTIME_DIR/readiness.log" >&2
  die "canonical inventory did not become ready"
fi

old_umask=$(umask)
umask 077
{
  printf 'VERSION=2\nRUNTIME_DIR=%s\nPROJECT_DIR=%s\nBIN=%s\nADAPTER_BIN=%s\n' "$RUNTIME_DIR" "$PROJECT_DIR" "$BIN" "$ADAPTER_BIN"
  printf 'TMUX_SOCKET=%s\nSESSION=%s\nBROKER_SOCKET=%s\nFRONT_SOCKET=%s\n' "$TMUX_SOCKET" "$SESSION" "$BROKER_SOCKET" "$FRONT_SOCKET"
  printf 'BROKER_PID=%s\nBROKER_START=%s\nBROKER_EXE=%s\nBROKER_PGID=%s\n' "$BROKER_PID" "$BROKER_START" "$BROKER_EXE" "$BROKER_PGID"
  printf 'FRONT_PID=%s\nFRONT_START=%s\nFRONT_EXE=%s\nFRONT_PGID=%s\n' "$FRONT_PID" "$FRONT_START" "$FRONT_EXE" "$FRONT_PGID"
  printf 'ADAPTER_PID=%s\nADAPTER_START=%s\nADAPTER_EXE=%s\nADAPTER_PGID=%s\nPORT=%s\n' "$ADAPTER_PID" "$ADAPTER_START" "$ADAPTER_EXE" "$ADAPTER_PGID" "$PORT"
} >"$STATE_FILE"
chmod 0600 "$STATE_FILE"
umask "$old_umask"

printf 'URL=%s://127.0.0.1:%s/terminal\n' "$SCHEME" "$PORT"
printf 'RUNTIME_DIR=%s\n' "$RUNTIME_DIR"
printf 'TMUX_ATTACH=tmux -S %q attach-session -t %q\n' "$TMUX_SOCKET" "$SESSION"
printf 'STOP_COMMAND=%q %q\n' "$SCRIPT_DIR/local-stop.sh" "$RUNTIME_DIR"
trap - ERR INT TERM
