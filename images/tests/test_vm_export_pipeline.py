"""Export driver (vm_export.pipeline) and public evidence (vm_export.evidence) with fakes.

No docker, no PVE, no network, no real disk image: the capsule launchers, the cold boot, the tool image
pull and the PVE client are faked; the copy guards, the bundle, the publisher's bundle check and the
evidence builder are the real code on synthetic bytes.
"""
import contextlib
import copy as copy_module
import datetime
import hashlib
import io
import json
import os
from pathlib import Path
import sys
import tempfile
import types
import unittest
from unittest.mock import patch

IMAGES = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(IMAGES))
from vm_export import capsule, coldboot, coldboot_guest, evidence, pipeline, postscan_probe  # noqa: E402
import vm_artifact  # noqa: E402
import vm_publish_stage  # noqa: E402
import vm_sanitize  # noqa: E402

SHA = "c" * 40
NOW = datetime.datetime(2026, 11, 14, 12, 0, 0, tzinfo=datetime.timezone.utc)
NODE, STORAGE, VMID = "labnode7", "pc-ci-images", 105
PRIVATE = ("labnode7", "192.0.2.50", "192.0.2.51", "192.0.2.1", "203.0.113.9", "images@pve!packer", "token-secret-value",
           "pc-templates", "import-store", "iso-store", "vmbr7", "/srv/", "/tmp/", "/system.slice", "pc-export-test", "d" * 64,
           "base-105-disk-0")
BUILT = 1794650400  # The template's creation time on the lab (`meta: ...,ctime=`), seconds.
ISO = "ubuntu-26.04.1-live-server-amd64.iso"
DISK = b"QFI\xfb" + bytes(4092)  # NOT a valid QCOW2; no image parser runs here.
VENDOR = "9" * 64


def github(**changes):
    env = {"EXPORT_VARIANT": "kubernetes", "EXPORT_OS": "ubuntu-2604", "EXPORT_KUBERNETES_VERSION": "1.37.2",
           "EXPORT_TEMPLATE_VMID": str(VMID), "GITHUB_REPOSITORY": "mutex-solutions/proxcloud",
           "GITHUB_REF": "refs/heads/main", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_SHA": SHA,
           "GITHUB_RUN_ID": "4242", "GITHUB_RUN_ATTEMPT": "1"}
    env.update(changes)
    return env


def secrets(mount, **changes):
    env = {"PVE_URL": "https://203.0.113.9:8006", "PVE_TOKEN_ID": "images@pve!packer", "PVE_TOKEN_SECRET": "token-secret-value",
           "PVE_NODES": NODE, "PVE_TEMPLATE_STORAGE": STORAGE, "PVE_IMPORT_STORAGE": "import-store",
           "PVE_ISO_STORAGE": "iso-store", "PVE_BRIDGE": "vmbr7", "EXPORT_VLAN": "242", "BUILD_GATEWAY": "192.0.2.1",
           "BUILD_NAMESERVER": "",
           "EXPORT_TEST_ADDRESSES": "192.0.2.50/24,192.0.2.51/24", "EXPORT_SOURCE_MOUNT": str(mount)}
    env.update(changes)
    return env


class FakePVE:
    """coldboot.Client's surface: call(method, path, **params) returning decoded data."""

    def __init__(self, name, tags, volume, imports=(), templates=(), isos=(ISO, "virtio-win.iso")):
        self.config = {"template": 1, "name": name, "tags": ";".join(tags), "digest": "a" * 40, "scsi0": volume + ",size=20G",
                       "meta": "creation-qemu=10.0.2,ctime=%d" % BUILT}
        self.imports, self.templates, self.isos, self.calls = list(imports), list(templates), list(isos), []
        self.held = BUILT - 3600  # When the node got the vendor file: before the template was built from it.

    def call(self, method, path, **params):
        self.calls.append((method, path))
        if method != "GET":
            raise AssertionError("the copy stage only reads")
        if params:  # As coldboot.Client: the parameters of a GET travel in the query string.
            path += "?" + "&".join("%s=%s" % item for item in params.items())
        if path.endswith("/config"):
            return dict(self.config)
        if path.endswith("/status/current"):
            return {"status": "stopped"}
        if "/tasks" in path:
            return []
        if path.startswith("/cluster/resources"):
            return [{"vmid": VMID, "pool": "pc-templates", "template": 1, "node": NODE, "name": self.config["name"],
                     "tags": self.config["tags"]}, *self.templates]
        if path.endswith("/content?content=import"):
            self.storages = getattr(self, "storages", []) + [path.split("/")[4]]
            return [{"volid": "import-store:import/" + name, "ctime": self.held} for name in self.imports]
        if path.endswith("/content?content=iso"):
            self.storages = getattr(self, "storages", []) + [path.split("/")[4]]
            return [{"volid": "iso-store:iso/" + name, "ctime": self.held} for name in self.isos]
        raise AssertionError(path)


