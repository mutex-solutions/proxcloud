"""Source-only, NOT run. Pure in-memory GuestFS fake; no commands, disk images or appliances.

These tests prove policy/call ordering only, never actual libguestfs or journal erasure.
"""
import contextlib
import copy
import fnmatch
import importlib.util
import io
from pathlib import Path
import hashlib
import json
import os
import re
import sys
import tempfile
import unittest
from unittest.mock import patch

IMAGES = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(IMAGES))  # vm_sanitize lazily imports the pure k8s_copy_policy sibling.
SPEC = importlib.util.spec_from_file_location("vm_sanitize", IMAGES / "vm_sanitize.py")
s = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(s)
import k8s_copy_policy  # noqa: E402  Pure policy; no IO.


class FakeGuestFS:
    """A deliberately small offline FILE API fake; no target-command API exists."""
    def __init__(self, selected="ubuntu-2604"):
        self.nodes = {"/": ("dir", None)}
        self.calls, self.modes = [], {}
        self.distro, self.major, self.minor, self.user = s.OS[selected]
        self.devices, self.volumes = ["/dev/sda"], []
        self.mounts = {"/": "/dev/sda1", "/boot": "/dev/sda13", "/boot/efi": "/dev/sda15"}
        self.filesystems = {"/dev/sda1": "ext4", "/dev/sda13": "ext4", "/dev/sda14": "unknown", "/dev/sda15": "vfat"}
        self.features = {dev: "has_journal extents" for dev in ("/dev/sda1", "/dev/sda13")}
        self.mounted = set()
        self.guid = s.BIOS_GUID
        for path in ("/boot/efi", "/etc/cloud/cloud.cfg.d", "/etc/netplan", "/etc/network/interfaces.d",
                     "/etc/systemd/network", "/etc/NetworkManager/system-connections", "/etc/ssh",
                     "/etc/sudoers.d", "/etc/default", "/boot/grub", "/root/.ssh", "/home/" + self.user + "/.ssh",
                     "/home/builder", "/var/log/installer", "/var/cache/apt", "/var/lib/cloud/seed/nocloud",
                     "/var/lib/apt/lists", "/var/lib/systemd", "/var/lib/dbus", "/tmp", "/var/tmp", "/var/backups"):
            self.mkdir_p(path)
        self.put("/etc/passwd", ("root:x:0:0:root:/root:/bin/bash\n" + self.user + ":x:1000:1000::/home/" + self.user +
                                  ":/bin/bash\nbuilder:x:1001:1001::/home/builder:/bin/bash\n").encode())
        self.put("/etc/shadow", b"root:$6$OLD:1:2:3:4:5:6:7\nbuilder:!!$6$OLD:1:2:3:4:5:6:7\n" +
                 self.user.encode() + b":$6$OLD:1:2:3:4:5:6:7\n")
        self.put("/etc/gshadow", b"root:$6$GROUP::\nsudo:*::builder\n")
        self.put("/etc/group", b"root:x:0:\nsudo:x:27:builder\n")
        self.put("/etc/sudoers", b"root ALL=(ALL:ALL) ALL\n%sudo ALL=(ALL:ALL) ALL\n")
        self.put("/etc/sudoers.d/90-cloud", b"builder ALL=(ALL) NOPASSWD:ALL\n")
        self.put("/etc/cloud/cloud.cfg", b"users: [default]\n")
        self.put("/etc/cloud/cloud.cfg.d/99-disable-network-config.cfg", b"network: {config: disabled}\n")
        for path in ("/root/.ssh/id_ed25519", "/root/.bash_history", "/home/" + self.user + "/.ssh/authorized_keys",
                     "/home/" + self.user + "/.bash_history", "/home/builder/.bash_history", "/var/log/auth.log",
                     "/var/log/installer/autoinstall-user-data", "/var/lib/cloud/seed/nocloud/user-data", "/autoinstall.yaml",
                     "/etc/netplan/50-cloud-init.yaml", "/etc/ssh/ssh_host_ed25519_key", "/etc/shadow-",
                     "/var/lib/systemd/random-seed", "/var/lib/dbus/machine-id", "/var/backups/passwd.bak"):
            self.put(path, b"PRIVATE SYNTHETIC LAB 10.200.1.2\n")
        self.put("/etc/machine-id", b"old identity\n")
        self.calls.clear()

    def mkdir_p(self, path):
        current = ""
        for part in path.strip("/").split("/"):
            current += "/" + part
            self.nodes.setdefault(current, ("dir", None))
    def put(self, path, data):
        self.mkdir_p(path.rsplit("/", 1)[0] or "/")
        self.nodes[path] = ("file", data)
    def is_symlink(self, path): return self.nodes.get(path, (None,))[0] == "link"
    def exists(self, path): return path in self.nodes
    def is_file(self, path, followsymlinks=True): return self.nodes.get(path, (None,))[0] == "file"
    def is_dir(self, path, followsymlinks=True): return self.nodes.get(path, (None,))[0] == "dir"
    def filesize(self, path): return len(self.nodes[path][1])
    def read_file(self, path): return self.nodes[path][1]
    def write(self, path, data):
        assert self.is_dir(path.rsplit("/", 1)[0])
        assert not self.is_symlink(path)
        self.nodes[path] = ("file", data)
    def glob_expand(self, pattern):
        return sorted(p for p in self.nodes if p.count("/") == pattern.count("/") and fnmatch.fnmatchcase(p, pattern))
    def ls(self, path):
        prefix = path.rstrip("/") + "/"
        return sorted(p[len(prefix):] for p in self.nodes if p.startswith(prefix) and "/" not in p[len(prefix):])
    def find(self, path):
        prefix = path + "/"
        return sorted(p[len(prefix):] for p in self.nodes if p.startswith(prefix))
    def rm(self, path):
        self.calls.append(("rm", path))
        del self.nodes[path]
    def rm_rf(self, path):
        self.calls.append(("rm_rf", path))
        for found in list(self.nodes):
            if found == path or found.startswith(path + "/"):
                del self.nodes[found]
    def chown(self, uid, gid, path): self.calls.append(("chown", uid, gid, path))
    def chmod(self, mode, path): self.modes[path] = mode
    def ln_s(self, target, path): self.nodes[path] = ("link", target)
    def list_devices(self): return self.devices
    def lvs(self): return self.volumes
    def inspect_os(self): return ["/dev/sda1"]
    def inspect_get_type(self, root): return "linux"
    def inspect_get_distro(self, root): return self.distro
    def inspect_get_major_version(self, root): return self.major
    def inspect_get_minor_version(self, root): return self.minor
    def inspect_get_mountpoints(self, root): return self.mounts
    def list_filesystems(self): return self.filesystems
    def part_get_gpt_type(self, device, part): return self.guid
    def mount_options(self, options, device, path):
        self.calls.append(("mount", device, path))
        self.mounted.add(device)
    def sync(self): self.calls.append(("sync",))
    def umount_all(self):
        self.calls.append(("umount_all",))
        self.mounted.clear()
    def debug(self, subcmd, extraargs):
        assert not self.mounted and subcmd == "sh" and len(extraargs) == 1
        match = re.fullmatch(r"/usr/sbin/tune2fs -O (\^?has_journal) (/dev/sda[1-9][0-9]?)", extraargs[0])
        assert match is not None
        feature, device = match.groups()
        self.features[device] = "extents" if feature.startswith("^") else "has_journal extents"
        self.calls.append(("journal", device, not feature.startswith("^")))
        return ""
    def tune2fs_l(self, device): return {"Filesystem features": self.features[device]}
    def e2fsck(self, device, *, correct=None):
        assert correct is True and not self.mounted
        self.calls.append(("e2fsck", device))
    def zero_free_space(self, directory):
        assert self.mounted and all("has_journal" not in features for features in self.features.values())
        self.calls.append(("zero", directory))


