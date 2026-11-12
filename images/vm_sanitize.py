#!/usr/bin/env python3
"""Offline native-OS/Kubernetes sanitizer. Run ONLY in the separately reviewed offline capsule."""
import argparse
import hashlib
import json
import os
import re
import stat
import subprocess
import sys

SOURCE = "/input/source.qcow2"
# variant=kubernetes ONLY: the template's RAW root disk. EFI vars and cloud-init media are
# never inputs. The parent-reviewed facts/drop-in decisions file is small trusted data.
RAW_SOURCE = "/input/source.raw"
K8S_REVIEW = "/input/k8s-review.json"
WORK = "/scratch/work/disk.qcow2"
OUTPUT = "/scratch/export/disk.qcow2"
FACTS = "/scratch/export/clean-export-facts.json"
OS = {"ubuntu-2404": ("ubuntu", 24, 4, "ubuntu"),
      "ubuntu-2604": ("ubuntu", 26, 4, "ubuntu"),
      "debian-12": ("debian", 12, 0, "debian"),
      "debian-13": ("debian", 13, 0, "debian")}
VIRTUAL_BYTES = 20 << 30  # Deliberately narrower than the artifact contract.
MAX_BYTES = 8 << 30
ENV = {"PATH": "/usr/sbin:/usr/bin:/sbin:/bin", "HOME": "/scratch", "TMPDIR": "/scratch",
       "LC_ALL": "C", "LIBGUESTFS_BACKEND": "direct", "LIBGUESTFS_BACKEND_SETTINGS": "force_tcg"}
BIOS_GUID = "21686148-6449-6e6f-744e-656564454649"
REMOVE = ("/var/lib/cloud", "/var/log/installer", "/var/lib/curtin", "/var/lib/subiquity",
          "/autoinstall.yaml", "/autoinstall.yml", "/root", "/var/log", "/var/cache",
          "/var/lib/apt/lists", "/tmp", "/var/tmp", "/var/lib/dhcp", "/var/lib/dhcpcd",
          "/var/lib/NetworkManager", "/var/lib/systemd/network", "/var/lib/systemd/random-seed",
          "/var/lib/dbus/machine-id", "/etc/ssh/ssh_host_*", "/etc/ssh/authorized_keys*",
          "/etc/netplan/*.yaml", "/etc/netplan/*.yml", "/etc/network/interfaces.d/*",
          "/etc/systemd/network/*", "/etc/NetworkManager/system-connections/*",
          "/etc/udev/rules.d/70-persistent-net.rules", "/etc/cloud/cloud-init.disabled",
          "/etc/cloud/cloud.cfg.d/*",  # Discard build overrides; keep packaged cloud.cfg/modules.
          "/etc/passwd-", "/etc/group-", "/etc/shadow-", "/etc/gshadow-",
          "/var/backups/*", "/etc/sudoers.d/*builder*", "/etc/sudoers.d/*packer*")
RECREATE = {"/root": 0o700, "/var/log": 0o755, "/var/cache": 0o755,
            "/var/lib/apt/lists": 0o755, "/tmp": 0o1777, "/var/tmp": 0o1777}
SCAN = ("/etc/cloud", "/etc/netplan", "/etc/network", "/etc/systemd/network",
        "/etc/NetworkManager", "/etc/ssh", "/etc/sudoers", "/etc/sudoers.d",
        "/boot/grub/grub.cfg", "/etc/default/grub")
BAD = re.compile(rb"10\.200\.|(?:BEGIN [A-Z0-9 ]*PRIVATE KEY)|(?:ssh-(?:rsa|ed25519) |ecdsa-sha2-)|(?:[0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}")
# --- variant=kubernetes only (Ubuntu 26.04 accepted template; see k8s_copy_policy) ---
K8S_OS = ("ubuntu-2604",)
# Exact retained, LOCKED local account. The cloud default user is upstream `ubuntu` (review key
# cloud_default_user == "ubuntu"): cloud-init creates it at first boot, so it must not pre-exist.
K8S_USER = "builder"
K8S_REVIEW_KEYS = {"kubernetes_version", "source_raw_sha256", "source_hash_verified", "source_stopped", "bios",
                   "kubelet_active", "runtime_version", "retained_package_policy_reviewed", "package_policy_sha256",
                   "network_backend", "cloud_default_user", "cloud_base_config_reviewed", "reviewed_dropins"}
K8S_PACKAGES = ("kubeadm", "kubelet", "kubectl")
K8S_DEB_RELEASE = "1.1"  # k8s-build-config.py pins kubernetes_deb_version = <version>-1.1.
# image-builder apt-holds kubeadm/kubelet/kubectl ("hold ok installed"); any other dpkg Status refuses.
K8S_DPKG_STATUS = (b"install ok installed", b"hold ok installed")
# curtin's fstab names root /dev/disk/by-uuid/<uuid> and inspect_get_mountpoints returns that spec verbatim.
# ONLY this exact canonical lowercase form is resolved (findfs_uuid); by-label/by-partuuid/other specs refuse.
K8S_BY_UUID = re.compile(r"/dev/disk/by-uuid/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})", re.ASCII)
KUBELET_WANTS = "/etc/systemd/system/multi-user.target.wants/kubelet.service"  # Preserved, never toggled.
KUBELET_UNITS = ("/usr/lib/systemd/system/kubelet.service", "/lib/systemd/system/kubelet.service")
KUBELET_MASK = "/etc/systemd/system/kubelet.service"  # Any symlink here (mask/redirect) refuses.
DROPINS = "/etc/cloud/cloud.cfg.d/"
README = "README"  # cloud-init's shipped notice: only via an exact reviewed (README, sha, action) row.
# Pre-policy refusals (counted, never wiped): the parent must learn the real state.
K8S_SWAP_FILES = ("/swap.img", "/swapfile")
K8S_SWAP_UNITS = ("/etc/systemd/system/*.swap",)
K8S_REGISTRY_FILES = ("/var/lib/kubelet/config.json", "/var/lib/kubelet/.dockercfg", "/.docker/config.json",
                      "/.dockercfg", "/root/.docker/config.json")
