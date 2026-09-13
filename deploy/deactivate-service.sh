#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib.sh"

(($# == 0)) || persea_die 'deactivate-service.sh takes no arguments'
persea_init_root
persea_require_root
if [[ $PERSEA_HERMETIC == 1 ]]; then persea_require_hermetic_mocks tailscale systemctl; fi
for command in tailscale systemctl python3 cmp date; do command -v "$command" >/dev/null || persea_die "required command is missing: $command"; done
persea_require_tailscale_cli
persea_require_tailscaled
"$SCRIPT_DIR/verify.sh" --require-sidecar >/dev/null || persea_die 'exact sidecar verification failed'
persea_assert_sidecar_runtime

read -r deployments deployments_identity < <(persea_prepare_evidence_root)
persea_assert_evidence_root "$deployments" "$deployments_identity"
evidence="$deployments/deactivate-$(date -u +%Y%m%dT%H%M%SZ)-$$"
if [[ $PERSEA_HERMETIC == 1 ]]; then mkdir -m 0700 -- "$evidence"; else install -d -o root -g root -m 0700 "$evidence"; fi
persea_assert_evidence_root "$deployments" "$deployments_identity"
persea_write_tailscale_evidence "$evidence/tailscale-version.json"
persea_capture_main_serve "$evidence/main-serve-before.json"
persea_sidecar_tailscale status --json >"$evidence/tailscale-status-before.json"
python3 - "$evidence/tailscale-status-before.json" "$PERSEA_SIDECAR_TAG" "$PERSEA_PROTECTED_MAIN_DNS_NAME" "$PERSEA_TAILNET_SUFFIX" <<'PY'
import json, sys
status = json.load(open(sys.argv[1], encoding="utf-8"))
assert status.get("BackendState") == "Running", "sidecar is not running"
self_node = status.get("Self") or {}
expected_tag, protected_main, expected_suffix = sys.argv[2:]
assert self_node.get("Tags") == [expected_tag], "sidecar tag identity mismatch"
assert self_node.get("DNSName", "").rstrip(".") != protected_main, "protected main node cannot host the Service"
suffix = (status.get("MagicDNSSuffix") or (status.get("CurrentTailnet") or {}).get("MagicDNSSuffix") or "").rstrip(".")
assert suffix == expected_suffix, "sidecar tailnet mismatch"
PY
persea_sidecar_tailscale serve status --json >"$evidence/node-serve-before.json"

mutated=0
rollback_in_progress=0
finish_clear() {
  local rc=$1 cleanup_rc=0
  trap - ERR INT TERM HUP
  if ((rollback_in_progress)); then exit "$rc"; fi
  rollback_in_progress=1
  set +e
  if ((mutated)); then
    persea_sidecar_tailscale serve drain "$PERSEA_SERVICE" || cleanup_rc=1
    persea_sidecar_tailscale serve clear "$PERSEA_SERVICE" || cleanup_rc=1
    persea_sidecar_tailscale serve status --json >"$evidence/node-serve-after-cleanup.json" || cleanup_rc=1
    python3 - "$evidence/node-serve-after-cleanup.json" "$PERSEA_SERVICE" <<'PY' || cleanup_rc=1
import json, sys
config = json.load(open(sys.argv[1], encoding="utf-8"))
assert sys.argv[2] not in (config.get("Services") or {}), "target Service remains after deactivation cleanup"
PY
  fi
  persea_capture_main_serve "$evidence/main-serve-after-cleanup.json" || cleanup_rc=1
  persea_assert_main_serve_unchanged "$evidence/main-serve-before.json" "$evidence/main-serve-after-cleanup.json" || cleanup_rc=1
  if ((cleanup_rc)); then printf 'persea-terminal deploy: Service deactivation cleanup verification failed\n' >&2; fi
  exit "$rc"
}

mutated=1
trap 'finish_clear $?' ERR
trap 'finish_clear 130' INT
trap 'finish_clear 143' TERM
trap 'finish_clear 129' HUP
persea_assert_evidence_root "$deployments" "$deployments_identity"
persea_sidecar_tailscale serve drain "$PERSEA_SERVICE"
persea_assert_evidence_root "$deployments" "$deployments_identity"
persea_sidecar_tailscale serve clear "$PERSEA_SERVICE"
persea_assert_evidence_root "$deployments" "$deployments_identity"
persea_sidecar_tailscale serve status --json >"$evidence/node-serve-after.json"
python3 - "$evidence/node-serve-before.json" "$evidence/node-serve-after.json" "$PERSEA_SERVICE" <<'PY'
import json, sys
before = json.load(open(sys.argv[1], encoding="utf-8"))
after = json.load(open(sys.argv[2], encoding="utf-8"))
service = sys.argv[3]
before_services = dict(before.pop("Services", {}) or {})
after_services = dict(after.pop("Services", {}) or {})
before_services.pop(service, None)
assert service not in after_services, "target Service remains configured"
assert after_services == before_services, "another Service changed"
assert after == before, "sidecar Node Serve state changed outside the target Service"
PY
persea_capture_main_serve "$evidence/main-serve-after.json"
persea_assert_main_serve_unchanged "$evidence/main-serve-before.json" "$evidence/main-serve-after.json"
chmod 0400 "$evidence"/*
trap - ERR INT TERM HUP
printf 'DEACTIVATED=%s\nEVIDENCE=%s\n' "$PERSEA_SERVICE" "$evidence"
