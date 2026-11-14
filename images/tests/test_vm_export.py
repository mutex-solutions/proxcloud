"""Pure tests for images/vm_export: no docker, guestfs, network or PVE. Fakes only."""
import copy as copy_module
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

IMAGES = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(IMAGES))
from vm_export import bundle, capsule, copy as vcopy, inspect_k8s, inspect_os, postscan_probe, review, stream_sanitized  # noqa: E402,F401
from vm_export.sanitize import StreamSink  # noqa: E402
import vm_artifact  # noqa: E402
import vm_sanitize  # noqa: E402

SPEC = importlib.util.spec_from_file_location("test_vm_sanitize_fakes", IMAGES / "tests" / "test_vm_sanitize.py")
fakes = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(fakes)
BASELINE = json.loads((IMAGES / "vm_export" / "baselines" / "ubuntu-2604-k8s.json").read_text())
SOURCE_SHA = "1749277d590172e38e1928922daa53908ab3a88b860aa1aa3f1cc7cd02bed492"


def capsule_spec(**changes):
    args = dict(check="k8s-inventory-1.37.2", binds={"/srv/a/source.raw": "/input/source.raw",
                                                      "/repo/images/vm_sanitize.py": "/bootstrap/vm_sanitize.py"},
                scratch="/runner/tmp/x-scratch", argv=["/usr/bin/python3", "-I", "/bootstrap/inspect_k8s.py"])
    args.update(changes)
    return capsule.spec(**args)


def inspected(s):
    """What `docker inspect` reports for a container created from create_argv(s)."""
    return {"Image": s["image"], "Config": {"User": "1000:1000", "Entrypoint": ["/usr/bin/env"],
                                            "Cmd": ["-i", *capsule.ENV, *s["argv"]]},
            "HostConfig": {"NetworkMode": "none", "ReadonlyRootfs": True, "Privileged": False,
                           "LogConfig": {"Type": "none"}, "PidMode": "", "IpcMode": "private", "DeviceRequests": None,
                           "Devices": [], "CapAdd": None, "CapDrop": ["ALL"], "SecurityOpt": ["no-new-privileges"],
                           "Memory": capsule.size_bytes(s["memory"]), "MemorySwap": capsule.size_bytes(s["memory"]),
                           "NanoCpus": s["cpus"] * 10 ** 9, "PidsLimit": s["pids"], "ShmSize": 128 << 20,
                           "PortBindings": {}, "Binds": None, "VolumesFrom": None, "Links": None,
                           "Tmpfs": {"/tmp": "rw,exec,nosuid,nodev,size=1g,mode=0700,uid=1000,gid=1000"},
                           "Ulimits": [{"Name": "core", "Soft": 0, "Hard": 0}, {"Name": "nofile", "Soft": 256, "Hard": 256},
                                       {"Name": "fsize", "Soft": s["fsize"], "Hard": s["fsize"]},
                                       {"Name": "cpu", "Soft": s["cpu_seconds"], "Hard": s["cpu_seconds"]}]},
            "Mounts": [{"Type": "bind", "Source": h, "Destination": g, "RW": False} for h, g in s["binds"].items()] +
                      [{"Type": "bind", "Source": s["scratch"], "Destination": "/scratch", "RW": True}]}