K8S_SECRET_PATHS = ("/var/lib/systemd/credential.secret",)
# snapd's per-machine device key (generated at image seeding): policy COPY-ONLY wipe of its entries, NOT a
# refusal; the directory must be a real directory with glob-visible entries and be empty after cleanup.
K8S_SNAPD_KEYS = "/var/lib/snapd/device/private-keys-v1"
K8S_PROXY_FILES = ("/etc/apt/apt.conf.d", "/etc/apt/apt.conf", "/etc/environment",
                   "/etc/systemd/system/containerd.service.d")
K8S_KUBELET_STATE = "/var/lib/kubelet"  # Scanned except pki/ and pods/ (cluster counts cover those).
# http_proxy=, Acquire::http::Proxy, nested `Acquire { http { Proxy "..." } }`, systemd Environment=, lab.
K8S_PROXY = re.compile(rb"(?i)_proxy\s*=|::proxy\b|\bproxy\s+\"|10\.200\.")
K8S_LAB = re.compile(rb"10\.200\.")
# ACTIVE fstab swap rows only. A row whose first non-blank byte is '#' is a comment that mount, swapon -a and
# systemd-fstab-generator ignore (image-builder's late-command comments the installer row: s/^#?/#/).
K8S_FSTAB_SWAP = re.compile(rb"(?m)^[ \t]*[^\s#]\S*[ \t]+\S+[ \t]+swap(?=[ \t]|$)")
# (guest path, depth): entries (incl. symlinks/dotfiles) are COUNTED, never followed or deleted.
# Any existing cluster/workload state refuses; it is not a wipe recommendation.
K8S_CLUSTER = {
    "kubernetes_pki": (("/etc/kubernetes/pki", 1),),
    "admin_conf": (("/etc/kubernetes/admin.conf", 1), ("/etc/kubernetes/super-admin.conf", 1),
                   ("/etc/kubernetes/controller-manager.conf", 1), ("/etc/kubernetes/scheduler.conf", 1)),
    "bootstrap_kubelet_conf": (("/etc/kubernetes/bootstrap-kubelet.conf", 1),),
    "kubelet_conf": (("/etc/kubernetes/kubelet.conf", 1),),
    "kubelet_pki": (("/var/lib/kubelet/pki", 1),),
    "etcd_db": (("/var/lib/etcd/member", 1),),
    "etcd_other": (("/var/lib/etcd", 1),),
    "static_manifests": (("/etc/kubernetes/manifests", 1),),  # Minus ONLY K8S_MANIFEST_KEEP, see k8s_cluster.
    "workloads": (("/var/lib/kubelet/pods", 1), ("/var/lib/containerd/io.containerd.runtime.v2.task", 2),
                  ("/var/lib/cni/networks", 1), ("/var/lib/cni/results", 1),
                  ("/var/log/pods", 1), ("/var/log/containers", 1)),
}
# image-builder keeps the static-manifest dir with this exact name as an EMPTY placeholder; not cluster state.
K8S_MANIFESTS, K8S_MANIFEST_KEEP = "/etc/kubernetes/manifests", ".kubelet-keep"
K8S_LINKS = ("/etc", "/etc/kubernetes", "/etc/containerd", "/etc/cloud", "/etc/cloud/cloud.cfg.d", "/home",
             "/home/" + K8S_USER, "/root", "/var/lib/kubelet", "/var/lib/etcd", "/var/lib/containerd",
             "/etc/passwd", "/etc/shadow", "/etc/fstab")
K8S_SCAN = ("/etc/kubernetes", "/etc/containerd", "/etc/crictl.yaml", "/etc/cni/net.d", "/etc/default/kubelet",
            "/etc/systemd/system/kubelet.service.d", "/etc/apt/sources.list", "/etc/apt/sources.list.d")
K8S_APT = re.compile(rb"://[^/\s]*@|10\.200\.")
# Includes containerd hosts.toml [host.*.header] authorization/bearer/basic credentials.
K8S_REGISTRY = re.compile(rb"(?i)\b(?:auth|password|identitytoken|registrytoken|authorization)\b[\"']?\s*[=:]"
                          rb"|\b(?:bearer|basic)\s+[A-Za-z0-9+/._~-]+=*[\"']")
K8S_CREDENTIAL = re.compile(rb"(?i)PRIVATE KEY-----|client-key-data|\btoken\s*[=:]")
NOLOGIN = ("/usr/sbin/nologin", "/sbin/nologin", "/bin/false")


def require(ok):
    if not ok:
        raise ValueError("sanitization refused")


