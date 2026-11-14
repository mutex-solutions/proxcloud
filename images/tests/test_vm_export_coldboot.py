"""Pure tests for images/vm_export/coldboot.py: a fake PVE (token view: pool ACL only), a fake ssh/ssh-keygen
and one loopback HTTP server for the client's streaming upload. No PVE, no guest, no real SSH."""
import argparse
import base64
import contextlib
import hashlib
import http.server
import io
import json
import os
from pathlib import Path
import re
import signal
import ssl
import subprocess
import sys
import tempfile
import threading
import types
import unittest
import urllib.parse
from unittest.mock import patch

IMAGES = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(IMAGES))
from vm_export import coldboot, coldboot_guest  # noqa: E402

NODE, STORAGE, POOL = "pve1", "pc-ci-images", "pc-templates"
ADDRESSES = ("192.0.2.50/24", "192.0.2.51/24")
DISK = b"sealed sanitized qcow2 bytes " * 4096
SHA = hashlib.sha256(DISK).hexdigest()
VOLID = "%s:import/pc-export-%s.qcow2" % (STORAGE, SHA[:12])
TAG = "pc-artifact-" + SHA[:12]


def sha(text):
    return hashlib.sha256(text.encode()).hexdigest()


class FakePVE:
    """What the least-privilege token sees: only guests in the pool; anything else answers 403, a guest
    joins its pool when its create task ends, and a create carrying more than vmid+pool is refused."""
    tls_verified = False

    def __init__(self):
        self.vms, self.volumes, self.calls, self.mutations, self.uploads = {}, [], [], [], []
        self.pending, self.fail, self.count, self.hook = {}, {}, 0, None

    def add(self, vmid, pool=POOL, status="stopped", **config):
        self.vms[vmid] = {"pool": pool, "status": status, "config": dict(config)}

    def digest(self, vmid):
        return hashlib.sha1(json.dumps(self.vms[vmid]["config"], sort_keys=True).encode()).hexdigest()

    def upid(self, kind, ident="", done=None):
        self.count += 1
        upid = "UPID:%s:%08X:%08X:%08X:%s:%s:images@pve!packer:" % (NODE, self.count, 1, 2, kind, ident)
        self.pending[upid] = done
        return upid

    def vm(self, vmid):
        vm = self.vms.get(int(vmid))
        if vm is None or vm["pool"] != POOL:
            raise coldboot.ApiError(403)  # Outside the pool and missing look the same.
        return vm

    def upload(self, node, storage, name, path, sha256, size):
        self.mutations.append(("UPLOAD", name, {}))
        assert (node, storage) == (NODE, STORAGE) and hashlib.sha256(Path(path).read_bytes()).hexdigest() == sha256
        self.uploads.append(name)
        return self.upid("imgcopy", done=lambda: self.volumes.append("%s:import/%s" % (storage, name)))

    def call(self, method, path, **params):
        self.calls.append((method, path, dict(params)))
        if self.hook:
            self.hook(method, path, params)
        if method != "GET":
            self.mutations.append((method, path, dict(params)))
        qemu = re.fullmatch(r"/nodes/%s/qemu/([0-9]+)(/.*)?" % NODE, path)
        if (method, path) == ("GET", "/cluster/resources"):
            return [{"vmid": i, "node": NODE, "type": "qemu", "status": v["status"], "pool": POOL}
                    for i, v in sorted(self.vms.items()) if v["pool"] == POOL]
        if (method, path) == ("GET", "/cluster/nextid"):
            if int(params["vmid"]) in self.vms:
                raise coldboot.ApiError(400)
            return str(params["vmid"])
        if (method, path) == ("GET", "/pools/" + POOL):
            return {"members": [{"vmid": i, "node": NODE, "type": "qemu"} for i, v in self.vms.items() if v["pool"] == POOL]}
        if path == "/nodes/%s/storage/%s/content" % (NODE, STORAGE):
            return [{"volid": v} for v in self.volumes]
        if method == "DELETE" and path.startswith("/nodes/%s/storage/%s/content/" % (NODE, STORAGE)):
            self.volumes.remove(urllib.parse.unquote(path.rsplit("/", 1)[1]))
            return self.upid("imgdel")
        if method == "GET" and path.startswith("/nodes/%s/tasks/" % NODE):
            upid = urllib.parse.unquote(path.split("/")[4])
            done = self.pending.pop(upid, None)
            status = self.fail.get(upid.split(":")[5], "OK")
            if done and status == "OK":
                done()
            return {"status": "stopped", "exitstatus": status}
        if (method, path) == ("POST", "/nodes/%s/qemu" % NODE):
            vmid = int(params["vmid"])
            if set(params) != {"vmid", "pool"}:
                raise coldboot.ApiError(403)  # Checked on /vms/<id>, where the token has nothing.
            if vmid in self.vms:
                raise coldboot.ApiError(500)
            self.add(vmid, pool=None)
            return self.upid("qmcreate", vmid, lambda: self.vms[vmid].update(pool=params["pool"]))
        vmid, tail = int(qemu.group(1)), qemu.group(2) or ""
        vm = self.vm(vmid)
        config = vm["config"]
        if method == "GET":
            return {"/config": dict(config, digest=self.digest(vmid)), "/status/current": {"status": vm["status"]},
                    "/snapshot": [{"name": n} for n in vm.get("snapshots", ())] + [{"name": "current"}]}[tail]
        if "digest" in params and params.pop("digest") != self.digest(vmid):
            raise coldboot.ApiError(500)
        if (method, tail) == ("POST", "/config"):
            imported = "import-from=" in str(params.get("scsi0"))
            for key, value in params.items():
                if key == "scsi0" and imported:
                    assert value == "%s:0,discard=on,import-from=%s" % (STORAGE, VOLID) and VOLID in self.volumes
                    value = "%s:%d/vm-%d-disk-0.qcow2,discard=on,size=20G" % (STORAGE, vmid, vmid)
                elif key == "ide2":
                    value = "%s:%d/vm-%d-cloudinit.qcow2,media=cdrom" % (STORAGE, vmid, vmid)
                elif key == "net0":
                    value = str(value).replace("virtio,", "virtio=BC:24:11:00:00:%02X," % (vmid % 256))
                config[key] = value if type(value) is int else str(value)
            return self.upid("qmconfig", vmid) if imported else None
        if (method, tail) == ("PUT", "/resize"):
            config["scsi0"] = config["scsi0"].replace("size=20G", "size=" + params["size"])
            return None
        if method == "POST" and tail in ("/status/start", "/status/stop"):
            vm["status"] = "running" if tail.endswith("start") else "stopped"
            return self.upid("qm" + tail.rsplit("/", 1)[1], vmid)
        if (method, tail) == ("DELETE", ""):
            assert params == {"purge": 1} and vm["status"] == "stopped"
            return self.upid("qmdestroy", vmid, lambda: self.vms.pop(vmid))
        raise AssertionError((method, path))

    def touched(self, vmid):
        return [m for m in self.mutations if re.search(r"/qemu/%d(/|$)" % vmid, m[1]) or m[2].get("vmid") == vmid]


