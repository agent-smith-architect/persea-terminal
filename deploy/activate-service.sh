#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib.sh"

(($# == 0)) || persea_die 'activate-service.sh takes no arguments'
persea_init_root
persea_require_root
if [[ $PERSEA_HERMETIC == 1 ]]; then persea_require_hermetic_mocks tailscale systemctl runuser; fi
for command in tailscale systemctl python3 stat sha256sum cmp date sleep sync; do command -v "$command" >/dev/null || persea_die "required command is missing: $command"; done
command -v runuser >/dev/null || persea_die 'required command is missing: runuser'

persea_require_tailscale_cli
persea_require_tailscaled

install_root=$(persea_path "$PERSEA_INSTALL_ROOT")
front_socket=$(persea_path "$PERSEA_FRONT_SOCKET")
front_parent=$(dirname -- "$front_socket")
[[ -L $install_root/current ]] || persea_die 'installed current release is missing'
current_target=$(readlink -- "$install_root/current")
[[ $current_target == releases/* && $current_target != *'..'* && -d $install_root/$current_target && ! -L $install_root/$current_target ]] || persea_die 'installed current release is unsafe'
release="$install_root/$current_target"
front_config="$install_root/current/config/front.json"
[[ -f $front_config && ! -L $front_config ]] || persea_die 'front config is missing or unsafe'
canonical_host=$(python3 - "$front_config" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as handle:
    value = json.load(handle)
print(value["ingress"]["canonical_host"])
PY
)
persea_validate_host "$canonical_host" || persea_die 'installed canonical host is invalid'
probe_helper="$release/$PERSEA_PROBE_HELPER"
[[ -f $probe_helper && ! -L $probe_helper ]] || persea_die 'installed probe helper is missing or unsafe'
read -r helper_owner helper_mode < <(stat -Lc '%u %a' -- "$probe_helper")
expected_owner=0; [[ $PERSEA_HERMETIC == 1 ]] && expected_owner=$(id -u)
[[ $helper_owner == "$expected_owner" && $helper_mode == 444 ]] || persea_die 'installed probe helper metadata mismatch'

"$SCRIPT_DIR/verify.sh" --require-active --require-sidecar >/dev/null || persea_die 'exact active runtime and sidecar verification failed'
persea_assert_sidecar_runtime

work=$(mktemp -d "${TMPDIR:-/tmp}/persea-terminal-activate.XXXXXXXX")
cleanup_work() { rm -rf -- "$work"; }
trap cleanup_work EXIT
persea_capture_main_serve "$work/main-serve-before.json"
persea_sidecar_tailscale status --json >"$work/status.json"
persea_sidecar_tailscale debug prefs >"$work/prefs.json"
python3 - "$work/status.json" "$PERSEA_SERVICE" "$canonical_host" "$PERSEA_SIDECAR_TAG" "$PERSEA_PROTECTED_MAIN_DNS_NAME" "$PERSEA_TAILNET_SUFFIX" <<'PY'
import json, sys
status = json.load(open(sys.argv[1], encoding="utf-8"))
service, host, expected_tag, protected_main, expected_suffix = sys.argv[2:]
assert status.get("BackendState") == "Running", "sidecar is not running"
self_node = status.get("Self") or {}
tags = self_node.get("Tags") or []
assert tags == [expected_tag], f"hosting identity tags mismatch: {tags!r}"
assert self_node.get("DNSName", "").rstrip(".") != protected_main, "protected main node cannot host the Service"
suffix = (status.get("MagicDNSSuffix") or (status.get("CurrentTailnet") or {}).get("MagicDNSSuffix") or "").rstrip(".")
assert suffix == expected_suffix, f"unexpected MagicDNS suffix: {suffix}"
expected = service.removeprefix("svc:") + "." + suffix
assert host == expected, f"Service FQDN mismatch: {host} != {expected}"
PY

[[ -d $front_parent && ! -L $front_parent ]] || persea_die 'front socket parent is unsafe'
[[ -S $front_socket && ! -L $front_socket ]] || persea_die 'front socket is missing or unsafe'
socket_before=$(stat -Lc '%d:%i' -- "$front_socket")
sleep 0.05
[[ $(stat -Lc '%d:%i' -- "$front_socket") == "$socket_before" ]] || persea_die 'front socket inode changed during preflight'

python3 "$probe_helper" --socket "$front_socket" --host "$canonical_host" --login "$PERSEA_OPERATOR" || persea_die 'root-peer direct Unix probe failed'
runuser -u "$PERSEA_FRONT_USER" -- python3 "$probe_helper" --socket "$front_socket" --host "$canonical_host" --login "$PERSEA_OPERATOR" --expect-denied || persea_die 'peer-UID mismatch probe failed'
python3 "$probe_helper" --socket "$front_socket" --host "$canonical_host" --login "$PERSEA_OPERATOR" || persea_die 'post-denial root-peer direct Unix probe failed'

read -r deployments deployments_identity < <(persea_prepare_evidence_root)
persea_assert_evidence_root "$deployments" "$deployments_identity"
stamp=$(date -u +%Y%m%dT%H%M%SZ)-$$
evidence="$deployments/$stamp"
if [[ $PERSEA_HERMETIC == 1 ]]; then mkdir -m 0700 -- "$evidence"; else install -d -o root -g root -m 0700 "$evidence"; fi
persea_assert_evidence_root "$deployments" "$deployments_identity"
persea_write_tailscale_evidence "$evidence/tailscale-version.json"
persea_sidecar_tailscale serve status --json >"$evidence/node-serve-before.json"
sha256sum "$evidence/node-serve-before.json" >"$evidence/node-serve-before.sha256"
cp -- "$work/main-serve-before.json" "$evidence/main-serve-before.json"
cp -- "$work/status.json" "$evidence/tailscale-status-before.json"
cp -- "$work/prefs.json" "$evidence/tailscale-prefs-before.json"
python3 - "$evidence/node-serve-before.json" "$PERSEA_SERVICE" <<'PY'
import json, sys
config = json.load(open(sys.argv[1], encoding="utf-8"))
assert sys.argv[2] not in (config.get("Services") or {}), "target Service already has local config"
PY
persea_capture_main_serve "$work/main-serve-pre-mutation.json"
persea_assert_main_serve_unchanged "$work/main-serve-before.json" "$work/main-serve-pre-mutation.json"

mutated=0
rollback_in_progress=0
rollback_service() {
  local rc=$1 cleanup_rc=0
  trap - ERR INT TERM HUP
  if ((rollback_in_progress)); then exit "$rc"; fi
  rollback_in_progress=1
  set +e
  if ((mutated)); then
    persea_assert_evidence_root "$deployments" "$deployments_identity" || cleanup_rc=1
    persea_sidecar_tailscale serve drain "$PERSEA_SERVICE" || cleanup_rc=1
    persea_assert_evidence_root "$deployments" "$deployments_identity" || cleanup_rc=1
    persea_sidecar_tailscale serve clear "$PERSEA_SERVICE" || cleanup_rc=1
    persea_assert_evidence_root "$deployments" "$deployments_identity" || cleanup_rc=1
    persea_sidecar_tailscale serve status --json >"$evidence/node-serve-after-rollback.json" || cleanup_rc=1
    if ! cmp -s "$evidence/node-serve-before.json" "$evidence/node-serve-after-rollback.json"; then
      printf 'node Serve state changed during failed activation rollback\n' >&2
      cleanup_rc=1
    fi
    persea_capture_main_serve "$evidence/main-serve-after-rollback.json" || cleanup_rc=1
    persea_assert_main_serve_unchanged "$evidence/main-serve-before.json" "$evidence/main-serve-after-rollback.json" || cleanup_rc=1
  fi
  if ((cleanup_rc)); then printf 'persea-terminal deploy: Service rollback verification failed\n' >&2; fi
  exit "$rc"
}
mutated=1
trap 'rollback_service $?' ERR
trap 'rollback_service 130' INT
trap 'rollback_service 143' TERM
trap 'rollback_service 129' HUP
persea_assert_evidence_root "$deployments" "$deployments_identity"
persea_sidecar_tailscale serve --service="$PERSEA_SERVICE" --https=443 --yes "unix:$PERSEA_FRONT_SOCKET"
persea_assert_evidence_root "$deployments" "$deployments_identity"
persea_sidecar_tailscale serve --service="$PERSEA_SERVICE" --http=80 --yes "unix:$PERSEA_FRONT_SOCKET"

convergence_attempts=30
if [[ $PERSEA_HERMETIC == 1 ]]; then convergence_attempts=${PERSEA_TEST_CONVERGENCE_ATTEMPTS:-2}; fi
converged=0
for ((attempt = 1; attempt <= convergence_attempts; attempt++)); do
  persea_assert_evidence_root "$deployments" "$deployments_identity"
  persea_sidecar_tailscale status --json >"$evidence/tailscale-status-after.json"
  persea_sidecar_tailscale debug prefs >"$evidence/tailscale-prefs-after.json"
  if python3 - "$evidence/tailscale-status-after.json" "$evidence/tailscale-prefs-after.json" "$PERSEA_SERVICE" "$canonical_host" "$PERSEA_SIDECAR_TAG" "$PERSEA_PROTECTED_MAIN_DNS_NAME" "$PERSEA_TAILNET_SUFFIX" <<'PY'
import json, sys
status = json.load(open(sys.argv[1], encoding="utf-8"))
prefs = json.load(open(sys.argv[2], encoding="utf-8"))
service, host, expected_tag, protected_main, expected_suffix = sys.argv[3:]
assert status.get("BackendState") == "Running", "sidecar stopped running"
self_node = status.get("Self") or {}
assert self_node.get("Tags") == [expected_tag], "hosting identity tags drifted"
assert self_node.get("DNSName", "").rstrip(".") != protected_main, "protected main node cannot host the Service"
assert service in (prefs.get("AdvertiseServices") or []), "Service advertisement did not converge"
def control_has_service(value):
    if isinstance(value, dict):
        if service in value and value[service]:
            return True
        return any(control_has_service(child) for child in value.values())
    if isinstance(value, list):
        return any(control_has_service(child) for child in value)
    return False
assert control_has_service(self_node.get("CapMap") or {}), "Service approval/control mapping did not converge"
suffix = (status.get("MagicDNSSuffix") or (status.get("CurrentTailnet") or {}).get("MagicDNSSuffix") or "").rstrip(".")
assert suffix == expected_suffix, "MagicDNS suffix drifted"
assert host == service.removeprefix("svc:") + "." + suffix, "Service DNS/FQDN did not converge"
PY
  then
    converged=1
    break
  fi
  ((attempt == convergence_attempts)) || sleep 1
done
if (( ! converged )); then
  printf 'persea-terminal deploy: Service advertisement/approval/readback did not converge\n' >&2
  false
fi

persea_assert_evidence_root "$deployments" "$deployments_identity"
persea_sidecar_tailscale serve status --json >"$evidence/node-serve-after.json"
python3 "$SCRIPT_DIR/verify-service-config.py" "$evidence/node-serve-before.json" "$evidence/node-serve-after.json" "$PERSEA_SERVICE" "$canonical_host" "unix:$PERSEA_FRONT_SOCKET"
persea_capture_main_serve "$evidence/main-serve-after.json"
persea_assert_main_serve_unchanged "$evidence/main-serve-before.json" "$evidence/main-serve-after.json"
printf 'https://%s/\n' "$canonical_host" >"$evidence/url"
chmod 0400 "$evidence"/*
sync -f "$evidence/url"
sync -f "$evidence"
trap - ERR INT TERM HUP
printf 'URL=https://%s/\nEVIDENCE=%s\n' "$canonical_host" "$evidence"
