"""Parent launcher: run the repo offline sanitizer (vm_sanitize.py) in one confined no-network capsule.

    cd images && python3 -m vm_export.sanitize k8s --source RAW --copy-receipt R --review k8s-review.json \
        --kubernetes-version 1.37.2 --scratch-base DIR --out-dir DIR
    cd images && python3 -m vm_export.sanitize os --os ubuntu-2404 --source QCOW2 --copy-receipt R \
        --scratch-base DIR --out-dir DIR

The source must be the file of the copy receipt; a k8s review must name that same source sha256.

The capsule streams one fixed facts line plus the disk bytes over stdout; the parent spools them
privately into <out-dir>/disk.qcow2.partial, and only after the exact stop/cgroup proof, a matching
streamed hash and a re-read hash seals the file (0400) as <out-dir>/disk.qcow2 with
<out-dir>/sanitize-facts.json. Scratch is a fresh disk-backed directory, removed afterwards.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys

from vm_export import capsule
from vm_export.stream_sanitized import MAX, valid

HERE = Path(__file__).resolve().parent
IMAGES = HERE.parent


class StreamSink:
    """Header line (bounded, schema-checked) then exactly disk_bytes into a private spool."""

    def __init__(self, spool, selected, k8s_version):
        self.spool, self.selected, self.k8s_version = spool, selected, k8s_version
        self.header, self.facts, self.received, self.hashed = bytearray(), None, 0, hashlib.sha256()

    def __call__(self, data):
        if self.facts is None:
            first, separator, data = data.partition(b"\n")
            self.header.extend(first)
            if len(self.header) > 8192:
                return "fixed_header_limit"
            if not separator:
                return None
            try:
                facts = json.loads(self.header.decode("ascii"))
            except (ValueError, UnicodeError):
                return "fixed_header_refused"
            if not valid(facts, self.selected, self.k8s_version):
                return "fixed_header_refused"
            self.facts = facts
        self.received += len(data)
        if self.received > self.facts["disk_bytes"] or self.received >= MAX:
            return "binary_output_limit"
        self.hashed.update(data)
        self.spool.write(data)
        return None


def mem_available():
    rows = dict(line.split(":", 1) for line in Path("/proc/meminfo").read_text().splitlines() if ":" in line)
    return int(rows["MemAvailable"].split()[0]) * 1024


OUTPUTS = ("disk.qcow2.partial", "disk.qcow2", "sanitize-facts.json", "sanitize-run.json")


def sealable(term, sink, spool_path):
    """Seal only after the exact stop and cgroup proof, a complete stream whose hash matches the facts
    header, and a re-read of the private spool with the same hash."""
    facts = sink.facts
    if not (facts is not None and term["exit_code"] == term["attach_exit_code"] == 0 and term["stop_reason"] is None
            and term["cgroup_empty_or_absent"] and not term["cleanup_errors"]
            and sink.received == facts["disk_bytes"] and sink.hashed.hexdigest() == facts["disk_sha256"]):
        return False
    st = spool_path.lstat()
    return (stat.S_ISREG(st.st_mode) and st.st_nlink == 1 and st.st_size == sink.received
            and capsule.sha256_file(spool_path) == facts["disk_sha256"])


def launch(selected, k8s_version, source, receipt, review, scratch_base, out_dir, memory, wall):
    out_dir = Path(out_dir)
    need = capsule.need
    need(out_dir.is_absolute() and out_dir.is_dir() and not out_dir.is_symlink(), "out dir refused")
    need(not any((out_dir / name).exists() for name in OUTPUTS), "output already exists")
    need(mem_available() >= capsule.size_bytes(memory) + (2 << 30), "insufficient memory for the capsule")
    binds = {str(Path(source)): "/input/source.raw" if k8s_version else "/input/source.qcow2",
             str(IMAGES / "vm_sanitize.py"): "/bootstrap/vm_sanitize.py",
             str(IMAGES / "k8s_copy_policy.py"): "/bootstrap/k8s_copy_policy.py",
             str(HERE / "stream_sanitized.py"): "/bootstrap/stream_sanitized.py"}
    if k8s_version:
        binds[str(Path(review))] = "/input/k8s-review.json"
        args = ["--k8s-os", selected, k8s_version, str(wall - 120)]
    else:
        args = ["--os", selected, str(wall - 120)]
    pins = capsule.pin_all(binds)
    guest_source = "/input/source.raw" if k8s_version else "/input/source.qcow2"
    capsule.bound_to_receipt(receipt, source, pins[guest_source])
    if k8s_version:
        try:
            reviewed = json.loads(Path(review).read_bytes().decode("ascii")).get("source_raw_sha256")
        except (ValueError, UnicodeError, AttributeError):
            reviewed = None
        need(reviewed == pins[guest_source], "review names another source")
    check = "sanitize-" + selected + ("-k8s-" + k8s_version if k8s_version else "")
    spool_path, final = out_dir / "disk.qcow2.partial", out_dir / "disk.qcow2"
    vfs = os.statvfs(out_dir)
    need(vfs.f_bavail * vfs.f_frsize >= MAX + (1 << 30), "insufficient output space")
    scratch = capsule.prepare_scratch(scratch_base, check, 24 << 30)
    try:
        s = capsule.spec(check, binds, scratch, ["/usr/bin/python3", "-I", "/bootstrap/stream_sanitized.py", *args],
                         memory=memory, wall=wall, cpu_seconds=wall, fsize=32 << 30)
        fd = os.open(spool_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, "wb") as spool:
            sink = StreamSink(spool, selected, k8s_version)
            term = capsule.run(s, sink, pins)
            spool.flush()
            os.fsync(spool.fileno())
    except BaseException as exc:
        spool_path.unlink(missing_ok=True)  # Parent-owned; no half-written disk is left behind.
        if isinstance(exc, capsule.Unresolved):
            scratch = None
        raise
    finally:
        if scratch is not None:
            capsule.remove_scratch(scratch)
    facts = sink.facts
    success = sealable(term, sink, spool_path)
    if success:
        os.chmod(spool_path, 0o400)  # Seal only after exact stop and surviving-writer/cgroup proof.
        os.rename(spool_path, final)
        capsule.write_new(out_dir / "sanitize-facts.json", (json.dumps(facts, sort_keys=True) + "\n").encode("ascii"))
    else:
        spool_path.unlink(missing_ok=True)
    report = dict(term, check_id=check, image=s["image"], pins=pins, private_bytes=sink.received,
                  sealed_after_cgroup_stop=success, facts=facts if success else None,
                  disk=str(final) if success else None, public_eligible=False)
    capsule.write_new(out_dir / "sanitize-run.json", (json.dumps(report, indent=2, sort_keys=True) + "\n").encode())
    print(json.dumps({"check_id": check, "sealed": success, "stop_reason": term["stop_reason"],
                      "exit_code": term["exit_code"]}, sort_keys=True))
    return 0 if success else 1


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="variant", required=True)
    k = sub.add_parser("k8s")
    k.add_argument("--review", required=True)
    k.add_argument("--kubernetes-version", required=True)
    o = sub.add_parser("os")
    o.add_argument("--os", required=True, choices=("ubuntu-2404", "ubuntu-2604", "debian-12", "debian-13"))
    for p in (k, o):
        p.add_argument("--source", required=True)
        p.add_argument("--copy-receipt", required=True)
        p.add_argument("--scratch-base", required=True)
        p.add_argument("--out-dir", required=True)
        p.add_argument("--memory", default="8g", help="capsule memory limit (default 8g; scratch must be disk-backed)")
        p.add_argument("--wall-seconds", type=int, default=1800)
    a = parser.parse_args(argv)
    if a.wall_seconds < 300:
        parser.error("wall too small")
    if a.variant == "k8s":
        if not re.fullmatch(r"[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}", a.kubernetes_version):
            parser.error("invalid version")
        return launch("ubuntu-2604", a.kubernetes_version, a.source, a.copy_receipt, a.review, a.scratch_base,
                      a.out_dir, a.memory, a.wall_seconds)
    return launch(a.os, None, a.source, a.copy_receipt, None, a.scratch_base, a.out_dir, a.memory, a.wall_seconds)


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (capsule.CapsuleError, OSError, subprocess.SubprocessError) as exc:
        print("sanitize refused: " + (str(exc) if isinstance(exc, capsule.CapsuleError) else type(exc).__name__),
              file=sys.stderr)
        sys.exit(2)