class FakeHost:
    """ssh-keygen and ssh: a clone answers only while it runs with this job's address and key."""

    def __init__(self, pve):
        self.pve, self.argvs, self.rows, self.on_guest, self.guest_rc, self.down = pve, [], {}, None, 0, 0

    def row(self, variant, address):
        names = coldboot_guest.COMMON + (coldboot_guest.K8S if variant == "kubernetes" else ())
        return {"checks": {name: True for name in names}, "machine_id_sha256": sha("machine" + address),
                "host_key_sha256": sha(self.host_key(address)), "snapd_present": True,
                "snapd_serial_sha256": sha("serial" + address), "snapd_device_key_sha256": sha("key" + address),
                "snapd_changes_error_lines": 0, "cloud_init_recoverable_warnings": 0}

    @staticmethod
    def host_key(address):
        return base64.b64encode(hashlib.sha256(address.encode()).digest()).decode()

    def __call__(self, argv, **kwargs):
        self.argvs.append(list(argv))
        if argv[0] == "ssh-keygen":
            key = Path(argv[argv.index("-f") + 1])
            os.close(os.open(key, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600))
            key.write_text("-----BEGIN OPENSSH PRIVATE KEY-----\nfake\n")
            Path(str(key) + ".pub").write_text("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFakeJobKey %s\n" % coldboot.KEY_COMMENT)
            return subprocess.CompletedProcess(argv, 0)
        assert argv[0] == "ssh" and kwargs.get("stderr") == subprocess.DEVNULL
        address = next(a for a in argv if a.startswith("ubuntu@")).split("@")[1]
        known = Path(next(a for a in argv if a.startswith("UserKnownHostsFile=")).split("=", 1)[1])
        remote = argv[argv.index("--") + 1:]
        public = Path(argv[argv.index("-i") + 1] + ".pub").read_text().strip()
        booted = [v for v in self.pve.vms.values() if v["status"] == "running"
                  and v["config"].get("ipconfig0", "").startswith("ip=%s/" % address)
                  and urllib.parse.unquote(v["config"].get("sshkeys", "")) == public]
        if not booted or self.down:
            self.down = max(0, self.down - 1)  # Refused connections left before sshd answers.
            return subprocess.CompletedProcess(argv, 255, b"")
        if not known.read_text():
            known.write_text("%s ssh-ed25519 %s\n" % (address, self.host_key(address)))  # accept-new
        if remote == ["true"]:
            return subprocess.CompletedProcess(argv, 0, b"")
        assert remote[:5] == ["sudo", "-n", "/usr/bin/python3", "-I", "-"] and kwargs["input"].startswith(b"#!/usr/bin/env python3")
        if self.on_guest:
            self.on_guest(address)
        row = dict(self.row(remote[5], address), **self.rows.get(address, {}))
        return subprocess.CompletedProcess(argv, self.guest_rc, json.dumps(row).encode() + b"\n")


