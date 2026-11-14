"""Pure tests for images/vm_export/coldboot.py: a fake PVE (token view: pool ACL only, every mutation a task
that takes effect when it ends), served in process and, for one end-to-end run, over loopback HTTP; a fake
ssh/ssh-keygen. No PVE, no guest, no real SSH."""
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
import time
import types
import unittest
import urllib.error
import urllib.parse
from unittest.mock import patch

IMAGES = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(IMAGES))
from vm_export import coldboot, coldboot_guest  # noqa: E402

NODE, API_NODE, STORAGE, POOL = "pve1", "pve0", "pc-ci-images", "pc-templates"
TOKEN = "images@pve!packer"
AUTH = "PVEAPIToken=%s=not-a-secret" % TOKEN
ADDRESSES = ("192.0.2.50/24", "192.0.2.51/24")
PROBE = coldboot.address_answers  # The real probe: every test case replaces the module attribute.
DISK = b"sealed sanitized qcow2 bytes " * 4096
SHA = hashlib.sha256(DISK).hexdigest()
TAG = "pc-artifact-" + SHA[:12]
QEMU = "/nodes/%s/qemu" % NODE
CONTENT = "/nodes/%s/storage/%s/content" % (NODE, STORAGE)


def sha(text):
    return hashlib.sha256(text.encode()).hexdigest()


class FakePVE:
    """What the least-privilege token sees: only guests in the pool; anything else answers 403. A guest
    joins its pool when its create task ends, a create may carry vmid, pool and name but no tags, every
    write needs the digest it read, and every mutation is a task whose effect lands when the task is
    polled to its end (an upload's task runs on the API node, not on the storage's node)."""
    tls_verified = False
    token_id = TOKEN

    def __init__(self):
        self.vms, self.volumes, self.orphans = {}, [], {}
        self.calls, self.mutations, self.uploads = [], [], []
        self.pending, self.fail, self.count, self.hook = {}, {}, 0, None
        # Per task kind: report another kind, answer without a task (older PVE), end "OK" without effect, never end.
        self.kinds, self.sync, self.broken, self.stall = {}, set(), set(), set()
        self.rootfs, self.import_size, self.resize_to = None, "20G", None

    def add(self, vmid, pool=POOL, status="stopped", **config):
        self.vms[vmid] = {"pool": pool, "status": status, "config": dict(config)}

    def digest(self, vmid):
        return hashlib.sha1(json.dumps(self.vms[vmid]["config"], sort_keys=True).encode()).hexdigest()

    def task(self, kind, ident="", done=None, undo=None, node=NODE):
        if kind in self.broken:
            done = None
        if kind in self.sync:
            if done:
                done()
            return None
        self.count += 1
        upid = "UPID:%s:%08X:%08X:%08X:%s:%s:%s:" % (node, self.count, 1, 2, self.kinds.get(kind, kind), ident, TOKEN)
        self.pending[upid] = (done, undo)
        return upid

    def vm(self, vmid):
        vm = self.vms.get(int(vmid))
        if vm is None or vm["pool"] != POOL:
            raise coldboot.ApiError(403)  # Outside the pool and missing look the same.
        return vm

    def upload(self, node, storage, name, path, sha256, size):
        data = Path(path).read_bytes()
        assert hashlib.sha256(data).hexdigest() == sha256 and len(data) == size
        return self.store(node, storage, name)

    def store(self, node, storage, name):
        self.mutations.append(("UPLOAD", name, {}))
        assert (node, storage) == (NODE, STORAGE)
        self.uploads.append(name)
        return self.task("imgcopy", done=lambda: self.volumes.append("%s:import/%s" % (storage, name)), node=API_NODE)

    def call(self, method, path, **params):
        self.calls.append((method, path, dict(params)))
        if self.hook:
            self.hook(method, path, params)
        if method != "GET":
            self.mutations.append((method, path, dict(params)))
        if (method, path) == ("GET", "/cluster/resources"):
            return [{"vmid": i, "node": NODE, "type": "qemu", "status": v["status"], "pool": POOL}
                    for i, v in sorted(self.vms.items()) if v["pool"] == POOL]
        if (method, path) == ("GET", "/cluster/nextid"):
            if int(params["vmid"]) in self.vms:
                raise coldboot.ApiError(400)
            return str(params["vmid"])
        if (method, path) == ("GET", "/pools/" + POOL):
            return {"members": [{"vmid": i, "node": NODE, "type": "qemu"} for i, v in self.vms.items() if v["pool"] == POOL]}
        if method == "GET" and path in ("/cluster/status", "/nodes/%s/status" % API_NODE):
            if self.rootfs is None:
                raise coldboot.ApiError(403)  # No Sys.Audit.
            return {"rootfs": {"avail": self.rootfs}} if path.startswith("/nodes/") else [
                {"type": "cluster", "name": "lab"}, {"type": "node", "name": NODE, "local": 0},
                {"type": "node", "name": API_NODE, "local": 1}]
        if (method, path) == ("GET", CONTENT):
            # As PVE 9: a row of a guest's volume carries its vmid; an import file or ISO carries none, and
            # the vmid filter keeps those ownerless rows (seen on the lab: every reserved VMID "had" volumes).
            rows = [{"volid": v} for v in self.volumes] + [{"volid": v, "vmid": owner}
                                                           for owner, volumes in self.orphans.items() for v in volumes]
            if "vmid" in params:
                rows = [r for r in rows if r.get("vmid") in (None, int(params["vmid"]))]
            return [r for r in rows if params.get("content") != "import" or ":import/" in r["volid"]]
        if method == "DELETE" and path.startswith(CONTENT + "/"):
            volume = urllib.parse.unquote(path.rsplit("/", 1)[1])
            assert volume in self.volumes
            return self.task("imgdel", done=lambda: self.volumes.remove(volume))
        task = re.fullmatch(r"/nodes/([^/]+)/tasks/([^/]+)/status", path)
        if method == "GET" and task:
            upid = urllib.parse.unquote(task.group(2))
            kind = upid.split(":")[5]
            assert task.group(1) == upid.split(":")[1]  # Asked on the node the task runs on.
            if kind in self.stall:
                return {"status": "running"}
            done, undo = self.pending.pop(upid, (None, None))
            status = self.fail.get(kind, "OK")
            step = done if status == "OK" else undo
            if step:
                step()
            return {"status": "stopped", "exitstatus": status}
        if (method, path) == ("POST", QEMU):
            vmid = int(params["vmid"])
            if not {"vmid", "pool"} <= set(params) <= {"vmid", "pool", "name"}:
                raise coldboot.ApiError(403)  # Tags are checked on /vms/<id>, where the token has nothing.
            if vmid in self.vms:
                raise coldboot.ApiError(500)
            self.add(vmid, pool=None, **{k: params[k] for k in ("name",) if k in params})
            return self.task("qmcreate", vmid, lambda: self.vms[vmid].update(pool=params["pool"]), lambda: self.vms.pop(vmid))
        qemu = re.fullmatch(QEMU + r"/([0-9]+)(/.*)?", path)
        vmid, tail = int(qemu.group(1)), qemu.group(2) or ""
        vm = self.vm(vmid)
        config = vm["config"]
        if method == "GET":
            return {"/config": lambda: dict(config, digest=self.digest(vmid)),
                    "/status/current": lambda: {"status": vm["status"]},
                    "/snapshot": lambda: [{"name": n} for n in vm.get("snapshots", ())] + [{"name": "current"}],
                    "/pending": lambda: [{"key": k, "value": v} for k, v in config.items()] + list(vm.get("pending", ()))}[tail]()
        if tail in ("/config", "/resize") and params.pop("digest", None) != self.digest(vmid):
            raise coldboot.ApiError(500)  # A write without the digest it read, or with a stale one.
        if (method, tail) == ("POST", "/config"):
            changes = {}
            for key, value in params.items():
                if key == "scsi0":
                    assert value == "%s:0,discard=on,import-from=%s:import/%s" % (STORAGE, STORAGE, self.uploads[-1])
                    assert value.split("import-from=")[1] in self.volumes
                    value = "%s:%d/vm-%d-disk-0.qcow2,discard=on,size=%s" % (STORAGE, vmid, vmid, self.import_size)
                elif key == "ide2":
                    value = "%s:%d/vm-%d-cloudinit.qcow2,media=cdrom" % (STORAGE, vmid, vmid)
                elif key == "net0":
                    value = str(value).replace("virtio,", "virtio=BC:24:11:00:00:%02X," % (vmid % 256))
                changes[key] = value if type(value) is int else str(value)
            return self.task("qmconfig", vmid, lambda: config.update(changes))
        if (method, tail) == ("PUT", "/resize"):
            size = self.resize_to or params["size"]
            return self.task("resize", vmid, lambda: config.update(scsi0=config["scsi0"].replace("size=20G", "size=" + size)))
        if method == "POST" and tail in ("/status/start", "/status/stop"):
            verb = tail.rsplit("/", 1)[1]
            return self.task("qm" + verb, vmid, lambda: vm.update(status="running" if verb == "start" else "stopped"))
        if (method, tail) == ("DELETE", ""):
            assert params == {"purge": 1} and vm["status"] == "stopped"
            return self.task("qmdestroy", vmid, lambda: self.vms.pop(vmid))
        raise AssertionError((method, path))

    def touched(self, vmid):
        return [m for m in self.mutations if re.search(r"/qemu/%d(/|$)" % vmid, m[1]) or m[2].get("vmid") == vmid]


class FakeHost:
    """ssh-keygen and ssh: a clone answers only while it runs with this job's address and key, and the
    guest script only when it is called with exactly the expected arguments."""

    def __init__(self, pve):
        self.pve, self.argvs, self.rows, self.on_guest, self.guest_rc, self.down = pve, [], {}, None, 0, 0
        self.expect = ("kubernetes", "26.04", "1.37.2")
        self.keygen_rc, self.key_mode, self.known = 0, 0o600, "%(address)s ssh-ed25519 %(key)s\n"
        self.key_public = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFakeJobKey %s\n" % coldboot.KEY_COMMENT

    def row(self, variant, address):
        names = coldboot_guest.COMMON + (coldboot_guest.K8S if variant == "kubernetes" else coldboot_guest.OS_ONLY)
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
            os.close(os.open(key, os.O_WRONLY | os.O_CREAT | os.O_EXCL, self.key_mode))
            key.write_text("-----BEGIN OPENSSH PRIVATE KEY-----\nfake\n")
            Path(str(key) + ".pub").write_text(self.key_public)
            return subprocess.CompletedProcess(argv, self.keygen_rc)
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
            known.write_text(self.known % {"address": address, "key": self.host_key(address)})  # accept-new
        if remote == ["true"]:
            return subprocess.CompletedProcess(argv, 0, b"")
        assert kwargs["input"].startswith(b"#!/usr/bin/env python3")
        if remote != ["sudo", "-n", "/usr/bin/python3", "-I", "-", *self.expect, sha(public.split()[1])]:
            return subprocess.CompletedProcess(argv, 2, b"")  # The guest script refuses any other call.
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
        # One SSH attempt unless a test asks for more, and a short task deadline (a fake task ends at its first
        # poll): a broken run must fail here, not spin for minutes.
        self.answering = set()  # Test addresses a foreign host holds.
        for mocked in (patch.object(coldboot.subprocess, "run", self.host), patch.object(coldboot, "SSH_DEADLINE", 0),
                       patch.object(coldboot, "TASK_DEADLINE", 5),
                       patch.object(coldboot, "address_answers", lambda ip: ip in self.answering)):
            mocked.start()
            self.addCleanup(mocked.stop)
        self.slept = self.sleep.start()
        self.addCleanup(self.sleep.stop)
        handlers = {s: signal.getsignal(s) for s in (signal.SIGTERM, signal.SIGINT)}  # finish() ignores both.
        self.addCleanup(lambda: [signal.signal(s, h) for s, h in handlers.items()])

    def record(self, variant="kubernetes", os_name="ubuntu-2604", version="1.37.2", size=len(DISK), **changes):
        facts = {"kind": "OfflineSanitizationFacts", "variant": variant, "os": os_name, "disk_sha256": SHA,
                 "disk_bytes": size, "virtual_size_bytes": 20 << 30}
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

    def state(self, sdir=None):
        return json.loads(((sdir or self.sdir) / coldboot.STATE).read_text())

    def steps(self):
        """(clone states, import state) so far; ([], None) before the run has a record."""
        if not (self.sdir / coldboot.STATE).exists():
            return [], None
        return [c["state"] for c in self.state()["clones"]], self.state()["import"]

    def rewrite(self, change, sdir=None):
        state = self.state(sdir)
        change(state)
        ((sdir or self.sdir) / coldboot.STATE).write_text(json.dumps(state))

    def name(self, suffix="a"):
        return coldboot.NAME % (SHA[:8], self.state()["cfg"]["run"], suffix)

    def near_tag(self):
        """Another run's ownership tag that differs from this run's in its last character only."""
        run = self.state()["cfg"]["run"]
        return coldboot.RUN_TAG % (run[:-1] + ("0" if run[-1] != "0" else "1"))

    def volid(self):
        return "%s:import/pc-export-%s-%s.qcow2" % (STORAGE, SHA[:12], self.state()["cfg"]["run"])

    def assert_clean(self, result, clones=("destroyed", "destroyed"), volume="deleted"):
        self.assertEqual((sorted(result["cleanup"]["clones"].values()), result["cleanup"]["import_volume"]),
                         (sorted(clones), volume))
        self.assertTrue(result["cleanup"]["private_key_deleted"])
        self.assertFalse((self.sdir / coldboot.KEY).exists() or (self.sdir / (coldboot.KEY + ".pub")).exists())
        self.assertEqual(sorted(self.pve.vms), [100, 9205])
        self.assertEqual(self.pve.volumes, [])
        self.assertIs(self.state()["clean"], result["protected_set"]["unchanged"] and result["storage_set"]["unchanged"])


