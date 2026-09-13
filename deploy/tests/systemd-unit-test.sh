#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
DEPLOY_DIR=$(cd -- "$SCRIPT_DIR/.." && pwd -P)
for command in systemd-analyze install mktemp; do command -v "$command" >/dev/null || { printf 'missing %s\n' "$command" >&2; exit 1; }; done

TMP=$(mktemp -d "${TMPDIR:-/tmp}/persea-terminal-systemd.XXXXXXXX")
cleanup() {
  local rc=$?
  chmod -R u+w -- "$TMP" 2>/dev/null || true
  rm -rf -- "$TMP"
  exit "$rc"
}
trap cleanup EXIT

candidate="$TMP/candidate"
root="$TMP/root"
unit_root="$root/etc/systemd/system"
"$DEPLOY_DIR/generate-candidate.sh" --host-config "$DEPLOY_DIR/host.example.json" --output "$candidate" >/dev/null
mkdir -p "$unit_root" "$root/opt/persea-terminal/releases/test/bin" "$root/usr/sbin"
printf 'test\n' >"$root/opt/persea-terminal/releases/test/bin/persea-terminal"
chmod 0555 "$root/opt/persea-terminal/releases/test/bin/persea-terminal"
printf '#!/usr/bin/env bash\nexit 0\n' >"$root/usr/sbin/tailscaled"
chmod 0555 "$root/usr/sbin/tailscaled"
ln -s releases/test "$root/opt/persea-terminal/current"
install -m 0444 "$candidate"/units/*.service "$unit_root/"
python3 - "$candidate/config/host.json" "$root/etc/passwd" "$root/etc/group" <<'PY'
import json, sys
manifest = json.load(open(sys.argv[1], encoding="utf-8"))
front = manifest["front"]
accounts = {front["user"]: front["uid"]}
accounts.update({realm["user"]: realm["uid"] for realm in manifest["realms"]})
with open(sys.argv[2], "w", encoding="utf-8") as handle:
    handle.write("root:x:0:0:root:/root:/bin/sh\n")
    for user, uid in accounts.items():
        handle.write(f"{user}:x:{uid}:{front['gid']}::/nonexistent/{user}:/bin/sh\n")
with open(sys.argv[3], "w", encoding="utf-8") as handle:
    handle.write("root:x:0:\n")
    handle.write(f"{front['group']}:x:{front['gid']}:{','.join(accounts)}\n")
PY
for target in sysinit.target basic.target multi-user.target network-online.target; do
  printf '[Unit]\nDescription=%s\n' "$target" >"$unit_root/$target"
done

mapfile -t managed_units <"$candidate/config/managed-units"
systemd-analyze --root="$root" verify "${managed_units[@]}" persea-terminal-tailscaled.service
guarded="$candidate/units/persea-terminal-broker-desk-a7.service"
grep -Fxq 'TemporaryFileSystem=/run/persea-terminal-desk-a7/unified-journal:rw,nosuid,nodev,noexec,noswap,size=96M,nr_inodes=256,mode=0700,uid=42001,gid=42003' "$guarded"
grep -Fxq 'MemoryHigh=850M' "$guarded"
grep -Fxq 'MemoryMax=1G' "$guarded"
grep -Fxq 'LimitCORE=0' "$guarded"
unguarded="$candidate/units/persea-terminal-broker-lab-k4.service"
! grep -Eq '^(TemporaryFileSystem|MemoryHigh|MemoryMax|LimitCORE)=' "$unguarded"
grep -Fxq 'ReadWritePaths=/var/lib/persea-terminal-staging/desk-a7' "$guarded"
! grep -Eq '^ReadWritePaths=' "$unguarded"
# host.example.json enables staging, so this pins the incident fix: the front
# unit keeps the alias store's exact-0700 state directory and never gains
# image directives.
grep -Fxq 'StateDirectoryMode=0700' "$candidate/units/persea-terminal-front.service"
! grep -q 'persea-terminal-staging' "$candidate/units/persea-terminal-front.service"
for unit in "$candidate"/units/*.service; do
  report="$TMP/$(basename "$unit").security"
  systemd-analyze security --offline=yes --no-pager "$unit" >"$report"
  score=$(sed -n 's/.*Overall exposure level.*: \([0-9][0-9.]*\) .*/\1/p' "$report")
  [[ -n $score ]] || { printf 'missing systemd security score for %s\n' "$unit" >&2; exit 1; }
  limit=3.0
  [[ $(basename "$unit") != persea-terminal-tailscaled.service ]] || limit=4.5
  awk -v score="$score" -v limit="$limit" 'BEGIN { exit !(score <= limit) }' || { printf 'systemd exposure %s exceeds %s for %s\n' "$score" "$limit" "$unit" >&2; exit 1; }
done
printf 'systemd unit syntax and offline security: PASS\n'
