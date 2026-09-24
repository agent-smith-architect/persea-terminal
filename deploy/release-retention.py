#!/usr/bin/env python3
"""Conservative release retention; called with the deployment lock held."""

import json
import os
from pathlib import Path
import re
import stat
import sys
import tempfile
import secrets
from typing import NamedTuple


RELEASE = re.compile(r"[0-9a-f]{40}-[0-9a-f]{16}")
ORDER = ".release-order.json"
QUARANTINE = re.compile(r"\.prune-[0-9a-f]{32}")
DIRECTORY_FLAGS = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC


class Inventory(NamedTuple):
    top: os.stat_result
    objects: dict
    children: dict


def object_metadata(info: os.stat_result) -> tuple:
    # Directory size, link count and timestamps change as we remove children.
    # Keep the stable security attributes, including type in st_mode.
    return (info.st_dev, info.st_ino, info.st_uid, info.st_gid, info.st_mode,
            info.st_nlink if stat.S_ISREG(info.st_mode) else None)


def witness(info: os.stat_result) -> tuple:
    # Reading inventories may update atime; it is not a mutation witness.
    return (info.st_dev, info.st_ino, info.st_mode, info.st_uid, info.st_gid,
            info.st_nlink, info.st_size, info.st_mtime_ns, info.st_ctime_ns)


def require(condition: bool, message: str) -> None:
    if not condition:
        raise ValueError(message)


def unique_object(pairs: list[tuple]) -> dict:
    result = {}
    for key, value in pairs:
        require(key not in result, "duplicate install-order ledger key")
        result[key] = value
    return result


def metadata(path: Path, uid: int, gid: int, mode: int, directory: bool) -> os.stat_result:
    value = path.lstat()
    kind = stat.S_ISDIR if directory else stat.S_ISREG
    require(kind(value.st_mode), f"unexpected file type: {path.name}")
    require((value.st_uid, value.st_gid, stat.S_IMODE(value.st_mode)) == (uid, gid, mode),
            f"unexpected owner or mode: {path.name}")
    require(directory or value.st_nlink == 1, f"hardlinked file: {path.name}")
    return value


def release_metadata(path: Path, uid: int, gid: int, device: int) -> Inventory:
    top = metadata(path, uid, gid, 0o555, True)
    require(top.st_dev == device, "release crosses a filesystem boundary")
    files = set()
    folders = set()
    objects = {"": object_metadata(top)}
    children = {}
    for parent, directories, names in os.walk(path, followlinks=False):
        relative_parent = Path(parent).relative_to(path).as_posix()
        children["" if relative_parent == "." else relative_parent] = frozenset(directories + names)
        for name in directories:
            directory = Path(parent) / name
            folders.add(directory.relative_to(path).as_posix())
            info = metadata(directory, uid, gid, 0o555, True)
            objects[directory.relative_to(path).as_posix()] = object_metadata(info)
            require(info.st_dev == device, "release directory crosses a filesystem boundary")
        for name in names:
            item = Path(parent) / name
            relative = item.relative_to(path).as_posix()
            mode = 0o555 if relative == "bin/persea-terminal" else 0o444
            info = metadata(item, uid, gid, mode, False)
            objects[relative] = object_metadata(info)
            require(info.st_dev == device, "release file crosses a filesystem boundary")
            files.add(relative)
    required = {"MANIFEST.sha256", "config/host.json", "config/resolved-host.json",
                "config/managed-units", "bin/persea-terminal"}
    require(required <= files, "release lacks the public-release shape")
    manifest = set()
    for line in (path / "MANIFEST.sha256").read_text(encoding="ascii").splitlines():
        match = re.fullmatch(r"[0-9a-f]{64}  ([A-Za-z0-9._/-]+)", line)
        require(match is not None, "malformed release manifest")
        relative = match[1]
        require(not relative.startswith("/") and ".." not in relative and "//" not in relative,
                "unsafe release manifest path")
        require(relative not in manifest, "duplicate release manifest path")
        manifest.add(relative)
    require(files == manifest | {"MANIFEST.sha256"} and "MANIFEST.sha256" not in manifest,
            "unexpected release file inventory")
    expected_folders = {parent.as_posix() for name in manifest
                        for parent in Path(name).parents if parent != Path(".")}
    require(folders == expected_folders, "unexpected release directory inventory")
    return Inventory(top, objects, children)