def stat_mode(path):
    return path.lstat().st_mode & 0o777


class HappyPathTests(ColdBootCase):
    def test_kubernetes_acceptance_end_to_end(self):
        before = {vmid: self.pve.digest(vmid) for vmid in self.pve.vms}
        result = self.run_job()
        self.assertEqual((result["accepted"], result["reason"], result["step"]), (True, None, "done"))
        self.assert_clean(result)
        run = result["run"]
        self.assertRegex(run, r"\A[0-9a-f]{12}\Z")
        self.assertEqual(result["protected_set"], {"scope": coldboot.SCOPE, "guests": 2, "unchanged": True})
        self.assertEqual(result["storage_set"], {"volumes": 0, "unchanged": True})
        self.assertEqual({vmid: self.pve.digest(vmid) for vmid in self.pve.vms}, before)
        self.assertEqual(self.pve.touched(100) + self.pve.touched(9205), [])
        self.assertEqual([(c["name"], c["all_passed"]) for c in result["clones"]],
                         [("pc-export-test-%s-%s-a" % (SHA[:8], run), True), ("pc-export-test-%s-%s-b" % (SHA[:8], run), True)])
        self.assertEqual(set(result["clones"][0]["checks"]), set(coldboot_guest.COMMON + coldboot_guest.K8S))
        self.assertEqual(result["distinct_identity"], dict.fromkeys(coldboot.IDENTITY + ("snapd_present",), True))
        self.assertEqual((result["import_volume"], result["disk_sha256"], result["pve_tls_verified"], result["api_node_room"]),
                         ("pc-export-%s-%s.qcow2" % (SHA[:12], run), SHA, False, "unreadable"))
        self.assertEqual(self.pve.uploads, [result["import_volume"]])
        state = self.state()
        self.assertEqual((state["phase"], state["clean"], state["import"], [c["state"] for c in state["clones"]]),
                         ("finished", True, "deleted", ["destroyed", "destroyed"]))
        # Hashes, booleans and run names only: no address, key, guest text, VMID, node, pool or storage id.
        text = json.dumps(result)
        self.assertFalse(re.search(r"192\.0\.2\.|ssh-ed25519|PRIVATE KEY|AAAA|%s|%s|%s" % (STORAGE, NODE, POOL), text))
        self.assertEqual(sorted(result["cleanup"]["clones"]), ["a", "b"])
        self.assertFalse(any("vmid" in row for row in result["clones"]))
        self.assertIn(("GET", "/nodes/%s/tasks/%s/status" % (API_NODE, urllib.parse.quote(state["upload_upid"], safe="")), {}),
                      self.pve.calls)  # The upload's task is polled where it runs: the API node.

    def test_create_carries_pool_and_run_name_then_the_tags_alone_then_the_config(self):
        result = self.run_job()
        run = result["run"]
        creates = [m[2] for m in self.pve.mutations if m[:2] == ("POST", QEMU)]
        self.assertEqual(creates, [{"vmid": 9290, "pool": POOL, "name": "pc-export-test-%s-%s-a" % (SHA[:8], run)},
                                   {"vmid": 9291, "pool": POOL, "name": "pc-export-test-%s-%s-b" % (SHA[:8], run)}])
        writes = [m[2] for m in self.pve.mutations if m[1] == QEMU + "/9290/config"]
        self.assertEqual(set(writes[0]), {"digest", "tags"})
        self.assertEqual(writes[0]["tags"], "proxcloud;pc-artifact-test;%s;pc-export-run-%s" % (TAG, run))
        self.assertEqual({k: writes[1][k] for k in ("scsihw", "serial0", "vga", "boot", "net0", "ide2", "scsi0")},
                         {"scsihw": "virtio-scsi-single", "serial0": "socket", "vga": "serial0", "boot": "order=scsi0",
                          "net0": "virtio,bridge=vmbr0,tag=242", "ide2": STORAGE + ":cloudinit",
                          "scsi0": "%s:0,discard=on,import-from=%s:import/%s" % (STORAGE, STORAGE, result["import_volume"])})
        self.assertFalse({"bios", "efidisk0", "tpmstate0", "cipassword"} & set().union(*writes))
        self.assertEqual({k: writes[1][k] for k in ("ostype", "cores", "memory", "cpu", "agent")},
                         {"ostype": "l26", "cores": 2, "memory": 4096, "cpu": "host", "agent": 1})
        self.assertEqual({k: v for k, v in writes[2].items() if k != "digest" and k != "sshkeys"},
                         {"ciuser": "ubuntu", "ipconfig0": "ip=192.0.2.50/24,gw=192.0.2.1", "nameserver": "1.1.1.1 8.8.8.8"})
        self.assertTrue(all("digest" in w for w in writes))
        order = [m[1].rsplit("/", 1)[1] for m in self.pve.mutations if "/qemu/9290" in m[1]]
        self.assertEqual(order, ["config", "config", "resize", "config", "start", "stop", "9290"])
        resize = next(m[2] for m in self.pve.mutations if m[1].endswith("/9290/resize"))
        self.assertEqual({k: resize[k] for k in ("disk", "size")}, {"disk": "scsi0", "size": "30G"})
        self.assertRegex(resize["digest"], r"\A[0-9a-f]{40}\Z")

    def test_an_untagged_network_and_answers_without_a_task_are_accepted(self):
        self.pve.sync = {"qmconfig", "resize", "imgdel"}  # Older PVE answers these synchronously.
        result = self.run_job(vlan=0)
        self.assertTrue(result["accepted"])
        self.assertEqual(next(m[2]["net0"] for m in self.pve.mutations if "net0" in m[2]), "virtio,bridge=vmbr0")
        self.assert_clean(result)

    def test_os_variant_swaps_the_kubernetes_checks_for_the_os_ones(self):
        self.record(variant="os", os_name="ubuntu-2404", version=None)
        self.host.expect = ("os", "24.04", "-")
        result = self.run_job(variant="os", os="ubuntu-2404", kubernetes_version=None)
        self.assertTrue(result["accepted"])
        self.assertEqual(set(result["clones"][1]["checks"]), set(coldboot_guest.COMMON + coldboot_guest.OS_ONLY))

    def test_the_guest_script_gets_exactly_this_jobs_variant_version_and_key_hash(self):
        for wrong in (("kubernetes", "26.04", "1.36.6"), ("os", "26.04", "1.37.2"), ("kubernetes", "24.04", "1.37.2")):
            with self.subTest(expect=wrong):
                self.setUp()
                self.host.expect = wrong
                self.assertEqual(self.run_job()["reason"], "guest_checks_unreadable")
        self.setUp()
        self.host.key_public = self.host.key_public.replace("FakeJobKey", "FakeJobKeyB")  # Another key, another hash.
        self.run_job()
        self.assertEqual(next(a for a in self.host.argvs if "sudo" in a)[-1], sha("AAAAC3NzaC1lZDI1NTE5AAAAIFakeJobKeyB"))

    def test_ssh_is_batch_mode_with_a_fresh_known_hosts_file_and_the_job_key(self):
        seen = []
        self.host.on_guest = lambda address: seen.append(stat_mode(self.sdir / coldboot.KEY))
        self.run_job()
        self.assertEqual(seen, [0o600, 0o600])  # Private key: state dir only, mode 600, while the job runs.
        calls = [a for a in self.host.argvs if a[0] == "ssh"]
        for argv in calls:
            for option in ("BatchMode=yes", "IdentitiesOnly=yes", "IdentityAgent=none", "GlobalKnownHostsFile=/dev/null",
                           "StrictHostKeyChecking=accept-new", "PasswordAuthentication=no", "HostKeyAlgorithms=ssh-ed25519",
                           "KbdInteractiveAuthentication=no", "ForwardAgent=no", "ClearAllForwardings=yes"):
                self.assertEqual(argv[argv.index(option) - 1], "-o")
            self.assertEqual(argv[1:3] + [argv[argv.index("-i") + 1]], ["-F", "/dev/null", str(self.sdir / coldboot.KEY)])
        self.assertEqual({next(x for x in a if x.startswith("UserKnownHostsFile=")) for a in calls},
                         {"UserKnownHostsFile=%s/known_hosts-%s" % (self.sdir, s) for s in "ab"})
        keygen = next(a for a in self.host.argvs if a[0] == "ssh-keygen")
        self.assertEqual(keygen[2:6], ["-t", "ed25519", "-N", ""])
        (self.sdir / "known_hosts-a").write_text("")  # A stale file is never reused.
        with self.assertRaises(FileExistsError):
            coldboot.guest_checks(self.sdir, {}, {"suffix": "a"}, "192.0.2.50", "k")