def command(*args):
    result = subprocess.run(["/usr/bin/qemu-img", *args], check=True, stdin=subprocess.DEVNULL,
                            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, env=ENV, timeout=1800)
    require(len(result.stdout) <= 1 << 20)
    return result.stdout


def inspect(value):
    if isinstance(value, dict):
        for key, item in value.items():
            require("backing" not in key and "data-file" not in key)
            require("encrypt" not in key or (key == "encrypted" and item is False))
            require("snapshot" not in key or (key == "snapshots" and item == []))
            require(key != "corrupt" or item is False)
            inspect(item)
    elif isinstance(value, list):
        for item in value:
            inspect(item)


def image_info(path):
    info = json.loads(command("info", "--output=json", "-f", "qcow2", path))
    require(type(info) is dict and info.get("format") == "qcow2")
    require(type(info.get("virtual-size")) is int and info["virtual-size"] == VIRTUAL_BYTES)
    inspect(info)  # Before qemu-img check/convert; never follow a backing or external file.
    command("check", "-f", "qcow2", path)
    return info


def raw_info(path):
    """variant=kubernetes RAW root only, inside the capsule. qemu-img cannot check raw."""
    info = json.loads(command("info", "--output=json", "-f", "raw", path))
    require(type(info) is dict and info.get("format") == "raw")
    require(type(info.get("virtual-size")) is int and info["virtual-size"] == VIRTUAL_BYTES)
    inspect(info)  # Before conversion; an explicit -f raw never probes guest bytes as a format.
    return info


def safe(g, path):
    """Refuse symlink ancestors; unlinking a selected symlink leaf never follows it."""
    require(path.startswith("/") and ".." not in path.split("/") and "\x00" not in path)
    parts = path.strip("/").split("/")
    for i in range(1, len(parts)):
        require(not g.is_symlink("/" + "/".join(parts[:i])))
    return path


def exists(g, path):
    safe(g, path)
    return bool(g.is_symlink(path) or g.exists(path))  # GuestFS may return int 0/1; facts need bool.


def remove(g, path):
    if exists(g, path):
        if g.is_symlink(path):
            g.rm(path)
        else:
            g.rm_rf(path)


def read(g, path, limit=1 << 20):
    safe(g, path)
    require(not g.is_symlink(path) and g.is_file(path, followsymlinks=False))
    require(g.filesize(path) <= limit)
    return g.read_file(path)


def write(g, path, data):
    safe(g, path)
    require(not g.is_symlink(path))
    g.write(path, data)


def layout(g, selected):
    require(selected in OS and g.list_devices() == ["/dev/sda"] and not g.lvs())
    roots = g.inspect_os()
    require(len(roots) == 1)
    root = roots[0]
    distro, major, minor, user = OS[selected]
    require(g.inspect_get_type(root) == "linux" and g.inspect_get_distro(root) == distro)
    require(g.inspect_get_major_version(root) == major)
    require(distro == "debian" or g.inspect_get_minor_version(root) == minor)
    mounts = g.inspect_get_mountpoints(root)  # Python API returns mountpoint -> device.
    require(mounts.get("/") == root and set(mounts) <= {"/", "/boot", "/boot/efi"})
    require(len(set(mounts.values())) == len(mounts))
    filesystems = g.list_filesystems()
    for mount, device in mounts.items():
        require(re.fullmatch(r"/dev/sda[1-9][0-9]?", device) is not None)
        require(filesystems.get(device) == ("vfat" if mount == "/boot/efi" else "ext4"))
    for device, kind in filesystems.items():
        if device not in mounts.values():
            require(re.fullmatch(r"/dev/sda[1-9][0-9]?", device) is not None and kind == "unknown")
            require(g.part_get_gpt_type("/dev/sda", int(device[8:])).lower() == BIOS_GUID)
    return mounts, user


def mount_all(g, mounts):
    for path in sorted(mounts, key=lambda p: (p.count("/"), len(p))):
        if path != "/":
            safe(g, path)
            require(not g.is_symlink(path) and g.is_dir(path, followsymlinks=False))
        g.mount_options("", mounts[path], path)


def lock_table(g, path, fields):
    rows = read(g, path).decode("utf-8").splitlines()
    require(rows)
    result = []
    for row in rows:
        cols = row.split(":")
        require(len(cols) == fields and re.fullmatch(r"[a-zA-Z0-9_.-]+\$?", cols[0]))
        cols[1] = "!"  # Replace, not prepend: no recoverable hash in active account tables.
        result.append(":".join(cols))
    write(g, path, ("\n".join(result) + "\n").encode())