class SanitizeTests(unittest.TestCase):
    def test_cleanup_locks_hashes_and_removes_private_build_state(self):
        g = FakeGuestFS()
        original = copy.deepcopy(g.nodes["/etc/passwd"])
        s.cleanup(g, "ubuntu")
        s.verify(g, "ubuntu")
        self.assertEqual(g.nodes["/etc/passwd"], original)
        self.assertEqual(g.read_file("/etc/shadow"), b"root:!:1:2:3:4:5:6:7\nbuilder:!:1:2:3:4:5:6:7\nubuntu:!:1:2:3:4:5:6:7\n")
        self.assertEqual(g.read_file("/etc/gshadow"), b"root:!::\nsudo:!::builder\n")
        for home in ("/root", "/home/ubuntu", "/home/builder"):
            self.assertEqual(g.ls(home), [])
        self.assertEqual(g.read_file("/etc/hostname"), b"localhost\n")
        self.assertEqual(g.read_file("/etc/machine-id"), b"")
        self.assertEqual(g.nodes["/etc/resolv.conf"], ("link", "../run/systemd/resolve/stub-resolv.conf"))
        self.assertEqual(g.modes["/tmp"], 0o1777)
        self.assertEqual(g.read_file("/etc/cloud/cloud.cfg"), b"users: [default]\n")
        self.assertNotIn("/etc/sudoers.d/90-cloud", g.nodes)

    def test_native_os_metadata_and_simple_layout_only(self):
        for selected in s.OS:
            with self.subTest(selected=selected):
                g = FakeGuestFS(selected)
                self.assertEqual(s.layout(g, selected), (g.mounts, g.user))
        for change in (lambda g: setattr(g, "major", 99), lambda g: setattr(g, "minor", 10),
                       lambda g: setattr(g, "distro", "alpine"), lambda g: setattr(g, "devices", ["/dev/sda", "/dev/sdb"]),
                       lambda g: setattr(g, "volumes", ["/dev/vg/root"]), lambda g: setattr(g, "guid", "not-bios"),
                       lambda g: g.filesystems.update({"/dev/sda2": "swap"}),
                       lambda g: g.mounts.update({"/data": "/dev/sda2"}),
                       lambda g: g.filesystems.update({"/dev/sda1": "xfs"})):
            g = FakeGuestFS()
            change(g)
            with self.assertRaises(ValueError): s.layout(g, "ubuntu-2604")
            self.assertEqual(g.calls, [])
        with self.assertRaises(ValueError): s.layout(FakeGuestFS(), "kubernetes")

    def test_journal_removal_zero_every_filesystem_then_restore_order(self):
        g = FakeGuestFS()
        s.sanitize(g, "ubuntu-2604")
        steps = [call for call in g.calls if call[0] in ("journal", "zero")]
        self.assertEqual(steps, [("journal", "/dev/sda1", False), ("journal", "/dev/sda13", False),
                                 ("zero", "/boot/efi"), ("zero", "/boot"), ("zero", "/"),
                                 ("journal", "/dev/sda1", True), ("journal", "/dev/sda13", True)])
        self.assertFalse(g.mounted)
        self.assertTrue(all("has_journal" in features for features in g.features.values()))

    def test_symlink_ancestors_and_critical_leaf_refused_leaf_cleanup_unlinks(self):
        g = FakeGuestFS()
        g.nodes["/var/lib/cloud"] = ("link", "/etc")
        s.remove(g, "/var/lib/cloud")
        self.assertTrue(g.is_dir("/etc"))
        for path in ("/etc", "/etc/shadow", "/home", "/home/ubuntu"):
            with self.subTest(path=path):
                g = FakeGuestFS()
                g.nodes[path] = ("link", "/elsewhere")
                with self.assertRaises(ValueError): s.cleanup(g, "ubuntu")
        with self.assertRaises(ValueError): s.journal(FakeGuestFS(), "/dev/sda1;arbitrary", False)

    def test_scoped_scans_fail_closed_and_do_not_emit_guest_data(self):
        for private in (b"10.200.9.8", b"-----BEGIN OPENSSH PRIVATE KEY-----", b"ssh-ed25519 AAAA",
                        b"52:54:00:aa:bb:cc"):
            g = FakeGuestFS()
            s.cleanup(g, "ubuntu")
            g.put("/etc/cloud/cloud.cfg", private)
            with self.assertRaisesRegex(ValueError, "^sanitization refused$"):
                s.verify(g, "ubuntu")
        g = FakeGuestFS()
        g.put("/etc/shadow", b"malformed\n")
        with self.assertRaises(ValueError): s.lock_table(g, "/etc/shadow", 9)
        g = FakeGuestFS()
        g.put("/etc/sudoers", b"packer ALL=(ALL) NOPASSWD:ALL\n")
        with self.assertRaises(ValueError): s.cleanup(g, "ubuntu")

    def test_large_grub_font_is_not_text_but_grub_config_is_scanned(self):
        g = FakeGuestFS()
        g.put("/boot/grub/fonts/unicode.pf2", bytes((1 << 20) + 1))
        s.cleanup(g, "ubuntu")
        s.verify(g, "ubuntu")
        g.put("/boot/grub/grub.cfg", b"set build_ip=10.200.242.50\n")
        with self.assertRaises(ValueError):
            s.verify(g, "ubuntu")

    def test_nonshadow_passwd_hash_or_empty_password_refused(self):
        for password in ("$6$SYNTHETIC_HASH", "", "!$6$OLD", "*SYNTHETIC"):
            g = FakeGuestFS()
            rows = g.read_file("/etc/passwd").replace(b"builder:x:", ("builder:" + password + ":").encode())
            g.put("/etc/passwd", rows)
            with self.subTest(password=password), self.assertRaises(ValueError):
                s.cleanup(g, "ubuntu")
            g = FakeGuestFS()
            s.cleanup(g, "ubuntu")
            g.put("/etc/passwd", rows)
            with self.assertRaises(ValueError):
                s.verify(g, "ubuntu")

    def test_qcow_metadata_rejected_before_any_check_or_conversion(self):
        clean = {"format": "qcow2", "virtual-size": 20 << 30, "format-specific": {"data": {"corrupt": False}}}
        bad = [{"backing-filename": "/etc/passwd"}, {"data-file": "/etc/shadow"}, {"encrypted": True},
               {"snapshots": [{"name": "private"}]}, {"format": "raw"}, {"virtual-size": 21 << 30},
               {"format-specific": {"data": {"encrypt": {"format": "luks"}}}}]
        for fields in bad:
            with self.subTest(fields=fields), patch.object(s, "command", return_value=__import__("json").dumps(dict(clean, **fields)).encode()) as run:
                with self.assertRaises(ValueError): s.image_info(s.SOURCE)
                self.assertEqual(run.call_count, 1)
        with patch.object(s, "command", side_effect=[__import__("json").dumps(clean).encode(), b""]) as run:
            self.assertEqual(s.image_info(s.SOURCE), clean)
            self.assertEqual(run.call_args_list[1].args, ("check", "-f", "qcow2", s.SOURCE))

    def test_cli_refuses_unknown_os_variant_and_paths_without_execution(self):
        for args in (["--os", "debian-14"], ["--os", "ubuntu-2604", "--variant", "kubernetes"],
                     ["--os", "ubuntu-2604", "--input", "/other"]):
            with contextlib.redirect_stderr(io.StringIO()), patch.object(s, "command") as run:
                with self.assertRaises(SystemExit): s.main(args)
                run.assert_not_called()
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err), patch.object(s.os, "getuid", return_value=0), patch.object(s, "command") as run:
            self.assertEqual(s.main(["--os", "ubuntu-2604"]), 2)
            run.assert_not_called()
        self.assertEqual(out.getvalue(), "")
        self.assertEqual(err.getvalue(), "sanitization refused\n")


PRESERVE_NAME = "05_logging.cfg"  # Name class of the real package-owned (cloud-init-base) vendor drop-in.
PRESERVE = b"## synthetic packaged vendor drop-in, NOT the real file bytes\n_log: []\n"
REMOVE_BUILD = b"network: {config: disabled}\n"
UUID = "0f1e2d3c-4b5a-4978-8a6b-5c4d3e2f1a0b"  # Synthetic, canonical lowercase.
# Synthetic stand-ins shaped like stock Ubuntu sudo policy (NOT the real bytes; the real hashes come from review).
SUDOERS = (b"Defaults\tenv_reset\nDefaults\tuse_pty\nroot\tALL=(ALL:ALL) ALL\n%admin ALL=(ALL) ALL\n"
           b"%sudo\tALL=(ALL:ALL) ALL\n@includedir /etc/sudoers.d\n")
SUDOERS_README = b"#\n# Files in this directory are parsed by sudo (synthetic notice).\n#\n"
# Real class: builder in adm,cdrom,dip,lxd,plugdev,sudo (group AND gshadow), own primary group `builder`.
GROUP = (b"root:x:0:\nadm:x:4:syslog,builder\ncdrom:x:24:builder\nsudo:x:27:builder\nstaff:x:50:builder2\n"
         b"lxd:x:101:builder\nbuilder:x:1000:\n")
GSHADOW = (b"root:*::\nadm:*::syslog,builder\ncdrom:*::builder\nsudo:*::builder\nstaff:*:builder:builder2\n"
           b"lxd:!::builder\nbuilder:!::\n")
SNAPD_STATE = (b'{"data":{"auth":{"device":{"brand":"canonical","model":"generic-classic","serial":"synthetic-serial",'
               b'"key-id":"synthetic-key-id"},"last-id":0,"users":[]},"seeded":true,"patch-level":6},'
               b'"changes":{},"tasks":{},"last-change-id":0,"last-task-id":0,"last-lane-id":0}')
SNAPD_RESET = (b'{"data":{"auth":{"device":{"brand":"canonical","model":"generic-classic"},"last-id":0,"users":[]},'
               b'"seeded":true,"patch-level":6},"changes":{},"tasks":{},"last-change-id":0,"last-task-id":0,'
               b'"last-lane-id":0}')
SERIAL = "/var/lib/snapd/assertions/asserts-v0/serial"


class FakeK8sGuestFS(FakeGuestFS):
    """Synthetic single-partition msdos/seabios Kubernetes node layout; NOT template100 facts."""
    def __init__(self):
        super().__init__("ubuntu-2604")
        for path in [p for p in self.nodes if p == "/home/ubuntu" or p.startswith("/home/ubuntu/")]:
            del self.nodes[path]
        self.uuids = {UUID: "/dev/sda1"}  # Filesystem uuid -> device, as findfs_uuid resolves it.
        self.readonly, self.nlinks = False, {}
        self.mounts = {"/": "/dev/sda1"}
        self.filesystems = {"/dev/sda1": "ext4"}
        self.features = {"/dev/sda1": "has_journal extents"}
        self.parttype, self.partitions = "msdos", [{"part_num": 1}]
        self.put("/etc/passwd", b"root:x:0:0:root:/root:/bin/bash\ndaemon:x:1:1::/usr/sbin:/usr/sbin/nologin\n"
                                b"sync:x:4:65534:sync:/bin:/bin/sync\nbuilder:x:1000:1000::/home/builder:/bin/bash\n")
        self.put("/etc/shadow", b"root:*:1:2:3:4:5:6:7\ndaemon:*:1:2:3:4:5:6:7\nsync:*:1:2:3:4:5:6:7\n"
                                b"builder:!:1:2:3:4:5:6:7\n")
        self.put("/etc/fstab", b"# static\nUUID=synthetic / ext4 defaults 0 1\n")
        self.put("/usr/sbin/netplan", b"")
        self.put("/usr/lib/systemd/system/systemd-networkd.service", b"")
        self.put("/usr/lib/systemd/system/kubelet.service", b"[Service]\nExecStart=/usr/bin/kubelet\n")
        self.nodes[s.KUBELET_WANTS] = ("link", "/usr/lib/systemd/system/kubelet.service")
        self.mkdir_p("/etc/systemd/system/multi-user.target.wants")
        self.put("/var/lib/dpkg/status", b"".join(
            b"Package: %s\nStatus: install ok installed\nVersion: 1.35.10-1.1\n\n" % name.encode()
            for name in s.K8S_PACKAGES))
        for path in ("/etc/kubernetes/manifests", "/var/lib/kubelet", "/etc/cni/net.d", "/opt/cni/bin"):
            self.mkdir_p(path)
        self.put("/etc/containerd/config.toml", b'version = 3\n[plugins."io.containerd.cri.v1.runtime"]\n')
        self.put("/etc/apt/sources.list.d/kubernetes.list", b"deb https://pkgs.k8s.io/core:/stable:/v1.35/deb/ /\n")
        self.put("/etc/cloud/cloud.cfg.d/" + PRESERVE_NAME, PRESERVE)
        self.put("/etc/cloud/cloud.cfg.d/99-disable-network-config.cfg", REMOVE_BUILD)
        self.put("/home/builder/.ssh/authorized_keys", b"ssh-ed25519 AAAA synthetic\n")
        self.put("/etc/sudoers", SUDOERS)
        self.put("/etc/sudoers.d/README", SUDOERS_README)  # 90-cloud (builder NOPASSWD) stays: cleanup removes it.
        self.put("/etc/group", GROUP)
        self.put("/etc/gshadow", GSHADOW)
        # Registered snapd identity from image seeding (real class): key, serial assertions, state.json device.
        self.put(s.K8S_SNAPD_KEYS + "/synthetic-device-key-id", b"synthetic device key bytes\n")
        self.put(SERIAL + "/generic/generic-classic/synthetic-serial/active", b"type: serial\n")
        self.put(s.K8S_SNAPD_STATE, SNAPD_STATE)
        self.put("/var/lib/snapd/seed/seed.yaml", b"snaps: []\n")
        self.calls.clear()
    def part_get_parttype(self, device): return self.parttype
    def part_list(self, device): return self.partitions
    def readlink(self, path): return self.nodes[path][1]
    def lstatns(self, path):
        assert path in self.nodes
        return {"st_nlink": self.nlinks.get(path, 1)}
    # Read-only mount: any guest mutation while mounted "ro" is a test failure.
    def mount_options(self, options, device, path):
        self.readonly = options == "ro"
        self.calls.append(("mount_ro", device, path) if self.readonly else ("mount", device, path))
        self.mounted.add(device)
    def umount_all(self):
        self.readonly = False
        super().umount_all()
    def mkdir_p(self, path):
        assert not getattr(self, "readonly", False)
        super().mkdir_p(path)
    def write(self, path, data):
        assert not self.readonly
        super().write(path, data)
    def rm(self, path):
        assert not self.readonly
        super().rm(path)
    def rm_rf(self, path):
        assert not self.readonly
        super().rm_rf(path)
    def chown(self, uid, gid, path):
        assert not self.readonly
        super().chown(uid, gid, path)
    def ln_s(self, target, path):
        assert not self.readonly
        super().ln_s(target, path)
    def findfs_uuid(self, uuid):
        self.calls.append(("findfs_uuid", uuid))
        if uuid not in self.uuids:
            raise RuntimeError("findfs_uuid: synthetic unresolvable " + uuid)
        return self.uuids[uuid]