class RefusalTests(ColdBootCase):
    def assert_untouched(self, result, reason):
        self.assertEqual((result["accepted"], result["reason"]), (False, reason))
        self.assertEqual(self.pve.mutations, [])  # Not one write reached PVE.
        self.assertFalse((self.sdir / coldboot.KEY).exists())

    def test_test_address_already_used_refuses_before_any_mutation(self):
        self.pve.vms[100]["config"]["ipconfig1"] = "ip=192.0.2.50/24"  # Any interface of any visible guest.
        self.assert_untouched(self.run_job(), "test_address_in_use")
        self.pve.vms[100]["config"].pop("ipconfig1")
        self.pve.vms[9205]["config"]["ipconfig0"] = "ip=192.0.2.51/24,gw=192.0.2.1"
        self.assert_untouched(self.run_job(), "test_address_in_use")
        self.pve.vms[9205]["config"] = {"net1": "name=eth0,bridge=vmbr0,ip=192.0.2.50/24"}  # A container's form.
        self.assert_untouched(self.run_job(), "test_address_in_use")

    def test_test_address_held_by_a_foreign_host_refuses_before_any_mutation(self):
        # The token cannot see a guest outside its pool, and other machines share the VLAN.
        for held in ("192.0.2.50", "192.0.2.51"):
            self.answering = {held}
            self.assert_untouched(self.run_job(), "test_address_answers")

    def test_address_probe_reads_ping_then_tcp_22(self):
        probe = PROBE  # The unpatched function, captured at import.
        calls = []

        def ping(code=None, timeout=False):
            def run(argv, **kwargs):
                calls.append(argv)
                if timeout:
                    raise subprocess.TimeoutExpired(argv, 15)
                return subprocess.CompletedProcess(argv, code)
            return run

        def connect(error=None):
            def create(address, timeout):
                calls.append(address)
                if error:
                    raise error
                return types.SimpleNamespace(close=lambda: None)
            return create
        for name, run, sock, answers in (
                ("ping answers", ping(0), connect(TimeoutError()), True),
                ("ping never returns", ping(timeout=True), connect(TimeoutError()), True),
                ("ssh open", ping(1), connect(), True),
                ("ssh refused: a host is there", ping(1), connect(ConnectionRefusedError()), True),
                ("silent", ping(1), connect(TimeoutError()), False),
                ("no route", ping(1), connect(OSError(113, "No route to host")), False)):
            with self.subTest(name), patch.object(coldboot.shutil, "which", return_value="/usr/bin/ping"), \
                    patch.object(coldboot.subprocess, "run", run), patch.object(coldboot.socket, "create_connection", sock):
                self.assertIs(probe("192.0.2.50"), answers)
        self.assertEqual(calls[0], ["/usr/bin/ping", "-n", "-c", "2", "-W", "1", "192.0.2.50"])
        self.assertIn(("192.0.2.50", 22), calls)
        with patch.object(coldboot.shutil, "which", return_value=None), \
                patch.object(coldboot.socket, "create_connection", connect()):  # No ping binary: tcp/22 alone decides.
            self.assertIs(probe("192.0.2.50"), True)

    def test_upload_name_exists_refuses_and_never_deletes_that_volume(self):
        volume = "%s:import/pc-export-%s-%s.qcow2" % (STORAGE, SHA[:12], "00" * 6)
        self.pve.volumes.append(volume)
        with patch.object(coldboot.os, "urandom", return_value=bytes(6)):
            self.assert_untouched(self.run_job(), "import_volume_exists")
        self.assertEqual((self.pve.volumes, self.pve.uploads), ([volume], []))

    def test_vmid_range_exhausted(self):
        for vmid in range(9290, 9300):
            self.pve.add(vmid, pool=POOL if vmid % 2 else "other", name="foreign-%d" % vmid)
        self.pve.vms.pop(9294)  # One free id is not enough for two clones.
        self.assert_untouched(self.run_job(), "vmid_range_exhausted")
        self.assertEqual(len(self.pve.vms), 11)

    def test_range_taken_between_preflight_and_create_cleans_up_the_first_clone_only(self):
        def take(method, path, params):
            if (method, path) == ("POST", QEMU) and params["vmid"] == 9290:
                for vmid in range(9291, 9300):
                    self.pve.add(vmid, pool="other")
        self.pve.hook = take
        result = self.run_job()
        self.assertEqual((result["accepted"], result["reason"]), (False, "vmid_range_exhausted"))
        self.assertEqual(result["cleanup"]["clones"], {"a": "destroyed"})
        self.assertEqual(sorted(self.pve.vms), [100, 9205] + list(range(9291, 9300)))
        self.assertEqual(sum(len(self.pve.touched(v)) for v in range(9291, 9300)), 0)

    def test_foreign_vms_in_the_range_are_never_touched(self):
        self.pve.add(9290, name="pc-export-test-%s-%s-a" % (SHA[:8], "0" * 12), tags="proxcloud;pc-artifact-test;" + TAG)  # Lookalike.
        self.pve.add(9291, pool="customers", name="tenant-vm", status="running")  # Invisible to the token.
        before = {vmid: (self.pve.digest(vmid), self.pve.vms[vmid]["status"]) for vmid in (9290, 9291)}
        result = self.run_job()
        self.assertTrue(result["accepted"])
        self.assertEqual([c["vmid"] for c in self.state()["clones"]], [9292, 9293])
        self.assertEqual(self.pve.touched(9290) + self.pve.touched(9291), [])
        self.assertEqual({vmid: (self.pve.digest(vmid), self.pve.vms[vmid]["status"]) for vmid in (9290, 9291)}, before)
        self.assertEqual((result["protected_set"]["guests"], result["protected_set"]["unchanged"]), (3, True))

    def test_a_vmid_that_still_owns_a_volume_is_not_reused(self):
        self.pve.orphans[9290] = ["%s:9290/vm-9290-disk-0.qcow2" % STORAGE]  # A killed import left it in no config.
        result = self.run_job()
        self.assertTrue(result["accepted"])
        self.assertEqual([c["vmid"] for c in self.state()["clones"]], [9291, 9292])
        self.assertEqual((self.pve.touched(9290), result["storage_set"]), ([], {"volumes": 1, "unchanged": True}))

    def test_sanitize_run_must_be_sealed_and_match_the_disk(self):
        cwd = os.getcwd()
        self.addCleanup(os.chdir, cwd)
        os.chdir(self.root)  # So that the relative path does name the disk: only "not absolute" refuses it.
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
        self.record(size=len(DISK) + 1)  # The file is not the size the sealed facts state.
        self.assert_untouched(self.run_job(), "sealed_disk_refused")
        self.record()
        with patch.object(coldboot, "MAX_DISK_BYTES", len(DISK) - 1):  # Larger than the API node may spool.
            self.assert_untouched(self.run_job(), "sanitize_run_refused")
        for name, change in {"writable": lambda: self.disk.chmod(0o600), "hardlink": lambda: os.link(self.disk, self.root / "alias"),
                             "content": lambda: (self.disk.chmod(0o600), self.disk.write_bytes(DISK[::-1]), self.disk.chmod(0o400))}.items():
            with self.subTest(case=name):
                change()
                self.assert_untouched(self.run_job(), "sealed_disk_refused")
                self.disk.chmod(0o400)
                (self.root / "alias").unlink(missing_ok=True)

    def test_api_node_room_is_checked_when_the_token_may_read_it(self):
        self.pve.rootfs = len(DISK) + coldboot.ROOM_MARGIN - 1
        self.assert_untouched(self.run_job(), "api_node_no_room")
        self.pve.rootfs = len(DISK) + coldboot.ROOM_MARGIN
        result = self.run_job()
        self.assertEqual((result["accepted"], result["api_node_room"]), (True, "checked"))
        self.assertIn(("GET", "/nodes/%s/status" % API_NODE, {}), self.pve.calls)  # The node pveproxy spools on.
        failing = types.SimpleNamespace(call=lambda *a, **k: (_ for _ in ()).throw(coldboot.ApiError(596)))
        with self.assertRaises(coldboot.ApiError):  # Only "not allowed" means unreadable.
            coldboot.api_node_room(failing, 1)
        for rows in ([], [{"type": "node", "name": "a", "local": 1}, {"type": "node", "name": "b", "local": 1}],
                     [{"type": "node", "name": "a/b", "local": 1}]):
            with self.subTest(rows=rows), self.assertRaises(coldboot.Refused):
                coldboot.api_node_room(types.SimpleNamespace(call=lambda *a, rows=rows, **k: rows), 1)

    def test_settings_refuse_odd_inputs(self):
        coldboot.settings(self.args(vmid_range="9200-9299", vlan=4094))  # The widest range and the last VLAN.
        v6 = {"test_address": ["2001:db8::50/64", "2001:db8::51/64"], "gateway": "2001:db8::1", "nameserver": ["2001:db8::53"]}
        for changes in ({"test_address": [ADDRESSES[0]]}, {"test_address": [ADDRESSES[0]] * 2},
                        {"test_address": ["192.0.2.50/24", "198.51.100.51/24"]}, {"test_address": ["192.0.2.50", "192.0.2.51"]},
                        {"test_address": ["192.0.2.0/24", "192.0.2.51/24"]}, {"test_address": ["192.0.2.1/24", "192.0.2.51/24"]},
                        {"test_address": ["192.0.2.255/24", "192.0.2.51/24"]}, v6,
                        {"gateway": "198.51.100.1"}, {"gateway": "x"}, {"nameserver": []}, {"nameserver": ["dns.example"]},
                        {"vmid_range": "9299-9290"}, {"vmid_range": "1-99999"}, {"vmid_range": "9200-9300"}, {"vmid_range": "9290"},
                        {"vlan": 4095}, {"vlan": -1},
                        {"node": "pve1/../x"}, {"storage": "a:b"}, {"bridge": "vmbr0,firewall=0"}, {"pool": ""},
                        {"kubernetes_version": "1.30.0"}, {"kubernetes_version": None}, {"os": "ubuntu-2404"},
                        {"variant": "os"}, {"os": "debian-13", "variant": "os", "kubernetes_version": None}):
            with self.subTest(changes=changes), self.assertRaises(coldboot.Refused):
                coldboot.settings(self.args(**changes))

    def test_state_dir_must_be_private_plain_and_a_previous_record_is_never_overwritten(self):
        self.sdir.chmod(0o755)
        link, odd = self.root / "link", self.root / "odd %h"
        for path in (odd, self.root / "private"):
            path.mkdir(mode=0o700)
        link.symlink_to(self.root / "private")  # A private directory, reached through a link.
        self.assertEqual(coldboot.state_dir(self.root / "private"), self.root / "private")
        for path in (self.sdir, self.root / "absent", Path("relative"), self.disk, link, odd):
            with self.subTest(path=path), self.assertRaises(coldboot.Refused):
                coldboot.state_dir(path)
        self.sdir.chmod(0o700)
        self.assertEqual(coldboot.state_dir(str(self.sdir)), self.sdir)
        with patch.object(coldboot.os, "getuid", return_value=os.getuid() + 1), self.assertRaises(coldboot.Refused):
            coldboot.state_dir(self.sdir)  # Another user's directory.
        (self.sdir / coldboot.STATE).write_text("{}")
        self.assert_untouched(self.run_job(), "state_exists")
        self.assertEqual((self.sdir / coldboot.STATE).read_text(), "{}")

    def test_keygen_must_leave_one_private_ed25519_key_of_this_job(self):
        cases = {"rc": lambda: setattr(self.host, "keygen_rc", 1), "mode": lambda: setattr(self.host, "key_mode", 0o644),
                 "type": lambda: setattr(self.host, "key_public", self.host.key_public.replace("ssh-ed25519", "ssh-rsa")),
                 "comment": lambda: setattr(self.host, "key_public", "ssh-ed25519 AAAA someone@else\n")}
        for name, arrange in cases.items():
            with self.subTest(case=name):
                self.setUp()
                arrange()
                result = self.run_job()
                self.assert_untouched(result, "keygen_failed")
                self.assertEqual(result["cleanup"], {"clones": {}, "import_volume": "not_uploaded", "private_key_deleted": True})
        for stale in (coldboot.KEY, coldboot.KEY + ".pub"):
            with self.subTest(stale=stale):
                self.setUp()
                (self.sdir / stale).write_text("x")
                self.assert_untouched(self.run_job(), "key_exists")
                self.assertEqual([a for a in self.host.argvs if a[0] == "ssh-keygen"], [])


