#!/usr/bin/env python3
"""Strict host-manifest validator and deterministic release renderer."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import posixpath
import re
import stat
import sys
import unicodedata
from pathlib import Path
from typing import Any


MAX_MANIFEST_BYTES = 64 * 1024
MAX_UNIX_PATH_BYTES = 107
ID_RE = re.compile(r"^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$")
ACCOUNT_RE = re.compile(r"^[a-z_][a-z0-9_-]{0,31}$")
DNS_LABEL_RE = re.compile(r"^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$")
SERVICE_RE = re.compile(r"^svc:([a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)$")
TAG_RE = re.compile(r"^tag:([a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)$")

TOP_KEYS = {"version", "front", "ingress", "tailscale", "realms"}
FRONT_KEYS = {"user", "uid", "group", "gid"}
OPTIONAL_FRONT_KEYS = {"diagnostic_trace_dir", "image_upload_max_bytes", "staging_root"}
# The wire cap (proto.MaxImage) and the config floor the front door enforces at
# load time; validating here makes a bad manifest fail at packaging instead.
IMAGE_UPLOAD_MIN_BYTES = 1 << 20
IMAGE_UPLOAD_MAX_BYTES = 10 << 20
# Default broker-side staging root (config.DefaultImageStagingRoot). The root
# is host-level configuration: front.staging_root overrides it, and realm
# staging directories are derived as <root>/<realm-id> — never authored per
# realm in the manifest. The default is a sibling of the front's state
# directory, never inside it: the front's alias store requires
# /var/lib/persea-terminal to keep mode exactly 0700, so the front unit must
# gain nothing image-related.
DEFAULT_IMAGE_STAGING_ROOT = "/var/lib/persea-terminal-staging"
# Subtrees a staging root may never live under (or be); mirrors the Go
# config's forbiddenImageStagingParents so a bad manifest fails at packaging
# instead of at broker load.
FORBIDDEN_STAGING_PARENTS = ("/var/lib/persea-terminal", "/tmp", "/opt/persea-terminal")
DEPLOY_PATH_RE = re.compile(r"^[A-Za-z0-9/_.-]+$")
DIAGNOSTIC_TRACE_ROOT = "/var/lib/persea-terminal-diagnostics"
INGRESS_KEYS = {"canonical_host", "operator_login", "max_connections"}
TAILSCALE_KEYS = {
    "service",
    "sidecar_hostname",
    "sidecar_tag",
    "tailnet_suffix",
    "protected_main_dns_name",
}
REALM_KEYS = {"id", "display_name", "user", "uid", "servers"}
# Optional keys are permitted but not required, so an existing manifest stays
# valid and an absent block keeps that realm attach-only.
OPTIONAL_REALM_KEYS = {"session_create", "unified_terminal_dev", "image_staging"}
SESSION_CREATE_REQUIRED = {"enabled"}
SESSION_CREATE_OPTIONAL = {"servers", "name_pattern", "max_sessions", "start_directory", "columns", "rows"}
UNIFIED_TERMINAL_DEV_KEYS = {"enabled", "server", "session", "observer_session"}
SESSION_NAME_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$")
SERVER_KEYS = {"label", "socket_name", "socket_path"}


class ManifestError(ValueError):
    pass


def fail(message: str) -> None:
    raise ManifestError(message)


def strict_object(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            fail(f"duplicate JSON key: {key}")
        result[key] = value
    return result


def exact_keys(value: Any, expected: set[str], where: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        fail(f"{where} must be an object")
    actual = set(value)
    if actual != expected:
        missing = sorted(expected - actual)
        unknown = sorted(actual - expected)
        fail(f"{where} keys mismatch; missing={missing!r} unknown={unknown!r}")
    return value


def exact_int(value: Any, where: str, lower: int, upper: int) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or not lower <= value <= upper:
        fail(f"{where} must be an integer in [{lower}, {upper}]")
    return value


def exact_string(value: Any, where: str) -> str:
    if not isinstance(value, str):
        fail(f"{where} must be a string")
    return value


def safe_id(value: Any, where: str) -> str:
    result = exact_string(value, where)
    if not ID_RE.fullmatch(result):
        fail(f"{where} is not a safe lowercase identifier")
    return result


def account_name(value: Any, where: str) -> str:
    result = exact_string(value, where)
    if not ACCOUNT_RE.fullmatch(result):
        fail(f"{where} is not a safe Unix account name")
    return result


def dns_name(value: Any, where: str, *, require_ts_net: bool = False) -> str:
    result = exact_string(value, where)
    if not result or result != result.lower() or len(result.encode()) > 253:
        fail(f"{where} is not a canonical lowercase DNS name")
    labels = result.split(".")
    if len(labels) < 2 or any(not DNS_LABEL_RE.fullmatch(label) for label in labels):
        fail(f"{where} has an invalid DNS label")
    if require_ts_net and (len(labels) < 3 or labels[-2:] != ["ts", "net"]):
        fail(f"{where} must be a Tailscale ts.net suffix")
    return result


def display_name(value: Any, where: str) -> tuple[str, str]:
    result = exact_string(value, where)
    if not result or result != result.strip() or len(result.encode()) > 128:
        fail(f"{where} must be trimmed, nonempty, and at most 128 UTF-8 bytes")
    if any(unicodedata.category(char) in {"Cc", "Cf"} for char in result):
        fail(f"{where} contains a Unicode control or format character")
    return result, unicodedata.normalize("NFKC", result).casefold()


def socket_path(value: Any, where: str) -> str:
    result = exact_string(value, where)
    if (
        not result.startswith("/")
        or result.startswith("//")
        or result != posixpath.normpath(result)
        or len(result.encode()) > MAX_UNIX_PATH_BYTES
        or "\x00" in result
    ):
        fail(f"{where} must be a clean absolute Unix socket path")
    managed = (
        result == "/run/persea-terminal"
        or result.startswith("/run/persea-terminal/")
        or result.startswith("/run/persea-terminal-")
    )
    if managed:
        fail(f"{where} collides with a Persea-managed runtime path")
    return result


def load_manifest(path: Path, production: bool = False) -> tuple[dict[str, Any], bytes, str]:
    try:
        info = path.lstat()
    except OSError as exc:
        fail(f"cannot stat host manifest: {exc}")
    if not stat.S_ISREG(info.st_mode) or path.is_symlink():
        fail("host manifest must be an ordinary non-symlink file")
    if production and (info.st_uid, info.st_gid, stat.S_IMODE(info.st_mode)) != (0, 0, 0o600):
        fail("production host manifest must be root:root mode 0600")
    try:
        raw = path.read_bytes()
    except OSError as exc:
        fail(f"cannot read host manifest: {exc}")
    if len(raw) > MAX_MANIFEST_BYTES:
        fail("host manifest exceeds 64 KiB")
    try:
        text = raw.decode("utf-8", "strict")
        value = json.loads(text, object_pairs_hook=strict_object)
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        fail(f"invalid UTF-8 JSON host manifest: {exc}")
    manifest = validate_manifest(value)
    return manifest, raw, hashlib.sha256(raw).hexdigest()


def session_create(value: Any, where: str, labels: set[str]) -> dict[str, Any] | None:
    """Validate a realm's optional session-creation policy.

    Absent means creation stays disabled, which is what keeps an existing
    deployment attach-only after an upgrade. The broker re-validates all of this
    at load time; the checks here exist so a bad manifest fails at packaging
    rather than at service start.
    """
    if value is None:
        return None
    if not isinstance(value, dict):
        fail(f"{where} must be an object")
    block = exact_keys(value, SESSION_CREATE_REQUIRED | (SESSION_CREATE_OPTIONAL & set(value)), where)
    enabled = block.get("enabled")
    if not isinstance(enabled, bool):
        fail(f"{where}.enabled must be a boolean")
    rendered: dict[str, Any] = {"enabled": enabled}
    servers_value = block.get("servers")
    if servers_value is not None:
        if not isinstance(servers_value, list) or not servers_value:
            fail(f"{where}.servers must be a nonempty list when present")
        seen: set[str] = set()
        for index, label in enumerate(servers_value):
            name = exact_string(label, f"{where}.servers[{index}]")
            if name not in labels:
                fail(f"{where}.servers[{index}] is not a server of this realm")
            if name in seen:
                fail(f"{where}.servers[{index}] is duplicated")
            seen.add(name)
        rendered["servers"] = list(servers_value)
    pattern = block.get("name_pattern")
    if pattern is not None:
        text = exact_string(pattern, f"{where}.name_pattern")
        if not text.startswith("^") or not text.endswith("$"):
            fail(f"{where}.name_pattern must be anchored with ^ and $")
        try:
            re.compile(text)
        except re.error:
            fail(f"{where}.name_pattern is not a valid regular expression")
        # The broker compiles this with Go's RE2, which has no lookaround and no
        # backreferences. Python's re accepts both, so a pattern validated here
        # could still fail at service start — a deployment that packages cleanly
        # and then refuses to run. Reject the constructs the two dialects disagree
        # on rather than discovering the difference in production.
        for construct in ("(?=", "(?!", "(?<=", "(?<!", "(?P=", r"\1", r"\2", r"\3",
                          r"\4", r"\5", r"\6", r"\7", r"\8", r"\9"):
            if construct in text:
                fail(f"{where}.name_pattern uses {construct!r}, which Go's RE2 engine does not support")
        rendered["name_pattern"] = text
    maximum = block.get("max_sessions")
    if maximum is not None:
        rendered["max_sessions"] = exact_int(maximum, f"{where}.max_sessions", 1, 256)
    directory = block.get("start_directory")
    if directory is not None:
        rendered["start_directory"] = socket_path(directory, f"{where}.start_directory")
    columns, rows = block.get("columns"), block.get("rows")
    if (columns is None) != (rows is None):
        fail(f"{where} columns and rows must be set together")
    if columns is not None:
        rendered["columns"] = exact_int(columns, f"{where}.columns", 1, 1000)
        rendered["rows"] = exact_int(rows, f"{where}.rows", 1, 1000)
    return rendered


def unified_terminal_dev(
    value: Any,
    where: str,
    realm_id: str,
    labels: set[str],
    creation: dict[str, Any] | None,
) -> dict[str, Any] | None:
    """Validate unified admission policy and derive its private runtime root."""
    if value is None:
        return None
    expected = UNIFIED_TERMINAL_DEV_KEYS | ({"adoption_slots"} if isinstance(value, dict) and "adoption_slots" in value else set())
    block = exact_keys(value, expected, where)
    slots = {}
    if "adoption_slots" in block:
        slots["adoption_slots"] = exact_int(block["adoption_slots"], f"{where}.adoption_slots", 1, 4096)
    if block["enabled"] is not True:
        fail(f"{where}.enabled must be exactly true")
    server = safe_id(block["server"], f"{where}.server")
    session = exact_string(block["session"], f"{where}.session")
    observer = exact_string(block["observer_session"], f"{where}.observer_session")
    if not SESSION_NAME_RE.fullmatch(session):
        fail(f"{where}.session is invalid")
    if not SESSION_NAME_RE.fullmatch(observer) or observer == session:
        fail(f"{where}.observer_session is invalid")
    if creation is None or not creation["enabled"] or server not in labels:
        fail(f"{where} requires enabled session_create authority for its server")
    allowed_servers = creation.get("servers")
    if allowed_servers is not None and server not in allowed_servers:
        fail(f"{where}.server is outside session_create policy")
    pattern = creation.get("name_pattern", r"^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$")
    if re.fullmatch(pattern, session) is None:
        fail(f"{where}.session is outside session_create policy")
    return {
        "enabled": True,
        "server": server,
        "session": session,
        "observer_session": observer,
        "runtime_dir": f"/run/persea-terminal-{realm_id}/unified-journal",
        **slots,
    }


def validate_manifest(value: Any) -> dict[str, Any]:
    root = exact_keys(value, TOP_KEYS, "manifest")
    if root["version"] != 1 or isinstance(root["version"], bool):
        fail("manifest.version must be exactly 1")

    if not isinstance(root["front"], dict):
        fail("front must be an object")
    front = root["front"]
    front_keys = set(front)
    missing_front = sorted(FRONT_KEYS - front_keys)
    unknown_front = sorted(front_keys - FRONT_KEYS - OPTIONAL_FRONT_KEYS)
    if missing_front or unknown_front:
        fail(f"front keys mismatch; missing={missing_front!r} unknown={unknown_front!r}")
    front_user = account_name(front["user"], "front.user")
    front_uid = exact_int(front["uid"], "front.uid", 1, 2**32 - 2)
    front_group = account_name(front["group"], "front.group")
    front_gid = exact_int(front["gid"], "front.gid", 1, 2**32 - 2)
    diagnostic_trace_dir = None
    if "diagnostic_trace_dir" in front:
        diagnostic_trace_dir = exact_string(front["diagnostic_trace_dir"], "front.diagnostic_trace_dir")
        # Match the front's runtime boundary before any build or activation.
        # These paths also appear verbatim in systemd ReadWritePaths directives.
        if (
            not diagnostic_trace_dir.startswith(DIAGNOSTIC_TRACE_ROOT + "/")
            or posixpath.normpath(diagnostic_trace_dir) != diagnostic_trace_dir
            or not DEPLOY_PATH_RE.fullmatch(diagnostic_trace_dir)
        ):
            fail(f"front.diagnostic_trace_dir must be a clean absolute path below {DIAGNOSTIC_TRACE_ROOT}")
    image_upload_max_bytes = None
    if "image_upload_max_bytes" in front:
        image_upload_max_bytes = exact_int(
            front["image_upload_max_bytes"],
            "front.image_upload_max_bytes",
            IMAGE_UPLOAD_MIN_BYTES,
            IMAGE_UPLOAD_MAX_BYTES,
        )
    staging_root = None
    if "staging_root" in front:
        staging_root = exact_string(front["staging_root"], "front.staging_root")
        if (
            not staging_root
            or staging_root == "/"
            or not posixpath.isabs(staging_root)
            or posixpath.normpath(staging_root) != staging_root
            or not DEPLOY_PATH_RE.fullmatch(staging_root)
        ):
            fail("front.staging_root must be a clean absolute path")
        for forbidden in FORBIDDEN_STAGING_PARENTS:
            if staging_root == forbidden or staging_root.startswith(forbidden + "/"):
                fail(f"front.staging_root must not live under {forbidden}")
    if front_user == "root" or front_group == "root":
        fail("front must use an unprivileged account and non-root group")

    ingress = exact_keys(root["ingress"], INGRESS_KEYS, "ingress")
    canonical_host = dns_name(ingress["canonical_host"], "ingress.canonical_host")
    operator_login = exact_string(ingress["operator_login"], "ingress.operator_login")
    if (
        not operator_login
        or operator_login != operator_login.strip()
        or len(operator_login.encode()) > 254
        or any(unicodedata.category(char) in {"Cc", "Cf"} for char in operator_login)
    ):
        fail("ingress.operator_login is invalid")
    max_connections = exact_int(ingress["max_connections"], "ingress.max_connections", 1, 256)

    tailscale = exact_keys(root["tailscale"], TAILSCALE_KEYS, "tailscale")
    service = exact_string(tailscale["service"], "tailscale.service")
    service_match = SERVICE_RE.fullmatch(service)
    if not service_match:
        fail("tailscale.service must be svc:<dns-label>")
    sidecar_hostname = safe_id(tailscale["sidecar_hostname"], "tailscale.sidecar_hostname")
    sidecar_tag = exact_string(tailscale["sidecar_tag"], "tailscale.sidecar_tag")
    if not TAG_RE.fullmatch(sidecar_tag):
        fail("tailscale.sidecar_tag must be tag:<dns-label>")
    tailnet_suffix = dns_name(tailscale["tailnet_suffix"], "tailscale.tailnet_suffix", require_ts_net=True)
    protected_main = dns_name(tailscale["protected_main_dns_name"], "tailscale.protected_main_dns_name")
    expected_host = f"{service_match.group(1)}.{tailnet_suffix}"
    sidecar_dns = f"{sidecar_hostname}.{tailnet_suffix}"
    if canonical_host != expected_host:
        fail("ingress.canonical_host does not match the Service and tailnet suffix")
    if not protected_main.endswith(f".{tailnet_suffix}"):
        fail("tailscale.protected_main_dns_name is outside the configured tailnet")
    if protected_main in {canonical_host, sidecar_dns}:
        fail("protected main DNS identity collides with the Service or sidecar identity")

    realms_value = root["realms"]
    if not isinstance(realms_value, list) or not 1 <= len(realms_value) <= 32:
        fail("realms must contain 1 to 32 entries")
    realm_ids: set[str] = set()
    normalized_displays: set[str] = set()
    global_socket_paths: set[str] = set()
    global_named_selectors: set[tuple[int, str]] = set()
    realms: list[dict[str, Any]] = []
    for realm_index, raw_realm in enumerate(realms_value):
        where = f"realms[{realm_index}]"
        optional = OPTIONAL_REALM_KEYS & set(raw_realm) if isinstance(raw_realm, dict) else set()
        realm = exact_keys(raw_realm, REALM_KEYS | optional, where)
        realm_id = safe_id(realm["id"], f"{where}.id")
        if realm_id in {"front", "tailscaled"} or realm_id in realm_ids:
            fail(f"{where}.id is reserved or duplicated")
        realm_ids.add(realm_id)
        shown, normalized = display_name(realm["display_name"], f"{where}.display_name")
        if normalized in normalized_displays:
            fail(f"{where}.display_name collides after NFKC casefold normalization")
        normalized_displays.add(normalized)
        user = account_name(realm["user"], f"{where}.user")
        uid = exact_int(realm["uid"], f"{where}.uid", 1, 2**32 - 2)
        if user == "root":
            fail(f"{where}.user must be unprivileged")
        servers_value = realm["servers"]
        if not isinstance(servers_value, list) or not 1 <= len(servers_value) <= 16:
            fail(f"{where}.servers must contain 1 to 16 entries")
        labels: set[str] = set()
        selectors: set[str] = set()
        servers: list[dict[str, str]] = []
        for server_index, raw_server in enumerate(servers_value):
            server_where = f"{where}.servers[{server_index}]"
            if not isinstance(raw_server, dict):
                fail(f"{server_where} must be an object")
            if set(raw_server) not in ({"label", "socket_name"}, {"label", "socket_path"}):
                fail(f"{server_where} must contain label and exactly one socket selector")
            server = exact_keys(raw_server, set(raw_server), server_where)
            label = safe_id(server["label"], f"{server_where}.label")
            if label in labels:
                fail(f"{server_where}.label is duplicated")
            labels.add(label)
            if "socket_name" in server:
                selector_value = safe_id(server["socket_name"], f"{server_where}.socket_name")
                selector_key = f"name:{selector_value}"
                global_key = (uid, selector_value)
                if global_key in global_named_selectors:
                    fail(f"{server_where} duplicates a named tmux selector for UID {uid}")
                global_named_selectors.add(global_key)
                normalized_server = {"label": label, "socket_name": selector_value}
            else:
                selector_value = socket_path(server["socket_path"], f"{server_where}.socket_path")
                selector_key = f"path:{selector_value}"
                if selector_value in global_socket_paths:
                    fail(f"{server_where} duplicates an absolute tmux selector across realms")
                global_socket_paths.add(selector_value)
                normalized_server = {"label": label, "socket_path": selector_value}
            if selector_key in selectors:
                fail(f"{server_where} duplicates an exact tmux selector")
            selectors.add(selector_key)
            servers.append(normalized_server)
        runtime_socket = f"/run/persea-terminal-{realm_id}/broker.sock"
        if len(runtime_socket.encode()) > MAX_UNIX_PATH_BYTES:
            fail(f"{where}.id makes the broker socket path too long")
        creation = session_create(realm.get("session_create"), f"{where}.session_create", labels)
        unified_dev = unified_terminal_dev(
            realm.get("unified_terminal_dev"),
            f"{where}.unified_terminal_dev",
            realm_id,
            labels,
            creation,
        )
        image_staging = realm.get("image_staging", True)
        if not isinstance(image_staging, bool):
            fail(f"{where}.image_staging must be a boolean")
        realms.append(
            {
                "id": realm_id,
                "display_name": shown,
                "user": user,
                "uid": uid,
                "servers": servers,
                **({"session_create": creation} if creation is not None else {}),
                **({"unified_terminal_dev": unified_dev} if unified_dev is not None else {}),
                "image_staging": image_staging,
            }
        )

    # Every realm gets image attachment unless explicitly disabled. Keep the
    # normalized false value so validating a resolved manifest cannot re-enable
    # an operator's opt-out. The front endpoint follows the enabled realms.
    any_staging = any(realm["image_staging"] for realm in realms)
    if any_staging and image_upload_max_bytes is None:
        image_upload_max_bytes = 10 << 20
    if not any_staging and image_upload_max_bytes is not None:
        fail("front.image_upload_max_bytes and at least one realms[].image_staging must be configured together")

    return {
        "version": 1,
        "front": {
            "user": front_user,
            "uid": front_uid,
            "group": front_group,
            "gid": front_gid,
            **({"diagnostic_trace_dir": diagnostic_trace_dir} if diagnostic_trace_dir is not None else {}),
            **({"image_upload_max_bytes": image_upload_max_bytes} if image_upload_max_bytes is not None else {}),
            **({"staging_root": staging_root} if staging_root is not None else {}),
        },
        "ingress": {
            "canonical_host": canonical_host,
            "operator_login": operator_login,
            "max_connections": max_connections,
        },
        "tailscale": {
            "service": service,
            "sidecar_hostname": sidecar_hostname,
            "sidecar_tag": sidecar_tag,
            "tailnet_suffix": tailnet_suffix,
            "protected_main_dns_name": protected_main,
        },
        "realms": realms,
    }


def compact_json(value: Any) -> bytes:
    return (json.dumps(value, ensure_ascii=False, separators=(",", ":")) + "\n").encode()


def hardening() -> str:
    return """NoNewPrivileges=yes
