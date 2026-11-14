#!/usr/bin/env python3
"""In-capsule framing: only fixed sanitizer facts and the sealed disk bytes reach stdout.

Usage (in capsule): python3 -I /bootstrap/stream_sanitized.py --os <os> <timeout>
                    python3 -I /bootstrap/stream_sanitized.py --k8s-os ubuntu-2604 <k8s_version> <timeout>

Runs the repo sanitizer (vm_sanitize.py) through runpy with /bootstrap first on sys.path (python -I
implies -P, so its lazy `import k8s_copy_policy` resolves to the bound copy). Stdout = one compact
facts line, then exactly disk_bytes of /scratch/export/disk.qcow2. Any failure exits 1 with NOTHING
on stdout: raw guest/tool exceptions and diagnostics are never repeated.
"""
import hashlib
import json
import os
import re
import stat
import subprocess
import sys

CHECKS = {"cleaned_copy_only", "no_backing_file", "qemu_img_check", "offline_sanitization",
          "zero_free_space", "no_build_credentials_or_lab_network"}
MAX = 8 << 30
OSES = ("ubuntu-2404", "ubuntu-2604", "debian-12", "debian-13")
ENV = {"PATH": "/usr/sbin:/usr/bin:/sbin:/bin", "HOME": "/scratch", "TMPDIR": "/scratch",
       "LC_ALL": "C", "LIBGUESTFS_BACKEND": "direct", "LIBGUESTFS_BACKEND_SETTINGS": "force_tcg"}
LAUNCH = ("import runpy, sys; sys.path.insert(0, '/bootstrap'); "
          "sys.argv = ['/bootstrap/vm_sanitize.py', %r, %r]; "
          "runpy.run_path('/bootstrap/vm_sanitize.py', run_name='__main__')")


def valid(row, selected, k8s_version):
    keys = {"kind", "os", "variant", "disk_sha256", "disk_bytes", "virtual_size_bytes", "checks"}
    keys |= {"kubernetes_version"} if k8s_version else set()
    return (type(row) is dict and set(row) == keys and row["kind"] == "OfflineSanitizationFacts"
            and row["os"] == selected and row["variant"] == ("kubernetes" if k8s_version else "os")
            and row.get("kubernetes_version") == k8s_version
            and type(row["disk_sha256"]) is str and re.fullmatch(r"[0-9a-f]{64}", row["disk_sha256"]) is not None
            and type(row["disk_bytes"]) is int and 0 < row["disk_bytes"] < MAX
            and type(row["virtual_size_bytes"]) is int and row["virtual_size_bytes"] == 20 << 30
            and type(row["checks"]) is dict and set(row["checks"]) == CHECKS
            and all(value is True for value in row["checks"].values()))


def parse(argv):
    if len(argv) == 3 and argv[0] == "--os" and argv[1] in OSES and argv[2].isdigit():
        return argv[0], argv[1], None, int(argv[2])
    if (len(argv) == 4 and argv[:2] == ["--k8s-os", "ubuntu-2604"] and argv[3].isdigit()
            and re.fullmatch(r"[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}", argv[2])):
        return argv[0], argv[1], argv[2], int(argv[3])
    raise ValueError("usage")


def main(argv):
    flag, selected, k8s_version, timeout = parse(argv)
    if not os.getuid() == os.geteuid() >= 1000:  # The invoking host user's uid; never root.
        return 1
    result = subprocess.run(["/usr/bin/python3", "-I", "-c", LAUNCH % (flag, selected)], stdin=subprocess.DEVNULL,
                            capture_output=True, timeout=timeout, env=ENV)
    if result.returncode or len(result.stdout) + len(result.stderr) > 65536:
        return 1
    row = json.loads(result.stdout.decode("ascii"))
    if not valid(row, selected, k8s_version):
        return 1
    fd = os.open("/scratch/export/clean-export-facts.json", os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as f:
        info = os.fstat(f.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_size > 8192:
            return 1
        if json.loads(f.read(8193)) != row:
            return 1
    fd = os.open("/scratch/export/disk.qcow2", os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as disk:
        before = os.fstat(disk.fileno())
        if not stat.S_ISREG(before.st_mode) or before.st_nlink != 1 or before.st_size != row["disk_bytes"]:
            return 1
        digest = hashlib.sha256()
        for data in iter(lambda: disk.read(1 << 20), b""):
            digest.update(data)
        if digest.hexdigest() != row["disk_sha256"]:
            return 1
        disk.seek(0)
        sys.stdout.buffer.write((json.dumps(row, sort_keys=True, separators=(",", ":")) + "\n").encode("ascii"))
        sys.stdout.buffer.flush()
        count = 0
        for data in iter(lambda: disk.read(1 << 20), b""):
            count += len(data)
            if count > row["disk_bytes"]:
                return 1
            sys.stdout.buffer.write(data)
        sys.stdout.buffer.flush()
        after = os.fstat(disk.fileno())
        if count != row["disk_bytes"] or ((before.st_ino, before.st_size, before.st_mtime_ns) !=
                                          (after.st_ino, after.st_size, after.st_mtime_ns)):
            return 1
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main(sys.argv[1:]))
    except Exception:  # noqa: BLE001 - never echo
        sys.exit(1)