class FailureCleanupTests(ColdBootCase):
    def test_state_file_names_each_step_before_it_happens(self):
        seen = []

        def watch(method, path, params):
            state = self.state() if (self.sdir / coldboot.STATE).exists() else None
            if (method, path) == ("POST", QEMU):
                seen.append(("create", params["vmid"], [(c["vmid"], c["state"]) for c in state["clones"]][-1],
                             state["import"], stat_mode(self.sdir / coldboot.STATE)))
            elif method == "DELETE" and path.startswith(QEMU):
                seen.append(("destroy", [c["state"] for c in state["clones"] if path.endswith(str(c["vmid"]))]))
        self.pve.hook = watch
        upload = self.pve.upload
        self.pve.upload = lambda *a: (seen.append(("upload", self.state()["import"])), upload(*a))[1]
        self.run_job()
        self.assertEqual(seen, [("upload", "sending"), ("create", 9290, (9290, "reserved"), "present", 0o600),
                                ("create", 9291, (9291, "reserved"), "present", 0o600),
                                ("destroy", ["destroying"]), ("destroy", ["destroying"])])

    def test_guest_check_failure_still_cleans_up(self):
        for check in ("k8s_binaries_exact", "snapd_no_change_errors"):
            with self.subTest(check=check):
                self.setUp()
                row = self.host.row("kubernetes", "192.0.2.51")
                self.host.rows["192.0.2.51"] = {"checks": dict(row["checks"], **{check: False}), "snapd_changes_error_lines": 7}
                result = self.run_job()
                self.assertEqual((result["accepted"], result["reason"], result["step"]), (False, "guest_checks_failed", "guest_checks"))
                self.assertEqual([c["all_passed"] for c in result["clones"]], [True, False])
                self.assert_clean(result)
                self.assertTrue(result["protected_set"]["unchanged"])

    def test_identical_or_unproven_identities_are_refused(self):
        identity = ("machine_id_sha256", "host_key_sha256", "snapd_serial_sha256", "snapd_device_key_sha256")
        self.assertEqual(coldboot.IDENTITY, identity)
        for key in identity:
            with self.subTest(key=key):
                self.setUp()
                self.host.rows = {a.split("/")[0]: {key: "a" * 64} for a in ADDRESSES}
                if key == "host_key_sha256":  # Both clones really present one host key.
                    self.host.rows, self.host.host_key = {}, lambda address: "AAAAsameHostKey"
                result = self.run_job()
                self.assertEqual((result["accepted"], result["reason"]), (False, "identity_not_distinct"))
                self.assertEqual({k for k, v in result["distinct_identity"].items() if not v}, {key})
                self.assert_clean(result)
        self.setUp()  # A serial that never came back is not a new identity either.
        self.host.rows = {"192.0.2.50": {"snapd_serial_sha256": None}}
        self.assertEqual(self.run_job()["reason"], "identity_not_distinct")
        absent = {"snapd_present": False, "snapd_serial_sha256": None, "snapd_device_key_sha256": None}
        for rows in ({a.split("/")[0]: absent for a in ADDRESSES},  # No snapd on either clone proves nothing.
                     {"192.0.2.51": {"snapd_present": False}}):  # ... and one saying so while it still reports hashes.
            with self.subTest(rows=rows):
                self.setUp()
                self.host.rows = rows
                result = self.run_job()
                self.assertEqual((result["accepted"], result["reason"]), (False, "identity_not_distinct"))
                self.assertIs(result["distinct_identity"]["snapd_present"], False)

    def test_guest_output_is_gated_and_never_copied(self):
        good = self.host.row("kubernetes", "192.0.2.50")
        for name, change in {"extra": {"hostname": "leak"}, "text": {"machine_id_sha256": "not-a-hash"},
                             "nonbool": {"checks": dict(good["checks"], no_active_swap="yes")},
                             "missing": {"checks": {k: v for k, v in good["checks"].items() if k != "disk_30g"}},
                             "unknown": {"checks": dict(good["checks"], surprise=True)},
                             "os_only": {"checks": dict(good["checks"], apt_metadata_available=True)},
                             "count": {"snapd_changes_error_lines": "many"}, "huge": {"snapd_changes_error_lines": 1 << 20},
                             "warnings": {"cloud_init_recoverable_warnings": -1}, "present": {"snapd_present": "yes"},
                             "serial": {"snapd_serial_sha256": "short"}, "device": {"snapd_device_key_sha256": 5},
                             "hostkey_text": {"host_key_sha256": "ssh-ed25519 AAAA"},
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
        for known in ("%(address)s ssh-ed25519 %(key)s\n%(address)s ssh-ed25519 AAAAsecond\n",  # Two keys on record.
                      "%(address)s ssh-rsa %(key)s\n", "%(address)s ssh-ed25519 %(key)s comment\n", ""):
            with self.subTest(known=known):
                self.setUp()
                self.host.known = known  # The last one: ssh recorded nothing at all.
                self.assertEqual(self.run_job()["reason"], "host_key_mismatch")
        line = json.dumps(good).encode()
        self.assertEqual(coldboot.guest_record(line + b" " * (65536 - len(line)), "kubernetes"), good)
        with self.assertRaises(coldboot.Refused):  # Valid, and one byte over the bound.
            coldboot.guest_record(line + b" " * (65537 - len(line)), "kubernetes")
        with self.assertRaises(coldboot.Refused):
            coldboot.guest_record(b"\xff{", "os")

    def test_unreachable_guest_and_failed_tasks_clean_up(self):
        self.host.down = 10 ** 9
        result = self.run_job()
        self.assertEqual((result["accepted"], result["reason"], result["step"]), (False, "ssh_unreachable", "guest_checks"))
        self.assert_clean(result)
        for kind, cleaned in (("qmconfig", ("destroyed",)), ("resize", ("destroyed",)), ("qmstart", ("destroyed",)), ("imgcopy", ())):
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

    def test_a_failed_or_refused_create_leaves_nothing_and_touches_nothing(self):
        self.pve.fail["qmcreate"] = "unable to create VM"
        result = self.run_job()
        self.assertEqual((result["reason"], result["cleanup"]["clones"]), ("task_failed", {"a": "not_created"}))
        self.assertEqual([m[:2] for m in self.pve.touched(9290)], [("POST", QEMU)])
        self.assert_clean(result, clones=("not_created",))
        for status in (403, 500, 596):  # PVE refused it (403, 500), or nobody knows what happened (596).
            with self.subTest(status=status):
                self.setUp()

                def refuse(method, path, params, status=status):
                    if (method, path) == ("POST", QEMU):
                        raise coldboot.ApiError(status)
                self.pve.hook = refuse
                result = self.run_job()
                self.assertEqual((result["reason"], result["cleanup"]["clones"]), ("pve_http_%d" % status, {"a": "not_created"}))
                self.assertEqual(self.state()["clones"][0]["state"], "not_created")
                self.assert_clean(result, clones=("not_created",))

    def test_a_create_that_is_still_running_is_left_alone_and_removed_once_it_is_this_runs_vm(self):
        def cancelled(method, path, params):  # The job is cancelled while the create task runs.
            if "/tasks/" in path and ":qmcreate:" in urllib.parse.unquote(path):
                raise coldboot.Refused("terminated")
        self.pve.hook = cancelled
        result = self.run_job()
        self.assertEqual((result["reason"], result["cleanup"]["clones"]), ("terminated", {"a": "refused_not_owned_pool"}))
        self.assertEqual((self.state()["clones"][0]["state"], [m[:2] for m in self.pve.touched(9290)]), ("reserved", [("POST", QEMU)]))
        self.pve.hook = None
        self.pve.vms[9290]["pool"] = POOL  # The task ended: the VM is in the pool with the run name and no tag.
        result = coldboot.cleanup_only(self.pve, self.sdir)
        self.assertEqual((result["clean"], result["cleanup"]["clones"], sorted(self.pve.vms)), (True, {"a": "destroyed"}, [100, 9205]))

    def test_an_untagged_vm_is_this_runs_only_in_the_window_before_the_tag_write(self):
        self.pve.fail["qmconfig"] = "x"  # The tag write never happened: name, pool and "no tag at all" are the proof.
        result = self.run_job()
        self.assertEqual((result["reason"], result["cleanup"]["clones"]), ("task_failed", {"a": "destroyed"}))
        self.setUp()
        self.pve.hook = lambda m, p, q: q.update(tags="") if "tags" in q else None  # PVE "wrote" no tag.
        result = self.run_job()
        self.assertEqual((result["reason"], result["cleanup"]["clones"]), ("tags_readback", {"a": "destroyed"}))

    def test_a_created_vm_that_is_not_fresh_is_not_adopted(self):
        cases = {"name": ({"name": "someone-else"}, "not_created"), "tags": ({"tags": "pc-base"}, "refused_not_owned_identity"),
                 "template": ({"template": 1}, "refused_not_owned_template_or_lock"),
                 "lock": ({"lock": "clone"}, "refused_not_owned_template_or_lock"),
                 "disk": ({"scsi0": "other:9290/vm-9290-disk-0.raw"}, "refused_unexpected_disk")}
        for name, (change, outcome) in cases.items():
            with self.subTest(case=name):
                self.setUp()

                def adopt(method, path, params, change=change):  # The id holds something that is not a fresh empty VM.
                    if method == "GET" and path.endswith("/9290/config"):
                        self.pve.vms[9290]["config"].update(change)
                self.pve.hook = adopt
                result = self.run_job()
                self.assertEqual((result["accepted"], result["reason"]), (False, "fresh_vm_not_empty"))
                self.assertEqual(result["cleanup"]["clones"], {"a": outcome})
                self.assertEqual([m[1] for m in self.pve.touched(9290)], [QEMU])  # The create, nothing after.
                self.assertIn(9290, self.pve.vms)
                self.assertIs(result["protected_set"]["unchanged"], False)  # Reported, not hidden.

    def test_a_created_vm_must_have_joined_the_pool(self):
        def moved(method, path, params):
            vm = self.pve.vms.get(9290)
            if path == "/pools/" + POOL and vm and vm["pool"] == POOL and "tags" not in vm["config"]:
                vm["pool"] = "customers"
        self.pve.hook = moved
        result = self.run_job()
        self.assertEqual((result["reason"], result["cleanup"]["clones"]), ("fresh_vm_not_in_pool", {"a": "refused_not_owned_pool"}))
        self.assertEqual([m[1] for m in self.pve.touched(9290)], [QEMU])
        api = types.SimpleNamespace(call=lambda *a, **k: {"members": [
            {"vmid": 9290, "node": "pve2", "type": "qemu"}, {"vmid": 9290, "node": NODE, "type": "lxc"},
            {"vmid": 9291, "node": NODE, "type": "qemu"}]})
        cfg = {"pool": POOL, "node": NODE}
        self.assertEqual([coldboot.in_pool(api, cfg, vmid) for vmid in (9290, 9291)], [False, True])  # This node, a VM.

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

    def test_every_write_is_read_back_before_the_boot(self):
        def late(key, **change):  # The config differs from what was asked once `key` is there.
            def hook(method, path, params):
                config = self.pve.vms.get(9290, {}).get("config", {})
                if method == "GET" and path.endswith("/9290/config") and key in config:
                    config.update({k: v(config) if callable(v) else v for k, v in change.items()})
            return lambda: setattr(self.pve, "hook", hook)
        cases = {"import_not_20g": lambda: setattr(self.pve, "import_size", "10G"),
                 "resize_not_30g": lambda: setattr(self.pve, "resize_to", "25G"),
                 "network_readback": late("net0", net0=lambda c: c["net0"].replace(",tag=242", "")),
                 "network_readback bridge": late("net0", net0=lambda c: c["net0"].replace("vmbr0", "vmbr1")),
                 "unexpected_disk": late("ciuser", unused0="%s:9290/vm-9290-disk-1.qcow2" % STORAGE)}
        for reason, arrange in cases.items():
            with self.subTest(reason=reason):
                self.setUp()
                arrange()
                result = self.run_job()
                self.assertEqual((result["accepted"], result["reason"]), (False, reason.split()[0]))
                self.assertEqual([m for m in self.pve.mutations if m[1].endswith("/status/start")], [])
                self.assert_clean(result, clones=("destroyed",))

    def test_ownership_is_reread_after_every_write_the_resize_and_the_start(self):
        sites = {"tags": lambda p, q: p.endswith("/9290/config") and "tags" in q,
                 "hardware": lambda p, q: p.endswith("/9290/config") and "ostype" in q,
                 "resize": lambda p, q: p.endswith("/9290/resize"),
                 "cloudinit": lambda p, q: p.endswith("/9290/config") and "ciuser" in q,
                 "start": lambda p, q: p.endswith("/9290/status/start")}
        for site, at in sites.items():
            with self.subTest(site=site):
                self.setUp()
                taken = []

                def restore():
                    if taken == [1]:
                        taken.append(self.pve.vms[9290]["config"].update(name=self.name()))

                def hook(method, path, params, at=at):  # Someone renames the VM while exactly this one step's task runs.
                    if method != "GET" and "/qemu/9290" in path:
                        taken.append(0) if at(path, params) else restore()
                    elif "/tasks/" in path and taken == [0]:
                        taken[:] = [self.pve.vms[9290]["config"].update(name="taken-over") or 1]
                self.pve.hook, self.host.on_guest = hook, lambda address: restore()
                result = self.run_job()
                self.assertEqual((taken[:1], result["accepted"], result["reason"]), ([1], False, "not_owned_identity"))

    def test_a_task_of_another_kind_is_never_waited_for(self):
        for kind in ("qmcreate", "qmconfig", "resize", "qmstart"):
            with self.subTest(kind=kind):
                self.setUp()
                self.pve.kinds[kind] = "qmsnapshot"
                self.assertEqual(self.run_job()["reason"], "task_id_unexpected")
        for kind in ("qmcreate", "qmstart"):  # ... and a VM task that answers with no task id is not a done task.
            with self.subTest(sync=kind):
                self.setUp()
                self.pve.sync.add(kind)
                self.assertEqual(self.run_job()["reason"], "task_id_missing")
        for kind in ("qmstop", "qmdestroy"):
            with self.subTest(kind=kind):
                self.setUp()
                self.pve.kinds[kind] = "qmsnapshot"
                result = self.run_job()
                self.assertEqual((result["reason"], set(result["cleanup"]["clones"].values())),
                                 ("cleanup_incomplete", {"refused_task_id_unexpected"}))

    def test_a_start_or_stop_that_did_not_happen_is_noticed(self):
        self.pve.broken.add("qmstart")
        result = self.run_job()
        self.assertEqual((result["reason"], self.host.argvs[1:]), ("unexpected_status", []))  # No SSH to a VM that is not up.
        self.assert_clean(result, clones=("destroyed",))
        self.setUp()
        self.pve.broken.add("qmstop")
        result = self.run_job()
        self.assertEqual(result["cleanup"]["clones"], {"a": "refused_unexpected_status", "b": "refused_unexpected_status"})
        self.assertEqual([m for m in self.pve.mutations if m[0] == "DELETE" and m[1].startswith(QEMU)], [])

    def test_cleanup_refuses_a_vm_whose_name_tag_pool_or_disks_changed(self):
        vm = lambda **config: lambda vm: vm["config"].update(config)  # noqa: E731
        changes = {"name": (vm(name="renamed"), "refused_not_owned_identity"),
                   "tag": (vm(tags="proxcloud;pc-artifact-test;" + TAG), "refused_not_owned_identity"),
                   "other_run": (lambda vm: vm["config"].update(tags="proxcloud;" + self.near_tag()), "refused_not_owned_identity"),
                   "untagged": (vm(tags=""), "refused_not_owned_identity"),  # The no-tag window closed with the tag write.
                   "pool": (lambda vm: vm.update(pool="customers"), "refused_not_owned_pool"),
                   "template": (vm(template=1), "refused_not_owned_template_or_lock"),
                   "lock": (vm(lock="backup"), "refused_not_owned_template_or_lock"),
                   "disk": (vm(scsi1="other:100/base-100-disk-0.raw"), "refused_unexpected_disk"),
                   "disk_other_vmid": (vm(scsi1="%s:100/vm-100-disk-0.qcow2" % STORAGE), "refused_unexpected_disk"),
                   "disk_other_storage": (vm(virtio0="local:9290/vm-9290-disk-0.qcow2"), "refused_unexpected_disk"),
                   "disk_unused": (vm(unused0="%s:100/vm-100-disk-1.qcow2" % STORAGE), "refused_unexpected_disk"),
                   "efi": (vm(efidisk0="x"), "refused_unexpected_firmware"),
                   "tpm": (vm(tpmstate0="%s:9290/vm-9290-disk-1.raw" % STORAGE), "refused_unexpected_firmware"),
                   "bios": (vm(bios="ovmf"), "refused_unexpected_firmware"),
                   "snapshot": (lambda vm: vm.update(snapshots=["kept"]), "refused_unexpected_snapshot"),
                   "pending": (lambda vm: vm.update(pending=[{"key": "memory", "value": 4096, "pending": 8192}]), "refused_unexpected_pending"),
                   "pending_delete": (lambda vm: vm.update(pending=[{"key": "net1", "value": "x", "delete": 1}]), "refused_unexpected_pending")}
        for name, (change, outcome) in changes.items():
            with self.subTest(change=name):
                self.setUp()
                self.host.on_guest = lambda address, change=change: address.endswith(".51") and change(self.pve.vms[9290])
                result = self.run_job()
                self.assertEqual((result["accepted"], result["reason"]), (False, "cleanup_incomplete"))
                self.assertEqual(result["cleanup"]["clones"], {"a": outcome, "b": "destroyed"})
                self.assertIn(9290, self.pve.vms)
                self.assertEqual(self.pve.vms[9290]["status"], "running")  # Not even stopped.
                self.assertEqual([m for m in self.pve.touched(9290) if m[1].endswith(("/stop", "/9290"))], [])
                self.assertTrue(result["cleanup"]["private_key_deleted"])
                self.assertEqual(result["protected_set"]["unchanged"], name == "pool")  # A leftover is "something new".
                self.assertEqual(self.pve.volumes, [])  # The run's own upload still goes.
                self.assertIs(self.state()["clean"], False)

    def test_cleanup_rereads_ownership_between_stop_and_destroy(self):
        self.pve.hook = lambda m, p, q: p.endswith("/9290/status/stop") and self.pve.vms[9290]["config"].update(name="taken-over")
        result = self.run_job()
        self.assertEqual(result["cleanup"]["clones"], {"a": "refused_not_owned_identity", "b": "destroyed"})
        self.assertIn(9290, self.pve.vms)
        self.pve.hook = None
        self.setUp()
        self.pve.fail["qmdestroy"] = "storage busy"
        result = self.run_job()
        self.assertEqual((result["reason"], set(result["cleanup"]["clones"].values())), ("cleanup_incomplete", {"refused_task_failed"}))

    def test_a_destroy_or_delete_that_left_something_is_not_reported_done(self):
        self.pve.broken.add("qmdestroy")  # The task says OK and the VM is still there.
        result = self.run_job()
        self.assertEqual((result["reason"], set(result["cleanup"]["clones"].values())), ("cleanup_incomplete", {"refused_destroy_not_confirmed"}))
        self.setUp()
        self.pve.broken.add("imgdel")
        result = self.run_job()
        self.assertEqual((result["reason"], result["cleanup"]["import_volume"], self.pve.volumes),
                         ("cleanup_incomplete", "refused_import_volume_remains", [self.volid()]))
        self.setUp()  # purge frees what the config names; a disk in no config stays and is reported.
        left = "%s:9290/vm-9290-disk-1.qcow2" % STORAGE
        self.pve.hook = lambda m, p, q: (m, p) == ("DELETE", QEMU + "/9290") and self.pve.orphans.update({9290: [left]})
        result = self.run_job()
        self.assertEqual((result["reason"], result["cleanup"]["clones"], result["storage_set"]["unchanged"]),
                         ("cleanup_incomplete", {"a": "refused_volumes_remain", "b": "destroyed"}, False))
        self.assertEqual(self.state()["clones"][0]["state"], "destroying")

    def test_an_upload_that_ended_ok_without_its_file_is_refused(self):
        self.pve.broken.add("imgcopy")
        result = self.run_job()
        self.assertEqual((result["reason"], result["cleanup"]["import_volume"]), ("import_volume_missing", "absent"))
        self.assertEqual([m for m in self.pve.mutations if m[0] != "UPLOAD"], [])

    def test_import_volume_still_referenced_is_kept(self):
        self.host.on_guest = lambda address: self.pve.vms[9205]["config"].update(scsi5=self.volid())
        result = self.run_job()
        self.assertEqual(result["cleanup"]["import_volume"], "refused_import_volume_referenced")
        self.assertEqual((self.pve.volumes, result["accepted"], result["reason"]), ([self.volid()], False, "cleanup_incomplete"))

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
                self.assertEqual(result["protected_set"], {"scope": coldboot.SCOPE, "guests": 2, "unchanged": False})
                self.assertEqual(sorted(result["cleanup"]["clones"].values()), ["destroyed", "destroyed"])
                self.assertEqual(self.pve.touched(100) + self.pve.touched(9205) + self.pve.touched(9300), [])
                self.assertIs(self.state()["clean"], False)

    def test_storage_drift_is_detected_and_a_foreign_volume_is_never_deleted(self):
        other = STORAGE + ":import/pc-tu-0123456789abcdef-e1-000000000000.qcow2"
        for name, drift in {"new": lambda: self.pve.volumes.append(other), "gone": lambda: self.pve.volumes.remove(other)}.items():
            with self.subTest(drift=name):
                self.setUp()
                if name == "gone":
                    self.pve.volumes.append(other)
                self.host.on_guest = lambda address, drift=drift: address.endswith(".50") and drift()
                result = self.run_job()
                self.assertEqual((result["accepted"], result["reason"]), (False, "storage_set_changed"))
                self.assertEqual((result["storage_set"]["unchanged"], result["protected_set"]["unchanged"]), (False, True))
                self.assertIs(self.state()["clean"], False)  # So a later `cleanup` looks again.
                self.assertEqual(self.pve.volumes, [other] if name == "new" else [])
                self.assertEqual([m for m in self.pve.mutations if m[0] == "DELETE" and other in urllib.parse.unquote(m[1])], [])

    def test_a_snapshot_that_cannot_be_read_again_counts_as_changed(self):
        for path, reason in (("/cluster/resources", "protected_set_changed"), (CONTENT, "storage_set_changed")):
            with self.subTest(path=path):
                self.setUp()
                seen = []

                def fail_last(method, target, params, path=path):  # Only the final comparison's read fails.
                    if target == path and not params.get("content") and "vmid" not in params:
                        seen.append(1)
                        if self.steps() == (["destroyed", "destroyed"], "deleted"):
                            raise coldboot.ApiError(596)
                self.pve.hook = fail_last
                result = self.run_job()
                self.assertEqual((result["accepted"], result["reason"]), (False, reason))
                self.assertEqual(sorted(result["cleanup"]["clones"].values()), ["destroyed", "destroyed"])

    def test_a_private_key_that_could_not_be_removed_is_not_a_clean_run(self):
        with patch.object(coldboot.Path, "unlink", side_effect=PermissionError()):
            result = self.run_job()
        self.assertEqual((result["accepted"], result["reason"], result["cleanup"]["private_key_deleted"]),
                         (False, "cleanup_incomplete", False))
        self.assertEqual((sorted(result["cleanup"]["clones"].values()), self.state()["clean"]), (["destroyed", "destroyed"], False))
        result = coldboot.cleanup_only(self.pve, self.sdir)  # The repeat removes it without one PVE write.
        self.assertEqual((result["clean"], result["cleanup"]["private_key_deleted"], (self.sdir / coldboot.KEY).exists()),
                         (True, True, False))

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
        none = coldboot.cleanup_only(self.pve, self.sdir)
        self.assertEqual((none["reason"], none["clean"], self.pve.calls), ("no_state", True, []))
        self.host.on_guest = lambda address: (_ for _ in ()).throw(KeyboardInterrupt())
        with patch.object(coldboot, "finish", side_effect=KeyboardInterrupt()):  # Killed before any cleanup ran.
            result = self.run_job()
        self.assertEqual((result["accepted"], sorted(self.pve.vms)), (False, [100, 9205, 9290, 9291]))
        self.assertTrue((self.sdir / coldboot.KEY).exists())
        self.pve.add(9292, name=self.name("a")[:-1] + "c", tags=self.pve.vms[9290]["config"]["tags"])  # Never adopted by prefix.
        result = coldboot.cleanup_only(self.pve, self.sdir)
        self.assertEqual((result["clean"], result["protected_set"]["unchanged"]), (False, False))  # 9292 is new.
        self.assertEqual(result["cleanup"], {"clones": {"a": "destroyed", "b": "destroyed"},
                                              "import_volume": "deleted", "private_key_deleted": True})
        self.assertEqual((sorted(self.pve.vms), self.pve.touched(9292)), ([100, 9205, 9292], []))
        self.pve.vms.pop(9292)
        calls = len(self.pve.calls)
        result = coldboot.cleanup_only(self.pve, self.sdir)  # Terminal outcomes are recorded: only the comparison is left.
        self.assertEqual((result["clean"], result["cleanup"]["clones"], result["cleanup"]["import_volume"]),
                         (True, {"a": "destroyed", "b": "destroyed"}, "deleted"))
        self.assertEqual({c[1] for c in self.pve.calls[calls:]} & {QEMU, "/cluster/nextid", "/pools/" + POOL}, set())
        calls = len(self.pve.calls)
        result = coldboot.cleanup_only(self.pve, self.sdir)  # Recorded clean: not one call.
        self.assertEqual((result["clean"], result["reason"], len(self.pve.calls)), (True, "already_clean", calls))

    def test_tampered_state_file_is_refused_before_any_call(self):
        self.run_job()
        good = self.state()
        for name, change in {"name": lambda s: s["clones"][0].update(name="pc-ubuntu-2604-k8s"),
                             "old_name": lambda s: s["clones"][0].update(name="pc-export-test-%s-a" % SHA[:8]),
                             "vmid": lambda s: s["clones"][0].update(vmid=100),
                             "run": lambda s: s["cfg"].update(run="../../etc"), "no_run": lambda s: s["cfg"].pop("run"),
                             "run_alone": lambda s: (s["cfg"].update(run="0" * 11 + "/"), s.update(clones=[])),
                             "three": lambda s: s["clones"].append(dict(s["clones"][0], suffix="c", name=s["clones"][0]["name"][:-1] + "c")),
                             "sha": lambda s: s["cfg"].update(sha256="x"), "clones": lambda s: s.update(clones=s["clones"] * 2),
                             "twice": lambda s: s["clones"][1].update(suffix="a", name=s["clones"][0]["name"]),
                             "step": lambda s: s["clones"][0].update(state="adopt"), "import": lambda s: s.update({"import": "yes"}),
                             "upid": lambda s: s.update(upload_upid=5), "sent": lambda s: s.update(import_sent_at="now"),
                             "node": lambda s: s["cfg"].update(node="a/b")}.items():
            with self.subTest(case=name):
                state = json.loads(json.dumps(good))
                change(state)
                state["clean"] = False
                (self.sdir / coldboot.STATE).write_text(json.dumps(state))
                calls = len(self.pve.calls)
                result = coldboot.cleanup_only(self.pve, self.sdir)
                self.assertEqual((result["clean"], result["reason"], len(self.pve.calls)), (False, "state_refused", calls))


class CrossRunTests(ColdBootCase):
    """Ownership is this run's, not the artifact's: two runs of one sealed disk never share a name."""

    def test_two_runs_of_one_disk_use_different_names_tags_and_files(self):
        first = self.run_job()
        self.sdir = self.root / "state2"
        self.sdir.mkdir(mode=0o700)
        second = self.run_job()
        self.assertTrue(first["accepted"] and second["accepted"])
        self.assertNotEqual(first["run"], second["run"])
        self.assertEqual(len(set(self.pve.uploads)), 2)
        self.assertEqual(len({m[2]["name"] for m in self.pve.mutations if m[:2] == ("POST", QEMU)}), 4)
        self.assertEqual(len({m[2]["tags"] for m in self.pve.mutations if "tags" in m[2]}), 2)

    def test_cleanup_of_an_earlier_run_never_touches_a_later_run_of_the_same_disk(self):
        fired = []

        def refuse_once(method, path, params):
            if (method, path) == ("POST", QEMU) and not fired:
                fired.append(1)
                raise coldboot.ApiError(500)
        self.pve.hook = refuse_once
        first = self.run_job()
        self.assertEqual((first["reason"], first["cleanup"]["clones"]), ("pve_http_500", {"a": "not_created"}))
        self.pve.hook = None
        old, self.sdir = self.sdir, self.root / "state2"
        self.sdir.mkdir(mode=0o700)
        seen = []

        def during(address):  # The earlier run's cleanup is repeated while this run's clones are up.
            if address.endswith(".51"):
                calls, writes = len(self.pve.calls), len(self.pve.mutations)
                seen.append(coldboot.cleanup_only(self.pve, old))
                seen.append((len(self.pve.calls) - calls, len(self.pve.mutations) - writes))
                # The worst record: that run believes it started 9290 and that its upload is still there.
                self.rewrite(lambda s: (s["clones"][0].update(state="started"), s.update({"clean": False, "import": "present"})), old)
                seen.append(coldboot.cleanup_only(self.pve, old))
                seen.append(len(self.pve.mutations) - writes)
        self.host.on_guest = during
        second = self.run_job()
        self.assertEqual((seen[0]["reason"], seen[1]), ("already_clean", (0, 0)))  # A clean finished record is a no-op.
        self.assertEqual((seen[2]["cleanup"]["clones"], seen[2]["cleanup"]["import_volume"], seen[3]),
                         ({"a": "refused_not_owned_identity"}, "absent", 0))
        self.assertEqual((second["accepted"], second["reason"]), (True, None))  # And the later run never noticed.
        self.assert_clean(second)

    def test_a_vmid_another_job_took_first_is_never_destroyed(self):
        winner = {"name": coldboot.NAME % (SHA[:8], "0" * 12, "a"), "tags": "proxcloud;pc-artifact-test;%s;pc-export-run-%s" % (TAG, "0" * 12)}
        for status in (500, 596):  # Refused, or the answer was lost: either way that VM carries another run's name.
            with self.subTest(status=status):
                self.setUp()

                def race(method, path, params, status=status):
                    if (method, path) == ("POST", QEMU) and 9290 not in self.pve.vms:
                        self.pve.add(9290, status="running", **winner)
                        raise coldboot.ApiError(status)
                self.pve.hook = race
                result = self.run_job()
                self.assertEqual((result["reason"], result["cleanup"]["clones"]), ("pve_http_%d" % status, {"a": "not_created"}))
                self.assertEqual((self.pve.touched(9290), self.pve.vms[9290]["status"]), ([], "running"))
                self.assertIs(result["protected_set"]["unchanged"], False)
        # The same race lost to a VM outside the pool, which the token cannot read: a refusal by PVE proves this
        # run created nothing; a lost answer proves nothing, and the VMID is reported instead of guessed at.
        for status, outcome in ((500, "not_created"), (596, "refused_not_owned_pool")):
            with self.subTest(invisible=status):
                self.setUp()

                def race(method, path, params, status=status):
                    if (method, path) == ("POST", QEMU) and 9290 not in self.pve.vms:
                        self.pve.add(9290, pool="customers", status="running", name="tenant-vm")
                        raise coldboot.ApiError(status)
                self.pve.hook = race
                result = self.run_job()
                self.assertEqual((result["reason"], result["cleanup"]["clones"]), ("pve_http_%d" % status, {"a": outcome}))
                self.assertEqual((self.pve.touched(9290), self.state()["clean"]), ([], status == 500))

    def test_a_clone_that_vanished_under_a_run_is_not_a_clean_run(self):
        self.host.on_guest = lambda address: address.endswith(".51") and self.pve.vms.pop(9290)  # Someone else destroyed it.
        result = self.run_job()
        self.assertEqual((result["accepted"], result["reason"]), (False, "cleanup_incomplete"))
        self.assertEqual(result["cleanup"]["clones"], {"a": "absent", "b": "destroyed"})
        again = coldboot.cleanup_only(self.pve, self.sdir)  # For the cleanup command nothing is left: that is its job.
        self.assertEqual((again["clean"], again["cleanup"]["clones"]), (True, {"a": "absent", "b": "destroyed"}))


class UploadOutcomeTests(ColdBootCase):
    """An upload whose outcome is not known is never reported absent."""

    def test_an_upload_task_that_outlives_the_deadline_is_waited_for_before_the_volume_counts_as_gone(self):
        with patch.object(coldboot, "TASK_DEADLINE", 0):
            result = self.run_job()
        self.assertEqual((result["reason"], result["cleanup"]["import_volume"]), ("task_deadline", "refused_task_deadline"))
        self.assertEqual((self.state()["import"], self.state()["clean"], self.pve.volumes), ("task", False, []))
        result = coldboot.cleanup_only(self.pve, self.sdir)  # The task has ended by now and its file landed.
        self.assertEqual((result["clean"], result["cleanup"]["import_volume"], self.pve.volumes), (True, "deleted", []))
        self.assertEqual([m[0] for m in self.pve.mutations], ["UPLOAD", "DELETE"])

    def test_an_upload_with_no_answer_is_unknown_until_it_lands_or_settles(self):
        for lands in (True, False):
            with self.subTest(lands=lands):
                self.setUp()
                store = self.pve.upload

                def lost(*a):
                    store(*a)  # The node has the whole body and works on it; only the answer is lost.
                    raise TimeoutError()
                self.pve.upload = lost
                result = self.run_job()
                self.assertEqual((result["reason"], result["cleanup"]["import_volume"], result["accepted"]),
                                 ("error_TimeoutError", "unknown", False))
                self.assertEqual((self.state()["import"], self.state()["clean"]), ("sending", False))
                again = coldboot.cleanup_only(self.pve, self.sdir)
                self.assertEqual((again["clean"], again["cleanup"]["import_volume"]), (False, "unknown"))
                if lands:
                    self.pve.volumes.append(self.volid())
                    again = coldboot.cleanup_only(self.pve, self.sdir)
                    self.assertEqual((again["clean"], again["cleanup"]["import_volume"], self.pve.volumes), (True, "deleted", []))
                else:
                    with patch.object(coldboot.time, "time", return_value=time.time() + coldboot.IMPORT_SETTLE):
                        again = coldboot.cleanup_only(self.pve, self.sdir)
                    self.assertEqual((again["clean"], again["cleanup"]["import_volume"]), (True, "absent"))

    def test_only_a_refusal_by_pve_or_a_body_this_job_cut_short_means_nothing_was_uploaded(self):
        other = "UPID:%s:00000001:00000001:00000002:imgcopy::root@pam:" % API_NODE
        cases = {"403": (coldboot.ApiError(403), "pve_http_403", "not_uploaded"),
                 "500": (coldboot.ApiError(500), "pve_http_500", "not_uploaded"),
                 "changed": (coldboot.Refused("upload_source_changed"), "upload_source_changed", "not_uploaded"),
                 "596": (coldboot.ApiError(596), "pve_http_596", "unknown"),
                 "cancelled": (coldboot.Refused("terminated"), "terminated", "unknown"),
                 "foreign task": (other, "task_id_unexpected", "unknown"), "no task": (None, "task_id_unexpected", "unknown")}
        for name, (answer, reason, outcome) in cases.items():
            with self.subTest(case=name):
                self.setUp()

                def upload(*a, answer=answer):
                    if isinstance(answer, Exception):
                        raise answer
                    return answer
                self.pve.upload = upload
                result = self.run_job()
                self.assertEqual((result["reason"], result["cleanup"]["import_volume"]), (reason, outcome))
                self.assertEqual((result["accepted"], self.state()["clean"]), (False, outcome == "not_uploaded"))


class InterruptedCleanupTests(ColdBootCase):
    """Cleanup is not single-shot and cannot be cancelled."""

    def test_one_failed_read_or_task_poll_does_not_strand_a_clone(self):
        cases = {"snapshot": (lambda m, p: p.endswith("/9290/snapshot"), coldboot.ApiError(596)),
                 "probe": (lambda m, p: p == "/cluster/nextid" and self.steps()[0] == ["started", "started"], coldboot.ApiError(596)),
                 "stop poll": (lambda m, p: "/tasks/" in p and ":qmstop:" in urllib.parse.unquote(p), coldboot.ApiError(596)),
                 "destroy poll": (lambda m, p: "/tasks/" in p and ":qmdestroy:" in urllib.parse.unquote(p), urllib.error.URLError("reset")),
                 "volume list": (lambda m, p: p == CONTENT and self.steps()[0] == ["destroyed", "destroyed"], coldboot.ApiError(596))}
        for name, (at, error) in cases.items():
            with self.subTest(case=name):
                self.setUp()
                fired = []

                def once(method, path, params, at=at, error=error):
                    if not fired and at(method, path):
                        fired.append(1)
                        raise error
                self.pve.hook = once
                result = self.run_job()
                self.assertEqual((fired, result["accepted"], result["reason"]), ([1], True, None))
                self.assert_clean(result)
                self.assertEqual(len([m for m in self.pve.mutations if m[1].endswith("/status/stop")]), 2)  # No task was repeated.
                self.assertEqual(len([m for m in self.pve.mutations if m[0] == "DELETE"]), 3)

    def test_a_signal_during_cleanup_is_ignored(self):
        for sig in (signal.SIGTERM, signal.SIGINT):
            with self.subTest(sig=sig):
                self.setUp()
                signal.signal(sig, coldboot._terminated)
                # The first signal of the job arrives in every phase of the cleanup, the key removal included.
                self.pve.hook = lambda m, p, q, sig=sig: (m != "GET" or "/snapshot" in p or p == "/cluster/resources") and os.kill(os.getpid(), sig)
                self.host.on_guest = lambda address: setattr(self, "armed", True)
                armed = self.pve.hook
                self.pve.hook = lambda m, p, q: getattr(self, "armed", False) and armed(m, p, q)
                self.armed = False
                result = self.run_job()
                self.assertEqual((result["accepted"], result["reason"]), (True, None))
                self.assert_clean(result)
                self.assertEqual(signal.getsignal(sig), signal.SIG_IGN)
                # No step was abandoned and redone by the second pass.
                self.assertEqual(len([c for c in self.pve.calls if c[1].endswith("/9290/snapshot")]), 1)

    def test_a_cancel_reaches_the_guarded_cleanup_through_main(self):
        argv = ["run", "--sanitize-run", str(self.root / "sanitize-run.json"), "--variant", "kubernetes", "--os", "ubuntu-2604",
                "--kubernetes-version", "1.37.2", "--node", NODE, "--storage", STORAGE, "--vlan", "242", "--gateway", "192.0.2.1",
                "--test-address", ADDRESSES[0], "--test-address", ADDRESSES[1], "--nameserver", "1.1.1.1"]
        env = {"PVE_URL": "https://pve.invalid:8006", "PVE_TOKEN_ID": TOKEN, "PVE_TOKEN_SECRET": "not-a-secret"}

        def unrouted(signum, frame):
            raise AssertionError("main() did not route the signal")
        for sig in (signal.SIGTERM, signal.SIGINT):
            with self.subTest(sig=sig):
                self.setUp()
                signal.signal(sig, unrouted)
                self.host.on_guest = lambda address, sig=sig: os.kill(os.getpid(), sig)
                self.pve.hook = lambda m, p, q, sig=sig: p.endswith("/status/stop") and os.kill(os.getpid(), sig)  # And again.
                with patch.dict(os.environ, env), patch.object(coldboot, "Client", lambda pve: self.pve), \
                        contextlib.redirect_stdout(io.StringIO()):
                    self.assertEqual(coldboot.main(argv + ["--state-dir", str(self.sdir)]), 1)
                result = json.loads((self.sdir / coldboot.RESULT).read_text())
                self.assertEqual((result["reason"], result["step"]), ("terminated", "guest_checks"))
                self.assert_clean(result)


class GuardUnitTests(ColdBootCase):
    def test_task_wait_accepts_only_the_expected_task_of_this_token_and_is_bounded(self):
        good = self.pve.task("qmstart", 9290)
        coldboot.wait(self.pve, NODE, good, "qmstart:9290")
        coldboot.wait(self.pve, NODE, None, "qmconfig:9290", allow_sync=True)
        coldboot.wait(self.pve, None, self.pve.task("imgcopy", node=API_NODE), coldboot.STORAGE_TASK)  # Wherever it runs.
        for upid, task, node in ((None, "qmstart:9290", NODE), ("", "qmstart:9290", NODE), (5, "qmstart:9290", NODE),
                                 (self.pve.task("qmstart", 9291), "qmstart:9290", NODE),
                                 (self.pve.task("qmdestroy", 9290), "qmstart:9290", NODE),
                                 (self.pve.task("qmstart", 9290, node="pve2"), "qmstart:9290", NODE),  # A VM task: this node only.
                                 (good + "x", "qmstart:9290", NODE), (good.replace(TOKEN, "root@pam"), "qmstart:9290", NODE),
                                 (good.replace(TOKEN, TOKEN + "x"), "qmstart:9290", NODE),
                                 (self.pve.task("imgcopy", node="pve/../x"), coldboot.STORAGE_TASK, None),
                                 (self.pve.task("imgcopy") + "\nUPID:x", coldboot.STORAGE_TASK, None)):
            with self.subTest(upid=upid), self.assertRaises(coldboot.Refused):
                coldboot.wait(self.pve, node, upid, task)
        running = types.SimpleNamespace(call=lambda *a, **k: {"status": "running"}, token_id=TOKEN)
        with patch.object(coldboot, "TASK_DEADLINE", 0), self.assertRaises(coldboot.Refused) as caught:
            coldboot.wait(running, NODE, good, "qmstart:9290")
        self.assertEqual(str(caught.exception), "task_deadline")

    def test_task_poll_retries_transport_errors_and_5xx_but_not_a_refusal(self):
        upid = self.pve.task("qmstop", 9290)
        answers = [coldboot.ApiError(596), urllib.error.URLError("reset"), {"status": "running"}, {"status": "stopped", "exitstatus": "OK"}]
        seen = []

        def call(method, path):
            seen.append(path)
            answer = answers[len(seen) - 1]
            if isinstance(answer, Exception):
                raise answer
            return answer
        api = types.SimpleNamespace(call=call, token_id=TOKEN)
        coldboot.wait(api, NODE, upid, "qmstop:9290")
        self.assertEqual((len(seen), set(seen)), (4, {"/nodes/%s/tasks/%s/status" % (NODE, urllib.parse.quote(upid, safe=""))}))
        answers[:] = [coldboot.ApiError(403)]
        del seen[:]
        with self.assertRaises(coldboot.ApiError):
            coldboot.wait(api, NODE, upid, "qmstop:9290")
        answers[:] = [{"status": "stopped", "exitstatus": "storage busy"}]
        del seen[:]
        with self.assertRaises(coldboot.Refused) as caught:
            coldboot.wait(api, NODE, upid, "qmstop:9290")
        self.assertEqual(str(caught.exception), "task_failed")

    def test_the_protected_set_records_node_type_status_and_digest_and_refuses_odd_rows(self):
        snapshot, configs = coldboot.protected(self.pve)
        self.assertEqual(snapshot, {"100": [NODE, "qemu", "stopped", self.pve.digest(100)],
                                    "9205": [NODE, "qemu", "running", self.pve.digest(9205)]})
        self.assertEqual(sorted(configs), [100, 9205])
        good = {"vmid": 100, "node": NODE, "type": "qemu", "status": "stopped"}
        for row, config in ((dict(good, vmid="100"), {"digest": "a" * 40}), (dict(good, type="storage"), {"digest": "a" * 40}),
                            (dict(good, node="a/b"), {"digest": "a" * 40}), (good, {}), (good, {"digest": "A" * 40})):
            api = types.SimpleNamespace(call=lambda m, path, row=row, config=config, **k: [row] if path == "/cluster/resources" else config)
            with self.subTest(row=row, config=config), self.assertRaises(coldboot.Refused):
                coldboot.protected(api)

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
        env = {"PVE_URL": "https://pve.invalid:8006", "PVE_TOKEN_ID": TOKEN, "PVE_TOKEN_SECRET": "not-a-secret"}
        out = io.StringIO()
        with patch.dict(os.environ, env), patch.object(coldboot, "Client", lambda pve: self.pve), contextlib.redirect_stdout(out):
            self.assertEqual(coldboot.main(argv), 0)
            self.assertEqual(json.loads(out.getvalue()), {"kind": "ColdBootAcceptance", "ok": True, "reason": None})
            result = self.sdir / coldboot.RESULT
            self.assertEqual((json.loads(result.read_text())["accepted"], stat_mode(result)), (True, 0o600))
            calls = len(self.pve.calls)
            for leftover in (coldboot.STATE, coldboot.RESULT):  # Either file of the first run refuses a second one.
                moved = self.sdir / leftover
                moved.rename(self.root / leftover)
                with contextlib.redirect_stderr(io.StringIO()) as err:
                    self.assertEqual(coldboot.main(argv), 2)
                self.assertEqual(err.getvalue(), "coldboot refused: state_exists\n")
                (self.root / leftover).rename(moved)
            self.assertEqual((json.loads(result.read_text())["accepted"], len(self.pve.calls)), (True, calls))  # Not replaced.
            self.assertEqual(coldboot.main(["cleanup", "--state-dir", str(self.sdir)]), 0)
            cleaned = json.loads((self.sdir / "coldboot-cleanup-result.json").read_text())
            self.assertEqual((cleaned["clean"], cleaned["reason"]), (True, "already_clean"))
            with contextlib.redirect_stderr(io.StringIO()) as err:
                self.assertEqual(coldboot.main(argv[:-1] + [str(self.root / "absent")]), 2)
            self.assertEqual(err.getvalue(), "coldboot refused: state_dir_refused\n")
        with self.assertRaises(coldboot.Refused):  # A cancelled job raises into run()'s guarded cleanup.
            coldboot._terminated(signal.SIGTERM, None)
        self.assertEqual(signal.getsignal(signal.SIGTERM), signal.SIG_IGN)


class PVEHandler(http.server.BaseHTTPRequestHandler):
    """The fake PVE behind the real HTTP API shape: /api2/json, the token header, query or form parameters."""

    def serve(self):
        pve, url = self.server.pve, urllib.parse.urlsplit(self.path)
        path = url.path.removeprefix("/api2/json")
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        status, data = 200, None
        try:
            assert self.headers["Authorization"] == AUTH and url.path.startswith("/api2/json/")
            if path.endswith("/upload"):
                boundary = self.headers["Content-Type"].split("boundary=")[1].encode()
                parts = body.split(b"--" + boundary)[1:-1]
                fields = {re.search(rb'name="([a-z-]+)"', p).group(1).decode(): p.split(b"\r\n\r\n", 1)[1][:-2] for p in parts}
                assert fields["content"] == b"import" and fields["checksum-algorithm"] == b"sha256"
                assert hashlib.sha256(fields["filename"]).hexdigest() == fields["checksum"].decode()
                _, _, node, _, storage, _ = path.split("/")
                data = pve.store(node, storage, re.search(rb'filename="([^"]+)"', parts[-1]).group(1).decode())
            else:
                raw = url.query if self.command in ("GET", "DELETE") else body.decode()
                assert self.command in ("GET", "DELETE") or not url.query
                data = pve.call(self.command, path, **{k: int(v) if v.isdigit() else v
                                                       for k, v in urllib.parse.parse_qsl(raw, keep_blank_values=True)})
        except coldboot.ApiError as exc:
            status = exc.status
        except Exception as exc:  # noqa: BLE001 - a broken request is a test failure, reported by the test
            self.server.errors.append(repr(exc))
            status = 599
        reply = json.dumps({"data": data}).encode()
        self.send_response(status)
        self.send_header("Content-Length", str(len(reply)))
        self.end_headers()
        self.wfile.write(reply)

    do_GET = do_POST = do_PUT = do_DELETE = serve

    def log_message(self, *args):
        pass


class OverHTTPTests(ColdBootCase):
    def test_the_whole_run_through_the_real_client_and_a_fake_pve_http_server(self):
        server = http.server.HTTPServer(("127.0.0.1", 0), PVEHandler)
        server.pve, server.errors = self.pve, []
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        client = coldboot.Client(types.SimpleNamespace(base="http://127.0.0.1:%d/api2/json" % server.server_port,
                                                       auth=AUTH, ctx=ssl._create_unverified_context()))
        self.assertEqual(client.token_id, TOKEN)
        result = coldboot.run(client, coldboot.settings(self.args()), self.sdir)
        self.assertEqual((server.errors, result["accepted"], result["reason"]), ([], True, None))
        self.assert_clean(result)
        self.assertEqual(self.pve.uploads, [result["import_volume"]])
        self.assertEqual([m[1].rsplit("/", 1)[1] for m in self.pve.mutations if "/qemu/9291" in m[1]],
                         ["config", "config", "resize", "config", "start", "stop", "9291"])
        self.assertEqual(coldboot.cleanup_only(client, self.sdir)["reason"], "already_clean")


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
        elif "huge" in self.path:
            self.reply(200, {"data": {"pad": "x" * int(self.path.rsplit("=", 1)[1])}})
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
                                    auth=AUTH, ctx=ssl._create_unverified_context())
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
        self.assertEqual(headers["Authorization"], AUTH)
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
        self.assertEqual(self.client.token_id, TOKEN)  # Tasks are pinned to this token.

    def test_a_response_is_read_up_to_a_bound(self):
        with patch.object(coldboot, "MAX_RESPONSE", 4096):
            self.assertEqual(len(self.client.call("GET", "/huge", pad=4000)["pad"]), 4000)
            with self.assertRaises(coldboot.Refused) as caught:
                self.client.call("GET", "/huge", pad=5000)
        self.assertEqual(str(caught.exception), "pve_response_too_large")


