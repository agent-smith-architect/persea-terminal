#!/usr/bin/env bash
set -euo pipefail

SECOND_USER=${1:-}
[[ -n "$SECOND_USER" ]] || { printf 'usage: %s SECOND_UNIX_USER\n' "$0" >&2; exit 2; }
[[ $(uname -s) == Linux ]] || { printf 'Linux is required\n' >&2; exit 1; }
for command in go tmux node python3 pgrep sudo stat setsid timeout; do command -v "$command" >/dev/null || { printf 'missing %s\n' "$command" >&2; exit 1; }; done
NODE_BIN=$(command -v node)
id "$SECOND_USER" >/dev/null
CALLER_UID=$(id -u)
SECOND_UID=$(id -u "$SECOND_USER")
[[ $CALLER_UID != "$SECOND_UID" ]] || { printf 'second user must have a distinct UID\n' >&2; exit 1; }
SHARED_GROUP=
while IFS= read -r group; do
	if id -nG "$SECOND_USER" | tr ' ' '\n' | grep -Fxq "$group"; then SHARED_GROUP=$group; break; fi
done < <(id -nG | tr ' ' '\n')
[[ -n $SHARED_GROUP ]] || { printf 'caller and %s have no shared supplementary group\n' "$SECOND_USER" >&2; exit 1; }

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
PROJECT_DIR=$(cd -- "$SCRIPT_DIR/.." && pwd -P)
ROOT=$(mktemp -d /tmp/persea-terminal-two-realm.XXXXXXXX)
chgrp "$SHARED_GROUP" "$ROOT"
chmod 0710 "$ROOT"
LOCAL="$ROOT/local"
REMOTE="$ROOT/remote"
SHARED="$ROOT/shared"
BIN="$ROOT/persea-terminal"
ADAPTER_BIN="$ROOT/persea-terminal-ingress-test"
WRONG_BROKER_SOCKET="$LOCAL/wrong-broker.sock"
WRONG_BROKER_BYTES="$LOCAL/wrong-broker.bytes"
PIDS=()
STARTS=()
PGIDS=()

