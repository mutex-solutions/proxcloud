#!/usr/bin/env python3
"""Offline native-OS sanitizer. Run ONLY in the separately reviewed offline capsule."""
import argparse
import hashlib
import json
import os
import re
import stat
import subprocess
import sys

SOURCE = "/input/source.qcow2"
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


def require(ok):
    if not ok:
        raise ValueError("sanitization refused")


def command(*args):
    result = subprocess.run(["/usr/bin/qemu-img", *args], check=True, stdin=subprocess.DEVNULL,
                            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, env=ENV, timeout=1800)
    require(len(result.stdout) <= 1 << 20)
    return result.stdout


def image_info(path):
    info = json.loads(command("info", "--output=json", "-f", "qcow2", path))
    require(type(info) is dict and info.get("format") == "qcow2")
    require(type(info.get("virtual-size")) is int and info["virtual-size"] == VIRTUAL_BYTES)

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
    inspect(info)  # Before qemu-img check/convert; never follow a backing or external file.
    command("check", "-f", "qcow2", path)
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
    return g.is_symlink(path) or g.exists(path)


def remove(g, path):
    if exists(g, path):
        if g.is_symlink(path):
            g.rm(path)
        else:
            g.rm_rf(path)


def read(g, path):
    safe(g, path)
    require(not g.is_symlink(path) and g.is_file(path, followsymlinks=False))
    require(g.filesize(path) <= 1 << 20)
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


def cleanup(g, user):
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
    for path in REMOVE:
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


def verify(g, homes_user):
    require(read(g, "/etc/machine-id") == b"" and read(g, "/etc/hostname") == b"localhost\n")
    for path in ("/etc/shadow", "/etc/gshadow"):
        require(all(row.split(b":")[1] == b"!" for row in read(g, path).splitlines()))
    for pattern in REMOVE:
        if pattern not in RECREATE:
            require(not g.glob_expand(pattern))
    for base in SCAN:
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
    verify(g, user)
    g.sync()
    g.umount_all()


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--os", required=True, choices=tuple(OS))
    args = parser.parse_args(argv)
    try:
        require(os.getuid() == os.geteuid() == 1000)
        os.umask(0o077)
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
            sanitize(g, args.os)
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
        facts = dict(kind="OfflineSanitizationFacts", os=args.os, variant="os", disk_sha256=digest.hexdigest(),
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
