#!/usr/bin/env python3
"""Independent READ-ONLY re-open of a sealed export; runs ONLY inside the confined capsule.

Usage (in capsule): python3 -I /bootstrap/postscan_probe.py --k8s <kubernetes_version>
                    python3 -I /bootstrap/postscan_probe.py --os <os>

Re-runs the repo sanitizer's own post-cleanup guards against the exported bytes (including the
snapd unregistered check), plus boot-essentials and journal checks. Never writes, never runs guest
code. Prints ONE fixed-schema line of booleans; any failed guard exits 3 with class+frames only.
"""
import hashlib
import json
import os
import pathlib
import re
import sys

SOURCE = "/input/source.qcow2"
OSES = ("ubuntu-2404", "ubuntu-2604", "debian-12", "debian-13")


def expected(variant, value):
    """The exact success line; the parent compares the probe output to this byte-for-byte schema."""
    common = {"sanitizer_final_guards_reverified": True, "snapd_unregistered": True,
              "accounts_locked_no_keys_no_hostkeys_empty_machine_id": True, "boot_and_runtime_binaries_present": True,
              "ext4_journal_restored": True, "fresh_independent_readonly_appliance": True,
              "guest_code_executed": False, "not_boot_acceptance": True}
    if variant == "kubernetes":
        return dict(common, phase="sealed-k8s-export-postscan", variant="kubernetes", kubernetes_version=value,
                    builder_no_groups_own_primary=True, sudo_bound_to_reviewed_bytes=True,
                    kubelet_enabled_packages_kept=True, no_cluster_state=True, no_active_swap=True)
    return dict(common, phase="sealed-os-export-postscan", variant="os", os=value)


def parse(argv):
    if len(argv) == 2 and argv[0] == "--k8s" and re.fullmatch(r"[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}", argv[1]):
        return "kubernetes", argv[1]
    if len(argv) == 2 and argv[0] == "--os" and argv[1] in OSES:
        return "os", argv[1]
    raise ValueError("usage")


def check_k8s(vs, k8s, g, review, version):
    assert g.list_filesystems() == {"/dev/sda1": "ext4"} and g.inspect_os() == ["/dev/sda1"]
    g.mount_ro("/dev/sda1", "/")
    preserved = {n: d for n, d, a in review["reviewed_dropins"] if a == "preserve_vendor"}
    removed = [n for n, d, a in review["reviewed_dropins"] if a == "remove_build"]
    patterns = tuple(p for p in vs.REMOVE if p != vs.DROPINS + "*") + k8s.COPY_WIPE + tuple(vs.DROPINS + n for n in removed)
    vs.verify(g, vs.K8S_USER, patterns, vs.SCAN + vs.K8S_SCAN)
    assert sorted(g.ls(vs.DROPINS.rstrip("/"))) == sorted(preserved)
    for name, digest in preserved.items():
        assert hashlib.sha256(vs.read(g, vs.DROPINS + name)).hexdigest() == digest
    assert vs.k8s_builder_groups(g) == 0 and vs.k8s_builder_primary_group_own(g) is True
    vs.k8s_sudo(g, review, after=True)
    vs.snapd_unregistered(g)
    enabled, packages, cluster = vs.k8s_state(g)
    assert enabled is True and packages == {"kubeadm": version, "kubelet": version, "kubectl": version}
    assert all(v == 0 for v in cluster.values())
    shadow = {r.split(b":")[0]: r.split(b":")[1] for r in vs.read(g, "/etc/shadow").splitlines()}
    assert all(v == b"!" for v in shadow.values()) and b"ubuntu" not in shadow
    assert not vs.exists(g, "/home/builder/.ssh") and not vs.exists(g, "/root/.ssh")
    assert vs.read(g, "/etc/machine-id") == b""
    assert not g.glob_expand("/etc/ssh/ssh_host_*")
    for path in ("/usr/bin/cloud-init", "/usr/sbin/sshd", "/usr/bin/kubeadm", "/usr/bin/kubelet",
                 "/usr/local/bin/containerd", "/usr/local/sbin/runc"):
        assert g.is_file(path), path
    assert any(g.is_file(p) for p in ("/usr/sbin/qemu-ga", "/usr/bin/qemu-ga"))
    assert "has_journal" in g.tune2fs_l("/dev/sda1")["Filesystem features"].split()
    assert not vs.exists(g, "/var/lib/cloud") and not vs.exists(g, "/var/log/installer")
    swap_rows = [line for line in vs.read(g, "/etc/fstab").splitlines()
                 if line.strip() and not line.lstrip().startswith(b"#") and b" swap " in line.replace(b"\t", b" ")]
    assert not swap_rows and not vs.exists(g, "/swap.img") and not vs.exists(g, "/swapfile")


