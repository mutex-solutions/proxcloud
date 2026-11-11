#!/usr/bin/env python3
"""Actions staging authorization and bounded ZIP extraction; no network/subprocess/image parser.

The hosted workflow supplies API responses from fixed same-repository endpoints. This
module treats those responses and the archive as data, never as executable code.
"""
import argparse
import datetime
import os
import re
import stat
import struct
import sys
import zipfile

import vm_artifact as contract

REPOSITORY = "mutex-solutions/proxcloud"
STAGING_WORKFLOW = ".github/workflows/vm-artifact-stage.yml"
MEMBERS = {"disk.qcow2": contract.MAX_DISK_BYTES - 1, "manifest.json": 65536, "SHA256SUMS": 65536}
MAX_PAYLOAD_BYTES = sum(MEMBERS.values())
MAX_ARCHIVE_BYTES = MAX_PAYLOAD_BYTES + 1024 ** 2
MAX_METADATA_BYTES = 1024 ** 2
NAME = r"vm-export-(ubuntu-2404|ubuntu-2604|debian-12|debian-13)-([0-9]{14})-([0-9a-f]{12})"


def require(ok):
    if not ok:
        raise ValueError("staging publication refused")


def positive(value):
    return type(value) is int and 0 < value <= 10 ** 20 - 1


def inputs(run_id, artifact_name, manifest_sha, disk_sha, publisher_sha, repository):
    require(contract.match(r"[1-9][0-9]{0,19}", run_id) and repository == REPOSITORY)
    require(all(contract.match(r"[0-9a-f]{64}", value) for value in (manifest_sha, disk_sha)))
    require(contract.match(r"[0-9a-f]{40}", publisher_sha))
    require(type(artifact_name) is str)
    found = re.fullmatch(NAME, artifact_name, flags=re.ASCII)
    require(found is not None)
    datetime.datetime.strptime(found[2], "%Y%m%d%H%M%S")
    return dict(run_id=int(run_id), artifact_name=artifact_name, manifest_sha=manifest_sha,
                disk_sha=disk_sha, publisher_sha=publisher_sha, os=found[1],
                build_version=found[2], source_prefix=found[3])


def authorize(workflow, run, listing, selected):
    """Bind exactly one unexpired artifact to the approved successful producer run."""
    require(type(workflow) is dict and positive(workflow.get("id")))
    require(workflow.get("path") == STAGING_WORKFLOW and workflow.get("state") == "active")
    require(type(run) is dict and run.get("id") == selected["run_id"])
    require(run.get("workflow_id") == workflow["id"] and run.get("path") == STAGING_WORKFLOW)
    require(run.get("event") == "workflow_dispatch" and run.get("status") == "completed")
    require(run.get("conclusion") == "success" and run.get("head_branch") == "main")
    require(run.get("head_sha") == selected["publisher_sha"])
    repo, head = run.get("repository"), run.get("head_repository")
    require(type(repo) is dict and type(head) is dict and positive(repo.get("id")))
    require(repo.get("full_name") == head.get("full_name") == REPOSITORY and repo["id"] == head.get("id"))
    require(type(listing) is dict and type(listing.get("total_count")) is int and listing["total_count"] == 1)
    rows = listing.get("artifacts")
    require(type(rows) is list and len(rows) == 1 and type(rows[0]) is dict)
    artifact = rows[0]
    require(positive(artifact.get("id")) and artifact.get("name") == selected["artifact_name"])
    require(artifact.get("expired") is False)
    require(type(artifact.get("size_in_bytes")) is int and 0 < artifact["size_in_bytes"] <= MAX_ARCHIVE_BYTES)
    artifact_id = artifact["id"]
    require(artifact.get("archive_download_url") ==
            "https://api.github.com/repos/%s/actions/artifacts/%s/zip" % (REPOSITORY, artifact_id))
    producer = artifact.get("workflow_run")
    require(type(producer) is dict and producer.get("id") == selected["run_id"])
    require(producer.get("repository_id") == producer.get("head_repository_id") == repo["id"])
    require(producer.get("head_branch") == "main" and producer.get("head_sha") == selected["publisher_sha"])
    return artifact_id


def zip_members(archive):
    """Validate every member before creating a destination or writing payload bytes."""
    rows = archive.infolist()
    require(len(rows) == len(MEMBERS) and {row.filename for row in rows} == MEMBERS.keys())
    require(not archive.comment)
    for row in rows:
        require(row.orig_filename == row.filename and row.filename in MEMBERS)
        require(not row.is_dir() and not (row.flag_bits & 1))
        mode = row.external_attr >> 16
        require(stat.S_IFMT(mode) in (0, stat.S_IFREG) and not (row.external_attr & 0x10))
        require(row.compress_type in (zipfile.ZIP_STORED, zipfile.ZIP_DEFLATED))
        require(0 < row.file_size <= MEMBERS[row.filename] and 0 < row.compress_size <= MAX_ARCHIVE_BYTES)
        # The producer should upload with compression-level: 0. Do not admit a high-ratio bomb.
        require(row.file_size <= row.compress_size * 100)
    require(sum(row.file_size for row in rows) <= MAX_PAYLOAD_BYTES)
    return rows


