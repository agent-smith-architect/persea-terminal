#!/usr/bin/env python3
from __future__ import annotations

import copy
import hashlib
import json
import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path


DEPLOY = Path(__file__).resolve().parents[1]
PROJECT = DEPLOY.parent
HELPER = DEPLOY / "host-config.py"


def fixture() -> dict:
    return {
        "version": 1,
        "front": {"user": "termop_q7", "uid": 42001, "group": "termshare_x9", "gid": 42003},
        "ingress": {
            "canonical_host": "terminal.testing-f8.ts.net",
            "operator_login": "operator-q7@example.invalid",
            "max_connections": 64,
        },
        "tailscale": {
            "service": "svc:terminal",
            "sidecar_hostname": "terminal-sidecar",
            "sidecar_tag": "tag:terminal-service",
            "tailnet_suffix": "testing-f8.ts.net",
            "protected_main_dns_name": "console.testing-f8.ts.net",
        },
        "realms": [
            {
                "id": "desk-a7",
                "display_name": "Operator Q7",
                "user": "termop_q7",
                "uid": 42001,
                "image_staging": False,
                "session_create": {"enabled": True, "servers": ["primary"]},
                "unified_terminal_dev": {"enabled": True, "server": "primary", "session": "unified-dev", "observer_session": "observer-main"},
                "servers": [{"label": "primary", "socket_name": "main-a7"}],
            },
            {
                "id": "lab-k4",
                "display_name": "Automation K4",
                "user": "tmuxbot_k4",
                "uid": 42002,
                "image_staging": False,
                "session_create": {"enabled": True, "servers": ["primary"]},
                "unified_terminal_dev": {"enabled": True, "server": "primary", "session": "unified-dev", "observer_session": "observer-main"},
                "servers": [{"label": "primary", "socket_path": "/srv/tmux-k4/main.sock"}],
            },
        ],
    }


class HostConfigTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory(prefix="pt-host-config-")
        self.root = Path(self.temp.name)

    def tearDown(self) -> None:
        self.temp.cleanup()

    def write(self, value: object, name: str = "host.json") -> Path:
        path = self.root / name
        path.write_text(json.dumps(value, ensure_ascii=False), encoding="utf-8")
        return path

    def run_helper(self, *args: object, success: bool = True) -> subprocess.CompletedProcess[bytes]:
        result = subprocess.run(
            ["python3", os.fspath(HELPER), *(os.fspath(value) for value in args)],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )
        self.assertEqual(
            result.returncode == 0,
            success,
            msg=f"stdout={result.stdout!r} stderr={result.stderr!r}",
        )
        return result

    def assert_invalid(self, mutate) -> None:
        value = fixture()
        mutate(value)
        self.run_helper("validate", self.write(value), success=False)

    def test_render_is_complete_deterministic_and_host_bound(self) -> None:
        manifest = self.write(fixture())
        first = self.root / "first"
        second = self.root / "second"
        self.run_helper("render", manifest, first)
        self.run_helper("render", manifest, second)
        expected = {
            "config/broker-desk-a7.json",
            "config/broker-lab-k4.json",
            "config/front.json",
            "config/host.json",
            "config/managed-units",
            "config/resolved-host.json",
            "units/persea-terminal-broker-desk-a7.service",
            "units/persea-terminal-broker-lab-k4.service",
            "units/persea-terminal-front.service",
            "units/persea-terminal-tailscaled.service",
        }
        actual = {path.relative_to(first).as_posix() for path in first.rglob("*") if path.is_file()}
        self.assertEqual(actual, expected)
        for relative in expected:
            self.assertEqual((first / relative).read_bytes(), (second / relative).read_bytes())
        serving_hashes = {
            "config/broker-desk-a7.json": "066cbb0166f7f7937c7cfa0cf6a292d2f2aab1a1a454dec643f29aaba656b907",
            "config/broker-lab-k4.json": "52a2f062d4ca20f41ddf63d4d5823dcb7ce4c6b9da454336e9438fa98c0d20b1",
            "units/persea-terminal-broker-desk-a7.service": "b5b08ceb4a7c777deaaaac10f40624882d62cf773837bb38f8e1f94680d7d649",
            "units/persea-terminal-broker-lab-k4.service": "2d7c989e1a5188b88a332e5812388b01461ca6a27808ddcf140b72d2e605b9da"
        }
        for relative, expected_hash in serving_hashes.items():
            self.assertEqual(hashlib.sha256((first / relative).read_bytes()).hexdigest(), expected_hash)
        self.assertEqual((first / "config/host.json").read_bytes(), manifest.read_bytes())
        resolved = json.loads((first / "config/resolved-host.json").read_text(encoding="utf-8"))
        self.assertEqual(resolved["manifest_sha256"], hashlib.sha256(manifest.read_bytes()).hexdigest())
        self.assertEqual(
            (first / "config/managed-units").read_text(encoding="utf-8").splitlines(),
            [
                "persea-terminal-broker-desk-a7.service",
                "persea-terminal-broker-lab-k4.service",
                "persea-terminal-front.service",
            ],
        )
        self.run_helper("verify-release", manifest, first)

    def test_records_have_terminal_digest_and_no_shell_code(self) -> None:
        manifest = self.write(fixture())
        output = self.run_helper("records", manifest).stdout.split(b"\0")
        self.assertEqual(output[0], b"V1")
        self.assertEqual(output[-3], b"END")
        self.assertEqual(output[-2], hashlib.sha256(manifest.read_bytes()).hexdigest().encode())
        self.assertEqual(output[-1], b"")

    def test_diagnostic_directory_matches_runtime_boundary_before_render(self) -> None:
        root = "/var/lib/persea-terminal-diagnostics"
        for directory in ("/tmp/old-capture", root, root + "-other/capture",
                          root + "/../escape", root + "/capture/", root + "//capture",
                          root + "/capture\nother-path"):
            with self.subTest(directory=directory):
                value = fixture()
                value["front"]["diagnostic_trace_dir"] = directory
                output = self.root / "refused-diagnostic"
                self.run_helper("render", self.write(value), output, success=False)
                self.assertFalse(output.exists())
        value = fixture()
        directory = root + "/capture/run-1"
        value["front"]["diagnostic_trace_dir"] = directory
        output = self.root / "valid-diagnostic"
        self.run_helper("render", self.write(value), output)
        front = json.loads((output / "config/front.json").read_text())
        self.assertEqual(front["diagnostic_trace_dir"], directory)
        self.assertIn(directory, (output / "units/persea-terminal-front.service").read_text())

    def test_missing_or_disabled_unified_serving_policy_cannot_render(self) -> None:
        for disabled in (False, True):
            value = fixture()
            if disabled:
                value["realms"][0]["unified_terminal_dev"]["enabled"] = False
            else:
                value["realms"][0].pop("unified_terminal_dev")
            output = self.root / "refused"
            self.run_helper("render", self.write(value), output, success=False)
            self.assertFalse(output.exists())

    def test_duplicate_and_unknown_keys_fail(self) -> None:
        duplicate = self.root / "duplicate.json"
        duplicate.write_text(
            '{"version":1,"version":1,"front":{},"ingress":{},"tailscale":{},"realms":[]}',
            encoding="utf-8",
        )
        self.run_helper("validate", duplicate, success=False)
        self.assert_invalid(lambda value: value.update({"surprise": True}))
        self.assert_invalid(lambda value: value["front"].update({"surprise": True}))
        self.assert_invalid(lambda value: value["realms"][0].update({"surprise": True}))

    def test_unified_dev_is_required_exact_and_service_private(self) -> None:
        value = fixture()
        value["realms"][0]["session_create"] = {
            "enabled": True,
            "servers": ["primary"],
            "name_pattern": "^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$",
            "max_sessions": 8,
            "start_directory": "/srv/example-project",
        }
        value["realms"][0]["unified_terminal_dev"] = {
            "enabled": True,
            "server": "primary",
            "session": "unified-dev",
            "observer_session": "observer-main",
        }
        manifest = self.write(value)
        release = self.root / "unified"
        self.run_helper("render", manifest, release)

        broker = json.loads((release / "config/broker-desk-a7.json").read_text(encoding="utf-8"))
        self.assertEqual(
            broker["unified_terminal_dev"],
            {
                "enabled": True,
                "server": "primary",
                "session": "unified-dev",
                "observer_session": "observer-main",
                "runtime_dir": "/run/persea-terminal-desk-a7/unified-journal",
            },
        )
        self.assertIn(
            "unified_terminal_dev",
            json.loads((release / "config/broker-lab-k4.json").read_text(encoding="utf-8")),
        )
        guarded = (release / "units/persea-terminal-broker-desk-a7.service").read_text(encoding="utf-8")
        self.assertIn(
            "TemporaryFileSystem=/run/persea-terminal-desk-a7/unified-journal:"
            "rw,nosuid,nodev,noexec,noswap,size=96M,nr_inodes=256,mode=0700,uid=42001,gid=42003\n",
            guarded,
        )
        for directive in ("MemoryHigh=850M\n", "MemoryMax=1G\n", "Environment=GOMEMLIMIT=528MiB\n", "LimitCORE=0\n"):
            self.assertIn(directive, guarded)
        unguarded = (release / "units/persea-terminal-broker-lab-k4.service").read_text(encoding="utf-8")
        for token in ("TemporaryFileSystem=", "MemoryHigh=", "MemoryMax=", "GOMEMLIMIT=", "LimitCORE="):
            self.assertIn(token, unguarded)

        def rejected(block: object) -> None:
            candidate = fixture()
            candidate["realms"][0]["session_create"] = copy.deepcopy(value["realms"][0]["session_create"])
            candidate["realms"][0]["unified_terminal_dev"] = block
            self.run_helper("validate", self.write(candidate), success=False)

        for slots in (1, 64, 4096):
            candidate = copy.deepcopy(value)
            candidate["realms"][0]["unified_terminal_dev"]["adoption_slots"] = slots
            target = self.root / f"slots-{slots}"
            self.run_helper("render", self.write(candidate), target)
            rendered = json.loads((target / "config/broker-desk-a7.json").read_text(encoding="utf-8"))
            self.assertEqual(rendered["unified_terminal_dev"]["adoption_slots"], slots)
            self.assertEqual((target / "units/persea-terminal-broker-desk-a7.service").read_text(encoding="utf-8"), guarded)
        for slots in (0, -1, 4097, True, "64", 1.5, None):
            rejected({**value["realms"][0]["unified_terminal_dev"], "adoption_slots": slots})

        for block in (
            {"enabled": False, "server": "primary", "session": "unified-dev", "observer_session": "observer-main"},
            {"enabled": True, "server": "missing", "session": "unified-dev", "observer_session": "observer-main"},
            {"enabled": True, "server": "primary", "session": "bad.name", "observer_session": "observer-main"},
            {"enabled": True, "server": "primary", "session": "same", "observer_session": "same"},
            {
                "enabled": True,
                "server": "primary",
                "session": "unified-dev",
                "observer_session": "observer-main",
                "runtime_dir": "/tmp/operator-supplied",
            },
            {
                "enabled": True,
                "server": "primary",
                "session": "unified-dev",
                "observer_session": "observer-main",
                "memory_max": "2G",
            },
        ):
            rejected(block)

        duplicate = self.root / "duplicate-unified.json"
        duplicate.write_text(
            json.dumps(value, ensure_ascii=False).replace(
                '"unified_terminal_dev": {',
                '"unified_terminal_dev": {"enabled": true, "enabled": true,',
                1,
            ),
            encoding="utf-8",
        )
        self.run_helper("validate", duplicate, success=False)

    def test_image_staging_renders_configs_units_and_fails_incoherent_manifests(self) -> None:
        value = fixture()
        value["front"]["image_upload_max_bytes"] = 10485760
        value["realms"][0]["image_staging"] = True
        release = self.root / "staging"
        self.run_helper("render", self.write(value), release)

        front = json.loads((release / "config/front.json").read_text(encoding="utf-8"))
        self.assertEqual(front["image_upload_max_bytes"], 10485760)
        broker = json.loads((release / "config/broker-desk-a7.json").read_text(encoding="utf-8"))
        self.assertEqual(broker["image_staging_dir"], "/var/lib/persea-terminal-staging/desk-a7")
        self.assertNotIn(
            "image_staging_dir",
            json.loads((release / "config/broker-lab-k4.json").read_text(encoding="utf-8")),
        )
        guarded = (release / "units/persea-terminal-broker-desk-a7.service").read_text(encoding="utf-8")
        self.assertIn("ReadWritePaths=/var/lib/persea-terminal-staging/desk-a7\n", guarded)
        unguarded = (release / "units/persea-terminal-broker-lab-k4.service").read_text(encoding="utf-8")
        self.assertNotIn("ReadWritePaths=", unguarded)

        plain = self.root / "plain"
        self.run_helper("render", self.write(fixture(), "plain.json"), plain)
        self.assertIn(
            "StateDirectoryMode=0700\n",
            (plain / "units/persea-terminal-front.service").read_text(encoding="utf-8"),
        )
        self.assertNotIn(
            "ReadWritePaths=",
            (plain / "units/persea-terminal-broker-desk-a7.service").read_text(encoding="utf-8"),
        )

        # An endpoint with every realm explicitly disabled is incoherent.
        self.assert_invalid(lambda value: value["front"].update({"image_upload_max_bytes": 10485760}))
        for bad_bytes in (0, (1 << 20) - 1, (10 << 20) + 1, True, "10485760"):
            self.assert_invalid(lambda value, bad=bad_bytes: (
                value["front"].update({"image_upload_max_bytes": bad}),
                value["realms"][0].update({"image_staging": True}),
            ))
        self.assert_invalid(lambda value: (
            value["front"].update({"image_upload_max_bytes": 10485760}),
            value["realms"][0].update({"image_staging": "yes"}),
        ))

    def test_image_staging_defaults_to_every_realm_and_preserves_explicit_opt_out(self) -> None:
        value = fixture()
        for realm in value["realms"]:
            del realm["image_staging"]
        release = self.root / "default-staging"
        self.run_helper("render", self.write(value), release)
        front = json.loads((release / "config/front.json").read_text(encoding="utf-8"))
        self.assertEqual(front["image_upload_max_bytes"], 10 << 20)
        for realm in value["realms"]:
            realm_id = realm["id"]
            broker = json.loads((release / f"config/broker-{realm_id}.json").read_text(encoding="utf-8"))
            self.assertEqual(broker["image_staging_dir"], f"/var/lib/persea-terminal-staging/{realm_id}")
            unit = (release / f"units/persea-terminal-broker-{realm_id}.service").read_text(encoding="utf-8")
            self.assertIn(f"ReadWritePaths=/var/lib/persea-terminal-staging/{realm_id}\n", unit)

        value["realms"][0]["image_staging"] = False
        value["front"]["image_upload_max_bytes"] = 2 << 20
        disabled = self.root / "one-disabled"
        self.run_helper("render", self.write(value, "one-disabled.json"), disabled)
        front = json.loads((disabled / "config/front.json").read_text(encoding="utf-8"))
        self.assertEqual(front["image_upload_max_bytes"], 2 << 20)
        broker = json.loads((disabled / "config/broker-desk-a7.json").read_text(encoding="utf-8"))
        self.assertNotIn("image_staging_dir", broker)
        broker = json.loads((disabled / "config/broker-lab-k4.json").read_text(encoding="utf-8"))
        self.assertIn("image_staging_dir", broker)
        repeated = self.root / "one-disabled-again"
        self.run_helper("render", disabled / "config/host.json", repeated)
        for filename in ("front.json", "broker-desk-a7.json", "broker-lab-k4.json"):
            self.assertEqual((disabled / "config" / filename).read_bytes(), (repeated / "config" / filename).read_bytes())

    def test_staging_root_override_renders_coherently_and_fails_bad_roots(self) -> None:
        # The host-level front.staging_root override must land in one piece:
        # the broker config's image_staging_root and image_staging_dir, the
        # broker unit's ReadWritePaths, and the front config's
        # image_staging_root all derive from the same value, while the front
        # unit still carries nothing image-related.
        value = fixture()
        value["front"]["image_upload_max_bytes"] = 10485760
        value["front"]["staging_root"] = "/srv/persea-staging"
        value["realms"][0]["image_staging"] = True
        release = self.root / "staging-override"
        self.run_helper("render", self.write(value), release)

        broker = json.loads((release / "config/broker-desk-a7.json").read_text(encoding="utf-8"))
        self.assertEqual(broker["image_staging_root"], "/srv/persea-staging")
        self.assertEqual(broker["image_staging_dir"], "/srv/persea-staging/desk-a7")
        unguarded_broker = json.loads((release / "config/broker-lab-k4.json").read_text(encoding="utf-8"))
        self.assertNotIn("image_staging_root", unguarded_broker)
        self.assertNotIn("image_staging_dir", unguarded_broker)
        front = json.loads((release / "config/front.json").read_text(encoding="utf-8"))
        self.assertEqual(front["image_staging_root"], "/srv/persea-staging")
        guarded = (release / "units/persea-terminal-broker-desk-a7.service").read_text(encoding="utf-8")
        self.assertIn("ReadWritePaths=/srv/persea-staging/desk-a7\n", guarded)
        self.assertNotIn("persea-terminal-staging", guarded)
        front_unit = (release / "units/persea-terminal-front.service").read_text(encoding="utf-8")
        self.assertIn("StateDirectoryMode=0700\n", front_unit)
        self.assertNotIn("/srv/persea-staging", front_unit)
        resolved = json.loads((release / "config/resolved-host.json").read_text(encoding="utf-8"))
        self.assertEqual(resolved["front"]["staging_root"], "/srv/persea-staging")

        # Without the override nothing changes: no image_staging_root key, the
        # default root everywhere (the byte-stable path for existing hosts).
        value = fixture()
        value["front"]["image_upload_max_bytes"] = 10485760
        value["realms"][0]["image_staging"] = True
        default_release = self.root / "staging-default"
        self.run_helper("render", self.write(value, "default.json"), default_release)
        broker = json.loads((default_release / "config/broker-desk-a7.json").read_text(encoding="utf-8"))
        self.assertNotIn("image_staging_root", broker)
        self.assertEqual(broker["image_staging_dir"], "/var/lib/persea-terminal-staging/desk-a7")
        self.assertNotIn(
            "image_staging_root",
            json.loads((default_release / "config/front.json").read_text(encoding="utf-8")),
        )

        # The forbidden subtrees and unclean shapes fail at packaging, staging
        # enabled or not: the key is host policy, not a per-realm toggle.
        for bad_root in (
            "/var/lib/persea-terminal",
            "/var/lib/persea-terminal/staging",
            "/tmp",
            "/tmp/persea-staging",
            "/opt/persea-terminal",
            "/opt/persea-terminal/staging",
            "relative/path",
            "/srv/persea-staging/",
            "/srv/../srv/persea-staging",
            "/",
            "/srv/has space",
            "",
            42,
        ):
            self.assert_invalid(lambda value, bad=bad_root: value["front"].update({"staging_root": bad}))

    def test_front_state_directory_mode_is_0700_even_with_staging(self) -> None:
        # Pin for the 2026-08-20 activation failure: rendering image staging
        # support as StateDirectoryMode=0711 on the front unit violated the
        # alias store's startup invariant (its parent must have mode exactly
        # 0700), so the front refused to start and the deploy rolled back.
        # Image staging lives in its own root outside the state directory; the
        # front unit must keep 0700 and carry nothing image-related.
        value = fixture()
        value["front"]["image_upload_max_bytes"] = 10485760
        value["realms"][0]["image_staging"] = True
        release = self.root / "staging-front-mode"
        self.run_helper("render", self.write(value), release)
        front_unit = (release / "units/persea-terminal-front.service").read_text(encoding="utf-8")
        self.assertIn("StateDirectoryMode=0700\n", front_unit)
        self.assertNotIn("0711", front_unit)
        self.assertNotIn("persea-terminal-staging", front_unit)

    def test_front_store_paths_share_the_alias_store_home(self) -> None:
        # The preferences and snippet stores are the alias store's siblings
        # in the front's 0700 state directory: never the
        # staging root, never anywhere the staging override can move them.
        for name, mutate in (("default", None), ("staging-override", "/srv/persea-staging")):
            value = fixture()
            if mutate is not None:
                value["front"]["staging_root"] = mutate
                value["front"]["image_upload_max_bytes"] = 10485760
                value["realms"][0]["image_staging"] = True
            release = self.root / f"stores-{name}"
            self.run_helper("render", self.write(value, f"{name}.json"), release)
            front = json.loads((release / "config/front.json").read_text(encoding="utf-8"))
            alias_home = os.path.dirname(front["alias_store_path"])
            self.assertEqual(alias_home, "/var/lib/persea-terminal")
            self.assertEqual(front["preferences_store_path"], "/var/lib/persea-terminal/preferences.json")
            self.assertEqual(front["snippet_store_path"], "/var/lib/persea-terminal/snippets.json")
            self.assertEqual(front["workspace_store_path"], "/var/lib/persea-terminal/workspaces.json")
            self.assertEqual(front["keyboard_preferences_store_path"], "/var/lib/persea-terminal/keyboard-v1.json")
            self.assertEqual(
                len({front["alias_store_path"], front["preferences_store_path"], front["snippet_store_path"], front["workspace_store_path"], front["keyboard_preferences_store_path"]}), 5
            )
            staging_root = front.get("image_staging_root", "/var/lib/persea-terminal-staging")
            for key in ("preferences_store_path", "snippet_store_path", "workspace_store_path", "keyboard_preferences_store_path"):
                self.assertFalse(front[key].startswith(staging_root + "/"))
            self.assertNotIn("preferences", (release / "units/persea-terminal-front.service").read_text(encoding="utf-8"))
            self.run_helper("verify-release", self.write(value, f"{name}-verify.json"), release)

    def verify_store_paths_via_lib(self, front: dict, staging_root: str) -> subprocess.CompletedProcess[bytes]:
        release = self.root / "store-paths-release"
        release.mkdir(exist_ok=True)
        (release / "config").mkdir(exist_ok=True)
        (release / "config/front.json").write_text(json.dumps(front), encoding="utf-8")
        script = (
            "set -euo pipefail\n"
            'source "$1/lib.sh"\n'
            'SCRIPT_DIR="$1"\n'
            "PERSEA_HERMETIC=1\n"
            'persea_verify_front_store_paths "$2" "$3"\n'
        )
        return subprocess.run(
            ["bash", "-c", script, "bash", os.fspath(DEPLOY), os.fspath(release), staging_root],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )

    def test_front_store_path_verification_accepts_old_releases_and_refuses_drift(self) -> None:
        # verify.sh judges a release by its own front.json: a release rendered
        # before the stores existed carries neither key and passes; a release
        # that carries them must keep them beside the alias store.
        alias = "/var/lib/persea-terminal/aliases.json"
        staging = "/var/lib/persea-terminal-staging"
        good = {
            "old-release": {"alias_store_path": alias},
            "current": {
                "alias_store_path": alias,
                "preferences_store_path": "/var/lib/persea-terminal/preferences.json",
                "snippet_store_path": "/var/lib/persea-terminal/snippets.json",
                "workspace_store_path": "/var/lib/persea-terminal/workspaces.json",
                "keyboard_preferences_store_path": "/var/lib/persea-terminal/keyboard-v1.json",
            },
            "one-of-two": {"alias_store_path": alias, "snippet_store_path": "/var/lib/persea-terminal/snippets.json"},
        }
        for name, front in good.items():
            result = self.verify_store_paths_via_lib(front, staging)
            self.assertEqual(result.returncode, 0, msg=f"{name}: {result.stderr!r}")
        bad = {
            "keyboard-not-sibling": {"alias_store_path": alias, "keyboard_preferences_store_path": "/var/lib/other/keyboard-v1.json"},
            "keyboard-reuses-alias": {"alias_store_path": alias, "keyboard_preferences_store_path": alias},
            "keyboard-relative": {"alias_store_path": alias, "keyboard_preferences_store_path": "keyboard-v1.json"},
            "keyboard-unclean": {"alias_store_path": alias, "keyboard_preferences_store_path": "/var/lib/persea-terminal/./keyboard-v1.json"},
            "keyboard-reuses-workspace": {"alias_store_path": alias, "workspace_store_path": "/var/lib/persea-terminal/shared.json", "keyboard_preferences_store_path": "/var/lib/persea-terminal/shared.json"},
            "under-staging-root": {"alias_store_path": alias, "snippet_store_path": f"{staging}/snippets.json"},
            "workspace-under-staging-root": {"alias_store_path": alias, "workspace_store_path": f"{staging}/workspaces.json"},
            "not-a-sibling": {"alias_store_path": alias, "preferences_store_path": "/var/lib/other/preferences.json"},
            "relative": {"alias_store_path": alias, "snippet_store_path": "snippets.json"},
            "unclean": {"alias_store_path": alias, "snippet_store_path": "/var/lib/persea-terminal/../persea-terminal/snippets.json"},
            "reuses-alias": {"alias_store_path": alias, "preferences_store_path": alias},
            "reuses-each-other": {
                "alias_store_path": alias,
                "preferences_store_path": "/var/lib/persea-terminal/shared.json",
                "snippet_store_path": "/var/lib/persea-terminal/shared.json",
            },
            "workspace-reuses-snippet": {
                "alias_store_path": alias,
                "snippet_store_path": "/var/lib/persea-terminal/shared.json",
                "workspace_store_path": "/var/lib/persea-terminal/shared.json",
            },
            "not-a-string": {"alias_store_path": alias, "snippet_store_path": 1},
        }
        for name, front in bad.items():
            result = self.verify_store_paths_via_lib(front, staging)
            self.assertNotEqual(result.returncode, 0, msg=name)
            self.assertIn(b"front store paths are incoherent", result.stderr, msg=name)

    def test_size_encoding_and_unicode_fail_closed(self) -> None:
        oversize = self.root / "oversize.json"
        oversize.write_bytes(b" " * (64 * 1024 + 1))
        self.run_helper("validate", oversize, success=False)
        invalid_utf8 = self.root / "invalid-utf8.json"
        invalid_utf8.write_bytes(b"{\xff}")
        self.run_helper("validate", invalid_utf8, success=False)
        self.assert_invalid(lambda value: value["realms"][0].update({"display_name": "Operator\u200bQ7"}))
        self.assert_invalid(lambda value: value["realms"][0].update({"display_name": " padded "}))

    def test_nfkc_casefold_display_collision_fails(self) -> None:
        def mutate(value: dict) -> None:
            value["realms"][0]["display_name"] = "Kelvin K"
            value["realms"][1]["display_name"] = "Kelvin K"

        self.assert_invalid(mutate)

    def test_identifier_selector_and_path_mutants_fail(self) -> None:
        self.assert_invalid(lambda value: value["realms"][0].update({"id": "front"}))
        self.assert_invalid(lambda value: value["realms"][1].update({"id": "desk-a7"}))
        self.assert_invalid(lambda value: value["realms"][0].update({"id": "Desk-A7"}))

        def duplicate_selector(value: dict) -> None:
            value["realms"][0]["servers"].append({"label": "secondary", "socket_name": "main-a7"})

        self.assert_invalid(duplicate_selector)

        def cross_realm_path(value: dict) -> None:
            first = value["realms"][0]["servers"][0]
            first.pop("socket_name")
            first["socket_path"] = "/srv/shared-z8/main.sock"
            value["realms"][1]["servers"][0]["socket_path"] = "/srv/shared-z8/main.sock"

        self.assert_invalid(cross_realm_path)

        def cross_realm_named_selector_for_one_uid(value: dict) -> None:
            value["realms"][1]["user"] = value["realms"][0]["user"]
            value["realms"][1]["uid"] = value["realms"][0]["uid"]
            second = value["realms"][1]["servers"][0]
            second.pop("socket_path")
            second["socket_name"] = "main-a7"

        self.assert_invalid(cross_realm_named_selector_for_one_uid)

        def ambiguous_selector(value: dict) -> None:
            value["realms"][0]["servers"][0]["socket_path"] = "/srv/tmux-a7/main.sock"

        self.assert_invalid(ambiguous_selector)
        self.assert_invalid(
            lambda value: value["realms"][1]["servers"][0].update({"socket_path": "/run/persea-terminal-lab-k4/owned.sock"})
        )
        self.assert_invalid(lambda value: value["realms"][1]["servers"][0].update({"socket_path": "/srv/../tmp/main.sock"}))
        self.assert_invalid(lambda value: value["realms"][1]["servers"][0].update({"socket_path": "//srv/tmux-k4/main.sock"}))

    def test_identity_and_tailscale_cross_binding_mutants_fail(self) -> None:
        self.assert_invalid(lambda value: value["front"].update({"user": "Bad User"}))
        self.assert_invalid(lambda value: value["front"].update({"user": "root"}))
        self.assert_invalid(lambda value: value["front"].update({"uid": 0}))
        self.assert_invalid(lambda value: value["front"].update({"uid": 2**32 - 1}))
        self.assert_invalid(lambda value: value["front"].update({"group": "root"}))
        self.assert_invalid(lambda value: value["front"].update({"gid": 0}))
        self.assert_invalid(lambda value: value["realms"][1].update({"user": "root"}))
        self.assert_invalid(lambda value: value["realms"][1].update({"uid": 0}))
        self.assert_invalid(lambda value: value["ingress"].update({"canonical_host": "other.testing-f8.ts.net"}))
        self.assert_invalid(
            lambda value: value["tailscale"].update({"protected_main_dns_name": "terminal.testing-f8.ts.net"})
        )
        self.assert_invalid(lambda value: value["tailscale"].update({"tailnet_suffix": "public.example"}))

    def test_render_refuses_existing_or_symlink_output(self) -> None:
        manifest = self.write(fixture())
        existing = self.root / "existing"
        existing.mkdir()
        self.run_helper("render", manifest, existing, success=False)
        target = self.root / "target"
        target.mkdir()
        link = self.root / "link"
        link.symlink_to(target, target_is_directory=True)
        self.run_helper("render", manifest, link, success=False)

    def test_verify_release_rejects_generated_disagreement_and_residue(self) -> None:
        manifest = self.write(fixture())
        release = self.root / "release"
        self.run_helper("render", manifest, release)
        front = release / "config/front.json"
        front.write_bytes(front.read_bytes() + b" ")
        self.run_helper("verify-release", manifest, release, success=False)
        front.write_bytes(front.read_bytes()[:-1])
        (release / "units/unexpected.service").write_text("[Unit]\n", encoding="utf-8")
        self.run_helper("verify-release", manifest, release, success=False)

    def apply_release_modes(self, release: Path) -> None:
        for path in [release, *release.rglob("*")]:
            os.chown(path, -1, os.getgid())
            if path.is_dir() or path.relative_to(release).as_posix() == "bin/persea-terminal":
                os.chmod(path, 0o555)
            else:
                os.chmod(path, 0o444)

    def seal_release(self, release: Path) -> None:
        entries = sorted(
            path.relative_to(release).as_posix()
            for path in release.rglob("*")
            if path.is_file() and path.name != "MANIFEST.sha256"
        )
        (release / "MANIFEST.sha256").write_text(
            "".join(
                f"{hashlib.sha256((release / relative).read_bytes()).hexdigest()}  {relative}\n"
                for relative in entries
            ),
            encoding="utf-8",
        )
        self.apply_release_modes(release)

    def unseal_release(self, release: Path) -> None:
        os.chmod(release, 0o755)
        for path in release.rglob("*"):
            os.chmod(path, 0o755 if path.is_dir() else 0o644)

    def verify_release_via_lib(
        self, script_dir: Path, release: Path, function: str = "persea_verify_release"
    ) -> subprocess.CompletedProcess[bytes]:
        script = (
            "set -euo pipefail\n"
            'source "$1/lib.sh"\n'
            'SCRIPT_DIR="$2"\n'
            "PERSEA_HERMETIC=1\n"
            f'{function} "$3"\n'
        )
        return subprocess.run(
            ["bash", "-c", script, "bash", os.fspath(DEPLOY), os.fspath(script_dir), os.fspath(release)],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )

    def test_release_verification_pins_the_bundled_generator(self) -> None:
        # Cross-version pin for the 2026-08-20 deploy failure: verifying an
        # installed release by re-rendering its snapshot with the CURRENT
        # source tree's generator turns every legitimate renderer change into
        # a false tamper alarm on the previous release. Each release bundles
        # the generator that produced it; verification must use that copy.
        value = fixture()
        value["front"]["image_upload_max_bytes"] = 10485760
        value["realms"][0]["image_staging"] = True
        release = self.root / "release"
        self.run_helper("render", self.write(value), release)
        (release / "bin").mkdir()
        (release / "bin/persea-terminal").write_bytes(b"#!/bin/sh\nexit 0\n")
        (release / "libexec").mkdir()
        (release / "libexec/host-config.py").write_bytes(HELPER.read_bytes())

        # A future source tree whose renderer moved the staging path constant,
        # the exact shape of the incident's rename.
        future = self.root / "future-tree"
        future.mkdir()
        mutated = HELPER.read_bytes().replace(
            b"/var/lib/persea-terminal-staging", b"/var/lib/persea-terminal-imgnext"
        )
        self.assertNotEqual(mutated, HELPER.read_bytes())
        (future / "host-config.py").write_bytes(mutated)

        try:
            self.seal_release(release)
            # (a) The bundled generator verifies its own release even after
            # the current renderer moved on.
            result = self.verify_release_via_lib(future, release)
            self.assertEqual(result.returncode, 0, msg=result.stderr.decode())
            # (b) The replaced behavior is pinned as the bug: the future
            # renderer re-renders the same snapshot to different bytes.
            direct = subprocess.run(
                [
                    "python3",
                    os.fspath(future / "host-config.py"),
                    "verify-release",
                    os.fspath(release / "config/host.json"),
                    os.fspath(release),
                ],
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                check=False,
            )
            self.assertNotEqual(direct.returncode, 0)
            self.assertIn(b"disagrees with host snapshot", direct.stderr)
            # (c) Manifest-consistent tampering with a generated file is still
            # caught, by the bundled generator.
            self.unseal_release(release)
            broker = release / "config/broker-desk-a7.json"
            tampered = json.loads(broker.read_text(encoding="utf-8"))
            tampered["image_staging_dir"] = "/var/lib/persea-terminal-staging/lab-k4"
            broker.write_text(json.dumps(tampered, ensure_ascii=False), encoding="utf-8")
            self.seal_release(release)
            result = self.verify_release_via_lib(future, release)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn(b"release host snapshot and generated files disagree", result.stderr)
            # (d) A bundled generator whose bytes disagree with its checksum
            # manifest entry must never execute — including on the direct
            # metadata-verification path, which does not run the caller's
            # full-manifest hash check first.
            self.unseal_release(release)
            bundled = release / "libexec/host-config.py"
            bundled.write_bytes(bundled.read_bytes() + b"\n# tampered generator\n")
            self.apply_release_modes(release)
            result = self.verify_release_via_lib(
                future, release, function="persea_verify_release_metadata"
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertIn(b"release bundled generator disagrees with the checksum manifest", result.stderr)
        finally:
            self.unseal_release(release)

    def load_release_manifest_via_lib(
        self, script_dir: Path, release: Path
    ) -> subprocess.CompletedProcess[bytes]:
        script = (
            "set -euo pipefail\n"
            'source "$1/lib.sh"\n'
            'SCRIPT_DIR="$2"\n'
            "PERSEA_HERMETIC=1\n"
            'persea_load_release_manifest "$3"\n'
            'printf "%s\\n" "${PERSEA_REALM_IDS[@]}" "${PERSEA_BROKER_UNITS[@]}"\n'
        )
        return subprocess.run(
            ["bash", "-c", script, "bash", os.fspath(DEPLOY), os.fspath(script_dir), os.fspath(release)],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )

    def future_tree_refusing_old_records(self) -> Path:
        # A simulated future source tree whose `records` implementation
        # refuses the old schema outright; rendering and verification are
        # untouched. This is the sharpest cross-version shape: any schema
        # reinterpretation is a milder variant of the same selection bug.
        future = self.root / "future-tree"
        future.mkdir()
        mutated = HELPER.read_bytes().replace(
            b'values: list[str] = ["V1"]',
            b'raise ManifestError("future generator refused old records schema")',
        )
        self.assertNotEqual(mutated, HELPER.read_bytes())
        (future / "host-config.py").write_bytes(mutated)
        return future

    def test_release_record_loading_pins_the_bundled_generator(self) -> None:
        # Cross-version generator pin: rollback
        # verified a target release with its bundled generator but then loaded
        # its realm records with the CURRENT tree's `records` parser. The
        # NUL-record stream is a deploy ABI; a future parser that refuses or
        # reinterprets the old schema would fail — or silently reshape — a
        # release that just passed self-verification. Record loading must use
        # the same manifest-hash-pinned bundled generator as verification.
        value = fixture()
        release = self.root / "release"
        self.run_helper("render", self.write(value), release)
        (release / "bin").mkdir()
        (release / "bin/persea-terminal").write_bytes(b"#!/bin/sh\nexit 0\n")
        (release / "libexec").mkdir()
        (release / "libexec/host-config.py").write_bytes(HELPER.read_bytes())
        future = self.future_tree_refusing_old_records()

        try:
            self.seal_release(release)
            # (a) The sealed release passes self-verification under the future
            # tree: its bundled generator speaks for it.
            result = self.verify_release_via_lib(future, release)
            self.assertEqual(result.returncode, 0, msg=result.stderr.decode())
            # (b) Record loading under the same future
            # tree must select the bundled generator too, so it still yields
            # the release's own realm inventory.
            result = self.load_release_manifest_via_lib(future, release)
            self.assertEqual(result.returncode, 0, msg=result.stderr.decode())
            self.assertNotIn(b"future generator refused old records schema", result.stderr)
            self.assertEqual(
                result.stdout.decode().split(),
                [
                    "desk-a7",
                    "lab-k4",
                    "persea-terminal-broker-desk-a7.service",
                    "persea-terminal-broker-lab-k4.service",
                ],
            )
            # (c) The future parser genuinely refuses this snapshot when
            # invoked directly, proving (b) did not go through it.
            direct = subprocess.run(
                [
                    "python3",
                    os.fspath(future / "host-config.py"),
                    "records",
                    os.fspath(release / "config/host.json"),
                ],
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                check=False,
            )
            self.assertNotEqual(direct.returncode, 0)
            self.assertIn(b"future generator refused old records schema", direct.stderr)
            # (d) Tamper detection carries over unchanged: a bundled generator
            # whose bytes disagree with the checksum manifest never executes
            # for record loading either.
            self.unseal_release(release)
            bundled = release / "libexec/host-config.py"
            bundled.write_bytes(bundled.read_bytes() + b"\n# tampered generator\n")
            self.apply_release_modes(release)
            result = self.load_release_manifest_via_lib(future, release)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn(b"release bundled generator disagrees with the checksum manifest", result.stderr)
        finally:
            self.unseal_release(release)

    def test_generatorless_release_record_loading_mirrors_verification_fallback(self) -> None:
        # The explicit rule for generator-less releases mirrors verification:
        # a release that genuinely bundles no libexec/host-config.py is parsed
        # by the current source tree's generator, and when that generator no
        # longer speaks the release's schema the load fails closed with the
        # loader's own refusal — it never guesses at the stream.
        value = fixture()
        release = self.root / "release"
        self.run_helper("render", self.write(value), release)
        (release / "bin").mkdir()
        (release / "bin/persea-terminal").write_bytes(b"#!/bin/sh\nexit 0\n")
        future = self.future_tree_refusing_old_records()
        try:
            self.seal_release(release)
            result = self.load_release_manifest_via_lib(DEPLOY, release)
            self.assertEqual(result.returncode, 0, msg=result.stderr.decode())
            self.assertIn(b"desk-a7", result.stdout)
            result = self.load_release_manifest_via_lib(future, release)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn(b"host manifest parser returned an incomplete record stream", result.stderr)
        finally:
            self.unseal_release(release)

    def test_checked_in_tests_and_example_exclude_live_host_identities(self) -> None:
        live = Path("/etc/persea-terminal/host.json")
        if not live.is_file() or live.is_symlink():
            self.skipTest("no live host manifest is available")
        if not os.access(live, os.R_OK):
            self.skipTest("live host manifest is root-readable only; rerun this test as root")
        value = json.loads(live.read_text(encoding="utf-8"))
        forbidden = {
            value["front"]["user"],
            value["front"]["group"],
            value["ingress"]["operator_login"],
            value["ingress"]["canonical_host"],
            value["tailscale"]["tailnet_suffix"],
            value["tailscale"]["service"],
            value["tailscale"]["sidecar_hostname"],
            value["tailscale"]["sidecar_tag"],
            value["tailscale"]["protected_main_dns_name"],
            value["tailscale"]["protected_main_dns_name"].split(".", 1)[0],
            *(realm["user"] for realm in value["realms"]),
            *(realm["display_name"] for realm in value["realms"]),
        }
        scan_roots = [DEPLOY / "tests", DEPLOY / "host.example.json", PROJECT / "ui/test", PROJECT / "internal"]
        leaks: list[str] = []
        for root in scan_roots:
            paths = [root] if root.is_file() else list(root.rglob("*"))
            for path in paths:
                if not path.is_file() or "__pycache__" in path.parts:
                    continue
                try:
                    text = path.read_text(encoding="utf-8")
                except UnicodeDecodeError:
                    continue
                for identity in forbidden:
                    if identity and identity in text:
                        leaks.append(f"{path.relative_to(PROJECT)}:{identity!r}")
        self.assertEqual(leaks, [])


if __name__ == "__main__":
    unittest.main()
