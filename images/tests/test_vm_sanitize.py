"""Source-only, NOT run. Pure in-memory GuestFS fake; no commands, disk images or appliances.

These tests prove policy/call ordering only, never actual libguestfs or journal erasure.
"""
import contextlib
import copy
import fnmatch
import importlib.util
import io
from pathlib import Path
import re
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("vm_sanitize", Path(__file__).resolve().parents[1] / "vm_sanitize.py")
s = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(s)


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


if __name__ == "__main__":
    unittest.main()