def cleanup(g, user, remove_patterns=REMOVE):
    passwd = read(g, "/etc/passwd").decode("utf-8").splitlines()
    users = [row.split(":") for row in passwd]
    require(all(len(row) == 7 and row[1] in ("x", "!", "*", "!!")
                and row[2].isdigit() and re.fullmatch(r"/[a-zA-Z0-9_./-]*", row[5])
                and ".." not in row[5].split("/") for row in users) and any(row[0] == user for row in users))
    for row in users:
        for name in (".ssh", ".bash_history", ".zsh_history", ".python_history", ".lesshst", ".wget-hsts"):
            for found in g.glob_expand(row[5].rstrip("/") + "/" + name):
                remove(g, found)
    homes = {row[5] for row in users if row[5] == "/root" or row[5].startswith("/home/")}
    # Unexpected non-system login homes are not widened into arbitrary deletion paths.
    require(all(row[5] in homes or int(row[2]) < 1000 or row[6].endswith(("/nologin", "/false")) for row in users))
    for path in remove_patterns:
        for found in g.glob_expand(path):
            remove(g, found)
    # Retain home directories/ownership and native cloud account; discard ALL personal state.
    safe(g, "/home/sentinel")
    if g.is_dir("/home", followsymlinks=False):
        homes.update("/home/" + name for name in g.ls("/home"))
    for home in homes - {"/root"}:
        safe(g, home)
        if not exists(g, home):
            continue
        require(not g.is_symlink(home) and g.is_dir(home, followsymlinks=False))
        for name in g.ls(home):
            remove(g, home + "/" + name)
    for path, mode in RECREATE.items():
        safe(g, path)
        require(not g.is_symlink(path))
        g.mkdir_p(path)
        g.chown(0, 0, path)
        g.chmod(mode, path)
    for path in g.glob_expand("/etc/sudoers.d/*"):
        if not g.is_symlink(safe(g, path)) and re.search(rb"\b(?:builder|packer)\b", read(g, path)):
            remove(g, path)
    # Refuse a build-account rule in the main sudoers rather than rewriting policy blindly.
    require(not re.search(rb"\b(?:builder|packer)\b", read(g, "/etc/sudoers")))
    lock_table(g, "/etc/shadow", 9)
    lock_table(g, "/etc/gshadow", 4)
    write(g, "/etc/machine-id", b"")
    write(g, "/etc/hostname", b"localhost\n")
    write(g, "/etc/hosts", b"127.0.0.1 localhost\n::1 localhost ip6-localhost ip6-loopback\n")
    safe(g, "/etc/network/interfaces")
    g.mkdir_p("/etc/network")
    write(g, "/etc/network/interfaces", b"auto lo\niface lo inet loopback\n")
    remove(g, "/etc/resolv.conf")
    g.ln_s("../run/systemd/resolve/stub-resolv.conf", "/etc/resolv.conf")
    # Remove boot-command-line network/cloud-init disable overrides by refusal, never guest execution.
    for path in ("/etc/default/grub", "/boot/grub/grub.cfg"):
        if exists(g, path):
            require(not re.search(rb"(?:cloud-init=disabled|\bip=|\bds=)", read(g, path)))


def verify(g, homes_user, remove_patterns=REMOVE, scan=SCAN):
    require(read(g, "/etc/machine-id") == b"" and read(g, "/etc/hostname") == b"localhost\n")
    for path in ("/etc/shadow", "/etc/gshadow"):
        require(all(row.split(b":")[1] == b"!" for row in read(g, path).splitlines()))
    for pattern in remove_patterns:
        if pattern not in RECREATE:
            require(not g.glob_expand(pattern))
    for base in scan:
        if not exists(g, base):
            continue
        paths = [base] if g.is_file(base, followsymlinks=False) else [base + "/" + p for p in g.find(base)]
        require(len(paths) <= 20000)
        for path in paths:
            safe(g, path)
            if not g.is_symlink(path) and g.is_file(path, followsymlinks=False):
                require(not BAD.search(read(g, path)))
    passwd = [row.split(b":") for row in read(g, "/etc/passwd").splitlines()]
    require(all(len(row) == 7 and row[1] in (b"x", b"!", b"*", b"!!") for row in passwd))
    require(any(row[0] == homes_user.encode() for row in passwd))


def journal(g, device, enabled):
    require(re.fullmatch(r"/dev/sda[1-9][0-9]?", device) is not None)
    # Trusted APPLIANCE tool, with target filesystems unmounted; NOT guestfs.command/sh.
    # tune2fs(features=...) does not exist in Bookworm's Python API. Probe debug sh first.
    feature = "has_journal" if enabled else "^has_journal"
    g.debug("sh", ["/usr/sbin/tune2fs -O " + feature + " " + device])
    require(("has_journal" in g.tune2fs_l(device)["Filesystem features"].split()) is enabled)
    g.e2fsck(device, correct=True)


def sanitize(g, selected):
    mounts, user = layout(g, selected)
    mount_all(g, mounts)
    cleanup(g, user)
    verify(g, user)
    reset_and_zero(g, mounts, lambda: verify(g, user))


def reset_and_zero(g, mounts, final_check):
    g.sync()
    g.umount_all()
    ext4 = [dev for path, dev in mounts.items() if path != "/boot/efi"]
    for device in ext4:
        journal(g, device, False)
    mount_all(g, mounts)
    for path in sorted(mounts, reverse=True):
        g.zero_free_space(path)  # Actual writes, including freed old journal blocks, not trim/compression.
    g.sync()
    g.umount_all()
    for device in ext4:
        journal(g, device, True)
    mount_all(g, mounts)
    final_check()
    g.sync()
    g.umount_all()


