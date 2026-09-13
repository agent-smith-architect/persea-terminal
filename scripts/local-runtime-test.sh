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
ALIAS_PROOF=$(node - "$PORT" <<'NODE'
const http = require('node:http');
const port = Number(process.argv[2]);
http.get({host:'127.0.0.1', port, path:'/api/inventory', headers:{Host:`127.0.0.1:${port}`}}, response => {
  let body=''; response.on('data', chunk => body += chunk); response.on('end', () => {
    const value=JSON.parse(body); const server=value.realms?.[0]?.servers?.[0];
    const session=server?.sessions?.[0], alias=value.aliases?.[0];
    if (response.statusCode === 200 && server?.status === 'ok' && session?.name === 'persea' &&
        Number.isSafeInteger(session?.authority?.uid) && session.authority.uid === process.getuid() &&
        ['socket_name','socket_path'].includes(session?.authority?.selector_kind) && typeof session.authority.selector_value === 'string' &&
        Number.isSafeInteger(session?.authority?.session_created) && session.authority.session_created > 0 &&
        alias?.display_alias === 'Local' && alias?.state === 'active' && alias?.alias_id) {
      process.stdout.write(`${alias.alias_id}\t${JSON.stringify(alias.session_incarnation)}`); return;
    }
    process.exitCode=1;
  });
}).on('error', () => process.exit(1));
NODE
)
IFS=$'\t' read -r ALIAS_ID ALIAS_INCARNATION <<<"$ALIAS_PROOF"
[[ -n "$ALIAS_ID" ]] || { printf 'canonical identity or seeded durable alias missing\n' >&2; exit 1; }

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

# Renaming the configured seed causes the next inventory to re-seed Local. Both
# distinct ACTIVE records must remain byte-exact matches for the live incarnation.
node - "$PORT" "$ALIAS_ID" <<'NODE'
const http=require('node:http');const [portText,id]=process.argv.slice(2),port=Number(portText);
http.get({host:'127.0.0.1',port,path:'/api/inventory'},r=>{let b='';r.on('data',c=>b+=c);r.on('end',()=>{const v=JSON.parse(b),s=v.realms?.[0]?.servers?.[0]?.sessions?.[0],setCookie=Array.isArray(r.headers['set-cookie'])?r.headers['set-cookie']:(r.headers['set-cookie']?[r.headers['set-cookie']]:[]),cookie=setCookie.map(x=>x.split(';',1)[0]).find(x=>x.startsWith('__Host-persea-terminal-csrf='));if(!cookie||!s?.handles?.alias){console.error('alias mutation prerequisites unavailable');process.exit(1)}const csrf=cookie.slice(cookie.indexOf('=')+1),body=JSON.stringify({display_alias:'Renamed Local',handle:s.handles.alias});const request=http.request({host:'127.0.0.1',port,path:`/api/aliases/${encodeURIComponent(id)}`,method:'PATCH',headers:{'Content-Type':'application/json','Content-Length':Buffer.byteLength(body),'If-Match':'"1"','Cookie':cookie,'Origin':`http://127.0.0.1:${port}`,'Sec-Fetch-Site':'same-origin','X-Persea-CSRF':csrf}},reply=>{reply.resume();reply.on('end',()=>{if(reply.statusCode!==200)console.error(`alias mutation status ${reply.statusCode}`);process.exit(reply.statusCode===200?0:1)})});request.on('error',()=>process.exit(1));request.end(body)})}).on('error',()=>process.exit(1));
NODE
printf 'alias mutation request PASS\n'
for _ in {1..100}; do
  node - "$PORT" "$ALIAS_ID" "$ALIAS_INCARNATION" <<'NODE' && break
