#!/usr/bin/env python3
"""Derive a v1 host manifest from one verified legacy installed release."""

from __future__ import annotations

import argparse
import filecmp
import grp
import importlib.util
import json
import os
import pwd
import re
import sys
from pathlib import Path
from typing import Any

sys.dont_write_bytecode = True


def load_host_module():
    path = Path(__file__).with_name("host-config.py")
    spec = importlib.util.spec_from_file_location("persea_host_config", path)
    if spec is None or spec.loader is None:
        raise RuntimeError("cannot load host-config.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


HOST = load_host_module()
UNIT_RE = re.compile(r"^persea-terminal-broker-([a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?)\.service$")


class MigrationError(ValueError):
    pass


def fail(message: str) -> None:
    raise MigrationError(message)


def load_json(path: Path, max_bytes: int = 1024 * 1024) -> Any:
    if not path.is_file() or path.is_symlink():
        fail(f"input is missing or unsafe: {path}")
    raw = path.read_bytes()
    if len(raw) > max_bytes:
        fail(f"input is oversized: {path}")
    try:
        return json.loads(raw.decode("utf-8", "strict"), object_pairs_hook=HOST.strict_object)
    except (UnicodeDecodeError, json.JSONDecodeError, HOST.ManifestError) as exc:
        fail(f"invalid strict JSON input {path}: {exc}")


def exact_keys(value: Any, keys: set[str], where: str) -> dict[str, Any]:
    if not isinstance(value, dict) or set(value) != keys:
        fail(f"{where} has unexpected structure")
    return value


def unit_fields(path: Path, required: set[str]) -> dict[str, str]:
    if not path.is_file() or path.is_symlink():
        fail(f"unit is missing or unsafe: {path}")
    values: dict[str, str] = {}
    for raw_line in path.read_text(encoding="utf-8").splitlines():
        if not raw_line or raw_line.startswith("#") or raw_line.startswith("[") or "=" not in raw_line:
            continue
        key, value = raw_line.split("=", 1)
        if key in required:
            if key in values:
                fail(f"unit repeats {key}: {path.name}")
            values[key] = value
    if set(values) != required:
        fail(f"unit lacks required fields: {path.name}")
    return values


class Accounts:
    def __init__(self, fixture: dict[str, Any] | None):
        self.fixture = fixture

    def uid(self, user: str) -> int:
        if self.fixture is not None:
            users = self.fixture.get("users")
            if not isinstance(users, dict) or user not in users or isinstance(users[user], bool) or not isinstance(users[user], int):
                fail(f"fixture account is missing: {user}")
            return users[user]
        try:
            return pwd.getpwnam(user).pw_uid
        except KeyError:
            fail(f"Unix account is missing: {user}")

    def group(self, group: str) -> tuple[int, set[str]]:
        if self.fixture is not None:
            groups = self.fixture.get("groups")
            record = groups.get(group) if isinstance(groups, dict) else None
            if not isinstance(record, dict) or set(record) != {"gid", "members"}:
                fail(f"fixture group is missing: {group}")
            gid, members = record["gid"], record["members"]
            if isinstance(gid, bool) or not isinstance(gid, int) or not isinstance(members, list) or not all(isinstance(v, str) for v in members):
                fail(f"fixture group is invalid: {group}")
            return gid, set(members)
        try:
            record = grp.getgrnam(group)
        except KeyError:
            fail(f"Unix group is missing: {group}")
        members = set(record.gr_mem)
        for entry in pwd.getpwall():
            if entry.pw_gid == record.gr_gid:
                members.add(entry.pw_name)
        return record.gr_gid, members


def derive(
    release: Path,
    unit_root: Path,
    main_status_path: Path,
    sidecar_status_path: Path,
    sidecar_prefs_path: Path,
    accounts: Accounts,
) -> dict[str, Any]:
    front = exact_keys(
        load_json(release / "config/front.json"),
        {"ingress", "realms", "alias_store_path", "handle_ttl_seconds", "handle_capacity"},
        "legacy front config",
    )
    ingress = exact_keys(
        front["ingress"],
        {"socket_path", "peer_uid", "canonical_host", "operator_login", "max_connections"},
        "legacy ingress",
    )
    if ingress["socket_path"] != "/run/persea-terminal/front.sock" or ingress["peer_uid"] != 0:
        fail("legacy front ingress is not the production root-peer shape")
    if front["alias_store_path"] != "/var/lib/persea-terminal/aliases.json":
        fail("legacy alias store path is unexpected")
    if front["handle_ttl_seconds"] != 120 or front["handle_capacity"] != 4096:
        fail("legacy capability bounds are unexpected")

    front_unit_name = "persea-terminal-front.service"
    front_release_unit = release / "units" / front_unit_name
    front_installed_unit = unit_root / front_unit_name
    if not filecmp.cmp(front_release_unit, front_installed_unit, shallow=False):
        fail("installed front unit differs from the current release")
    front_fields = unit_fields(front_release_unit, {"User", "Group", "ExecStart"})
    if front_fields["ExecStart"] != "/opt/persea-terminal/current/bin/persea-terminal front --config /opt/persea-terminal/current/config/front.json --static-dir ui":
        fail("legacy front ExecStart is unexpected")
    front_user = front_fields["User"]
    group = front_fields["Group"]
    front_uid = accounts.uid(front_user)
    group_gid, group_members = accounts.group(group)
    if front_user not in group_members:
        fail("legacy front user is not a member of the shared group")

    raw_realms = front["realms"]
    if not isinstance(raw_realms, list) or not raw_realms:
        fail("legacy front has no realms")
    realms: list[dict[str, Any]] = []
    expected_units = {front_unit_name}
    seen_realms: set[str] = set()
    for index, raw_realm in enumerate(raw_realms):
        realm = exact_keys(raw_realm, {"name", "display_name", "socket", "broker_uid"}, f"legacy realm {index}")
        realm_id = realm["name"]
        if not isinstance(realm_id, str) or realm_id in seen_realms:
            fail("legacy realm ID is invalid or duplicated")
        seen_realms.add(realm_id)
        expected_socket = f"/run/persea-terminal-{realm_id}/broker.sock"
        if realm["socket"] != expected_socket:
            fail(f"legacy realm socket is unexpected: {realm_id}")
        unit_name = f"persea-terminal-broker-{realm_id}.service"
        if not UNIT_RE.fullmatch(unit_name):
            fail(f"legacy realm cannot map to a safe unit: {realm_id}")
        expected_units.add(unit_name)
        release_unit = release / "units" / unit_name
        installed_unit = unit_root / unit_name
        if not filecmp.cmp(release_unit, installed_unit, shallow=False):
            fail(f"installed broker unit differs from the current release: {unit_name}")
        fields = unit_fields(release_unit, {"User", "Group", "ExecStart"})
        if fields["Group"] != group:
            fail(f"broker group differs from the front group: {realm_id}")
        expected_exec = (
            f"/opt/persea-terminal/current/bin/persea-terminal broker --socket {expected_socket} "
            f"--config /opt/persea-terminal/current/config/broker-{realm_id}.json"
        )
        if fields["ExecStart"] != expected_exec:
            fail(f"legacy broker ExecStart is unexpected: {realm_id}")
        user = fields["User"]
        uid = accounts.uid(user)
        if uid != realm["broker_uid"] or user not in group_members:
            fail(f"legacy broker account binding is invalid: {realm_id}")
        broker = exact_keys(
            load_json(release / f"config/broker-{realm_id}.json"),
            {"realm", "front_uid", "servers"},
            f"legacy broker config {realm_id}",
        )
        if broker["realm"] != realm_id or broker["front_uid"] != front_uid:
            fail(f"legacy broker authority binding is invalid: {realm_id}")
        realms.append(
            {
                "id": realm_id,
                "display_name": realm["display_name"],
                "user": user,
                "uid": uid,
                "servers": broker["servers"],
                # This migration accepts only the pre-image broker shape.
                # Preserve its disabled feature when applying newer defaults.
                "image_staging": False,
            }
        )

    installed_package_units = {
        path.name
        for path in unit_root.glob("persea-terminal-*.service")
        if path.name != "persea-terminal-tailscaled.service"
    }
    if installed_package_units != expected_units:
        fail("installed legacy application-unit inventory is ambiguous")

    main_status = load_json(main_status_path)
    sidecar_status = load_json(sidecar_status_path)
    sidecar_prefs = load_json(sidecar_prefs_path)
    if not isinstance(main_status, dict) or not isinstance(sidecar_status, dict) or not isinstance(sidecar_prefs, dict):
        fail("Tailscale status inputs must be objects")
    if sidecar_status.get("BackendState") != "Running":
        fail("legacy sidecar is not running")
    main_self = main_status.get("Self") or {}
    sidecar_self = sidecar_status.get("Self") or {}
    protected_main = str(main_self.get("DNSName") or "").rstrip(".")
    sidecar_dns = str(sidecar_self.get("DNSName") or "").rstrip(".")
    tags = sidecar_self.get("Tags") or []
    if not protected_main or not sidecar_dns or not isinstance(tags, list) or len(tags) != 1 or not isinstance(tags[0], str):
        fail("legacy Tailscale identities are incomplete or ambiguous")
    suffix = str(
        sidecar_status.get("MagicDNSSuffix")
        or (sidecar_status.get("CurrentTailnet") or {}).get("MagicDNSSuffix")
        or ""
    ).rstrip(".")
    main_suffix = str(main_status.get("MagicDNSSuffix") or (main_status.get("CurrentTailnet") or {}).get("MagicDNSSuffix") or "").rstrip(".")
    if not suffix or main_suffix != suffix or not sidecar_dns.endswith(f".{suffix}") or not protected_main.endswith(f".{suffix}"):
        fail("legacy Tailscale endpoints do not share one exact tailnet suffix")
    services = sidecar_prefs.get("AdvertiseServices") or []
    if not isinstance(services, list) or len(services) != 1 or not isinstance(services[0], str):
        fail("legacy sidecar does not advertise exactly one Service")
    service = services[0]
    canonical_host = ingress["canonical_host"]
    expected_host = f"{service.removeprefix('svc:')}.{suffix}" if service.startswith("svc:") else ""
    if canonical_host != expected_host:
        fail("legacy canonical host does not match the advertised Service")
    sidecar_hostname = sidecar_dns[: -(len(suffix) + 1)]
    if "." in sidecar_hostname or not sidecar_hostname:
        fail("legacy sidecar DNS identity is not a single hostname label")

    candidate = {
        "version": 1,
        "front": {"user": front_user, "uid": front_uid, "group": group, "gid": group_gid},
        "ingress": {
            "canonical_host": canonical_host,
            "operator_login": ingress["operator_login"],
            "max_connections": ingress["max_connections"],
        },
        "tailscale": {
            "service": service,
            "sidecar_hostname": sidecar_hostname,
            "sidecar_tag": tags[0],
            "tailnet_suffix": suffix,
            "protected_main_dns_name": protected_main,
        },
        "realms": realms,
    }
    return HOST.validate_manifest(candidate)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--release", type=Path, required=True)
    parser.add_argument("--unit-root", type=Path, required=True)
    parser.add_argument("--main-status", type=Path, required=True)
    parser.add_argument("--sidecar-status", type=Path, required=True)
    parser.add_argument("--sidecar-prefs", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--accounts-json", type=Path)
    args = parser.parse_args()
    try:
        if args.accounts_json is not None and os.environ.get("PERSEA_MIGRATION_HERMETIC") != "1":
            fail("fixture account maps require PERSEA_MIGRATION_HERMETIC=1")
        fixture = load_json(args.accounts_json) if args.accounts_json is not None else None
        candidate = derive(
            args.release,
            args.unit_root,
            args.main_status,
            args.sidecar_status,
            args.sidecar_prefs,
            Accounts(fixture),
        )
        if args.output.exists() or args.output.is_symlink():
            fail("migration output already exists")
        args.output.write_text(json.dumps(candidate, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
        args.output.chmod(0o600)
        return 0
    except (MigrationError, HOST.ManifestError, OSError) as exc:
        print(f"persea-terminal legacy migration: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
