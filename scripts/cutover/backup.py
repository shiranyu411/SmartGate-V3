#!/usr/bin/env python3
"""Validate private device backups without extracting or printing their contents."""

import argparse
import hashlib
import io
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import sys
import tarfile
from datetime import datetime, timezone


def digest(stream):
    result = hashlib.sha256()
    for block in iter(lambda: stream.read(1024 * 1024), b""):
        result.update(block)
    return result.hexdigest()


def file_digest(path):
    with path.open("rb") as stream:
        return digest(stream)


def private_directory(path, forbidden=(), create=False):
    directory = Path(path).expanduser().resolve()
    for root in forbidden:
        root = Path(root).resolve()
        if directory == root or root in directory.parents:
            raise ValueError("backup must be outside the workspace and ops directories")
    if create:
        directory.mkdir(mode=0o700, parents=True, exist_ok=False)
    if not directory.is_dir() or directory.stat().st_mode & 0o077:
        raise ValueError("backup directory must exist with mode 0700")
    if directory.stat().st_uid != os.getuid():
        raise ValueError("backup directory must belong to the current user")
    return directory


def member_path(name):
    path = PurePosixPath(name)
    if path.is_absolute() or ".." in path.parts:
        raise ValueError("unsafe archive member path")
    return str(path)


def archive_manifest(path):
    entries = {}
    with tarfile.open(path, "r:gz") as archive:
        for member in archive:
            name = member_path(member.name)
            if name in entries:
                raise ValueError("duplicate archive member")
            entry = {
                "size": member.size,
                "mode": member.mode,
                "uid": member.uid,
                "gid": member.gid,
                "type": member.type.decode("ascii"),
            }
            if member.isfile() or member.islnk():
                with archive.extractfile(member) as stream:
                    entry["sha256"] = digest(stream)
            if member.issym() or member.islnk():
                entry["linkname"] = member.linkname
            entries[name] = entry
    return entries


def source_hashes(path):
    entries = {}
    for line in path.read_text().splitlines():
        sha, name = line.split("  ", 1)
        if len(sha) != 64 or any(c not in "0123456789abcdef" for c in sha):
            raise ValueError("invalid source checksum")
        entries[member_path(name.lstrip("/"))] = sha
    return entries


def read_json(archive, name):
    with archive.extractfile(name) as stream:
        return json.load(stream)


def write_json(path, value):
    with path.open("x", encoding="utf-8") as stream:
        json.dump(value, stream, ensure_ascii=True, indent=2, sort_keys=True)
        stream.write("\n")


def finalize(directory, target, host, commit, legacy_directory=None):
    before = source_hashes(directory / "v2-before.sha256")
    after = source_hashes(directory / "v2-after.sha256")
    if not before or before != after:
        raise ValueError("source changed during backup; retain this attempt and take a new snapshot")
    files = archive_manifest(directory / "v2-files.tar.gz")
    required = (
        "usr/bin/smartgate", "etc/init.d/smartgate", "www/smartgate/index.html",
        "etc/smartgate/version.json", "etc/smartgate/spec.json",
        "etc/smartgate/applied-state.json", "etc/smartgate/auth.json",
    )
    for name in required:
        if name not in before:
            raise ValueError("required SmartGate recovery file is missing")
    for name, sha in before.items():
        if files.get(name, {}).get("sha256") != sha:
            raise ValueError("archive content does not match the stable source checksums")
    with tarfile.open(directory / "v2-files.tar.gz", "r:gz") as archive:
        version = read_json(archive, "etc/smartgate/version.json")
        applied = read_json(archive, "etc/smartgate/applied-state.json")
        spec = read_json(archive, "etc/smartgate/spec.json")
    if version.get("commit") != commit:
        raise ValueError("backup commit does not match the expected baseline")
    schema = applied.get("schema_version", 1)
    if schema not in (1, 2):
        raise ValueError("unsupported applied baseline schema")
    services = dict(
        line.split("=", 1)
        for line in (directory / "services.txt").read_text().splitlines()
        if "=" in line
    )
    if any(services.get(name) not in ("yes", "no") for name in ("smartgate", "legacy")):
        raise ValueError("missing service enable-state metadata")
    metadata = {
        "old-schema": str(schema),
        "old-mode": applied.get("gateway_mode", "off"),
        "smartgate-enabled": services["smartgate"],
        "legacy-enabled": services["legacy"],
    }
    legacy_path = member_path(legacy_directory.lstrip("/")) if legacy_directory and schema == 1 else None
    if legacy_path:
        if legacy_path == "." or not any(name.startswith(legacy_path + "/") for name in files):
            raise ValueError("complete legacy directory is missing from the source archive")
        metadata["legacy-directory-complete"] = legacy_path
    recovery_paths = [
        member_path(line) for line in (directory / "recovery-paths.txt").read_text().splitlines()
        if line
    ]
    def selected(name):
        name = member_path(name)
        return any(name == root or name.startswith(root + "/") for root in recovery_paths)

    if not all(selected(name) for name in required):
        raise ValueError("recovery archive would omit required SmartGate files")
    if legacy_path and not selected(legacy_path):
        raise ValueError("complete legacy directory is not in the recovery archive")
    with tarfile.open(directory / "v2-files.tar.gz", "r:gz") as source:
        with tarfile.open(directory / "v2-rollback.tar.gz", "w:gz") as dest:
            for member in source:
                if not selected(member.name):
                    continue
                stream = source.extractfile(member) if member.isfile() else None
                try:
                    dest.addfile(member, stream)
                finally:
                    if stream is not None:
                        stream.close()
            for name, value in metadata.items():
                data = (value + "\n").encode()
                member = tarfile.TarInfo(name)
                member.size, member.mode = len(data), 0o600
                dest.addfile(member, io.BytesIO(data))
    root = archive_manifest(directory / "persistent-root.tar.gz")
    rollback = archive_manifest(directory / "v2-rollback.tar.gz")
    for name, sha in before.items():
        if selected(name) and rollback.get(name, {}).get("sha256") != sha:
            raise ValueError("repacked rollback archive failed content verification")
    write_json(directory / "persistent-root.manifest.json", root)
    write_json(directory / "v2-files.manifest.json", files)
    marker = {
        "format": 1,
        "created_at": datetime.now(timezone.utc).isoformat(),
        "target": target,
        "host": host,
        "commit": commit,
        "version": version,
        "applied_schema": schema,
        "gateway_mode": applied.get("gateway_mode"),
        "spec_sha256": before["etc/smartgate/spec.json"],
        "persistent_entries": len(root),
        "verified_v2_files": len(before),
        "providers": len(spec.get("providers", [])),
        "archives": {
            name: file_digest(directory / name)
            for name in ("persistent-root.tar.gz", "v2-files.tar.gz", "v2-rollback.tar.gz")
        },
        "scope": "persistent merged filesystem plus SmartGate files; not a raw flash image or live sessions",
    }
    write_json(directory / "BACKUP_VERIFIED.json", marker)
    print(f"BACKUP: PASS commit={commit} verified_v2_files={len(before)} persistent_entries={len(root)}")
    print(f"private backup: {directory}")