const http=require('node:http');const [portText,renamedId,incarnation]=process.argv.slice(2),port=Number(portText);
http.get({host:'127.0.0.1',port,path:'/api/inventory',headers:{Host:`127.0.0.1:${port}`}},r=>{let b='';r.on('data',c=>b+=c);r.on('end',()=>{const v=JSON.parse(b),matches=v.aliases?.filter(a=>a?.state==='active'&&JSON.stringify(a?.session_incarnation)===incarnation)??[],renamed=matches.find(a=>a.alias_id===renamedId),seeded=matches.find(a=>a.alias_id!==renamedId&&a.display_alias==='Local'),s=v.realms?.[0]?.servers?.[0]?.sessions?.find(x=>x.name==='persea'),ok=r.statusCode===200&&matches.length===2&&renamed?.display_alias==='Renamed Local'&&renamed?.revision===2&&seeded?.revision===1&&JSON.stringify(s?.authority)===incarnation;if(!ok)console.error(JSON.stringify({status:r.statusCode,match_count:matches.length,renamed_name:renamed?.display_alias,renamed_revision:renamed?.revision,seeded:Boolean(seeded),session_authority_match:JSON.stringify(s?.authority)===incarnation}));process.exit(ok?0:1)})}).on('error',()=>process.exit(1));
NODE
  sleep 0.05
done
node - "$PORT" "$ALIAS_ID" "$ALIAS_INCARNATION" <<'NODE'
const http=require('node:http');const [portText,renamedId,incarnation]=process.argv.slice(2),port=Number(portText);
http.get({host:'127.0.0.1',port,path:'/api/inventory',headers:{Host:`127.0.0.1:${port}`}},r=>{let b='';r.on('data',c=>b+=c);r.on('end',()=>{const v=JSON.parse(b),matches=v.aliases?.filter(a=>a?.state==='active'&&JSON.stringify(a?.session_incarnation)===incarnation)??[],renamed=matches.find(a=>a.alias_id===renamedId),seeded=matches.find(a=>a.alias_id!==renamedId&&a.display_alias==='Local'),s=v.realms?.[0]?.servers?.[0]?.sessions?.find(x=>x.name==='persea');process.exit(r.statusCode===200&&matches.length===2&&renamed?.display_alias==='Renamed Local'&&renamed?.revision===2&&seeded?.revision===1&&JSON.stringify(s?.authority)===incarnation?0:1)})}).on('error',()=>process.exit(1));
NODE
printf 'alias reseed/CAS PASS\n'
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

# The test operator replaces the disposable source; the product must not transfer the alias.
timeout -k 2 5 tmux -S "$RUNTIME_DIR/tmux.sock" kill-session -t persea
timeout -k 2 5 tmux -S "$RUNTIME_DIR/tmux.sock" new-session -d -x 80 -y 24 -s persea
for _ in {1..100}; do
  node - "$PORT" "$ALIAS_ID" "$ALIAS_INCARNATION" <<'NODE' && break
const http=require('node:http');const [portText,id,oldIncarnation]=process.argv.slice(2),port=Number(portText);
http.get({host:'127.0.0.1',port,path:'/api/inventory',headers:{Host:`127.0.0.1:${port}`}},r=>{let b='';r.on('data',c=>b+=c);r.on('end',()=>{const v=JSON.parse(b),a=v.aliases?.find(x=>x.alias_id===id),s=v.realms?.[0]?.servers?.[0]?.sessions?.find(x=>x.name==='persea');process.exit(r.statusCode===200&&a?.state==='tombstone'&&JSON.stringify(a?.session_incarnation)===oldIncarnation&&s&&s.alias!=='Renamed Local'&&JSON.stringify(s.authority)!==oldIncarnation?0:1)})}).on('error',()=>process.exit(1));
NODE
  sleep 0.05
done
node - "$PORT" "$ALIAS_ID" "$ALIAS_INCARNATION" <<'NODE'
const http=require('node:http');const [portText,id,oldIncarnation]=process.argv.slice(2),port=Number(portText);
http.get({host:'127.0.0.1',port,path:'/api/inventory',headers:{Host:`127.0.0.1:${port}`}},r=>{let b='';r.on('data',c=>b+=c);r.on('end',()=>{const v=JSON.parse(b),a=v.aliases?.find(x=>x.alias_id===id),s=v.realms?.[0]?.servers?.[0]?.sessions?.find(x=>x.name==='persea');process.exit(r.statusCode===200&&a?.state==='tombstone'&&JSON.stringify(a?.session_incarnation)===oldIncarnation&&s&&s.alias!=='Renamed Local'&&JSON.stringify(s.authority)!==oldIncarnation?0:1)})}).on('error',()=>process.exit(1));
NODE
printf 'source replacement tombstone PASS\n'

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