# ---------------------------------------------------------------------------------------
# variant=kubernetes: accepted Ubuntu 26.04 / Kubernetes 1.35.10 node template, RAW root.
# Every fact feeds k8s_copy_policy.validate_inventory, which refuses unknown/mismatched
# facts. Nothing here toggles kubelet enablement or rewrites the policy's facts.
# ---------------------------------------------------------------------------------------
def load_review(raw):
    """Parent-reviewed small JSON (never guest data). Exact keys, duplicate keys refused."""
    def unique(pairs):
        require(len(dict(pairs)) == len(pairs))
        return dict(pairs)
    require(type(raw) is bytes and 0 < len(raw) <= 65536)
    review = json.loads(raw.decode("ascii"), object_pairs_hook=unique)
    require(type(review) is dict and set(review) == K8S_REVIEW_KEYS)
    require(type(review["source_raw_sha256"]) is str and
            re.fullmatch(r"[0-9a-f]{64}", review["source_raw_sha256"], re.ASCII) is not None)
    rows = review["reviewed_dropins"]
    require(type(rows) is list and all(type(row) is list and len(row) == 3 for row in rows))
    review["reviewed_dropins"] = tuple(tuple(row) for row in rows)
    return review


def open_private(path, limit):
    """O_NOFOLLOW open + fstat: one regular single-link file; returns (file, stat)."""
    handle = os.fdopen(os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK), "rb")
    info = os.fstat(handle.fileno())
    if not (stat.S_ISREG(info.st_mode) and info.st_nlink == 1 and 0 < info.st_size <= limit):
        handle.close()
        require(False)
    return handle, info


def raw_digest(path, expected_size):
    """Stream-hash the exact raw root INSIDE the capsule; bounded, stable-file checked."""
    handle, before = open_private(path, expected_size)
    with handle:
        require(before.st_size == expected_size)
        size, digest = 0, hashlib.sha256()
        for block in iter(lambda: handle.read(1 << 20), b""):
            size += len(block)
            require(size <= expected_size)
            digest.update(block)
        after = os.fstat(handle.fileno())
    require(size == expected_size and identity(before) == identity(after))
    return digest.hexdigest(), identity(before)


def identity(info):
    """File identity bound to a hash: later path reopens must still name exactly these bytes."""
    return (info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns, info.st_ctime_ns)


def k8s_root_device(g, spec, root):
    """The root fs spec as an inspected device: verbatim, or ONLY exact canonical by-uuid -> findfs_uuid.

    Returns the device that is then mounted (never the symlink-style spec). Any other spec form
    (by-label, by-partuuid, LABEL=, upper-case/odd uuid), an unresolvable uuid or a uuid
    resolving to anything but the single inspected root refuses."""
    require(type(spec) is str and type(root) is str)
    if spec == root:
        return root
    match = K8S_BY_UUID.fullmatch(spec)
    require(match is not None)
    try:
        device = g.findfs_uuid(match.group(1))  # Filesystem uuid (blkid -U), never a partuuid.
    except Exception:  # Unresolvable: refuse with the fixed text, never the tool's message.
        device = None
    require(type(device) is str and device == root)
    return device


def k8s_layout(g, selected, review):
    """Facts for policy.layout; the policy (not this probe) decides acceptability."""
    require(selected in K8S_OS and g.list_devices() == ["/dev/sda"] and not g.lvs())
    roots = g.inspect_os()
    require(len(roots) == 1 and g.inspect_get_type(roots[0]) == "linux")
    root = roots[0]
    mounts = g.inspect_get_mountpoints(root)
    filesystems = g.list_filesystems()
    parts = g.part_list("/dev/sda")
    # Root-only before ANY mount: no /boot, ESP or extra mountpoint is ever mounted here. Later
    # mount_all/reset_and_zero only ever see this resolved {"/": <inspected root device>} mapping.
    require(type(mounts) is dict and sorted(mounts) == ["/"])
    mounts = {"/": k8s_root_device(g, mounts["/"], root)}
    require(root in filesystems)
    os_facts = {"distro": g.inspect_get_distro(root), "major": g.inspect_get_major_version(root),
                "minor": g.inspect_get_minor_version(root)}
    facts = {"bios": review["bios"], "partition_table": g.part_get_parttype("/dev/sda"),
             "partition_count": len(parts),
             "root_partition_number": int(root[8:]) if re.fullmatch(r"/dev/sda[1-9][0-9]?", root) else 0,
             "root_fs": filesystems[root], "root_count": len(roots), "mountpoints": sorted(mounts),
             "esp_count": sum(kind == "vfat" for kind in filesystems.values()),
             "extra_filesystems": sum(dev != root for dev in filesystems), "external_fstab": None,
             "unsafe_links": 0}
    return mounts, os_facts, facts


def k8s_count(g, path, depth):
    """Count entries without following links; any existing link/leaf counts as state."""
    if not exists(g, path):
        return 0
    if g.is_symlink(path) or not g.is_dir(path, followsymlinks=False):
        return 1
    names = g.ls(path)
    if depth <= 1:
        return len(names)
    return sum(k8s_count(g, path + "/" + name, depth - 1) for name in names)


def k8s_files(g, bases, limit=20000):
    found = []
    for base in bases:
        if not exists(g, base):
            continue
        paths = [base] if not g.is_dir(base, followsymlinks=False) else [base + "/" + p for p in g.find(base)]
        found += [p for p in paths if not g.is_symlink(safe(g, p)) and g.is_file(p, followsymlinks=False)]
        require(len(found) <= limit)
    return found


def k8s_marker(value, allowed):
    return value if value in allowed else "UNKNOWN"  # Never forward hash bytes to the policy.


