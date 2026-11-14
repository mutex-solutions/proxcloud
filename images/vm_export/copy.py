"""Read-only private copy of a stopped template's disk from the read-only NFS mount of its storage.

    cd images && python3 -m vm_export.copy --source /srv/pc-ci-images/images/<vmid>/base-<vmid>-disk-N.raw \
        --expected-bytes 21474836480 --out DIR/source.raw --receipt DIR/copy-receipt.json

Never parses the disk and never runs guest code. Guards: S_ISREG, nlink 1, exact size, file identity
(dev/ino/size/mtime/ctime) unchanged before == after, streamed sha256 of the copy equal to a second
full re-read of the source, sparse local write, output mode 0400. The PVE API template guard
(template_guard) takes an injected client; the workflow calls it before and after the copy.
"""
import argparse
import hashlib
import json
import os
import re
import stat
import sys

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


def template_guard(client, node, vmid, name, tags):
    """Snapshot that must be identical before and after the copy. client.get(path) returns the decoded
    `data` of a GET /api2/json<path>. Refuses unless the VM is a stopped, unlocked template in pool
    pc-templates with the exact name/tags and no running task for the vmid."""
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
    return {"node": node, "vmid": vmid, "name": name, "digest": config["digest"]}


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--source", required=True)
    parser.add_argument("--expected-bytes", required=True, type=int)
    parser.add_argument("--out", required=True)
    parser.add_argument("--receipt", required=True)
    a = parser.parse_args(argv)
    try:
        receipt = copy(a.source, a.expected_bytes, a.out)
        with open(a.receipt, "x", encoding="ascii") as f:
            json.dump(receipt, f, indent=2, sort_keys=True)
            f.write("\n")
        print(json.dumps({"raw_sha256": receipt["raw_sha256"], "raw_bytes": receipt["raw_bytes"]}))
        return 0
    except (CopyRefused, OSError) as exc:
        print("copy refused: " + (str(exc) if isinstance(exc, CopyRefused) else type(exc).__name__), file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