class Lab:
    """A temp RUNNER_TEMP, a read-only-looking storage mount and fake launchers that write real-shaped records."""

    def __init__(self, case, **inputs):
        self.case = case
        self.tmp = tempfile.TemporaryDirectory()
        case.addCleanup(self.tmp.cleanup)
        root = Path(os.path.realpath(self.tmp.name))
        self.runner, self.mount, self.home = root / "runner", root / "mount", root / "home"
        (self.mount / "images" / str(VMID)).mkdir(parents=True)
        self.runner.mkdir()
        self.home.mkdir()
        self.work = self.runner / "vm-export"
        self.states = self.home / ".proxcloud-vm-export-coldboot"  # NOT under the runner's temp directory.
        self.env = dict(github(**inputs), **secrets(self.mount), RUNNER_TEMP=str(self.runner), HOME=str(self.home),
                        GITHUB_OUTPUT=str(root / "output.txt"), GITHUB_STEP_SUMMARY=str(root / "summary.md"))
        self.k8s = self.env["EXPORT_KUBERNETES_VERSION"] or None
        self.os = self.env["EXPORT_OS"]
        self.raw = bytes(1024) + b"template" + bytes(3064)  # 4096 bytes, mostly zeros (sparse copy path).
        if not self.k8s:  # A native template disk is a QCOW2 file: LARGER than its virtual size (metadata).
            self.raw += b"qcow2 metadata" + bytes(498)
        suffix = "raw" if self.k8s else "qcow2"
        volume = "%s:%d/base-%d-disk-0.%s" % (STORAGE, VMID, VMID, suffix)
        (self.mount / "images" / str(VMID) / ("base-%d-disk-0.%s" % (VMID, suffix))).write_bytes(self.raw)
        if self.k8s:
            self.api = FakePVE("ubuntu-2604-kube-v" + self.k8s, ["pc-k8s", "pc-k8s-" + self.k8s.replace(".", "-"),
                                                                 "pc-k8s-os-ubuntu-2604", "proxcloud"], volume)
        else:
            tags = ["proxcloud", "pc-public", "pc-img-" + self.os, "pc-v-202611140900"]
            self.api = FakePVE("pc-%s-202611140900" % self.os, tags, volume,
                               imports=["pc-%s-%s.qcow2" % (self.os, VENDOR[:12]), "pc-debian-13-000000000000.qcow2"],
                               templates=[{"vmid": 104, "pool": "pc-templates", "template": 1, "node": NODE,
                                           "tags": "proxcloud;pc-public;pc-img-%s;pc-v-202611010900" % self.os}])
        self.calls, self.fail = [], None
        self.source = "/input/source.raw" if self.k8s else "/input/source.qcow2"
        for target, fake in ((pipeline.inventory, self.inventory), (pipeline.review, self.review),
                             (pipeline.sanitize, self.sanitize), (pipeline.postscan, self.postscan),
                             (pipeline.coldboot, self.coldboot)):
            patcher = patch.object(target, "main", fake)
            patcher.start()
            case.addCleanup(patcher.stop)
        real = os.statvfs
        mount = str(self.mount)

        def statvfs(path):  # The temp "mount" reports read-only, like the NFS mount of the storage.
            found = real(path)
            if str(path) == mount:
                return types.SimpleNamespace(f_flag=found.f_flag | os.ST_RDONLY, f_bavail=found.f_bavail, f_frsize=found.f_frsize)
            return found
        import build
        for patcher in (patch.object(pipeline, "pve", lambda: self.api),
                        patch.object(capsule, "pull", self.pull), patch.object(pipeline.os, "statvfs", statvfs),
                        patch.object(pipeline.vm_sanitize, "VIRTUAL_BYTES", 4096),
                        patch.object(pipeline, "WORK_ROOM", 1 << 20),
                        patch.object(build, "checksum_of", lambda image: VENDOR)):
            patcher.start()
            case.addCleanup(patcher.stop)

    # --- fakes of the capsule launchers and the cold boot: argv in, real-shaped records out ---
    def option(self, argv, name):
        return argv[argv.index(name) + 1]

    def run_record(self, check, pins, **more):
        return dict({"check_id": check, "image": capsule.IMAGE_IDS[0], "exit_code": 0, "attach_exit_code": 0,
                     "stop_reason": None, "cgroup_empty_or_absent": True, "cleanup_errors": [], "stderr_bytes_discarded": 12,
                     "container_id": "d" * 64, "known_cgroup": "/system.slice/docker-%s.scope" % ("d" * 64),
                     "pins": pins}, **more)

    def pins(self, *names, **fixed):
        found = {"/bootstrap/" + name: hashlib.sha256(name.encode()).hexdigest() for name in names}
        found.update(fixed)
        return found

    def sha(self, path):
        return hashlib.sha256(Path(path).read_bytes()).hexdigest()

    def pull(self, config_dir):
        self.calls.append(("tools", [str(config_dir)]))
        self.case.assertEqual(os.listdir(config_dir), [])
        return capsule.IMAGE_IDS[0]

    def finish(self, name, argv):
        self.calls.append((name, list(argv)))
        return 1 if self.fail == name else 0

    def inventory(self, argv):
        out = Path(self.option(argv, "--out-dir"))
        row = {"phase": "k8s-readonly-inventory-v1" if self.k8s else "os-readonly-inventory-v1", "sanitize_eligible": True,
               "guest_account": "builder"}
        data = (json.dumps(row, sort_keys=True) + "\n").encode()
        (out / "inventory.json").write_bytes(data)
        check = "k8s-inventory-" + self.k8s if self.k8s else self.os + "-inventory"
        pins = self.pins("inspect_k8s.py" if self.k8s else "inspect_os.py", "vm_sanitize.py", "k8s_copy_policy.py",
                         **{self.source: self.sha(self.option(argv, "--source"))})
        pipeline.write(out / "inventory-run.json", self.run_record(
            check, pins, clean=True, accepted=True, inventory_sha256=hashlib.sha256(data).hexdigest(),
            result={"phase": row["phase"]}))
        return self.finish("inventory", argv)

    def review(self, argv):
        receipt = pipeline.load(self.option(argv, "--copy-receipt"))
        Path(self.option(argv, "--out")).write_text(json.dumps(
            {"kubernetes_version": self.k8s, "source_raw_sha256": receipt["raw_sha256"], "package_policy_sha256": "5" * 64,
             "cloud_default_user": "ubuntu"}, sort_keys=True, separators=(",", ":")))
        return self.finish("review", argv)

    def facts(self):
        facts = {"kind": "OfflineSanitizationFacts", "os": self.os, "variant": "kubernetes" if self.k8s else "os",
                 "disk_sha256": hashlib.sha256(DISK).hexdigest(), "disk_bytes": len(DISK), "virtual_size_bytes": 20 << 30,
                 "checks": dict.fromkeys(vm_artifact.CHECKS, True)}
        return dict(facts, kubernetes_version=self.k8s) if self.k8s else facts

    def sanitize(self, argv):
        out = Path(self.option(argv, "--out-dir"))
        (out / "disk.qcow2").write_bytes(DISK)
        os.chmod(out / "disk.qcow2", 0o400)
        pins = self.pins("stream_sanitized.py", "vm_sanitize.py", "k8s_copy_policy.py",
                         **{self.source: self.sha(self.option(argv, "--source"))})
        if self.k8s:
            pins["/input/k8s-review.json"] = self.sha(self.option(argv, "--review"))
        check = "sanitize-" + self.os + ("-k8s-" + self.k8s if self.k8s else "")
        pipeline.write(out / "sanitize-run.json", self.run_record(
            check, pins, private_bytes=len(DISK), sealed_after_cgroup_stop=True, facts=self.facts(),
            disk=str(out / "disk.qcow2"), public_eligible=False))
        return self.finish("sanitize", argv)

    def postscan(self, argv):
        out = Path(self.option(argv, "--out-dir"))
        pins = self.pins("postscan_probe.py", "vm_sanitize.py", "k8s_copy_policy.py",
                         **{"/input/source.qcow2": self.sha(self.option(argv, "--disk"))})
        if self.k8s:
            pins["/input/k8s-review.json"] = self.sha(self.option(argv, "--review"))
        variant, value = ("kubernetes", self.k8s) if self.k8s else ("os", self.os)
        pipeline.write(out / "postscan-run.json", self.run_record(
            "postscan-" + ("k8s-" + self.k8s if self.k8s else self.os), pins, clean=True,
            result=postscan_probe.expected(variant, value), disk_sha256=pins["/input/source.qcow2"]))
        return self.finish("postscan", argv)

    def coldboot(self, argv):
        if argv[0] == "cleanup":  # `recover`: the real cleanup would call PVE; a state recorded clean is clean.
            self.calls.append(("cleanup", list(argv)))
            try:
                return 0 if pipeline.load(Path(argv[2]) / coldboot.STATE).get("clean") is True else 1
            except (ValueError, AttributeError):
                return 2
        state = Path(self.option(argv, "--state-dir"))
        self.case.assertEqual((state.parent, state.stat().st_mode & 0o777, os.listdir(state)), (self.states, 0o700, []))
        run = pipeline.load(self.option(argv, "--sanitize-run"))
        names = coldboot_guest.COMMON + (coldboot_guest.K8S if self.k8s else coldboot_guest.OS_ONLY)

        def clone(suffix):
            return {"suffix": suffix, "name": "pc-export-test-12345678-0123456789ab-" + suffix, "all_passed": True,
                    "checks": dict.fromkeys(names, True), "machine_id_sha256": suffix * 64, "host_key_sha256": suffix * 64,
                    "snapd_present": True, "snapd_serial_sha256": suffix * 64, "snapd_device_key_sha256": suffix * 64,
                    "snapd_changes_error_lines": 0, "snapd_snaps": 3, "cloud_init_recoverable_warnings": 1}
        pipeline.write(self.option(argv, "--out"), {
            "kind": "ColdBootAcceptance", "accepted": True, "reason": None, "step": "done",
            "variant": "kubernetes" if self.k8s else "os", "os": self.os, "kubernetes_version": self.k8s,
            "pve_tls_verified": False, "api_node_room": "checked", "disk_sha256": run["facts"]["disk_sha256"],
            "disk_bytes": run["facts"]["disk_bytes"], "run": "0123456789ab", "import_volume": "pc-export-0123-0123456789ab.qcow2",
            "clones": [clone("a"), clone("b")],
            "distinct_identity": dict.fromkeys(coldboot.IDENTITY + ("snapd_present",), True),
            "cleanup": {"clones": {"a": "destroyed", "b": "destroyed"}, "import_volume": "deleted", "private_key_deleted": True},
            "protected_set": {"scope": coldboot.SCOPE, "guests": 7, "unchanged": True},
            "storage_set": {"volumes": 31, "unchanged": True}})
        pipeline.write(state / coldboot.STATE, {"clean": self.fail != "cleanup"})
        return max(self.finish("coldboot", argv), int(self.fail == "cleanup"))  # Not accepted unless cleaned up.

    def stage(self, name, env=None):
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err), \
                patch.object(pipeline.datetime, "datetime", FixedNow):
            code = pipeline.main([name], self.env if env is None else env)
        self.stdout, self.stderr = out.getvalue(), err.getvalue()
        self.printed = dict(getattr(self, "printed", {}), **{name: self.stdout})
        return code

    def through(self, last):
        for name in pipeline.STAGES[:pipeline.STAGES.index(last) + 1]:
            self.case.assertEqual(self.stage(name), 0, (name, getattr(self, "stderr", "")))


class FixedNow(datetime.datetime):
    @classmethod
    def now(cls, tz=None):
        return NOW


class PlanTests(unittest.TestCase):
    def test_valid_inputs_name_the_bundle_the_publisher_expects(self):
        k8s = pipeline.plan(github(), NOW)
        self.assertEqual(k8s, {"variant": "kubernetes", "os": "ubuntu-2604", "kubernetes_version": "1.37.2",
                               "template_vmid": 105, "source_commit": SHA, "run_id": 4242, "run_attempt": 1,
                               "build_version": "20261114120000",
                               "artifact_name": "vm-k8s-export-ubuntu-2604-20261114120000-cccccccccccc"})
        native = pipeline.plan(github(EXPORT_VARIANT="os", EXPORT_OS="ubuntu-2404", EXPORT_KUBERNETES_VERSION=""), NOW)
        self.assertEqual(native, pipeline.plan(github(EXPORT_VARIANT="os", EXPORT_OS="ubuntu-2404",
                                                      EXPORT_KUBERNETES_VERSION="none"), NOW))
        self.assertEqual((native["artifact_name"], native["kubernetes_version"]),
                         ("vm-export-ubuntu-2404-20261114120000-cccccccccccc", None))
        for selected in (k8s, native):
            self.assertEqual(vm_publish_stage.selector(selected["artifact_name"]),
                             (selected["os"], "20261114120000", SHA[:12], selected["variant"]))
        self.assertEqual(pipeline.OSES, ("ubuntu-2404", "ubuntu-2604"))
        # The build version is the export run's own UTC time, to the second.
        with patch.object(pipeline.datetime, "datetime", FixedNow):
            self.assertEqual(pipeline.plan(github())["build_version"], "20261114120000")

    def test_every_input_is_validated_and_nothing_else_is_read(self):
        for changes in ({"EXPORT_VARIANT": "k8s"}, {"EXPORT_VARIANT": ""}, {"EXPORT_OS": "debian-13"},
                        {"EXPORT_OS": "ubuntu-2604; id"}, {"EXPORT_KUBERNETES_VERSION": ""}, {"EXPORT_KUBERNETES_VERSION": "none"}, {"EXPORT_KUBERNETES_VERSION": "None"},
                        {"EXPORT_KUBERNETES_VERSION": "1.38.0"}, {"EXPORT_KUBERNETES_VERSION": "1.37.2\n"},
                        {"EXPORT_OS": "ubuntu-2404"},  # Kubernetes node templates are Ubuntu 26.04 only.
                        {"EXPORT_VARIANT": "os"},  # A version is forbidden for a native export.
                        {"EXPORT_TEMPLATE_VMID": ""}, {"EXPORT_TEMPLATE_VMID": "0105"}, {"EXPORT_TEMPLATE_VMID": "99"},
                        {"EXPORT_TEMPLATE_VMID": "105 "}, {"EXPORT_TEMPLATE_VMID": "-105"}, {"EXPORT_TEMPLATE_VMID": "1e3"},
                        {"EXPORT_TEMPLATE_VMID": "1" * 10}, {"EXPORT_TEMPLATE_VMID": "105/../1"},
                        {"EXPORT_TEMPLATE_VMID": "9290"}, {"EXPORT_TEMPLATE_VMID": "9299"},
                        {"EXPORT_TEMPLATE_VMID": "١٠٥"},
                        {"GITHUB_REPOSITORY": "attacker/proxcloud"}, {"GITHUB_REF": "refs/heads/feature"},
                        {"GITHUB_REF": "refs/tags/v1"}, {"GITHUB_EVENT_NAME": "pull_request"}, {"GITHUB_EVENT_NAME": "push"},
                        {"GITHUB_SHA": "c" * 39}, {"GITHUB_SHA": "C" * 40}, {"GITHUB_RUN_ID": "0"}, {"GITHUB_RUN_ID": ""},
                        {"GITHUB_RUN_ATTEMPT": "0"}, {"GITHUB_RUN_ATTEMPT": "x"}):
            with self.subTest(changes=changes), self.assertRaises(pipeline.Refused):
                pipeline.plan(github(**changes), NOW)
        # A re-run exports at the commit of the first attempt, however old: only a new dispatch exports.
        for attempt in ("2", "17"):
            with self.subTest(attempt=attempt), self.assertRaises(pipeline.Refused) as caught:
                pipeline.plan(github(GITHUB_RUN_ATTEMPT=attempt), NOW)
            self.assertEqual(str(caught.exception), "rerun_refused_dispatch_a_new_run")
        self.assertEqual(pipeline.plan(github(EXPORT_TEMPLATE_VMID="9289"), NOW)["template_vmid"], 9289)
        self.assertEqual(pipeline.CLONE_VMIDS, (9290, 9299))  # coldboot's default reserved range.
        self.assertIn('default="9290-9299"', (IMAGES / "vm_export" / "coldboot.py").read_text())

        class NoSecrets(dict):  # The plan never asks for a secret or a variable.
            def get(self, key, default=None):
                if not key.startswith(("EXPORT_VARIANT", "EXPORT_OS", "EXPORT_KUBERNETES", "EXPORT_TEMPLATE", "GITHUB_")):
                    raise AssertionError(key)
                return super().get(key, default)
        pipeline.plan(NoSecrets(github()), NOW)


