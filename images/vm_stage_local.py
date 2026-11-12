#!/usr/bin/env python3
"""Copy three pinned local files only; exporter assertions are NOT attested here."""
import argparse
import contextlib
import os
import sys

import vm_artifact as contract
from vm_publish_stage import MEMBERS, manifest_matches, selector

# Owner installs/configures this fixed location separately; no CLI/env source override.
SOURCE_ROOT = "/home/gha-runner-01/proxcloud-vm-artifacts/staging"
DIRECTORY = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW


def inputs(name, manifest_sha, disk_sha):
    contract.require(all(contract.match(r"[0-9a-f]{64}", pin) for pin in (manifest_sha, disk_sha)))
    return selector(name)  # (os, build_version, source_prefix, variant); os vs vm-k8s-export names.


@contextlib.contextmanager
def directory(path):
    contract.require(type(path) is str and path.startswith("/") and path != "/")
    contract.require(all(part not in ("", ".", "..") for part in path.split("/")[1:]))
    fd = os.open("/", DIRECTORY)
    try:
        for index, part in enumerate(path.split("/")[1:]):
            child = os.open(part, DIRECTORY, dir_fd=fd)
            os.close(fd)
            fd = child
            info = os.fstat(fd)
            # Root or the runner owns every ancestor. Only the conventional root-owned
            # sticky /tmp may be shared; all descendants and RUNNER_TEMP are private.
            shared_tmp = index == 0 and part == "tmp" and info.st_uid == 0 and bool(info.st_mode & 0o1000)
            contract.require(info.st_uid in (0, os.getuid()) and (not info.st_mode & 0o022 or shared_tmp))
        yield fd
    finally:
        os.close(fd)


def stamp(info):
    return (info.st_dev, info.st_ino, info.st_mode, info.st_uid, info.st_gid,
            info.st_nlink, info.st_size, info.st_mtime_ns, info.st_ctime_ns)


def private(fd):
    info = os.fstat(fd)
    contract.require(info.st_uid == os.getuid() and not info.st_mode & 0o022)
    return stamp(info)


def verify(fd, selected, manifest_sha, disk_sha):
    contract.require(set(os.listdir(fd)) == MEMBERS.keys())
    manifest = contract.verify(fd, manifest_sha, disk_sha)
    manifest_matches(manifest, *selected)
    contract.require(manifest["source_commit"][:12] == selected[2])
    return manifest


def stage(name, manifest_sha, disk_sha):
    selected = inputs(name, manifest_sha, disk_sha)  # Before any path or environment use.
    temporary = os.environ.get("RUNNER_TEMP", "")  # Trusted runner-provided value, not dispatch input.
    with directory(SOURCE_ROOT) as root, directory(temporary) as temp:
        root_before = private(root)
        private(temp)  # Its children are reopened by upload-artifact after this process exits.
        contract.require(os.path.commonpath((SOURCE_ROOT, temporary + "/proxcloud-vm-payload"))
                         not in (SOURCE_ROOT, temporary + "/proxcloud-vm-payload"))
        with contextlib.ExitStack() as stack:
            source = os.open(name, DIRECTORY, dir_fd=root)
            stack.callback(os.close, source)
            source_before = private(source)
            contract.require(os.fstat(temp).st_ino != os.fstat(source).st_ino or
                             os.fstat(temp).st_dev != os.fstat(source).st_dev)
            verify(source, selected, manifest_sha, disk_sha)
            total = 0
            files = []
            for member, limit in MEMBERS.items():
                raw = stack.enter_context(contract.local_file(source, member, limit))
                before = os.fstat(raw.fileno())
                contract.require(before.st_uid == os.getuid() and not before.st_mode & 0o022)
                total += before.st_size
                files.append((member, raw, stamp(before)))
            contract.require(total < contract.MAX_DISK_BYTES)
            os.mkdir("proxcloud-vm-payload", 0o700, dir_fd=temp)  # Exclusive; never overwrite.
            target = os.open("proxcloud-vm-payload", DIRECTORY, dir_fd=temp)
            stack.callback(os.close, target)
            for member, raw, before in files:
                flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW
                with os.fdopen(os.open(member, flags, 0o600, dir_fd=target), "wb") as out:
                    count = 0
                    for block in iter(lambda: raw.read(1024 ** 2), b""):
                        count += len(block)
                        contract.require(count <= before[6])
                        out.write(block)
                    contract.require(count == before[6] and stamp(os.fstat(raw.fileno())) == before)
            verify(target, selected, manifest_sha, disk_sha)  # Both pins and exact checksums again.
            contract.require(stamp(os.fstat(source)) == source_before and stamp(os.fstat(root)) == root_before)


class Parser(argparse.ArgumentParser):
    def error(self, message):
        raise ValueError("invalid stage arguments")


def main(argv=None):
    parser = Parser(add_help=False)
    for argument in ("artifact-name", "expectedmanifestsha", "expecteddisksha"):
        parser.add_argument("--" + argument, required=True)
    try:
        args = parser.parse_args(argv)
        stage(args.artifact_name, args.expectedmanifestsha, args.expecteddisksha)
        print("VM artifact staged: three pinned files; exporter facts not attested")
        return 0
    except (OSError, ValueError, TypeError, KeyError, RecursionError):
        print("VM artifact staging refused: inputs or pinned local payload invalid", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
