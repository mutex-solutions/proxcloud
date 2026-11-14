#!/usr/bin/env python3
"""Read-only facts for a copied Kubernetes node RAW root disk; runs ONLY inside the confined capsule.

Usage (in capsule): python3 -I /bootstrap/inspect_k8s.py <kubernetes_version> <containerd_version> <runc_version>

Reuses the repo sanitizer's read-only Kubernetes inventory (vm_sanitize.k8s_inventory) with the drive
attached read-only and the root filesystem mounted read-only. Never writes, never runs guest code.
Emits ONE bounded JSON line: counts, versions, account names/marker classes, drop-in names/hashes,
sudo policy hashes. Never file contents, password hashes, keys, tokens, IPs, hostnames, UUIDs or labels.
Exit 0 = inventory, 3 = message-free probe_error, 4 = layout_refused. No package imports: the parent
binds this file, vm_sanitize.py and k8s_copy_policy.py into /bootstrap.
"""
import hashlib
import json
import os
import pathlib
import re
import sys

PHASE = "k8s-readonly-inventory-v1"
RAW = "/input/source.raw"
VERSION = re.compile(r"[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}")
WATCHED = ("kubeadm", "kubelet", "kubectl", "kubernetes-cni", "cri-tools", "containerd", "containerd.io",
           "runc", "snapd", "cloud-init", "openssh-server", "qemu-guest-agent")


def frames_only(exc):
    """Exception class plus (file basename, line, function) frames only: no message, no guest data."""
    import traceback
    return {"phase": "probe_error", "error_class": type(exc).__name__,
            "frames": [[pathlib.Path(f.filename).name, f.lineno, f.name]
                       for f in traceback.extract_tb(exc.__traceback__)][-12:]}


def dpkg_rows(vs, g):
    for stanza in vs.read(g, "/var/lib/dpkg/status", 64 << 20).split(b"\n\n"):
        yield dict(line.split(b": ", 1) for line in stanza.splitlines() if b": " in line and not line[:1].isspace())


def layout_facts(vs, g):
    """Mount-free layout facts (device names, fs types, mountpoint paths, partition types only)."""
    roots = g.inspect_os()
    parttype = g.part_get_parttype("/dev/sda")
    mounts = g.inspect_get_mountpoints(roots[0]) if len(roots) == 1 else {}
    resolved = None
    if sorted(mounts) == ["/"]:
        try:
            resolved = vs.k8s_root_device(g, mounts["/"], roots[0])  # Exact canonical by-uuid only.
        except ValueError:
            resolved = None
    filesystems = g.list_filesystems()
    layout = {"devices": g.list_devices(), "lvs": len(g.lvs()), "roots": roots, "filesystems": filesystems,
              "parttype": parttype,
              "partitions": [{"num": p["part_num"], "bytes": p["part_size"],
                              "mbr_id": g.part_get_mbr_id("/dev/sda", p["part_num"]) if parttype == "msdos" else None,
                              "gpt_type": g.part_get_gpt_type("/dev/sda", p["part_num"]).lower() if parttype == "gpt" else None}
                             for p in g.part_list("/dev/sda")],
              "mountpoints": {k: ("<by-uuid>" if v.startswith("/dev/disk/by-uuid/") else v) for k, v in mounts.items()}}
    ok = len(roots) == 1 and resolved == roots[0] and roots[0] in filesystems
    return ok, layout


def sudo_hashes(vs, g):
    """sha256 of /etc/sudoers and /etc/sudoers.d/README (bytes hashed, never emitted), mounted read-only."""
    mounts, _, _ = vs.k8s_layout(g, "ubuntu-2604", {"bios": "seabios"})
    vs.mount_all(g, mounts, "ro")
    try:
        return {"sudoers_sha256": hashlib.sha256(vs.read(g, vs.SUDOERS)).hexdigest(),
                "sudoers_readme_sha256": hashlib.sha256(vs.read(g, vs.SUDOERS_D + "/" + vs.README, 65536)).hexdigest()}
    finally:
        g.umount_all()


def binary_has(vs, g, path, needles, limit=192 << 20):
    if not vs.exists(g, path) or g.is_symlink(path) or not g.is_file(path, followsymlinks=False):
        return None
    size = g.filesize(path)
    found, offset, tail = {n: False for n in needles}, 0, b""
    while offset < min(size, limit):
        chunk = g.pread(path, 1 << 20, offset)
        data = tail + chunk
        for n in needles:
            found[n] = found[n] or n.encode() in data
        tail, offset = data[-64:], offset + len(chunk)
        if not chunk:
            break
    return {"bytes": size, **found}


