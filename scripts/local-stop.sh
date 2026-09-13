#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
[[ $# == 1 ]] || { printf 'usage: %s RUNTIME_DIR\n' "$0" >&2; exit 2; }
RUNTIME_DIR=$1
STATE_FILE="$RUNTIME_DIR/state.env"
[[ "$RUNTIME_DIR" == /tmp/persea-terminal.* && -d "$RUNTIME_DIR" && ! -L "$RUNTIME_DIR" ]] || { printf 'invalid runtime directory\n' >&2; exit 1; }
[[ $(LC_ALL=C stat -Lc '%F:%u:%a' "$RUNTIME_DIR") == "directory:$(id -u):700" ]] || { printf 'runtime directory must be private and owned by this user\n' >&2; exit 1; }
[[ -f "$STATE_FILE" && ! -L "$STATE_FILE" ]] || { printf 'state file is not a regular file\n' >&2; exit 1; }
[[ $(LC_ALL=C stat -Lc '%F:%u:%a' "$STATE_FILE") == "regular file:$(id -u):600" ]] || { printf 'state file must be private and owned by this user\n' >&2; exit 1; }

keys=(VERSION RUNTIME_DIR PROJECT_DIR BIN ADAPTER_BIN TMUX_SOCKET SESSION BROKER_SOCKET FRONT_SOCKET BROKER_PID BROKER_START BROKER_EXE BROKER_PGID FRONT_PID FRONT_START FRONT_EXE FRONT_PGID ADAPTER_PID ADAPTER_START ADAPTER_EXE ADAPTER_PGID PORT)
declare -A allowed=() seen=()
for key in "${keys[@]}"; do allowed[$key]=1; done
while IFS= read -r line || [[ -n "$line" ]]; do
  [[ "$line" != *$'\r'* && "$line" =~ ^([A-Z][A-Z0-9_]*)=(.*)$ ]] || { printf 'malformed state record\n' >&2; exit 1; }
  key=${BASH_REMATCH[1]}; value=${BASH_REMATCH[2]}
  [[ ${allowed[$key]+yes} && ! ${seen[$key]+yes} ]] || { printf 'unknown or duplicate state key\n' >&2; exit 1; }
  seen[$key]=1
  printf -v "$key" '%s' "$value"
done <"$STATE_FILE"
for key in "${keys[@]}"; do [[ ${seen[$key]+yes} ]] || { printf 'missing state key %s\n' "$key" >&2; exit 1; }; done

expected_project=$(cd -- "$SCRIPT_DIR/.." && pwd -P)
[[ "$VERSION" == 2 && "$RUNTIME_DIR" == "$1" && "$PROJECT_DIR" == "$expected_project" && "$BIN" == "$RUNTIME_DIR/persea-terminal" && "$ADAPTER_BIN" == "$RUNTIME_DIR/persea-terminal-ingress-test" ]] || { printf 'state provenance mismatch\n' >&2; exit 1; }
[[ "$TMUX_SOCKET" == "$RUNTIME_DIR/tmux.sock" && "$BROKER_SOCKET" == "$RUNTIME_DIR/broker.sock" && "$FRONT_SOCKET" == "$RUNTIME_DIR/front.sock" && "$SESSION" == persea ]] || { printf 'state runtime paths mismatch\n' >&2; exit 1; }

# Match the kernel process incarnation, including a non-dumpable broker.
# EXE fields below validate launch provenance, not readable /proc/PID/exe links.
read_identity() {
  local pid=$1 line rest
  local -a fields
  [[ "$pid" =~ ^[1-9][0-9]*$ && -r "/proc/$pid/stat" ]] || return 1
  IFS= read -r line <"/proc/$pid/stat" || return 1
  [[ "$line" == *') '* ]] || return 1
  rest=${line##*) }
  read -r -a fields <<<"$rest"
  (( ${#fields[@]} > 19 )) || return 1
  ACTUAL_START=${fields[19]}
  ACTUAL_PGID=${fields[2]}
  [[ "$ACTUAL_START" =~ ^[1-9][0-9]*$ && "$ACTUAL_PGID" =~ ^[0-9]+$ ]]
}

process_exists() {
  local pid=$1 line rest state
  [[ -r "/proc/$pid/stat" ]] || return 1
  IFS= read -r line <"/proc/$pid/stat" || return 1
  rest=${line##*) }; state=${rest%% *}
  [[ "$state" != Z && "$state" != X ]]
}

matches() {
  read_identity "$1" && [[ "$ACTUAL_START" == "$2" && "$ACTUAL_PGID" == "$3" && "$ACTUAL_PGID" == "$1" ]]
}

stop_process() {
  local role=$1 pid=$2 start=$3 exe=$4 pgid=$5 expected_exe=$6 i
  [[ "$pid" =~ ^[1-9][0-9]*$ && "$start" =~ ^[1-9][0-9]*$ && "$pgid" =~ ^[1-9][0-9]*$ && "$exe" == "$expected_exe" ]] || { printf 'invalid %s identity\n' "$role" >&2; return 1; }
  process_exists "$pid" || return 0
  matches "$pid" "$start" "$pgid" || { printf 'recorded %s identity is gone; not signaling reused PID %s\n' "$role" "$pid" >&2; return 0; }
  kill -TERM -- "-$pgid"
  for ((i=0; i<40; i++)); do
    process_exists "$pid" || return 0
    matches "$pid" "$start" "$pgid" || { process_exists "$pid" || return 0; printf 'refusing further signals to %s: identity changed\n' "$role" >&2; return 1; }
    sleep 0.05
  done
  matches "$pid" "$start" "$pgid" || return 0
  kill -KILL -- "-$pgid"
  for ((i=0; i<40; i++)); do process_exists "$pid" || return 0; sleep 0.05; done
  process_exists "$pid" && { printf '%s did not stop\n' "$role" >&2; return 1; }
}

stop_process adapter "$ADAPTER_PID" "$ADAPTER_START" "$ADAPTER_EXE" "$ADAPTER_PGID" "$ADAPTER_BIN"
stop_process front "$FRONT_PID" "$FRONT_START" "$FRONT_EXE" "$FRONT_PGID" "$BIN"
stop_process broker "$BROKER_PID" "$BROKER_START" "$BROKER_EXE" "$BROKER_PGID" "$BIN"
for _ in {1..40}; do [[ ! -e "$FRONT_SOCKET" ]] && break; sleep 0.05; done
[[ ! -e "$FRONT_SOCKET" ]] || { printf 'front socket survived exact stop\n' >&2; exit 1; }
timeout -k 2 5 tmux -S "$TMUX_SOCKET" has-session -t "$SESSION" 2>/dev/null || { printf 'private tmux recovery session is unavailable\n' >&2; exit 1; }

printf 'RUNTIME_DIR=%s\n' "$RUNTIME_DIR"
printf 'TMUX_ATTACH=tmux -S %q attach-session -t %q\n' "$TMUX_SOCKET" "$SESSION"
printf 'RECOVERY_COMMAND=tmux -S %q attach-session -t %q\n' "$TMUX_SOCKET" "$SESSION"
