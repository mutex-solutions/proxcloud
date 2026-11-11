#!/usr/bin/env python3
"""Pure local VM-artifact contract; no image parser, subprocess, network or attestation."""
import argparse
import contextlib
import datetime
import hashlib
import json
import os
import re
import stat
import sys

ARTIFACT_TYPE = "application/vnd.proxcloud.vm-template.v1+json"
MAX_DISK_BYTES = 8 * 1024 ** 3  # Strictly below this, not GHCR's 10 GB ceiling.
META = {"kind", "os", "arch", "variant", "source_commit", "upstream_checksum", "build_version"}
OPTIONAL = {"kubernetes_version", "provenance"}
CHECKS = {"cleaned_copy_only", "no_backing_file", "qemu_img_check", "offline_sanitization",
          "zero_free_space", "no_build_credentials_or_lab_network"}
RECEIPT = {"kind", "source_identity", "source_commit", "run_ref", "request_sha256",
           "disk_sha256", "disk_bytes", "virtual_size_bytes", "checks"}


def require(ok):
    if not ok:
        raise ValueError("invalid artifact contract")


def keys(obj, required, optional=frozenset()):
    require(type(obj) is dict and required <= obj.keys() <= required | optional)


def match(pattern, value):
    return type(value) is str and re.fullmatch(pattern, value, flags=re.ASCII) is not None


def sha(data):
    return hashlib.sha256(data).hexdigest()


def encode(obj):
    return (json.dumps(obj, sort_keys=True, indent=2, ensure_ascii=True) + "\n").encode()


def unique(pairs):
    obj = dict(pairs)
    require(len(obj) == len(pairs))
    return obj


def decode(raw):
    return json.loads(raw.decode("utf-8"), object_pairs_hook=unique)


@contextlib.contextmanager
def local_file(fd, name, limit):
    # Operator selects a trusted directory; every member name is a source constant.
    flags = os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK
    with os.fdopen(os.open(name, flags, dir_fd=fd), "rb") as f:
        before = os.fstat(f.fileno())
        require(stat.S_ISREG(before.st_mode) and before.st_nlink == 1 and 0 < before.st_size <= limit)
        yield f
        after = os.fstat(f.fileno())
        require((before.st_size, before.st_mtime_ns, before.st_ctime_ns) ==
                (after.st_size, after.st_mtime_ns, after.st_ctime_ns))


def read_small(fd, name):
    with local_file(fd, name, 65536) as f:
        raw = f.read(65537)
        require(len(raw) <= 65536)
        return raw


def disk_hash(fd):
    digest, size = hashlib.sha256(), 0
    with local_file(fd, "disk.qcow2", MAX_DISK_BYTES - 1) as f:
        for chunk in iter(lambda: f.read(1024 ** 2), b""):
            size += len(chunk)
            require(size < MAX_DISK_BYTES)
            digest.update(chunk)
        require(size == os.fstat(f.fileno()).st_size)
    return digest.hexdigest(), size


def request_valid(q):
    keys(q, META, OPTIONAL)
    require(q["kind"] == "vm-template" and q["arch"] == "amd64")
    require(q["os"] in ("ubuntu-2404", "ubuntu-2604", "debian-12", "debian-13"))
    require(q["variant"] in ("os", "kubernetes") and match(r"[0-9a-f]{40}", q["source_commit"]))
    require(match(r"(?:[0-9a-f]{64}|[0-9a-f]{128})", q["upstream_checksum"]))
    require(match(r"[0-9]{14}", q["build_version"]))
    datetime.datetime.strptime(q["build_version"], "%Y%m%d%H%M%S")
    if q["variant"] == "kubernetes":
        require(q["os"].startswith("ubuntu-") and match(r"[1-9][0-9]{0,2}\.(?:0|[1-9][0-9]{0,2})\.(?:0|[1-9][0-9]{0,2})", q.get("kubernetes_version")))
    else:
        require("kubernetes_version" not in q)
    if "provenance" in q:
        keys(q["provenance"], {"upstream_url"})
        host = r"(?:cloud-images\.ubuntu\.com|releases\.ubuntu\.com)" if q["os"].startswith("ubuntu-") else r"cloud\.debian\.org"
        url = q["provenance"]["upstream_url"]
        require(match(r"https://" + host + r"/[A-Za-z0-9._/-]+", url) and ".." not in url and len(url) <= 2048)


def image_tag(q):
    variant = "os" if q["variant"] == "os" else "k8s-" + q["kubernetes_version"]
    return "%s-%s-%s-%s" % (q["os"], variant, q["build_version"], q["source_commit"][:12])