class SettingsTests(unittest.TestCase):
    def setUp(self):
        self.plan = pipeline.plan(github(), NOW)

    def test_valid_configuration_and_the_masks_of_its_parts(self):
        cfg = pipeline.settings(secrets("/srv/pc-ci-images"), self.plan)
        self.assertEqual(cfg, {"node": NODE, "storage": STORAGE, "import_storage": "import-store", "iso_storage": "iso-store",
                               "bridge": "vmbr7", "vlan": 242, "mount": "/srv/pc-ci-images", "gateway": "192.0.2.1",
                               "addresses": ["192.0.2.50/24", "192.0.2.51/24"], "nameservers": ["192.0.2.1"]})
        self.assertEqual(pipeline.masks(cfg), ["::add-mask::" + v for v in (
            NODE, "192.0.2.1", "192.0.2.50/24", "192.0.2.51/24", "192.0.2.50", "192.0.2.51")])
        defaults = secrets("/x", EXPORT_SOURCE_MOUNT="", EXPORT_VLAN="", PVE_ISO_STORAGE="", PVE_IMPORT_STORAGE="",
                           BUILD_NAMESERVER="198.51.100.1 198.51.100.2")
        cfg = pipeline.settings(defaults, self.plan)
        # The ISO store falls back to the template storage, as in the node template builder.
        self.assertEqual((cfg["mount"], cfg["vlan"], cfg["nameservers"], cfg["iso_storage"], cfg["import_storage"]),
                         ("/srv/pc-ci-images", 0, ["198.51.100.1", "198.51.100.2"], STORAGE, ""))
        self.assertIn('PVE_ISO_STORAGE="${PVE_ISO_STORAGE:-$PVE_TEMPLATE_STORAGE}"', (IMAGES / "k8s-node-template.sh").read_text())
        # Each name server is masked too (GitHub masks a secret as a whole, not its parts).
        self.assertEqual(pipeline.masks(cfg), ["::add-mask::" + v for v in (
            NODE, "192.0.2.1", "198.51.100.1", "198.51.100.2", "192.0.2.50/24", "192.0.2.51/24", "192.0.2.50", "192.0.2.51")])
        # A value pasted with a trailing newline or space is the value: used stripped, everywhere.
        padded = {key: value + " \n" for key, value in secrets("/srv/pc-ci-images").items()}
        self.assertEqual(pipeline.settings(padded, self.plan), pipeline.settings(secrets("/srv/pc-ci-images"), self.plan))
        # The VLAN is not read from the secret the template builds use: a masked short number hides job outputs.
        self.assertEqual(pipeline.settings(secrets("/x", EXPORT_VLAN="", PVE_BUILD_VLAN="77"), self.plan)["vlan"], 0)
        # The import storage is a native export's need only.
        native = pipeline.plan(github(EXPORT_VARIANT="os", EXPORT_KUBERNETES_VERSION=""), NOW)
        with self.assertRaises(pipeline.Refused) as caught:
            pipeline.settings(secrets("/x", PVE_IMPORT_STORAGE=" "), native)
        self.assertEqual(str(caught.exception), "config_missing_PVE_IMPORT_STORAGE")
        self.assertEqual(pipeline.settings(secrets("/x"), native)["import_storage"], "import-store")

    def test_a_secret_that_would_hide_a_job_output_is_refused_before_any_work(self):
        """GitHub drops a job output that contains a masked value: the publisher would get empty inputs after
        the whole export. A secret that can occur in a hex digest or in the artifact name refuses at once."""
        self.assertEqual(self.plan["artifact_name"], "vm-k8s-export-ubuntu-2604-20261114120000-cccccccccccc")
        for key, value in (("PVE_NODES", "a1"), ("PVE_NODES", "cafe"), ("PVE_TEMPLATE_STORAGE", "26"), ("PVE_BRIDGE", "0"),
                           ("PVE_IMPORT_STORAGE", "deadbeef"), ("PVE_ISO_STORAGE", "f" * 16), ("PVE_NODES", "ubuntu"),
                           ("PVE_TEMPLATE_STORAGE", "export"), ("PVE_BRIDGE", "vm"), ("PVE_ISO_STORAGE", "k8s"),
                           ("PVE_TOKEN_SECRET", "2604"), ("PVE_TOKEN_ID", "c"), ("BUILD_NAMESERVER", "1114")):
            with self.subTest(key=key, value=value), self.assertRaises(pipeline.Refused) as caught:
                pipeline.settings(secrets("/srv/x", **{key: value}), self.plan)
            self.assertEqual(str(caught.exception), "config_%s_would_hide_a_job_output" % key)
        # Longer than any chance match, or with a character no digest and no name contains: accepted.
        for key, value in (("PVE_ISO_STORAGE", "f" * 17), ("PVE_TEMPLATE_STORAGE", "ceph-vm"), ("PVE_NODES", "pve"),
                           ("PVE_BRIDGE", "vmbr0")):
            pipeline.settings(secrets("/srv/x", **{key: value}), self.plan)
        self.assertEqual(sorted(pipeline.REQUIRED + pipeline.OPTIONAL), sorted(
            key for key in secrets("/x") if key not in ("EXPORT_VLAN", "EXPORT_SOURCE_MOUNT")))

    def test_a_refusal_names_the_variable_never_its_value(self):
        for key in pipeline.REQUIRED:
            for value in ("", "   "):
                with self.subTest(key=key), self.assertRaises(pipeline.Refused) as caught:
                    pipeline.settings(secrets("/srv/x", **{key: value}), self.plan)
                self.assertEqual(str(caught.exception), "config_missing_" + key)
        cases = (({"PVE_NODES": "node1 node2"}, "config_PVE_NODES_must_be_one_node"),
                 ({"PVE_NODES": "Node_1"}, "config_PVE_NODES_must_be_one_node"),
                 ({"EXPORT_VLAN": "4095"}, "config_EXPORT_VLAN"), ({"EXPORT_VLAN": "-1"}, "config_EXPORT_VLAN"),
                 ({"PVE_TEMPLATE_STORAGE": "pc ci"}, "config_cold_boot_settings"),
                 ({"EXPORT_SOURCE_MOUNT": "srv/x"}, "config_EXPORT_SOURCE_MOUNT"),
                 ({"EXPORT_SOURCE_MOUNT": "/srv/../etc"}, "config_EXPORT_SOURCE_MOUNT"),
                 ({"EXPORT_SOURCE_MOUNT": "/srv/a b"}, "config_EXPORT_SOURCE_MOUNT"),
                 ({"EXPORT_TEST_ADDRESSES": "192.0.2.50/24"}, "config_cold_boot_settings"),
                 ({"EXPORT_TEST_ADDRESSES": "192.0.2.50/24,192.0.2.50/24"}, "config_cold_boot_settings"),
                 ({"EXPORT_TEST_ADDRESSES": "192.0.2.50/24,198.51.100.51/24"}, "config_cold_boot_settings"),
                 ({"EXPORT_TEST_ADDRESSES": "192.0.2.50/24,192.0.2.51/24,192.0.2.52/24"}, "config_cold_boot_settings"),
                 ({"EXPORT_TEST_ADDRESSES": "192.0.2.50,192.0.2.1/24"}, "config_cold_boot_settings"),
                 ({"BUILD_GATEWAY": "198.51.100.1"}, "config_cold_boot_settings"),
                 ({"BUILD_NAMESERVER": "not-an-address"}, "config_cold_boot_settings"),
                 ({"PVE_BRIDGE": "vmbr0,firewall=1"}, "config_cold_boot_settings"))
        for changes, code in cases:
            with self.subTest(changes=changes), self.assertRaises(pipeline.Refused) as caught:
                pipeline.settings(secrets("/srv/x", **changes), self.plan)
            self.assertEqual(str(caught.exception), code)
            for value in changes.values():
                self.assertNotIn(value, str(caught.exception))


