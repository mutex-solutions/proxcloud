"""Pure tests for images/vm_export: no docker, guestfs, network or PVE. Fakes only."""
import contextlib
import copy as copy_module
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import MagicMock, patch

IMAGES = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(IMAGES))
from vm_export import bundle, capsule, copy as vcopy, inspect_k8s, inspect_os, postscan_probe, review, stream_sanitized  # noqa: E402,F401
from vm_export import inventory, postscan, sanitize  # noqa: E402
from vm_export.sanitize import StreamSink  # noqa: E402
import vm_artifact  # noqa: E402
import vm_sanitize  # noqa: E402

SPEC = importlib.util.spec_from_file_location("test_vm_sanitize_fakes", IMAGES / "tests" / "test_vm_sanitize.py")
fakes = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(fakes)
BASELINE = json.loads((IMAGES / "vm_export" / "baselines" / "ubuntu-2604-k8s.json").read_text())
SOURCE_SHA = "1749277d590172e38e1928922daa53908ab3a88b860aa1aa3f1cc7cd02bed492"
IMAGE_ENV = ["PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/scratch/home"]
CID = "c" * 64
REAL_POPEN = subprocess.Popen
HOST_USER = capsule.host_user  # The real rule; everything else sees the CI runner's uid:gid (not 1000).
RUNNER = patch.object(capsule, "host_user", return_value=(1002, 1002))
LOCAL_IMAGE = capsule.local_image  # The real lookup; everything else sees the image as pulled (no docker here).
PULLED = patch.object(capsule, "local_image", return_value=capsule.IMAGE_IDS[0])
HAND_LOADED = "sha256:1c0e402e0ff1da2732b03f93ff61e2c1e16a05f0a2091749abffc4d5360367d9"


def setUpModule():
    RUNNER.start()
    PULLED.start()


def tearDownModule():
    PULLED.stop()
    RUNNER.stop()


def capsule_spec(**changes):
    args = dict(check="k8s-inventory-1.37.2", binds={"/srv/a/source.raw": "/input/source.raw",
                                                      "/repo/images/vm_sanitize.py": "/bootstrap/vm_sanitize.py"},
                scratch="/runner/tmp/x-scratch", argv=["/usr/bin/python3", "-I", "/bootstrap/inspect_k8s.py"])
    args.update(changes)
    return capsule.spec(**args)


def inspected(s):
    """What `docker inspect` reports for a container created from create_argv(s)."""
    return {"Image": s["image"], "Config": {"User": "1002:1002", "Entrypoint": ["/usr/bin/env"], "Env": list(IMAGE_ENV),
                                            "Cmd": ["-i", *capsule.ENV, *s["argv"]]},
            "HostConfig": {"NetworkMode": "none", "ReadonlyRootfs": True, "Privileged": False, "Runtime": "runc",
                           "CgroupnsMode": "private", "UTSMode": "", "UsernsMode": "", "GroupAdd": None,
                           "MaskedPaths": ["/proc/kcore"], "ReadonlyPaths": ["/proc/sys"],
                           "LogConfig": {"Type": "none"}, "PidMode": "", "IpcMode": "private", "DeviceRequests": None,
                           "Devices": [], "CapAdd": None, "CapDrop": ["ALL"], "SecurityOpt": ["no-new-privileges"],
                           "Memory": capsule.size_bytes(s["memory"]), "MemorySwap": capsule.size_bytes(s["memory"]),
                           "NanoCpus": s["cpus"] * 10 ** 9, "PidsLimit": s["pids"], "ShmSize": 128 << 20,
                           "PortBindings": {}, "Binds": None, "VolumesFrom": None, "Links": None,
                           "Tmpfs": {"/tmp": "rw,exec,nosuid,nodev,size=1g,mode=0700,uid=1002,gid=1002"},
                           "Ulimits": [{"Name": "core", "Soft": 0, "Hard": 0}, {"Name": "nofile", "Soft": 256, "Hard": 256},
                                       {"Name": "fsize", "Soft": s["fsize"], "Hard": s["fsize"]},
                                       {"Name": "cpu", "Soft": s["cpu_seconds"], "Hard": s["cpu_seconds"]}]},
            "Mounts": [{"Type": "bind", "Source": h, "Destination": g, "RW": False} for h, g in s["binds"].items()] +
                      [{"Type": "bind", "Source": s["scratch"], "Destination": "/scratch", "RW": True}]}


