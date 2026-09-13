#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib.sh"

host_config=
output=
while (($#)); do
  case $1 in
    --host-config) [[ $# -ge 2 ]] || persea_die 'missing --host-config value'; host_config=$2; shift 2 ;;
    --output) [[ $# -ge 2 ]] || persea_die 'missing --output value'; output=$2; shift 2 ;;
    *) persea_die "unknown argument: $1" ;;
  esac
done
[[ $host_config == /* && $output == /* ]] || persea_die 'usage: generate-candidate.sh --host-config ABSOLUTE_PATH --output ABSOLUTE_PATH'
persea_generate_candidate "$output" "$host_config"
printf 'CANDIDATE=%s\n' "$output"
