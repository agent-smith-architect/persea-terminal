#!/usr/bin/env python3
"""Exercise pruning against copies of a real hermetic installer payload."""

import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


DEPLOY = Path(__file__).resolve().parents[1]
SOURCE = Path(sys.argv.pop(1))
SCRATCH = Path(sys.argv.pop(1))


class RetentionTests(unittest.TestCase):
    def setUp(self) -> None:
        self.work = Path(tempfile.mkdtemp(prefix="retention-", dir=SCRATCH))
        self.root = self.work / "opt/persea-terminal"
        self.releases = self.root / "releases"
        self.releases.mkdir(parents=True, mode=0o755)
        self.names = [f"{index:040x}-{'a' * 16}" for index in range(8)]
        for index, name in enumerate(self.names):
            target = self.releases / name
            shutil.copytree(SOURCE, target)
            os.utime(target, ns=(index + 1, index + 1))
        (self.root / "current").symlink_to("releases/" + self.names[0])
        (self.root / "previous").symlink_to("releases/" + self.names[1])
        self.mocks = self.work / "mocks"
        self.mocks.mkdir()
        systemctl = self.mocks / "systemctl"
        systemctl.write_text("#!/bin/sh\nprintf '0\\n'\n")
        systemctl.chmod(0o755)

    def tearDown(self) -> None:
        for parent, directories, _ in os.walk(self.work, followlinks=False):
            os.chmod(parent, 0o755)
        shutil.rmtree(self.work)

    def prune(self, keep: int = 5, skipped: bool = False) -> None:
        # Use the same warning-only wrapper as install and rollback.
        command = (
            'source "$1/lib.sh"; SCRIPT_DIR=$1; PERSEA_HERMETIC=1; '
            'PERSEA_ROOT_PREFIX=$2; persea_prune_releases "$3"'
        )
        result = subprocess.run(
            ["bash", "-euo", "pipefail", "-c", command, "retention",
            str(DEPLOY), str(self.work), str(keep)],
            env={**os.environ, "PATH": str(self.mocks) + ":" + os.environ["PATH"],
                 "PERSEA_DEPLOY_MOCK_BIN": str(self.mocks)},
            capture_output=True, text=True, check=False,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual("warning:" in result.stderr, skipped, result.stderr)

    def remaining(self) -> set[str]:
        return {path.name for path in self.releases.iterdir()}

    def test_default_keeps_current_previous_and_newest(self) -> None:
        self.prune()
        self.assertEqual(self.remaining(), set(self.names[:2] + self.names[-3:]))

    def test_minimum_larger_limit_and_disabled(self) -> None:
        self.prune(20)
        self.assertEqual(self.remaining(), set(self.names))
        self.prune(0)
        self.assertEqual(self.remaining(), set(self.names))
        self.prune(2)
        self.assertEqual(self.remaining(), set(self.names[:2]))

    def test_recorded_order_wins_over_changed_mtime(self) -> None:
        self.prune(0)
        os.utime(self.releases / self.names[2], ns=(10**18, 10**18))
        self.prune(3)
        self.assertEqual(self.remaining(), set(self.names[:2] + self.names[-1:]))

    def test_other_pointer_is_protected(self) -> None:
        (self.root / "saved").symlink_to(self.releases / self.names[2])
        self.prune(4)
        self.assertEqual(self.remaining(), set(self.names[:3] + self.names[-1:]))

    def test_running_executable_is_protected(self) -> None:
        (self.mocks / "systemctl").write_text("#!/bin/sh\nprintf '123\\n'\n")
        binary = self.releases / self.names[2] / "bin/persea-terminal"
        info = binary.stat()
        witness = self.mocks / "persea-process-metadata"
        witness.write_text(f"#!/bin/sh\nprintf '{os.getuid()}:{info.st_dev}:{info.st_ino}\\n'\n")
        witness.chmod(0o755)
        self.prune(4)
        self.assertEqual(self.remaining(), set(self.names[:3] + self.names[-1:]))

    def test_too_many_protected_releases_skip_all_deletion(self) -> None:
        (self.root / "saved").symlink_to("releases/" + self.names[2])
        self.prune(2, skipped=True)
        self.assertEqual(self.remaining(), set(self.names))

    def test_unexpected_entries_and_transactions_skip_all_deletion(self) -> None:
        for parent, name in ((self.releases, "foreign"), (self.releases, ".stage.new.123"),
                             (self.releases, "bridge-" + "a" * 16),
                             (self.root, "transaction"), (self.root, ".current.new.123")):
            with self.subTest(name=name):
                entry = parent / name
                entry.mkdir()
                self.prune(2, skipped=True)
                entry.rmdir()
                self.assertEqual(self.remaining(), set(self.names))

    def test_unsafe_release_modes_and_inventory_skip_all_deletion(self) -> None:
        victim = self.releases / self.names[2]
        os.chmod(victim, 0o755)
        self.prune(2, skipped=True)
        extra = victim / "unexpected"
        extra.write_text("foreign data")
        os.chmod(extra, 0o444)
        os.chmod(victim, 0o555)
        self.prune(2, skipped=True)
        self.assertEqual(self.remaining(), set(self.names))

    def test_release_symlink_and_hardlink_leave_external_data_untouched(self) -> None:
        outside = self.work / "outside"
        outside.mkdir()
        sentinel = outside / "sentinel"
        sentinel.write_text("preserve")
        victim = self.releases / self.names[2]
        for parent, _, _ in os.walk(victim):
            os.chmod(parent, 0o755)
        shutil.rmtree(victim)
        victim.symlink_to(outside, target_is_directory=True)
        self.prune(2, skipped=True)
        self.assertEqual(sentinel.read_text(), "preserve")
        victim.unlink()
        shutil.copytree(SOURCE, victim)
        os.link(victim / "config/host.json", outside / "linked")
        self.prune(2, skipped=True)
        self.assertTrue((outside / "linked").is_file())
        self.assertEqual(self.remaining(), set(self.names))

    def test_nested_symlink_and_external_pointer_skip_all_deletion(self) -> None:
        victim = self.releases / self.names[2]
        os.chmod(victim, 0o755)
        (victim / "escape").symlink_to(self.work)
        os.chmod(victim, 0o555)
        self.prune(2, skipped=True)
        os.chmod(victim, 0o755)
        (victim / "escape").unlink()
        os.chmod(victim, 0o555)
        (self.root / "saved").symlink_to(self.work)
        self.prune(2, skipped=True)
        self.assertEqual(self.remaining(), set(self.names))

    def test_invalid_ledger_is_not_replaced(self) -> None:
        path = self.root / ".release-order.json"
        value = json.dumps({"version": 1, "releases": [self.names[0], self.names[0]]})
        path.write_text(value)
        os.chmod(path, 0o600)
        self.prune(2, skipped=True)
        self.assertEqual(path.read_text(), value)
        self.assertEqual(self.remaining(), set(self.names))

    def test_wrong_owner_skips_all_deletion(self) -> None:
        victim = self.releases / self.names[2]
        subprocess.run(["sudo", "-n", "chown", "0:0", str(victim)], check=True)
        try:
            self.prune(2, skipped=True)
            self.assertEqual(self.remaining(), set(self.names))
        finally:
            subprocess.run(["sudo", "-n", "chown", f"{os.getuid()}:{os.getgid()}",
                            str(victim)], check=True)


if __name__ == "__main__":
    unittest.main()
