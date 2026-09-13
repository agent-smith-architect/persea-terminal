#!/usr/bin/env bash
# Makes the preferences store readable by an older release. It performs two
# independent repairs, both required by the same property: every release loads
# this file with DisallowUnknownFields and a closed schema, so ANY field the
# reader does not know about — or any value outside its range — rejects the
# WHOLE FILE.
#
# Repair 1 — the J-UX-9 font tri-state. Since J-UX-9 the store carries
# `"font_size": null` for an operator whose font is on auto. A pre-J-UX-9 binary
# decodes that JSON null into a plain `int` as a no-op zero, then fails its own
# 9…24 range check. This script rewrites those to an explicit 14.
#
# Repair 2 — the UX-9 composer face. Since UX-9 a record this release writes
# carries `"composer_font_size"`. A release older than UX-9 has never heard of
# that key and its strict decode rejects it. This script deletes the key; the
# affected operators fall back to the composer face their older release
# hardcodes, and rolling forward reads them as the UX-9 default again.
#
# Either way the blast radius is the same and it is why this script exists: one
# unreadable record takes every other operator's theme and default session down
# with it — GET degrades to defaults with `available:false` and every PUT is 503
# until the file is repaired or the newer binary is restored.
#
# Nothing is destroyed: the old binary refuses to load the file rather than
# rewriting it, so rolling forward instead of running this script also works.
# After this script runs, the operators it touched hold an explicit 14 — the
# pre-J-UX-9 default — which the newer binary reads back as a deliberate size,
# not as auto. That is the honest cost of the downgrade and it is stated in the
# summary this script prints.
set -Eeuo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib.sh"

# The pre-J-UX-9 default, and the value this script writes. It is inside the
# 9…24 range every release in scope accepts.
DOWNGRADE_FONT_SIZE=14
FRONT_UNIT=persea-terminal-front.service
DEFAULT_STORE=/var/lib/persea-terminal/preferences.json

die() { printf 'persea-terminal preferences downgrade: %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<'USAGE'
Usage: preferences-store-downgrade.sh [--force] [--dry-run] [STORE_PATH]

  Rewrites "auto" font preferences to an explicit 14 and removes the UX-9
  composer_font_size key, so that an older release can read the store.

  STORE_PATH  the preferences store to rewrite. Defaults to
              /var/lib/persea-terminal/preferences.json — the front's own 0700
              StateDirectory, beside the alias store.
  --force     proceed even though the front unit is active. The front owns this
              file and rewrites it on the next PUT, so a rewrite underneath a
              live front can be silently reverted; stop the unit first.
  --dry-run   report what would change and leave every file alone.

Rolling back past the font tri-state or the UX-9 composer face:
  systemctl stop persea-terminal-front.service
  deploy/preferences-store-downgrade.sh
  deploy/rollback.sh --to-release <older> --activate-local

Rolling forward again needs nothing: an explicit size is valid in every release.
USAGE
}

force=0
dry_run=0
store=
while (($# > 0)); do
  case $1 in
    --force) force=1 ;;
    --dry-run) dry_run=1 ;;
    -h | --help) usage; exit 0 ;;
    --) shift; break ;;
    -*) die "unknown option: $1" ;;
    *)
      [[ -z $store ]] || die 'exactly one store path may be given'
      store=$1
      ;;
  esac
  shift