def k8s_accounts(g):
    shadow = {row.split(b":")[0]: row.split(b":") for row in read(g, "/etc/shadow").splitlines()}
    accounts = []
    for row in read(g, "/etc/passwd").decode("utf-8").splitlines():
        cols = row.split(":")
        require(len(cols) == 7 and cols[2].isdigit())
        name, uid, home, shell = cols[0], int(cols[2]), cols[5], cols[6]
        kind = ("root" if name == "root" else "native_login" if name == K8S_USER
                else "system" if shell in NOLOGIN or (name, uid, home, shell) == ("sync", 4, "/bin", "/bin/sync")
                else "UNKNOWN")
        entry = shadow.get(name.encode(), [b"", b"UNKNOWN"])
        require(len(entry) == 9)
        accounts.append({"name": name, "uid": uid, "home": home, "shell": shell, "kind": kind,
                         "passwd_marker": k8s_marker(cols[1], ("x", "!", "*", "!!")),
                         "shadow_marker": k8s_marker(entry[1].decode("ascii", "replace"), ("!", "!!", "*", "!*"))})
    return accounts


def k8s_packages(g):
    """Normalize ONLY the exact pinned deb revision; anything else stays raw and refuses.

    Status must be exactly installed, either unheld or apt-held (image-builder holds these)."""
    found = {}
    for stanza in read(g, "/var/lib/dpkg/status", 64 << 20).split(b"\n\n"):
        fields = dict(line.split(b": ", 1) for line in stanza.splitlines() if b": " in line and not line[:1].isspace())
        name = fields.get(b"Package", b"").decode("ascii", "replace")
        if name in K8S_PACKAGES:
            require(name not in found and fields.get(b"Status") in K8S_DPKG_STATUS)
            version = fields.get(b"Version", b"").decode("ascii", "replace")
            found[name] = version[:-len("-" + K8S_DEB_RELEASE)] if version.endswith("-" + K8S_DEB_RELEASE) else "UNKNOWN"
    return found


def kubelet_enabled(g):
    """Observed only: wants-link to a present kubelet unit; masked/redirected/absent is False."""
    if exists(g, KUBELET_MASK) and g.is_symlink(KUBELET_MASK):
        return False  # /etc/systemd/system/kubelet.service -> /dev/null (masked) or a redirect.
    if not g.is_symlink(KUBELET_WANTS):
        return False
    target = g.readlink(KUBELET_WANTS)
    # GuestFS bool-ish returns may be int 0/1: the policy requires an exact bool.
    return bool(type(target) is str and target != "/dev/null" and target.rsplit("/", 1)[-1] == "kubelet.service" and
                any(exists(g, unit) and not g.is_symlink(unit) for unit in KUBELET_UNITS))


def k8s_cluster(g):
    """Cluster-state counts; static_manifests excludes ONLY a 0-byte regular non-symlink .kubelet-keep."""
    counts = {key: sum(k8s_count(g, path, depth) for path, depth in paths) for key, paths in K8S_CLUSTER.items()}
    keep = K8S_MANIFESTS + "/" + K8S_MANIFEST_KEEP
    if (exists(g, K8S_MANIFESTS) and not g.is_symlink(K8S_MANIFESTS) and g.is_dir(K8S_MANIFESTS, followsymlinks=False)
            and K8S_MANIFEST_KEEP in g.ls(K8S_MANIFESTS) and not g.is_symlink(safe(g, keep))
            and g.is_file(keep, followsymlinks=False) and g.filesize(keep) == 0):
        counts["static_manifests"] -= 1
    return counts


