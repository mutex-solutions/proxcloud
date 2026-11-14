"""Read-only private copy of a stopped template's disk from the read-only NFS mount of its storage.

    cd images && PVE_URL=... PVE_TOKEN_ID=... PVE_TOKEN_SECRET=... python3 -m vm_export.copy \
        --node NODE --vmid 105 --name NAME --tags 'a;b' --mount /srv/pc-ci-images \
        --expected-bytes 21474836480 --out DIR/source.raw --receipt DIR/copy-receipt.json

Never parses the disk and never runs guest code. The PVE API template guard runs before and after the
copy and must return the same snapshot (stopped unlocked template, exact name/tags/pool, config digest
and root disk volume); the copied file is derived from that volume, never given by the caller. File
guards: S_ISREG, nlink 1, exact size, identity (dev/ino/size/mtime/ctime) unchanged before == after,
streamed sha256 of the copy equal to a second full re-read of the source, sparse local write, mode 0400.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import sys
import types

BLOCK = 1 << 20
ZERO = bytes(BLOCK)


class CopyRefused(ValueError):
    pass


def need(ok, reason):
    if not ok:
        raise CopyRefused(reason)


def identity(st):
    return {"dev": st.st_dev, "ino": st.st_ino, "size": st.st_size, "mtime_ns": st.st_mtime_ns, "ctime_ns": st.st_ctime_ns}


def source_identity(path, expected_bytes):
    st = os.lstat(path)
    need(stat.S_ISREG(st.st_mode) and st.st_nlink == 1, "source must be a regular single-link file")
    need(st.st_size == expected_bytes, "source size")
    return identity(st)


def stream_hash(path, expected_bytes, out_fd=None):
    """sha256 of exactly expected_bytes read through an O_NOFOLLOW fd; optionally a sparse copy to out_fd."""
    digest, total, written = hashlib.sha256(), 0, 0
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as f:
        need(identity(os.fstat(f.fileno())) == source_identity(path, expected_bytes), "source swapped")
        for chunk in iter(lambda: f.read(BLOCK), b""):
            total += len(chunk)
            need(total <= expected_bytes, "source grew")
            digest.update(chunk)
            if out_fd is not None:
                if chunk == ZERO[:len(chunk)]:
                    os.lseek(out_fd, len(chunk), os.SEEK_CUR)  # Keep holes sparse.
                else:
                    os.write(out_fd, chunk)
                    written += len(chunk)
    need(total == expected_bytes, "short read")
    return digest.hexdigest(), written


def copy(source, expected_bytes, out):
    need(os.path.isabs(source) and os.path.isabs(out), "paths must be absolute")
    need(type(expected_bytes) is int and expected_bytes > 0, "expected bytes")
    before = source_identity(source, expected_bytes)
    fd = os.open(out, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    try:
        try:
            first, dense = stream_hash(source, expected_bytes, fd)
            os.ftruncate(fd, expected_bytes)
            os.fsync(fd)
        finally:
            os.close(fd)
        second, _ = stream_hash(source, expected_bytes)  # Independent full re-read of the source.
        need(first == second, "source hash changed between reads")
        need(source_identity(source, expected_bytes) == before, "source identity changed during copy")
        local, _ = stream_hash(out, expected_bytes)
        need(local == first, "local copy hash")
    except BaseException:
        os.unlink(out)
        raise
    os.chmod(out, 0o400)
    return {"kind": "TemplateDiskCopy", "raw_bytes": expected_bytes, "raw_sha256": first,
            "source_identity_before_after_equal": True, "source_hash_reread_equal": True,
            "local_dense_written_bytes": dense, "disk_parsed_or_guest_executed": False}


# --- PVE API template guard (client injected; the workflow supplies the real token client) ---
POOL = "pc-templates"
STORAGE = "pc-ci-images"


def template_guard(client, node, vmid, name, tags):
    """Snapshot that must be identical before and after the copy. client.get(path) returns the decoded
    `data` of a GET /api2/json<path>. Refuses unless the VM is a stopped, unlocked template in pool
    pc-templates with the exact name/tags, no running task for the vmid and a root disk (scsi0, else
    virtio0) that is its own base volume on pc-ci-images."""
    need(re.fullmatch(r"[a-z0-9-]{1,63}", str(node)) is not None and type(vmid) is int and vmid > 0, "node/vmid")
    config = client.get("/nodes/%s/qemu/%d/config" % (node, vmid))
    status = client.get("/nodes/%s/qemu/%d/status/current" % (node, vmid))
    tasks = client.get("/nodes/%s/tasks?vmid=%d&source=active" % (node, vmid))
    pools = [r for r in client.get("/cluster/resources?type=vm") if r.get("vmid") == vmid]
    need(config.get("template") == 1 and status.get("status") == "stopped" and not config.get("lock"),
         "template must be stopped, unlocked")
    need(not [t for t in tasks if str(t.get("id")) == str(vmid) and not t.get("endtime")], "active task")
    need(config.get("name") == name and sorted(filter(None, str(config.get("tags", "")).split(";"))) == sorted(tags),
         "name/tags")
    need(len(pools) == 1 and pools[0].get("pool") == POOL and pools[0].get("template") == 1, "pool")
    need(re.fullmatch(r"[0-9a-f]{40}", str(config.get("digest"))) is not None, "config digest")
    volume = str(config.get("scsi0") or config.get("virtio0") or "").split(",")[0]
    need(re.fullmatch(r"%s:%d/base-%d-disk-[0-9]{1,2}\.(?:raw|qcow2)" % (STORAGE, vmid, vmid), volume) is not None,
         "root disk volume")
    return {"node": node, "vmid": vmid, "name": name, "digest": config["digest"], "volume": volume}


def template_source(snapshot, mount):
    """The file of the guarded root volume under the read-only storage mount: <mount>/images/<vmid>/<file>."""
    need(os.path.isabs(mount), "mount must be absolute")
    return str(Path(mount) / "images" / str(snapshot["vmid"]) / snapshot["volume"].split("/", 1)[1])


def guarded_copy(client, node, vmid, name, tags, mount, expected_bytes, out):
    """Template guard before, copy of the guarded volume, template guard after (must be equal)."""
    before = template_guard(client, node, vmid, name, tags)
    source = template_source(before, mount)
    receipt = copy(source, expected_bytes, out)
    try:
        need(template_guard(client, node, vmid, name, tags) == before, "template changed during the copy")
    except BaseException:
        os.unlink(out)
        raise
    return dict(receipt, source=source, template=before, template_unchanged_stopped=True)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--node", required=True)
    parser.add_argument("--vmid", required=True, type=int)
    parser.add_argument("--name", required=True)
    parser.add_argument("--tags", required=True, help="';'-separated exact tag set")
    parser.add_argument("--mount", default="/srv/" + STORAGE)
    parser.add_argument("--expected-bytes", required=True, type=int)
    parser.add_argument("--out", required=True)
    parser.add_argument("--receipt", required=True)
    a = parser.parse_args(argv)
    try:
        need(not os.path.lexists(a.receipt), "receipt already exists")  # Never leave a copy without its receipt.
        from build import PVE  # Token client from PVE_URL/PVE_TOKEN_ID/PVE_TOKEN_SECRET.
        pve = PVE()
        client = types.SimpleNamespace(get=lambda path: pve.call("GET", path))
        receipt = guarded_copy(client, a.node, a.vmid, a.name, list(filter(None, a.tags.split(";"))), a.mount,
                               a.expected_bytes, a.out)
        with open(a.receipt, "x", encoding="ascii") as f:
            json.dump(receipt, f, indent=2, sort_keys=True)
            f.write("\n")
        print(json.dumps({"raw_sha256": receipt["raw_sha256"], "raw_bytes": receipt["raw_bytes"]}))
        return 0
    except (CopyRefused, OSError, KeyError) as exc:
        print("copy refused: " + (str(exc) if isinstance(exc, CopyRefused) else type(exc).__name__), file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