class IntBoolK8sGuestFS(FakeK8sGuestFS):
    """Like the real Python binding: predicate calls return int 0/1, not bool."""
    def is_symlink(self, path): return int(super().is_symlink(path))
    def exists(self, path): return int(super().exists(path))
    def is_file(self, path, followsymlinks=True): return int(super().is_file(path, followsymlinks))
    def is_dir(self, path, followsymlinks=True): return int(super().is_dir(path, followsymlinks))


def k8s_review(**changes):
    review = dict(kubernetes_version="1.35.10", source_raw_sha256="e" * 64, source_hash_verified=True, source_stopped=True, bios="seabios",
                  kubelet_active=False, runtime_version="2.1.0", retained_package_policy_reviewed=True,
                  package_policy_sha256="c" * 64, network_backend="netplan-systemd-networkd",
                  cloud_default_user="ubuntu", cloud_base_config_reviewed=True,
                  sudoers_sha256=hashlib.sha256(SUDOERS).hexdigest(),
                  sudoers_readme_sha256=hashlib.sha256(SUDOERS_README).hexdigest(),
                  reviewed_dropins=((PRESERVE_NAME, hashlib.sha256(PRESERVE).hexdigest(), "preserve_vendor"),
                                    ("99-disable-network-config.cfg", hashlib.sha256(REMOVE_BUILD).hexdigest(),
                                     "remove_build")))
    review.update(changes)
    return review


PARENT = {"format": "raw", "virtual_bytes": s.VIRTUAL_BYTES, "raw_bytes": s.VIRTUAL_BYTES,
          "source_hash_verified": True, "source_stopped": True}