def inventory(vs, policy, g, virtual_bytes, k8s_version, containerd_version, runc_version):
    """All facts from an attached read-only drive of virtual_bytes (raw_info's size). Returns (exit_code, record)."""
    ok, layout = layout_facts(vs, g)
    if not ok:
        return 4, {"phase": "layout_refused", "layout": layout}
    sudo = sudo_hashes(vs, g)
    # Placeholder review: only the parent's later review binds versions/hashes; these values are never
    # treated as reviewed. The observed sudo hashes let the sanitizer's structural sudo guards run.
    review = {"bios": "seabios", "network_backend": "netplan-systemd-networkd", "cloud_default_user": "ubuntu",
              "cloud_base_config_reviewed": True, "kubernetes_version": k8s_version, "kubelet_active": False,
              "runtime_version": "0.0.0", "retained_package_policy_reviewed": True,
              "package_policy_sha256": "0" * 64, **sudo}
    parent = {"format": "raw", "virtual_bytes": virtual_bytes, "raw_bytes": virtual_bytes,
              "source_hash_verified": True, "source_stopped": True}
    _, facts, tokens = vs.k8s_inventory(g, "ubuntu-2604", review, parent)
    observed_backend = ("netplan-systemd-networkd" if g.exists("/usr/sbin/netplan") and
                        g.exists("/usr/lib/systemd/system/systemd-networkd.service") and
                        not g.exists("/usr/sbin/NetworkManager") else "UNKNOWN")

    # Raw shadow-marker CLASS for accounts the policy marks UNKNOWN (never the hash bytes).
    shadow = {r.split(b":")[0].decode("ascii", "replace"): r.split(b":")[1]
              for r in vs.read(g, "/etc/shadow").splitlines() if r.count(b":") == 8}

    def shadow_class(value):
        if value in (b"!", b"!!", b"*", b"!*", b""):
            return value.decode() or "empty"
        if value.startswith(b"!$"):
            return "locked_hash"
        if value.startswith(b"$"):
            return "hash"
        return "other"
    marker_classes = {a["name"]: shadow_class(shadow.get(a["name"], b"?"))
                      for a in facts["accounts"] if a["shadow_marker"] == "UNKNOWN" or a["kind"] == "UNKNOWN"}
    unknown_kind = [{"name": a["name"], "uid": a["uid"], "shell": a["shell"]} for a in facts["accounts"]
                    if a["kind"] == "UNKNOWN"]

    fstab = vs.read(g, "/etc/fstab")
    swap = {"swap_img": bool(vs.exists(g, "/swap.img")), "swapfile": bool(vs.exists(g, "/swapfile")),
            "fstab_rows_any": len(vs.K8S_FSTAB_SWAP.findall(fstab)),
            "fstab_rows_active": sum(1 for line in fstab.splitlines()
                                     if not line.lstrip().startswith(b"#") and re.search(rb"\sswap\s", line)),
            "units": sum(len(g.glob_expand(u)) for u in vs.K8S_SWAP_UNITS)}

    apt = vs.k8s_files(g, ("/etc/apt/sources.list", "/etc/apt/sources.list.d"))
    proxies = vs.k8s_files(g, vs.K8S_PROXY_FILES)
    containerd_files = vs.k8s_files(g, ("/etc/containerd",))
    scanned = [p for p in vs.k8s_files(g, vs.K8S_SCAN) if p not in tokens]
    kubelet = [p for p in vs.k8s_files(g, (vs.K8S_KUBELET_STATE,)) if p not in tokens and
               not p.startswith((vs.K8S_KUBELET_STATE + "/pki/", vs.K8S_KUBELET_STATE + "/pods/"))]

    def hits(rx, paths):
        return sorted({p.rsplit("/", 1)[0] for p in paths if rx.search(vs.read(g, p))})[:10]
    hygiene = {"apt_url_files": sum(bool(vs.K8S_APT.search(vs.read(g, p))) for p in apt),
               "proxy_files": sum(bool(vs.K8S_PROXY.search(vs.read(g, p))) for p in proxies),
               "proxy_dirs": hits(vs.K8S_PROXY, proxies),
               "containerd_auth_files": sum(bool(vs.K8S_REGISTRY.search(vs.read(g, p))) for p in containerd_files),
               "registry_files_present": sum(bool(vs.exists(g, p)) for p in vs.K8S_REGISTRY_FILES),
               "credential_scan_files": sum(bool(vs.K8S_CREDENTIAL.search(vs.read(g, p))) for p in scanned),
               "credential_scan_dirs": hits(vs.K8S_CREDENTIAL, scanned),
               "kubelet_state_files": sum(bool(vs.K8S_CREDENTIAL.search(d) or vs.K8S_LAB.search(d))
                                          for d in (vs.read(g, p) for p in kubelet)),
               "secret_paths": {p: vs.k8s_count(g, p, 1) for p in vs.K8S_SECRET_PATHS}}

    holds, versions = {}, {}
    for f in dpkg_rows(vs, g):
        name = f.get(b"Package", b"").decode("ascii", "replace")
        if name in WATCHED:
            version = f.get(b"Version", b"").decode("ascii", "replace")
            versions[name] = version if re.fullmatch(r"[A-Za-z0-9.+:~_-]{1,64}", version) else "UNSAFE"
            holds[name] = f.get(b"Status", b"").split(b" ")[0].decode("ascii", "replace")

    runtime = {"containerd": binary_has(vs, g, "/usr/local/bin/containerd", ("v" + containerd_version, containerd_version)),
               "runc": binary_has(vs, g, "/usr/local/sbin/runc", (runc_version,)),
               "crictl_present": bool(vs.exists(g, "/usr/local/bin/crictl")),
               "containerd_config_sha256": (hashlib.sha256(vs.read(g, "/etc/containerd/config.toml")).hexdigest()
                                            if vs.exists(g, "/etc/containerd/config.toml") else None),
               "containerd_unit_enabled": bool(g.is_symlink("/etc/systemd/system/multi-user.target.wants/containerd.service"))}

    cloud_cfg = vs.read(g, "/etc/cloud/cloud.cfg") if vs.exists(g, "/etc/cloud/cloud.cfg") else b""
    m = re.search(rb"(?ms)^\s*default_user:\s*\n\s*name:\s*([a-z_][a-z0-9_-]{0,30})\s*$", cloud_cfg)
    base_default_user = m.group(1).decode() if m else "UNKNOWN"
    dropin_users = {}
    for d in facts["cloud_dropins"]:
        if d["regular_no_symlinks"]:
            mm = re.search(rb"(?ms)default_user:\s*\n\s*name:\s*([a-z_][a-z0-9_-]{0,30})", vs.read(g, vs.DROPINS + d["name"]))
            dropin_users[d["name"]] = mm.group(1).decode() if mm else None
    snapd = {"installed": "snapd" in versions, "seed_dir": bool(vs.exists(g, "/var/lib/snapd/seed")),
             "snaps": len(g.glob_expand("/var/lib/snapd/snaps/*.snap")) if vs.exists(g, "/var/lib/snapd/snaps") else 0}

    refusal = getattr(policy, "PolicyRefusal", ValueError)
    try:
        policy.validate_inventory(facts, tuple((d["name"], d["sha256"], "remove_build") for d in facts["cloud_dropins"]))
        result = "PASS_WITH_PLACEHOLDER_REVIEW"
    except refusal:
        result = "REFUSED"
    except Exception as exc:  # noqa: BLE001 - class name only
        result = "ERROR_" + type(exc).__name__
    return 0, {"phase": PHASE, "policy_result": result, "observed_backend": observed_backend,
               "facts": facts, "shadow_marker_classes": marker_classes, "unknown_kind_accounts": unknown_kind,
               "swap": swap, "hygiene_detail": hygiene, "packages": versions, "package_status_word": holds,
               "runtime": runtime, "cloud_base_default_user": base_default_user, "dropin_default_users": dropin_users,
               "snapd": snapd, "token_files": len(tokens), "sudoers": sudo,
               "readonly_no_guest_code_or_mutation": True, "publication_eligible": False}