def check_os(vs, g, selected):
    mounts, user = vs.layout(g, selected)
    vs.mount_all(g, mounts, "ro")
    vs.verify(g, user, vs.REMOVE + vs.SNAPD_WIPE)
    vs.snapd_unregistered(g)
    shadow = [r.split(b":") for r in vs.read(g, "/etc/shadow").splitlines()]
    assert shadow and all(len(r) == 9 and r[1] == b"!" for r in shadow)
    assert vs.read(g, "/etc/machine-id") == b"" and not g.glob_expand("/etc/ssh/ssh_host_*")
    assert not vs.exists(g, "/root/.ssh") and not vs.exists(g, "/home/" + user + "/.ssh")
    assert not vs.exists(g, "/var/lib/cloud")
    for path in ("/usr/bin/cloud-init", "/usr/sbin/sshd"):
        assert g.is_file(path), path
    assert any(g.is_file(p) for p in ("/usr/sbin/qemu-ga", "/usr/bin/qemu-ga"))
    for mount, device in mounts.items():
        if mount != "/boot/efi":
            assert "has_journal" in g.tune2fs_l(device)["Filesystem features"].split()


def main(argv):
    variant, value = parse(argv)
    assert os.getuid() == 1000 and not pathlib.Path("/dev/kvm").exists()
    for name in ("home", "tmp", "cache"):
        pathlib.Path("/scratch", name).mkdir(mode=0o700, exist_ok=True)
    os.environ.clear()
    os.environ.update(PATH="/usr/sbin:/usr/bin:/sbin:/bin", HOME="/scratch/home", TMPDIR="/scratch/tmp",
                      LIBGUESTFS_TMPDIR="/scratch/tmp", LIBGUESTFS_CACHEDIR="/scratch/cache",
                      LIBGUESTFS_BACKEND="direct", LIBGUESTFS_BACKEND_SETTINGS="force_tcg", LC_ALL="C")
    sys.path.insert(0, "/bootstrap")
    import vm_sanitize as vs
    import guestfs
    review = vs.load_review(pathlib.Path("/input/k8s-review.json").read_bytes()) if variant == "kubernetes" else None
    vs.image_info(SOURCE)  # Read-only qemu-img info/check, no repair.
    g = guestfs.GuestFS(python_return_dict=True)
    try:
        g.set_backend("direct")
        g.set_backend_setting("force_tcg", "true")
        g.set_network(False)
        g.add_drive_opts(SOURCE, format="qcow2", readonly=True)
        g.launch()
        if variant == "kubernetes":
            import k8s_copy_policy as k8s
            check_k8s(vs, k8s, g, review, value)
        else:
            check_os(vs, g, value)
        g.umount_all()
        g.shutdown()
    finally:
        g.close()
    sys.stdout.write(json.dumps(expected(variant, value), sort_keys=True) + "\n")
    sys.stdout.flush()
    os._exit(0)


def frames_only(exc):
    import traceback
    return {"phase": "probe_error", "error_class": type(exc).__name__,
            "frames": [[pathlib.Path(f.filename).name, f.lineno, f.name]
                       for f in traceback.extract_tb(exc.__traceback__)][-8:]}


if __name__ == "__main__":
    try:
        main(sys.argv[1:])
    except BaseException as exc:  # noqa: BLE001 - message-free: class + frames only
        sys.stdout.write(json.dumps(frames_only(exc), sort_keys=True) + "\n")
        sys.stdout.flush()
        os._exit(3)