class ColdBootCase(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        self.sdir = self.root / "state"
        self.sdir.mkdir(mode=0o700)
        self.disk = self.root / "disk.qcow2"
        self.disk.write_bytes(DISK)
        self.disk.chmod(0o400)
        self.record(sealed_after_cgroup_stop=True)
        self.pve = FakePVE()
        self.pve.add(100, name="pc-ubuntu-2604-k8s", template=1, tags="proxcloud;pc-public", ipconfig0="ip=192.0.2.9/24,gw=192.0.2.1")
        self.pve.add(9205, status="running", name="smoke-other", net0="virtio=AA:BB:CC:00:00:01,bridge=vmbr0")
        self.host = FakeHost(self.pve)
        self.sleep = patch.object(coldboot.time, "sleep")
        # One SSH attempt unless a test asks for more: a broken run must fail here, not spin for minutes.
        for mocked in (patch.object(coldboot.subprocess, "run", self.host), patch.object(coldboot, "SSH_DEADLINE", 0)):
            mocked.start()
            self.addCleanup(mocked.stop)
        self.slept = self.sleep.start()
        self.addCleanup(self.sleep.stop)

    def record(self, variant="kubernetes", os_name="ubuntu-2604", version="1.37.2", **changes):
        facts = {"kind": "OfflineSanitizationFacts", "variant": variant, "os": os_name, "disk_sha256": SHA,
                 "disk_bytes": len(DISK), "virtual_size_bytes": 20 << 30}
        if version:
            facts["kubernetes_version"] = version
        run = dict({"sealed_after_cgroup_stop": True, "facts": facts, "disk": str(self.disk)}, **changes)
        (self.root / "sanitize-run.json").write_text(json.dumps(run))

    def args(self, **changes):
        values = dict(sanitize_run=str(self.root / "sanitize-run.json"), variant="kubernetes", os="ubuntu-2604",
                      kubernetes_version="1.37.2", node=NODE, storage=STORAGE, pool=POOL, bridge="vmbr0", vlan=242,
                      test_address=list(ADDRESSES), gateway="192.0.2.1", nameserver=["1.1.1.1", "8.8.8.8"],
                      vmid_range="9290-9299")
        values.update(changes)
        return argparse.Namespace(**values)

    def run_job(self, **changes):
        return coldboot.run(self.pve, coldboot.settings(self.args(**changes)), self.sdir)

    def state(self):
        return json.loads((self.sdir / coldboot.STATE).read_text())

    def assert_clean(self, result, clones=("destroyed", "destroyed"), volume="deleted"):
        self.assertEqual((sorted(result["cleanup"]["clones"].values()), result["cleanup"]["import_volume"]),
                         (sorted(clones), volume))
        self.assertTrue(result["cleanup"]["private_key_deleted"])
        self.assertFalse((self.sdir / coldboot.KEY).exists() or (self.sdir / (coldboot.KEY + ".pub")).exists())
        self.assertEqual(sorted(self.pve.vms), [100, 9205])
        self.assertEqual(self.pve.volumes, [])


class HappyPathTests(ColdBootCase):
    def test_kubernetes_acceptance_end_to_end(self):
        before = {vmid: self.pve.digest(vmid) for vmid in self.pve.vms}
        result = self.run_job()
        self.assertEqual((result["accepted"], result["reason"], result["step"]), (True, None, "done"))
        self.assert_clean(result)
        self.assertEqual(result["protected_set"], {"guests": 2, "unchanged": True})
        self.assertEqual({vmid: self.pve.digest(vmid) for vmid in self.pve.vms}, before)
        self.assertEqual(self.pve.touched(100) + self.pve.touched(9205), [])
        self.assertEqual([(c["vmid"], c["name"], c["all_passed"]) for c in result["clones"]],
                         [(9290, "pc-export-test-%s-a" % SHA[:8], True), (9291, "pc-export-test-%s-b" % SHA[:8], True)])
        self.assertEqual(set(result["clones"][0]["checks"]), set(coldboot_guest.COMMON + coldboot_guest.K8S))
        self.assertEqual(result["distinct_identity"], dict.fromkeys(
            ("machine_id_sha256", "host_key_sha256", "snapd_serial_sha256", "snapd_device_key_sha256"), True))
        self.assertEqual((result["import_volume"], result["disk_sha256"], result["pve_tls_verified"]), (VOLID, SHA, False))
        self.assertEqual(self.pve.uploads, ["pc-export-%s.qcow2" % SHA[:12]])
        self.assertEqual(self.state()["phase"], "finished")
        text = json.dumps(result)  # Hashes, booleans and names only: no address, key or guest text.
        self.assertFalse(re.search(r"192\.0\.2\.|ssh-ed25519|PRIVATE KEY|AAAA", text))

    def test_create_is_empty_and_pool_only_then_name_and_tags_then_config(self):
        self.run_job()
        creates = [m for m in self.pve.mutations if m[:2] == ("POST", "/nodes/%s/qemu" % NODE)]
        self.assertEqual([m[2] for m in creates], [{"vmid": 9290, "pool": POOL}, {"vmid": 9291, "pool": POOL}])
        writes = [m[2] for m in self.pve.mutations if m[1] == "/nodes/%s/qemu/9290/config" % NODE]
        self.assertEqual(set(writes[0]), {"digest", "name", "tags"})
        self.assertEqual(writes[0]["tags"], "proxcloud;pc-artifact-test;" + TAG)
        self.assertEqual({k: writes[1][k] for k in ("scsihw", "serial0", "vga", "boot", "net0", "ide2", "scsi0")},
                         {"scsihw": "virtio-scsi-single", "serial0": "socket", "vga": "serial0", "boot": "order=scsi0",
                          "net0": "virtio,bridge=vmbr0,tag=242", "ide2": STORAGE + ":cloudinit",
                          "scsi0": "%s:0,discard=on,import-from=%s" % (STORAGE, VOLID)})
        self.assertFalse({"bios", "efidisk0", "tpmstate0", "cipassword"} & set().union(*writes))
        self.assertEqual({k: v for k, v in writes[2].items() if k != "digest" and k != "sshkeys"},
                         {"ciuser": "ubuntu", "ipconfig0": "ip=192.0.2.50/24,gw=192.0.2.1", "nameserver": "1.1.1.1 8.8.8.8"})
        self.assertTrue(all("digest" in w for w in writes))
        order = [m[1].rsplit("/", 1)[1] for m in self.pve.mutations if "/qemu/9290" in m[1]]
        self.assertEqual(order, ["config", "config", "resize", "config", "start", "stop", "9290"])
        resize = next(m[2] for m in self.pve.mutations if m[1].endswith("/9290/resize"))
        self.assertEqual({k: resize[k] for k in ("disk", "size")}, {"disk": "scsi0", "size": "30G"})

    def test_os_variant_drops_the_kubernetes_checks(self):
        self.record(variant="os", os_name="ubuntu-2404", version=None)
        result = self.run_job(variant="os", os="ubuntu-2404", kubernetes_version=None)
        self.assertTrue(result["accepted"])
        self.assertEqual(set(result["clones"][1]["checks"]), set(coldboot_guest.COMMON))
        remote = next(a for a in self.host.argvs if "sudo" in a)
        self.assertEqual(remote[remote.index("-") + 1:remote.index("-") + 4], ["os", "24.04", "-"])

    def test_ssh_is_batch_mode_with_a_fresh_known_hosts_file_and_the_job_key(self):
        seen = []
        self.host.on_guest = lambda address: seen.append(stat_mode(self.sdir / coldboot.KEY))
        self.run_job()
        self.assertEqual(seen, [0o600, 0o600])  # Private key: state dir only, mode 600, while the job runs.
        calls = [a for a in self.host.argvs if a[0] == "ssh"]
        for argv in calls:
            for option in ("BatchMode=yes", "IdentitiesOnly=yes", "IdentityAgent=none", "GlobalKnownHostsFile=/dev/null",
                           "StrictHostKeyChecking=accept-new", "PasswordAuthentication=no", "HostKeyAlgorithms=ssh-ed25519"):
                self.assertIn(option, argv)
            self.assertEqual(argv[1:3] + [argv[argv.index("-i") + 1]], ["-F", "/dev/null", str(self.sdir / coldboot.KEY)])
        self.assertEqual({next(x for x in a if x.startswith("UserKnownHostsFile=")) for a in calls},
                         {"UserKnownHostsFile=%s/known_hosts-%s" % (self.sdir, s) for s in "ab"})
        keygen = next(a for a in self.host.argvs if a[0] == "ssh-keygen")
        self.assertEqual(keygen[2:6], ["-t", "ed25519", "-N", ""])
        (self.sdir / "known_hosts-a").write_text("")  # A stale file is never reused.
        with self.assertRaises(FileExistsError):
            coldboot.guest_checks(self.sdir, {}, {"suffix": "a"}, "192.0.2.50", "k")


def stat_mode(path):
    return path.lstat().st_mode & 0o777


class RefusalTests(ColdBootCase):
    def assert_untouched(self, result, reason):
        self.assertEqual((result["accepted"], result["reason"]), (False, reason))
        self.assertEqual(self.pve.mutations, [])  # Not one write reached PVE.
        self.assertFalse((self.sdir / coldboot.KEY).exists())

    def test_test_address_already_used_refuses_before_any_mutation(self):
        self.pve.vms[9205]["config"]["ipconfig0"] = "ip=192.0.2.51/24,gw=192.0.2.1"
        self.assert_untouched(self.run_job(), "test_address_in_use")
        self.pve.vms[9205]["config"] = {"net1": "name=eth0,bridge=vmbr0,ip=192.0.2.50/24"}  # A container's form.
        self.assert_untouched(self.run_job(), "test_address_in_use")

    def test_upload_name_exists_refuses_and_never_deletes_that_volume(self):
        self.pve.volumes.append(VOLID)
        self.assert_untouched(self.run_job(), "import_volume_exists")
        self.assertEqual((self.pve.volumes, self.pve.uploads), ([VOLID], []))

    def test_vmid_range_exhausted(self):
        for vmid in range(9290, 9300):
            self.pve.add(vmid, pool=POOL if vmid % 2 else "other", name="foreign-%d" % vmid)
        self.pve.vms.pop(9294)  # One free id is not enough for two clones.
        self.assert_untouched(self.run_job(), "vmid_range_exhausted")
        self.assertEqual(len(self.pve.vms), 11)

    def test_range_taken_between_preflight_and_create_cleans_up_the_first_clone_only(self):
        def take(method, path, params):
            if (method, path) == ("POST", "/nodes/%s/qemu" % NODE) and params["vmid"] == 9290:
                for vmid in range(9291, 9300):
                    self.pve.add(vmid, pool="other")
        self.pve.hook = take
        result = self.run_job()
        self.assertEqual((result["accepted"], result["reason"]), (False, "vmid_range_exhausted"))
        self.assertEqual(result["cleanup"]["clones"], {"9290": "destroyed"})
        self.assertEqual(sorted(self.pve.vms), [100, 9205] + list(range(9291, 9300)))
        self.assertEqual(sum(len(self.pve.touched(v)) for v in range(9291, 9300)), 0)

    def test_foreign_vms_in_the_range_are_never_touched(self):
        self.pve.add(9290, name="pc-export-test-%s-a" % ("0" * 8), tags="proxcloud;pc-artifact-test")  # Visible lookalike.
        self.pve.add(9291, pool="customers", name="tenant-vm", status="running")  # Invisible to the token.
        before = {vmid: (self.pve.digest(vmid), self.pve.vms[vmid]["status"]) for vmid in (9290, 9291)}
        result = self.run_job()
        self.assertTrue(result["accepted"])
        self.assertEqual([c["vmid"] for c in result["clones"]], [9292, 9293])
        self.assertEqual(self.pve.touched(9290) + self.pve.touched(9291), [])
        self.assertEqual({vmid: (self.pve.digest(vmid), self.pve.vms[vmid]["status"]) for vmid in (9290, 9291)}, before)
        self.assertEqual(result["protected_set"], {"guests": 3, "unchanged": True})

    def test_sanitize_run_must_be_sealed_and_match_the_disk(self):
        cases = {"unsealed": lambda: self.record(sealed_after_cgroup_stop=False, facts=None),
                 "unsealed_with_facts": lambda: self.record(sealed_after_cgroup_stop=False),
                 "os": lambda: self.record(os_name="ubuntu-2404"),
                 "variant": lambda: self.record(variant="os"), "version": lambda: self.record(version="1.36.6"),
                 "relative": lambda: self.record(disk="disk.qcow2"),
                 "missing": lambda: self.record(disk=str(self.root / "absent.qcow2"))}
        for name, change in cases.items():
            with self.subTest(case=name):
                change()
                self.assert_untouched(self.run_job(), "sanitize_run_refused")
        self.record()
        for name, change in {"writable": lambda: self.disk.chmod(0o600), "hardlink": lambda: os.link(self.disk, self.root / "alias"),
                             "content": lambda: (self.disk.chmod(0o600), self.disk.write_bytes(DISK[::-1]), self.disk.chmod(0o400))}.items():
            with self.subTest(case=name):
                change()
                self.assert_untouched(self.run_job(), "sealed_disk_refused")
                self.disk.chmod(0o400)
                (self.root / "alias").unlink(missing_ok=True)

    def test_settings_refuse_odd_inputs(self):
        for changes in ({"test_address": [ADDRESSES[0]]}, {"test_address": [ADDRESSES[0]] * 2},
                        {"test_address": ["192.0.2.50/24", "198.51.100.51/24"]}, {"test_address": ["192.0.2.50", "192.0.2.51"]},
                        {"test_address": ["192.0.2.0/24", "192.0.2.51/24"]}, {"test_address": ["192.0.2.1/24", "192.0.2.51/24"]},
                        {"gateway": "198.51.100.1"}, {"gateway": "x"}, {"nameserver": []}, {"nameserver": ["dns.example"]},
                        {"vmid_range": "9299-9290"}, {"vmid_range": "1-99999"}, {"vmid_range": "9290"}, {"vlan": 4095},
                        {"node": "pve1/../x"}, {"storage": "a:b"}, {"bridge": "vmbr0,firewall=0"}, {"pool": ""},
                        {"kubernetes_version": "1.30.0"}, {"kubernetes_version": None}, {"os": "ubuntu-2404"},
                        {"variant": "os"}, {"os": "debian-13", "variant": "os", "kubernetes_version": None}):
            with self.subTest(changes=changes), self.assertRaises(coldboot.Refused):
                coldboot.settings(self.args(**changes))

    def test_state_dir_must_be_private_and_a_previous_record_is_never_overwritten(self):
        self.sdir.chmod(0o755)
        for path in (self.sdir, self.root / "absent", Path("relative"), self.disk):
            with self.subTest(path=path), self.assertRaises(coldboot.Refused):
                coldboot.state_dir(path)
        self.sdir.chmod(0o700)
        self.assertEqual(coldboot.state_dir(str(self.sdir)), self.sdir)
        (self.sdir / coldboot.STATE).write_text("{}")
        self.assert_untouched(self.run_job(), "state_exists")
        self.assertEqual((self.sdir / coldboot.STATE).read_text(), "{}")


class FailureCleanupTests(ColdBootCase):
    def test_state_file_names_each_step_before_it_happens(self):
        seen = []

        def watch(method, path, params):
            if (method, path) == ("POST", "/nodes/%s/qemu" % NODE):
                state = self.state()
                seen.append(("create", params["vmid"], [(c["vmid"], c["state"]) for c in state["clones"]][-1],
                             state["import_uploaded"], stat_mode(self.sdir / coldboot.STATE)))
        self.pve.hook = watch
        upload = self.pve.upload
        self.pve.upload = lambda *a: (seen.append(("upload", self.state()["import_uploaded"])), upload(*a))[1]
        self.run_job()
        self.assertEqual(seen, [("upload", True), ("create", 9290, (9290, "reserved"), True, 0o600),
                                ("create", 9291, (9291, "reserved"), True, 0o600)])

    def test_guest_check_failure_still_cleans_up(self):
        row = self.host.row("kubernetes", "192.0.2.51")
        self.host.rows["192.0.2.51"] = {"checks": dict(row["checks"], k8s_binaries_exact=False)}
        result = self.run_job()
        self.assertEqual((result["accepted"], result["reason"], result["step"]), (False, "guest_checks_failed", "guest_checks"))
        self.assertEqual([c["all_passed"] for c in result["clones"]], [True, False])
        self.assert_clean(result)
        self.assertTrue(result["protected_set"]["unchanged"])

    def test_identical_identities_are_refused(self):
        for key in ("machine_id_sha256", "snapd_serial_sha256", "snapd_device_key_sha256"):
            with self.subTest(key=key):
                self.setUp()
                self.host.rows = {a.split("/")[0]: {key: "a" * 64} for a in ADDRESSES}
                result = self.run_job()
                self.assertEqual((result["accepted"], result["reason"]), (False, "identity_not_distinct"))
                self.assertIs(result["distinct_identity"][key], False)
                self.assert_clean(result)
        self.setUp()  # A serial that never came back is not a new identity either.
        self.host.rows = {"192.0.2.50": {"snapd_serial_sha256": None}}
        self.assertEqual(self.run_job()["reason"], "identity_not_distinct")

    def test_guest_output_is_gated_and_never_copied(self):
        good = self.host.row("kubernetes", "192.0.2.50")
        for name, change in {"extra": {"hostname": "leak"}, "text": {"machine_id_sha256": "not-a-hash"},
                             "nonbool": {"checks": dict(good["checks"], no_active_swap="yes")},
                             "missing": {"checks": {k: v for k, v in good["checks"].items() if k != "disk_30g"}},
                             "unknown": {"checks": dict(good["checks"], surprise=True)},
                             "count": {"snapd_changes_error_lines": "many"},
                             "hostkey": {"host_key_sha256": "b" * 64}}.items():
            with self.subTest(case=name):
                self.setUp()
                self.host.rows["192.0.2.50"] = change
                result = self.run_job()
                self.assertEqual(result["reason"], "host_key_mismatch" if name == "hostkey" else "guest_output_refused")
                self.assertEqual(result["clones"], [])
                self.assertNotIn("leak", json.dumps(result))
                self.assert_clean(result)
        self.setUp()
        self.host.guest_rc = 1
        self.assertEqual(self.run_job()["reason"], "guest_checks_unreadable")
        with self.assertRaises(coldboot.Refused):
            coldboot.guest_record(b" " * 70000, "os")
        with self.assertRaises(coldboot.Refused):
            coldboot.guest_record(b"\xff{", "os")

    def test_unreachable_guest_and_failed_tasks_clean_up(self):
        self.host.down = 10 ** 9
        result = self.run_job()
        self.assertEqual((result["accepted"], result["reason"], result["step"]), (False, "ssh_unreachable", "guest_checks"))
        self.assert_clean(result)
        for kind, cleaned in (("qmconfig", ("destroyed",)), ("qmstart", ("destroyed",)), ("imgcopy", ())):
            with self.subTest(kind=kind):
                self.setUp()
                self.pve.fail[kind] = "import failed"
                result = self.run_job()
                self.assertEqual((result["accepted"], result["reason"]), (False, "task_failed"))
                self.assert_clean(result, clones=cleaned, volume="deleted" if cleaned else "absent")
                self.assertNotIn("import failed", json.dumps(result))

    def test_ssh_is_retried_until_the_guest_answers(self):
        self.host.down = 3
        with patch.object(coldboot, "SSH_DEADLINE", 600):
            result = self.run_job()
        self.assertTrue(result["accepted"])
        self.assertEqual(len([a for a in self.host.argvs if a[-1] == "true"]), 5)  # 3 refused + one per clone.
        self.assertEqual([c.args for c in self.slept.call_args_list if c.args == (5,)], [(5,)] * 3)

    def test_create_task_is_awaited_and_its_failure_touches_nothing(self):
        self.pve.fail["qmcreate"] = "unable to create VM"
        result = self.run_job()
        self.assertEqual(result["reason"], "task_failed")
        # The half-created guest never joined the pool: not ours to touch, and reported, not hidden.
        self.assertEqual(result["cleanup"]["clones"], {"9290": "refused_not_owned_pool"})
        self.assertEqual([m for m in self.pve.touched(9290) if m[0] != "POST" or m[1] != "/nodes/%s/qemu" % NODE], [])
        self.assertEqual(result["cleanup"]["import_volume"], "deleted")

    def test_a_created_vm_that_is_not_empty_is_not_adopted(self):
        def adopt(method, path, params):  # The id turned out to hold something that is not a fresh empty VM.
            if method == "GET" and path.endswith("/9290/config") and "name" not in self.pve.vms[9290]["config"]:
                self.pve.vms[9290]["config"].update(name="someone-else", scsi0="other:9290/vm-9290-disk-0.raw")
        self.pve.hook = adopt
        result = self.run_job()
        self.assertEqual((result["accepted"], result["reason"]), (False, "fresh_vm_not_empty"))
        self.assertEqual(result["cleanup"]["clones"], {"9290": "refused_not_owned_identity"})
        self.assertEqual([m[1] for m in self.pve.touched(9290)], ["/nodes/%s/qemu" % NODE])  # The create, nothing after.
        self.assertEqual(self.pve.vms[9290]["config"]["name"], "someone-else")

    def test_a_password_or_another_key_in_cloud_init_never_boots(self):
        changes = {"cipassword": {"cipassword": "**********"}, "key": {"sshkeys": "ssh-ed25519%20AAAAother%20build"},
                   "user": {"ciuser": "builder"}, "address": {"ipconfig0": "ip=dhcp"}}
        for name, change in changes.items():
            with self.subTest(change=name):
                self.setUp()

                def inject(method, path, params, change=change):
                    config = self.pve.vms.get(9290, {}).get("config", {})
                    if method == "GET" and path.endswith("/9290/config") and "ciuser" in config:
                        config.update(change)
                self.pve.hook = inject
                result = self.run_job()
                self.assertEqual((result["accepted"], result["reason"]), (False, "cloudinit_readback"))
                self.assertEqual([m for m in self.pve.mutations if m[1].endswith("/status/start")], [])
                self.assert_clean(result, clones=("destroyed",))

    def test_cleanup_refuses_a_vm_whose_name_tag_or_pool_changed(self):
        changes = {"name": (lambda vm: vm["config"].update(name="renamed"), "refused_not_owned_identity"),
                   "tag": (lambda vm: vm["config"].update(tags="proxcloud;pc-artifact-test"), "refused_not_owned_identity"),
                   "other_tag": (lambda vm: vm["config"].update(tags="proxcloud;pc-artifact-" + "0" * 12), "refused_not_owned_identity"),
                   "pool": (lambda vm: vm.update(pool="customers"), "refused_not_owned_pool"),
                   "template": (lambda vm: vm["config"].update(template=1), "refused_not_owned_template_or_lock"),
                   "lock": (lambda vm: vm["config"].update(lock="backup"), "refused_not_owned_template_or_lock"),
                   "disk": (lambda vm: vm["config"].update(scsi1="other:100/base-100-disk-0.raw"), "refused_unexpected_disk"),
                   "efi": (lambda vm: vm["config"].update(efidisk0="x"), "refused_unexpected_firmware"),
                   "snapshot": (lambda vm: vm.update(snapshots=["kept"]), "refused_unexpected_snapshot")}
        for name, (change, outcome) in changes.items():
            with self.subTest(change=name):
                self.setUp()
                self.host.on_guest = lambda address, change=change: address.endswith(".51") and change(self.pve.vms[9290])
                result = self.run_job()
                self.assertEqual((result["accepted"], result["reason"]), (False, "cleanup_incomplete"))
                self.assertEqual(result["cleanup"]["clones"], {"9290": outcome, "9291": "destroyed"})
                self.assertIn(9290, self.pve.vms)
                self.assertEqual(self.pve.vms[9290]["status"], "running")  # Not even stopped.
                self.assertEqual([m for m in self.pve.touched(9290) if m[1].endswith(("/stop", "/9290"))], [])
                self.assertTrue(result["cleanup"]["private_key_deleted"])
                self.assertEqual(result["protected_set"]["unchanged"], name == "pool")  # A leftover is "something new".
                self.assertEqual(self.pve.volumes, [])  # The run's own upload still goes.

    def test_cleanup_rereads_ownership_between_stop_and_destroy(self):
        self.pve.hook = lambda m, p, q: p.endswith("/9290/status/stop") and self.pve.vms[9290]["config"].update(name="taken-over")
        result = self.run_job()
        self.assertEqual(result["cleanup"]["clones"], {"9290": "refused_not_owned_identity", "9291": "destroyed"})
        self.assertIn(9290, self.pve.vms)
        self.pve.hook = None
        self.setUp()
        self.pve.fail["qmdestroy"] = "storage busy"
        result = self.run_job()
        self.assertEqual((result["reason"], set(result["cleanup"]["clones"].values())), ("cleanup_incomplete", {"refused_task_failed"}))

    def test_import_volume_still_referenced_is_kept(self):
        self.host.on_guest = lambda address: self.pve.vms[9205]["config"].update(scsi5=VOLID)
        result = self.run_job()
        self.assertEqual(result["cleanup"]["import_volume"], "refused_import_volume_referenced")
        self.assertEqual((self.pve.volumes, result["accepted"], result["reason"]), ([VOLID], False, "cleanup_incomplete"))

    def test_protected_set_drift_is_detected(self):
        drifts = {"config": lambda: self.pve.vms[100]["config"].update(description="edited"),
                  "status": lambda: self.pve.vms[9205].update(status="stopped"),
                  "gone": lambda: self.pve.vms.pop(9205, None),
                  "new": lambda: self.pve.add(9300, name="appeared")}
        for name, drift in drifts.items():
            with self.subTest(drift=name):
                self.setUp()
                self.host.on_guest = lambda address, drift=drift: drift()
                result = self.run_job()
                self.assertEqual((result["accepted"], result["reason"]), (False, "protected_set_changed"))
                self.assertEqual(result["protected_set"], {"guests": 2, "unchanged": False})
                self.assertEqual(sorted(result["cleanup"]["clones"].values()), ["destroyed", "destroyed"])
                self.assertEqual(self.pve.touched(100) + self.pve.touched(9205) + self.pve.touched(9300), [])

    def test_private_key_is_removed_on_every_outcome(self):
        outcomes = {"ok": lambda: None, "upload": lambda: self.pve.fail.update(imgcopy="x"),
                    "create": lambda: self.pve.fail.update(qmcreate="x"), "guest": lambda: setattr(self.host, "guest_rc", 1),
                    "error": lambda: setattr(self.host, "on_guest", lambda a: 1 / 0)}
        for name, arrange in outcomes.items():
            with self.subTest(outcome=name):
                self.setUp()
                arrange()
                result = self.run_job()
                self.assertEqual(result["accepted"], name == "ok")
                self.assertTrue(result["cleanup"]["private_key_deleted"])
                self.assertEqual(sorted(p.name for p in self.sdir.iterdir()), [coldboot.STATE])
        self.assertEqual(result["reason"], "error_ZeroDivisionError")  # A class name, never a message.

    def test_cleanup_command_finishes_an_interrupted_run_from_its_state_file(self):
        self.assertEqual(coldboot.cleanup_only(self.pve, self.sdir)["reason"], "no_state")
        self.host.on_guest = lambda address: (_ for _ in ()).throw(KeyboardInterrupt())
        with patch.object(coldboot, "finish", side_effect=KeyboardInterrupt()):  # Killed before any cleanup ran.
            result = self.run_job()
        self.assertEqual((result["accepted"], sorted(self.pve.vms)), (False, [100, 9205, 9290, 9291]))
        self.assertTrue((self.sdir / coldboot.KEY).exists())
        self.pve.add(9292, name="pc-export-test-%s-c" % SHA[:8], tags=TAG)  # Never adopted by name prefix.
        result = coldboot.cleanup_only(self.pve, self.sdir)
        self.assertEqual((result["clean"], result["protected_set"]["unchanged"]), (False, False))  # 9292 is new.
        self.assertEqual(result["cleanup"], {"clones": {"9290": "destroyed", "9291": "destroyed"},
                                              "import_volume": "deleted", "private_key_deleted": True})
        self.assertEqual((sorted(self.pve.vms), self.pve.touched(9292)), ([100, 9205, 9292], []))
        self.pve.vms.pop(9292)
        self.assertTrue(coldboot.cleanup_only(self.pve, self.sdir)["clean"])  # Idempotent.

    def test_tampered_state_file_is_refused_before_any_call(self):
        self.run_job()
        good = self.state()
        for name, change in {"name": lambda s: s["clones"][0].update(name="pc-ubuntu-2604-k8s"),
                             "vmid": lambda s: s["clones"][0].update(vmid=100),
                             "volid": lambda s: s["cfg"].update(volid=STORAGE + ":import/other.qcow2"),
                             "sha": lambda s: s["cfg"].update(sha256="x"), "clones": lambda s: s.update(clones=s["clones"] * 2),
                             "node": lambda s: s["cfg"].update(node="a/b")}.items():
            with self.subTest(case=name):
                state = json.loads(json.dumps(good))
                change(state)
                (self.sdir / coldboot.STATE).write_text(json.dumps(state))
                calls = len(self.pve.calls)
                result = coldboot.cleanup_only(self.pve, self.sdir)
                self.assertEqual((result["clean"], result["reason"], len(self.pve.calls)), (False, "state_refused", calls))


class GuardUnitTests(ColdBootCase):
    def test_task_wait_accepts_only_the_expected_task_and_is_bounded(self):
        good = self.pve.upid("qmstart", 9290)
        coldboot.wait(self.pve, NODE, good, "qmstart:9290")
        coldboot.wait(self.pve, NODE, None, "qmconfig:9290", allow_sync=True)
        for upid, task in ((None, "qmstart:9290"), ("", "qmstart:9290"), (5, "qmstart:9290"),
                           (self.pve.upid("qmstart", 9291), "qmstart:9290"), (self.pve.upid("qmdestroy", 9290), "qmstart:9290"),
                           (good.replace(NODE, "pve2"), "qmstart:9290"), (good + "x", "qmstart:9290"),
                           (self.pve.upid("imgcopy") + "\nUPID:x", coldboot.STORAGE_TASK)):
            with self.subTest(upid=upid), self.assertRaises(coldboot.Refused):
                coldboot.wait(self.pve, NODE, upid, task)
        running = types.SimpleNamespace(call=lambda *a, **k: {"status": "running"})
        with patch.object(coldboot, "TASK_DEADLINE", 0), self.assertRaises(coldboot.Refused) as caught:
            coldboot.wait(running, NODE, good, "qmstart:9290")
        self.assertEqual(str(caught.exception), "task_deadline")

    def test_vmid_probe_treats_only_http_400_as_taken(self):
        self.assertFalse(coldboot.exists(self.pve, 9290))
        self.assertTrue(coldboot.exists(self.pve, 100))
        denied = types.SimpleNamespace(call=lambda *a, **k: (_ for _ in ()).throw(coldboot.ApiError(403)))
        with self.assertRaises(coldboot.Refused):
            coldboot.exists(denied, 9290)

    def test_main_writes_the_result_and_exit_codes(self):
        argv = ["run", "--sanitize-run", str(self.root / "sanitize-run.json"), "--variant", "kubernetes", "--os", "ubuntu-2604",
                "--kubernetes-version", "1.37.2", "--node", NODE, "--storage", STORAGE, "--vlan", "242", "--gateway", "192.0.2.1",
                "--test-address", ADDRESSES[0], "--test-address", ADDRESSES[1], "--nameserver", "1.1.1.1",
                "--state-dir", str(self.sdir)]
        handlers = {s: signal.getsignal(s) for s in (signal.SIGTERM, signal.SIGINT)}
        self.addCleanup(lambda: [signal.signal(s, h) for s, h in handlers.items()])
        env = {"PVE_URL": "https://pve.invalid:8006", "PVE_TOKEN_ID": "images@pve!packer", "PVE_TOKEN_SECRET": "not-a-secret"}
        out = io.StringIO()
        with patch.dict(os.environ, env), patch.object(coldboot, "Client", lambda pve: self.pve), contextlib.redirect_stdout(out):
            self.assertEqual(coldboot.main(argv), 0)
            self.assertEqual(json.loads(out.getvalue()), {"kind": "ColdBootAcceptance", "ok": True, "reason": None})
            result = self.sdir / coldboot.RESULT
            self.assertEqual((json.loads(result.read_text())["accepted"], stat_mode(result)), (True, 0o600))
            self.assertEqual(coldboot.main(argv), 1)  # The state file of that run is still there.
            self.assertEqual(json.loads(result.read_text())["reason"], "state_exists")
            self.assertEqual(coldboot.main(["cleanup", "--state-dir", str(self.sdir)]), 0)
            self.assertTrue(json.loads((self.sdir / "coldboot-cleanup-result.json").read_text())["clean"])
            with contextlib.redirect_stderr(io.StringIO()) as err:
                self.assertEqual(coldboot.main(argv[:-1] + [str(self.root / "absent")]), 2)
            self.assertEqual(err.getvalue(), "coldboot refused: state_dir_refused\n")
        with self.assertRaises(coldboot.Refused):  # A cancelled job raises into run()'s guarded cleanup.
            coldboot._terminated(signal.SIGTERM, None)
        self.assertEqual(signal.getsignal(signal.SIGTERM), signal.SIG_IGN)


class Handler(http.server.BaseHTTPRequestHandler):
    seen = []

    def do_POST(self):
        body = self.rfile.read(int(self.headers["Content-Length"]))
        Handler.seen.append((self.command, self.path, dict(self.headers), body))
        self.reply(200, {"data": "UPID:pve1:0001:0002:0003:imgcopy::images@pve!packer:"})

    def do_GET(self):
        Handler.seen.append((self.command, self.path, dict(self.headers), b""))
        if "9999" in self.path:
            self.reply(403, {"data": None, "message": "Permission check failed (/vms/9999, VM.Audit) secret-detail"})
        else:
            self.reply(200, {"data": {"path": self.path}})

    do_DELETE = do_GET

    def reply(self, status, payload):
        data = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *args):
        pass