class CapsuleTests(unittest.TestCase):
    def test_spec_refuses_unpinned_image_odd_paths_and_limits(self):
        for changes in ({"image": "proxcloud-vm-inspection-tools:latest"}, {"check": "../x"},
                        {"binds": {"/a": "/etc/shadow"}}, {"binds": {"/a": "/input/x", "/b": "/input/x"}},
                        {"scratch": "relative"}, {"argv": []}, {"memory": "8G"}, {"pids": 0}):
            with self.subTest(changes=changes), self.assertRaises(capsule.CapsuleError):
                capsule_spec(**changes)

    def test_create_argv_is_confined_and_binds_are_readonly(self):
        s = capsule_spec()
        argv = capsule.create_argv(s, "n")
        for flag in ("--pull=never", "--network=none", "--read-only", "--user=1000:1000", "--cap-drop=ALL",
                     "--security-opt=no-new-privileges", "--memory=8g", "--memory-swap=8g", "--log-driver=none"):
            self.assertIn(flag, argv)
        mounts = [argv[i + 1] for i, a in enumerate(argv) if a == "--mount"]
        self.assertEqual(mounts[0], "type=bind,src=/runner/tmp/x-scratch,dst=/scratch")
        self.assertTrue(all(m.endswith(",readonly") for m in mounts[1:]))
        self.assertEqual(argv[argv.index(s["image"]) + 1:argv.index(s["image"]) + 2], ["-i"])
        self.assertNotIn("--privileged", argv)

    def test_confinement_inspect_accepts_exact_and_refuses_any_drift(self):
        s = capsule_spec()
        capsule.check_confinement(inspected(s), s)
        drifts = (lambda i: i["HostConfig"].update(NetworkMode="bridge"),
                  lambda i: i["HostConfig"].update(Privileged=True),
                  lambda i: i["HostConfig"].update(CapAdd=["SYS_ADMIN"]),
                  lambda i: i["HostConfig"].update(Memory=32 << 30),
                  lambda i: i["HostConfig"].update(Devices=[{"PathOnHost": "/dev/kvm"}]),
                  lambda i: i["HostConfig"]["Tmpfs"].update({"/scratch": "rw"}),
                  lambda i: i["HostConfig"]["Ulimits"].pop(),
                  lambda i: i["Mounts"].append({"Type": "bind", "Source": "/var/run/docker.sock",
                                                "Destination": "/input/d", "RW": False}),
                  lambda i: i["Mounts"][0].update(RW=True),
                  lambda i: i["Mounts"][-1].update(Type="volume"),
                  lambda i: i["Config"].update(User="0:0"),
                  lambda i: i["Config"].update(Cmd=["-i", "sh"]),
                  lambda i: i.update(Image="sha256:" + "0" * 64))
        for drift in drifts:
            info = inspected(s)
            drift(info)
            with self.assertRaises(capsule.CapsuleError):
                capsule.check_confinement(info, s)

    def test_json_sink_is_bounded(self):
        sink = capsule.JsonSink(limit=10)
        self.assertIsNone(sink(b"12345"))
        self.assertEqual(sink(b"678901"), "parent_output_limit")

    def test_schema_gate(self):
        term = {"exit_code": 0, "attach_exit_code": 0, "stop_reason": None, "known_cgroup": "/system.slice/x"}
        ok = lambda row: row.get("phase") == "p"  # noqa: E731
        self.assertEqual(capsule.gate_json(term, b'{"phase": "p"}\n', ok), (True, {"phase": "p"}))
        refused = {"phase": "inspection_refused", "exit_code": 0, "raw_output_retained": False}
        for data in (b'{"phase": "q"}\n', b'{"phase": "p"}\n{"phase": "p"}\n', b'{"phase": "p", "x": "10.200.1.2"}\n',
                     b'{"phase": "p", "k": "-----BEGIN PRIVATE KEY"}\n', b"not json\n", b"\xff\n", b"[]\n"):
            self.assertEqual(capsule.gate_json(term, data, ok), (False, refused))
        for bad in ({"stop_reason": "parent_wall_limit"}, {"known_cgroup": ""}, {"attach_exit_code": 1}):
            self.assertFalse(capsule.gate_json(dict(term, **bad), b'{"phase": "p"}\n', ok)[0])
        err = {"phase": "probe_error", "error_class": "ValueError", "frames": [["vm_sanitize.py", 144, "require"]]}
        t3 = dict(term, exit_code=3, attach_exit_code=3)
        self.assertEqual(capsule.gate_json(t3, json.dumps(err).encode(), ok), (False, err))
        err["frames"] = [["vm_sanitize.py", 1, "sanitization refused: /etc/shadow"]]
        self.assertEqual(capsule.gate_json(t3, json.dumps(err).encode(), ok)[1]["phase"], "inspection_refused")
        lay = {"phase": "layout_refused", "layout": {"roots": ["/dev/sda1"], "mountpoints": {"/": "<by-uuid>"}}}
        t4 = dict(term, exit_code=4, attach_exit_code=4)
        self.assertEqual(capsule.gate_json(t4, json.dumps(lay).encode(), ok), (False, lay))

    def test_stream_sink_header_then_exact_disk(self):
        disk = b"QFI\xfb" + bytes(100)
        facts = {"kind": "OfflineSanitizationFacts", "os": "ubuntu-2604", "variant": "kubernetes",
                 "kubernetes_version": "1.37.2", "disk_sha256": hashlib.sha256(disk).hexdigest(),
                 "disk_bytes": len(disk), "virtual_size_bytes": 20 << 30,
                 "checks": {k: True for k in stream_sanitized.CHECKS}}
        header = json.dumps(facts).encode() + b"\n"
        spool = io.BytesIO()
        sink = StreamSink(spool, "ubuntu-2604", "1.37.2")
        self.assertIsNone(sink(header[:10]))
        self.assertIsNone(sink(header[10:] + disk[:50]))
        self.assertIsNone(sink(disk[50:]))
        self.assertEqual((spool.getvalue(), sink.hashed.hexdigest()), (disk, facts["disk_sha256"]))
        self.assertEqual(sink(b"x"), "binary_output_limit")
        self.assertEqual(StreamSink(io.BytesIO(), "ubuntu-2604", "1.36.6")(header), "fixed_header_refused")
        self.assertEqual(StreamSink(io.BytesIO(), "ubuntu-2604", None)(header), "fixed_header_refused")
        self.assertEqual(StreamSink(io.BytesIO(), "ubuntu-2604", "1.37.2")(b"a" * 9000), "fixed_header_limit")
        self.assertEqual(stream_sanitized.parse(["--k8s-os", "ubuntu-2604", "1.37.2", "1680"]),
                         ("--k8s-os", "ubuntu-2604", "1.37.2", 1680))
        for argv in (["--os", "alpine", "1"], ["--k8s-os", "ubuntu-2404", "1.37.2", "1"], ["--os", "debian-13", "x"]):
            with self.assertRaises(ValueError):
                stream_sanitized.parse(argv)