def require_no_mounts(path: Path) -> None:
    # st_dev alone cannot detect a bind mount of another directory on the same
    # filesystem. Do not traverse a mounted subtree even when its modes match.
    for line in Path("/proc/self/mountinfo").read_text(encoding="ascii").splitlines():
        fields = line.split()
        require(len(fields) >= 6, "cannot inspect mount boundaries")
        mountpoint = Path(re.sub(r"\\([0-7]{3})",
                                lambda match: chr(int(match[1], 8)), fields[4]))
        require(not mountpoint.is_relative_to(path),
                "release subtree contains a mount point")


def snapshot(root: Path, uid: int, gid: int) -> tuple[dict, set, dict]:
    metadata(root, uid, gid, 0o755, True)
    releases = root / "releases"
    device = metadata(releases, uid, gid, 0o755, True).st_dev
    entries = {}
    for path in releases.iterdir():
        if QUARANTINE.fullmatch(path.name):
            info = metadata(path, uid, gid, 0o700, True)
            require(info.st_dev == device, "quarantine crosses a filesystem boundary")
            print(f"persea-terminal deploy: warning: retained quarantine {path}; "
                  "operator inspection required", file=sys.stderr)
            continue
        require(RELEASE.fullmatch(path.name) is not None,
                "unexpected release entry or unfinished stage; retaining all releases")
        require_no_mounts(path)
        entries[path.name] = release_metadata(path, uid, gid, device)
    protected, pointers = protected_pointers(root, uid, entries)
    return entries, protected, pointers


def protected_pointers(root: Path, uid: int, entries: dict) -> tuple[set, dict]:
    releases = root / "releases"
    protected = set()
    pointers = {}
    for path in root.iterdir():
        if path.name in ("releases", ORDER):
            continue
        # Hidden transaction pointers can be part of a multi-step transition.
        # Keep everything until that transaction has been resolved.
        require(not path.name.startswith(".") and path.is_symlink(),
                "unexpected install-root entry or unfinished transaction")
        require(path.lstat().st_uid == uid, "pointer has unexpected owner")
        target = path.resolve(strict=True)
        require(target.parent == releases and target.name in entries,
                "pointer does not resolve to an immediate managed release")
        protected.add(target.name)
        pointers[path.name] = os.readlink(path)
    require("current" in pointers, "current release pointer is missing")
    return protected, pointers


def identity(info: os.stat_result) -> tuple[int, int]:
    return info.st_dev, info.st_ino


def mount_id(fd: int) -> str:
    # Unlike st_dev, Linux mount IDs distinguish same-filesystem bind mounts.
    # Inspect the opened object, not a pathname that can resolve differently.
    for line in Path(f"/proc/self/fdinfo/{fd}").read_text(encoding="ascii").splitlines():
        if line.startswith("mnt_id:"):
            value = line.split()[1]
            require(value.isdecimal(), "cannot identify directory mount")
            return value
    raise ValueError("cannot identify directory mount")


def child_path(relative: str, name: str) -> str:
    return f"{relative}/{name}" if relative else name


def check_directory(fd: int, mount: str, inventory: Inventory, relative: str,
                    remaining: set, writable: bool = False, checked: tuple | None = None) -> tuple:
    expected = inventory.objects[relative]
    if writable:
        expected = (*expected[:4], stat.S_IFDIR | 0o755, expected[5])
    before = os.fstat(fd)
    require(object_metadata(before) == expected and mount_id(fd) == mount,
            "validated directory replaced, changed or mounted")
    if witness(before) == checked:
        return checked
    require(set(os.listdir(fd)) == remaining, "validated directory entry set changed")
    # Check all immediate entries before changing this directory or touching any
    # child. A same-mode replacement is not a newly acceptable removal target.
    for name in remaining:
        info = os.stat(name, dir_fd=fd, follow_symlinks=False)
        require(object_metadata(info) == inventory.objects[child_path(relative, name)],
                "validated descendant replaced or changed")
    require(witness(os.fstat(fd)) == witness(before), "directory changed during inventory check")
    return witness(before)