class ClientTests(unittest.TestCase):
    def setUp(self):
        Handler.seen = []
        self.server = http.server.HTTPServer(("127.0.0.1", 0), Handler)
        self.server.handle_error = lambda *a: None  # An aborted upload is expected to break its connection.
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        pve = types.SimpleNamespace(base="http://127.0.0.1:%d/api2/json" % self.server.server_port,
                                    auth="PVEAPIToken=images@pve!packer=not-a-secret", ctx=ssl._create_unverified_context())
        self.client = coldboot.Client(pve)
        self.client.UPLOAD_TIMEOUT = 3  # A body that ends short must fail the test quickly, not after minutes.
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.disk = Path(tmp.name) / "disk.qcow2"
        self.disk.write_bytes(DISK)

    def test_upload_streams_one_multipart_file_with_the_checksum_fields_first(self):
        upid = self.client.upload(NODE, STORAGE, "pc-export-%s.qcow2" % SHA[:12], str(self.disk), SHA, len(DISK))
        self.assertTrue(upid.startswith("UPID:pve1:"))
        (method, path, headers, body), = Handler.seen
        self.assertEqual((method, path), ("POST", "/api2/json/nodes/pve1/storage/pc-ci-images/upload"))
        self.assertEqual(headers["Authorization"], "PVEAPIToken=images@pve!packer=not-a-secret")
        self.assertEqual(int(headers["Content-Length"]), len(body))
        self.assertNotIn("Transfer-Encoding", headers)
        boundary = re.fullmatch(r"multipart/form-data; boundary=(proxcloud[0-9a-f]{32})", headers["Content-Type"]).group(1).encode()
        parts = body.split(b"--" + boundary)
        self.assertEqual((parts[0], parts[-1]), (b"", b"--\r\n"))
        fields = [re.match(rb'\r\nContent-Disposition: form-data; name="([a-z-]+)"', p).group(1) for p in parts[1:-1]]
        self.assertEqual(fields, [b"content", b"checksum-algorithm", b"checksum", b"filename"])
        self.assertEqual([p.split(b"\r\n\r\n", 1)[1] for p in parts[1:4]], [b"import\r\n", b"sha256\r\n", SHA.encode() + b"\r\n"])
        head, data = parts[4].split(b"\r\n\r\n", 1)
        self.assertIn(b'filename="pc-export-%s.qcow2"' % SHA[:12].encode(), head)
        self.assertEqual(data, DISK + b"\r\n")

    def test_upload_of_a_changed_or_linked_file_never_completes(self):
        for size, digest in ((len(DISK) - 1, SHA), (len(DISK) + 1, SHA), (len(DISK), "0" * 64)):
            with self.subTest(size=size, digest=digest[:4]), self.assertRaises((coldboot.Refused, OSError)):
                self.client.upload(NODE, STORAGE, "x.qcow2", str(self.disk), digest, size)
        link = self.disk.with_name("link.qcow2")
        link.symlink_to(self.disk)
        with self.assertRaises(OSError):
            self.client.upload(NODE, STORAGE, "x.qcow2", str(link), SHA, len(DISK))
        self.assertEqual([s for s in Handler.seen if s[3].endswith(b"--\r\n")], [])  # No complete body was ever sent.

    def test_calls_put_get_parameters_in_the_query_and_errors_carry_only_the_status(self):
        self.assertEqual(self.client.call("GET", "/cluster/nextid", vmid=9290), {"path": "/api2/json/cluster/nextid?vmid=9290"})
        self.assertEqual(self.client.call("DELETE", "/nodes/pve1/qemu/9290", purge=1)["path"], "/api2/json/nodes/pve1/qemu/9290?purge=1")
        self.client.call("POST", "/nodes/pve1/qemu", vmid=9290, pool=POOL)
        self.assertEqual(Handler.seen[-1][3], b"vmid=9290&pool=pc-templates")
        with self.assertRaises(coldboot.ApiError) as caught:
            self.client.call("GET", "/nodes/pve1/qemu/9999/config")
        self.assertEqual((caught.exception.status, str(caught.exception)), (403, "pve_http_403"))
        self.assertFalse(self.client.tls_verified)  # build.PVE's context does not verify the certificate today.


