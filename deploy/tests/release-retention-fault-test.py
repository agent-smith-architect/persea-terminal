#!/usr/bin/env python3
"""Deterministic races at the privileged removal boundary, in disposable trees."""

import hashlib
import importlib.util
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch


HELPER = Path(os.environ.get("PERSEA_TEST_RETENTION_HELPER",
                            Path(__file__).resolve().parents[1] / "release-retention.py"))
MOUNT_WORKER = "--mount-worker" in sys.argv
if MOUNT_WORKER:
    sys.argv.remove("--mount-worker")


def payload(path):
    (path / "config").mkdir(parents=True)
    (path / "bin").mkdir()
    for name in ("bin/persea-terminal", "config/host.json", "config/resolved-host.json",
                 "config/managed-units"):
        item = path / name
        item.write_text("fixture\n")
        item.chmod(0o555 if name.startswith("bin/") else 0o444)
    files = sorted(item for item in path.rglob("*") if item.is_file())
    (path / "MANIFEST.sha256").write_text("".join(
        hashlib.sha256(item.read_bytes()).hexdigest() + "  " + str(item.relative_to(path)) + "\n"
        for item in files))
    (path / "MANIFEST.sha256").chmod(0o444)
    for parent, _, _ in os.walk(path):
        os.chmod(parent, 0o555)


class RetentionFaultTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="retention-fault-")
        self.work = Path(self.temporary.name).resolve()
        self.root = self.work / "install"
        self.releases = self.root / "releases"
        self.releases.mkdir(parents=True)
        self.names = [f"{i:040x}-{'a' * 16}" for i in range(4)]
        for i, name in enumerate(self.names):
            payload(self.releases / name)
            os.utime(self.releases / name, ns=(i + 1, i + 1))
        (self.root / "current").symlink_to("releases/" + self.names[0])
        (self.root / "previous").symlink_to("releases/" + self.names[1])
        self.victim = self.releases / self.names[2]
        self.outside = self.work / "outside"
        self.outside.mkdir()
        self.sentinel = self.outside / "sentinel"
        self.sentinel.write_text("preserve\n")
        self.outside_mode = self.outside.stat().st_mode
        self.mounted = None
        spec = importlib.util.spec_from_file_location("retention", HELPER)
        self.helper = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(self.helper)

    def tearDown(self):
        if self.mounted is not None:
            subprocess.run(["umount", str(self.mounted)], check=True)
        for parent, _, _ in os.walk(self.work, followlinks=False):
            os.chmod(parent, 0o755)
        self.temporary.cleanup()

    def prune(self):
        self.helper.prune(self.root, 2, os.getuid(), os.getgid(), [])

    def at_removal(self, action):
        # The old implementation's last boundary is rmtree; the new one opens
        # and quarantines the candidate inside remove_release. No product hooks.
        owner, name = (self.helper, "remove_release") if hasattr(self.helper, "remove_release") \
            else (self.helper.shutil, "rmtree")
        original = getattr(owner, name)
        fired = False
        def intercept(*args, **kwargs):
            nonlocal fired
            if not fired:
                fired = True
                action()
            return original(*args, **kwargs)
        if hasattr(original, "avoids_symlink_attacks"):
            intercept.avoids_symlink_attacks = original.avoids_symlink_attacks
        return patch.object(owner, name, intercept)

    def assert_sentinel(self):
        self.assertEqual(self.sentinel.read_text(), "preserve\n")
        self.assertEqual(self.outside.stat().st_mode, self.outside_mode)

    def test_pointer_changes_to_victim_at_removal(self):
        def change_pointer():
            (self.root / "current").unlink()
            (self.root / "current").symlink_to("releases/" + self.victim.name)
        with self.at_removal(change_pointer), self.assertRaises((ValueError, OSError)):
            self.prune()
        self.assertTrue((self.root / "current/bin/persea-terminal").is_file())
        self.assertTrue(self.victim.is_dir())
        self.assert_sentinel()

    def test_foreign_directory_replaces_victim_at_removal(self):
        def replace():
            self.victim.chmod(0o755)
            self.victim.rename(self.work / "validated")
            self.outside.rename(self.victim)
        with self.at_removal(replace), self.assertRaises((ValueError, OSError)):
            self.prune()
        self.assertEqual((self.victim / "sentinel").read_text(), "preserve\n")
        self.assertEqual(self.victim.stat().st_mode, self.outside_mode)
        self.assertTrue((self.work / "validated/bin/persea-terminal").is_file())

    def test_foreign_replacement_during_quarantine_is_retained(self):
        if not hasattr(self.helper, "remove_release"):
            self.skipTest("quarantine-specific boundary is absent in the old implementation")
        original = os.rename
        def replace(src, dst, **kwargs):
            if src == self.victim.name and "src_dir_fd" in kwargs:
                self.victim.chmod(0o755)
                original(self.victim, self.work / "validated")
                original(self.outside, self.victim)
            return original(src, dst, **kwargs)
        with patch.object(os, "rename", replace), self.assertRaises((OSError, ValueError)):
            self.prune()
        self.assertTrue((self.work / "validated/bin/persea-terminal").is_file())
        quarantines = list(self.releases.glob(".prune-*"))
        self.assertEqual(len(quarantines), 1)
        self.assertEqual((quarantines[0] / self.victim.name / "sentinel").read_text(), "preserve\n")
        self.assertEqual((quarantines[0] / self.victim.name).stat().st_mode, self.outside_mode)
        # Residue is reported but never entered and does not block later pruning.
        self.prune()
        self.assertFalse((self.releases / self.names[3]).exists())
        self.assertTrue((quarantines[0] / self.victim.name / "sentinel").is_file())

    def test_pointer_rechecked_for_each_victim(self):
        if not hasattr(self.helper, "remove_release"):
            self.skipTest("per-victim boundary is absent in the old implementation")
        original = self.helper.remove_release
        def remove(*args, **kwargs):
            original(*args, **kwargs)
            (self.root / "current").unlink()
            (self.root / "current").symlink_to("releases/" + self.names[3])
        with patch.object(self.helper, "remove_release", remove), self.assertRaises((OSError, ValueError)):
            self.prune()
        self.assertTrue((self.root / "current/bin/persea-terminal").is_file())

    def test_nested_directory_exchange_preserves_both_trees(self):
        original = self.helper.remove_tree
        moved = self.work / "validated-config"
        replacement = None
        self.outside.chmod(0o555)
        self.sentinel.chmod(0o444)
        def exchange(*args, **kwargs):
            nonlocal replacement
            if replacement is None:
                quarantine = next(self.releases.glob(".prune-*"))
                replacement = quarantine / self.victim.name / "config"
                replacement.parent.chmod(0o755)
                replacement.chmod(0o755)
                replacement.rename(moved)
                moved.chmod(0o555)
                self.outside.chmod(0o755)
                self.outside.rename(replacement)
                replacement.chmod(0o555)
                replacement.parent.chmod(0o555)
            return original(*args, **kwargs)
        with patch.object(self.helper, "remove_tree", exchange), self.assertRaises((ValueError, OSError)):
            self.prune()
        self.assertIsNotNone(replacement, "exchange injection did not run")
        self.assertEqual((replacement / "sentinel").read_text(), "preserve\n")
        self.assertEqual(replacement.stat().st_mode & 0o7777, 0o555)
        self.assertEqual((replacement / "sentinel").stat().st_mode & 0o7777, 0o444)
        self.assertEqual(moved.stat().st_mode & 0o7777, 0o555)
        self.assertEqual({item.name for item in moved.iterdir()},
                         {"host.json", "resolved-host.json", "managed-units"})
        for item in moved.iterdir():
            self.assertEqual(item.read_text(), "fixture\n")
            self.assertEqual(item.stat().st_mode & 0o7777, 0o444)

    def assert_inventory_refusal(self, change):
        original = self.helper.remove_tree
        target = None
        def inject(*args, **kwargs):
            nonlocal target
            if target is None:
                target = next(self.releases.glob(".prune-*")) / self.victim.name / "config"
                change(target)
            return original(*args, **kwargs)
        with patch.object(self.helper, "remove_tree", inject), self.assertRaises((ValueError, OSError)):
            self.prune()
        self.assertIsNotNone(target, "inventory mutation did not run")
        self.assertEqual(target.stat().st_mode & 0o7777, 0o555)
        return target

    def test_added_nested_entry_is_retained_without_chmod(self):
        def add(target):
            target.chmod(0o755)
            (target / "addition").write_text("preserve\n")
            (target / "addition").chmod(0o444)
            target.chmod(0o555)
        target = self.assert_inventory_refusal(add)
        self.assertEqual((target / "addition").read_text(), "preserve\n")
        self.assertEqual((target / "addition").stat().st_mode & 0o7777, 0o444)
        self.assertEqual((target / "host.json").read_text(), "fixture\n")

    def test_replaced_nested_file_preserves_both_files(self):
        moved = self.work / "validated-host.json"
        def replace(target):
            target.chmod(0o755)
            (target / "host.json").rename(moved)
            self.sentinel.rename(target / "host.json")
            (target / "host.json").chmod(0o444)
            target.chmod(0o555)
        target = self.assert_inventory_refusal(replace)
        self.assertEqual((target / "host.json").read_text(), "preserve\n")
        self.assertEqual((target / "host.json").stat().st_mode & 0o7777, 0o444)
        self.assertEqual(moved.read_text(), "fixture\n")
        self.assertEqual(moved.stat().st_mode & 0o7777, 0o444)

    def test_new_hardlink_is_retained(self):
        def link(target):
            os.link(target / "host.json", self.outside / "alias")
        target = self.assert_inventory_refusal(link)
        self.assertEqual((target / "host.json").stat().st_nlink, 2)
        self.assertEqual((self.outside / "alias").read_text(), "fixture\n")

    def mount_case(self, after_mount_check):
        if not MOUNT_WORKER:
            prefix = [] if os.geteuid() == 0 else ["sudo", "-n"]
            probe = subprocess.run(prefix + ["unshare", "--mount", "--propagation", "private", "true"],
                                   capture_output=True, text=True)
            if probe.returncode:
                self.skipTest("private mount namespaces unavailable: " + probe.stderr.strip())
            result = subprocess.run(prefix + ["unshare", "--mount", "--propagation", "private",
                "env", "PERSEA_TEST_RETENTION_HELPER=" + str(HELPER),
                sys.executable, str(Path(__file__).resolve()), "--mount-worker",
                self.id().removeprefix("__main__.")],
                capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            return
        # Match a valid release's metadata so mode checks cannot mask a missing
        # mount-boundary check in the traversal sensitivity test.
        self.outside.chmod(0o555)
        self.sentinel.chmod(0o444)
        self.outside_mode = self.outside.stat().st_mode
        def mount(path):
            subprocess.run(["mount", "--bind", str(self.outside), str(path)], check=True)
            self.mounted = path
        if hasattr(self.helper, "remove_release"):
            boundary = "remove_tree" if after_mount_check else "require_no_mounts"
            original = getattr(self.helper, boundary)
            def intercept(*args, **kwargs):
                quarantine = list(self.releases.glob(".prune-*"))
                if quarantine and self.mounted is None:
                    mount(quarantine[0] / self.victim.name / "config")
                return original(*args, **kwargs)
            injection = patch.object(self.helper, boundary, intercept)
        else:
            injection = self.at_removal(lambda: mount(self.victim / "config"))
        with injection, self.assertRaises((OSError, ValueError)):
            self.prune()
        self.assertIsNotNone(self.mounted, "mount injection did not run")
        self.assert_sentinel()

    def test_late_bind_mount_before_deletion(self):
        self.mount_case(False)

    def test_bind_mount_after_mount_table_check(self):
        self.mount_case(True)


if __name__ == "__main__":
    os.umask(0o022)
    unittest.main()
