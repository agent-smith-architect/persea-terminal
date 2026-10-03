#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
RUNTIME_DIR=
RUNTIME_DIR2=
TEST_BIN_DIR=
cleanup() {
  local rc=$?
  trap - EXIT
  local runtime
  for runtime in "$RUNTIME_DIR" "$RUNTIME_DIR2"; do
    if [[ -n "$runtime" && "$runtime" == /tmp/persea-terminal.* ]]; then
      if [[ -S "$runtime/tmux.sock" ]]; then
        timeout -k 2 5 tmux -S "$runtime/tmux.sock" kill-server >/dev/null 2>&1 || true
      fi
      [[ -d "$runtime" && ! -L "$runtime" ]] && rm -rf -- "$runtime"
    fi
  done
  if [[ -n "$TEST_BIN_DIR" && "$TEST_BIN_DIR" == /tmp/persea-terminal-test-bin.* ]]; then
    rm -rf -- "$TEST_BIN_DIR"
  fi
  exit "$rc"
}
trap cleanup EXIT

runtime_snapshot() {
  local path
  shopt -s nullglob
  for path in /tmp/persea-terminal.*; do printf '%s\n' "$path"; done | LC_ALL=C sort
  shopt -u nullglob
}

recorded_process_live() {
  local pid=$1 expected_start=$2 line rest state
  local -a fields
  [[ -r "/proc/$pid/stat" ]] || return 1
  IFS= read -r line <"/proc/$pid/stat" || return 1
  [[ "$line" == *') '* ]] || return 1
  rest=${line##*) }
  state=${rest%% *}
  read -r -a fields <<<"$rest"
  [[ "$state" != Z && "$state" != X && ${fields[19]:-} == "$expected_start" ]]
}

before_failed_start=$(runtime_snapshot)
REAL_TMUX=$(command -v tmux)
TEST_BIN_DIR=$(mktemp -d /tmp/persea-terminal-test-bin.XXXXXXXXXX)
# Wrapper variables expand only when the generated fault-injection command runs.
# shellcheck disable=SC2016
printf '%s\n' '#!/usr/bin/env bash' 'if [[ " $* " == *" display-message "* ]]; then exit 1; fi' 'exec "$REAL_TMUX" "$@"' >"$TEST_BIN_DIR/tmux"
chmod 0700 "$TEST_BIN_DIR/tmux"
if REAL_TMUX="$REAL_TMUX" PATH="$TEST_BIN_DIR:$PATH" "$SCRIPT_DIR/local-start.sh" >/dev/null 2>&1; then
  printf 'forced launcher failure unexpectedly succeeded\n' >&2
  exit 1
fi
after_failed_start=$(runtime_snapshot)
[[ "$before_failed_start" == "$after_failed_start" ]] || { printf 'failed-start rollback left a runtime behind\n' >&2; exit 1; }
rm -rf -- "$TEST_BIN_DIR"
TEST_BIN_DIR=

default_socket="/tmp/tmux-$(id -u)/default"
if [[ -S "$default_socket" ]]; then default_before=$(stat -Lc '%d:%i:%s:%Y:%Z' "$default_socket"); else default_before=absent; fi
output=$("$SCRIPT_DIR/local-start.sh")
printf '%s\n' "$output"
for key in URL RUNTIME_DIR TMUX_ATTACH STOP_COMMAND; do
  [[ $(printf '%s\n' "$output" | awk -F= -v key="$key" '$1 == key {count++} END {print count+0}') == 1 ]] || { printf 'launcher did not print exactly one %s line\n' "$key" >&2; exit 1; }
done
RUNTIME_DIR=$(printf '%s\n' "$output" | sed -n 's/^RUNTIME_DIR=//p')
[[ -d "$RUNTIME_DIR" && $(stat -Lc '%a' "$RUNTIME_DIR") == 700 ]] || { printf 'runtime directory contract failed\n' >&2; exit 1; }
# State is strict data; tests read only the fixed numeric identity fields.
BROKER_PID=$(sed -n 's/^BROKER_PID=//p' "$RUNTIME_DIR/state.env")
BROKER_START=$(sed -n 's/^BROKER_START=//p' "$RUNTIME_DIR/state.env")
FRONT_PID=$(sed -n 's/^FRONT_PID=//p' "$RUNTIME_DIR/state.env")
FRONT_START=$(sed -n 's/^FRONT_START=//p' "$RUNTIME_DIR/state.env")
ADAPTER_PID=$(sed -n 's/^ADAPTER_PID=//p' "$RUNTIME_DIR/state.env")
ADAPTER_START=$(sed -n 's/^ADAPTER_START=//p' "$RUNTIME_DIR/state.env")
ADAPTER_BIN=$(sed -n 's/^ADAPTER_BIN=//p' "$RUNTIME_DIR/state.env")
PORT=$(sed -n 's/^PORT=//p' "$RUNTIME_DIR/state.env")
[[ -r "/proc/$BROKER_PID/stat" && -r "/proc/$FRONT_PID/stat" && -r "/proc/$ADAPTER_PID/stat" ]] || { printf 'service processes are not live\n' >&2; exit 1; }
node - "$PORT" <<'NODE'
const http = require('node:http');
const port = Number(process.argv[2]);
function get(path) { return new Promise((resolve, reject) => http.get({host: '127.0.0.1', port, path, headers: {Host: `127.0.0.1:${port}`}}, response => { let body = ''; response.setEncoding('utf8'); response.on('data', chunk => { body += chunk; }); response.on('end', () => resolve({status: response.statusCode, body})); }).on('error', reject)); }
const noncePattern = /(<meta name="persea-style-nonce" content=")[^"]+(">)/g;
function normalizeNonce(body) {
  const matches = body.match(noncePattern);
  if (matches?.length !== 1) throw new Error('expected one style nonce');
  return body.replace(noncePattern, '$1__PERSEA_TEST_NONCE__$2');
}
(async () => { const root = await get('/'); const bundle = await get('/app.js'); const terminal = await get('/terminal'); if (root.status !== 200 || !root.body.includes('id="app"') || bundle.status !== 200 || !bundle.body.includes('/api/inventory') || !bundle.body.includes('/terminal') || terminal.status !== 200 || normalizeNonce(terminal.body) !== normalizeNonce(root.body)) process.exit(1); })().catch(() => process.exit(1));
NODE
# Sends one alias request with the CSRF cookie that the inventory sets and the
# live "persea" session's alias handle; prints "<status> <body>".
alias_request() {
  node - "$PORT" "$@" <<'NODE'
const http=require('node:http');const [portText,method,path,displayAlias,ifMatch]=process.argv.slice(2),port=Number(portText);
http.get({host:'127.0.0.1',port,path:'/api/inventory',headers:{Host:`127.0.0.1:${port}`}},r=>{let b='';r.on('data',c=>b+=c);r.on('end',()=>{
  const v=JSON.parse(b),s=v.realms?.[0]?.servers?.[0]?.sessions?.find(x=>x.name==='persea');
  const setCookie=[].concat(r.headers['set-cookie']??[]),cookie=setCookie.map(x=>x.split(';',1)[0]).find(x=>x.startsWith('__Host-persea-terminal-csrf='));
  if(!cookie||!s?.handles?.alias){console.error('alias request prerequisites unavailable');process.exit(1)}
  const body=JSON.stringify(method==='POST'?{display_alias:displayAlias,handle:s.handles.alias}:{display_alias:displayAlias});
  const headers={'Content-Type':'application/json','Content-Length':Buffer.byteLength(body),'Cookie':cookie,'Origin':`http://127.0.0.1:${port}`,'Sec-Fetch-Site':'same-origin','X-Persea-CSRF':cookie.slice(cookie.indexOf('=')+1),...(ifMatch?{'If-Match':ifMatch}:{})};
  const request=http.request({host:'127.0.0.1',port,path,method,headers},reply=>{let out='';reply.on('data',c=>out+=c);reply.on('end',()=>{process.stdout.write(`${reply.statusCode} ${out.trim()}`)})});
  request.on('error',()=>process.exit(1));request.end(body);
})}).on('error',()=>process.exit(1));
NODE
}
# Polls the inventory until the predicate holds for v (the inventory) and s (the
# live "persea" session); extra arguments reach it as args.
await_inventory() {
  local failure=$1 predicate=$2 _
  shift 2
  for _ in {1..100}; do
    node - "$PORT" "$predicate" "$@" <<'NODE' && return 0
const http=require('node:http');const [portText,predicate,...args]=process.argv.slice(2),port=Number(portText),test=new Function('v','s','args',`return (${predicate});`);
http.get({host:'127.0.0.1',port,path:'/api/inventory',headers:{Host:`127.0.0.1:${port}`}},r=>{let b='';r.on('data',c=>b+=c);r.on('end',()=>{let ok=false;try{const v=JSON.parse(b),s=v.realms?.[0]?.servers?.[0]?.sessions?.find(x=>x.name==='persea');ok=r.statusCode===200&&Boolean(test(v,s,args))}catch{}process.exit(ok?0:1)})}).on('error',()=>process.exit(1));
NODE
    sleep 0.05
  done
  printf '%s\n' "$failure" >&2
  exit 1
}
await_inventory 'canonical session identity missing' "s?.authority?.uid === process.getuid() && ['socket_name','socket_path'].includes(s.authority.selector_kind) && typeof s.authority.selector_value === 'string' && Number.isSafeInteger(s.authority.session_created) && s.authority.session_created > 0 && v.realms[0].servers[0].status === 'ok' && v.aliases.length === 0"
created=$(alias_request POST /api/aliases Local)
[[ $created == "201 "* ]] || { printf 'alias creation failed: %s\n' "$created" >&2; exit 1; }
ALIAS_PROOF=$(node -e 'const a=JSON.parse(process.argv[1]); if (a.display_alias !== "Local" || a.state !== "active" || a.session_name !== "persea" || a.revision !== 1) process.exit(1); process.stdout.write(`${a.alias_id}\t${JSON.stringify(a.session_incarnation)}`)' "${created#201 }")
IFS=$'\t' read -r ALIAS_ID ALIAS_INCARNATION <<<"$ALIAS_PROOF"
[[ -n "$ALIAS_ID" ]] || { printf 'created alias record is not as expected: %s\n' "$created" >&2; exit 1; }

# Restart both product processes against the same tmux source and durable store.
FRONT_BIN="$RUNTIME_DIR/persea-terminal"
BROKER_BIN="$RUNTIME_DIR/persea-terminal"
kill -TERM -- "$FRONT_PID" "$BROKER_PID"
for _ in {1..40}; do recorded_process_live "$FRONT_PID" "$FRONT_START" || break; sleep 0.05; done
recorded_process_live "$FRONT_PID" "$FRONT_START" && { printf 'front did not stop for persistence proof\n' >&2; exit 1; }
for _ in {1..40}; do recorded_process_live "$BROKER_PID" "$BROKER_START" || break; sleep 0.05; done
recorded_process_live "$BROKER_PID" "$BROKER_START" && { printf 'broker did not stop for persistence proof\n' >&2; exit 1; }
(cd "$SCRIPT_DIR/.." && setsid "$BROKER_BIN" broker --socket "$RUNTIME_DIR/broker.sock" --config "$RUNTIME_DIR/broker.json") >"$RUNTIME_DIR/broker.log" 2>&1 &
BROKER_PID=$!
BROKER_START=$(sed -n 's/^[^)]*) //p' "/proc/$BROKER_PID/stat" | awk '{print $20}')
for _ in {1..100}; do [[ -S "$RUNTIME_DIR/broker.sock" ]] && break; recorded_process_live "$BROKER_PID" "$BROKER_START" || { printf 'broker restart exited before readiness\n' >&2; exit 1; }; sleep 0.05; done
[[ -S "$RUNTIME_DIR/broker.sock" ]] || { printf 'broker restart socket was not ready\n' >&2; exit 1; }
(cd "$RUNTIME_DIR" && setsid "$FRONT_BIN" front --config "$RUNTIME_DIR/front.json" --static-dir ui) >"$RUNTIME_DIR/front.log" 2>&1 &
FRONT_PID=$!
FRONT_START=$(sed -n 's/^[^)]*) //p' "/proc/$FRONT_PID/stat" | awk '{print $20}')
BROKER_PGID=$(ps -o pgid= -p "$BROKER_PID" | tr -d ' ')
FRONT_PGID=$(ps -o pgid= -p "$FRONT_PID" | tr -d ' ')
sed -i "s/^BROKER_PID=.*/BROKER_PID=$BROKER_PID/; s/^BROKER_START=.*/BROKER_START=$BROKER_START/; s/^BROKER_PGID=.*/BROKER_PGID=$BROKER_PGID/; s/^FRONT_PID=.*/FRONT_PID=$FRONT_PID/; s/^FRONT_START=.*/FRONT_START=$FRONT_START/; s/^FRONT_PGID=.*/FRONT_PGID=$FRONT_PGID/" "$RUNTIME_DIR/state.env"
for _ in {1..100}; do
  node -e 'const http=require("node:http"),p=Number(process.argv[1]);const q=http.get({host:"127.0.0.1",port:p,path:"/api/inventory"},r=>{r.resume();process.exit(r.statusCode===200?0:1)});q.on("error",()=>process.exit(1))' "$PORT" && break
  sleep 0.05
done
persistence_ready=0
for _ in {1..100}; do
node - "$PORT" "$ALIAS_ID" "$ALIAS_INCARNATION" <<'NODE' && { persistence_ready=1; break; }
const http=require('node:http');const [portText,id,incarnation]=process.argv.slice(2),port=Number(portText);
http.get({host:'127.0.0.1',port,path:'/api/inventory',headers:{Host:`127.0.0.1:${port}`}},r=>{let b='';r.on('data',c=>b+=c);r.on('end',()=>{const v=JSON.parse(b),a=v.aliases?.find(x=>x.alias_id===id),s=v.realms?.[0]?.servers?.[0]?.sessions?.[0],ok=r.statusCode===200&&a&&JSON.stringify(a.session_incarnation)===incarnation&&a.state==='active'&&s?.alias==='Local';if(!ok)console.error(JSON.stringify({status:r.statusCode,alias_found:Boolean(a),incarnation_match:Boolean(a)&&JSON.stringify(a.session_incarnation)===incarnation,alias_state:a?.state,session_alias:s?.alias,realm_error:v.realms?.[0]?.error}));process.exit(ok?0:1)})}).on('error',()=>process.exit(1));
NODE
sleep 0.05
done
(( persistence_ready == 1 )) || { printf 'durable alias did not recover after product restart\n' >&2; exit 1; }
printf 'restart persistence PASS\n'

# Renaming changes the text only; a session keeps one alias.
renamed=$(alias_request PATCH "/api/aliases/$ALIAS_ID" 'Renamed Local' '"1"')
[[ $renamed == "200 "* ]] || { printf 'alias rename failed: %s\n' "$renamed" >&2; exit 1; }
second=$(alias_request POST /api/aliases Second)
[[ $second == "409 alias_exists" ]] || { printf 'a second alias for one session was not refused: %s\n' "$second" >&2; exit 1; }
await_inventory 'renamed alias is not the one active alias of its session' "v.aliases.length === 1 && v.aliases[0].alias_id === args[0] && v.aliases[0].display_alias === 'Renamed Local' && v.aliases[0].revision === 2 && v.aliases[0].state === 'active' && JSON.stringify(s?.authority) === args[1] && s.alias === 'Renamed Local'" "$ALIAS_ID" "$ALIAS_INCARNATION"
printf 'alias rename and one alias per session PASS\n'
if [[ -S "$default_socket" ]]; then default_after=$(stat -Lc '%d:%i:%s:%Y:%Z' "$default_socket"); else default_after=absent; fi
[[ "$default_before" == "$default_after" ]] || { printf 'default tmux socket changed\n' >&2; exit 1; }

source_identity=$(timeout -k 2 5 tmux -S "$RUNTIME_DIR/tmux.sock" display-message -p -t persea: '#{session_id}:#{window_id}:#{pane_id}:#{pane_pid}:#{pane_width}:#{pane_height}')
ws_lifecycle_probe() {
  "$ADAPTER_BIN" --probe "http://127.0.0.1:$PORT" --realm local --session persea --mode control --sentinel "$1"
}

abnormal_sentinel="persea-close-$RANDOM-$$"
ws_lifecycle_probe "$abnormal_sentinel"
# One source-owned observer stays alive across browser detach. It must be the
# broker's exact child, attached only to this source; no per-attach client remains.
observer_identity() {
  local rows pid rest parent start shadows sessions
  rows=$(timeout -k 2 5 tmux -S "$RUNTIME_DIR/tmux.sock" list-clients -F '#{client_pid}:#{session_name}') || return 1
  [[ "$rows" != *$'\n'* && "$rows" == *:persea ]] || return 1
  sessions=$(timeout -k 2 5 tmux -S "$RUNTIME_DIR/tmux.sock" list-sessions -F '#{session_name}') || return 1
  shadows=$(awk '/^persea-attach-/ { count++ } END { print count+0 }' <<<"$sessions") || return 1
  [[ "$shadows" == 0 ]] || return 1
  pid=${rows%%:*}
  [[ "$pid" =~ ^[0-9]+$ ]] || return 1
  rest=$(sed -n 's/^[^)]*) //p' "/proc/$pid/stat")
  parent=$(awk '{print $2}' <<<"$rest")
  start=$(awk '{print $20}' <<<"$rest")
  [[ "$parent" == "$BROKER_PID" && "$start" =~ ^[0-9]+$ ]] || return 1
  printf '%s:%s\n' "$rows" "$start"
}
observer_before=$(observer_identity) || { printf 'source observer ownership mismatch\n' >&2; exit 1; }
normal_sentinel="persea-reattach-$RANDOM-$$"
ws_lifecycle_probe "$normal_sentinel"
[[ $(observer_identity) == "$observer_before" ]] || { printf 'observer changed across browser reattach\n' >&2; exit 1; }
after_reattach_identity=$(timeout -k 2 5 tmux -S "$RUNTIME_DIR/tmux.sock" display-message -p -t persea: '#{session_id}:#{window_id}:#{pane_id}:#{pane_pid}:#{pane_width}:#{pane_height}')
[[ "$after_reattach_identity" == "$source_identity" ]] || { printf 'source identity or geometry changed across abnormal reattach\n' >&2; exit 1; }
timeout -k 2 5 tmux -S "$RUNTIME_DIR/tmux.sock" has-session -t persea
printf 'control close/reattach PASS\n'

# Replacing the session with one of the same name, as session restore does
# after a reboot, keeps the alias: it binds to the new incarnation.
timeout -k 2 5 tmux -S "$RUNTIME_DIR/tmux.sock" kill-session -t persea
timeout -k 2 5 tmux -S "$RUNTIME_DIR/tmux.sock" new-session -d -x 80 -y 24 -s persea
await_inventory 'alias did not follow the replaced session by name' "v.aliases.length === 1 && v.aliases[0].alias_id === args[0] && v.aliases[0].state === 'active' && JSON.stringify(s?.authority) !== args[1] && JSON.stringify(v.aliases[0].session_incarnation) === JSON.stringify(s.authority) && s.alias === 'Renamed Local'" "$ALIAS_ID" "$ALIAS_INCARNATION"
printf 'source replacement rebind PASS\n'

# A second build/runtime must not invalidate the first runtime's executable identity.
output2=$("$SCRIPT_DIR/local-start.sh")
RUNTIME_DIR2=$(printf '%s\n' "$output2" | sed -n 's/^RUNTIME_DIR=//p')
BROKER_PID2=$(sed -n 's/^BROKER_PID=//p' "$RUNTIME_DIR2/state.env")
BROKER_START2=$(sed -n 's/^BROKER_START=//p' "$RUNTIME_DIR2/state.env")
FRONT_PID2=$(sed -n 's/^FRONT_PID=//p' "$RUNTIME_DIR2/state.env")
FRONT_START2=$(sed -n 's/^FRONT_START=//p' "$RUNTIME_DIR2/state.env")
ADAPTER_PID2=$(sed -n 's/^ADAPTER_PID=//p' "$RUNTIME_DIR2/state.env")
ADAPTER_START2=$(sed -n 's/^ADAPTER_START=//p' "$RUNTIME_DIR2/state.env")
printf 'second runtime isolation start PASS\n'

stop_output=$("$SCRIPT_DIR/local-stop.sh" "$RUNTIME_DIR")
printf '%s\n' "$stop_output"
post_stop_clients=$(timeout -k 2 5 tmux -S "$RUNTIME_DIR/tmux.sock" list-clients -F '#{client_pid}') || { printf 'client listing failed after broker stop\n' >&2; exit 1; }
[[ -z "$post_stop_clients" ]] || { printf 'source observer survived broker stop\n' >&2; exit 1; }
if recorded_process_live "$BROKER_PID" "$BROKER_START" || recorded_process_live "$FRONT_PID" "$FRONT_START" || recorded_process_live "$ADAPTER_PID" "$ADAPTER_START"; then
  printf 'service process survived stop\n' >&2
  exit 1
fi
[[ -f "$RUNTIME_DIR/state.env" && -S "$RUNTIME_DIR/tmux.sock" ]] || { printf 'stop removed recovery data or tmux socket\n' >&2; exit 1; }
timeout -k 2 5 tmux -S "$RUNTIME_DIR/tmux.sock" has-session -t persea
printf 'first runtime exact stop PASS\n'
# Simulate the stopped front's recorded PID being reused; broker cleanup must continue.
kill -TERM -- "$FRONT_PID2"
for _ in {1..40}; do recorded_process_live "$FRONT_PID2" "$FRONT_START2" || break; sleep 0.05; done
recorded_process_live "$FRONT_PID2" "$FRONT_START2" && { printf 'second front did not stop for PID-reuse probe\n' >&2; exit 1; }
sed -i "s/^FRONT_PID=.*/FRONT_PID=$$/" "$RUNTIME_DIR2/state.env"
chmod 0600 "$RUNTIME_DIR2/state.env"
"$SCRIPT_DIR/local-stop.sh" "$RUNTIME_DIR2" >/dev/null
if recorded_process_live "$BROKER_PID2" "$BROKER_START2" || recorded_process_live "$FRONT_PID2" "$FRONT_START2" || recorded_process_live "$ADAPTER_PID2" "$ADAPTER_START2" || [[ ! -S "$RUNTIME_DIR2/tmux.sock" ]]; then
  printf 'second runtime stop contract failed\n' >&2
  exit 1
fi
printf 'PID reuse fail-closed stop PASS\n'

timeout -k 2 5 tmux -S "$RUNTIME_DIR/tmux.sock" kill-server
timeout -k 2 5 tmux -S "$RUNTIME_DIR2/tmux.sock" kill-server
for _ in {1..40}; do [[ ! -S "$RUNTIME_DIR/tmux.sock" ]] && break; sleep 0.05; done
rm -rf -- "$RUNTIME_DIR"
rm -rf -- "$RUNTIME_DIR2"
[[ ! -e "$RUNTIME_DIR" ]] || { printf 'exact test cleanup failed\n' >&2; exit 1; }
[[ ! -e "$RUNTIME_DIR2" ]] || { printf 'second exact test cleanup failed\n' >&2; exit 1; }
RUNTIME_DIR=
RUNTIME_DIR2=
printf 'local runtime test PASS\n'
