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


PRESERVE = b"#cloud-config\nsystem_info: {default_user: {name: builder, lock_passwd: true}}\n"
REMOVE_BUILD = b"network: {config: disabled}\n"


class FakeK8sGuestFS(FakeGuestFS):
    """Synthetic single-partition msdos/seabios Kubernetes node layout; NOT template100 facts."""
    def __init__(self):
        super().__init__("ubuntu-2604")
        for path in [p for p in self.nodes if p == "/home/ubuntu" or p.startswith("/home/ubuntu/")]:
            del self.nodes[path]
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
        self.put("/etc/cloud/cloud.cfg.d/90-builder-user.cfg", PRESERVE)
        self.put("/etc/cloud/cloud.cfg.d/99-disable-network-config.cfg", REMOVE_BUILD)
        self.put("/home/builder/.ssh/authorized_keys", b"ssh-ed25519 AAAA synthetic\n")
        self.calls.clear()
    def part_get_parttype(self, device): return self.parttype
    def part_list(self, device): return self.partitions
    def readlink(self, path): return self.nodes[path][1]


def k8s_review(**changes):
    review = dict(kubernetes_version="1.35.10", source_raw_sha256="e" * 64, source_hash_verified=True, source_stopped=True, bios="seabios",
                  kubelet_active=False, runtime_version="2.1.0", retained_package_policy_reviewed=True,
                  package_policy_sha256="c" * 64, network_backend="netplan-systemd-networkd",
                  cloud_default_user="builder", cloud_base_config_reviewed=True,
                  reviewed_dropins=(("90-builder-user.cfg", hashlib.sha256(PRESERVE).hexdigest(), "preserve_vendor"),
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
        self.assertEqual(g.ls("/etc/cloud/cloud.cfg.d"), ["90-builder-user.cfg"])
        self.assertEqual(g.read_file("/etc/cloud/cloud.cfg.d/90-builder-user.cfg"), PRESERVE)
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
        g = FakeK8sGuestFS(); g.put("/etc/cloud/cloud.cfg.d/90-builder-user.cfg", PRESERVE + b"# changed\n")
        self.refuses(g)
        g = FakeK8sGuestFS(); g.nodes["/etc/cloud/cloud.cfg.d/90-builder-user.cfg"] = ("link", "/etc/shadow")
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

    def test_swap_files_fstab_rows_and_units_refuse_never_wipe(self):
        for change in (lambda g: g.put("/swap.img", b"\0"), lambda g: g.put("/swapfile", b"\0"),
                       lambda g: g.nodes.__setitem__("/swap.img", ("link", "/dev/sda2")),
                       lambda g: g.put("/etc/fstab", g.read_file("/etc/fstab") + b"/swap.img none swap sw 0 0\n"),
                       lambda g: g.put("/etc/fstab", g.read_file("/etc/fstab") + b"#/swap.img\tnone\tswap\tsw\t0\t0\n"),
                       lambda g: g.put("/etc/fstab", g.read_file("/etc/fstab") + b"  # /swapfile none swap sw 0 0\n"),
                       lambda g: g.put("/etc/systemd/system/swap.img.swap", b"[Swap]\nWhat=/swap.img\n")):
            g = FakeK8sGuestFS(); change(g)
            self.policy_refuses(g)
        g = FakeK8sGuestFS(); g.put("/etc/fstab", g.read_file("/etc/fstab") + b"# no swapping here\n")
        s.sanitize_k8s(g, "ubuntu-2604", k8s_review(), dict(PARENT))  # Prose mentioning swap is not an entry.

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
                    lambda g: g.put("/var/lib/snapd/device/private-keys-v1/key", b"synthetic")]
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

    def test_root_only_mount_checked_before_mounting(self):
        for mounts in ({"/": "/dev/sda1", "/boot": "/dev/sda2"}, {"/": "/dev/sda1", "/boot/efi": "/dev/sda2"}):
            g = FakeK8sGuestFS(); g.mounts = mounts; g.filesystems["/dev/sda2"] = "ext4"
            self.refuses(g)
            self.assertFalse([c for c in g.calls if c[0] == "mount"])

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
                           ("cloud_default_user", "ubuntu"), ("network_backend", "UNKNOWN"),
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
                       patch.object(s, "VIRTUAL_BYTES", size), patch.object(s.os, "getuid", return_value=1000),
                       patch.object(s.os, "geteuid", return_value=1000), patch.object(s.os, "umask")):
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


if __name__ == "__main__":
    unittest.main()