class CapsuleTests(unittest.TestCase):
    def test_spec_refuses_unpinned_image_odd_paths_and_limits(self):
        for changes in ({"image": "proxcloud-vm-inspection-tools:latest"}, {"image": HAND_LOADED},
                        {"image": "sha256:" + "0" * 64}, {"image": capsule.IMAGE_REFERENCE}, {"check": "../x"},
                        {"binds": {"/a": "/etc/shadow"}}, {"binds": {"/a": "/input/x", "/b": "/input/x"}},
                        {"scratch": "relative"}, {"argv": []}, {"memory": "8G"}, {"pids": 0},
                        {"binds": {"/a,dst=/etc": "/input/x"}}, {"scratch": "/runner/a=b"}):
            with self.subTest(changes=changes), self.assertRaises(capsule.CapsuleError):
                capsule_spec(**changes)

    def test_only_the_ci_built_tool_image_starts_a_capsule(self):
        """Pinned by registry digest; locally known by its manifest digest (containerd image store) or its
        config digest (classic store), and by nothing else."""
        repository, digest = capsule.IMAGE_REFERENCE.split("@")
        self.assertEqual(repository, "ghcr.io/mutex-solutions/proxcloud-vm-export-tools")
        self.assertEqual(capsule.IMAGE_IDS, (digest, "sha256:df1770b213fb1bb85f72659797f6123ccce8941f0cf1959f999728552454f917"))
        self.assertEqual(digest, "sha256:d3ab36a5efbe47a285be14356bcbac92ced6f6eeb69555aa1e3338a5f37328b3")
        self.assertNotIn(HAND_LOADED, capsule.IMAGE_IDS)
        self.assertFalse(hasattr(capsule, "IMAGE"))
        for image in capsule.IMAGE_IDS:
            self.assertEqual(capsule_spec(image=image)["image"], image)
        calls = []

        def docker(stdout, code=0):
            def run(args, **kw):
                calls.append((args, kw))
                return subprocess.CompletedProcess(args, code, stdout=stdout)
            return patch.object(capsule, "_command", run)
        for image in capsule.IMAGE_IDS:
            with docker(image + "\n"):
                self.assertEqual(LOCAL_IMAGE(), image)
                with patch.object(capsule, "local_image", LOCAL_IMAGE):
                    self.assertEqual(capsule_spec()["image"], image)  # The default is the image that is present.
        self.assertEqual(calls[0][0], ["docker", "image", "inspect", "--format", "{{.Id}}", capsule.IMAGE_REFERENCE])
        for stdout, code in ((HAND_LOADED + "\n", 0), ("sha256:" + "0" * 64 + "\n", 0), ("", 1), (capsule.IMAGE_IDS[0], 1)):
            with self.subTest(stdout=stdout, code=code), docker(stdout, code), self.assertRaises(capsule.CapsuleError):
                LOCAL_IMAGE()

    def test_pull_is_by_digest_reference_without_stored_credentials(self):
        with tempfile.TemporaryDirectory() as tmp:
            calls = []

            def run(args, **kw):
                calls.append((args, kw))
                return subprocess.CompletedProcess(args, 0)
            with patch.object(capsule, "_command", run):
                self.assertEqual(capsule.pull(tmp), capsule.IMAGE_IDS[0])
            args, kw = calls[0]
            self.assertEqual(args, ["docker", "pull", "--quiet", capsule.IMAGE_REFERENCE])
            self.assertEqual(kw["env"]["DOCKER_CONFIG"], tmp)
            failed = lambda args, **kw: subprocess.CompletedProcess(args, 1)  # noqa: E731
            with patch.object(capsule, "_command", failed), self.assertRaises(capsule.CapsuleError):
                capsule.pull(tmp)
            (Path(tmp) / "config.json").write_text("{}")  # A client configuration could carry a credential.
            with patch.object(capsule, "_command", run), self.assertRaises(capsule.CapsuleError):
                capsule.pull(tmp)
        # A capsule itself never pulls.
        self.assertIn("--pull=never", capsule.create_argv(capsule_spec(), "n"))

    def test_create_argv_is_confined_and_binds_are_readonly(self):
        s = capsule_spec()
        argv = capsule.create_argv(s, "n")
        for flag in ("--pull=never", "--network=none", "--read-only", "--user=1002:1002", "--cap-drop=ALL",
                     "--security-opt=no-new-privileges", "--memory=8g", "--memory-swap=8g", "--log-driver=none"):
            self.assertIn(flag, argv)
        mounts = [argv[i + 1] for i, a in enumerate(argv) if a == "--mount"]
        self.assertEqual(mounts[0], "type=bind,src=/runner/tmp/x-scratch,dst=/scratch")
        self.assertTrue(all(m.endswith(",readonly") for m in mounts[1:]))
        self.assertEqual(argv[argv.index(s["image"]) + 1:argv.index(s["image"]) + 2], ["-i"])
        self.assertNotIn("--privileged", argv)

    def test_capsule_user_is_the_invoking_non_root_host_user(self):
        ids = lambda uid, euid, gid, egid: [patch.object(capsule.os, n, return_value=v) for n, v in (  # noqa: E731
            ("getuid", uid), ("geteuid", euid), ("getgid", gid), ("getegid", egid))]
        with contextlib.ExitStack() as stack:
            [stack.enter_context(p) for p in ids(1002, 1002, 1003, 1003)]
            self.assertEqual(HOST_USER(), (1002, 1003))
        for bad in ((0, 0, 0, 0), (999, 999, 999, 999), (999, 999, 1002, 1002), (1002, 0, 1002, 1002), (0, 1002, 1002, 1002),
                    (1002, 1002, 0, 0), (1002, 1002, 1002, 0), (1002, 1002, 999, 999), (1002, 1002, 6, 6)):
            with self.subTest(ids=bad), contextlib.ExitStack() as stack, self.assertRaises(capsule.CapsuleError):
                [stack.enter_context(p) for p in ids(*bad)]
                HOST_USER()
        with patch.object(capsule, "host_user", return_value=(1234, 1235)):  # Follows the uid, never a constant.
            argv = capsule.create_argv(capsule_spec(), "n")
            self.assertIn("--user=1234:1235", argv)
            self.assertIn("/tmp:rw,exec,nosuid,nodev,mode=0700,uid=1234,gid=1235,size=1g", argv)
            with self.assertRaises(capsule.CapsuleError):  # An inspect of a 1002:1002 container.
                capsule.check_confinement(inspected(capsule_spec()), capsule_spec(), IMAGE_ENV)

    def test_confinement_inspect_accepts_exact_and_refuses_any_drift(self):
        s = capsule_spec()
        capsule.check_confinement(inspected(s), s, IMAGE_ENV)
        hc = lambda **kw: lambda i: i["HostConfig"].update(**kw)  # noqa: E731
        drifts = (hc(CapDrop=[]), hc(CapDrop=["ALL", "x"]), hc(SecurityOpt=[]),
                  hc(SecurityOpt=["no-new-privileges", "seccomp=unconfined"]), hc(PidsLimit=0), hc(PidsLimit=None),
                  hc(Binds=["/:/host"]), hc(VolumesFrom=["other"]), hc(Links=["x:y"]), hc(Runtime="runsc"),
                  hc(CgroupnsMode="host"), hc(UTSMode="host"), hc(UsernsMode="host"), hc(GroupAdd=["0"]),
                  hc(Sysctls={"net.ipv4.ip_forward": "1"}), hc(Init=True), hc(MaskedPaths=[]), hc(ReadonlyPaths=None),
                  hc(Tmpfs={"/tmp": "rw,exec,nosuid,nodev,size=1g,mode=0700,uid=1002,gid=1002,suid"}),
                  hc(Tmpfs={"/tmp": "rw,exec,nosuid,nodev,size=1g,mode=0700,uid=1000,gid=1000"}),
                  lambda i: i["Config"].update(User="1000:1000"), lambda i: i["Config"].update(User="0:0"),
                  lambda i: i["Config"].update(User="1002:0"), lambda i: i["Config"].update(User=""),
                  lambda i: i["Config"]["Env"].append("LD_PRELOAD=/scratch/x.so"),
lambda i: i["HostConfig"].update(NetworkMode="bridge"),
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
                capsule.check_confinement(info, s, IMAGE_ENV)

    def test_json_sink_is_bounded(self):
        sink = capsule.JsonSink(limit=10)
        self.assertIsNone(sink(b"12345"))
        self.assertEqual(sink(b"678901"), "parent_output_limit")

    def test_schema_gate(self):
        term = {"exit_code": 0, "attach_exit_code": 0, "stop_reason": None, "known_cgroup": "/system.slice/x",
                "cleanup_errors": [], "cgroup_empty_or_absent": True}
        ok = lambda row: row.get("phase") == "p"  # noqa: E731
        self.assertEqual(capsule.gate_json(term, b'{"phase": "p"}\n', ok), (True, {"phase": "p"}))
        refused = {"phase": "inspection_refused", "exit_code": 0, "raw_output_retained": False}
        for data in (b'{"phase": "q"}\n', b'{"phase": "p"}\n{"phase": "p"}\n', b'{"phase": "p", "x": "10.200.1.2"}\n',
                     b'{"phase": "p", "k": "-----BEGIN PRIVATE KEY"}\n', b"not json\n", b"\xff\n", b"[]\n"):
            self.assertEqual(capsule.gate_json(term, data, ok), (False, refused))
        for bad in ({"stop_reason": "parent_wall_limit"}, {"known_cgroup": ""}, {"attach_exit_code": 1},
                    {"cleanup_errors": ["docker-stop-exit"]}, {"cgroup_empty_or_absent": False}):
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

    def test_stream_gate_accepts_only_the_variants_allowlisted_virtual_sizes(self):
        k8s = {"kind": "OfflineSanitizationFacts", "os": "ubuntu-2604", "variant": "kubernetes",
               "kubernetes_version": "1.37.2", "disk_sha256": "a" * 64, "disk_bytes": 100,
               "checks": {k: True for k in stream_sanitized.CHECKS}}
        native = {k: v for k, v in dict(k8s, os="debian-13", variant="os").items() if k != "kubernetes_version"}
        cases = ((k8s, "ubuntu-2604", "1.37.2", (8 << 30, 20 << 30), (5 << 30, 10 << 30, (8 << 30) + 512, float(8 << 30))),
                 (native, "debian-13", None, (5 << 30, 20 << 30), (8 << 30, 4 << 30, (5 << 30) - 1, "5368709120")))
        for facts, selected, version, good, bad in cases:
            for size, accepted in [(size, True) for size in good] + [(size, False) for size in bad]:
                with self.subTest(variant=facts["variant"], size=size):
                    self.assertIs(stream_sanitized.valid(dict(facts, virtual_size_bytes=size), selected, version), accepted)


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

    def test_source_disk_size_is_allowlisted_not_compared_with_the_baselines_20_gib(self):
        """The committed baseline records its own 20 GiB template; an 8 GiB template derives the same review."""
        self.assertEqual(BASELINE["inventory"]["facts"]["parent"]["virtual_bytes"], 20 << 30)
        legacy, _ = review.derive(reviewed_inventory(), BASELINE, "1.37.2", SOURCE_SHA)
        inv = reviewed_inventory()
        inv["facts"]["parent"].update(virtual_bytes=8 << 30, raw_bytes=8 << 30)
        small, _ = review.derive(inv, BASELINE, "1.37.2", SOURCE_SHA)
        self.assertEqual(small, legacy)
        for virtual, raw in ((5 << 30, 5 << 30), (10 << 30, 10 << 30), (8 << 30, 20 << 30), (20 << 30, 8 << 30),
                             (float(8 << 30), float(8 << 30)), (float(8 << 30), 8 << 30), (8 << 30, float(8 << 30)),
                             ("8589934592", "8589934592")):
            inv = reviewed_inventory()
            inv["facts"]["parent"].update(virtual_bytes=virtual, raw_bytes=raw)
            with self.subTest(virtual=virtual, raw=raw), self.assertRaises(review.ReviewRefused) as caught:
                review.derive(inv, BASELINE, "1.37.2", SOURCE_SHA)
            self.assertEqual(caught.exception.fields, ["facts.parent.virtual_bytes"])
        for mutate in (lambda i: i["facts"]["parent"].pop("raw_bytes"), lambda i: i["facts"].pop("parent")):
            inv = reviewed_inventory()
            mutate(inv)
            with self.assertRaises(review.ReviewRefused):
                review.derive(inv, BASELINE, "1.37.2", SOURCE_SHA)
        # Every other parent fact still has to equal the baseline.
        inv = reviewed_inventory()
        inv["facts"]["parent"].update(virtual_bytes=8 << 30, raw_bytes=8 << 30, source_stopped=False)
        with self.assertRaises(review.ReviewRefused) as caught:
            review.derive(inv, BASELINE, "1.37.2", SOURCE_SHA)
        self.assertEqual(caught.exception.fields, ["facts.parent.source_stopped"])

    def test_project_drops_only_version_fields(self):
        inv = reviewed_inventory()
        self.assertEqual(review.project(inv), BASELINE["inventory"])
        self.assertEqual(review.diff({"a": {"b": [1, 2]}}, {"a": {"b": [2, 1]}}), ["a.b"])
        self.assertEqual(review.diff({"a": 1}, {"a": True}), ["a"])


class FakePVE:
    def __init__(self, **changes):
        self.config = {"template": 1, "name": "pc-ubuntu-2604-kube-v1.37.2", "tags": "pc-public;pc-img-x",
                       "digest": "de0de6c1788a246a5b5cf03433e47d9216a425ee",
                       "scsi0": "pc-ci-images:105/base-105-disk-1.raw,size=20G", "efidisk0": "pc-ci-images:105/x.raw"}
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
        snap = vcopy.template_guard(FakePVE(), *args)
        self.assertEqual((snap["digest"], snap["volume"]),
                         ("de0de6c1788a246a5b5cf03433e47d9216a425ee", "pc-ci-images:105/base-105-disk-1.raw"))
        self.assertEqual(vcopy.template_source(snap, "/srv/pc-ci-images"),
                         "/srv/pc-ci-images/images/105/base-105-disk-1.raw")
        base = FakePVE().config
        for changes in ({"config": dict(base, template=0)}, {"config": dict(base, lock="backup")},
                        {"config": dict(base, name="other")}, {"config": dict(base, tags="pc-public")},
                        {"config": dict(base, digest=None)}, {"status": {"status": "running"}},
                        {"tasks": [{"id": "105", "upid": "UPID:x"}]},
                        {"resources": [{"vmid": 105, "pool": "tenant", "template": 1}]},
                        {"config": dict(base, scsi0="pc-ci-images:106/base-106-disk-1.raw")},
                        {"config": dict(base, scsi0="local-lvm:base-105-disk-1")},
                        {"config": dict(base, scsi0="pc-ci-images:105/vm-105-disk-1.raw")},
                        {"config": {k: v for k, v in base.items() if k != "scsi0"}}):
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



class FakeDocker:
    """Fakes capsule._command/_inspect; `docker start --attach` runs a real local python child."""

    def __init__(self, s, script, stop_works=True, kill_works=True, stop_exit=0, drift=None):
        self.s, self.script, self.drift = s, script, drift
        self.stop_works, self.kill_works, self.stop_exit = stop_works, kill_works, stop_exit
        self.running, self.calls, self.child = False, [], None

    def command(self, args, timeout=20, **kw):
        self.calls.append(args[1])
        if args[1] in ("stop", "kill"):
            if self.stop_works if args[1] == "stop" else self.kill_works:
                self.running = False
                if self.child is not None and self.child.poll() is None:
                    self.child.kill()
            return subprocess.CompletedProcess(args, self.stop_exit if args[1] == "stop" else 0)
        return subprocess.CompletedProcess(args, 0, stdout=CID + "\n")  # create / rm

    def inspect(self, ref):
        if ref == self.s["image"]:
            return {"Config": {"Env": list(IMAGE_ENV)}}
        info = inspected(self.s)
        if self.drift:
            self.drift(info)
        info.update(Id=CID, State={"Running": self.running, "Pid": 4242 if self.running else 0,
                                   "Status": "running" if self.running else "exited", "ExitCode": 0})
        return info

    def popen(self, args, **kw):
        self.calls.append("start")
        self.running = True
        self.child = REAL_POPEN([sys.executable, "-c", self.script], **kw)
        return self.child


class RunTests(unittest.TestCase):
    """capsule.run() over a fake docker: stop proof, kill path, wall limit, cgroup proof, re-pin."""

    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        root = Path(self.dir.name)
        (root / "source.raw").write_bytes(b"disk")
        (root / "scratch").mkdir()
        self.s = capsule.spec("t-run", {str(root / "source.raw"): "/input/source.raw"}, root / "scratch",
                              ["/usr/bin/python3", "-I", "/bootstrap/p.py"], wall=30)
        self.pins = capsule.pin_all(self.s["binds"])

    def tearDown(self):
        self.dir.cleanup()

    def run_capsule(self, fake, empty=True, cgroup=None, pins=None, **spec):
        sink = capsule.JsonSink()
        rows = cgroup if cgroup is not None else ["/system.slice/docker-%s.scope" % CID]
        with patch.object(capsule, "_command", fake.command), patch.object(capsule, "_inspect", fake.inspect), \
                patch.object(capsule.subprocess, "Popen", fake.popen), \
                patch.object(capsule, "_cgroup_of", side_effect=rows if callable(rows) else lambda pid: rows), \
                patch.object(capsule, "_cgroup_empty", return_value=empty):
            term = capsule.run(dict(self.s, **spec), sink, self.pins if pins is None else pins)
        return term, bytes(sink.data)

    def test_clean_run_proves_stop_and_removes_exact_container(self):
        fake = FakeDocker(self.s, "import sys; sys.stdout.write('{\"phase\": \"p\"}\\n')")
        term, data = self.run_capsule(fake)
        self.assertEqual((term["stop_reason"], term["cleanup_errors"], term["cgroup_empty_or_absent"], term["exit_code"]),
                         (None, [], True, 0))
        self.assertEqual(data, b'{"phase": "p"}\n')
        self.assertEqual(fake.calls, ["create", "start", "stop", "rm"])
        self.assertTrue(capsule.gate_json(term, data, lambda row: True)[0])

    def test_container_still_running_is_unresolved_and_kept(self):
        fake = FakeDocker(self.s, "pass", stop_works=False, kill_works=False)
        with self.assertRaises(capsule.Unresolved):
            self.run_capsule(fake)
        self.assertIn("kill", fake.calls)
        self.assertNotIn("rm", fake.calls)

    def test_populated_cgroup_is_unresolved_and_kept(self):
        fake = FakeDocker(self.s, "pass")
        with self.assertRaises(capsule.Unresolved):
            self.run_capsule(fake, empty=False)
        self.assertNotIn("rm", fake.calls)

    def test_failed_stop_falls_back_to_exact_kill(self):
        fake = FakeDocker(self.s, "pass", stop_works=False, stop_exit=1)
        term, _ = self.run_capsule(fake)
        self.assertEqual(term["cleanup_errors"], ["docker-stop-exit"])
        self.assertEqual(fake.calls, ["create", "start", "stop", "kill", "rm"])
        self.assertFalse(capsule.gate_json(term, b'{"phase": "p"}\n', lambda row: True)[0])

    def test_wall_limit_stops_the_capsule(self):
        fake = FakeDocker(self.s, "import time; time.sleep(30)")
        started = time.monotonic()
        term, _ = self.run_capsule(fake, wall=1)
        self.assertEqual(term["stop_reason"], "parent_wall_limit")
        self.assertLess(time.monotonic() - started, 15)

    def test_unobserved_cgroup_and_changed_inputs_are_stop_reasons(self):
        def gone(pid):
            raise FileNotFoundError
        term, _ = self.run_capsule(FakeDocker(self.s, "pass"), cgroup=gone)
        self.assertEqual((term["stop_reason"], term["cgroup_empty_or_absent"]), ("exact_live_cgroup_not_observed", False))
        term, _ = self.run_capsule(FakeDocker(self.s, "pass"), pins={"/input/source.raw": "0" * 64})
        self.assertEqual(term["stop_reason"], "input_changed_during_run")

    def test_confinement_drift_never_starts(self):
        fake = FakeDocker(self.s, "pass", drift=lambda i: i["HostConfig"].update(SecurityOpt=[]))
        term, _ = self.run_capsule(fake)
        self.assertEqual(term["stop_reason"], "confinement_caps")
        self.assertNotIn("start", fake.calls)


class ScratchTests(unittest.TestCase):
    def test_prepare_scratch_guards(self):
        with tempfile.TemporaryDirectory() as base:
            with patch.object(capsule, "fs_type", return_value="ext4"):
                with patch.object(capsule, "host_user", side_effect=capsule.CapsuleError("capsule user")), \
                        self.assertRaises(capsule.CapsuleError):
                    capsule.prepare_scratch(base, "x", 0)
                self.assertFalse((Path(base) / "x-scratch").exists())
                for check in ("../escaped", "/abs", "UPPER"):
                    with self.subTest(check=check), self.assertRaises(capsule.CapsuleError):
                        capsule.prepare_scratch(base, check, 0)
                self.assertFalse((Path(base).parent / "escaped-scratch").exists())
                self.assertEqual(capsule.prepare_scratch(base, "ok", 0), Path(base) / "ok-scratch")
            for kind in ("tmpfs", "ramfs"):
                with patch.object(capsule, "fs_type", return_value=kind), self.assertRaises(capsule.CapsuleError):
                    capsule.prepare_scratch(base, "y", 0)
        self.assertIsNotNone(capsule.fs_type("/"))

    def test_remove_scratch_unlocks_capsule_written_dirs(self):
        with tempfile.TemporaryDirectory() as base:
            locked = Path(base) / "s" / "work" / "locked"
            locked.mkdir(parents=True)
            (locked / "guest.qcow2").write_bytes(b"x")
            os.chmod(locked, 0)
            capsule.remove_scratch(Path(base) / "s")
            self.assertFalse((Path(base) / "s").exists())


def receipt_for(path, data):
    return {"kind": "TemplateDiskCopy", "raw_sha256": hashlib.sha256(data).hexdigest(), "raw_bytes": len(data),
            "template_unchanged_stopped": True, "template": {"vmid": 105}, "source": str(path)}


class SealTests(unittest.TestCase):
    def test_seal_only_after_full_proof_and_reread(self):
        disk = b"QFI\xfb" + bytes(100)
        facts = {"kind": "OfflineSanitizationFacts", "os": "ubuntu-2604", "variant": "kubernetes",
                 "kubernetes_version": "1.37.2", "disk_sha256": hashlib.sha256(disk).hexdigest(),
                 "disk_bytes": len(disk), "virtual_size_bytes": 20 << 30,
                 "checks": {k: True for k in stream_sanitized.CHECKS}}
        term = {"exit_code": 0, "attach_exit_code": 0, "stop_reason": None, "cgroup_empty_or_absent": True,
                "cleanup_errors": []}
        with tempfile.TemporaryDirectory() as root:
            spool_path = Path(root) / "disk.qcow2.partial"
            with open(spool_path, "wb") as spool:
                sink = StreamSink(spool, "ubuntu-2604", "1.37.2")
                sink(json.dumps(facts).encode() + b"\n" + disk)
            self.assertTrue(sanitize.sealable(term, sink, spool_path))
            for bad in ({"stop_reason": "parent_wall_limit"}, {"cgroup_empty_or_absent": False},
                        {"cleanup_errors": ["docker-rm-OSError"]}, {"exit_code": 1}, {"attach_exit_code": 137}):
                with self.subTest(bad=bad):
                    self.assertFalse(sanitize.sealable(dict(term, **bad), sink, spool_path))
            spool_path.write_bytes(disk[:-1] + b"x")  # Re-read differs from the streamed hash.
            self.assertFalse(sanitize.sealable(term, sink, spool_path))
            short = StreamSink(io.BytesIO(), "ubuntu-2604", "1.37.2")
            short(json.dumps(facts).encode() + b"\n" + disk[:10])
            self.assertFalse(sanitize.sealable(term, short, spool_path))

    def test_sanitize_refuses_existing_outputs_and_unbound_sources_before_any_capsule(self):
        with tempfile.TemporaryDirectory() as root:
            root = Path(root)
            src, data = root / "source.raw", b"raw bytes"
            src.write_bytes(data)
            (root / "receipt.json").write_text(json.dumps(receipt_for(src, data)))
            (root / "review.json").write_text(json.dumps({"source_raw_sha256": "f" * 64}))
            out = root / "out"
            out.mkdir()
            args = ("ubuntu-2604", "1.37.2", str(src), str(root / "receipt.json"), str(root / "review.json"),
                    str(root), str(out), "1g", 600)
            with patch.object(sanitize, "mem_available", return_value=64 << 30), \
                    patch.object(capsule, "run", side_effect=AssertionError("no capsule")):
                with self.assertRaisesRegex(capsule.CapsuleError, "review names another source"):
                    sanitize.launch(*args)
                (root / "receipt.json").write_text(json.dumps(receipt_for(src, b"other")))
                with self.assertRaisesRegex(capsule.CapsuleError, "copy receipt"):
                    sanitize.launch(*args)
                (out / "sanitize-facts.json").write_text("{}")
                with self.assertRaisesRegex(capsule.CapsuleError, "output already exists"):
                    sanitize.launch(*args)


class LauncherTests(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.root = Path(self.dir.name)
        self.src, self.data = self.root / "source.raw", b"raw bytes"
        self.src.write_bytes(self.data)
        self.receipt = self.root / "receipt.json"
        self.receipt.write_text(json.dumps(receipt_for(self.src, self.data)))
        self.out = self.root / "out"
        self.out.mkdir()

    def tearDown(self):
        self.dir.cleanup()

    def inventory(self, row, removed, run=None):
        term = {"container_id": CID, "exit_code": 0, "attach_exit_code": 0, "stop_reason": None,
                "known_cgroup": "/x", "cgroup_empty_or_absent": True, "cleanup_errors": [], "stderr_bytes_discarded": 0}

        def fake_run(s, sink, pins):
            sink(json.dumps(row).encode() + b"\n")
            return term
        with patch.object(capsule, "prepare_scratch", return_value=self.root / "scratch"), \
                patch.object(capsule, "remove_scratch", side_effect=removed.append), \
                patch.object(capsule, "run", side_effect=run or fake_run), contextlib.redirect_stdout(io.StringIO()):
            return inventory.launch("k8s-inventory-1.37.2", str(self.src), str(self.receipt), "/input/source.raw",
                                    "inspect_k8s.py", ["1.37.2", "2.3.2", "1.4.3"], inventory.valid_k8s,
                                    inventory.accepted_k8s, str(self.root), str(self.out), 60)

    def test_inventory_binds_receipt_and_exits_1_on_a_refusing_result(self):
        row = dict({k: None for k in inventory.K8S_TOP}, phase="k8s-readonly-inventory-v1", policy_result="REFUSED",
                   readonly_no_guest_code_or_mutation=True, publication_eligible=False)
        removed = []
        self.assertEqual(self.inventory(row, removed), 1)
        run = json.loads((self.out / "inventory-run.json").read_text())
        written = (self.out / "inventory.json").read_bytes()
        self.assertEqual((run["clean"], run["accepted"], run["inventory_sha256"]),
                         (True, False, hashlib.sha256(written).hexdigest()))
        self.assertEqual(run["pins"]["/input/source.raw"], hashlib.sha256(self.data).hexdigest())
        self.assertEqual(removed, [self.root / "scratch"])
        for name in ("inventory.json", "inventory-run.json"):
            (self.out / name).unlink()
        self.assertEqual(self.inventory(dict(row, policy_result="PASS_WITH_PLACEHOLDER_REVIEW"), []), 0)
        self.assertTrue(inventory.accepted_os({"sanitize_eligible": True}))
        self.assertFalse(inventory.accepted_os({"sanitize_eligible": False}))

    def test_inventory_refuses_another_disk_and_keeps_scratch_when_unresolved(self):
        self.receipt.write_text(json.dumps(receipt_for(self.src, b"another disk")))
        with self.assertRaisesRegex(capsule.CapsuleError, "copy receipt"):
            self.inventory({}, [])
        self.receipt.write_text(json.dumps(receipt_for(self.src, self.data)))
        removed = []
        with self.assertRaises(capsule.Unresolved):
            self.inventory({}, removed, run=capsule.Unresolved("x"))
        self.assertEqual(removed, [])

    def test_postscan_binds_the_sealed_disk_and_sanitized_review(self):
        disk = self.root / "disk.qcow2"
        disk.write_bytes(b"QFI\xfb sealed")
        os.chmod(disk, 0o400)
        review_file = self.root / "k8s-review.json"
        review_file.write_bytes(b'{"r": 1}')
        facts = {"variant": "kubernetes", "kubernetes_version": "1.37.2", "disk_bytes": disk.stat().st_size,
                 "disk_sha256": hashlib.sha256(disk.read_bytes()).hexdigest()}
        run = {"sealed_after_cgroup_stop": True, "facts": facts,
               "pins": {"/input/k8s-review.json": hashlib.sha256(b'{"r": 1}').hexdigest()}}
        cases = ((dict(run, sealed_after_cgroup_stop=False), "not sealed"),
                 (dict(run, facts=dict(facts, disk_sha256="0" * 64)), "sealed disk hash"),
                 (dict(run, pins={"/input/k8s-review.json": "0" * 64}), "review differs"))
        for i, (record, reason) in enumerate(cases):
            path = self.root / ("run-%d.json" % i)
            path.write_text(json.dumps(record))
            with self.subTest(reason=reason), patch.object(capsule, "run", side_effect=AssertionError("no capsule")), \
                    self.assertRaisesRegex(capsule.CapsuleError, reason):
                postscan.launch("kubernetes", "1.37.2", str(disk), str(path), str(review_file), str(self.root),
                                str(self.out), 60)


class CopyGuardTests(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.root = Path(self.dir.name)
        self.data = b"boot" + bytes(2 << 20) + b"tail"
        (self.root / "images" / "105").mkdir(parents=True)
        (self.root / "images" / "105" / "base-105-disk-1.raw").write_bytes(self.data)
        self.out = self.root / "source.raw"
        self.args = ("dev-ceph", 105, "pc-ubuntu-2604-kube-v1.37.2", ["pc-img-x", "pc-public"], str(self.root))

    def tearDown(self):
        self.dir.cleanup()

    def test_hash_mismatches_refuse_and_leave_no_output(self):
        real = vcopy.stream_hash
        for bad_call in (2, 3):  # 2 = the independent source re-read, 3 = the local copy re-read.
            calls = []

            def flaky(path, size, out_fd=None):
                calls.append(path)
                digest, written = real(path, size, out_fd)
                return ("0" * 64 if len(calls) == bad_call else digest), written
            with self.subTest(bad_call=bad_call), patch.object(vcopy, "stream_hash", side_effect=flaky), \
                    self.assertRaises(vcopy.CopyRefused):
                vcopy.copy(str(self.root / "images/105/base-105-disk-1.raw"), len(self.data), str(self.out))
            self.assertFalse(self.out.exists())

    def test_guarded_copy_reads_the_guarded_volume_and_binds_the_snapshot(self):
        r = vcopy.guarded_copy(FakePVE(), *self.args, len(self.data), str(self.out))
        self.assertEqual((r["source"], r["template"]["volume"], r["template_unchanged_stopped"], r["raw_sha256"]),
                         (str(self.root / "images/105/base-105-disk-1.raw"), "pc-ci-images:105/base-105-disk-1.raw",
                          True, hashlib.sha256(self.data).hexdigest()))
        self.out.chmod(0o600)
        self.out.unlink()

        class Changing(FakePVE):
            def get(self, path):
                if path.endswith("/config"):
                    self.reads = getattr(self, "reads", 0) + 1
                    if self.reads > 1:
                        return dict(self.config, digest="f" * 40)
                return super().get(path)
        with self.assertRaisesRegex(vcopy.CopyRefused, "template changed"):
            vcopy.guarded_copy(Changing(), *self.args, len(self.data), str(self.out))
        self.assertFalse(self.out.exists())

    def test_main_refuses_an_existing_receipt_before_copying(self):
        receipt = self.root / "receipt.json"
        receipt.write_text("{}")
        err = io.StringIO()
        with contextlib.redirect_stderr(err):
            code = vcopy.main(["--node", "dev-ceph", "--vmid", "105", "--name", "n", "--tags", "a", "--mount",
                               str(self.root), "--expected-bytes", str(len(self.data)), "--out", str(self.out),
                               "--receipt", str(receipt)])
        self.assertEqual((code, err.getvalue()), (2, "copy refused: receipt already exists\n"))
        self.assertFalse(self.out.exists())


class EvidenceTests(unittest.TestCase):
    def test_review_bind_requires_the_whole_evidence_chain(self):
        inv = b'{"phase": "k8s-readonly-inventory-v1"}\n'
        run = {"clean": True, "accepted": True, "stop_reason": None, "cleanup_errors": [], "cgroup_empty_or_absent": True,
               "inventory_sha256": hashlib.sha256(inv).hexdigest(), "pins": {"/input/source.raw": SOURCE_SHA}}
        receipt = {"kind": "TemplateDiskCopy", "template_unchanged_stopped": True, "raw_sha256": SOURCE_SHA}
        self.assertEqual(review.bind(inv, run, receipt), SOURCE_SHA)
        for bad_run, bad_receipt, field in ((dict(run, accepted=False), receipt, "inventory_run"),
                                            (dict(run, stop_reason="x"), receipt, "inventory_run"),
                                            (dict(run, cleanup_errors=["e"]), receipt, "inventory_run"),
                                            (dict(run, inventory_sha256="0" * 64), receipt, "inventory_run"),
                                            (run, dict(receipt, raw_sha256="0" * 64), "copy_receipt"),
                                            (run, dict(receipt, template_unchanged_stopped=False), "copy_receipt"),
                                            ({}, receipt, "evidence")):
            with self.subTest(field=field), self.assertRaises(review.ReviewRefused) as caught:
                review.bind(inv, bad_run, bad_receipt)
            self.assertIn(field, caught.exception.fields)

    def test_diff_hides_guest_keys_and_is_type_strict_in_lists(self):
        self.assertEqual(review.diff({"dropin_default_users": {"a.cfg": None}},
                                     {"dropin_default_users": {"guest-secret.cfg": None}}), ["dropin_default_users"])
        self.assertEqual(review.diff({"a": [True]}, {"a": [1]}), ["a"])
        inv = reviewed_inventory()
        inv["dropin_default_users"]["guest-secret.cfg"] = None
        with self.assertRaises(review.ReviewRefused) as caught:
            review.derive(inv, BASELINE, "1.37.2", SOURCE_SHA)
        self.assertNotIn("guest-secret", str(caught.exception))

    def test_bundle_takes_facts_only_from_a_sealed_run_of_that_source(self):
        facts = {"kind": "OfflineSanitizationFacts"}
        run = {"sealed_after_cgroup_stop": True, "facts": facts, "pins": {"/input/source.qcow2": SOURCE_SHA}}
        self.assertIs(bundle.sealed_facts(run, SOURCE_SHA), facts)
        for bad in (dict(run, sealed_after_cgroup_stop=False), dict(run, facts=None),
                    dict(run, pins={"/input/source.qcow2": "0" * 64}),
                    dict(run, pins={"/input/source.raw": SOURCE_SHA, "/input/source.qcow2": SOURCE_SHA})):
            with self.assertRaises(ValueError):
                bundle.sealed_facts(bad, SOURCE_SHA)

    def test_bundle_manifest_carries_the_sealed_facts_actual_virtual_size(self):
        """5 GiB native OS, 8 GiB Kubernetes node and the legacy 20 GiB reach the manifest unchanged."""
        disk = b"QFI\xfb" + bytes(100)
        for version, virtual in ((None, 5 << 30), (None, 20 << 30), ("1.37.2", 8 << 30), ("1.37.2", 20 << 30)):
            with self.subTest(version=version, virtual=virtual), tempfile.TemporaryDirectory() as root:
                q = bundle.request("ubuntu-2604", "a" * 40, "b" * 64, "20261114000000", kubernetes_version=version)
                facts = {"kind": "OfflineSanitizationFacts", "os": "ubuntu-2604", "variant": q["variant"],
                         "disk_sha256": hashlib.sha256(disk).hexdigest(), "disk_bytes": len(disk),
                         "virtual_size_bytes": virtual, "checks": {k: True for k in stream_sanitized.CHECKS}}
                if version:
                    facts["kubernetes_version"] = version
                self.assertIs(stream_sanitized.valid(facts, "ubuntu-2604", version), True)
                (Path(root) / "disk").write_bytes(disk)
                (Path(root) / "staging").mkdir()
                m = bundle.stage(Path(root) / "staging", q, bundle.receipt(q, facts, 1, 2, SOURCE_SHA), Path(root) / "disk")
                self.assertEqual((m["disk"]["virtual_size_bytes"], m["clean_export_receipt"]["virtual_size_bytes"]),
                                 (virtual, virtual))

    def test_pinned_input_refuses_symlinked_parents(self):
        with tempfile.TemporaryDirectory() as root:
            (Path(root) / "real").mkdir()
            (Path(root) / "real" / "f").write_bytes(b"x")
            (Path(root) / "link").symlink_to(Path(root) / "real")
            self.assertEqual(capsule.pinned_input(Path(root) / "real" / "f"), hashlib.sha256(b"x").hexdigest())
            with self.assertRaises(capsule.CapsuleError):
                capsule.pinned_input(Path(root) / "link" / "f")


class ProbeSizeTests(unittest.TestCase):
    """The probes run under libguestfs only inside the capsule; these pin which disk sizes each one admits."""

    def run_main(self, module, argv, **replaced):
        """main() up to the first replaced call that raises: host uid, no KVM, a fake guestfs, nothing touched."""
        with contextlib.ExitStack() as stack:
            for mocked in (patch.object(module.os, "getuid", return_value=1002),
                           patch.object(module.os, "geteuid", return_value=1002),
                           patch.object(module.pathlib.Path, "exists", return_value=False),
                           patch.object(module.pathlib.Path, "mkdir"),
                           patch.object(module.pathlib.Path, "read_bytes", return_value=b"{}"),
                           patch.dict(os.environ), patch.object(sys, "path", list(sys.path)),
                           patch.dict(sys.modules, {"guestfs": MagicMock()}),
                           patch.object(vm_sanitize, "mount_all"), patch.object(vm_sanitize, "load_review")):
                stack.enter_context(mocked)
            mocks = {name: stack.enter_context(patch.object(owner, name, **how))
                     for name, (owner, how) in replaced.items()}
            with self.assertRaisesRegex(RuntimeError, "stop"):
                module.main(list(argv))
        return mocks

    def test_each_probe_admits_exactly_its_own_variant_row(self):
        stop = {"side_effect": RuntimeError("stop at the size gate")}
        for module, argv, variant in ((inspect_os, ["ubuntu-2404"], "os"), (postscan_probe, ["--os", "debian-13"], "os"),
                                      (postscan_probe, ["--k8s", "1.37.2"], "kubernetes")):
            with self.subTest(module=module.__name__, variant=variant):
                gate = self.run_main(module, argv, image_info=(vm_sanitize, stop))["image_info"]
                gate.assert_called_once_with(module.SOURCE, vm_sanitize.VIRTUAL_BYTES[variant])
        self.assertNotEqual(vm_sanitize.VIRTUAL_BYTES["os"], vm_sanitize.VIRTUAL_BYTES["kubernetes"])

    def test_kubernetes_inventory_records_the_size_that_was_read(self):
        for size in vm_sanitize.VIRTUAL_BYTES["kubernetes"]:
            with self.subTest(size=size):
                mocks = self.run_main(inspect_k8s, ["1.37.2", "2.3.2", "1.4.3"],
                                      raw_info=(vm_sanitize, {"return_value": {"virtual-size": size}}),
                                      inventory=(inspect_k8s, {"side_effect": RuntimeError("stop at the inventory")}))
                mocks["raw_info"].assert_called_once_with(inspect_k8s.RAW)  # The allowlist gate; its size is passed on.
                self.assertEqual(mocks["inventory"].call_args.args[3:], (size, "1.37.2", "2.3.2", "1.4.3"))
                vs = MagicMock()
                vs.k8s_inventory.side_effect = RuntimeError("stop at the facts")
                with patch.object(inspect_k8s, "layout_facts", return_value=(True, {})), \
                        patch.object(inspect_k8s, "sudo_hashes", return_value={}), self.assertRaisesRegex(RuntimeError, "stop"):
                    inspect_k8s.inventory(vs, None, MagicMock(), size, "1.37.2", "2.3.2", "1.4.3")
                parent = vs.k8s_inventory.call_args.args[3]
                self.assertEqual((parent["virtual_bytes"], parent["raw_bytes"]), (size, size))  # Never a constant.


class CapsuleUidTests(unittest.TestCase):
    """In-capsule entry points accept the invoking host uid (>= 1000, real == effective), never root."""
    UIDS = ((1002, 1002, True), (1000, 1000, True), (0, 0, False), (999, 999, False), (1002, 0, False),
            (0, 1002, False), (1002, 1000, False))

    def test_probes_follow_the_host_uid(self):
        stop = RuntimeError("past the uid gate")  # The scratch mkdir is the first thing after it.
        for module, argv in ((inspect_k8s, ["1.37.2", "1.37.2", "1.37.2"]), (inspect_os, ["ubuntu-2404"]),
                             (postscan_probe, ["--os", "ubuntu-2404"])):
            for uid, euid, accepted in self.UIDS:
                with self.subTest(module=module.__name__, uid=uid, euid=euid), \
                        patch.object(module.os, "getuid", return_value=uid), \
                        patch.object(module.os, "geteuid", return_value=euid), \
                        patch.object(module.pathlib.Path, "exists", return_value=False), \
                        patch.object(module.pathlib.Path, "mkdir", side_effect=stop), \
                        self.assertRaises(RuntimeError if accepted else AssertionError):
                    module.main(list(argv))

    def test_stream_launcher_follows_the_host_uid(self):
        stop = RuntimeError("past the uid gate")
        for uid, euid, accepted in self.UIDS:
            with self.subTest(uid=uid, euid=euid), patch.object(stream_sanitized.os, "getuid", return_value=uid), \
                    patch.object(stream_sanitized.os, "geteuid", return_value=euid), \
                    patch.object(stream_sanitized.subprocess, "run", side_effect=stop) as run:
                if accepted:
                    with self.assertRaises(RuntimeError):
                        stream_sanitized.main(["--os", "ubuntu-2404", "600"])
                else:
                    self.assertEqual(stream_sanitized.main(["--os", "ubuntu-2404", "600"]), 1)
                    run.assert_not_called()


class NativeProbeTests(unittest.TestCase):
    def test_unwipeable_snapd_key_makes_the_os_inventory_ineligible(self):
        g = fakes.FakeSnapdGuestFS("ubuntu-2404")
        g.put(vm_sanitize.K8S_SNAPD_KEYS + "/.hidden", b"synthetic")
        record = inspect_os.os_inventory(vm_sanitize, g, "ubuntu-2404")[1]
        self.assertGreater(record["snapd"]["unwipeable_key_entries"], 0)
        self.assertFalse(record["sanitize_eligible"])

    def test_native_postscan_requires_snapd_unregistered(self):
        g = fakes.FakeSnapdGuestFS("ubuntu-2404")
        vm_sanitize.sanitize(g, "ubuntu-2404")
        for path in ("/usr/bin/cloud-init", "/usr/sbin/sshd", "/usr/sbin/qemu-ga"):
            g.put(path, b"x")
        postscan_probe.check_os(vm_sanitize, g, "ubuntu-2404")
        g.put(vm_sanitize.K8S_SNAPD_STATE, fakes.SNAPD_STATE)  # Only the state.json identity is back.
        with self.assertRaises(ValueError):
            postscan_probe.check_os(vm_sanitize, g, "ubuntu-2404")


if __name__ == "__main__":
    unittest.main()