class FakeGuest:
    """The files and tools coldboot_guest reads inside a clone; nothing here touches the real host."""
    KEY = "AAAAC3NzaC1lZDI1NTE5AAAAIJobKey"

    def __init__(self, variant="kubernetes"):
        self.variant, self.snap, self.snap_rc = variant, True, 0
        self.groups = {"builder": "1000(builder)", "packer": "1002(packer)"}
        self.sudo = "User %s is not allowed to run sudo on clone."
        self.units = {"qemu-guest-agent": "active", "snapd": "active", "containerd": "active", "kubelet": "enabled"}
        self.tools = {"/usr/bin/kubeadm": "v1.37.2\n", "/usr/bin/kubelet": "Kubernetes v1.37.2",
                      "/usr/bin/kubectl": json.dumps({"clientVersion": {"gitVersion": "v1.37.2"}}),
                      "/usr/local/bin/crictl": "RuntimeName:  containerd\n",
                      "/usr/bin/apt-cache": "cloud-init:\n  Installed: 1\nqemu-guest-agent:\n  Installed: 1\nopenssh-server:\n  Installed: 1\n"}
        self.rc = {}
        self.serial, self.serial_late = "serial: 7c1f-serial\ndevice-key-sha3-384: devkeyhash\n", 0
        self.changes = "ID Status Spawn Ready Summary\n1 Done today today Initialize system state\n"
        self.cloud = {"status": "done", "errors": [], "extended_status": "done",
                      "init": {"errors": [], "recoverable_errors": {"WARNING": ["w1", "w2"]}}}
        self.blocks = [{"name": "sr0", "type": "rom", "size": 4 << 20},
                       {"name": "sda", "type": "disk", "size": 30 << 30, "children": [
                           {"name": "sda1", "type": "part", "size": (30 << 30) - (1 << 20), "mountpoints": ["/"]}]}]
        self.root_blocks = 29 << 18
        self.files = {"/etc/os-release": 'ID=ubuntu\nVERSION_ID="26.04"\n', "/etc/machine-id": "0123456789abcdef0123456789abcdef\n",
                      "/etc/passwd": "root:x:0:0:root:/root:/bin/bash\nubuntu:x:1001:1001::/home/ubuntu:/bin/bash\n"
                                     "builder:x:1000:1000::/home/builder:/bin/bash\n",
                      "/etc/shadow": "root:*:1:0:99999:7:::\nubuntu:!:1:0:99999:7:::\nbuilder:!:1:0:99999:7:::\n",
                      "/home/ubuntu/.ssh/authorized_keys": "ssh-ed25519 %s proxcloud-export-coldboot\n" % self.KEY,
                      "/etc/ssh/ssh_host_ed25519_key.pub": "ssh-ed25519 AAAAhostkey root@clone\n",
                      "/proc/swaps": "Filename Type Size Used Priority\n"}
        self.dirs = {"/var/lib/snapd/device/private-keys-v1": ["newkey"]}

    def run(self, args, timeout=60):
        out, code = self.tools.get(args[0], ""), self.rc.get(args[0], 0)
        if args[:2] == ["/usr/bin/cloud-init", "status"]:
            out = json.dumps(self.cloud)
        elif args[0] == "/usr/bin/id":
            out = "uid=1000(%s) gid=1000(%s) groups=%s" % (args[1], args[1], self.groups[args[1]])
        elif args[0] == "/usr/bin/sudo":
            out = self.sudo % args[-1] if "%s" in self.sudo else self.sudo
        elif args[:2] == ["/usr/bin/snap", "version"]:
            code = self.snap_rc
        elif args[:2] == ["/usr/bin/snap", "model"]:
            out, code = self.serial, 0 if self.serial else 1
            if self.serial_late:  # snapd has not got its new serial yet.
                self.serial_late, out, code = self.serial_late - 1, "", 1
        elif args[:2] == ["/usr/bin/snap", "changes"]:
            out = self.changes
        elif args[0] == "/usr/bin/systemctl":
            out = self.units[args[2]]
        elif args[0] == "/usr/bin/lsblk":
            out = json.dumps({"blockdevices": self.blocks})
        return types.SimpleNamespace(stdout=out, stderr="", returncode=code)

    def record(self):
        path = coldboot_guest.pathlib.Path
        out = io.StringIO()
        with patch.object(coldboot_guest, "run", self.run), patch.object(coldboot_guest, "SERIAL_WAIT", 60 if self.serial_late else 0), \
                patch.object(coldboot_guest.time, "sleep"), \
                patch.object(path, "read_text", lambda p, *a, **k: self.files[str(p)]), \
                patch.object(path, "is_file", lambda p: str(p) in self.files), \
                patch.object(path, "exists", lambda p, **k: str(p) in self.files or str(p) in self.dirs), \
                patch.object(path, "is_dir", lambda p: str(p) in self.dirs), \
                patch.object(path, "iterdir", lambda p: iter(self.dirs[str(p)])), \
                patch.object(coldboot_guest.os.path, "exists", lambda p: self.snap and p == "/usr/bin/snap"), \
                patch.object(coldboot_guest.os, "statvfs", return_value=types.SimpleNamespace(f_blocks=self.root_blocks, f_frsize=4096)), \
                contextlib.redirect_stdout(out):
            coldboot_guest.main([self.variant, "26.04", "1.37.2" if self.variant == "kubernetes" else "-", sha(self.KEY)])
        return coldboot.guest_record(out.getvalue().encode(), self.variant)  # What the guest prints passes the parent gate.