start_time() { sudo -n sed -n 's/^[^)]*) //p' "/proc/$1/stat" | awk '{print $20}'; }
record_pid() { PIDS+=("$1"); STARTS+=("$(start_time "$1")"); PGIDS+=("${2:-$(sudo -n ps -o pgid= -p "$1" | tr -d ' ')}"); }
same_process() { sudo -n test -r "/proc/$1/stat" && [[ $(start_time "$1") == "$2" ]]; }
cleanup() {
  local rc=$? i
  trap - EXIT
	if ((rc != 0)); then
		for log in "$LOCAL"/*.log; do [[ -r $log ]] && { printf '%s:\n' "$log" >&2; tail -n 20 "$log" >&2; }; done
		sudo -n -u "$SECOND_USER" sh -c 'printf "remote-config:\n"; cat "$1"; printf "access:\n"; stat -Lc "%A %a %u:%g %n" "$2" "$3" "$4" "$5"' _ "$REMOTE/broker.json" "$ROOT" "$REMOTE" "$SHARED" "$BIN" >&2 || true
	fi
  for ((i=${#PIDS[@]}-1; i>=0; i--)); do
    if same_process "${PIDS[i]}" "${STARTS[i]}"; then sudo -n kill -TERM -- "-${PGIDS[i]}" 2>/dev/null || true; fi
  done
  for ((i=0; i<50; i++)); do
    local live=0 j; for ((j=0; j<${#PIDS[@]}; j++)); do same_process "${PIDS[j]}" "${STARTS[j]}" && live=1; done
    ((live == 0)) && break; sleep .05
  done
  timeout -k 2 5 tmux -S "$LOCAL/tmux.sock" kill-server >/dev/null 2>&1 || true
  sudo -n -u "$SECOND_USER" timeout -k 2 5 tmux -S "$REMOTE/tmux.sock" kill-server >/dev/null 2>&1 || true
  sudo -n rm -rf -- "$ROOT"
  exit "$rc"
}
trap cleanup EXIT

default_identity() {
	local uid=$1 user=${2:-}
	local socket="/tmp/tmux-$uid/default"
  if [[ -n "$user" ]]; then sudo -n -u "$user" bash -c 's=$1; if [[ -S $s ]]; then stat -Lc "%d:%i:%s:%Y:%Z" "$s"; tmux -S "$s" list-sessions -F "#{session_id}:#{session_name}:#{session_created}" 2>/dev/null || true; else echo absent; fi' _ "$socket"
  elif [[ -S $socket ]]; then { stat -Lc '%d:%i:%s:%Y:%Z' "$socket"; tmux -S "$socket" list-sessions -F '#{session_id}:#{session_name}:#{session_created}' 2>/dev/null || true; }
  else echo absent; fi
}
DEFAULT_CALLER_BEFORE=$(default_identity "$CALLER_UID")
DEFAULT_SECOND_BEFORE=$(default_identity "$SECOND_UID" "$SECOND_USER")

mkdir -m 0700 "$LOCAL"
sudo -n install -d -o "$SECOND_USER" -g "$SHARED_GROUP" -m 0700 "$REMOTE"
sudo -n install -d -o "$SECOND_USER" -g "$SHARED_GROUP" -m 2710 "$SHARED"
(cd "$PROJECT_DIR" && go build -o "$BIN" ./cmd/persea-terminal)
(cd "$PROJECT_DIR" && go build -o "$ADAPTER_BIN" ./cmd/persea-terminal-ingress-test)
chmod 0755 "$BIN" "$ADAPTER_BIN"
tmux -S "$LOCAL/tmux.sock" -f /dev/null new-session -d -s local -x 120 -y 40
sudo -n -u "$SECOND_USER" tmux -S "$REMOTE/tmux.sock" -f /dev/null new-session -d -s remote -x 120 -y 40

LOCAL_ID=$(tmux -S "$LOCAL/tmux.sock" display-message -p -t local: '#{socket_path}:#{session_id}:#{window_id}:#{pane_id}:#{pane_pid}:#{pane_width}:#{pane_height}')
REMOTE_ID=$(sudo -n -u "$SECOND_USER" tmux -S "$REMOTE/tmux.sock" display-message -p -t remote: '#{socket_path}:#{session_id}:#{window_id}:#{pane_id}:#{pane_pid}:#{pane_width}:#{pane_height}')
[[ $LOCAL_ID == *':120:40' && $REMOTE_ID == *':120:40' ]]

source_id() {
	local socket=$1 session=$2 user=${3:-} row server_pid socket_id server_start pane_start
	if [[ -n $user ]]; then
		row=$(sudo -n -u "$user" tmux -S "$socket" display-message -p -t "=$session:" -F $'#{session_id}\t#{window_id}\t#{pane_id}\t#{pane_pid}\t#{pane_width}\t#{pane_height}')
		server_pid=$(sudo -n -u "$user" tmux -S "$socket" display-message -p -F '#{pid}')
		socket_id=$(sudo -n stat -Lc '%d:%i' "$socket")
	else
		row=$(tmux -S "$socket" display-message -p -t "=$session:" -F $'#{session_id}\t#{window_id}\t#{pane_id}\t#{pane_pid}\t#{pane_width}\t#{pane_height}')
		server_pid=$(tmux -S "$socket" display-message -p -F '#{pid}')
		socket_id=$(stat -Lc '%d:%i' "$socket")
	fi
	server_start=$(start_time "$server_pid")
	IFS=$'\t' read -r _ _ _ pane_pid _ _ <<<"$row"
	pane_start=$(start_time "$pane_pid")
	node - "$socket" "$socket_id" "$server_pid" "$server_start" "$row" "$pane_start" <<'NODE'
const crypto=require('node:crypto');
const [socket,socketID,serverPID,serverStart,row,paneStart]=process.argv.slice(2);
const [dev,ino]=socketID.split(':'); const [session,window,pane,panePID,cols,rows]=row.split('\t');
const identity=[socket,dev,ino,serverPID,serverStart,session,window,pane,panePID,paneStart,cols,rows].join('\0');
process.stdout.write(crypto.createHash('sha256').update(identity).digest('base64url'));
NODE
}
LOCAL_SOURCE=$(source_id "$LOCAL/tmux.sock" "\$0")
REMOTE_SOURCE=$(source_id "$REMOTE/tmux.sock" "\$0" "$SECOND_USER")
[[ -n "$LOCAL_SOURCE" && -n "$REMOTE_SOURCE" ]] || { printf 'source identity hashing failed\n' >&2; exit 1; }
PORT=$(node -e 'const n=require("node:net").createServer();n.listen(0,"127.0.0.1",()=>{console.log(n.address().port);n.close()})')
FRONT_SOCKET="$LOCAL/front.sock"
OPERATOR_LOGIN=two-realm-operator@example.test

mkdir -m 0700 "$LOCAL/unified-journal"
sudo -n -u "$SECOND_USER" mkdir -m 0700 "$REMOTE/unified-journal"
printf '{"realm":"local","front_uid":%s,"servers":[{"label":"private","socket_path":"%s"}],"session_create":{"enabled":true,"servers":["private"]},"unified_terminal_dev":{"enabled":true,"server":"private","session":"local","observer_session":"observer-local","runtime_dir":"%s"}}\n' "$CALLER_UID" "$LOCAL/tmux.sock" "$LOCAL/unified-journal" >"$LOCAL/broker.json"
sudo -n -u "$SECOND_USER" python3 - "$CALLER_UID" "$REMOTE/tmux.sock" "$REMOTE/unified-journal" "$REMOTE/broker.json" <<'PY'
import json, sys
uid, socket, runtime, output = sys.argv[1:]
with open(output, "w") as stream:
    json.dump({"realm":"remote","front_uid":int(uid),"servers":[{"label":"private","socket_path":socket}],"session_create":{"enabled":True,"servers":["private"]},"unified_terminal_dev":{"enabled":True,"server":"private","session":"remote","observer_session":"observer-remote","runtime_dir":runtime}},stream)
PY
printf '{"ingress":{"socket_path":"%s","peer_uid":%s,"canonical_host":"127.0.0.1:%s","operator_login":"%s","max_connections":64},"realms":[{"name":"local","socket":"%s","broker_uid":%s},{"name":"remote","socket":"%s","broker_uid":%s},{"name":"denied","socket":"%s","broker_uid":%s}],"alias_store_path":"%s","handle_ttl_seconds":60}\n' "$FRONT_SOCKET" "$CALLER_UID" "$PORT" "$OPERATOR_LOGIN" "$LOCAL/broker.sock" "$CALLER_UID" "$SHARED/broker.sock" "$SECOND_UID" "$WRONG_BROKER_SOCKET" "$SECOND_UID" "$LOCAL/aliases.json" >"$LOCAL/front.json"
chmod 0600 "$LOCAL/broker.json" "$LOCAL/front.json"
sudo -n -u "$SECOND_USER" chmod 0600 "$REMOTE/broker.json"

setsid "$BIN" broker --socket "$LOCAL/broker.sock" --config "$LOCAL/broker.json" >"$LOCAL/broker.log" 2>&1 &
LOCAL_BROKER_PID=$!
record_pid "$LOCAL_BROKER_PID"
# The caller intentionally owns this diagnostic log outside the second UID's private directory.
# shellcheck disable=SC2024
sudo -n -u "$SECOND_USER" setsid "$BIN" broker --socket "$SHARED/broker.sock" --config "$REMOTE/broker.json" >"$LOCAL/remote-broker.log" 2>&1 &
for _ in {1..100}; do [[ -S "$LOCAL/broker.sock" && -S "$SHARED/broker.sock" ]] && break; sleep .05; done
[[ -S "$LOCAL/broker.sock" && -S "$SHARED/broker.sock" ]] || { printf 'broker sockets did not become ready\n' >&2; exit 1; }
REMOTE_BROKER_PID=$(sudo -n pgrep -u "$SECOND_UID" -f "^$BIN broker --socket $SHARED/broker.sock --config $REMOTE/broker.json$" || true)
[[ $REMOTE_BROKER_PID =~ ^[0-9]+$ ]] || { printf 'could not resolve exact remote broker PID: %s\n' "$REMOTE_BROKER_PID" >&2; exit 1; }
record_pid "$REMOTE_BROKER_PID"
[[ $(stat -Lc '%a:%u' "$LOCAL/broker.sock") == "600:$CALLER_UID" ]]
[[ $(stat -Lc '%a:%u:%G' "$SHARED/broker.sock") == "660:$SECOND_UID:$SHARED_GROUP" ]]

# A real wrong front UID may connect to the cross-UID broker, but receives no protocol byte.
sudo -n -u "$SECOND_USER" "$NODE_BIN" - "$SHARED/broker.sock" <<'NODE'
const net=require('node:net'); const socket=process.argv[2]; let connected=false, bytes=0;
const fail=m=>{console.error(m);process.exit(1)}; const timer=setTimeout(()=>fail('wrong-front denial timeout'),3000);
const c=net.createConnection(socket,()=>{connected=true;c.write(Buffer.from([1,0,0,0,0]))});
c.on('data',b=>{bytes+=b.length});
c.on('error',e=>{if(!connected)fail(`wrong front did not connect: ${e.message}`)});
c.on('close',()=>{clearTimeout(timer);if(!connected)fail('wrong front was never connected');if(bytes!==0)fail(`broker emitted ${bytes} protocol bytes`) });
NODE

# A caller-owned Unix peer impersonates the expected second-UID broker.  It records
# every byte so the front-side credential gate is proven to precede hello/request.
node - "$WRONG_BROKER_SOCKET" "$WRONG_BROKER_BYTES" <<'NODE' &
const fs=require('node:fs'),net=require('node:net');const [socket,out]=process.argv.slice(2);let bytes=0;
const server=net.createServer(c=>{c.on('data',b=>bytes+=b.length);c.on('close',()=>server.close())});
server.on('close',()=>fs.writeFileSync(out,String(bytes)));server.listen(socket);
NODE
record_pid $!
for _ in {1..100}; do [[ -S $WRONG_BROKER_SOCKET ]] && break; sleep .05; done
[[ -S $WRONG_BROKER_SOCKET ]] || { printf 'wrong-broker recorder did not become ready\n' >&2; exit 1; }
(cd "$PROJECT_DIR" && setsid "$BIN" front --config "$LOCAL/front.json" --static-dir ui/dist) >"$LOCAL/front.log" 2>&1 & record_pid $!
for _ in {1..100}; do [[ -S "$FRONT_SOCKET" ]] && break; sleep .05; done
[[ -S "$FRONT_SOCKET" ]] || { printf 'front socket did not become ready\n' >&2; exit 1; }
(cd "$PROJECT_DIR" && setsid "$ADAPTER_BIN" --listen "127.0.0.1:$PORT" --socket "$FRONT_SOCKET" --operator "$OPERATOR_LOGIN") >"$LOCAL/adapter.log" 2>&1 & record_pid $!

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
(async () => { let root; for (let i = 0; i < 100; i++) { try { root = await get('/'); if (root.status === 200) break; } catch {} await new Promise(resolve => setTimeout(resolve, 50)); } const bundle = await get('/app.js'); const terminal = await get('/terminal'); if (!root || root.status !== 200 || !root.body.includes('id="app"') || bundle.status !== 200 || !bundle.body.includes('/api/inventory') || !bundle.body.includes('/api/aliases') || terminal.status !== 200 || normalizeNonce(terminal.body) !== normalizeNonce(root.body)) process.exit(1); })().catch(() => process.exit(1));
NODE

node - "$PORT" <<'NODE'
const http=require('node:http');const port=Number(process.argv[2]),fail=m=>{console.error(m);process.exit(1)};
const inventory=()=>new Promise((resolve,reject)=>http.get({host:'127.0.0.1',port,path:'/api/inventory'},r=>{let b='';r.on('data',x=>b+=x);r.on('end',()=>{try{resolve(JSON.parse(b))}catch(e){reject(e)}})}).on('error',reject));
(async()=>{let v,by;for(let i=0;i<100;i++){try{v=await inventory();by=Object.fromEntries(v.realms.map(r=>[r.name,r]));if(by.local?.servers?.[0]?.sessions?.[0]?.handles?.control&&by.remote?.servers?.[0]?.sessions?.[0]?.handles?.control&&by.denied?.error)break}catch{}await new Promise(r=>setTimeout(r,50))}if(!v||v.realms.length!==3)fail('negative inventory did not expose all realms');if(Object.keys(by).sort().join(',')!=='denied,local,remote')fail('realm names mismatch');if(!by.denied.error||by.denied.servers.length!==0||JSON.stringify(by.denied).includes('handles'))fail('denied realm was not explicitly degraded and handle-free');const local=by.local.servers[0].sessions[0].handles,remote=by.remote.servers[0].sessions[0].handles;if(local.control===remote.control||local.observe===remote.observe)fail('realm capabilities not distinct')})().catch(e=>fail(e.message));
NODE

# Observe the exact per-source unit, not a legacy per-browser client lifetime.
observer_identity() {
  local socket=$1 session=$2 broker=$3 user=${4:-} rows pid rest parent start shadows sessions
  if [[ -n "$user" ]]; then
    rows=$(sudo -n -u "$user" timeout -k 2 5 tmux -S "$socket" list-clients -F '#{client_pid}:#{session_name}') || return 1
    sessions=$(sudo -n -u "$user" timeout -k 2 5 tmux -S "$socket" list-sessions -F '#{session_name}') || return 1
  else
    rows=$(timeout -k 2 5 tmux -S "$socket" list-clients -F '#{client_pid}:#{session_name}') || return 1
    sessions=$(timeout -k 2 5 tmux -S "$socket" list-sessions -F '#{session_name}') || return 1
  fi
  shadows=$(awk '/^persea-attach-/ { count++ } END { print count+0 }' <<<"$sessions") || return 1
  [[ "$shadows" == 0 && "$rows" != *$'\n'* && "$rows" == *:"$session" ]] || return 1
  pid=${rows%%:*}
  [[ "$pid" =~ ^[0-9]+$ ]] || return 1
  rest=$(sudo -n sed -n 's/^[^)]*) //p' "/proc/$pid/stat")
  parent=$(awk '{print $2}' <<<"$rest")
  start=$(awk '{print $20}' <<<"$rest")
  [[ "$parent" == "$broker" && "$start" =~ ^[0-9]+$ ]] || return 1
  printf '%s:%s\n' "$rows" "$start"
}
"$ADAPTER_BIN" --probe "http://127.0.0.1:$PORT" --realm local --session local --mode control --sentinel LOCAL_UID_BOUNDARY_OK
"$ADAPTER_BIN" --probe "http://127.0.0.1:$PORT" --realm remote --session remote --mode control --sentinel REMOTE_UID_BOUNDARY_OK
LOCAL_OBSERVER=$(observer_identity "$LOCAL/tmux.sock" local "$LOCAL_BROKER_PID")
REMOTE_OBSERVER=$(observer_identity "$REMOTE/tmux.sock" remote "$REMOTE_BROKER_PID" "$SECOND_USER")
"$ADAPTER_BIN" --probe "http://127.0.0.1:$PORT" --realm local --session local --mode observe
"$ADAPTER_BIN" --probe "http://127.0.0.1:$PORT" --realm remote --session remote --mode observe

for _ in {1..100}; do [[ -s $WRONG_BROKER_BYTES ]] && break; sleep .05; done
[[ $(<"$WRONG_BROKER_BYTES") == 0 ]] || { printf 'front emitted bytes to wrong broker: %s\n' "$(<"$WRONG_BROKER_BYTES")" >&2; exit 1; }

[[ $(observer_identity "$LOCAL/tmux.sock" local "$LOCAL_BROKER_PID") == "$LOCAL_OBSERVER" ]]
[[ $(observer_identity "$REMOTE/tmux.sock" remote "$REMOTE_BROKER_PID" "$SECOND_USER") == "$REMOTE_OBSERVER" ]]
[[ $(tmux -S "$LOCAL/tmux.sock" display-message -p -t local: '#{socket_path}:#{session_id}:#{window_id}:#{pane_id}:#{pane_pid}:#{pane_width}:#{pane_height}') == "$LOCAL_ID" ]]
[[ $(sudo -n -u "$SECOND_USER" tmux -S "$REMOTE/tmux.sock" display-message -p -t remote: '#{socket_path}:#{session_id}:#{window_id}:#{pane_id}:#{pane_pid}:#{pane_width}:#{pane_height}') == "$REMOTE_ID" ]]

# Stop only recorded product process groups; both private tmux servers must survive.
for ((i=${#PIDS[@]}-1; i>=0; i--)); do same_process "${PIDS[i]}" "${STARTS[i]}" && sudo -n kill -TERM -- "-${PGIDS[i]}"; done
for _ in {1..100}; do LIVE=0; for ((i=0;i<${#PIDS[@]};i++)); do same_process "${PIDS[i]}" "${STARTS[i]}" && LIVE=1; done; ((LIVE==0)) && break; sleep .05; done
for _ in {1..40}; do [[ ! -e "$FRONT_SOCKET" ]] && break; sleep .05; done
[[ ! -e "$FRONT_SOCKET" ]] || { printf 'front socket survived two-realm stop\n' >&2; exit 1; }
local_clients_after_stop=$(timeout -k 2 5 tmux -S "$LOCAL/tmux.sock" list-clients -F '#{client_pid}') || { printf 'local client listing failed after broker stop\n' >&2; exit 1; }
remote_clients_after_stop=$(sudo -n -u "$SECOND_USER" timeout -k 2 5 tmux -S "$REMOTE/tmux.sock" list-clients -F '#{client_pid}') || { printf 'remote client listing failed after broker stop\n' >&2; exit 1; }
[[ -z "$local_clients_after_stop" ]]
[[ -z "$remote_clients_after_stop" ]]
tmux -S "$LOCAL/tmux.sock" has-session -t local
sudo -n -u "$SECOND_USER" tmux -S "$REMOTE/tmux.sock" has-session -t remote
[[ $(default_identity "$CALLER_UID") == "$DEFAULT_CALLER_BEFORE" ]]
[[ $(default_identity "$SECOND_UID" "$SECOND_USER") == "$DEFAULT_SECOND_BEFORE" ]]
printf 'two-realm runtime test PASS local=%s remote=%s residual_clients=0 residual_shadows=0\n' "$LOCAL_ID" "$REMOTE_ID"