def reviewed_inventory(version="1.37.2", cni="1.9.1-1.1"):
    inv = copy_module.deepcopy(BASELINE["inventory"])
    inv["facts"]["k8s"]["version"] = version
    inv["facts"]["k8s"]["kubernetes_packages"] = {name: version for name in review.K8S_PACKAGES}
    inv["packages"].update({name: version + "-1.1" for name in review.K8S_PACKAGES}, **{"kubernetes-cni": cni})
    return inv


class ReviewTests(unittest.TestCase):
    def test_derives_the_reviewed_1_37_2_bytes(self):
        r, policy = review.derive(reviewed_inventory(), BASELINE, "1.37.2", SOURCE_SHA)
        self.assertEqual(r["package_policy_sha256"], "4b94cebe6c9d9621364a963a1f6a689310132adeb5c3efb5e2615598e2cbbae2")
        # The exact bytes of the run-1 k8s-review.json that the private sanitize run pinned.
        self.assertEqual(hashlib.sha256(review.compact(r).encode()).hexdigest(),
                         "d6b090ada68934214c82349cc3572a0c765a2880ec2f15de9b20ab40f95c042f")
        vm_sanitize.load_review(review.compact(r).encode())  # Exactly the sanitizer's review schema.
        self.assertEqual(policy["held"], sorted(policy["held"]))

    def test_only_allowlisted_version_fields_may_differ(self):
        r, _ = review.derive(reviewed_inventory("1.36.6", "1.8.0-1.1"), BASELINE, "1.36.6", SOURCE_SHA)
        self.assertEqual(r["kubernetes_version"], "1.36.6")
        cases = {("1.38.0", "1.9.1-1.1", "1.38.0"): "kubernetes_version",
                 ("1.37.2", "1.9.2-1.1", "1.37.2"): "packages.kubernetes-cni",
                 ("1.36.6", "1.9.1-1.1", "1.37.2"): "facts.k8s.version"}
        for (inv_version, cni, wanted), field in cases.items():
            with self.subTest(field=field), self.assertRaises(review.ReviewRefused) as caught:
                review.derive(reviewed_inventory(inv_version, cni), BASELINE, wanted, SOURCE_SHA)
            self.assertIn(field, caught.exception.fields)
        inv = reviewed_inventory()
        inv["packages"]["kubelet"] = "1.37.2-2.1"
        with self.assertRaises(review.ReviewRefused) as caught:
            review.derive(inv, BASELINE, "1.37.2", SOURCE_SHA)
        self.assertEqual(caught.exception.fields, ["packages.kubelet"])

    def test_any_other_diff_refuses_with_field_paths_not_values(self):
        mutations = {"facts.accounts": lambda i: i["facts"]["accounts"][1].update(shell="/bin/bash"),
                     "runtime.containerd_config_sha256": lambda i: i["runtime"].update(containerd_config_sha256="f" * 64),
                     "package_status_word.snapd": lambda i: i["package_status_word"].update(snapd="install"),
                     "packages.snapd": lambda i: i["packages"].update(snapd="2.79+secret-value"),
                     "sudoers.sudoers_sha256": lambda i: i["sudoers"].update(sudoers_sha256="a" * 64),
                     "facts.cloud_dropins": lambda i: i["facts"]["cloud_dropins"].pop(),
                     "extra": lambda i: i.update(extra=1)}
        for field, mutate in mutations.items():
            inv = reviewed_inventory()
            mutate(inv)
            with self.subTest(field=field), self.assertRaises(review.ReviewRefused) as caught:
                review.derive(inv, BASELINE, "1.37.2", SOURCE_SHA)
            self.assertIn(field, caught.exception.fields)
            self.assertNotIn("secret-value", str(caught.exception))
        for inv, sha in ((dict(reviewed_inventory(), policy_result="REFUSED"), SOURCE_SHA),
                         (reviewed_inventory(), "not-a-hash"), ([], SOURCE_SHA)):
            with self.assertRaises(review.ReviewRefused):
                review.derive(inv, BASELINE, "1.37.2", sha)
        broken = dict(BASELINE, review=dict(BASELINE["review"], reviewed_dropins=BASELINE["review"]["reviewed_dropins"][:-1]))
        with self.assertRaises(review.ReviewRefused) as caught:
            review.derive(reviewed_inventory(), broken, "1.37.2", SOURCE_SHA)
        self.assertEqual(caught.exception.fields, ["review.reviewed_dropins"])

    def test_project_drops_only_version_fields(self):
        inv = reviewed_inventory()
        self.assertEqual(review.project(inv), BASELINE["inventory"])
        self.assertEqual(review.diff({"a": {"b": [1, 2]}}, {"a": {"b": [2, 1]}}), ["a.b"])
        self.assertEqual(review.diff({"a": 1}, {"a": True}), ["a"])