CapabilityBoundingSet=
AmbientCapabilities=
PrivateDevices=yes
ProtectSystem=strict
ProtectHome=read-only
ProtectControlGroups=yes
ProtectKernelModules=yes
ProtectKernelTunables=yes
ProtectKernelLogs=yes
ProtectClock=yes
ProtectHostname=yes
RestrictAddressFamilies=AF_UNIX
IPAddressDeny=any
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native"""


def broker_unit(manifest: dict[str, Any], realm: dict[str, Any]) -> bytes:
    realm_id = realm["id"]
    same_identity = realm["user"] == manifest["front"]["user"] and realm["uid"] == manifest["front"]["uid"]
    runtime_mode = "0700" if same_identity else "2710"
    umask = "0077" if same_identity else "0007"
    retention_controls = ""
    if "unified_terminal_dev" in realm:
        runtime_dir = realm["unified_terminal_dev"]["runtime_dir"]
        retention_controls = (
            f"TemporaryFileSystem={runtime_dir}:"
            f"rw,nosuid,nodev,noexec,noswap,size=96M,nr_inodes=256,mode=0700,"
            f"uid={realm['uid']},gid={manifest['front']['gid']}\n"
            "MemoryHigh=850M\n"
            "MemoryMax=1G\n"
            "Environment=GOMEMLIMIT=528MiB\n"
            "LimitCORE=0\n"
        )
    if realm["image_staging"]:
        # ProtectSystem=strict makes the filesystem read-only inside the
        # broker's mount namespace; the staging directory must be opened
        # explicitly. The path carries no `-` prefix on purpose: the installer
        # pre-creates it, and a missing directory must fail unit start rather
        # than silently disable staging.
        staging_root = manifest["front"].get("staging_root", DEFAULT_IMAGE_STAGING_ROOT)
        retention_controls += f"ReadWritePaths={staging_root}/{realm_id}\n"
    return f"""[Unit]