class SourceTests(unittest.TestCase):
    def test_template_identity_follows_the_builders_names_and_tags(self):
        plan = pipeline.plan(github(), NOW)
        self.assertEqual(pipeline.template_identity(plan, None, NODE), (
            "ubuntu-2604-kube-v1.37.2", ["proxcloud", "pc-k8s", "pc-k8s-1-37-2", "pc-k8s-os-ubuntu-2604"]))
        script = (IMAGES / "k8s-node-template.sh").read_text()
        for item in ('name="$K8S_OS-kube-v$K8S_VERSION"', 'tag="pc-k8s-${K8S_VERSION//./-}"', 'os_tag="pc-k8s-os-$K8S_OS"',
                     '"tags=proxcloud;pc-k8s;$tag;$os_tag"'):
            self.assertIn(item, script)
        native = pipeline.plan(github(EXPORT_VARIANT="os", EXPORT_KUBERNETES_VERSION=""), NOW)
        client = types.SimpleNamespace(get=lambda path: {"name": "pc-ubuntu-2604-202611140900"})
        self.assertEqual(pipeline.template_identity(native, client, NODE), (
            "pc-ubuntu-2604-202611140900", ["proxcloud", "pc-public", "pc-img-ubuntu-2604", "pc-v-202611140900"]))
        self.assertIn('tags="proxcloud;pc-public;pc-img-%s;pc-v-%s" % (a.os, version)', (IMAGES / "build.py").read_text())
        for name in ("pc-ubuntu-2404-202611140900", "pc-ubuntu-2604-20261114090", "pc-ubuntu-2604-202611140900-x",
                     "ubuntu-2604-kube-v1.37.2", None):
            client = types.SimpleNamespace(get=lambda path, name=name: {"name": name})
            with self.subTest(name=name), self.assertRaises(pipeline.Refused):
                pipeline.template_identity(native, client, NODE)

    def test_kubernetes_upstream_is_the_pinned_installer_iso_the_node_held_before_the_build(self):
        plan = pipeline.plan(github(), NOW)
        cfg = {"node": NODE, "iso_storage": "iso-store"}
        name = "ubuntu-2604-kube-v1.37.2"
        api = FakePVE(name, [], "x")
        checksum, url = pipeline.upstream(plan, api, cfg, name)
        self.assertRegex(checksum, r"^[0-9a-f]{64}$")
        self.assertEqual(url, "https://releases.ubuntu.com/26.04/" + ISO)
        self.assertEqual(api.storages, ["iso-store"])
        source = (IMAGES / "k8s-build-config.py").read_text()
        self.assertIn('"iso_sha256": "%s"' % checksum, source)
        self.assertIn('"iso_name": "%s"' % ISO, source)
        vm_artifact.request_valid(pipeline.bundle.request("ubuntu-2604", SHA, checksum, "20261114120000", "1.37.2", url))
        # The pin is read at the export commit: after a pin bump the node holds no such ISO yet, or one that
        # arrived after this template was built. Either refuses instead of attributing the new ISO.
        self.assertEqual(pipeline.CLOCK_SKEW, 300)
        for change, code in ((dict(isos=["ubuntu-26.04.2-live-server-amd64.iso"]), "upstream_image_is_not_the_built_one"),
                             (dict(isos=[]), "upstream_image_is_not_the_built_one"),
                             (dict(held=BUILT + 301), "upstream_image_newer_than_the_template"),
                             (dict(held=None), "upstream_image_time_unknown"), (dict(held=True), "upstream_image_time_unknown"),
                             (dict(held="1794650400"), "upstream_image_time_unknown"), (dict(held=0), "upstream_image_time_unknown"),
                             (dict(meta=None), "upstream_image_time_unknown"), (dict(meta="creation-qemu=10.0.2"), "upstream_image_time_unknown"),
                             (dict(meta="creation-qemu=10.0.2,xctime=1"), "upstream_image_time_unknown")):
            api = FakePVE(name, [], "x")
            if "meta" in change:
                api.config["meta"] = change["meta"]
            else:
                vars(api).update(change)
            with self.subTest(change=change), self.assertRaises(pipeline.Refused) as caught:
                pipeline.upstream(plan, api, cfg, name)
            self.assertEqual(str(caught.exception), code)
        for held, meta in ((BUILT + 300, None), (1, "ctime=%d" % BUILT), (BUILT, "ctime=%d,creation-qemu=10.0.2" % BUILT)):
            api = FakePVE(name, [], "x")
            api.held = held
            api.config["meta"] = meta or api.config["meta"]
            pipeline.upstream(plan, api, cfg, name)

    def test_native_upstream_must_be_the_image_the_node_holds_for_the_newest_template(self):
        import build
        plan = pipeline.plan(github(EXPORT_VARIANT="os", EXPORT_KUBERNETES_VERSION=""), NOW)
        cfg = {"node": NODE, "import_storage": "import-store"}
        name = "pc-ubuntu-2604-202611140900"
        tags = "proxcloud;pc-public;pc-img-ubuntu-2604;pc-v-"

        def api(imports, templates=()):
            fake = FakePVE(name, (tags + "202611140900").split(";"), "x", imports, templates)
            return fake
        held = ["pc-ubuntu-2604-%s.qcow2" % VENDOR[:12], "pc-ubuntu-2404-aaaaaaaaaaaa.qcow2"]
        with patch.object(build, "checksum_of", lambda image: VENDOR):
            self.assertEqual(pipeline.upstream(plan, api(held), cfg, name), (VENDOR, build.IMAGES["ubuntu-2604"]["url"]))
            # A later build (even one that failed) replaced the vendor download: the checksum published now is
            # not the one of the image this older template was built from.
            later = api(held)
            later.held = BUILT + 86400
            with self.assertRaises(pipeline.Refused) as caught:
                pipeline.upstream(plan, later, cfg, name)
            self.assertEqual(str(caught.exception), "upstream_image_newer_than_the_template")
            self.assertEqual(later.storages, ["import-store"])
            newer = {"vmid": 106, "pool": "pc-templates", "template": 1, "node": NODE, "tags": tags + "202611150900"}
            for imports, templates, code in (
                    ([], (), "upstream_image_is_not_the_built_one"),
                    (["pc-ubuntu-2604-000000000000.qcow2"], (), "upstream_image_is_not_the_built_one"),
                    (held + ["pc-ubuntu-2604-000000000000.qcow2"], (), "upstream_image_is_not_the_built_one"),
                    (held, (newer,), "template_not_newest")):
                with self.subTest(imports=imports, code=code), self.assertRaises(pipeline.Refused) as caught:
                    pipeline.upstream(plan, api(imports, templates), cfg, name)
                self.assertEqual(str(caught.exception), code)
        for failure in (SystemExit("no checksum for https://vendor"), OSError("dns"), lambda image: "not-hex"):
            fake = failure if callable(failure) else (lambda image, failure=failure: (_ for _ in ()).throw(failure))
            with self.subTest(failure=failure), patch.object(build, "checksum_of", fake), \
                    self.assertRaises(pipeline.Refused) as caught:
                pipeline.upstream(plan, api(held), cfg, name)
            self.assertEqual(str(caught.exception), "upstream_checksum_unavailable")

    def test_os_gate_binds_inventory_run_and_receipt(self):
        data = b'{"sanitize_eligible": true}\n'
        run = {"clean": True, "accepted": True, "stop_reason": None, "cleanup_errors": [], "cgroup_empty_or_absent": True,
               "inventory_sha256": hashlib.sha256(data).hexdigest(), "pins": {"/input/source.qcow2": "a" * 64}}
        receipt = {"kind": "TemplateDiskCopy", "template_unchanged_stopped": True, "raw_sha256": "a" * 64}
        pipeline.os_gate(data, run, receipt)
        cases = [(b'{"sanitize_eligible": false}\n', run, receipt), (b"not json", run, receipt),
                 (data, dict(run, clean=False), receipt), (data, dict(run, accepted=False), receipt),
                 (data, dict(run, stop_reason="parent_wall_limit"), receipt),
                 (data, dict(run, cleanup_errors=["docker-stop-exit"]), receipt),
                 (data, dict(run, cgroup_empty_or_absent=False), receipt), (data, dict(run, inventory_sha256="0" * 64), receipt),
                 (data, dict(run, pins={"/input/source.raw": "a" * 64}), receipt),
                 (data, run, dict(receipt, raw_sha256="b" * 64)), (data, run, dict(receipt, template_unchanged_stopped=False)),
                 (data, run, dict(receipt, kind="Other")), (data, {}, receipt), (data, run, None)]
        for inventory_bytes, record, copied in cases:
            with self.subTest(record=record, copied=copied), self.assertRaises(pipeline.Refused):
                pipeline.os_gate(inventory_bytes, record, copied)