class FakeGuest:
    """The files and tools coldboot_guest reads inside a clone; nothing here touches the real host."""
    KEY = "AAAAC3NzaC1lZDI1NTE5AAAAIJobKey"

    def __init__(self, variant="kubernetes"):
        self.variant, self.snap, self.groups, self.sudo = variant, True, "1000(builder)", "User builder is not allowed to run sudo."
        self.kubelet, self.serial, self.cloud = "Kubernetes v1.37.2", "serial: 7c1f-serial\ndevice-key-sha3-384: devkeyhash\n", "done"
        self.files = {"/etc/os-release": 'ID=ubuntu\nVERSION_ID="26.04"\n', "/etc/machine-id": "0123456789abcdef0123456789abcdef\n",
                      "/etc/passwd": "root:x:0:0:root:/root:/bin/bash\nubuntu:x:1001:1001::/home/ubuntu:/bin/bash\n"
                                     "builder:x:1000:1000::/home/builder:/bin/bash\n",
                      "/etc/shadow": "root:*:1:0:99999:7:::\nubuntu:!:1:0:99999:7:::\nbuilder:!:1:0:99999:7:::\n",
                      "/home/ubuntu/.ssh/authorized_keys": "ssh-ed25519 %s proxcloud-export-coldboot\n" % self.KEY,
                      "/etc/ssh/ssh_host_ed25519_key.pub": "ssh-ed25519 AAAAhostkey root@clone\n",
                      "/proc/swaps": "Filename Type Size Used Priority\n"}
        self.dirs = {"/var/lib/snapd/device/private-keys-v1": ["newkey"]}

    def run(self, args, timeout=60):
        out, code = "", 0
        if args[:2] == ["/usr/bin/cloud-init", "status"]:
            out = json.dumps({"status": self.cloud, "errors": [], "extended_status": "done",
                              "init": {"errors": [], "recoverable_errors": {"WARNING": ["w1", "w2"]}}})
        elif args[0] == "/usr/bin/id":
            out = "uid=1000(builder) gid=1000(builder) groups=%s" % self.groups
        elif args[0] == "/usr/bin/sudo":
            out = self.sudo
        elif args[:2] == ["/usr/bin/snap", "model"]:
            out, code = self.serial, 0 if self.serial else 1
        elif args[:2] == ["/usr/bin/snap", "changes"]:
            out = "ID Status Spawn Ready Summary\n1 Done today today Initialize system state\n2 Error today today Refresh\n"
        elif args[0] == "/usr/bin/systemctl":
            out = "enabled" if args[1] == "is-enabled" else "active"
        elif args[0] == "/usr/bin/lsblk":
            out = json.dumps({"blockdevices": [{"name": "sr0", "type": "rom", "size": 4 << 20}, {"name": "sda", "type": "disk", "size": 30 << 30}]})
        elif args[0] == "/usr/bin/kubeadm":
            out = "v1.37.2\n"
        elif args[0] == "/usr/bin/kubelet":
            out = self.kubelet
        elif args[0] == "/usr/bin/kubectl":
            out = json.dumps({"clientVersion": {"gitVersion": "v1.37.2"}})
        elif args[0] == "/usr/local/bin/crictl":
            out = "RuntimeName:  containerd\n"
        return types.SimpleNamespace(stdout=out, stderr="", returncode=code)

    def record(self):
        path = coldboot_guest.pathlib.Path
        out = io.StringIO()
        with patch.object(coldboot_guest, "run", self.run), patch.object(coldboot_guest, "SERIAL_WAIT", 0), \
                patch.object(path, "read_text", lambda p, *a, **k: self.files[str(p)]), \
                patch.object(path, "is_file", lambda p: str(p) in self.files), \
                patch.object(path, "exists", lambda p, **k: str(p) in self.files or str(p) in self.dirs), \
                patch.object(path, "is_dir", lambda p: str(p) in self.dirs), \
                patch.object(path, "iterdir", lambda p: iter(self.dirs[str(p)])), \
                patch.object(coldboot_guest.os.path, "exists", lambda p: self.snap and p == "/usr/bin/snap"), \
                patch.object(coldboot_guest.os, "statvfs", return_value=types.SimpleNamespace(f_blocks=29 << 18, f_frsize=4096)), \
                contextlib.redirect_stdout(out):
            coldboot_guest.main([self.variant, "26.04", "1.37.2" if self.variant == "kubernetes" else "-", sha(self.KEY)])
        return coldboot.guest_record(out.getvalue().encode(), self.variant)  # What the guest prints passes the parent gate.