class KubernetesSanitizeTests(unittest.TestCase):
    """variant=kubernetes only; the native-OS tests above are unchanged."""
    def test_curated_copy_preserves_k8s_surface_and_reviewed_dropins(self):
        g = FakeK8sGuestFS()
        g.put("/etc/kubernetes/bootstrap-token.txt", b"synthetic-prejoin\n")  # Curated leaf, not cluster state.
        s.sanitize_k8s(g, "ubuntu-2604", k8s_review(), dict(PARENT))
        self.assertEqual(g.nodes[s.KUBELET_WANTS][0], "link")  # enabled=True preserved, never toggled.
        self.assertEqual(g.ls("/etc/cloud/cloud.cfg.d"), [PRESERVE_NAME])
        self.assertEqual(g.read_file("/etc/cloud/cloud.cfg.d/" + PRESERVE_NAME), PRESERVE)
        self.assertNotIn("/etc/sudoers.d/90-cloud", g.nodes)  # builder NOPASSWD drop-in removed.
        self.assertFalse([p for p in g.nodes if p == "/home/ubuntu" or p.startswith("/home/ubuntu/")])
        # Identity reset: builder dropped from every supplementary group, everything else byte-identical.
        self.assertEqual(g.read_file("/etc/group"), GROUP.replace(b",builder\n", b"\n").replace(b":builder\n", b":\n"))
        self.assertEqual(g.read_file("/etc/gshadow"),
                         b"root:!::\nadm:!::syslog\ncdrom:!::\nsudo:!::\nstaff:!::builder2\nlxd:!::\nbuilder:!::\n")
        self.assertEqual(g.ls("/etc/sudoers.d"), ["README"])
        self.assertEqual((g.read_file("/etc/sudoers"), g.read_file("/etc/sudoers.d/README")), (SUDOERS, SUDOERS_README))
        self.assertEqual(g.ls(s.K8S_SNAPD_KEYS), [])
        self.assertFalse([p for p in g.nodes if p == SERIAL or p.startswith(SERIAL + "/")])
        self.assertEqual(g.read_file(s.K8S_SNAPD_STATE), SNAPD_RESET)
        self.assertIn("/var/lib/snapd/seed/seed.yaml", g.nodes)
        # Read-only inventory: the first mount is "ro"; the rw remount happens only after umount_all.
        mounts = [c for c in g.calls if c[0] in ("mount", "mount_ro")]
        self.assertEqual(mounts[0], ("mount_ro", "/dev/sda1", "/"))
        first_rw = g.calls.index(("mount", "/dev/sda1", "/"))
        self.assertEqual(g.calls[first_rw - 1], ("umount_all",))
        self.assertFalse([c for c in g.calls[:first_rw] if c[0] in ("rm", "rm_rf", "chown")])
        for gone in ("/home/builder/.ssh", "/etc/kubernetes/bootstrap-token.txt", "/var/lib/cloud"):
            self.assertNotIn(gone, g.nodes)
        for kept in ("/etc/containerd/config.toml", "/var/lib/dpkg/status", "/etc/kubernetes/manifests", "/opt/cni/bin"):
            self.assertIn(kept, g.nodes)
        self.assertIn(b"builder:!:", g.read_file("/etc/shadow"))
        self.assertEqual([c for c in g.calls if c[0] in ("journal", "zero")],
                         [("journal", "/dev/sda1", False), ("zero", "/"), ("journal", "/dev/sda1", True)])

    def refuses(self, g, review=None, parent=None):
        before = copy.deepcopy(g.nodes)
        with self.assertRaises(ValueError) as caught:
            s.sanitize_k8s(g, "ubuntu-2604", review or k8s_review(), parent or dict(PARENT))
        self.assertIn(str(caught.exception), ("sanitization refused", k8s_copy_policy.REFUSAL))
        self.assertEqual(g.nodes, before)  # Refusal precedes every guest write.
        self.assertFalse([c for c in g.calls if c[0] in ("rm", "rm_rf", "chown", "journal", "zero")])
        return str(caught.exception)

    def policy_refuses(self, g, review=None, parent=None):
        """The refusal must come from k8s_copy_policy itself, not an earlier sanitizer guard."""
        self.assertEqual(self.refuses(g, review, parent), k8s_copy_policy.REFUSAL)

    def test_existing_cluster_state_refuses_never_wipes(self):
        for path in ("/etc/kubernetes/pki/ca.crt", "/etc/kubernetes/admin.conf", "/etc/kubernetes/super-admin.conf",
                     "/etc/kubernetes/bootstrap-kubelet.conf", "/etc/kubernetes/kubelet.conf",
                     "/var/lib/kubelet/pki/kubelet.crt", "/var/lib/etcd/member/snap/db", "/var/lib/etcd/other",
                     "/etc/kubernetes/manifests/kube-apiserver.yaml", "/var/lib/kubelet/pods/uid/x",
                     "/var/lib/containerd/io.containerd.runtime.v2.task/k8s.io/id/x", "/var/lib/cni/networks/n/ip"):
            with self.subTest(path=path):
                g = FakeK8sGuestFS(); g.put(path, b"synthetic")
                self.refuses(g)
        g = FakeK8sGuestFS(); g.nodes["/etc/kubernetes/admin.conf"] = ("link", "/dev/null")  # Dangling counts.
        self.refuses(g)

    def test_dropins_require_exact_reviewed_decisions_no_blanket_wipe(self):
        g = FakeK8sGuestFS(); g.put("/etc/cloud/cloud.cfg.d/50-unreviewed.cfg", b"#cloud-config\n")
        self.refuses(g)
        g = FakeK8sGuestFS(); g.put("/etc/cloud/cloud.cfg.d/" + PRESERVE_NAME, PRESERVE + b"# changed\n")
        self.refuses(g)
        g = FakeK8sGuestFS(); g.nodes["/etc/cloud/cloud.cfg.d/" + PRESERVE_NAME] = ("link", "/etc/shadow")
        self.refuses(g)
        self.refuses(FakeK8sGuestFS(), k8s_review(reviewed_dropins=()))
        self.refuses(FakeK8sGuestFS(), k8s_review(reviewed_dropins=k8s_review()["reviewed_dropins"][:1]))

    def test_account_profile_is_exact_builder(self):
        # Rename in BOTH passwd and shadow so the policy itself (not a missing-shadow guard) refuses.
        for old, new in ((b"builder:", b"ubuntu:"),):
            g = FakeK8sGuestFS()
            for table in ("/etc/passwd", "/etc/shadow"):
                g.put(table, g.read_file(table).replace(old, new))
            self.policy_refuses(g)
        for old, new in ((b"/home/builder:/bin/bash", b"/srv/builder:/bin/bash"),
                         (b"daemon:x:1:1::/usr/sbin:/usr/sbin/nologin", b"daemon:x:1:1::/usr/sbin:/bin/sh"),
                         (b"sync:x:4:65534:sync:/bin:/bin/sync", b"sync:x:4:65534:sync:/bin:/bin/sh"),
                         (b"builder:x:", b"builder:$6$synthetic:")):
            with self.subTest(new=new):
                g = FakeK8sGuestFS(); g.put("/etc/passwd", g.read_file("/etc/passwd").replace(old, new))
                self.policy_refuses(g)
        g = FakeK8sGuestFS(); g.put("/etc/shadow", g.read_file("/etc/shadow").replace(b"builder:!:", b"builder:$6$X:"))
        self.policy_refuses(g)
        g = FakeK8sGuestFS()
        g.put("/etc/passwd", g.read_file("/etc/passwd") + b"extra:x:1001:1001::/home/extra:/bin/bash\n")
        g.put("/etc/shadow", g.read_file("/etc/shadow") + b"extra:!:1:2:3:4:5:6:7\n")
        self.policy_refuses(g)
        g = FakeK8sGuestFS(); g.put("/etc/passwd", g.read_file("/etc/passwd") + b"orphan:x:5:5::/:/usr/sbin/nologin\n")
        self.refuses(g)  # passwd row without a shadow row refuses before the policy.

    def test_upstream_ubuntu_default_user_and_locked_builder(self):
        # Owner decision: cloud-init's upstream default user `ubuntu` is created at FIRST BOOT.
        self.refuses(FakeK8sGuestFS(), k8s_review(cloud_default_user="builder"))  # builder is no default user.
        # A pre-existing ubuntu account (any shape) refuses in the policy itself.
        for row in (b"ubuntu:x:1001:1001::/home/ubuntu:/bin/bash\n", b"ubuntu:x:999:999::/nonexistent:/usr/sbin/nologin\n"):
            g = FakeK8sGuestFS()
            g.put("/etc/passwd", g.read_file("/etc/passwd") + row)
            g.put("/etc/shadow", g.read_file("/etc/shadow") + b"ubuntu:!:1:2:3:4:5:6:7\n")
            self.policy_refuses(g)
        g = FakeK8sGuestFS(); g.mkdir_p("/home/ubuntu")  # Stale home without an account refuses too.
        self.refuses(g)
        g = FakeK8sGuestFS(); g.put("/etc/group", g.read_file("/etc/group") + b"ubuntu:x:1001:\n")
        self.refuses(g)  # useradd at first boot would collide with an existing group.
        # builder must be locked: an unlocked or recoverable-hash shadow field refuses.
        for marker in (b"$6$synthetic", b"!$6$synthetic", b""):
            g = FakeK8sGuestFS()
            g.put("/etc/shadow", g.read_file("/etc/shadow").replace(b"builder:!:", b"builder:" + marker + b":"))
            self.policy_refuses(g)
        # A NOPASSWD builder rule in sudoers.d is removed; in the main sudoers it refuses (no rewrite).
        g = FakeK8sGuestFS()
        g.put("/etc/sudoers", g.read_file("/etc/sudoers") + b"builder ALL=(ALL) NOPASSWD:ALL\n")
        with self.assertRaises(ValueError): s.sanitize_k8s(g, "ubuntu-2604", k8s_review(), dict(PARENT))
        g = FakeK8sGuestFS()
        s.sanitize_k8s(g, "ubuntu-2604", k8s_review(), dict(PARENT))
        self.assertNotIn("/etc/sudoers.d/90-cloud", g.nodes)
        self.assertNotIn("/home/builder/.ssh/authorized_keys", g.nodes)
        self.assertIn(b"builder:!:", g.read_file("/etc/shadow"))
        self.assertIn(b"builder:x:1000:1000::/home/builder:/bin/bash", g.read_file("/etc/passwd"))

    def test_swap_files_active_fstab_rows_and_units_refuse_never_wipe(self):
        for change in (lambda g: g.put("/swap.img", b"\0"), lambda g: g.put("/swapfile", b"\0"),
                       lambda g: g.nodes.__setitem__("/swap.img", ("link", "/dev/sda2")),
                       lambda g: g.put("/etc/fstab", g.read_file("/etc/fstab") + b"/swap.img none swap sw 0 0\n"),
                       lambda g: g.put("/etc/fstab", g.read_file("/etc/fstab") + b"  /swapfile\tnone\tswap\tsw\t0\t0\n"),
                       lambda g: g.put("/etc/fstab", g.read_file("/etc/fstab") + b"UUID=x none swap sw 0 0 # comment\n"),
                       lambda g: g.put("/etc/fstab", g.read_file("/etc/fstab") + b"#/swap.img none swap sw 0 0\n"
                                                                                b"/swap.img none swap sw 0 0\n"),
                       lambda g: g.put("/etc/systemd/system/swap.img.swap", b"[Swap]\nWhat=/swap.img\n"),
                       lambda g: g.put("/etc/systemd/system/dev-sda2.swap", b"")):
            g = FakeK8sGuestFS(); change(g)
            self.policy_refuses(g)
        # The swap row itself (not external_fstab, which only flags a non-/ mountpoint) is what refuses:
        # `/swap.img none swap` has mountpoint `none`, so isolate the count on the inventory facts.
        for extra, count in ((b"/swap.img none swap sw 0 0\n", 1), (b"#/swap.img none swap sw 0 0\n", 0),
                             (b"/swap.img none swap sw 0 0\n/dev/sda2 none swap sw 0 0\n", 2)):
            with self.subTest(extra=extra):
                g = FakeK8sGuestFS(); g.put("/etc/fstab", g.read_file("/etc/fstab") + extra)
                _, facts, _ = s.k8s_inventory(g, "ubuntu-2604", k8s_review(), dict(PARENT))
                self.assertEqual(facts["layout"]["swap_entries"], count)
                facts["layout"]["external_fstab"] = False  # Even with that guard neutralised, a row refuses.
                if count:
                    with self.assertRaises(k8s_copy_policy.PolicyRefusal):
                        k8s_copy_policy.validate_inventory(facts, k8s_review()["reviewed_dropins"])
                else:
                    k8s_copy_policy.validate_inventory(facts, k8s_review()["reviewed_dropins"])
        # Commented rows (image-builder late-command `s/^#?/#/`) and prose are inert, not entries.
        for extra in (b"#/swap.img\tnone\tswap\tsw\t0\t0\n", b"  # /swapfile none swap sw 0 0\n",
                      b"##/swap.img none swap sw 0 0\n", b"# no swapping here\n"):
            with self.subTest(extra=extra):
                g = FakeK8sGuestFS(); g.put("/etc/fstab", g.read_file("/etc/fstab") + extra)
                s.sanitize_k8s(g, "ubuntu-2604", k8s_review(), dict(PARENT))
                self.assertIn(extra, g.read_file("/etc/fstab"))  # Not rewritten.

    def test_kubelet_registry_state_secrets_and_proxy_refuse(self):
        # Hardcoded on purpose: emptying the constant must not silently drop these cases.
        registry = ("/var/lib/kubelet/config.json", "/var/lib/kubelet/.dockercfg", "/.docker/config.json",
                    "/.dockercfg", "/root/.docker/config.json")
        self.assertTrue(set(registry) <= set(s.K8S_REGISTRY_FILES))
        changes = [lambda g, p=p: g.put(p, b"{}") for p in registry]
        changes += [lambda g: g.put("/var/lib/kubelet/kubeadm-flags.env", b"KUBELET_KUBEADM_ARGS=--node-ip=10.200.242.50\n"),
                    lambda g: g.put("/var/lib/kubelet/config.yaml.d/x", b"client-key-data: synthetic\n"),
                    lambda g: g.put("/etc/containerd/certs.d/r/hosts.toml",
                                    b'[host."https://r".header]\n  authorization = "Basic c3ludGhldGlj"\n'),
                    lambda g: g.put("/etc/containerd/certs.d/r/hosts.toml",
                                    b'[host."https://r".header]\n  "Authorization" = ["Bearer synthetic"]\n'),
                    lambda g: g.put("/etc/apt/apt.conf.d/90proxy", b'Acquire::http::Proxy "http://proxy.test:3128";\n'),
                    lambda g: g.put("/etc/apt/apt.conf.d/90lab", b'Acquire::http::Pipeline-Depth "0"; // 10.200.1.1\n'),
                    lambda g: g.put("/etc/environment", b'PATH="/usr/bin"\nhttps_proxy=http://proxy.test:3128\n'),
                    lambda g: g.put("/etc/apt/apt.conf", b'Acquire::https::Proxy "http://proxy.test:3128";\n'),
                    lambda g: g.put("/etc/apt/apt.conf.d/91nested", b'Acquire { http { Proxy "http://proxy.test"; }; };\n'),
                    lambda g: g.put("/etc/systemd/system/containerd.service.d/http-proxy.conf",
                                    b'[Service]\nEnvironment="HTTP_PROXY=http://proxy.test:3128"\n'),
                    lambda g: g.put("/var/lib/systemd/credential.secret", b"synthetic"),
                    lambda g: g.nodes.__setitem__("/var/lib/systemd/credential.secret", ("link", "/dev/null"))]
        for index, change in enumerate(changes):
            with self.subTest(index=index):
                g = FakeK8sGuestFS(); change(g)
                self.policy_refuses(g)
        g = FakeK8sGuestFS()
        g.put("/var/lib/kubelet/kubeadm-flags.env", b"KUBELET_KUBEADM_ARGS=--container-runtime-endpoint=unix:///run/containerd/containerd.sock\n")
        g.put("/etc/apt/apt.conf.d/20auto-upgrades", b'APT::Periodic::Update-Package-Lists "0";\n')
        g.put("/etc/environment", b'PATH="/usr/local/sbin:/usr/bin"\n')
        g.mkdir_p("/var/lib/snapd/device/private-keys-v1")  # Empty directory is accepted.
        s.sanitize_k8s(g, "ubuntu-2604", k8s_review(), dict(PARENT))
        self.assertIn("/var/lib/kubelet/kubeadm-flags.env", g.nodes)

    def test_snapd_device_key_is_copy_only_wipe_then_verified_empty(self):
        keys = s.K8S_SNAPD_KEYS
        self.assertEqual(keys, "/var/lib/snapd/device/private-keys-v1")
        self.assertIn(keys + "/*", k8s_copy_policy.COPY_WIPE)
        g = FakeK8sGuestFS()
        g.put(keys + "/n92-synthetic-device-key-id", b"-----BEGIN SYNTHETIC-----\n")  # Seeding-time machine key.
        g.put("/var/lib/snapd/device/serial-placeholder", b"kept")  # Only the key entries are wiped.
        s.sanitize_k8s(g, "ubuntu-2604", k8s_review(), dict(PARENT))
        self.assertTrue(g.is_dir(keys))
        self.assertEqual(g.ls(keys), [])  # snapd regenerates on first boot.
        self.assertIn("/var/lib/snapd/device/serial-placeholder", g.nodes)
        # Not cleanly wipeable by the exact `<dir>/*` row: still an unclassified credential -> refuse.
        def drop_keys(g):
            for path in [p for p in g.nodes if p.startswith(keys + "/")]:
                del g.nodes[path]
        for change in (lambda g: g.put(keys + "/.hidden-key", b"synthetic"),  # Dot entry: not glob-matched.
                       lambda g: g.nodes.__setitem__(keys + "/link", ("link", "/etc/shadow")),
                       lambda g: g.put(keys + "/subdir/key", b"synthetic"),
                       # A hardlinked key (nlink 2) would survive the unlink under another name.
                       lambda g: g.nlinks.__setitem__(keys + "/synthetic-device-key-id", 2),
                       lambda g: (drop_keys(g), g.nodes.__setitem__(keys, ("link", "/srv/keys"))),
                       lambda g: (drop_keys(g), g.put(keys, b"not a directory"))):
            g = FakeK8sGuestFS(); change(g)
            self.policy_refuses(g)
        g = FakeK8sGuestFS()
        _, facts, _ = s.k8s_inventory(g, "ubuntu-2604", k8s_review(), dict(PARENT))
        self.assertEqual(facts["identity"]["snapd"]["device_keys"], 1)
        self.assertEqual(facts["hygiene"]["unclassified_credentials"], 0)
        g = FakeK8sGuestFS(); g.nlinks[keys + "/synthetic-device-key-id"] = 2
        _, facts, _ = s.k8s_inventory(g, "ubuntu-2604", k8s_review(), dict(PARENT))
        self.assertEqual((facts["identity"]["snapd"]["device_keys"], facts["hygiene"]["unclassified_credentials"]), (0, 1))
        # credential.secret stays a refusal even next to a wipeable snapd key.
        g = FakeK8sGuestFS(); g.put(keys + "/k", b"synthetic"); g.put("/var/lib/systemd/credential.secret", b"x")
        self.policy_refuses(g)

    def test_builder_dropped_from_every_supplementary_group_copy_only(self):
        g = FakeK8sGuestFS()
        _, facts, _ = s.k8s_inventory(g, "ubuntu-2604", k8s_review(), dict(PARENT))
        self.assertEqual(facts["identity"]["builder_supplementary_groups"], 5)  # adm,cdrom,sudo,staff(admin),lxd.
        self.assertEqual(g.read_file("/etc/group"), GROUP)  # Inventory never writes.
        # Pure strip: exact member only, other members/rows/order kept; "builder2" is not "builder".
        self.assertEqual(s.k8s_group_strip(b"a:x:1:builder,x,builder2\nb:x:2:\nbuilder:x:1000:\n", (3,)),
                         b"a:x:1:x,builder2\nb:x:2:\nbuilder:x:1000:\n")
        self.assertEqual(s.k8s_group_strip(b"a:!:builder,y:builder\n", (2, 3)), b"a:!:y:\n")
        self.assertEqual(s.k8s_group_strip(b"a:x:1:builder", (3,)), b"a:x:1:")  # No trailing newline kept as is.
        for bad in (b"", b"\n", b"a:x:1\n", b"a:x:1:b:c\n", b":x:1:builder\n"):
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                s.k8s_group_strip(bad, (3,))
        for table, bad in (("/etc/group", GROUP + b"broken-row\n"), ("/etc/gshadow", GSHADOW + b"x:y\n")):
            g = FakeK8sGuestFS(); g.put(table, bad)
            self.refuses(g)  # Malformed table: refused before any write (inventory dry-runs the strip).
        g = FakeK8sGuestFS()
        s.sanitize_k8s(g, "ubuntu-2604", k8s_review(), dict(PARENT))
        self.assertIn(b"builder:x:1000:\n", g.read_file("/etc/group"))  # Own primary group kept.
        self.assertIn(b"builder:x:1000:1000::/home/builder:/bin/bash", g.read_file("/etc/passwd"))
        self.assertEqual(s.k8s_builder_groups(g), 0)
        # A rewrite that silently leaves builder in sudo must not pass the final check.
        g = FakeK8sGuestFS()
        with patch.object(s, "k8s_group_strip", side_effect=lambda data, indexes, user=b"builder": data):
            with self.assertRaises(ValueError): s.sanitize_k8s(g, "ubuntu-2604", k8s_review(), dict(PARENT))
        self.assertIn(b"sudo:x:27:builder", g.read_file("/etc/group"))
        self.assertFalse([c for c in g.calls if c[0] == "zero"])  # Refused before zeroing/export.

    def test_builder_primary_gid_must_be_own_unshared_group(self):
        g = FakeK8sGuestFS()
        _, facts, _ = s.k8s_inventory(g, "ubuntu-2604", k8s_review(), dict(PARENT))
        self.assertIs(facts["identity"]["builder_primary_group_own"], True)
        for change in (lambda g: g.put("/etc/passwd", g.read_file("/etc/passwd").replace(  # Primary gid 27 = sudo.
                           b"builder:x:1000:1000:", b"builder:x:1000:27:")),
                       lambda g: g.put("/etc/group", g.read_file("/etc/group") + b"shared:x:1000:\n"),  # Shared gid.
                       lambda g: g.put("/etc/group", g.read_file("/etc/group").replace(  # Own row missing.
                           b"builder:x:1000:\n", b"")),
                       lambda g: g.put("/etc/group", g.read_file("/etc/group").replace(  # Own row renamed.
                           b"builder:x:1000:\n", b"build:x:1000:\n")),
                       lambda g: g.put("/etc/group", g.read_file("/etc/group") + b"builder:x:1001:\n"),  # Two rows.
                       lambda g: g.put("/etc/passwd", g.read_file("/etc/passwd").replace(  # Non-numeric gid.
                           b"builder:x:1000:1000:", b"builder:x:1000:+1000:"))):
            g = FakeK8sGuestFS(); change(g)
            self.policy_refuses(g)
        g = FakeK8sGuestFS(); g.put("/etc/group", g.read_file("/etc/group").replace(b"lxd:x:101:", b"lxd:x:abc:"))
        self.refuses(g)  # Malformed gid in /etc/group refuses before the policy.
        # gid compares numerically: a zero-padded duplicate of builder's gid is still a shared gid.
        g = FakeK8sGuestFS(); g.put("/etc/group", g.read_file("/etc/group") + b"pad:x:01000:\n")
        self.policy_refuses(g)
        # Re-verified after cleanup: a primary-gid change that appears late refuses before zeroing.
        real_cleanup = s.cleanup
        def late(g, user, patterns):
            real_cleanup(g, user, patterns)
            g.put("/etc/passwd", g.read_file("/etc/passwd").replace(b"builder:x:1000:1000:", b"builder:x:1000:27:"))
        g = FakeK8sGuestFS()
        with patch.object(s, "cleanup", side_effect=late):
            with self.assertRaises(ValueError): s.sanitize_k8s(g, "ubuntu-2604", k8s_review(), dict(PARENT))
        self.assertFalse([c for c in g.calls if c[0] == "zero"])

    def test_group_member_lists_must_be_strict_tokens(self):
        # nss_files would still treat these as membership (or they hide a member from an exact-token strip).
        for field in (b"syslog, builder", b"builder ", b",builder", b"builder\r", b"a,,b", b"builder,",
                      b" builder", b"Builder", b"builder\t", b"syslog,builder ,x", b"b\xc3\xa9"):
            with self.subTest(field=field):
                for table, row in (("/etc/group", b"sudo:x:27:" + field), ("/etc/gshadow", b"sudo:!::" + field),
                                   ("/etc/gshadow", b"sudo:!:" + field + b":")):
                    with self.assertRaises(ValueError):
                        s.k8s_group_strip(row + b"\n", dict(s.K8S_GROUP_TABLES)[table])
                    g = FakeK8sGuestFS()
                    g.put(table, g.read_file(table).replace(b"sudo:x:27:builder\n" if table == "/etc/group"
                                                            else b"sudo:*::builder\n", row + b"\n"))
                    self.assertEqual(self.refuses(g), "sanitization refused")
        for field in (b"builder", b"a,builder,b", b"user$", b"x_y.z-1", b"_svc"):
            self.assertEqual(s.k8s_members(field), field.split(b","))
        self.assertEqual(s.k8s_members(b""), [])

    def test_sudo_rs_sudo_conf_and_userdb_guards(self):
        # sudo-rs (Ubuntu 26.04's active sudo) prefers /etc/sudoers-rs: any entry refuses.
        for change in (lambda g: g.put(s.SUDOERS_RS, SUDOERS),  # Even identical content is unreviewed.
                       lambda g: g.nodes.__setitem__(s.SUDOERS_RS, ("link", "/etc/sudoers")),
                       lambda g: g.mkdir_p(s.SUDOERS_RS),
                       # classic sudo.conf: any active directive refuses; comments are fine.
                       lambda g: g.put(s.SUDO_CONF, b"# comment\nPlugin sudoers_policy sudoers.so sudoers_file=/x\n"),
                       lambda g: g.put(s.SUDO_CONF, b"  Set disable_coredump false\n"),
                       lambda g: g.nodes.__setitem__(s.SUDO_CONF, ("link", "/tmp/sudo.conf")),
                       # systemd userdb records/memberships outside the rewritten group tables.
                       lambda g: g.put("/etc/userdb/builder:sudo.membership", b""),
                       lambda g: g.put("/etc/userdb/builder.user", b"{}"),
                       lambda g: g.put("/usr/lib/userdb/x.group", b"{}"),
                       lambda g: g.put("/etc/userdb", b"not a dir")):
            g = FakeK8sGuestFS(); change(g); g.calls.clear()
            self.assertEqual(self.refuses(g), "sanitization refused")
        g = FakeK8sGuestFS()
        g.put(s.SUDO_CONF, b"#\n# Sample /etc/sudo.conf (comment-only, as observed)\n#Plugin sudoers_policy x\n\n  #x\n")
        g.mkdir_p("/etc/userdb")  # Empty userdb dir is accepted.
        s.sanitize_k8s(g, "ubuntu-2604", k8s_review(), dict(PARENT))
        self.assertIn(s.SUDO_CONF, g.nodes)
        # Re-verified in check(): a late /etc/sudoers-rs or userdb membership refuses before zeroing.
        real_cleanup = s.cleanup
        for late in (lambda g: g.put(s.SUDOERS_RS, SUDOERS), lambda g: g.put("/etc/userdb/builder:sudo.membership", b""),
                     lambda g: g.put(s.SUDO_CONF, b"Path askpass /x\n")):
            g = FakeK8sGuestFS()
            def sneaky(g, user, patterns, late=late):
                real_cleanup(g, user, patterns)
                late(g)
            with patch.object(s, "cleanup", side_effect=sneaky):
                with self.assertRaises(ValueError): s.sanitize_k8s(g, "ubuntu-2604", k8s_review(), dict(PARENT))
            self.assertFalse([c for c in g.calls if c[0] == "zero"])

    def test_sudo_policy_bound_to_reviewed_hashes(self):
        g = FakeK8sGuestFS()
        s.sanitize_k8s(g, "ubuntu-2604", k8s_review(), dict(PARENT))  # Build NOPASSWD drop-in removed -> {README}.
        self.assertEqual(g.ls("/etc/sudoers.d"), ["README"])
        # Unreviewed sudo policy refuses before any write (no word-grep loophole).
        for change in (lambda g: g.put("/etc/sudoers", SUDOERS + b"%sudo ALL=(ALL) NOPASSWD:ALL\n"),
                       lambda g: g.put("/etc/sudoers", SUDOERS + b"#1000 ALL=(ALL) NOPASSWD:ALL\n"),
                       lambda g: g.put("/etc/sudoers", SUDOERS + b"@include /etc/sudoers.local\n"),
                       lambda g: g.put("/etc/sudoers.d/10-wheel", b"%sudo ALL=(ALL) NOPASSWD:ALL\n"),
                       lambda g: g.put("/etc/sudoers.d/10-uid", b"#1000 ALL=(ALL) NOPASSWD:ALL\n"),
                       lambda g: g.put("/etc/sudoers.d/10-inc", b"@include /etc/sudoers.local\n"),
                       lambda g: g.put("/etc/sudoers.d/.hidden-builder", b"builder ALL=(ALL) ALL\n"),
                       lambda g: g.put("/etc/sudoers.d/README", SUDOERS_README + b"%sudo ALL=(ALL) NOPASSWD:ALL\n"),
                       lambda g: g.nodes.pop("/etc/sudoers.d/README"),
                       lambda g: g.nodes.__setitem__("/etc/sudoers.d/90-cloud", ("link", "/etc/sudoers.local")),
                       lambda g: (g.rm_rf("/etc/sudoers.d"), g.nodes.__setitem__("/etc/sudoers.d", ("link", "/srv")))):
            g = FakeK8sGuestFS(); change(g); g.calls.clear()
            self.refuses(g)
        for key in ("sudoers_sha256", "sudoers_readme_sha256"):
            self.refuses(FakeK8sGuestFS(), k8s_review(**{key: "0" * 64}))
        # Final re-check: a drop-in that appears during cleanup (after the inventory) refuses before zeroing.
        g = FakeK8sGuestFS()
        real_cleanup = s.cleanup
        def sneaky(g, user, patterns):
            real_cleanup(g, user, patterns)
            g.put("/etc/sudoers.d/99-late", b"%sudo ALL=(ALL) NOPASSWD:ALL\n")
        with patch.object(s, "cleanup", side_effect=sneaky):
            with self.assertRaises(ValueError): s.sanitize_k8s(g, "ubuntu-2604", k8s_review(), dict(PARENT))
        self.assertFalse([c for c in g.calls if c[0] == "zero"])
        g = FakeK8sGuestFS()
        def tamper(g, user, patterns):
            real_cleanup(g, user, patterns)
            g.put("/etc/sudoers", SUDOERS + b"builder ALL=(ALL) ALL\n")
        with patch.object(s, "cleanup", side_effect=tamper):
            with self.assertRaises(ValueError): s.sanitize_k8s(g, "ubuntu-2604", k8s_review(), dict(PARENT))
        self.assertFalse([c for c in g.calls if c[0] == "zero"])

    def test_snapd_identity_reset_to_unregistered(self):
        g = FakeK8sGuestFS()
        _, facts, _ = s.k8s_inventory(g, "ubuntu-2604", k8s_review(), dict(PARENT))
        self.assertEqual(facts["identity"]["snapd"], {"state": True, "device_serial": True, "device_key_id": True,
                                                      "session_macaroon": False, "users": 0,
                                                      "serial_assertions": 4, "device_keys": 1})
        self.assertNotIn(b"synthetic-serial", json.dumps(facts).encode())  # Presence/counts only, never values.
        g = FakeK8sGuestFS()
        s.sanitize_k8s(g, "ubuntu-2604", k8s_review(), dict(PARENT))
        self.assertEqual(g.read_file(s.K8S_SNAPD_STATE), SNAPD_RESET)  # Compact, key order kept, brand/model kept.
        self.assertEqual(list(json.loads(SNAPD_RESET)), list(json.loads(SNAPD_STATE)))
        self.assertFalse([p for p in g.nodes if p.startswith(SERIAL)])
        self.assertTrue(g.is_dir("/var/lib/snapd/assertions/asserts-v0"))
        # Store user credentials refuse (policy), never wiped.
        for old, new in ((b'"key-id":"synthetic-key-id"}', b'"key-id":"synthetic-key-id","session-macaroon":"m"}'),
                         (b'"users":[]', b'"users":[{"id":1}]')):
            g = FakeK8sGuestFS(); g.put(s.K8S_SNAPD_STATE, SNAPD_STATE.replace(old, new))
            self.policy_refuses(g)
        # Strict parse: invalid, duplicate keys, NaN, unknown auth/device keys, non-canonical numbers, size bound.
        for bad in (b"{", b"[]", SNAPD_STATE.replace(b'"brand":"canonical"', b'"brand":"a","brand":"b"'),
                    SNAPD_STATE.replace(b'"patch-level":6', b'"patch-level":NaN'),
                    SNAPD_STATE.replace(b'"last-id":0', b'"last-id":0,"macaroon-key":"x"'),
                    SNAPD_STATE.replace(b'"brand":"canonical"', b'"brand":"canonical","extra":1'),
                    SNAPD_STATE.replace(b'"patch-level":6', b'"patch-level":6.0e0'),
                    SNAPD_STATE.replace(b'"users":[]', b'"users":{}'), b"\xff\xfe",
                    SNAPD_STATE + b" " * (s.K8S_SNAPD_STATE_MAX)):
            with self.subTest(bad=bad[:40]):
                g = FakeK8sGuestFS(); g.put(s.K8S_SNAPD_STATE, bad)
                self.assertEqual(self.refuses(g), "sanitization refused")
        # Irregular serial-assertion subtree refuses: symlink, hardlinked leaf, non-directory.
        for change in (lambda g: g.nodes.__setitem__(SERIAL + "/generic/link", ("link", "/etc/shadow")),
                       lambda g: g.nlinks.__setitem__(SERIAL + "/generic/generic-classic/synthetic-serial/active", 2),
                       lambda g: (g.rm_rf(SERIAL), g.put(SERIAL, b"not a dir"))):
            g = FakeK8sGuestFS(); change(g); g.calls.clear()
            self.refuses(g)
        # Unseeded image (no state.json, no serial subtree, no keys) is accepted untouched.
        g = FakeK8sGuestFS()
        for path in [p for p in g.nodes if p.startswith("/var/lib/snapd/")]:
            del g.nodes[path]
        s.sanitize_k8s(g, "ubuntu-2604", k8s_review(), dict(PARENT))
        self.assertNotIn(s.K8S_SNAPD_STATE, g.nodes)
        # state.json without registration keys is left byte-identical (no gratuitous rewrite).
        g = FakeK8sGuestFS(); g.put(s.K8S_SNAPD_STATE, b'{"data": {"auth": {"device": {"brand": "b"}}}}')
        s.sanitize_k8s(g, "ubuntu-2604", k8s_review(), dict(PARENT))
        self.assertEqual(g.read_file(s.K8S_SNAPD_STATE), b'{"data": {"auth": {"device": {"brand": "b"}}}}')
        # A reset that does not land (state still registered) refuses before zeroing.
        g = FakeK8sGuestFS()
        real_write = g.write
        g.write = lambda path, data: real_write(path, SNAPD_STATE if path == s.K8S_SNAPD_STATE else data)
        with self.assertRaises(ValueError): s.sanitize_k8s(g, "ubuntu-2604", k8s_review(), dict(PARENT))
        self.assertFalse([c for c in g.calls if c[0] == "zero"])

    def test_snapd_state_changed_since_inventory_or_unencodable_refuses(self):
        # state.json changed between the read-only inventory and the rewrite: never overwrite unseen bytes.
        real_cleanup = s.cleanup
        def drift(g, user, patterns):
            real_cleanup(g, user, patterns)
            g.put(s.K8S_SNAPD_STATE, SNAPD_STATE.replace(b'"patch-level":6', b'"patch-level":7'))
        g = FakeK8sGuestFS()
        with patch.object(s, "cleanup", side_effect=drift):
            with self.assertRaises(ValueError): s.sanitize_k8s(g, "ubuntu-2604", k8s_review(), dict(PARENT))
        self.assertIn(b'"patch-level":7', g.read_file(s.K8S_SNAPD_STATE))  # Not overwritten.
        self.assertFalse([c for c in g.calls if c[0] == "zero"])
        # A lone surrogate escape parses but cannot be re-encoded as UTF-8: refused BEFORE any write.
        g = FakeK8sGuestFS()
        g.put(s.K8S_SNAPD_STATE, SNAPD_STATE.replace(b'"generic-classic"', b'"generic-\\ud800"'))
        self.assertEqual(self.refuses(g), "sanitization refused")
        # Surrogate PAIRS (valid UTF-16 escapes) are fine and survive as the same characters.
        g = FakeK8sGuestFS()
        g.put(s.K8S_SNAPD_STATE, SNAPD_STATE.replace(b'"generic-classic"', b'"g-\\ud83d\\ude00"'))
        s.sanitize_k8s(g, "ubuntu-2604", k8s_review(), dict(PARENT))
        self.assertEqual(json.loads(g.read_file(s.K8S_SNAPD_STATE))["data"]["auth"]["device"]["model"], "g-\U0001F600")
        # check() itself re-verifies there is still no store user: the reset write lands a user (nothing else
        # differs: serial/key-id are gone), so ONLY the final users == 0 check can refuse it.
        g = FakeK8sGuestFS()
        real_write = g.write
        g.write = lambda path, data: real_write(path, SNAPD_RESET.replace(b'"users":[]', b'"users":[{"id":1}]')
                                                if path == s.K8S_SNAPD_STATE else data)
        with patch.object(s, "k8s_snapd", wraps=s.k8s_snapd) as probe:
            with self.assertRaises(ValueError) as caught:
                s.sanitize_k8s(g, "ubuntu-2604", k8s_review(), dict(PARENT))
        self.assertEqual(str(caught.exception), "sanitization refused")
        self.assertEqual(probe.call_count, 3)  # Inventory, rewrite preparation, then check() refuses.
        self.assertIn(b'"users":[{"id":1}]', g.read_file(s.K8S_SNAPD_STATE))
        self.assertFalse([c for c in g.calls if c[0] == "zero"])

    def test_snapd_key_dir_must_be_empty_after_cleanup(self):
        g = FakeK8sGuestFS()
        g.put(s.K8S_SNAPD_KEYS + "/k", b"synthetic")
        real_rm_rf = g.rm_rf
        def stubborn(path):  # A wipe that silently leaves the key behind must not pass verification.
            if not path.startswith(s.K8S_SNAPD_KEYS + "/"):
                real_rm_rf(path)
        g.rm_rf = stubborn
        with self.assertRaises(ValueError): s.sanitize_k8s(g, "ubuntu-2604", k8s_review(), dict(PARENT))

    def test_masked_or_redirected_kubelet_refuses(self):
        for change in (lambda g: g.nodes.__setitem__(s.KUBELET_MASK, ("link", "/dev/null")),
                       lambda g: g.nodes.__setitem__(s.KUBELET_WANTS, ("link", "/dev/null")),
                       lambda g: g.nodes.__setitem__(s.KUBELET_WANTS, ("link", "/usr/lib/systemd/system/other.service")),
                       lambda g: g.nodes.pop("/usr/lib/systemd/system/kubelet.service")):
            g = FakeK8sGuestFS(); change(g)
            self.policy_refuses(g)

    def test_readme_dropin_only_with_exact_reviewed_row(self):
        notice = b"# All files in this directory will be read by cloud-init\n"
        g = FakeK8sGuestFS(); g.put("/etc/cloud/cloud.cfg.d/README", notice)
        self.policy_refuses(g)
        row = ("README", hashlib.sha256(notice).hexdigest(), "preserve_vendor")
        s.sanitize_k8s(g, "ubuntu-2604", k8s_review(reviewed_dropins=k8s_review()["reviewed_dropins"] + (row,)), dict(PARENT))
        self.assertEqual(g.read_file("/etc/cloud/cloud.cfg.d/README"), notice)
        g = FakeK8sGuestFS(); g.put("/etc/cloud/cloud.cfg.d/README.txt", notice)
        self.policy_refuses(g, k8s_review(reviewed_dropins=k8s_review()["reviewed_dropins"] + (("README.txt",) + row[1:],)))

    def test_observed_dropin_classes_by_exact_parent_rows(self):
        """Real name classes (synthetic bytes): 05_logging.cfg + README owned by cloud-init-base are
        preserve_vendor; 20-disable-cc-dpkg-grub.cfg + 90_dpkg.cfg are not package-owned -> remove_build.
        Decisions/hashes come only from the parent review rows, never from source constants."""
        files = {"05_logging.cfg": (b"## synthetic logging\n", "preserve_vendor"),
                 "README": (b"# synthetic notice\n", "preserve_vendor"),
                 "20-disable-cc-dpkg-grub.cfg": (b"grub_dpkg: {enabled: false}\n", "remove_build"),
                 "90_dpkg.cfg": (b"datasource_list: [ NoCloud, None ]\n", "remove_build")}
        def fresh():
            g = FakeK8sGuestFS()
            for name in g.ls("/etc/cloud/cloud.cfg.d"):
                del g.nodes["/etc/cloud/cloud.cfg.d/" + name]
            for name, (data, _) in files.items():
                g.put("/etc/cloud/cloud.cfg.d/" + name, data)
            return g
        rows = tuple((n, hashlib.sha256(d).hexdigest(), a) for n, (d, a) in sorted(files.items()))
        g = fresh()
        s.sanitize_k8s(g, "ubuntu-2604", k8s_review(reviewed_dropins=rows), dict(PARENT))
        self.assertEqual(g.ls("/etc/cloud/cloud.cfg.d"), ["05_logging.cfg", "README"])
        for name in ("05_logging.cfg", "README"):
            self.assertEqual(g.read_file("/etc/cloud/cloud.cfg.d/" + name), files[name][0])
        self.policy_refuses(fresh(), k8s_review(reviewed_dropins=rows[:-1]))  # Every observed file needs a row.
        self.policy_refuses(fresh(), k8s_review(reviewed_dropins=tuple(  # Hash must match exactly.
            (n, "f" * 64 if n == "90_dpkg.cfg" else h, a) for n, h, a in rows)))

    def test_root_only_mount_checked_before_mounting(self):
        for mounts in ({"/": "/dev/sda1", "/boot": "/dev/sda2"}, {"/": "/dev/sda1", "/boot/efi": "/dev/sda2"},
                       {"/": "/dev/disk/by-uuid/" + UUID, "/boot": "/dev/sda2"}):
            g = FakeK8sGuestFS(); g.mounts = mounts; g.filesystems["/dev/sda2"] = "ext4"
            self.refuses(g)
            self.assertFalse([c for c in g.calls if c[0] in ("mount", "mount_ro")])

    def test_curtin_by_uuid_root_resolved_exactly_then_mounted_by_device(self):
        g = FakeK8sGuestFS(); g.mounts = {"/": "/dev/disk/by-uuid/" + UUID}  # curtin fstab spec, unresolved.
        s.sanitize_k8s(g, "ubuntu-2604", k8s_review(), dict(PARENT))
        mounts = [c for c in g.calls if c[0] in ("mount", "mount_ro")]
        self.assertTrue(mounts)
        # Every (re)mount, read-only inventory and read-write copy alike, uses the resolved device.
        self.assertEqual(set(mounts), {("mount_ro", "/dev/sda1", "/"), ("mount", "/dev/sda1", "/")})
        self.assertEqual([c for c in g.calls if c[0] == "findfs_uuid"], [("findfs_uuid", UUID)])
        self.assertEqual([c for c in g.calls if c[0] in ("journal", "zero")],
                         [("journal", "/dev/sda1", False), ("zero", "/"), ("journal", "/dev/sda1", True)])
        for spec, uuids in (("/dev/disk/by-label/cloudimg-rootfs", None), ("/dev/disk/by-partuuid/" + UUID, None),
                            ("LABEL=cloudimg-rootfs", None), ("UUID=" + UUID, None),
                            ("/dev/disk/by-uuid/" + UUID.upper(), None),  # Non-canonical case.
                            ("/dev/disk/by-uuid/" + UUID[:-1], None), ("/dev/disk/by-uuid/" + UUID + "/", None),
                            ("/dev/disk/by-uuid/../../sda1", None), ("/dev/sda2", None),
                            ("/dev/disk/by-uuid/" + UUID, {}),  # Unresolvable.
                            ("/dev/disk/by-uuid/" + UUID, {UUID: "/dev/sda2"}),  # Resolves to non-root.
                            ("/dev/disk/by-uuid/" + UUID, {UUID: "/dev/sda"})):
            with self.subTest(spec=spec, uuids=uuids):
                g = FakeK8sGuestFS(); g.mounts = {"/": spec}; g.filesystems["/dev/sda2"] = "ext4"
                if uuids is not None:
                    g.uuids = uuids
                self.assertEqual(self.refuses(g), "sanitization refused")
                self.assertFalse([c for c in g.calls if c[0] in ("mount", "mount_ro")])
                # Only the exact canonical form ever reaches findfs_uuid; every other spec refuses first.
                resolved = [c for c in g.calls if c[0] == "findfs_uuid"]
                self.assertEqual(resolved, [("findfs_uuid", UUID)] if uuids is not None else [])

    def test_held_or_installed_k8s_packages_only(self):
        held = lambda g, name: g.put("/var/lib/dpkg/status", g.read_file("/var/lib/dpkg/status").replace(  # noqa: E731
            b"Package: %s\nStatus: install ok installed" % name, b"Package: %s\nStatus: hold ok installed" % name))
        g = FakeK8sGuestFS()
        for name in (b"kubeadm", b"kubelet", b"kubectl"):  # image-builder apt-marks all three hold.
            held(g, name)
        self.assertEqual(g.read_file("/var/lib/dpkg/status").count(b"Status: hold ok installed"), 3)
        s.sanitize_k8s(g, "ubuntu-2604", k8s_review(), dict(PARENT))
        for status in (b"deinstall ok installed", b"purge ok installed", b"hold ok half-installed",
                       b"hold reinstreq installed", b"install ok unpacked", b"hold ok installed ",
                       b"Hold ok installed", b"hold  ok installed"):
            with self.subTest(status=status):
                g = FakeK8sGuestFS()
                g.put("/var/lib/dpkg/status", g.read_file("/var/lib/dpkg/status").replace(
                    b"Package: kubelet\nStatus: install ok installed", b"Package: kubelet\nStatus: " + status))
                self.refuses(g)
        g = FakeK8sGuestFS(); held(g, b"kubelet")  # Version still must be exactly the pinned deb release.
        g.put("/var/lib/dpkg/status", g.read_file("/var/lib/dpkg/status").replace(
            b"Package: kubelet\nStatus: hold ok installed\nVersion: 1.35.10-1.1",
            b"Package: kubelet\nStatus: hold ok installed\nVersion: 1.35.10-2.1"))
        self.policy_refuses(g)

    def test_static_manifests_exclude_only_empty_kubelet_keep_placeholder(self):
        keep = "/etc/kubernetes/manifests/.kubelet-keep"
        g = FakeK8sGuestFS(); g.put(keep, b"")  # image-builder placeholder, as observed.
        s.sanitize_k8s(g, "ubuntu-2604", k8s_review(), dict(PARENT))
        self.assertEqual(g.read_file(keep), b"")  # Retained, never wiped.
        for change in (lambda g: g.put(keep, b"\n"),  # Non-empty placeholder.
                       lambda g: g.nodes.__setitem__(keep, ("link", "/dev/null")),
                       lambda g: g.mkdir_p(keep),
                       lambda g: (g.put(keep, b""), g.put("/etc/kubernetes/manifests/kube-vip.yaml", b"")),
                       lambda g: g.put("/etc/kubernetes/manifests/.kubelet-keep.yaml", b""),
                       lambda g: g.put("/etc/kubernetes/manifests/kubelet-keep", b""),
                       lambda g: g.put("/etc/kubernetes/manifests/.Kubelet-keep", b""),
                       lambda g: g.nodes.__setitem__("/etc/kubernetes/manifests", ("link", "/srv/manifests"))):
            g = FakeK8sGuestFS(); change(g)
            self.policy_refuses(g)

    def test_guestfs_int_predicates_are_cast_to_policy_bools(self):
        # Real-image defect: drop-in regular_no_symlinks reached the policy as int 1 and refused.
        g = IntBoolK8sGuestFS()
        _, facts, _ = s.k8s_inventory(g, "ubuntu-2604", k8s_review(), dict(PARENT))
        self.assertTrue(facts["cloud_dropins"])
        for d in facts["cloud_dropins"]:
            self.assertIs(d["regular_no_symlinks"], True)
        self.assertIs(facts["k8s"]["kubelet_enabled"], True)
        self.assertIs(facts["layout"]["external_fstab"], False)
        for key in ("swap_entries", "unsafe_links"):
            self.assertIs(type(facts["layout"][key]), int)
        for value in list(facts["k8s"]["cluster"].values()) + list(facts["hygiene"].values()):
            self.assertIs(type(value), int)
        g = IntBoolK8sGuestFS()
        s.sanitize_k8s(g, "ubuntu-2604", k8s_review(), dict(PARENT))
        self.assertIs(s.exists(IntBoolK8sGuestFS(), "/etc"), True)
        self.assertIs(s.exists(IntBoolK8sGuestFS(), "/nonexistent"), False)

    def test_layout_package_kubelet_network_and_hygiene_unknowns_refuse(self):
        changes = [lambda g: setattr(g, "parttype", "gpt"), lambda g: g.partitions.append({"part_num": 2}),
                   lambda g: g.filesystems.update({"/dev/sda2": "vfat"}), lambda g: setattr(g, "minor", 10),
                   lambda g: g.put("/etc/fstab", b"UUID=x / ext4 defaults 0 1\nnfs:/x /mnt nfs defaults 0 0\n"),
                   lambda g: g.nodes.pop(s.KUBELET_WANTS),  # Disabled kubelet is not silently re-enabled.
                   lambda g: g.put("/var/lib/dpkg/status", g.read_file("/var/lib/dpkg/status").replace(
                       b"Package: kubelet\nStatus: install ok installed\nVersion: 1.35.10-1.1",
                       b"Package: kubelet\nStatus: install ok installed\nVersion: 1.35.11-1.1")),
                   lambda g: g.put("/usr/sbin/NetworkManager", b""),
                   lambda g: g.put("/etc/apt/sources.list.d/private.list", b"deb https://user:pw@mirror.test/ /\n"),
                   lambda g: g.put("/etc/containerd/config.toml", b'[registry.configs."r".auth]\npassword = "x"\n'),
                   lambda g: g.put("/etc/kubernetes/extra.yaml", b"client-key-data: synthetic\n"),
                   lambda g: g.nodes.__setitem__("/etc/kubernetes", ("link", "/srv"))]
        for index, change in enumerate(changes):
            with self.subTest(index=index):
                g = FakeK8sGuestFS(); change(g)
                self.refuses(g)

    def test_review_and_parent_facts_fail_closed(self):
        for key, value in (("kubernetes_version", "1.35.11"), ("kubelet_active", True), ("bios", "ovmf"),
                           ("cloud_default_user", "Ubuntu"),
                           ("cloud_default_user", "builder"), ("cloud_default_user", "root"),
                           ("network_backend", "UNKNOWN"),
                           ("cloud_base_config_reviewed", False), ("retained_package_policy_reviewed", 1),
                           ("source_hash_verified", False)):
            with self.subTest(key=key):
                self.refuses(FakeK8sGuestFS(), k8s_review(**{key: value}))
        for key, value in (("format", "qcow2"), ("raw_bytes", s.VIRTUAL_BYTES - 1), ("source_stopped", False)):
            with self.subTest(parent=key):
                self.refuses(FakeK8sGuestFS(), parent=dict(PARENT, **{key: value}))
        with self.assertRaises(ValueError): s.sanitize_k8s(FakeK8sGuestFS(), "ubuntu-2404", k8s_review(), dict(PARENT))

    def test_review_file_schema_and_raw_metadata(self):
        raw = json.dumps(dict(k8s_review(), reviewed_dropins=[list(r) for r in k8s_review()["reviewed_dropins"]])).encode()
        self.assertEqual(s.load_review(raw)["reviewed_dropins"], k8s_review()["reviewed_dropins"])
        for bad in (raw.replace(b'"bios"', b'"bios_x"'), b'{"bios": 1, "bios": 2}', b"", b"[]",
                    raw.replace(b'"reviewed_dropins": [', b'"reviewed_dropins": [["x.cfg"], '),
                    raw.replace(b'"' + b"e" * 64 + b'"', b'"' + b"E" * 64 + b'"'),
                    raw.replace(b'"' + b"e" * 64 + b'"', b'"sha256:' + b"e" * 64 + b'"'),
                    raw.replace(b'"source_raw_sha256": "' + b"e" * 64 + b'", ', b'')):
            with self.subTest(bad=bad[:24]), self.assertRaises(ValueError): s.load_review(bad)
        clean = {"format": "raw", "virtual-size": s.VIRTUAL_BYTES}
        for fields in ({"format": "qcow2"}, {"virtual-size": s.VIRTUAL_BYTES + 512}, {"backing-filename": "/x"},
                       {"data-file": "/x"}, {"encrypted": True}):
            with self.subTest(fields=fields), patch.object(s, "command", return_value=json.dumps(dict(clean, **fields)).encode()):
                with self.assertRaises(ValueError): s.raw_info(s.RAW_SOURCE)
        with patch.object(s, "command", return_value=json.dumps(clean).encode()) as run:
            self.assertEqual(s.raw_info(s.RAW_SOURCE), clean)
            self.assertEqual(run.call_args.args, ("info", "--output=json", "-f", "raw", s.RAW_SOURCE))

    def main_fixture(self, raw=b"\0" * 4096, digest=None, size=4096):
        """Synthetic tiny 'raw' file with VIRTUAL_BYTES patched; never a real disk or qemu run."""
        tmp = tempfile.TemporaryDirectory(); self.addCleanup(tmp.cleanup)
        root = Path(tmp.name)
        (root / "source.raw").write_bytes(raw)
        review = dict(k8s_review(source_raw_sha256=digest or hashlib.sha256(raw).hexdigest()),
                      reviewed_dropins=[list(r) for r in k8s_review()["reviewed_dropins"]])
        (root / "review.json").write_bytes(json.dumps(review).encode())
        for mocked in (patch.object(s, "RAW_SOURCE", str(root / "source.raw")),
                       patch.object(s, "K8S_REVIEW", str(root / "review.json")),
                       patch.object(s, "VIRTUAL_BYTES", size), patch.object(s.os, "getuid", return_value=1002),
                       patch.object(s.os, "geteuid", return_value=1002), patch.object(s.os, "umask")):
            mocked.start(); self.addCleanup(mocked.stop)
        return root

    def run_main(self):
        """mkdir is patched ONLY around main(): it marks 'source checks passed' and stops there."""
        err = io.StringIO()
        with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(err), \
                patch.object(s, "command") as run, \
                patch.object(s.os, "mkdir", side_effect=RuntimeError("stop after source checks")) as mkdir:
            self.assertEqual(s.main(["--k8s-os", "ubuntu-2604"]), 2)
        self.assertEqual(err.getvalue(), "sanitization refused\n")
        return run, mkdir

    def test_main_runs_only_as_a_non_root_host_uid(self):
        """The capsule runs as the invoking host user: real == effective, >= 1000, not only 1000."""
        for uid, euid, accepted in ((1002, 1002, True), (1000, 1000, True), (65534, 65534, True), (0, 0, False),
                                    (999, 999, False), (1002, 0, False), (0, 1002, False), (1002, 1000, False)):
            with self.subTest(uid=uid, euid=euid):
                self.main_fixture()
                with patch.object(s.os, "getuid", return_value=uid), patch.object(s.os, "geteuid", return_value=euid), \
                        patch.object(s, "raw_digest", wraps=s.raw_digest) as digest:
                    _, mkdir = self.run_main()
                self.assertEqual((digest.called, mkdir.called), (accepted, accepted))  # Refused before any read.

    def test_main_k8s_source_binding_refuses_before_any_tool(self):
        root = self.main_fixture()
        with patch.object(s, "raw_digest", wraps=s.raw_digest) as digest:
            run, mkdir = self.run_main()  # Valid inputs reach mkdir (patched to stop) only after hashing.
        digest.assert_called_once()
        mkdir.assert_called_once()
        run.assert_not_called()
        for case in ("mismatch", "size", "raw_link", "review_link", "raw_hardlink", "review_bad"):
            with self.subTest(case=case):
                root = self.main_fixture(digest="0" * 64) if case == "mismatch" else \
                    self.main_fixture(size=4097) if case == "size" else self.main_fixture()
                if case == "raw_link":
                    (root / "real.raw").write_bytes((root / "source.raw").read_bytes())
                    (root / "source.raw").unlink(); (root / "source.raw").symlink_to(root / "real.raw")
                elif case == "review_link":
                    (root / "real.json").write_bytes((root / "review.json").read_bytes())
                    (root / "review.json").unlink(); (root / "review.json").symlink_to(root / "real.json")
                elif case == "raw_hardlink":
                    os.link(root / "source.raw", root / "alias.raw")
                elif case == "review_bad":
                    (root / "review.json").write_bytes(b'{"kubernetes_version": "1.35.10"}')
                run, mkdir = self.run_main()
                mkdir.assert_not_called()  # Refused before scratch creation, raw_info or convert.
                run.assert_not_called()

    def test_main_k8s_convert_only_after_raw_info(self):
        self.main_fixture()
        info = json.dumps({"format": "raw", "virtual-size": 4096}).encode()
        calls = []
        def fake(*args):
            calls.append(args[:2])
            if args[:3] == ("info", "--output=json", "-f") and args[3] == "raw":
                return info
            raise RuntimeError("stop after convert")
        with patch.object(s.os, "mkdir"), patch.object(s, "command", side_effect=fake), \
                contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(s.main(["--k8s-os", "ubuntu-2604"]), 2)
        self.assertEqual(calls, [("info", "--output=json"), ("convert", "-f")])
        calls.clear()
        with patch.object(s.os, "mkdir"), patch.object(s, "command", side_effect=lambda *a: (calls.append(a[:2]), json.dumps({"format": "qcow2", "virtual-size": 4096}).encode())[1]), \
                contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(s.main(["--k8s-os", "ubuntu-2604"]), 2)
        self.assertEqual(calls, [("info", "--output=json")])  # Non-raw metadata: never converted.

    def test_k8s_cli_selector_is_narrow_and_refuses_without_execution(self):
        for args in (["--k8s-os", "ubuntu-2404"], ["--k8s-os", "debian-13"], ["--os", "ubuntu-2604", "--k8s-os", "ubuntu-2604"], []):
            with contextlib.redirect_stderr(io.StringIO()), patch.object(s, "command") as run:
                with self.assertRaises(SystemExit): s.main(args)
                run.assert_not_called()
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err), patch.object(s.os, "getuid", return_value=0), patch.object(s, "command") as run:
            self.assertEqual(s.main(["--k8s-os", "ubuntu-2604"]), 2)
            run.assert_not_called()
        self.assertEqual((out.getvalue(), err.getvalue()), ("", "sanitization refused\n"))