def validate(m):
    keys(m, META | {"schema_version", "image_tag", "disk", "clean_export_receipt"}, OPTIONAL)
    q = {k: v for k, v in m.items() if k in META | OPTIONAL}
    request_valid(q)
    require(type(m["schema_version"]) is int and m["schema_version"] == 1 and m["image_tag"] == image_tag(q))
    d, r = m["disk"], m["clean_export_receipt"]
    keys(d, {"filename", "format", "sha256", "bytes", "virtual_size_bytes"})
    keys(r, RECEIPT)
    require(d["filename"] == "disk.qcow2" and d["format"] == "qcow2" and match(r"[0-9a-f]{64}", d["sha256"]))
    require(type(d["bytes"]) is int and 0 < d["bytes"] < MAX_DISK_BYTES)
    require(type(d["virtual_size_bytes"]) is int and 20 * 1024 ** 3 <= d["virtual_size_bytes"] <= 100 * 1024 ** 3)
    require(r["kind"] == "CleanExportReceipt" and match(r"sha256:[0-9a-f]{64}", r["source_identity"]))
    require(match(r"https://github\.com/mutex-solutions/proxcloud/actions/runs/[1-9][0-9]{0,19}", r["run_ref"]))
    require(r["source_commit"] == q["source_commit"] and r["request_sha256"] == sha(encode(q)))
    require(type(r["disk_bytes"]) is int and type(r["virtual_size_bytes"]) is int)
    require((r["disk_sha256"], r["disk_bytes"], r["virtual_size_bytes"]) == (d["sha256"], d["bytes"], d["virtual_size_bytes"]))
    keys(r["checks"], CHECKS)
    require(all(value is True for value in r["checks"].values()))


def sums(m, raw):
    return (m["disk"]["sha256"] + "  disk.qcow2\n" + sha(raw) + "  manifest.json\n").encode()


def prepare(fd):
    q = decode(read_small(fd, "request.json"))
    request_valid(q)
    r = decode(read_small(fd, "clean-export-receipt.json"))
    keys(r, RECEIPT)
    digest, size = disk_hash(fd)
    m = dict(q, schema_version=1, image_tag=image_tag(q), clean_export_receipt=r,
             disk=dict(filename="disk.qcow2", format="qcow2", sha256=digest, bytes=size,
                       virtual_size_bytes=r["virtual_size_bytes"]))
    validate(m)  # Missing, false or mismatched exporter facts refuse output.
    raw = encode(m)
    for name, data in (("manifest.json", raw), ("SHA256SUMS", sums(m, raw))):
        flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW
        with os.fdopen(os.open(name, flags, 0o600, dir_fd=fd), "wb") as f:
            f.write(data)
    return m


def verify(fd, expected_manifest=None, expected_disk=None):
    require(all(value is None or match(r"[0-9a-f]{64}", value) for value in (expected_manifest, expected_disk)))
    raw = read_small(fd, "manifest.json")
    m = decode(raw)
    validate(m)
    digest, size = disk_hash(fd)
    require((digest, size) == (m["disk"]["sha256"], m["disk"]["bytes"]))
    require(read_small(fd, "SHA256SUMS") == sums(m, raw))
    require(expected_manifest is None or expected_manifest == sha(raw))
    require(expected_disk is None or expected_disk == digest)
    return m


def plan(m):
    package = "ghcr.io/mutex-solutions/proxcloud-vm-" + m["os"] + ("-k8s" if m["variant"] == "kubernetes" else "")
    return {"execute": False, "publish_eligible": False, "reference": package + ":" + m["image_tag"],
            "release_gates": {"trusted_export_receipt_review": False, "roundtrip_boot_accepted": False,
                              "explicit_public_setting_and_anonymous_pull": False},
            "argv": ["oras", "push", "--image-spec", "v1.0", "--artifact-type", ARTIFACT_TYPE,
                     package + ":" + m["image_tag"], "disk.qcow2:application/x-qemu-disk",
                     "manifest.json:application/vnd.proxcloud.vm-template.manifest.v1+json", "SHA256SUMS:text/plain"]}


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("prepare", "verify"))
    parser.add_argument("--staging-dir", required=True, help="absolute operator-controlled existing directory")
    parser.add_argument("--expected-manifest-sha256")
    parser.add_argument("--expected-disk-sha256")
    a = parser.parse_args(argv)
    try:
        require(os.path.isabs(a.staging_dir))
        require(a.command == "verify" or not (a.expected_manifest_sha256 or a.expected_disk_sha256))
        fd = os.open(a.staging_dir, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        try:
            m = prepare(fd) if a.command == "prepare" else verify(fd, a.expected_manifest_sha256, a.expected_disk_sha256)
        finally:
            os.close(fd)
        print(json.dumps(plan(m), sort_keys=True))  # A plan only; never runs ORAS.
        return 0
    except (OSError, ValueError, TypeError, KeyError, RecursionError):
        print("artifact refused: invalid schema, receipt, hashes or staging files", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