Description=Persea Terminal {realm_id} tmux broker
Before=persea-terminal-front.service

[Service]
Type=simple
User={realm['user']}
Group={manifest['front']['group']}
WorkingDirectory=/opt/persea-terminal/current
ExecStart=/opt/persea-terminal/current/bin/persea-terminal broker --socket /run/persea-terminal-{realm_id}/broker.sock --config /opt/persea-terminal/current/config/broker-{realm_id}.json
Restart=on-failure
RestartSec=2s
RuntimeDirectory=persea-terminal-{realm_id}
RuntimeDirectoryMode={runtime_mode}
UMask={umask}
{retention_controls}{hardening()}

[Install]
WantedBy=multi-user.target
""".encode()


def front_unit(manifest: dict[str, Any], broker_units: list[str]) -> bytes:
    dependencies = " ".join(broker_units)
    read_write_paths = "/var/lib/persea-terminal"
    diagnostic_trace_dir = manifest["front"].get("diagnostic_trace_dir")
    if diagnostic_trace_dir is not None:
        read_write_paths += f" {diagnostic_trace_dir}"
    # The state directory holds the alias store, whose startup invariant
    # requires the parent to have mode exactly 0700; image staging lives in
    # its own root (front.staging_root, default DEFAULT_IMAGE_STAGING_ROOT)
    # precisely so this mode never varies.
    return f"""[Unit]
