#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib.sh"

(($# == 0)) || persea_die 'login-tailscale-sidecar.sh takes no arguments or auth keys'
persea_init_root
persea_require_root
if [[ $PERSEA_HERMETIC == 1 ]]; then persea_require_hermetic_mocks tailscale systemctl persea-process-metadata; fi
for command in tailscale systemctl python3 mktemp cmp; do command -v "$command" >/dev/null || persea_die "required command is missing: $command"; done
"$SCRIPT_DIR/verify.sh" --require-sidecar >/dev/null
persea_assert_sidecar_runtime

work=$(mktemp -d "${TMPDIR:-/tmp}/persea-terminal-sidecar-login.XXXXXXXX")
persea_capture_main_serve "$work/main-before.json"
finish_login() {
  local rc=$? check_rc=0
  trap - EXIT
  set +e
  persea_capture_main_serve "$work/main-after.json" || check_rc=1
  persea_assert_main_serve_unchanged "$work/main-before.json" "$work/main-after.json" || check_rc=1
  rm -rf -- "$work"
  ((check_rc == 0)) || rc=1
  exit "$rc"
}
trap finish_login EXIT

persea_sidecar_tailscale status --json >"$work/status-before.json"
python3 - "$work/status-before.json" "$PERSEA_PROTECTED_MAIN_DNS_NAME" "$PERSEA_SIDECAR_TAG" <<'PY'
import json, sys
status = json.load(open(sys.argv[1], encoding="utf-8"))
self_node = status.get("Self") or {}
dns_name = self_node.get("DNSName", "").rstrip(".")
protected_main, expected_tag = sys.argv[2:]
assert dns_name != protected_main, "selected endpoint is the protected main-node identity"
tags = self_node.get("Tags") or []
assert tags in ([], [expected_tag]), f"selected endpoint has unexpected tags: {tags!r}"
PY
persea_sidecar_tailscale up --hostname="$PERSEA_SIDECAR_HOSTNAME" --advertise-tags="$PERSEA_SIDECAR_TAG"
persea_sidecar_tailscale status --json >"$work/status-after.json"
python3 - "$work/status-after.json" "$PERSEA_PROTECTED_MAIN_DNS_NAME" "$PERSEA_SIDECAR_TAG" "$PERSEA_TAILNET_SUFFIX" <<'PY'
import json, sys
status = json.load(open(sys.argv[1], encoding="utf-8"))
self_node = status.get("Self") or {}
tags = self_node.get("Tags") or []
protected_main, expected_tag, expected_suffix = sys.argv[2:]
assert tags == [expected_tag], f"sidecar tags mismatch after login: {tags!r}"
dns_name = self_node.get("DNSName", "").rstrip(".")
assert dns_name and dns_name != protected_main, "sidecar selected the protected main-node identity"
suffix = (status.get("MagicDNSSuffix") or (status.get("CurrentTailnet") or {}).get("MagicDNSSuffix") or "").rstrip(".")
assert suffix == expected_suffix, f"sidecar logged into unexpected tailnet: {suffix}"
assert dns_name.endswith("." + suffix), f"sidecar DNS identity is outside expected tailnet: {dns_name}"
assert status.get("BackendState") == "Running", f"sidecar is not running after login: {status.get('BackendState')}"
PY
printf 'SIDECAR_IDENTITY=verified\nHOSTNAME_REQUEST=%s\nTAG=%s\n' "$PERSEA_SIDECAR_HOSTNAME" "$PERSEA_SIDECAR_TAG"