class GuestScriptTests(unittest.TestCase):
    def test_guest_checks_pass_on_a_clean_clone_and_report_hashes_only(self):
        row = FakeGuest().record()
        self.assertEqual(row["checks"], dict.fromkeys(coldboot_guest.COMMON + coldboot_guest.K8S, True))
        self.assertEqual((row["machine_id_sha256"], row["host_key_sha256"], row["snapd_serial_sha256"], row["snapd_device_key_sha256"]),
                         (sha("0123456789abcdef0123456789abcdef"), sha("AAAAhostkey"), sha("7c1f-serial"), sha("devkeyhash")))
        self.assertEqual((row["snapd_present"], row["snapd_changes_error_lines"], row["cloud_init_recoverable_warnings"]), (True, 1, 2))
        native = FakeGuest("os")
        native.files["/etc/passwd"] = native.files["/etc/passwd"].replace("builder:x:1000:1000::/home/builder:/bin/bash\n", "")
        native.snap = False  # No snapd and no build account: nothing to fail, nothing to prove.
        row = native.record()
        self.assertEqual(row["checks"], dict.fromkeys(coldboot_guest.COMMON, True))
        self.assertEqual((row["snapd_present"], row["snapd_serial_sha256"], row["snapd_device_key_sha256"]), (False, None, None))

    def test_each_guest_check_fails_on_its_own_defect(self):
        def edit(path, old, new):
            return lambda g: g.files.update({path: g.files[path].replace(old, new)})
        defects = {
            "os_release": edit("/etc/os-release", "26.04", "24.04"),
            "cloud_init_done_no_errors": lambda g: setattr(g, "cloud", "error"),
            "default_user_exactly_job_key": lambda g: g.files.update({"/home/ubuntu/.ssh/authorized_keys": g.files[
                "/home/ubuntu/.ssh/authorized_keys"] + "ssh-ed25519 AAAAbuildkey packer\n"}),
            "build_user_only_own_group": lambda g: setattr(g, "groups", "1000(builder),27(sudo)"),
            "build_user_no_sudo": lambda g: setattr(g, "sudo", "User builder may run the following commands:\n    (ALL : ALL) ALL"),
            "build_user_locked": edit("/etc/shadow", "builder:!:", "builder:$y$j9T$hash:"),
            "build_user_no_keys": lambda g: g.files.update({"/home/builder/.ssh/authorized_keys": "ssh-ed25519 AAAA b\n"}),
            "machine_id_valid": edit("/etc/machine-id", "0123456789abcdef0123456789abcdef", ""),
            "snapd_reregistered_new_key": lambda g: setattr(g, "serial", ""),
            "no_active_swap": edit("/proc/swaps", "Priority\n", "Priority\n/swap.img file 2097148 0 -2\n"),
            "builder_account_retained": edit("/etc/passwd", "builder:x:1000:1000::/home/builder:/bin/bash\n", ""),
            "k8s_binaries_exact": lambda g: setattr(g, "kubelet", "Kubernetes v1.37.20"),
        }
        for check, defect in defects.items():
            with self.subTest(check=check):
                guest = FakeGuest()
                defect(guest)
                failed = {name for name, ok in guest.record()["checks"].items() if not ok}
                self.assertEqual(failed, {check})
        guest = FakeGuest()
        guest.files["/home/ubuntu/.ssh/authorized_keys"] = "ssh-ed25519 AAAAsomeoneelse x\n"  # One key, not the job's.
        self.assertFalse(guest.record()["checks"]["default_user_exactly_job_key"])

    def test_guest_script_is_standalone_stdlib_and_names_every_check(self):
        source = Path(coldboot_guest.__file__).read_text()
        imports = set(re.findall(r"(?m)^import ([a-z]+)$", source))
        self.assertEqual(imports, {"hashlib", "json", "os", "pathlib", "re", "subprocess", "sys", "time"})
        self.assertNotIn("from ", source.split('"""', 2)[2].split("def ", 1)[0])
        produced = set(re.findall(r'(?m)^\s+"([a-z0-9_]+)": ', source.split("checks = {", 1)[1].split("print(", 1)[0]))
        self.assertEqual(produced, set(coldboot_guest.COMMON + coldboot_guest.K8S))
        self.assertEqual(coldboot.GUEST_KEYS, set(re.findall(r'"([a-z0-9_]+)": ', source.split("print(json.dumps({", 1)[1])) | {"checks"})
        for word in ("write_text", "write_bytes", "unlink", "open("):  # Read-only in the guest.
            self.assertNotIn(word, source)


if __name__ == "__main__":
    unittest.main()