Description=Persea Terminal trusted AF_UNIX front door
Wants={dependencies}
After={dependencies}

[Service]
Type=simple
User={manifest['front']['user']}
Group={manifest['front']['group']}
WorkingDirectory=/opt/persea-terminal/current
ExecStart=/opt/persea-terminal/current/bin/persea-terminal front --config /opt/persea-terminal/current/config/front.json --static-dir ui
Restart=on-failure
RestartSec=2s
RuntimeDirectory=persea-terminal
RuntimeDirectoryMode=0700
StateDirectory=persea-terminal
StateDirectoryMode=0700
UMask=0077
ReadWritePaths={read_write_paths}
{hardening()}

[Install]
WantedBy=multi-user.target
""".encode()


def sidecar_unit() -> bytes:
    return b"""[Unit]
Description=Persea Terminal isolated Tailscale Service sidecar
Requires=persea-terminal-front.service
After=persea-terminal-front.service network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
Group=root
ExecStart=/usr/sbin/tailscaled --state=/var/lib/persea-terminal-tailscale/tailscaled.state --statedir=/var/lib/persea-terminal-tailscale --socket=/run/persea-terminal-tailscale/tailscaled.sock --tun=userspace-networking --port=0
Restart=on-failure
RestartSec=2s
StateDirectory=persea-terminal-tailscale
StateDirectoryMode=0700
RuntimeDirectory=persea-terminal-tailscale
RuntimeDirectoryMode=0700
UMask=0077
NoNewPrivileges=yes
CapabilityBoundingSet=CAP_DAC_OVERRIDE CAP_NET_ADMIN CAP_NET_RAW
AmbientCapabilities=
PrivateDevices=yes
PrivateTmp=yes
ProtectSystem=strict
ProtectHome=yes
ProtectControlGroups=yes
ProtectKernelModules=yes
ProtectKernelTunables=yes
ProtectKernelLogs=yes
ProtectClock=yes
ProtectHostname=yes
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
ReadWritePaths=/var/lib/persea-terminal-tailscale /run/persea-terminal-tailscale
InaccessiblePaths=/run/tailscale /var/lib/tailscale