def k8s_inventory(g, selected, review, parent):
    mounts, os_facts, layout_facts = k8s_layout(g, selected, review)
    mount_all(g, mounts)
    layout_facts["unsafe_links"] = sum(g.is_symlink(path) for path in K8S_LINKS if g.exists(path) or g.is_symlink(path))
    require(layout_facts["unsafe_links"] == 0)  # Before any further guest path is read.
    fstab = read(g, "/etc/fstab")
    rows = [line.split() for line in fstab.splitlines() if line.strip() and not line.lstrip().startswith(b"#")]
    layout_facts["external_fstab"] = any(len(row) < 2 or row[1] != b"/" for row in rows)
    # Swap is refused, never wiped, so the parent learns the real state of the template. Only ACTIVE
    # fstab rows count; a commented row is inert. Swap files and *.swap units always count.
    layout_facts["swap_entries"] = int(sum(bool(exists(g, path)) for path in K8S_SWAP_FILES) +
                                       len(K8S_FSTAB_SWAP.findall(fstab)) +
                                       sum(len(g.glob_expand(unit)) for unit in K8S_SWAP_UNITS))
    backend = ("netplan-systemd-networkd" if g.exists("/usr/sbin/netplan") and
               g.exists("/usr/lib/systemd/system/systemd-networkd.service") and
               not g.exists("/usr/sbin/NetworkManager") else "UNKNOWN")
    # Upstream default user `ubuntu` is created by cloud-init at first boot: no pre-existing home or group
    # (a passwd row is the policy's refusal). useradd would refuse an existing group of the same name.
    require(not exists(g, "/home/ubuntu") and
            not any(row.split(b":")[0] == b"ubuntu" for row in read(g, "/etc/group").splitlines()))
    dropins = []
    if exists(g, DROPINS.rstrip("/")):
        for name in g.ls(DROPINS.rstrip("/")):
            path = DROPINS + name
            # GuestFS returns is_file as int 0/1: the policy requires an exact bool.
            regular = bool(not g.is_symlink(safe(g, path)) and g.is_file(path, followsymlinks=False))
            digest = hashlib.sha256(read(g, path)).hexdigest() if regular else "0" * 64
            dropins.append({"name": name, "sha256": digest, "regular_no_symlinks": regular})
    tokens = sorted(set(g.glob_expand("/etc/kubernetes/*token*") + g.glob_expand("/var/lib/kubelet/*token*")))
    apt = k8s_files(g, ("/etc/apt/sources.list", "/etc/apt/sources.list.d"))
    proxies = k8s_files(g, K8S_PROXY_FILES)
    containerd = k8s_files(g, ("/etc/containerd",))
    # Curated token leaves are removal targets; every other scanned file must be clean.
    scanned = [p for p in k8s_files(g, K8S_SCAN) if p not in tokens]
    kubelet = [p for p in k8s_files(g, (K8S_KUBELET_STATE,)) if p not in tokens and
               not p.startswith((K8S_KUBELET_STATE + "/pki/", K8S_KUBELET_STATE + "/pods/"))]
    facts = {
        "parent": parent, "os": os_facts, "layout": layout_facts, "accounts": k8s_accounts(g),
        "firstboot": {"network_backend": backend if backend == review["network_backend"] else "UNKNOWN",
                      "cloud_default_user": review["cloud_default_user"],
                      "cloud_base_config_reviewed": review["cloud_base_config_reviewed"]},
        "k8s": {"version": review["kubernetes_version"],
                "kubelet_enabled": kubelet_enabled(g),  # Observed, preserved, never toggled.
                "kubelet_active": review["kubelet_active"], "runtime_version": review["runtime_version"],
                "cluster": k8s_cluster(g),
                "bootstrap_token_files": len(tokens), "kubernetes_packages": k8s_packages(g),
                "retained_package_policy_reviewed": review["retained_package_policy_reviewed"],
                "package_policy_sha256": review["package_policy_sha256"]},
        "cloud_dropins": dropins,
        "hygiene": {
            "unsafe_apt_repo_urls": (sum(bool(K8S_APT.search(read(g, p))) for p in apt) +
                                     sum(bool(K8S_PROXY.search(read(g, p))) for p in proxies)),
            "registry_config_credentials": (sum(bool(K8S_REGISTRY.search(read(g, p))) for p in containerd) +
                                            sum(exists(g, p) for p in K8S_REGISTRY_FILES)),
            "unclassified_credentials": (sum(bool(K8S_CREDENTIAL.search(read(g, p))) for p in scanned) +
                                         sum(bool(K8S_CREDENTIAL.search(data) or K8S_LAB.search(data))
                                             for data in (read(g, p) for p in kubelet)) +
                                         sum(k8s_count(g, p, 1) for p in K8S_SECRET_PATHS) +
                                         k8s_snapd_unwipeable(g))},
    }
    return mounts, facts, tokens


def k8s_snapd_unwipeable(g):
    """snapd device-key entries the policy's `<dir>/*` COPY-ONLY wipe would NOT cleanly remove.

    Regular non-symlink, non-dot files are wiped (not counted). A non-directory/symlinked key dir,
    dot entries (not glob-matched) and links/dirs/specials still count as unclassified credentials."""
    if not exists(g, K8S_SNAPD_KEYS):
        return 0
    if g.is_symlink(K8S_SNAPD_KEYS) or not g.is_dir(K8S_SNAPD_KEYS, followsymlinks=False):
        return 1
    path = K8S_SNAPD_KEYS + "/"
    return sum(name.startswith(".") or g.is_symlink(safe(g, path + name)) or
               not g.is_file(path + name, followsymlinks=False) for name in g.ls(K8S_SNAPD_KEYS))


def k8s_state(g):
    """Retained K8s surface re-observed after cleanup: enablement/packages/cluster must not drift."""
    return (kubelet_enabled(g), k8s_packages(g), k8s_cluster(g))


