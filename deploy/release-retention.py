#!/usr/bin/env python3
"""Conservative release retention; called with the deployment lock held."""

import json
import os
from pathlib import Path
import re
import shutil
import stat
import sys
import tempfile


RELEASE = re.compile(r"[0-9a-f]{40}-[0-9a-f]{16}")
ORDER = ".release-order.json"


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


def release_metadata(path: Path, uid: int, gid: int, device: int) -> os.stat_result:
    top = metadata(path, uid, gid, 0o555, True)
    require(top.st_dev == device, "release crosses a filesystem boundary")
    files = set()
    folders = set()
    for parent, directories, names in os.walk(path, followlinks=False):
        for name in directories:
            directory = Path(parent) / name
            folders.add(directory.relative_to(path).as_posix())
            info = metadata(directory, uid, gid, 0o555, True)
            require(info.st_dev == device, "release directory crosses a filesystem boundary")
        for name in names:
            item = Path(parent) / name
            relative = item.relative_to(path).as_posix()
            mode = 0o555 if relative == "bin/persea-terminal" else 0o444
            info = metadata(item, uid, gid, mode, False)
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
    return top


def snapshot(root: Path, uid: int, gid: int) -> tuple[dict, set, dict]:
    metadata(root, uid, gid, 0o755, True)
    releases = root / "releases"
    device = metadata(releases, uid, gid, 0o755, True).st_dev
    # st_dev alone cannot detect a bind mount of another directory on the same
    # filesystem. Do not traverse a mounted subtree even when its modes match.
    for line in Path("/proc/self/mountinfo").read_text(encoding="ascii").splitlines():
        fields = line.split()
        require(len(fields) >= 6, "cannot inspect mount boundaries")
        mountpoint = Path(re.sub(r"\\([0-7]{3})",
                                lambda match: chr(int(match[1], 8)), fields[4]))
        require(not mountpoint.is_relative_to(releases) or mountpoint == releases,
                "release subtree contains a mount point")
    entries = {}
    for path in releases.iterdir():
        require(RELEASE.fullmatch(path.name) is not None,
                "unexpected release entry or unfinished stage; retaining all releases")
        entries[path.name] = release_metadata(path, uid, gid, device)
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
    return entries, protected, pointers


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
    require(shutil.rmtree.avoids_symlink_attacks, "safe recursive removal is unavailable")
    entries, protected, pointers = snapshot(root, uid, gid)
    running = set()
    for identity in live_binaries:
        matches = []
        for name in entries:
            info = (root / "releases" / name / "bin/persea-terminal").stat()
            if identity == f"{info.st_dev}:{info.st_ino}":
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
    order += sorted(entries.keys() - known, key=lambda name: (entries[name].st_mtime_ns, name))
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
    require({name: witness(info) for name, info in fresh.items()} ==
            {name: witness(info) for name, info in entries.items()} and
            fresh_protected | running == protected and fresh_pointers == pointers,
            "release tree or pointers changed during retention")
    releases = root / "releases"
    fd = os.open(releases, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        for name in victims:
            require(witness(os.stat(name, dir_fd=fd, follow_symlinks=False)) == witness(entries[name]),
                    "release changed before removal")
            # Only validated directories gain owner write access. Files stay
            # read-only; no chmod follows links or touches hardlinked contents.
            for parent, directories, _ in os.walk(releases / name, followlinks=False):
                os.chmod(parent, 0o755, follow_symlinks=False)
            shutil.rmtree(name, dir_fd=fd)
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