[Install]
WantedBy=multi-user.target
"""


def rendered_files(manifest: dict[str, Any], raw: bytes, digest: str) -> dict[str, bytes]:
    files: dict[str, bytes] = {"config/host.json": raw}
    broker_units: list[str] = []
    resolved_realms: list[dict[str, Any]] = []
    front_realms: list[dict[str, Any]] = []
    for realm in manifest["realms"]:
        realm_id = realm["id"]
        unit = f"persea-terminal-broker-{realm_id}.service"
        config_path = f"config/broker-{realm_id}.json"
        unit_path = f"units/{unit}"
        socket = f"/run/persea-terminal-{realm_id}/broker.sock"
        broker_config = {"realm": realm_id, "front_uid": manifest["front"]["uid"], "servers": realm["servers"]}
        # Historical release verification preserves omitted fields exactly;
        # current render admission requires the enabled Unified policy.
        if "session_create" in realm:
            broker_config["session_create"] = realm["session_create"]
        if "unified_terminal_dev" in realm:
            broker_config["unified_terminal_dev"] = realm["unified_terminal_dev"]
        if realm["image_staging"]:
            staging_root = manifest["front"].get("staging_root", DEFAULT_IMAGE_STAGING_ROOT)
            # The override travels explicitly so the broker validates its
            # staging dir against the same root the unit and installer used;
            # the default stays absent, keeping existing configs byte-stable.
            if "staging_root" in manifest["front"]:
                broker_config["image_staging_root"] = staging_root
            broker_config["image_staging_dir"] = f"{staging_root}/{realm_id}"
        files[config_path] = compact_json(broker_config)
        files[unit_path] = broker_unit(manifest, realm)
        broker_units.append(unit)
        front_realms.append(
            {"name": realm_id, "display_name": realm["display_name"], "socket": socket, "broker_uid": realm["uid"]}
        )
        resolved_realms.append(
            {
                "id": realm_id,
                "unit": unit,
                "config": config_path,
                "socket": socket,
                "user": realm["user"],
                "uid": realm["uid"],
            }
        )
    front_config = {
        "ingress": {
            "socket_path": "/run/persea-terminal/front.sock",
            "peer_uid": 0,
            "canonical_host": manifest["ingress"]["canonical_host"],
            "operator_login": manifest["ingress"]["operator_login"],
            "max_connections": manifest["ingress"]["max_connections"],
        },
        "realms": front_realms,
        "alias_store_path": "/var/lib/persea-terminal/aliases.json",
        # The optional stores share the alias store's home: the
        # front's 0700 StateDirectory, never the broker-owned staging root.
        "preferences_store_path": "/var/lib/persea-terminal/preferences.json",
        "snippet_store_path": "/var/lib/persea-terminal/snippets.json",
        "workspace_store_path": "/var/lib/persea-terminal/workspaces.json",
        "keyboard_preferences_store_path": "/var/lib/persea-terminal/keyboard-v1.json",
        "handle_ttl_seconds": 120,
        "handle_capacity": 4096,
    }
    if "diagnostic_trace_dir" in manifest["front"]:
        front_config["diagnostic_trace_dir"] = manifest["front"]["diagnostic_trace_dir"]
    if "staging_root" in manifest["front"]:
        # The front confines broker-returned staged paths against the same
        # host-level root the brokers stage into.
        front_config["image_staging_root"] = manifest["front"]["staging_root"]
    if "image_upload_max_bytes" in manifest["front"]:
        front_config["image_upload_max_bytes"] = manifest["front"]["image_upload_max_bytes"]
    managed_units = broker_units + ["persea-terminal-front.service"]
    files["config/front.json"] = compact_json(front_config)
    files["units/persea-terminal-front.service"] = front_unit(manifest, broker_units)
    files["units/persea-terminal-tailscaled.service"] = sidecar_unit()
    files["config/managed-units"] = ("\n".join(managed_units) + "\n").encode()
    resolved = {
        "version": 1,
        "manifest_sha256": digest,
        "front": manifest["front"],
        "ingress": manifest["ingress"],
        "tailscale": manifest["tailscale"],
        "realms": resolved_realms,
        "managed_units": managed_units,
    }
    files["config/resolved-host.json"] = compact_json(resolved)
    return files


def render(manifest_path: Path, output: Path) -> None:
    manifest, raw, digest = load_manifest(manifest_path)
    for realm in manifest["realms"]:
        if not realm.get("unified_terminal_dev", {}).get("enabled"):
            fail(f"realm {realm['id']} requires enabled unified_terminal_dev for serving")
    if output.exists() or output.is_symlink():
        fail(f"candidate output already exists: {output}")
    output.mkdir(mode=0o700, parents=False)
    try:
        for relative, payload in rendered_files(manifest, raw, digest).items():
            target = output / relative
            target.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
            if target.is_symlink() or target.exists():
                fail(f"generated target collision: {relative}")
            target.write_bytes(payload)
            target.chmod(0o600)
    except Exception:
        # The caller owns cleanup; never follow or remove a path that appeared later.
        raise


def verify_release(manifest_path: Path, release: Path) -> None:
    manifest, raw, digest = load_manifest(manifest_path)
    expected = rendered_files(manifest, raw, digest)
    actual: set[str] = set()
    for directory_name in ("config", "units"):
        directory = release / directory_name
        if not directory.is_dir() or directory.is_symlink():
            fail(f"release {directory_name} directory is missing or unsafe")
        for target in directory.iterdir():
            if not target.is_file() or target.is_symlink():
                fail(f"release generated target is unsafe: {target.name}")
            actual.add(f"{directory_name}/{target.name}")
    if actual != set(expected):
        fail(
            "release generated inventory mismatch; "
            f"missing={sorted(set(expected) - actual)!r} unexpected={sorted(actual - set(expected))!r}"
        )
    for relative, payload in expected.items():
        if (release / relative).read_bytes() != payload:
            fail(f"release generated file disagrees with host snapshot: {relative}")


def emit_records(manifest: dict[str, Any], digest: str) -> None:
    values: list[str] = ["V1"]
    scalars = {
        "PERSEA_FRONT_USER": manifest["front"]["user"],
        "PERSEA_FRONT_UID": str(manifest["front"]["uid"]),
        "PERSEA_GROUP": manifest["front"]["group"],
        "PERSEA_GROUP_GID": str(manifest["front"]["gid"]),
        "PERSEA_OPERATOR": manifest["ingress"]["operator_login"],
        "PERSEA_MAX_CONNECTIONS": str(manifest["ingress"]["max_connections"]),
        "PERSEA_SERVICE": manifest["tailscale"]["service"],
        "PERSEA_SIDECAR_HOSTNAME": manifest["tailscale"]["sidecar_hostname"],
        "PERSEA_SIDECAR_TAG": manifest["tailscale"]["sidecar_tag"],
        "PERSEA_TAILNET_SUFFIX": manifest["tailscale"]["tailnet_suffix"],
        "PERSEA_CANONICAL_SERVICE_FQDN": manifest["ingress"]["canonical_host"],
        "PERSEA_PROTECTED_MAIN_DNS_NAME": manifest["tailscale"]["protected_main_dns_name"],
    }
    for key, value in scalars.items():
        values.extend(["SCALAR", key, value])
    for realm in manifest["realms"]:
        realm_id = realm["id"]
        values.extend(
            [
                "REALM",
                realm_id,
                realm["display_name"],
                realm["user"],
                str(realm["uid"]),
                f"persea-terminal-broker-{realm_id}.service",
                f"/run/persea-terminal-{realm_id}/broker.sock",
                f"config/broker-{realm_id}.json",
            ]
        )
    values.extend(["END", digest])
    sys.stdout.buffer.write(b"\0".join(value.encode() for value in values) + b"\0")


def main() -> int:
    parser = argparse.ArgumentParser()
    subparsers = parser.add_subparsers(dest="command", required=True)
    for name in ("validate", "records", "digest"):
        command = subparsers.add_parser(name)
        command.add_argument("manifest", type=Path)
        command.add_argument("--production", action="store_true")
    renderer = subparsers.add_parser("render")
    renderer.add_argument("manifest", type=Path)
    renderer.add_argument("output", type=Path)
    verifier = subparsers.add_parser("verify-release")
    verifier.add_argument("manifest", type=Path)
    verifier.add_argument("release", type=Path)
    args = parser.parse_args()
    try:
        if args.command == "render":
            render(args.manifest, args.output)
            return 0
        if args.command == "verify-release":
            verify_release(args.manifest, args.release)
            return 0
        manifest, _raw, digest = load_manifest(args.manifest, args.production)
        if args.command == "records":
            emit_records(manifest, digest)
        elif args.command == "digest":
            print(digest)
        else:
            print("VALID")
        return 0
    except ManifestError as exc:
        print(f"persea-terminal host config: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