def zip_directory_bound(raw, size):
    """Bound central-directory allocation before stdlib ZipFile reads it (ZIP64 too)."""
    require(size >= 22)
    raw.seek(size - 22)
    end = struct.unpack("<4s4H2IH", raw.read(22))
    require(end[0] == b"PK\x05\x06" and end[1:3] == (0, 0) and end[7] == 0)
    count_disk, count, directory_bytes, offset = end[3:7]
    boundary = size - 22
    # ZIP64 may appear even if only the offset, rather than the entry count, needs it.
    if size >= 42:
        raw.seek(size - 42)
        locator = raw.read(20)
        if locator[:4] == b"PK\x06\x07":
            signature, disk, position, disks = struct.unpack("<4sIQI", locator)
            require(disk == 0 and disks == 1 and 0 <= position <= size - 98)
            raw.seek(position)
            record = struct.unpack("<4sQ2H2I4Q", raw.read(56))
            require(record[0] == b"PK\x06\x06" and record[1] == 44 and record[4:6] == (0, 0))
            require(position + 56 == size - 42)
            count_disk, count, directory_bytes, offset = record[6:10]
            boundary = position
    require(count_disk == count == len(MEMBERS) and 0 < directory_bytes <= 65536)
    require(0 <= offset and offset + directory_bytes == boundary)
    raw.seek(0)


def extract(archive_path, staging_dir):
    """Use fixed basenames and exclusive writes, never ZipFile.extract/extractall."""
    require(os.path.isabs(archive_path) and os.path.isabs(staging_dir))
    archive_fd = os.open(archive_path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(archive_fd, "rb") as raw:
        before = os.fstat(raw.fileno())
        require(stat.S_ISREG(before.st_mode) and before.st_nlink == 1 and 0 < before.st_size <= MAX_ARCHIVE_BYTES)
        zip_directory_bound(raw, before.st_size)
        with zipfile.ZipFile(raw) as archive:
            rows = zip_members(archive)
            os.mkdir(staging_dir, 0o700)  # A pre-existing destination, including a symlink, is refused.
            directory = os.open(staging_dir, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
            try:
                for row in rows:
                    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW
                    out_fd = os.open(row.filename, flags, 0o600, dir_fd=directory)
                    with os.fdopen(out_fd, "wb") as out, archive.open(row) as source:
                        count = 0
                        while True:
                            block = source.read(min(1024 ** 2, row.file_size - count + 1))
                            if not block:
                                break
                            count += len(block)
                            require(count <= row.file_size and count <= MEMBERS[row.filename])
                            out.write(block)
                        require(count == row.file_size)  # ZipFile also checks CRC at EOF.
            finally:
                os.close(directory)
        after = os.fstat(raw.fileno())
        require((before.st_size, before.st_mtime_ns, before.st_ctime_ns) ==
                (after.st_size, after.st_mtime_ns, after.st_ctime_ns))


def publication_plan(staging_dir, selected):
    require(os.path.isabs(staging_dir))
    fd = os.open(staging_dir, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        require(set(os.listdir(fd)) == MEMBERS.keys())
        manifest = contract.verify(fd, selected["manifest_sha"], selected["disk_sha"])
    finally:
        os.close(fd)
    # The initial publication bridge supports native OS disks only, not Kubernetes exports.
    require(manifest["variant"] == "os" and manifest["os"] == selected["os"])
    require(manifest["build_version"] == selected["build_version"])
    require(manifest["source_commit"].startswith(selected["source_prefix"]))
    plan = contract.plan(manifest)  # Keep this local helper's acceptance gates false.
    plan.update(package=plan["reference"].split(":", 1)[0], source_commit=manifest["source_commit"],
                expected_manifest_sha256=selected["manifest_sha"], expected_disk_sha256=selected["disk_sha"])
    plan["argv"][2:2] = ["--annotation", "org.opencontainers.image.source=https://github.com/" + REPOSITORY,
                           "--annotation", "org.opencontainers.image.revision=" + manifest["source_commit"]]
    return plan


def absent_tag(status, body):
    """Only an explicit registry unknown-manifest/name 404 authorizes a first push."""
    require(status == 404)
    value = contract.decode(body)
    require(type(value) is dict and type(value.get("errors")) is list and value["errors"])
    require(all(type(error) is dict and error.get("code") in ("MANIFEST_UNKNOWN", "NAME_UNKNOWN")
                for error in value["errors"]))


def metadata_file(directory, name):
    fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        with contract.local_file(fd, name, MAX_METADATA_BYTES) as source:
            return contract.decode(source.read(MAX_METADATA_BYTES + 1))
    finally:
        os.close(fd)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("check-inputs", "authorize", "unpack"))
    parser.add_argument("--metadata-dir")
    parser.add_argument("--archive")
    parser.add_argument("--staging-dir")
    args = parser.parse_args(argv)
    try:
        selected = inputs(*(os.environ.get(key, "") for key in
                            ("STAGING_RUN_ID", "ARTIFACT_NAME", "EXPECTED_MANIFEST_SHA256",
                             "EXPECTED_DISK_SHA256", "PUBLISHER_SHA", "PUBLISHER_REPOSITORY")))
        if args.command == "authorize":
            require(args.metadata_dir is not None and os.path.isabs(args.metadata_dir))
            artifact_id = authorize(*(metadata_file(args.metadata_dir, name) for name in
                                      ("workflow.json", "run.json", "artifacts.json")), selected)
            print("artifact_id=%s\nmax_archive_bytes=%s" % (artifact_id, MAX_ARCHIVE_BYTES))
        elif args.command == "unpack":
            require(args.archive is not None and args.staging_dir is not None)
            extract(args.archive, args.staging_dir)
            print(contract.encode(publication_plan(args.staging_dir, selected)).decode(), end="")
        return 0
    except (OSError, ValueError, TypeError, KeyError, RecursionError, EOFError, RuntimeError, struct.error, zipfile.BadZipFile):
        print("staging publication refused: inputs, authority, archive or pinned bundle invalid", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