def remove_tree(fd: int, mount: str, inventory: Inventory, relative: str = "") -> None:
    remaining = set(inventory.children[relative])
    check_directory(fd, mount, inventory, relative, remaining)
    # This changes only the opened, checked directory; no pathname chmod walk.
    os.fchmod(fd, 0o755)
    checked = check_directory(fd, mount, inventory, relative, remaining, writable=True)
    for name in sorted(remaining):
        checked = check_directory(fd, mount, inventory, relative, remaining,
                                  writable=True, checked=checked)
        path = child_path(relative, name)
        expected = inventory.objects[path]
        if path in inventory.children:
            child = os.open(name, DIRECTORY_FLAGS, dir_fd=fd)
            try:
                remove_tree(child, mount, inventory, path)
                changed = (*expected[:4], stat.S_IFDIR | 0o755, expected[5])
                require(object_metadata(os.stat(name, dir_fd=fd, follow_symlinks=False)) == changed,
                        "directory replaced during removal")
                check_directory(child, mount, inventory, path, set(), writable=True)
                check_directory(fd, mount, inventory, relative, remaining,
                                writable=True, checked=checked)
                os.rmdir(name, dir_fd=fd)
            finally:
                os.close(child)
        else:
            # O_PATH does not read contents or block on a substituted special file.
            before = os.stat(name, dir_fd=fd, follow_symlinks=False)
            child = os.open(name, os.O_PATH | os.O_NOFOLLOW | os.O_CLOEXEC, dir_fd=fd)
            try:
                opened = os.fstat(child)
                require(object_metadata(opened) == expected and witness(opened) == witness(before)
                        and mount_id(child) == mount,
                        "file replaced or mounted during removal")
                check_directory(fd, mount, inventory, relative, remaining,
                                writable=True, checked=checked)
                require(witness(os.stat(name, dir_fd=fd, follow_symlinks=False)) == witness(opened),
                        "file replaced or changed before unlink")
                os.unlink(name, dir_fd=fd)
            finally:
                os.close(child)
        remaining.remove(name)
        # Our unlink/rmdir changes the parent witness. Accept that mutation;
        # later unexpected changes trigger a full rescan. Each selected object
        # is still checked against the original inventory at its own boundary,
        # since metadata changes to a child need not change its parent witness.
        checked = witness(os.fstat(fd))
    check_directory(fd, mount, inventory, relative, remaining, writable=True)


def remove_release(root: Path, parent: int, name: str, inventory: Inventory,
                   entries: dict, pointers: dict, uid: int, gid: int) -> None:
    expected = inventory.top
    victim = os.open(name, DIRECTORY_FLAGS, dir_fd=parent)
    quarantine = None
    quarantine_fd = None
    try:
        require(witness(os.fstat(victim)) == witness(expected), "release changed before removal")
        require(mount_id(victim) == mount_id(parent), "release crosses a mount boundary")
        quarantine = ".prune-" + secrets.token_hex(16)
        os.mkdir(quarantine, mode=0o700, dir_fd=parent)
        quarantine_fd = os.open(quarantine, DIRECTORY_FLAGS, dir_fd=parent)
        qinfo = os.fstat(quarantine_fd)
        require((qinfo.st_uid, qinfo.st_gid, stat.S_IMODE(qinfo.st_mode)) == (uid, gid, 0o700)
                and qinfo.st_dev == expected.st_dev and mount_id(quarantine_fd) == mount_id(parent),
                "unsafe quarantine directory")
        # Re-read all pointers for every victim, after opening it and immediately
        # before detaching its name. Cooperating transactions hold the same lock.
        protected, fresh_pointers = protected_pointers(root, uid, entries)
        require(name not in protected and fresh_pointers == pointers,
                "release pointers changed before removal")
        require(witness(os.stat(name, dir_fd=parent, follow_symlinks=False)) == witness(expected),
                "release replaced before quarantine")
        check_directory(victim, mount_id(parent), inventory, "", set(inventory.children[""]))
        # Moving a directory between parents requires owner write permission to
        # update '..' when exercising this same path without root in tests.
        os.fchmod(victim, 0o755)
        require(witness(os.stat(name, dir_fd=parent, follow_symlinks=False)) == witness(os.fstat(victim)),
                "release replaced before quarantine rename")
        os.rename(name, name, src_dir_fd=parent, dst_dir_fd=quarantine_fd)
        require(identity(os.stat(name, dir_fd=quarantine_fd, follow_symlinks=False)) == identity(expected),
                "quarantined release identity changed")
        require(identity(os.fstat(victim)) == identity(expected), "opened release identity changed")
        check_directory(victim, mount_id(parent), inventory, "", set(inventory.children[""]),
                        writable=True)
        os.fchmod(victim, 0o555)
        _, fresh_pointers = protected_pointers(root, uid, entries)
        require(fresh_pointers == pointers, "release pointers changed during quarantine")
        require_no_mounts(root / "releases" / quarantine)
        remove_tree(victim, mount_id(parent), inventory)
        require(identity(os.stat(name, dir_fd=quarantine_fd, follow_symlinks=False)) == identity(expected),
                "quarantined release replaced during removal")
        os.rmdir(name, dir_fd=quarantine_fd)
        require(identity(os.stat(quarantine, dir_fd=parent, follow_symlinks=False)) == identity(qinfo),
                "quarantine replaced during removal")
        os.rmdir(quarantine, dir_fd=parent)
        quarantine = None
    finally:
        os.close(victim)
        if quarantine_fd is not None:
            os.close(quarantine_fd)
        if quarantine is not None:
            print(f"persea-terminal deploy: warning: retained quarantine "
                  f"{root / 'releases' / quarantine}; operator inspection required", file=sys.stderr)


