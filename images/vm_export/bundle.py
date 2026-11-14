"""Stage the sealed export as a vm_artifact bundle (request + CleanExportReceipt + disk, then prepare).

    cd images && python3 -m vm_export.bundle --staging-dir DIR --disk disk.qcow2 --sanitize-facts sanitize-facts.json \
        --build-run-id N --export-run-id M --source-raw-sha256 H --source-commit SHA40 \
        --upstream-checksum HEX --build-version YYYYMMDDHHMMSS --os ubuntu-2604 [--kubernetes-version V] [--upstream-url URL]

source_identity = "sha256:" + sha256("proxcloud-template-build:run=<build run id>:raw_sha256=<source sha>"):
a run-scoped, non-secret identifier (never a bare VMID). run_ref is the public export workflow run.
"""
import argparse
import hashlib
import json
import os
import shutil
import sys

import vm_artifact

RUN_REF = "https://github.com/mutex-solutions/proxcloud/actions/runs/%d"


def source_identity(build_run_id, raw_sha256):
    vm_artifact.require(type(build_run_id) is int and build_run_id > 0 and vm_artifact.match(r"[0-9a-f]{64}", raw_sha256))
    preimage = "proxcloud-template-build:run=%d:raw_sha256=%s" % (build_run_id, raw_sha256)
    return "sha256:" + hashlib.sha256(preimage.encode("ascii")).hexdigest()


def request(os_name, source_commit, upstream_checksum, build_version, kubernetes_version=None, upstream_url=None):
    q = {"kind": "vm-template", "os": os_name, "arch": "amd64",
         "variant": "kubernetes" if kubernetes_version else "os", "source_commit": source_commit,
         "upstream_checksum": upstream_checksum, "build_version": build_version}
    if kubernetes_version:
        q["kubernetes_version"] = kubernetes_version
    if upstream_url:
        q["provenance"] = {"upstream_url": upstream_url}
    vm_artifact.request_valid(q)
    return q


def receipt(q, facts, build_run_id, export_run_id, raw_sha256):
    """CleanExportReceipt bound to the request and the sanitizer facts of THIS sealed disk."""
    vm_artifact.require(facts["kind"] == "OfflineSanitizationFacts" and facts["os"] == q["os"]
                        and facts["variant"] == q["variant"] and facts.get("kubernetes_version") == q.get("kubernetes_version"))
    vm_artifact.require(type(export_run_id) is int and export_run_id > 0)
    return {"kind": "CleanExportReceipt", "source_identity": source_identity(build_run_id, raw_sha256),
            "source_commit": q["source_commit"], "run_ref": RUN_REF % export_run_id,
            "request_sha256": vm_artifact.sha(vm_artifact.encode(q)), "disk_sha256": facts["disk_sha256"],
            "disk_bytes": facts["disk_bytes"], "virtual_size_bytes": facts["virtual_size_bytes"],
            "checks": facts["checks"]}


def stage(staging_dir, q, r, disk):
    """Write request.json, clean-export-receipt.json and a single-link copy of the disk, then prepare."""
    fd = os.open(staging_dir, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        for name, data in (("request.json", vm_artifact.encode(q)), ("clean-export-receipt.json", vm_artifact.encode(r))):
            with os.fdopen(os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=fd), "wb") as f:
                f.write(data)
        with open(disk, "rb") as src, os.fdopen(os.open("disk.qcow2", os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                                                        0o600, dir_fd=fd), "wb") as dst:
            shutil.copyfileobj(src, dst, 1 << 20)
        return vm_artifact.prepare(fd)  # Re-hashes the disk; any mismatch with the receipt refuses.
    finally:
        os.close(fd)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    for name in ("--staging-dir", "--disk", "--sanitize-facts", "--source-raw-sha256", "--source-commit",
                 "--upstream-checksum", "--build-version", "--os"):
        parser.add_argument(name, required=True)
    parser.add_argument("--build-run-id", required=True, type=int)
    parser.add_argument("--export-run-id", required=True, type=int)
    parser.add_argument("--kubernetes-version")
    parser.add_argument("--upstream-url")
    a = parser.parse_args(argv)
    try:
        q = request(a.os, a.source_commit, a.upstream_checksum, a.build_version, a.kubernetes_version, a.upstream_url)
        with open(a.sanitize_facts, encoding="ascii") as f:
            facts = json.load(f)
        m = stage(a.staging_dir, q, receipt(q, facts, a.build_run_id, a.export_run_id, a.source_raw_sha256), a.disk)
        print(json.dumps(vm_artifact.plan(m), sort_keys=True))
        return 0
    except (OSError, ValueError, TypeError, KeyError):
        print("bundle refused: invalid request, facts, receipt or staging files", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
