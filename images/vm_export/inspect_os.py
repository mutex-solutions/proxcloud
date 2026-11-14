#!/usr/bin/env python3
"""Read-only facts for a copied native OS template disk (qcow2); runs ONLY inside the confined capsule.

Usage (in capsule): python3 -I /bootstrap/inspect_os.py <os>

Reports layout, snapd identity PRESENCE/counts, accounts (names, uids, shells, marker classes),
cloud drop-in names/hashes and hygiene COUNTS. Never file contents, password hashes, keys, tokens,
IPs, hostnames, UUIDs or labels. Exit 0 = inventory, 3 = message-free probe_error, 4 = layout_refused.
"""
import hashlib
import json
import os
import pathlib
import re
import sys

PHASE = "os-readonly-inventory-v1"
SOURCE = "/input/source.qcow2"
OSES = ("ubuntu-2404", "ubuntu-2604", "debian-12", "debian-13")


def marker(value):
    if value in (b"x", b"!", b"!!", b"*", b"!*", b""):
        return value.decode() or "empty"
    return "locked_hash" if value.startswith(b"!$") else "hash" if value.startswith(b"$") else "other"


def os_inventory(vs, g, selected):
    """All facts from an attached read-only drive. Returns (exit_code, record)."""
    try:
        mounts, user = vs.layout(g, selected)
    except ValueError:
        return 4, {"phase": "layout_refused", "layout": {"filesystems": g.list_filesystems(),
                                                         "devices": g.list_devices(), "roots": g.inspect_os()}}
    vs.mount_all(g, mounts, "ro")
    shadow = {r.split(b":")[0]: r.split(b":")[1] for r in vs.read(g, "/etc/shadow").splitlines() if r.count(b":") == 8}
    accounts = []
    for row in vs.read(g, "/etc/passwd").splitlines():
        cols = row.split(b":")
        vs.require(len(cols) == 7 and cols[2].isdigit())
        accounts.append({"name": cols[0].decode("ascii", "replace"), "uid": int(cols[2]),
                         "shell": cols[6].decode("ascii", "replace"), "passwd_marker": marker(cols[1]),
                         "shadow_marker": marker(shadow.get(cols[0], b"?"))})
    dropins = []
    if vs.exists(g, vs.DROPINS.rstrip("/")):
        for name in g.ls(vs.DROPINS.rstrip("/")):
            path = vs.DROPINS + name
            regular = bool(not g.is_symlink(vs.safe(g, path)) and g.is_file(path, followsymlinks=False))
            dropins.append({"name": name, "regular_no_symlinks": regular,
                            "sha256": hashlib.sha256(vs.read(g, path)).hexdigest() if regular else None})
    facts, _, _ = vs.k8s_snapd(g)
    snapd = dict(facts, unwipeable_key_entries=vs.k8s_snapd_unwipeable(g),
                 seed_dir=bool(vs.exists(g, "/var/lib/snapd/seed")),
                 snaps=len(g.glob_expand("/var/lib/snapd/snaps/*.snap")))
    scanned = vs.k8s_files(g, vs.SCAN)
    sudoers_d = vs.k8s_files(g, (vs.SUDOERS_D,))
    hygiene = {"scan_files": len(scanned), "scan_hits": sum(bool(vs.BAD.search(vs.read(g, p))) for p in scanned),
               "build_sudoers_dropins": sum(bool(re.search(rb"\b(?:builder|packer)\b", vs.read(g, p))) for p in sudoers_d),
               "ssh_host_keys": len(g.glob_expand("/etc/ssh/ssh_host_*")),
               "machine_id_set": bool(vs.exists(g, "/etc/machine-id") and vs.read(g, "/etc/machine-id").strip()),
               "cloud_state": bool(vs.exists(g, "/var/lib/cloud"))}
    eligible = (not snapd["snapd_session_macaroon"] and snapd["snapd_users"] == 0
                and snapd["unwipeable_key_entries"] == 0 and any(a["name"] == user for a in accounts))
    return 0, {"phase": PHASE, "os": selected, "layout": {"mountpoints": sorted(mounts), "default_user": user},
               "accounts": accounts, "cloud_dropins": dropins, "snapd": snapd, "hygiene": hygiene,
               "sanitize_eligible": eligible, "readonly_no_guest_code_or_mutation": True,
               "publication_eligible": False}


def emit(record, code):
    line = json.dumps(record, sort_keys=True)
    if len(line) > 60000 or "10.200." in line or "PRIVATE KEY" in line:
        line, code = json.dumps({"phase": "probe_error", "error_class": "OutputRefused", "frames": []}), 3
    sys.stdout.write(line + "\n")
    sys.stdout.flush()
    os._exit(code)


def frames_only(exc):
    import traceback
    return {"phase": "probe_error", "error_class": type(exc).__name__,
            "frames": [[pathlib.Path(f.filename).name, f.lineno, f.name]
                       for f in traceback.extract_tb(exc.__traceback__)][-12:]}


def main(argv):
    if len(argv) != 1 or argv[0] not in OSES:
        emit({"phase": "probe_error", "error_class": "Usage", "frames": []}, 3)
    assert os.getuid() == os.geteuid() >= 1000 and not pathlib.Path("/dev/kvm").exists()
    for name in ("home", "tmp", "cache"):
        pathlib.Path("/scratch", name).mkdir(mode=0o700, exist_ok=True)
    os.environ.clear()
    os.environ.update(PATH="/usr/sbin:/usr/bin:/sbin:/bin", HOME="/scratch/home", TMPDIR="/scratch/tmp",
                      LIBGUESTFS_TMPDIR="/scratch/tmp", LIBGUESTFS_CACHEDIR="/scratch/cache",
                      LIBGUESTFS_BACKEND="direct", LIBGUESTFS_BACKEND_SETTINGS="force_tcg", LC_ALL="C")
    sys.path.insert(0, "/bootstrap")
    import vm_sanitize as vs
    import guestfs
    vs.image_info(SOURCE)  # Read-only qemu-img info/check: qcow2, 20 GiB, no backing/data-file/snapshots.
    g = guestfs.GuestFS(python_return_dict=True)
    g.set_backend("direct")
    g.set_backend_setting("force_tcg", "true")
    g.set_network(False)
    g.add_drive_opts(SOURCE, format="qcow2", readonly=True)
    g.launch()
    code, record = os_inventory(vs, g, argv[0])
    if code == 0:
        try:
            g.umount_all()
            g.shutdown()
            record["close_error"] = None
        except Exception as exc:  # noqa: BLE001
            record["close_error"] = type(exc).__name__
    emit(record, code)


if __name__ == "__main__":
    try:
        main(sys.argv[1:])
    except BaseException as exc:  # noqa: BLE001 - bounded, message-free diagnostics
        emit(frames_only(exc), 3)