def write_order(root: Path, order: list[str]) -> None:
    payload = json.dumps({"version": 1, "releases": order}, separators=(",", ":")) + "\n"
    target = root / ORDER
    if target.exists() and target.read_text(encoding="ascii") == payload:
        return
    fd, temporary = tempfile.mkstemp(prefix=".release-order.", dir=root)
    try:
        with os.fdopen(fd, "w", encoding="ascii") as stream:
            stream.write(payload)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, target)
        directory = os.open(root, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def prune(root: Path, keep: int, uid: int, gid: int, live_binaries: list[str]) -> None:
    require(sys.version_info >= (3, 11), "release retention requires Python 3.11 or later")
    require(root.is_absolute() and root.resolve(strict=True) == root,
            "install root contains a symlink or ambiguous path")
    entries, protected, pointers = snapshot(root, uid, gid)
    running = set()
    for binary_identity in live_binaries:
        matches = []
        for name in entries:
            info = (root / "releases" / name / "bin/persea-terminal").stat()
            if binary_identity == f"{info.st_dev}:{info.st_ino}":
                matches.append(name)
        require(len(matches) == 1, "running executable does not identify exactly one release")
        running.update(matches)
    protected.update(running)
    order_path = root / ORDER
    order = []
    if os.path.lexists(order_path):
        metadata(order_path, uid, gid, 0o600, False)
        value = json.loads(order_path.read_text(encoding="ascii"), object_pairs_hook=unique_object)
        require(isinstance(value, dict) and set(value) == {"version", "releases"}
                and type(value["version"]) is int and value["version"] == 1
                and isinstance(value["releases"], list),
                "invalid install-order ledger")
        order = value["releases"]
        require(all(isinstance(name, str) and RELEASE.fullmatch(name) for name in order),
                "invalid release in install-order ledger")
        require(len(set(order)) == len(order), "duplicate install-order entry")
    # Earlier installers recorded no order. Bootstrap their immutable directory
    # mtimes at nanosecond precision, with an explicit deterministic tie breaker.
    order = [name for name in order if name in entries]
    known = set(order)
    order += sorted(entries.keys() - known, key=lambda name: (entries[name].top.st_mtime_ns, name))
    write_order(root, order)
    if keep == 0:
        return
    require(len(protected) <= keep, "protected pointers exceed retention limit")
    survivors = set(protected)
    for name in reversed(order):
        if len(survivors) >= keep:
            break
        survivors.add(name)
    victims = [name for name in order if name not in survivors]
    if not victims:
        return
    # Recheck the complete plan before the first deletion. Cooperative installers,
    # rollback and uninstall hold the same lock throughout their transactions.
    fresh, fresh_protected, fresh_pointers = snapshot(root, uid, gid)
    require({name: (witness(info.top), info.objects, info.children) for name, info in fresh.items()} ==
            {name: (witness(info.top), info.objects, info.children) for name, info in entries.items()} and
            fresh_protected | running == protected and fresh_pointers == pointers,
            "release tree or pointers changed during retention")
    releases = root / "releases"
    parent_info = metadata(releases, uid, gid, 0o755, True)
    fd = os.open(releases, DIRECTORY_FLAGS)
    try:
        require(identity(os.fstat(fd)) == identity(parent_info), "releases directory changed")
        for name in victims:
            require(identity(releases.lstat()) == identity(parent_info), "releases directory replaced")
            remove_release(root, fd, name, entries[name], entries, pointers, uid, gid)
    finally:
        os.close(fd)
    write_order(root, [name for name in order if name in survivors])


def main() -> int:
    try:
        root, keep, uid, gid, *live_binaries = sys.argv[1:]
        require(keep == "0" or re.fullmatch(r"[2-9]|[1-9][0-9]{1,8}", keep) is not None,
                "invalid retention limit")
        prune(Path(root), int(keep), int(uid), int(gid), live_binaries)
        return 0
    except (OSError, ValueError, TypeError, KeyError, RuntimeError) as error:
        print(f"persea-terminal deploy: warning: release retention: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