class ChainTests(unittest.TestCase):
    def test_kubernetes_run_orders_the_stages_and_binds_each_to_the_previous_record(self):
        lab = Lab(self)
        lab.through("evidence")
        work = lab.work
        self.assertEqual([name for name, _ in lab.calls], ["tools", "inventory", "review", "sanitize", "postscan", "coldboot"])
        self.assertEqual(pipeline.load(work / "stages.json"), list(pipeline.STAGES))
        argv = dict(lab.calls)
        source, receipt, review = (str(work / "private" / "source.raw"), str(work / "private" / "copy-receipt.json"),
                                   str(work / "review" / "k8s-review.json"))
        scratch = ["--scratch-base", str(work / "scratch")]
        policy = json.loads((IMAGES / "vm_export" / "baselines" / "ubuntu-2604-k8s.json").read_text())["package_policy"]
        self.assertEqual(argv["tools"], [str(work / "docker-anonymous")])
        self.assertEqual(argv["inventory"], [
            "k8s", "--kubernetes-version", "1.37.2", "--containerd-version", policy["containerd"], "--runc-version",
            policy["runc"], "--source", source, "--copy-receipt", receipt, *scratch, "--out-dir", str(work / "inventory")])
        self.assertEqual(argv["review"], [
            "derive", "--baseline", str(IMAGES / "vm_export" / "baselines" / "ubuntu-2604-k8s.json"),
            "--kubernetes-version", "1.37.2", "--inventory-run", str(work / "inventory" / "inventory-run.json"),
            "--copy-receipt", receipt, "--out", review, "--inventory", str(work / "inventory" / "inventory.json")])
        self.assertEqual(argv["sanitize"], ["k8s", "--kubernetes-version", "1.37.2", "--review", review, "--source", source,
                                            "--copy-receipt", receipt, *scratch, "--out-dir", str(work / "sanitize"),
                                            "--wall-seconds", "3600"])
        self.assertEqual(argv["postscan"], [
            "k8s", "--kubernetes-version", "1.37.2", "--review", review, "--disk", str(work / "sanitize" / "disk.qcow2"),
            "--sanitize-run", str(work / "sanitize" / "sanitize-run.json"), *scratch, "--out-dir", str(work / "postscan")])
        self.assertEqual(argv["coldboot"], [
            "run", "--sanitize-run", str(work / "sanitize" / "sanitize-run.json"), "--variant", "kubernetes", "--os",
            "ubuntu-2604", "--node", NODE, "--storage", STORAGE, "--bridge", "vmbr7", "--vlan", "242", "--gateway",
            "192.0.2.1", "--vmid-range", "9290-9299", "--state-dir", str(lab.states / "4242-1"), "--out",
            str(work / "coldboot" / "coldboot-result.json"), "--kubernetes-version",
            "1.37.2", "--test-address", "192.0.2.50/24", "--test-address", "192.0.2.51/24", "--nameserver", "192.0.2.1"])
        # The state of the cold boot is outside the runner's temp directory; its result is a work file.
        self.assertEqual(os.listdir(lab.states), ["4242-1"])
        self.assertEqual(os.listdir(work / "coldboot"), ["coldboot-result.json"])
        self.assertNotIn(str(lab.runner), str(lab.states))
        # The real launchers parse exactly these argument lists.
        for name, module in (("inventory", pipeline.inventory), ("sanitize", pipeline.sanitize), ("postscan", pipeline.postscan)):
            self.assertIn('add_parser("k8s")', Path(module.__file__).read_text(), name)
        # The copy: the real guards, a read-only file whose hash the receipt records, the template untouched.
        copied = pipeline.load(receipt)
        self.assertEqual((copied["raw_sha256"], copied["raw_bytes"], copied["template_unchanged_stopped"]),
                         (hashlib.sha256(lab.raw).hexdigest(), len(lab.raw), True))
        self.assertEqual(copied["template"], {"node": NODE, "vmid": VMID, "name": "ubuntu-2604-kube-v1.37.2",
                                              "digest": "a" * 40, "volume": "pc-ci-images:105/base-105-disk-0.raw"})
        self.assertEqual(Path(source).read_bytes(), lab.raw)
        self.assertEqual(os.stat(source).st_mode & 0o777, 0o400)
        self.assertTrue(all(method == "GET" for method, _ in lab.api.calls))
        # The bundle: exactly three files, accepted by the publisher's own check with the recorded hashes.
        self.assertEqual(sorted(os.listdir(work / "bundle")), ["SHA256SUMS", "disk.qcow2", "manifest.json"])
        outputs = dict(line.split("=", 1) for line in Path(lab.env["GITHUB_OUTPUT"]).read_text().splitlines())
        manifest = json.loads((work / "bundle" / "manifest.json").read_bytes())
        self.assertEqual({key: value for key, value in outputs.items() if key != "evidence_sha256"},
                         {"artifact_name": "vm-k8s-export-ubuntu-2604-20261114120000-cccccccccccc",
                          "manifest_sha256": lab.sha(work / "bundle" / "manifest.json"),
                          "disk_sha256": hashlib.sha256(DISK).hexdigest()})
        chosen = vm_publish_stage.inputs("4242", "1", outputs["artifact_name"], outputs["manifest_sha256"],
                                         outputs["disk_sha256"], SHA, vm_publish_stage.REPOSITORY, vm_publish_stage.WORKFLOW_REF,
                                         "kubernetes", "ubuntu-2604", "1.37.2")
        plan = vm_publish_stage.publication_plan(str(work / "bundle"), chosen)
        # The dispatch decides what the bundle may be: the same bytes are refused for any other dispatch.
        for dispatched in (("os", "ubuntu-2604", "none"), ("kubernetes", "ubuntu-2604", "1.36.6"), ("kubernetes", "ubuntu-2404", "1.37.2")):
            with self.subTest(dispatched=dispatched), self.assertRaises(ValueError):
                vm_publish_stage.publication_plan(str(work / "bundle"), vm_publish_stage.inputs(
                    "4242", "1", outputs["artifact_name"], outputs["manifest_sha256"], outputs["disk_sha256"], SHA,
                    vm_publish_stage.REPOSITORY, vm_publish_stage.WORKFLOW_REF, *dispatched))
        self.assertEqual(plan["reference"], "ghcr.io/mutex-solutions/proxcloud-vm-ubuntu-2604-k8s:"
                                            "ubuntu-2604-k8s-1.37.2-20261114120000-cccccccccccc")
        receipt_row = manifest["clean_export_receipt"]
        self.assertEqual((manifest["build_version"], manifest["source_commit"], receipt_row["run_ref"]),
                         ("20261114120000", SHA, "https://github.com/mutex-solutions/proxcloud/actions/runs/4242"))
        self.assertEqual(receipt_row["source_identity"], pipeline.bundle.source_identity(4242, copied["raw_sha256"]))
        self.assertEqual(manifest["provenance"], {"upstream_url": "https://releases.ubuntu.com/26.04/ubuntu-26.04.1-live-server-amd64.iso"})
        # The evidence: ONE json and nothing else, valid, with nothing private in it or in the summary.
        self.assertEqual(os.listdir(work / "evidence"), ["vm-export-evidence.json"])
        text = (work / "evidence" / "vm-export-evidence.json").read_text()
        record = json.loads(text)
        evidence.check(record, dict(pipeline.load(work / "plan.json"), tool_image_id=capsule.IMAGE_IDS[0]))
        self.assertEqual((record["reference"], record["bundle"]["manifest_sha256"], record["run_id"]),
                         (plan["reference"], outputs["manifest_sha256"], 4242))
        # The log and the summary of this job are masked, the artifact is not: the record itself is NOT printed
        # here (a masked copy next to the unmasked one shows where a masked value occurs in public data). The
        # summary and every stage's output are exactly these lines: no plan (it holds the VMID), no receipt.
        digest = hashlib.sha256(text.encode()).hexdigest()
        self.assertEqual(Path(lab.env["GITHUB_STEP_SUMMARY"]).read_text(), (
            "## VM export staged (NOT yet published)\n\nEvidence record `vm-export-evidence.json`, sha256 `%s`: uploaded as "
            "the artifact `vm-k8s-export-ubuntu-2604-20261114120000-cccccccccccc-evidence`; the `verify` job prints it.\n" % digest))
        outputs = dict(line.split("=", 1) for line in Path(lab.env["GITHUB_OUTPUT"]).read_text().splitlines())
        self.assertEqual(sorted(outputs), ["artifact_name", "disk_sha256", "evidence_sha256", "manifest_sha256"])
        self.assertEqual(outputs["evidence_sha256"], digest)

        def line(**row):
            return json.dumps(row, sort_keys=True) + "\n"
        cfg = pipeline.settings(lab.env, pipeline.load(work / "plan.json"))
        self.assertEqual(lab.printed, {
            "plan": line(artifact_name=outputs["artifact_name"], build_version="20261114120000", variant="kubernetes",
                         os="ubuntu-2604", kubernetes_version="1.37.2"),
            "config": "".join(mask + "\n" for mask in pipeline.masks(cfg)), "tools": "",
            "copy": json.dumps({"raw_sha256": copied["raw_sha256"], "raw_bytes": 4096}) + "\n",
            "inventory": "", "review": "", "sanitize": "", "postscan": "", "coldboot": "",
            "bundle": line(artifact_name=outputs["artifact_name"], manifest_sha256=outputs["manifest_sha256"],
                           disk_sha256=outputs["disk_sha256"], reference=plan["reference"]),
            "evidence": line(evidence="vm-export-evidence.json", evidence_sha256=digest, reference=plan["reference"])})
        for private in PRIVATE + (str(VMID),):
            self.assertNotIn(private, text.replace("ubuntu-26.04.1-live", ""), private)
            for name, printed in lab.printed.items():
                if name != "config":  # The masks ARE the private values, by design.
                    self.assertNotIn(private, printed, (name, private))
        # Nothing private was printed by a refusal-free run either.
        self.assertEqual(lab.stderr, "")
        # The hosted verifier accepts exactly this record for exactly this publication.
        chosen = vm_publish_stage.inputs("4242", "1", outputs["artifact_name"], outputs["manifest_sha256"],
                                         outputs["disk_sha256"], SHA, vm_publish_stage.REPOSITORY, vm_publish_stage.WORKFLOW_REF,
                                         "kubernetes", "ubuntu-2604", "1.37.2")
        self.assertEqual(vm_publish_stage.evidence_text(
            str(work / "evidence" / "vm-export-evidence.json"), digest,
            vm_publish_stage.publication_plan(str(work / "bundle"), chosen), chosen), text)

    def test_native_run_uses_the_os_paths_and_the_eligibility_gate(self):
        lab = Lab(self, EXPORT_VARIANT="os", EXPORT_OS="ubuntu-2404", EXPORT_KUBERNETES_VERSION="")
        lab.through("evidence")
        work, argv = lab.work, dict(lab.calls)
        self.assertEqual([name for name, _ in lab.calls], ["tools", "inventory", "sanitize", "postscan", "coldboot"])
        source, receipt = str(work / "private" / "source.qcow2"), str(work / "private" / "copy-receipt.json")
        self.assertEqual(argv["inventory"][:6], ["os", "--os", "ubuntu-2404", "--source", source, "--copy-receipt"])
        self.assertEqual(argv["sanitize"][:7], ["os", "--os", "ubuntu-2404", "--source", source, "--copy-receipt", receipt])
        self.assertEqual(argv["postscan"][:5], ["os", "--os", "ubuntu-2404", "--disk", str(work / "sanitize" / "disk.qcow2")])
        self.assertNotIn("--kubernetes-version", argv["coldboot"])
        self.assertNotIn("--review", argv["sanitize"] + argv["postscan"])
        self.assertFalse((work / "review" / "k8s-review.json").exists())
        manifest = json.loads((work / "bundle" / "manifest.json").read_bytes())
        self.assertEqual((manifest["variant"], manifest["upstream_checksum"], manifest["image_tag"]),
                         ("os", VENDOR, "ubuntu-2404-os-20261114120000-cccccccccccc"))
        self.assertNotIn("kubernetes_version", manifest)
        record = json.loads((work / "evidence" / "vm-export-evidence.json").read_text())
        self.assertEqual((record["review"], record["kubernetes_version"]), ({"sanitize_eligible": True}, None))
        self.assertEqual(pipeline.load(receipt)["template"]["name"], "pc-ubuntu-2404-202611140900")
        # The copied QCOW2 FILE is larger than the virtual size of the disk (metadata): its length is the bound
        # of the copy, never the virtual size. (Recorded on the lab: 21478375424 bytes for a 20 GiB disk.)
        self.assertEqual(pipeline.load(receipt)["raw_bytes"], 4096 + 512)
        self.assertGreater(len(lab.raw), pipeline.vm_sanitize.VIRTUAL_BYTES)
        self.assertEqual(lab.printed["review"], '{"sanitize_eligible": true}\n')
        self.assertEqual(lab.printed["copy"], json.dumps({"raw_sha256": hashlib.sha256(lab.raw).hexdigest(), "raw_bytes": 4608}) + "\n")
        self.assertEqual(lab.api.storages, ["import-store"])

    def test_native_disk_file_is_bounded_as_the_sanitizer_bounds_it(self):
        self.assertEqual(vm_sanitize.NATIVE_SOURCE_MAX_BYTES, 24 << 30)
        self.assertGreater(21478375424, vm_sanitize.VIRTUAL_BYTES)  # The real ubuntu-2604 template file.
        self.assertLess(21478375424, vm_sanitize.NATIVE_SOURCE_MAX_BYTES)
        self.assertIn("0 < source.st_size <= NATIVE_SOURCE_MAX_BYTES)", (IMAGES / "vm_sanitize.py").read_text())
        for limit, written, code in ((4608, None, 0), (4607, None, 2), (1 << 20, b"", 2)):
            with self.subTest(limit=limit, written=written):
                lab = Lab(self, EXPORT_VARIANT="os", EXPORT_OS="ubuntu-2604", EXPORT_KUBERNETES_VERSION="")
                lab.through("tools")
                if written is not None:
                    (lab.mount / "images" / "105" / "base-105-disk-0.qcow2").write_bytes(written)
                with patch.object(pipeline.vm_sanitize, "NATIVE_SOURCE_MAX_BYTES", limit):
                    self.assertEqual(lab.stage("copy"), code)
                self.assertEqual(lab.stderr, "copy refused: template_disk_size\n" if code else "")

    def test_a_stage_runs_only_after_exactly_the_earlier_ones(self):
        lab = Lab(self)
        for name in pipeline.STAGES[1:]:  # Nothing before the plan: no work directory.
            with self.subTest(first=name):
                self.assertEqual(lab.stage(name), 2)
                self.assertFalse(lab.work.exists())
        lab.through("copy")
        self.assertEqual(lab.stage("plan"), 2)  # A second plan never reuses the work directory.
        for name in ("review", "sanitize", "postscan", "coldboot", "bundle", "evidence", "copy", "tools"):
            with self.subTest(skipped_to=name):
                self.assertEqual(lab.stage(name), 2)
                self.assertEqual(lab.stderr, name + " refused: stage_out_of_order\n")
        self.assertEqual([name for name, _ in lab.calls], ["tools"])
        lab.fail = "inventory"  # A stage that did not return 0 is not recorded: the next one refuses.
        self.assertEqual(lab.stage("inventory"), 1)
        self.assertEqual(lab.stage("review"), 2)
        self.assertEqual(pipeline.load(lab.work / "stages.json"), ["plan", "config", "tools", "copy"])
        self.assertEqual(pipeline.STAGES, ("plan", "config", "tools", "copy", "inventory", "review", "sanitize", "postscan",
                                           "coldboot", "bundle", "evidence"))
        self.assertEqual(sorted(pipeline.HANDLERS), sorted(pipeline.STAGES[1:]))

    def test_invalid_inputs_stop_before_the_work_directory_secrets_or_any_call(self):
        lab = Lab(self, EXPORT_TEMPLATE_VMID="105; rm -rf /")
        self.assertEqual(lab.stage("plan"), 2)
        self.assertEqual(lab.stderr, "plan refused: invalid_inputs\n")
        self.assertFalse(lab.work.exists())
        self.assertEqual((lab.calls, lab.api.calls), ([], []))

    def test_an_unexpected_error_is_reduced_to_its_class_name(self):
        """Only fixed reason codes are printed: an error text can carry a path, a value or guest data."""
        lab = Lab(self)
        lab.through("copy")
        for error, printed in ((OSError(13, "Permission denied", "/srv/pc-ci-images/images/105/base-105-disk-0.raw"), "PermissionError"),
                               (KeyError("token-secret-value"), "KeyError"), (ValueError("guest said: 192.0.2.50"), "ValueError"),
                               (AttributeError("'NoneType' object has no attribute 'token-secret-value'"), "AttributeError"),
                               (IndexError("192.0.2.50"), "IndexError"), (__import__("http.client").client.IncompleteRead(b"192.0.2.50"), "IncompleteRead"),
                               (RuntimeError("/srv/pc-ci-images"), "RuntimeError"),
                               (capsule.CapsuleError("source does not match the copy receipt"), "source does not match the copy receipt"),
                               (coldboot.ApiError(403), "pve_http_403")):
            def fail(argv, error=error):
                raise error
            with self.subTest(error=type(error).__name__), patch.object(pipeline.inventory, "main", fail):
                self.assertEqual(lab.stage("inventory"), 2)
                self.assertEqual(lab.stderr, "inventory refused: %s\n" % printed)
        self.assertEqual(pipeline.load(lab.work / "stages.json"), ["plan", "config", "tools", "copy"])

    def test_configuration_refusal_prints_no_secret_value(self):
        lab = Lab(self)
        lab.through("plan")
        self.assertEqual(lab.stage("config", dict(lab.env, EXPORT_TEST_ADDRESSES="192.0.2.50/24,203.0.113.77/24")), 2)
        self.assertEqual(lab.stderr, "config refused: config_cold_boot_settings\n")
        self.assertEqual(lab.stdout, "")
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            self.assertEqual(pipeline.main(["config"], lab.env), 0)
        self.assertEqual(out.getvalue().splitlines(), pipeline.masks(pipeline.settings(lab.env, pipeline.load(lab.work / "plan.json"))))

    def test_copy_refuses_a_writable_mount_a_wrong_template_and_leaves_no_copy(self):
        for change, code in (("writable", "source_mount_not_read_only"), ("name", "name/tags"), ("tags", "name/tags"),
                             ("qcow2", "template_disk_format"), ("size", "template_disk_size"),
                             ("room", "insufficient_work_space"), ("running", "template must be stopped, unlocked")):
            with self.subTest(change=change):
                lab = Lab(self)
                lab.through("tools")
                env = lab.env
                if change == "writable":
                    env = dict(lab.env, EXPORT_SOURCE_MOUNT=str(lab.runner))
                elif change == "name":
                    lab.api.config["name"] = "ubuntu-2604-kube-v1.36.6"
                elif change == "tags":
                    lab.api.config["tags"] += ";pc-extra"
                elif change == "qcow2":
                    lab.api.config["scsi0"] = "pc-ci-images:105/base-105-disk-0.qcow2"
                elif change == "size":
                    (lab.mount / "images" / "105" / "base-105-disk-0.raw").write_bytes(lab.raw + b"x")
                elif change == "room":
                    self.enterContext(patch.object(pipeline, "WORK_ROOM", 1 << 60))
                else:
                    lab.api.call = lambda method, path, **kw: ({"status": "running"} if path.endswith("/current")
                                                               else FakePVE.call(lab.api, method, path, **kw))
                self.assertEqual(lab.stage("copy", env), 2)
                self.assertEqual(lab.stderr, "copy refused: %s\n" % code)
                self.assertEqual(os.listdir(lab.work / "private"), [])
                self.assertFalse((lab.work / "source.json").exists())

    def test_cold_boot_and_bundle_refuse_records_that_are_not_bound(self):
        def edit(path, change):
            row = pipeline.load(path)
            change(row)
            os.chmod(path, 0o600)
            Path(path).write_text(json.dumps(row))
        cases = (("coldboot", "postscan/postscan-run.json", lambda r: r.update(clean=False)),
                 ("coldboot", "postscan/postscan-run.json", lambda r: r.update(disk_sha256="0" * 64)),
                 ("coldboot", "sanitize/sanitize-run.json", lambda r: r.update(sealed_after_cgroup_stop=False)),
                 ("coldboot", "sanitize/sanitize-run.json", lambda r: r.update(disk="/var/tmp/other/disk.qcow2")),
                 ("bundle", "coldboot/coldboot-result.json", lambda r: r.update(accepted=False)),
                 ("bundle", "coldboot/coldboot-result.json", lambda r: r.update(reason="guest_checks_failed")),
                 ("bundle", "coldboot/coldboot-result.json", lambda r: r.update(disk_sha256="0" * 64)),
                 ("bundle", "coldboot/coldboot-result.json", lambda r: r.update(kubernetes_version="1.36.6")),
                 ("bundle", "coldboot/coldboot-result.json", lambda r: r.update(kind="ColdBootCleanup")),
                 ("bundle", "postscan/postscan-run.json", lambda r: r.update(clean=False)),
                 ("bundle", "private/copy-receipt.json", lambda r: r.update(raw_sha256="0" * 64)),
                 ("evidence", "coldboot/coldboot-result.json", lambda r: r.update(accepted=False)),
                 ("evidence", "review/k8s-review.json", lambda r: r.update(cloud_default_user="other")),
                 ("evidence", "inventory/inventory-run.json", lambda r: r.update(image=capsule.IMAGE_IDS[1])),
                 ("evidence", "tools.json", lambda r: r.update(local_id="sha256:" + "0" * 64)),
                 ("evidence", "sanitize/sanitize-run.json", lambda r: r["pins"].update({"/input/source.raw": "0" * 64})))
        for stage, name, change in cases:
            with self.subTest(stage=stage, record=name):
                lab = Lab(self)
                lab.through(pipeline.STAGES[pipeline.STAGES.index(stage) - 1])
                edit(lab.work / name, change)
                calls = len(lab.calls)
                self.assertEqual(lab.stage(stage), 2)
                self.assertEqual(len(lab.calls), calls)  # Refused before the cold boot (or anything else) ran.
                self.assertFalse((lab.work / "evidence" / "vm-export-evidence.json").exists())
                if stage == "bundle":
                    self.assertNotIn("manifest.json", os.listdir(lab.work / "bundle"))
                    self.assertFalse(Path(lab.env["GITHUB_OUTPUT"]).exists())

    def test_purge_deletes_every_work_file(self):
        lab = Lab(self)
        self.assertEqual(lab.stage("purge"), 0)  # Nothing to delete before a plan.
        lab.through("evidence")
        (lab.work / "scratch" / "x-scratch").mkdir()
        (lab.work / "scratch" / "x-scratch" / "locked").mkdir(mode=0o000)  # A capsule-written unreadable directory.
        self.assertEqual(lab.stage("purge"), 0)
        self.assertFalse(lab.work.exists())
        self.assertEqual(os.listdir(lab.runner), [])
        self.assertEqual(os.listdir(lab.states), ["4242-1"])  # Not purge's to delete: see recover.
        self.assertEqual(sorted(pipeline.DIRS), sorted(["private", "scratch", "docker-anonymous", "inventory", "review",
                                                        "sanitize", "postscan", "coldboot", "bundle", "evidence"]))

    def test_cold_boot_state_outlives_the_job_until_its_cleanup_is_recorded_clean(self):
        """The runner empties its temp directory after every job. What a cold boot left on the lab is known
        only from its state file, so that file lives elsewhere and goes only when the cleanup is clean."""
        lab = Lab(self)
        self.assertEqual(lab.stage("recover"), 0)  # No state directory at all: nothing to do, no API call.
        lab.through("evidence")
        self.assertEqual((lab.stage("recover"), os.listdir(lab.states)), (0, []))
        self.assertEqual(lab.calls[-1], ("cleanup", ["cleanup", "--state-dir", str(lab.states / "4242-1")]))
        # An unclean cleanup: the state stays and the step fails, whatever purge and the runner then delete.
        lab = Lab(self)
        lab.fail = "cleanup"
        lab.through("postscan")
        self.assertEqual(lab.stage("coldboot"), 1)
        self.assertEqual(lab.stage("recover"), 1)
        self.assertEqual(lab.stderr, "recover: 1 cold-boot run(s) not recorded clean on the lab; the state is kept\n")
        self.assertEqual(lab.stage("purge"), 0)
        self.assertFalse(lab.work.exists())
        self.assertEqual(os.listdir(lab.states / "4242-1"), [coldboot.STATE])
        # The next run (a new dispatch, the same runner): its cold boot does not start next to that state...
        later = Lab(self, GITHUB_RUN_ID="4243")
        later.env["HOME"], later.states = lab.env["HOME"], lab.states
        later.through("postscan")
        self.assertEqual(later.stage("coldboot"), 2)
        self.assertEqual(later.stderr, "coldboot refused: leftover_cold_boot_state\n")
        self.assertEqual(later.stage("recover"), 1)
        # ...and once the lab answers the guarded cleanup, `recover` (which runs first) removes it.
        pipeline.write(lab.states / "4242-1" / "x", {})
        (lab.states / "4242-1" / coldboot.STATE).write_text(json.dumps({"clean": True}))
        self.assertEqual((later.stage("recover"), os.listdir(lab.states)), (0, []))
        self.assertEqual(later.stage("coldboot"), 0)
        self.assertEqual(os.listdir(lab.states), ["4243-1"])
        # A run that never wrote a state file changed nothing on the lab: removed without any API call. One
        # whose state cannot be read is kept.
        lab = Lab(self)
        for name, content in (("7-1", None), ("8-1", "not json"), ("9-1", json.dumps({"clean": True}))):
            (lab.states / name).mkdir(parents=True)
            (lab.states / name / "id_ed25519").write_text("key")
            if content is not None:
                (lab.states / name / coldboot.STATE).write_text(content)
        self.assertEqual(lab.stage("recover"), 1)
        self.assertEqual(os.listdir(lab.states), ["8-1"])
        self.assertEqual([argv[2] for name, argv in lab.calls if name == "cleanup"], [str(lab.states / "8-1"), str(lab.states / "9-1")])
        for home in ("", "relative/home", "/no/such/home", str(lab.home) + "/../home", str(lab.home) + "/a b"):
            with self.subTest(home=home):
                self.assertEqual(lab.stage("recover", dict(lab.env, HOME=home)), 2)
                self.assertEqual(lab.stderr, "recover refused: runner_home_refused\n")

    def test_a_capsule_not_proven_stopped_is_removed_before_anything_under_it_is_deleted(self):
        cid = "e" * 64
        lab = Lab(self)
        lab.through("copy")

        def unresolved(argv):
            raise capsule.Unresolved("UNRESOLVED exact capsule cleanup " + cid)
        with patch.object(pipeline.inventory, "main", unresolved):
            self.assertEqual(lab.stage("inventory"), 2)
        self.assertEqual((lab.work / "unresolved-container").read_text(), cid + "\n")
        commands = []

        def docker(listed):
            def run(args, **kw):
                commands.append(args)
                return types.SimpleNamespace(returncode=0, stdout=listed if args[1] == "ps" else "")
            return run
        with patch.object(capsule, "_command", docker(cid + "\n")):  # Docker still lists it: nothing is deleted.
            self.assertEqual(lab.stage("purge"), 2)
        self.assertEqual(lab.stderr, "purge refused: capsule_container_still_present\n")
        self.assertTrue((lab.work / "private" / "source.raw").exists())
        self.assertEqual(commands, [["docker", "rm", "--force", cid],
                                    ["docker", "ps", "--all", "--quiet", "--no-trunc", "--filter", "id=" + cid]])
        with patch.object(capsule, "_command", lambda args, **kw: types.SimpleNamespace(returncode=1, stdout="")):
            self.assertEqual(lab.stage("purge"), 2)  # Docker does not answer: not proven gone.
        (lab.work / "unresolved-container").write_text("--all\n")
        with patch.object(capsule, "_command", docker("")):
            self.assertEqual(lab.stage("purge"), 2)  # Never an option or a name: exactly a container id.
        self.assertEqual(lab.stderr, "purge refused: container id\n")
        (lab.work / "unresolved-container").write_text(cid + "\n")
        with patch.object(capsule, "_command", docker("")):
            self.assertEqual(lab.stage("purge"), 0)
        self.assertFalse(lab.work.exists())