done
(($# == 0)) || die 'exactly one store path may be given'

# The store is addressed directly, so the host manifest is deliberately NOT
# loaded: this script has to run on a host that is mid-rollback, where the
# manifest may already describe a different release. Only the hermetic root
# prefix and the effective-uid rule are borrowed from the deploy library.
PERSEA_HERMETIC=${PERSEA_DEPLOY_HERMETIC:-0}
PERSEA_ROOT_PREFIX=${PERSEA_DEPLOY_ROOT:-}
if [[ $PERSEA_HERMETIC == 1 ]]; then
  [[ $PERSEA_ROOT_PREFIX == /* && $PERSEA_ROOT_PREFIX != / ]] || die 'hermetic root must be an absolute non-root path'
  [[ -d $PERSEA_ROOT_PREFIX && ! -L $PERSEA_ROOT_PREFIX ]] || die 'hermetic root must be an existing real directory'
  PERSEA_ROOT_PREFIX=$(cd -- "$PERSEA_ROOT_PREFIX" && pwd -P)
else
  [[ -z $PERSEA_ROOT_PREFIX ]] || die 'alternate roots require PERSEA_DEPLOY_HERMETIC=1'
fi
[[ -n $store ]] || store=$(persea_path "$DEFAULT_STORE")

[[ $(persea_effective_uid) == 0 ]] || die 'root execution is required'
for command in python3 install mv cmp sync mktemp date stat chmod; do
  command -v "$command" >/dev/null || die "required command is missing: $command"
done

[[ $store == /* ]] || die "store path must be absolute: $store"
[[ ! -L $store ]] || die "store path is a symlink: $store"
[[ -f $store ]] || die "store file is missing or not a regular file: $store"
store_dir=${store%/*}
[[ -d $store_dir && ! -L $store_dir ]] || die "store directory is missing or unsafe: $store_dir"

# The front owns this file and republishes it on every PUT, so rewriting it
# underneath a live front is a lost update waiting to happen.
if [[ $PERSEA_HERMETIC == 1 ]]; then persea_require_hermetic_mocks systemctl; fi
if command -v systemctl >/dev/null; then
  front_state=$(systemctl is-active "$FRONT_UNIT" 2>/dev/null || true)
  if [[ $front_state == active ]]; then
    ((force == 1)) || die "$FRONT_UNIT is active; stop it first, or pass --force"
    printf 'persea-terminal preferences downgrade: WARNING %s is active and may overwrite this rewrite\n' "$FRONT_UNIT" >&2
  fi
elif ((force == 0)); then
  die 'systemctl is unavailable, so the front unit state cannot be checked; pass --force to proceed anyway'
fi

work=$(mktemp -d "$store_dir/.ptpd.XXXXXXXX")
chmod 0700 "$work"
cleanup() {
  local rc=$?
  trap - EXIT
  rm -rf -- "$work"
  exit "$rc"
}
trap cleanup EXIT

# The rewrite touches exactly two things: the value of font_size where it is
# null, and the presence of composer_font_size. No key is added and none is
# reordered, because the older loader decodes with DisallowUnknownFields and
# would reject a file that grew a field. Every other field — theme, default
# session, revision, the timestamps — is carried through untouched.
summary=$(python3 - "$store" "$work/next.json" "$DOWNGRADE_FONT_SIZE" <<'PY'
import json, sys

src, dst, default_size = sys.argv[1], sys.argv[2], int(sys.argv[3])
# The closed shape every release in scope knows, plus the keys this script is
# here to remove. A record carrying anything else is not one this script
# understands, and guessing at it would be worse than stopping.
KEYS = {"operator", "theme", "font_size", "default_session", "revision", "created_at", "updated_at"}
REMOVABLE = {"composer_font_size"}

with open(src, "rb") as handle:
    raw = handle.read()
try:
    doc = json.loads(raw)
except ValueError as err:
    sys.exit("store is not valid JSON: %s" % err)

if not isinstance(doc, dict) or set(doc) != {"version", "operators"}:
    sys.exit("store is not a preferences store file")
if doc["version"] != 1:
    sys.exit("unsupported preferences store version: %r" % (doc["version"],))
operators = doc["operators"]
if not isinstance(operators, list):
    sys.exit("store operators is not a list")

rewritten = explicit = stripped = 0
for entry in operators:
    if not isinstance(entry, dict) or not (KEYS <= set(entry) <= KEYS | REMOVABLE):
        sys.exit("store record has an unexpected shape")
    for key in REMOVABLE & set(entry):
        del entry[key]
        stripped += 1
    size = entry["font_size"]
    if size is None:
        entry["font_size"] = default_size
        rewritten += 1
        continue
    if not isinstance(size, int) or isinstance(size, bool) or not (9 <= size <= 24):
        sys.exit("store record holds a font size no release accepts: %r" % (size,))
    explicit += 1

# Validate what is about to be written, under the rules the OLD loader applies:
# the record's key set must be exactly the closed shape, and every record must
# carry an integer font size inside the closed 9…24 range. A record still
# holding null, or still carrying a key the old decoder does not know, would
# reproduce the very failure this script exists to repair.
for entry in operators:
    if set(entry) != KEYS:
        sys.exit("refusing to write a record shape an older release would reject: %r" % (sorted(entry),))
    size = entry["font_size"]
    if not isinstance(size, int) or isinstance(size, bool) or not (9 <= size <= 24):
        sys.exit("refusing to write a record an older release would reject: %r" % (size,))

text = json.dumps(doc, separators=(",", ":"), ensure_ascii=False)
json.loads(text)  # the bytes that will land must themselves parse
with open(dst, "w", encoding="utf-8") as handle:
    handle.write(text)
print("%d %d %d %d" % (len(operators), rewritten, explicit, stripped))
PY
) || die 'the store could not be rewritten'

read -r records rewritten explicit stripped <<<"$summary"

if ((rewritten == 0 && stripped == 0)); then
  printf 'persea-terminal preferences downgrade: %s\n' "$store"
  printf '  records: %s  on auto: 0  already explicit: %s  carrying composer_font_size: 0\n' "$records" "$explicit"
  printf '  no change: every record already carries an explicit font size and no key an older release would reject, so an older release can read this store.\n'
  exit 0
fi

if ((dry_run == 1)); then
  printf 'persea-terminal preferences downgrade (dry run): %s\n' "$store"
  printf '  records: %s  on auto: %s  already explicit: %s  carrying composer_font_size: %s\n' "$records" "$rewritten" "$explicit" "$stripped"
  printf '  would rewrite %s record(s) from auto to an explicit %s, remove composer_font_size from %s record(s), and leave every other field unchanged.\n' "$rewritten" "$DOWNGRADE_FONT_SIZE" "$stripped"
  exit 0
fi

backup="$store.pre-tristate-$(date -u +%Y%m%dT%H%M%SZ).bak"
[[ ! -e $backup ]] || die "backup already exists: $backup"
install -m 0600 -- "$store" "$backup"
cmp -s -- "$store" "$backup" || die 'backup does not match the store it copied'

chmod --reference="$store" -- "$work/next.json"
# Ownership is only transferable by a real root; a hermetic run is already
# confined to files it owns.
if [[ $(id -u) == 0 ]]; then
  chown --reference="$store" -- "$work/next.json"
fi
mv -f -- "$work/next.json" "$store"
sync -f -- "$store"

printf 'persea-terminal preferences downgrade: %s\n' "$store"
printf '  records: %s  rewritten from auto to %s: %s  already explicit: %s\n' "$records" "$DOWNGRADE_FONT_SIZE" "$rewritten" "$explicit"
printf '  composer_font_size removed from: %s record(s)\n' "$stripped"
printf '  backup: %s\n' "$backup"
printf '  the rewritten operators now hold an explicit %s; a newer release reads that as a deliberate size, not as auto.\n' "$DOWNGRADE_FONT_SIZE"
printf '  the operators whose composer_font_size was removed fall back to the composer face the older release hardcodes; rolling forward reads them as the UX-9 default again.\n'