def sanitize_k8s(g, selected, review, parent):
    import k8s_copy_policy as k8s  # Lazy: the native-OS path keeps its exact dependency set.
    # source_raw_sha256 is verified in-capsule by main(); source_stopped stays a parent attestation.
    require(type(parent) is dict and all(review[key] is True and parent.get(key) is True
                                         for key in ("source_hash_verified", "source_stopped")))
    mounts, facts, tokens = k8s_inventory(g, selected, review, parent)
    plan = k8s.validate_inventory(facts, review["reviewed_dropins"])  # Refusal before ANY write.
    require(plan.copy_only and not plan.public_eligible)
    for path in tokens:  # Curated reviewed prejoin leaves only; never directories/cluster state.
        require(not g.is_symlink(safe(g, path)) and g.is_file(path, followsymlinks=False))
    decided = {name: action for name, _, action in plan.reviewed_dropins}
    preserved = {name: digest for name, digest, action in plan.reviewed_dropins if action == "preserve_vendor"}
    # Replace (not extend) the blanket cloud.cfg.d wipe with explicit reviewed decisions.
    patterns = (tuple(p for p in REMOVE if p != DROPINS + "*") + plan.wipe + tuple(tokens) +
                tuple(DROPINS + name for name, action in sorted(decided.items()) if action == "remove_build"))
    before = k8s_state(g)

    def check():
        verify(g, K8S_USER, patterns, SCAN + K8S_SCAN)
        require(sorted(g.ls(DROPINS.rstrip("/"))) == sorted(preserved))
        for name, digest in preserved.items():
            require(hashlib.sha256(read(g, DROPINS + name)).hexdigest() == digest)
        require(k8s_state(g) == before)
        # snapd regenerates its device key on first boot: the copied key directory must be EMPTY.
        require(not exists(g, K8S_SNAPD_KEYS) or (not g.is_symlink(K8S_SNAPD_KEYS) and g.ls(K8S_SNAPD_KEYS) == []))
        # Locked builder: no sudo grant survives anywhere (a symlinked sudoers.d entry refuses in read()).
        for path in ["/etc/sudoers"] + g.glob_expand("/etc/sudoers.d/*"):
            require(not re.search(rb"\b(?:builder|packer)\b", read(g, path)))
    cleanup(g, K8S_USER, patterns)
    check()
    reset_and_zero(g, mounts, check)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    selector = parser.add_mutually_exclusive_group(required=True)
    selector.add_argument("--os", choices=tuple(OS))
    selector.add_argument("--k8s-os", choices=K8S_OS)  # variant=kubernetes; RAW root input only.
    args = parser.parse_args(argv)
    kubernetes = args.k8s_os is not None
    selected = args.k8s_os if kubernetes else args.os
    try:
        require(os.getuid() == os.geteuid() == 1000)
        os.umask(0o077)
        if kubernetes:
            handle, _ = open_private(K8S_REVIEW, 65536)  # O_NOFOLLOW + fstat; parent-reviewed data.
            with handle:
                review = load_review(handle.read(65537))
            require(review["kubernetes_version"] == "1.35.10")
            # Exactly the 20 GiB raw root; EFI vars and cloud-init media are never inputs.
            # The digest is computed HERE, streamed and bounded, and must equal the parent's
            # independently recorded source hash; a mismatch refuses before any parser runs.
            digest, hashed = raw_digest(RAW_SOURCE, VIRTUAL_BYTES)
            require(digest == review["source_raw_sha256"])
            os.mkdir("/scratch/work", 0o700)
            os.mkdir("/scratch/export", 0o700)
            info = raw_info(RAW_SOURCE)
            # Confined conversion to a standalone QCOW2 work copy; the raw input stays read-only.
            command("convert", "-f", "raw", "-O", "qcow2", "-o", "compat=1.1,lazy_refcounts=off", RAW_SOURCE, WORK)
            image_info(WORK)
            raw_after = os.lstat(RAW_SOURCE)
            # The path qemu-img reopened must still be the exact hashed file (no swap or rewrite).
            require(stat.S_ISREG(raw_after.st_mode) and identity(raw_after) == hashed)
            parent = {"format": "raw", "virtual_bytes": info["virtual-size"], "raw_bytes": raw_after.st_size,
                      # In-capsule hash matched; the stopped-source fact remains parent-attested.
                      "source_hash_verified": review["source_hash_verified"] is True,
                      "source_stopped": review["source_stopped"]}
        else:
            source = os.lstat(SOURCE)
            require(stat.S_ISREG(source.st_mode) and source.st_nlink == 1 and 0 < source.st_size <= 24 << 30)
            os.mkdir("/scratch/work", 0o700)
            os.mkdir("/scratch/export", 0o700)
            image_info(SOURCE)
            command("convert", "-f", "qcow2", "-O", "qcow2", "-o", "compat=1.1,lazy_refcounts=off", SOURCE, WORK)
        os.environ.clear()
        os.environ.update(ENV)
        import guestfs  # Trusted capsule binding only; pure tests never need this dependency.
        g = guestfs.GuestFS(python_return_dict=True)
        try:
            g.set_backend("direct")
            g.set_backend_setting("force_tcg", "true")
            g.set_network(False)
            g.add_drive_opts(WORK, format="qcow2", readonly=0, discard="enable")
            g.launch()
            if kubernetes:
                sanitize_k8s(g, selected, review, parent)
            else:
                sanitize(g, selected)
            g.shutdown()
        finally:
            g.close()
        command("convert", "-f", "qcow2", "-O", "qcow2", "-c", "-o", "compat=1.1,lazy_refcounts=off", WORK, OUTPUT)
        image_info(OUTPUT)
        size, digest = 0, hashlib.sha256()
        with open(OUTPUT, "rb") as disk:
            for block in iter(lambda: disk.read(1 << 20), b""):
                size += len(block)
                require(size < MAX_BYTES)
                digest.update(block)
        extra = dict(variant="kubernetes", kubernetes_version="1.35.10") if kubernetes else dict(variant="os")
        facts = dict(kind="OfflineSanitizationFacts", os=selected, **extra, disk_sha256=digest.hexdigest(),
                     disk_bytes=size, virtual_size_bytes=VIRTUAL_BYTES, checks={key: True for key in
                     ("cleaned_copy_only", "no_backing_file", "qemu_img_check", "offline_sanitization",
                      "zero_free_space", "no_build_credentials_or_lab_network")})
        require(0 < size < MAX_BYTES)
        with open(FACTS, "x", encoding="ascii") as out:
            json.dump(facts, out, sort_keys=True)
            out.write("\n")
        print(json.dumps(facts, sort_keys=True))
        return 0
    except Exception:  # Never emit guest paths/data, hashes, tool stderr or exception text on failure.
        print("sanitization refused", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