def emit(record, code):
    line = json.dumps(record, sort_keys=True)
    reasons = [name for name, bad in (("OutputTooLarge", len(line) > 60000), ("OutputHasLabAddress", "10.200." in line),
                                      ("OutputHasKeyMarker", "PRIVATE KEY" in line)) if bad]
    if reasons:
        sizes = {k: len(json.dumps(v)) for k, v in record.items()}
        line = json.dumps({"phase": "probe_error", "error_class": reasons[0],
                           "frames": [[k, sizes[k], "size"] for k in sorted(sizes)][:12]}, sort_keys=True)
        code = 3
    sys.stdout.write(line + "\n")
    sys.stdout.flush()
    os._exit(code)  # Skip guestfs finalizers after the single result line.


def main(argv):
    if len(argv) != 3 or not all(VERSION.fullmatch(a) for a in argv):
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
    import k8s_copy_policy as policy
    import guestfs

    real_mount = vs.mount_all

    def ro_only(g, mounts, options=""):
        vs.require(options == "ro")  # The inventory never mounts read-write.
        real_mount(g, mounts, options)
    vs.mount_all = ro_only
    virtual_bytes = vs.raw_info(RAW)["virtual-size"]  # Explicit -f raw; allowlisted size; no backing/external data.
    g = guestfs.GuestFS(python_return_dict=True)
    g.set_backend("direct")
    g.set_backend_setting("force_tcg", "true")
    g.set_network(False)
    g.add_drive_opts(RAW, format="raw", readonly=True)
    g.launch()
    code, record = inventory(vs, policy, g, virtual_bytes, *argv)
    if code == 0:
        try:
            g.umount_all()
            g.shutdown()
            record["close_error"] = None
        except Exception as exc:  # noqa: BLE001 - read-only drive; class only
            record["close_error"] = type(exc).__name__
    emit(record, code)


if __name__ == "__main__":
    try:
        main(sys.argv[1:])
    except BaseException as exc:  # noqa: BLE001 - bounded, message-free diagnostics
        emit(frames_only(exc), 3)