def check(directory, target, host, commit, spec_sha):
    marker = json.loads((directory / "BACKUP_VERIFIED.json").read_text())
    if marker.get("format") != 1:
        raise ValueError("unsupported backup format")
    if marker.get("rehearsal_only") and target != "devvm":
        raise ValueError("a rehearsal fixture cannot be restored to production")
    if any(marker.get(key) != value for key, value in (
        ("target", target), ("host", host), ("commit", commit),
    )):
        raise ValueError("backup does not belong to this target and installed baseline")
    if spec_sha and marker.get("spec_sha256") != spec_sha:
        raise ValueError("saved configuration changed since backup; take a new snapshot")
    for name in ("persistent-root.tar.gz", "v2-files.tar.gz", "v2-rollback.tar.gz"):
        path = directory / name
        if path.is_symlink() or path.stat().st_mode & 0o077:
            raise ValueError("backup archive must be a private regular file")
        if file_digest(path) != marker.get("archives", {}).get(name):
            raise ValueError("backup archive checksum mismatch")
    print(f"BACKUP: VERIFIED target={target} commit={commit}")


def reclaim_plan(directory, listing, deploy_root, keep=3):
    manifest = json.loads((directory / "persistent-root.manifest.json").read_text())
    candidates = []
    for line in listing.splitlines():
        sha, name = line.split("  ", 1)
        path = PurePosixPath(name)
        if str(path.parent) != deploy_root:
            raise ValueError("unexpected deployment backup directory")
        if not re.fullmatch(r"before-[0-9]{8}T[0-9]{6}Z\.tar\.gz", path.name):
            raise ValueError("unexpected deployment backup filename")
        candidates.append((path.name, sha))
    result = []
    for name, sha in sorted(candidates)[:-keep]:
        archived = manifest.get(str(PurePosixPath(deploy_root.lstrip("/")) / name), {})
        if archived.get("sha256") != sha:
            raise ValueError("retained device backup differs from the verified off-device copy")
        result.append(f"{sha}  {name}")
    return "\n".join(result) + ("\n" if result else "")


def restore_hashes(directory):
    entries = archive_manifest(directory / "v2-rollback.tar.gz")
    return "".join(
        f'{entry["sha256"]}  {name}\n'
        for name, entry in sorted(entries.items())
        if "sha256" in entry and name.startswith(("etc/", "usr/", "www/", "var/"))
    )


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("init", "finalize", "check", "reclaim-plan", "restore-hashes"))
    parser.add_argument("directory")
    parser.add_argument("--forbid", action="append", default=[])
    parser.add_argument("--target")
    parser.add_argument("--host")
    parser.add_argument("--commit")
    parser.add_argument("--spec-sha", default="")
    parser.add_argument("--deploy-root", default="/root/smartgate-deployments")
    parser.add_argument("--legacy-directory")
    args = parser.parse_args()
    os.umask(0o077)
    directory = private_directory(args.directory, args.forbid, args.action == "init")
    if args.action == "init":
        if shutil.disk_usage(directory).free < 1024 * 1024 * 1024:
            raise ValueError("less than 1 GiB free for private backups")
    elif args.action == "finalize":
        finalize(directory, args.target, args.host, args.commit, args.legacy_directory)
    elif args.action == "reclaim-plan":
        sys.stdout.write(reclaim_plan(directory, sys.stdin.read(), args.deploy_root))
    elif args.action == "restore-hashes":
        sys.stdout.write(restore_hashes(directory))
    else:
        check(directory, args.target, args.host, args.commit, args.spec_sha)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, tarfile.TarError, KeyError, json.JSONDecodeError) as error:
        # Do not include exceptions that might contain configuration values.
        print(f"backup: refused ({type(error).__name__}); check snapshot integrity and private paths", file=sys.stderr)
        sys.exit(1)