class FakePVE:
    def __init__(self, **changes):
        self.config = {"template": 1, "name": "pc-ubuntu-2604-kube-v1.37.2", "tags": "pc-public;pc-img-x",
                       "digest": "de0de6c1788a246a5b5cf03433e47d9216a425ee"}
        self.status, self.tasks = {"status": "stopped"}, []
        self.resources = [{"vmid": 105, "pool": "pc-templates", "template": 1}, {"vmid": 106, "pool": "other"}]
        for key, value in changes.items():
            setattr(self, key, value)

    def get(self, path):
        return {"config": self.config, "current": self.status, "tasks?vmid=105&source=active": self.tasks,
                "resources?type=vm": self.resources}[path.rsplit("/", 1)[1]]


class CopyTests(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.root = Path(self.dir.name)
        self.src = self.root / "base-105-disk-1.raw"
        self.data = b"boot" + bytes(3 << 20) + b"tail"
        self.src.write_bytes(self.data)

    def tearDown(self):
        self.dir.cleanup()

    def test_sparse_copy_double_hash_and_sealed_mode(self):
        out = self.root / "source.raw"
        r = vcopy.copy(str(self.src), len(self.data), str(out))
        self.assertEqual(r["raw_sha256"], hashlib.sha256(self.data).hexdigest())
        self.assertEqual((out.read_bytes(), out.stat().st_mode & 0o777), (self.data, 0o400))
        self.assertLess(r["local_dense_written_bytes"], len(self.data))

    def test_guards_refuse_and_leave_no_output(self):
        out = self.root / "source.raw"
        with self.assertRaises(vcopy.CopyRefused):
            vcopy.copy(str(self.src), len(self.data) + 1, str(out))
        os.link(self.src, self.root / "second-link")
        with self.assertRaises(vcopy.CopyRefused):
            vcopy.copy(str(self.src), len(self.data), str(out))
        os.unlink(self.root / "second-link")
        link = self.root / "link.raw"
        link.symlink_to(self.src)
        with self.assertRaises(vcopy.CopyRefused):
            vcopy.copy(str(link), len(self.data), str(out))
        real = vcopy.source_identity
        calls = []

        def drifting(path, size):  # Identity changes between the pre-copy and post-copy checks.
            calls.append(path)
            value = real(path, size)
            return dict(value, mtime_ns=value["mtime_ns"] + 1) if len(calls) > 3 else value
        with patch.object(vcopy, "source_identity", side_effect=drifting), self.assertRaises(vcopy.CopyRefused):
            vcopy.copy(str(self.src), len(self.data), str(out))
        self.assertFalse(out.exists())
        out.write_bytes(b"x")
        with self.assertRaises(FileExistsError):
            vcopy.copy(str(self.src), len(self.data), str(out))

    def test_template_guard(self):
        args = ("dev-ceph", 105, "pc-ubuntu-2604-kube-v1.37.2", ["pc-img-x", "pc-public"])
        self.assertEqual(vcopy.template_guard(FakePVE(), *args)["digest"], "de0de6c1788a246a5b5cf03433e47d9216a425ee")
        base = FakePVE().config
        for changes in ({"config": dict(base, template=0)}, {"config": dict(base, lock="backup")},
                        {"config": dict(base, name="other")}, {"config": dict(base, tags="pc-public")},
                        {"config": dict(base, digest=None)}, {"status": {"status": "running"}},
                        {"tasks": [{"id": "105", "upid": "UPID:x"}]},
                        {"resources": [{"vmid": 105, "pool": "tenant", "template": 1}]}):
            with self.subTest(changes=changes), self.assertRaises(vcopy.CopyRefused):
                vcopy.template_guard(FakePVE(**changes), *args)


class BundleTests(unittest.TestCase):
    def test_source_identity_and_receipt_feed_vm_artifact_prepare(self):
        self.assertEqual(bundle.source_identity(43505816158, SOURCE_SHA), "sha256:" + hashlib.sha256(
            b"proxcloud-template-build:run=43505816158:raw_sha256=" + SOURCE_SHA.encode()).hexdigest())
        for bad in ((0, SOURCE_SHA), (1, "x"), ("1", SOURCE_SHA)):
            with self.assertRaises(ValueError):
                bundle.source_identity(*bad)
        disk = b"QFI\xfb" + os.urandom(64)
        facts = {"kind": "OfflineSanitizationFacts", "os": "ubuntu-2604", "variant": "kubernetes",
                 "kubernetes_version": "1.37.2", "disk_sha256": hashlib.sha256(disk).hexdigest(),
                 "disk_bytes": len(disk), "virtual_size_bytes": 20 << 30,
                 "checks": {k: True for k in vm_artifact.CHECKS}}
        q = bundle.request("ubuntu-2604", "c" * 40, "a" * 64, "20261113205440", "1.37.2")
        r = bundle.receipt(q, facts, 43505816158, 7, SOURCE_SHA)
        self.assertEqual(r["run_ref"], "https://github.com/mutex-solutions/proxcloud/actions/runs/7")
        with self.assertRaises(ValueError):
            bundle.receipt(q, dict(facts, kubernetes_version="1.36.6"), 1, 7, SOURCE_SHA)
        with tempfile.TemporaryDirectory() as root:
            (Path(root) / "sealed.qcow2").write_bytes(disk)
            staging = Path(root) / "staging"
            staging.mkdir()
            m = bundle.stage(str(staging), q, r, str(Path(root) / "sealed.qcow2"))
            fd = os.open(staging, os.O_RDONLY | os.O_DIRECTORY)
            try:
                self.assertEqual(vm_artifact.verify(fd, expected_disk=facts["disk_sha256"]), m)
            finally:
                os.close(fd)
            self.assertEqual(m["image_tag"], "ubuntu-2604-k8s-1.37.2-20261113205440-cccccccccccc")
        with tempfile.TemporaryDirectory() as root:  # A receipt for another disk refuses the bundle.
            (Path(root) / "sealed.qcow2").write_bytes(disk + b"x")
            with self.assertRaises(ValueError):
                bundle.stage(root, q, r, str(Path(root) / "sealed.qcow2"))


class ProbeTests(unittest.TestCase):
    def test_os_inventory_reports_snapd_presence_never_values(self):
        g = fakes.FakeSnapdGuestFS("ubuntu-2404")
        code, record = inspect_os.os_inventory(vm_sanitize, g, "ubuntu-2404")
        self.assertEqual(code, 0)
        self.assertTrue(record["snapd"]["snapd_device_serial"] and record["sanitize_eligible"])
        self.assertEqual(record["snapd"]["snapd_serial_assertions"], 4)
        line = json.dumps(record)
        for secret in ("synthetic-serial", "synthetic-key-id", "$6$OLD", "10.200.", "PRIVATE"):
            self.assertNotIn(secret, line)
        self.assertGreater(record["hygiene"]["scan_hits"], 0)  # Lab address/key markers are counted.
        g = fakes.FakeSnapdGuestFS("ubuntu-2404")
        g.put(vm_sanitize.K8S_SNAPD_STATE, fakes.SNAPD_STATE.replace(b'"users":[]', b'"users":[{"id":1}]'))
        self.assertFalse(inspect_os.os_inventory(vm_sanitize, g, "ubuntu-2404")[1]["sanitize_eligible"])
        g = fakes.FakeGuestFS("ubuntu-2604")
        g.devices = ["/dev/sda", "/dev/sdb"]
        self.assertEqual(inspect_os.os_inventory(vm_sanitize, g, "ubuntu-2604")[0], 4)
        self.assertFalse([c for c in g.calls if c[0] in ("rm", "rm_rf", "write")])

    def test_probes_import_without_guestfs_or_side_effects(self):
        self.assertEqual(inspect_k8s.PHASE, "k8s-readonly-inventory-v1")
        self.assertEqual(inspect_k8s.VERSION.fullmatch("1.37.2").group(0), "1.37.2")

    def test_postscan_expected_schema(self):
        k = postscan_probe.expected("kubernetes", "1.37.2")
        self.assertEqual((k["kubernetes_version"], k["guest_code_executed"], k["snapd_unregistered"]), ("1.37.2", False, True))
        self.assertEqual(postscan_probe.expected("os", "ubuntu-2404")["phase"], "sealed-os-export-postscan")
        self.assertEqual(postscan_probe.parse(["--os", "debian-12"]), ("os", "debian-12"))
        for argv in (["--k8s", "latest"], ["--os", "alpine"], []):
            with self.assertRaises(ValueError):
                postscan_probe.parse(argv)


if __name__ == "__main__":
    unittest.main()