class FakeSnapdGuestFS(FakeGuestFS):
    """Native Ubuntu layout plus a registered snapd identity from image seeding (synthetic values)."""
    def __init__(self, selected="ubuntu-2404"):
        super().__init__(selected)
        self.nlinks = {}
        self.put(s.K8S_SNAPD_KEYS + "/synthetic-device-key-id", b"synthetic device key bytes\n")
        self.put(SERIAL + "/generic/generic-classic/synthetic-serial/active", b"type: serial\n")
        self.put(s.K8S_SNAPD_STATE, SNAPD_STATE)
        self.put("/var/lib/snapd/seed/seed.yaml", b"snaps: []\n")
        self.calls.clear()
    def lstatns(self, path):
        assert path in self.nodes
        return {"st_nlink": self.nlinks.get(path, 1)}


class NativeSnapdResetTests(unittest.TestCase):
    def test_wipe_patterns_equal_kubernetes_copy_policy(self):
        self.assertEqual(s.SNAPD_WIPE, tuple(p for p in k8s_copy_policy.COPY_WIPE if p.startswith("/var/lib/snapd/")))

    def test_native_os_resets_registered_snapd_identity(self):
        for selected in ("ubuntu-2404", "ubuntu-2604"):
            with self.subTest(selected=selected):
                g = FakeSnapdGuestFS(selected)
                s.sanitize(g, selected)
                self.assertEqual(g.read_file(s.K8S_SNAPD_STATE), SNAPD_RESET)
                self.assertEqual(g.ls(s.K8S_SNAPD_KEYS), [])
                self.assertFalse([p for p in g.nodes if p.startswith(SERIAL)])
                self.assertIn("/var/lib/snapd/seed/seed.yaml", g.nodes)  # Seed and snaps are kept.
                s.snapd_unregistered(g)  # The postscan guard passes on the result.

    def test_store_credentials_and_unwipeable_keys_refuse_before_any_write(self):
        changes = (lambda g: g.put(s.K8S_SNAPD_STATE, SNAPD_STATE.replace(
                       b'"key-id":"synthetic-key-id"}', b'"key-id":"synthetic-key-id","session-macaroon":"m"}')),
                   lambda g: g.put(s.K8S_SNAPD_STATE, SNAPD_STATE.replace(b'"users":[]', b'"users":[{"id":1}]')),
                   lambda g: g.put(s.K8S_SNAPD_KEYS + "/.hidden", b"synthetic"),
                   lambda g: g.nlinks.__setitem__(s.K8S_SNAPD_KEYS + "/synthetic-device-key-id", 2),
                   lambda g: g.put(s.K8S_SNAPD_STATE, b"{"))
        for change in changes:
            g = FakeSnapdGuestFS()
            change(g)
            g.calls.clear()
            with self.assertRaisesRegex(ValueError, "^sanitization refused$"):
                s.sanitize(g, "ubuntu-2404")
            self.assertFalse([c for c in g.calls if c[0] in ("rm", "rm_rf", "zero")])

    def test_reset_that_does_not_land_refuses_before_zeroing(self):
        g = FakeSnapdGuestFS()
        real_write = g.write
        g.write = lambda path, data: real_write(path, SNAPD_STATE if path == s.K8S_SNAPD_STATE else data)
        with self.assertRaises(ValueError):
            s.sanitize(g, "ubuntu-2404")
        self.assertFalse([c for c in g.calls if c[0] == "zero"])

    def test_unseeded_image_is_untouched(self):
        g = FakeGuestFS("debian-13")
        s.sanitize(g, "debian-13")
        self.assertFalse([p for p in g.nodes if p.startswith("/var/lib/snapd")])


if __name__ == "__main__":
    unittest.main()