class EvidenceTests(unittest.TestCase):
    def setUp(self):
        self.lab = Lab(self)
        self.lab.through("evidence")
        work = self.lab.work
        self.plan = dict(pipeline.load(work / "plan.json"), tool_image_id=capsule.IMAGE_IDS[0])
        self.record = json.loads((work / "evidence" / "vm-export-evidence.json").read_text())
        manifest_bytes = (work / "bundle" / "manifest.json").read_bytes()
        self.args = [self.plan, pipeline.load(work / "source.json"), pipeline.load(work / "private" / "copy-receipt.json"),
                     pipeline.load(work / "inventory" / "inventory-run.json"), (work / "review" / "k8s-review.json").read_bytes(),
                     pipeline.baseline(self.plan).read_bytes(), pipeline.load(work / "sanitize" / "sanitize-run.json"),
                     pipeline.load(work / "postscan" / "postscan-run.json"),
                     pipeline.load(work / "coldboot" / coldboot.RESULT), vm_artifact.decode(manifest_bytes), manifest_bytes]

    def test_record_holds_hashes_checks_versions_and_run_identity(self):
        record = evidence.build(*self.args)
        self.assertEqual(record, self.record)
        self.assertEqual(sorted(record), ["artifact_name", "build_version", "bundle", "coldboot", "image_tag", "inventory",
                                          "kind", "kubernetes_version", "os", "postscan", "reference", "repository", "review",
                                          "run_attempt", "run_id", "run_ref", "sanitize", "schema_version", "source",
                                          "source_commit", "tool_image", "variant"])
        self.assertEqual(record["tool_image"], {"reference": capsule.IMAGE_REFERENCE, "local_id": capsule.IMAGE_IDS[0]})
        self.assertEqual((record["source_commit"], record["run_id"], record["image_tag"], record["kubernetes_version"]),
                         (SHA, 4242, "ubuntu-2604-k8s-1.37.2-20261114120000-cccccccccccc", "1.37.2"))
        self.assertEqual(record["review"], {"sha256": hashlib.sha256(self.args[4]).hexdigest(),
                                            "package_policy_sha256": "5" * 64,
                                            "baseline_sha256": hashlib.sha256(self.args[5]).hexdigest()})
        self.assertEqual(record["inventory"]["pins"]["source.raw"], record["source"]["raw_sha256"])
        self.assertEqual(sorted(record["sanitize"]["pins"]), ["k8s-review.json", "k8s_copy_policy.py", "source.raw",
                                                              "stream_sanitized.py", "vm_sanitize.py"])
        self.assertEqual(record["postscan"]["result"], postscan_probe.expected("kubernetes", "1.37.2"))
        self.assertEqual(record["coldboot"]["clones"][0]["checks"],
                         dict.fromkeys(coldboot_guest.COMMON + coldboot_guest.K8S, True))
        self.assertEqual(evidence.encode(record), (self.lab.work / "evidence" / "vm-export-evidence.json").read_bytes())

    def test_nothing_private_from_any_record_gets_in(self):
        """Every record is stuffed with private-looking extras at every level; none may reach the output."""
        planted = {"node": NODE, "path": "/srv/pc-ci-images/images/105/base-105-disk-0.raw", "vmid": 105105105,
                   "address": "10.200.242.50", "mac": "bc:24:11:aa:bb:cc", "token": "images@pve!packer",
                   "guest": {"users": ["zz-guest-user"], "hostname": "zz-guest-host"}}

        whole = ("pins", "checks", "result", "distinct_identity")  # Taken as a whole: their key set must be exact.

        def stuff(value, key=None, everywhere=False):
            if type(value) is dict:
                if not everywhere and (key in whole or key == "clones"):
                    return value
                return dict({k: stuff(item, k, everywhere) for k, item in value.items()},
                            **{"planted_" + k: v for k, v in planted.items()})
            return [stuff(item, None, everywhere) for item in value] if type(value) is list else value
        args = [stuff(arg) if type(arg) is dict and index not in (0, 9) else arg for index, arg in enumerate(self.args)]
        self.assertEqual(evidence.build(*args), self.record)
        text = evidence.encode(evidence.build(*args)).decode()
        for index in (3, 6, 7, 8):  # An extra key inside a whole-taken map refuses the record: it never leaks.
            args = list(self.args)
            args[index] = stuff(self.args[index], everywhere=True)
            with self.subTest(index=index), self.assertRaises(evidence.EvidenceRefused):
                evidence.build(*args)
        for marker in ("planted", "105105105", "10.200", "bc:24", "@pve", "zz-guest", "hostname", "/srv", NODE,
                       "container_id", "known_cgroup", "pc-export-test", "import_volume\": \"pc-", "guests", "volumes",
                       "machine_id_sha256\": \"", "\"name\"", "\"disk\":", "\"run\":", "private_bytes", "vmid", "digest\""):
            self.assertNotIn(marker, text, marker)

    def test_check_refuses_any_key_that_is_not_allowlisted(self):
        def dicts(value, path=()):
            if type(value) is dict:
                yield path
                for key, item in value.items():
                    yield from dicts(item, path + (key,))
            elif type(value) is list:
                for index, item in enumerate(value):
                    yield from dicts(item, path + (index,))
        places = list(dicts(self.record))
        self.assertGreater(len(places), 20)
        for path in places:
            for key, value in (("node", "pve1"), ("extra", True), ("source_path", "x"), ("template_vmid", 105)):
                record = copy_module.deepcopy(self.record)
                target = record
                for part in path:
                    target = target[part]
                target[key] = value
                with self.subTest(path=path, key=key), self.assertRaises(evidence.EvidenceRefused):
                    evidence.check(record, self.plan)
        evidence.check(self.record, self.plan)
        for path in places[1:]:  # A missing section or key is refused too: the record is exactly the schema.
            record = copy_module.deepcopy(self.record)
            target = record
            for part in path[:-1]:
                target = target[part]
            if type(target) is dict:
                del target[path[-1]]
                with self.subTest(removed=path), self.assertRaises(evidence.EvidenceRefused):
                    evidence.check(record, self.plan)

    def test_check_refuses_an_address_or_path_looking_string_in_every_string_field(self):
        def strings(value, path=()):
            if type(value) is dict:
                for key, item in value.items():
                    yield from strings(item, path + (key,))
            elif type(value) is list:
                for index, item in enumerate(value):
                    yield from strings(item, path + (index,))
            elif type(value) is str:
                yield path
        places = list(strings(self.record))
        self.assertGreater(len(places), 40)
        hostile = ("/srv/pc-ci-images/images/105/base-105-disk-0.raw", "/home/gha-runner-01/_work/_temp/vm-export",
                   "10.200.242.50", "10.200.242.50/24", "bc:24:11:aa:bb:cc", "images@pve!packer", "pve-node-01",
                   "https://10.200.108.63:8006/api2/json", "../../etc/passwd", "pc-templates", "",
                   "ghcr.io/attacker/image:tag", "https://github.com/attacker/repo/actions/runs/1")
        for path in places:
            for value in hostile:
                record = copy_module.deepcopy(self.record)
                target = record
                for part in path[:-1]:
                    target = target[part]
                target[path[-1]] = value
                with self.subTest(path=path, value=value), self.assertRaises(evidence.EvidenceRefused):
                    evidence.check(record, self.plan)

    def test_scan_alone_refuses_addresses_paths_and_token_ids_whatever_the_schema_allows(self):
        for value in ("10.200.242.50", "192.0.2.1/24", "host 10.0.0.1", "bc:24:11:aa:bb:cc", "BC:24:11:AA:BB:CC",
                      "/srv/pc-ci-images", "images/105/disk", "a\\b", "images@pve!packer", "user@pve", "a b", "..",
                      "https://example.test/x"):
            with self.subTest(value=value), self.assertRaises(evidence.EvidenceRefused):
                evidence.scan({"field": value})
        for key in ("10.200.242.50", "/input/source.raw", "a b", "", "x" * 65, 5):
            with self.subTest(key=key), self.assertRaises(evidence.EvidenceRefused):
                evidence.scan({key: True})
        for value in (1.5, b"bytes", ("tuple",)):
            with self.subTest(value=value), self.assertRaises(evidence.EvidenceRefused):
                evidence.scan({"field": value})
        evidence.scan({"version": "1.37.2", "n": 3, "ok": True, "none": None, "hash": "sha256:" + "a" * 64,
                       "list": ["1.36.6", {"tag": "ubuntu-2604-k8s-1.37.2-20261114120000-cccccccccccc"}]})
        # A slash is tolerated only in the five public URL/reference fields, which have exact patterns.
        self.assertEqual(sorted(path for path in self.strings_with_slash()), sorted(evidence.SLASH_OK))

    def strings_with_slash(self):
        def walk(value, path="evidence"):
            if type(value) is dict:
                for key, item in value.items():
                    yield from walk(item, path + "." + key)
            elif type(value) is list:
                for item in value:
                    yield from walk(item, path)
            elif type(value) is str and "/" in value:
                yield path
        return walk(self.record)

    def test_build_refuses_records_that_do_not_name_the_same_bytes(self):
        def changed(index, change):
            args = copy_module.deepcopy(self.args)
            if type(args[index]) is bytes:
                args[index] = change(args[index])
            else:
                change(args[index])
            return args
        cases = ((2, lambda r: r.update(raw_sha256="0" * 64)), (2, lambda r: r.pop("template_unchanged_stopped")),
                 (2, lambda r: r.update(template_unchanged_stopped=False)), (2, lambda r: r.update(disk_parsed_or_guest_executed=True)),
                 (3, lambda r: r.update(accepted=False)), (3, lambda r: r.update(stop_reason="parent_wall_limit")),
                 (3, lambda r: r.update(cleanup_errors=["docker-stop-exit"])), (3, lambda r: r.update(image="sha256:" + "0" * 64)),
                 (3, lambda r: r.update(check_id="k8s-inventory-1.36.6")),
                 (3, lambda r: r["pins"].update({"/bootstrap/unknown.py": "0" * 64})),
                 (3, lambda r: r["pins"].update({"/srv/other/vm_sanitize.py": "0" * 64})),
                 (4, lambda b: b + b" "), (6, lambda r: r.update(sealed_after_cgroup_stop=False)),
                 (6, lambda r: r["facts"]["checks"].update(zero_free_space=False)),
                 (6, lambda r: r["facts"].update(disk_sha256="0" * 64)), (7, lambda r: r.update(clean=False)),
                 (7, lambda r: r["result"].update(snapd_unregistered=False)), (7, lambda r: r["result"].update(extra=True)),
                 (8, lambda r: r.update(disk_sha256="0" * 64)), (8, lambda r: r.update(step="cleanup")),
                 (8, lambda r: r["clones"].pop()), (8, lambda r: r["clones"][1]["checks"].update(kubelet_enabled=False)),
                 (8, lambda r: r["clones"][0].update(all_passed=False)),
                 (8, lambda r: r["distinct_identity"].update(host_key_sha256=False)),
                 (8, lambda r: r["cleanup"]["clones"].update(b="absent")), (8, lambda r: r["cleanup"].update(import_volume="unknown")),
                 (8, lambda r: r["protected_set"].update(unchanged=False)), (8, lambda r: r["storage_set"].update(unchanged=False)),
                 (8, lambda r: r.update(api_node_room="10.0.0.1")), (9, lambda r: r.update(source_commit="0" * 40)),
                 (10, lambda b: b.replace(b"amd64", b"arm64")), (1, lambda r: r.update(upstream_url="https://10.200.108.63/image.iso")),
                 (1, lambda r: r.update(upstream_url="file:///srv/image.iso")), (1, lambda r: r.pop("upstream_checksum")),
                 # The URL pattern alone would admit these two: the address/path scan refuses them.
                 (1, lambda r: r.update(upstream_url="https://releases.ubuntu.com/10.200.242.50/image.iso")),
                 (1, lambda r: r.update(upstream_url="https://releases.ubuntu.com/a/../b.iso")),
                 (3, lambda r: r["pins"].update({"/bootstrap/sub/vm_sanitize.py": "0" * 64})),  # Same file name twice.
                 (0, lambda r: r.update(run_id="4242")), (0, lambda r: r.update(tool_image_id="sha256:" + "0" * 64)))
        for index, change in cases:
            with self.subTest(index=index), self.assertRaises(evidence.EvidenceRefused) as caught:
                evidence.build(*changed(index, change))
            for private in PRIVATE:
                self.assertNotIn(private, str(caught.exception))


if __name__ == "__main__":
    unittest.main(verbosity=2)