class GuestScriptTests(unittest.TestCase):
    def test_guest_checks_pass_on_a_clean_clone_and_report_hashes_only(self):
        row = FakeGuest().record()
        self.assertEqual(row["checks"], dict.fromkeys(coldboot_guest.COMMON + coldboot_guest.K8S, True))
        self.assertEqual((row["machine_id_sha256"], row["host_key_sha256"], row["snapd_serial_sha256"], row["snapd_device_key_sha256"]),
                         (sha("0123456789abcdef0123456789abcdef"), sha("AAAAhostkey"), sha("7c1f-serial"), sha("devkeyhash")))
        self.assertEqual((row["snapd_present"], row["snapd_changes_error_lines"], row["cloud_init_recoverable_warnings"]), (True, 0, 2))
        late = FakeGuest()  # snapd asks for its serial after boot: the check waits for it. A comment is not a key.
        late.serial_late = 2
        late.files["/home/ubuntu/.ssh/authorized_keys"] = "# cloud-init\n" + late.files["/home/ubuntu/.ssh/authorized_keys"]
        self.assertEqual((late.record(), late.serial_late), (row, 0))
        huge = types.SimpleNamespace(stdout="x" * 262145, stderr="", returncode=0)
        with patch.object(coldboot_guest.subprocess, "run", return_value=huge), self.assertRaises(RuntimeError):
            coldboot_guest.run(["/usr/bin/true"])  # A tool's output is bounded.
        packer = FakeGuest()  # A second, equally unusable build account, and sudo's other wording of "no".
        packer.files["/etc/passwd"] += "packer:x:1002:1002::/home/packer:/bin/bash\n"
        packer.files["/etc/shadow"] += "packer:!:1:0:99999:7:::\n"
        packer.sudo = "Sorry, user %s may not run sudo on clone."
        self.assertEqual(packer.record()["checks"], row["checks"])
        native = FakeGuest("os")
        native.files["/etc/passwd"] = native.files["/etc/passwd"].replace("builder:x:1000:1000::/home/builder:/bin/bash\n", "")
        native.snap = False  # No snapd and no build account: nothing to fail in the guest (the parent still wants snapd).
        row = native.record()
        self.assertEqual(row["checks"], dict.fromkeys(coldboot_guest.COMMON + coldboot_guest.OS_ONLY, True))
        self.assertEqual((row["snapd_present"], row["snapd_serial_sha256"], row["snapd_device_key_sha256"]), (False, None, None))

    def test_each_guest_check_fails_on_its_own_defect(self):
        def edit(path, old, new):
            return lambda g: g.files.update({path: g.files[path].replace(old, new)})

        def tool(name, out=None, rc=0):
            return lambda g: (g.tools.update({name: g.tools[name] if out is None else out}), g.rc.update({name: rc}))

        def packer(shadow="!", groups="1002(packer)"):
            return lambda g: (edit("/etc/passwd", "ubuntu:x", "packer:x:1002:1002::/home/packer:/bin/bash\nubuntu:x")(g),
                              g.files.update({"/etc/shadow": g.files["/etc/shadow"] + "packer:%s:1:0:99999:7:::\n" % shadow}),
                              g.groups.update(packer=groups))
        keys = "/home/ubuntu/.ssh/authorized_keys"
        defects = {
            "os_release": [edit("/etc/os-release", "26.04", "24.04"), edit("/etc/os-release", "ID=ubuntu", "ID=debian")],
            "cloud_init_done_no_errors": [lambda g: g.cloud.update(status="error"), lambda g: g.cloud.update(errors=["boom"]),
                                          lambda g: g.cloud["init"].update(errors=["boom"]),
                                          lambda g: g.cloud.update({"modules-final": {"errors": ["boom"]}})],
            "default_user_exactly_job_key": [
                lambda g: g.files.update({keys: g.files[keys] + "ssh-ed25519 AAAAbuildkey packer\n"}),
                lambda g: g.files.update({keys: "ssh-ed25519 AAAAsomeoneelse x\n"}),  # One key, not the job's.
                edit(keys, "ssh-ed25519", "ssh-rsa"), lambda g: g.files.pop(keys)],
            "build_user_only_own_group": [lambda g: g.groups.update(builder="1000(builder),27(sudo)"),
                                          packer(groups="1002(packer),27(sudo)")],
            "build_user_no_sudo": [
                lambda g: setattr(g, "sudo", "User builder may run the following commands on clone:\n    /usr/bin/true"),
                lambda g: setattr(g, "sudo", "User builder is not allowed to run sudo.\nUser builder may run:\n    (ALL : ALL) ALL")],
            "build_user_locked": [edit("/etc/shadow", "builder:!:", "builder:$y$j9T$hash:"), packer(shadow="$y$j9T$hash")],
            "build_user_no_keys": [lambda g: g.files.update({"/home/builder/.ssh/authorized_keys": "ssh-ed25519 AAAA b\n"})],
            "machine_id_valid": [edit("/etc/machine-id", "0123456789abcdef0123456789abcdef", "")],
            "qemu_guest_agent_active": [lambda g: g.units.update({"qemu-guest-agent": "inactive"})],
            "snapd_active": [lambda g: g.units.update(snapd="failed"), lambda g: setattr(g, "snap_rc", 1)],
            "snapd_reregistered_new_key": [lambda g: setattr(g, "serial", ""), lambda g: setattr(g, "serial", "serial: 7c1f-serial\n"),
                                           lambda g: setattr(g, "serial", "device-key-sha3-384: devkeyhash\n"),
                                           lambda g: g.dirs.update({"/var/lib/snapd/device/private-keys-v1": []}),
                                           lambda g: g.dirs.clear()],
            "snapd_no_change_errors": [lambda g: setattr(g, "changes", g.changes + "2 Error today today Refresh snap\n")],
            "no_active_swap": [edit("/proc/swaps", "Priority\n", "Priority\n/swap.img file 2097148 0 -2\n")],
            "disk_30g": [lambda g: g.blocks[1].update(size=20 << 30)],
            "root_grown": [lambda g: setattr(g, "root_blocks", 19 << 18)],
            "builder_account_retained": [edit("/etc/passwd", "builder:x:1000:1000::/home/builder:/bin/bash\n", "")],
            "kubelet_enabled": [lambda g: g.units.update(kubelet="disabled")],
            "containerd_active": [lambda g: g.units.update(containerd="inactive")],
            "k8s_binaries_exact": [tool("/usr/bin/kubelet", "Kubernetes v1.37.20"), tool("/usr/bin/kubeadm", "v1.37.1\n"),
                                   tool("/usr/bin/kubectl", json.dumps({"clientVersion": {"gitVersion": "v1.36.6"}})),
                                   tool("/usr/bin/kubectl", rc=1)],
            "crictl_talks_to_containerd": [tool("/usr/local/bin/crictl", "RuntimeName:  cri-o\n"), tool("/usr/local/bin/crictl", rc=1)],
        }
        self.assertEqual(set(defects), set(coldboot_guest.COMMON + coldboot_guest.K8S))  # Every check has a failing case.
        os_defects = {
            "apt_metadata_available": [tool("/usr/bin/apt-cache", rc=100),
                                       tool("/usr/bin/apt-cache", "cloud-init:\nopenssh-server:\n")],
            "root_partition_grown": [lambda g: g.blocks[1]["children"][0].update(size=20 << 30),
                                     lambda g: g.blocks[1]["children"].append({"name": "sda2", "size": 1, "mountpoints": ["/"]}),
                                     lambda g: g.blocks[1]["children"][0].update(mountpoints=["/boot"])],
        }
        self.assertEqual(set(os_defects), set(coldboot_guest.OS_ONLY))
        for variant, table in (("kubernetes", defects), ("os", os_defects)):
            for check, cases in table.items():
                for index, defect in enumerate(cases):
                    with self.subTest(check=check, case=index):
                        guest = FakeGuest(variant)
                        defect(guest)
                        row = guest.record()
                        self.assertEqual({name for name, ok in row["checks"].items() if not ok}, {check})
        guest = FakeGuest()
        guest.changes += "2 Error today today Refresh snap\n3 Error today today Install\n"
        self.assertEqual(guest.record()["snapd_changes_error_lines"], 2)

    def test_guest_script_is_standalone_stdlib_and_names_every_check(self):
        source = Path(coldboot_guest.__file__).read_text()
        imports = set(re.findall(r"(?m)^import ([a-z]+)$", source))
        self.assertEqual(imports, {"hashlib", "json", "os", "pathlib", "re", "subprocess", "sys", "time"})
        self.assertNotIn("from ", source.split('"""', 2)[2].split("def ", 1)[0])
        produced = set(re.findall(r'(?m)^\s+"([a-z0-9_]+)": ', source.split("checks = {", 1)[1].split("print(", 1)[0]))
        self.assertEqual(produced, set(coldboot_guest.COMMON + coldboot_guest.K8S + coldboot_guest.OS_ONLY))
        self.assertEqual(coldboot.GUEST_KEYS, set(re.findall(r'"([a-z0-9_]+)": ', source.split("print(json.dumps({", 1)[1])) | {"checks"})
        for word in ("write_text", "write_bytes", "unlink", "open("):  # Read-only in the guest.
            self.assertNotIn(word, source)


if __name__ == "__main__":
    unittest.main()
