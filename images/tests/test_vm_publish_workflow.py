"""Authored source-only tests; NOT executed for this change.

Only synthetic data, stdlib ZIPs and temporary directories. No commands, HTTP,
registry authentication, image parser, actual QCOW2, GitHub Actions or PVE.
"""
import copy
import importlib.util
import io
import os
from pathlib import Path
import stat
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch
import zipfile

IMAGES = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(IMAGES))
try:
    SPEC = importlib.util.spec_from_file_location("vm_publish_stage", IMAGES / "vm_publish_stage.py")
    stage = importlib.util.module_from_spec(SPEC)
    SPEC.loader.exec_module(stage)
finally:
    sys.path.pop(0)
contract = stage.contract


class StagingTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.disk = b"QFI\xfb" + bytes(100)  # NOT a valid QCOW2; no image parser is called.
        request = dict(kind="vm-template", os="ubuntu-2604", arch="amd64", variant="os",
                       source_commit="a" * 40, upstream_checksum="b" * 64, build_version="20261111000000")
        receipt = dict(kind="CleanExportReceipt", source_identity="sha256:" + "c" * 64,
                       source_commit=request["source_commit"],
                       run_ref="https://github.com/mutex-solutions/proxcloud/actions/runs/12",
                       request_sha256=contract.sha(contract.encode(request)), disk_sha256=contract.sha(self.disk),
                       disk_bytes=len(self.disk), virtual_size_bytes=20 << 30,
                       checks={key: True for key in contract.CHECKS})
        self.manifest = dict(request, schema_version=1, image_tag=contract.image_tag(request),
                             clean_export_receipt=receipt,
                             disk=dict(filename="disk.qcow2", format="qcow2", sha256=contract.sha(self.disk),
                                       bytes=len(self.disk), virtual_size_bytes=20 << 30))
        raw = contract.encode(self.manifest)
        self.payload = {"disk.qcow2": self.disk, "manifest.json": raw, "SHA256SUMS": contract.sums(self.manifest, raw)}
        self.name = "vm-export-ubuntu-2604-20261111000000-aaaaaaaaaaaa"
        self.args = ["123", self.name, contract.sha(raw), contract.sha(self.disk), "d" * 40, stage.REPOSITORY]
        self.selected = stage.inputs(*self.args)
        self.workflow = dict(id=99, path=stage.STAGING_WORKFLOW, state="active")
        repo = dict(id=42, full_name=stage.REPOSITORY)
        self.run = dict(id=123, workflow_id=99, path=stage.STAGING_WORKFLOW, event="workflow_dispatch",
                        status="completed", conclusion="success", head_branch="main", head_sha="d" * 40,
                        repository=repo, head_repository=repo.copy())
        self.artifact = dict(id=456, name=self.name, expired=False, size_in_bytes=4096,
                             archive_download_url="https://api.github.com/repos/mutex-solutions/proxcloud/actions/artifacts/456/zip",
                             workflow_run=dict(id=123, repository_id=42, head_repository_id=42,
                                               head_branch="main", head_sha="d" * 40))
        self.listing = dict(total_count=1, artifacts=[self.artifact])

    def archive(self, payload=None):
        path = self.root / "payload.zip"
        with zipfile.ZipFile(path, "w", compression=zipfile.ZIP_STORED) as archive:
            for name, data in (self.payload if payload is None else payload).items():
                archive.writestr(name, data)
        return path

    def test_inputs_are_literal_bounded_selectors(self):
        for index, values in {
            0: ["", "0", "01", "-1", "1\n", "1/../../2", "1" * 21],
            1: [self.name + "\n", "../" + self.name, self.name.replace("ubuntu-2604", "ubuntu-2604-k8s"),
                self.name.replace("20261111000000", "20260230000000"), self.name.replace("aaaaaaaaaaaa", "AAAAAAAAAAAA")],
            2: ["a" * 63, "A" * 64, "sha256:" + "a" * 64, "a" * 64 + "\n"],
            3: ["", "$(command)", "0" * 65], 4: ["a" * 39, "A" * 40],
            5: ["attacker/proxcloud", stage.REPOSITORY + "\n"],
        }.items():
            for value in values:
                args = self.args.copy(); args[index] = value
                with self.subTest(index=index, value=value), self.assertRaises(ValueError):
                    stage.inputs(*args)

    def test_producer_authority_binds_workflow_repository_run_revision_and_artifact(self):
        self.assertEqual(stage.authorize(self.workflow, self.run, self.listing, self.selected), 456)
        cases = [
            ("workflow", (), "path", ".github/workflows/attacker.yml"),
            ("workflow", (), "state", "disabled_manually"), ("workflow", (), "id", True),
            ("run", (), "id", 124), ("run", (), "workflow_id", 100),
            ("run", (), "path", ".github/workflows/attacker.yml"),
            ("run", (), "event", "pull_request"), ("run", (), "event", "push"),
            ("run", (), "conclusion", "failure"), ("run", (), "status", "in_progress"),
            ("run", (), "head_branch", "feature"), ("run", (), "head_sha", "a" * 40),
            ("run", ("repository",), "full_name", "attacker/proxcloud"),
            ("run", ("head_repository",), "id", 43),
            ("listing", (), "total_count", 2), ("listing", (), "total_count", True),
            ("listing", ("artifacts", 0), "expired", True),
            ("listing", ("artifacts", 0), "name", self.name + "-extra"),
            ("listing", ("artifacts", 0), "size_in_bytes", stage.MAX_ARCHIVE_BYTES + 1),
            ("listing", ("artifacts", 0), "archive_download_url", "https://evil.test/archive.zip"),
            ("listing", ("artifacts", 0, "workflow_run"), "id", 124),
            ("listing", ("artifacts", 0, "workflow_run"), "head_repository_id", 43),
            ("listing", ("artifacts", 0, "workflow_run"), "head_sha", "a" * 40),
        ]
        for which, path, key, value in cases:
            objects = dict(workflow=copy.deepcopy(self.workflow), run=copy.deepcopy(self.run), listing=copy.deepcopy(self.listing))
            target = objects[which]
            for part in path:
                target = target[part]
            target[key] = value
            with self.subTest(which=which, key=key, value=value), self.assertRaises(ValueError):
                stage.authorize(objects["workflow"], objects["run"], objects["listing"], self.selected)
        self.listing["artifacts"].append(self.artifact.copy())
        with self.assertRaises(ValueError):
            stage.authorize(self.workflow, self.run, self.listing, self.selected)

    def test_valid_archive_pins_and_fixed_plan_are_data_not_boot_acceptance(self):
        destination = self.root / "out"
        stage.extract(str(self.archive()), str(destination))
        self.assertEqual(set(os.listdir(destination)), stage.MEMBERS.keys())
        plan = stage.publication_plan(str(destination), self.selected)
        self.assertEqual(plan["reference"], "ghcr.io/mutex-solutions/proxcloud-vm-ubuntu-2604:ubuntu-2604-os-20261111000000-aaaaaaaaaaaa")
        self.assertEqual(plan["source_commit"], "a" * 40)  # Source-image SHA is not the producer's checkout SHA.
        self.assertIn("org.opencontainers.image.source=https://github.com/mutex-solutions/proxcloud", plan["argv"])
        self.assertIn("org.opencontainers.image.revision=" + "a" * 40, plan["argv"])
        self.assertEqual(plan["argv"][-3:], ["disk.qcow2:application/x-qemu-disk",
                         "manifest.json:application/vnd.proxcloud.vm-template.manifest.v1+json", "SHA256SUMS:text/plain"])
        self.assertFalse(plan["publish_eligible"] or plan["execute"] or any(plan["release_gates"].values()))
        for changes in ({"manifest_sha": "0" * 64}, {"disk_sha": "0" * 64}, {"os": "debian-13"},
                        {"source_prefix": "b" * 12}, {"build_version": "20261112000000"}):
            with self.subTest(changes=changes), self.assertRaises(ValueError):
                stage.publication_plan(str(destination), dict(self.selected, **changes))
        (destination / "request.json").write_bytes(b"private")
        with self.assertRaises(ValueError):
            stage.publication_plan(str(destination), self.selected)

    def kubernetes_payload(self, version="1.35.10"):
        request = dict(kind="vm-template", os="ubuntu-2604", arch="amd64", variant="kubernetes",
                       kubernetes_version=version, source_commit="a" * 40, upstream_checksum="b" * 64,
                       build_version="20261111000000")
        receipt = dict(self.manifest["clean_export_receipt"], request_sha256=contract.sha(contract.encode(request)))
        manifest = dict(request, schema_version=1, image_tag=contract.image_tag(request), clean_export_receipt=receipt,
                        disk=self.manifest["disk"])
        raw = contract.encode(manifest)
        return {"disk.qcow2": self.disk, "manifest.json": raw, "SHA256SUMS": contract.sums(manifest, raw)}, raw

    def test_kubernetes_selector_publishes_only_to_k8s_package(self):
        payload, raw = self.kubernetes_payload()
        name = "vm-k8s-export-ubuntu-2604-20261111000000-aaaaaaaaaaaa"
        selected = stage.inputs("123", name, contract.sha(raw), contract.sha(self.disk), "d" * 40, stage.REPOSITORY)
        self.assertEqual((selected["os"], selected["variant"]), ("ubuntu-2604", "kubernetes"))
        destination = self.root / "k8s"
        stage.extract(str(self.archive(payload)), str(destination))
        plan = stage.publication_plan(str(destination), selected)
        self.assertEqual(plan["reference"], "ghcr.io/mutex-solutions/proxcloud-vm-ubuntu-2604-k8s:"
                                            "ubuntu-2604-k8s-1.35.10-20261111000000-aaaaaaaaaaaa")
        self.assertFalse(plan["publish_eligible"] or plan["execute"] or any(plan["release_gates"].values()))
        # A native-OS name never admits a K8s manifest (and vice versa).
        native = stage.inputs("123", self.name, contract.sha(raw), contract.sha(self.disk), "d" * 40, stage.REPOSITORY)
        with self.assertRaises(ValueError): stage.publication_plan(str(destination), native)
        destination = self.root / "os"
        stage.extract(str(self.archive()), str(destination))
        with self.assertRaises(ValueError):
            stage.publication_plan(str(destination), dict(self.selected, variant="kubernetes"))
        for bad in ("vm-k8s-export-ubuntu-2404-20261111000000-aaaaaaaaaaaa",
                    "vm-k8s-export-debian-13-20261111000000-aaaaaaaaaaaa",
                    "vm-k8s-export-ubuntu-2604-20260230000000-aaaaaaaaaaaa", name + "\n"):
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                stage.inputs("123", bad, contract.sha(raw), contract.sha(self.disk), "d" * 40, stage.REPOSITORY)

    def test_kubernetes_other_version_refused(self):
        payload, raw = self.kubernetes_payload("1.35.11")
        name = "vm-k8s-export-ubuntu-2604-20261111000000-aaaaaaaaaaaa"
        selected = stage.inputs("123", name, contract.sha(raw), contract.sha(self.disk), "d" * 40, stage.REPOSITORY)
        destination = self.root / "k8s-other"
        stage.extract(str(self.archive(payload)), str(destination))
        with self.assertRaises(ValueError): stage.publication_plan(str(destination), selected)

    def test_archive_extra_paths_and_existing_destination_refused_before_writes(self):
        for name in ("../escape", "/absolute", "nested/disk.qcow2", "request.json", "disk.qcow2/", "disk.qcow2\\suffix"):
            payload = dict(self.payload, **{name: b"hostile"})
            destination = self.root / "never-created"
            with self.subTest(name=name), self.assertRaises(ValueError):
                stage.extract(str(self.archive(payload)), str(destination))
            self.assertFalse(destination.exists())
        destination = self.root / "existing"; destination.mkdir()
        with self.assertRaises(FileExistsError):
            stage.extract(str(self.archive()), str(destination))
        self.assertEqual(list(destination.iterdir()), [])
        link = self.root / "link"; link.symlink_to(destination, target_is_directory=True)
        with self.assertRaises(FileExistsError):
            stage.extract(str(self.archive()), str(link))

    def test_zip_descriptors_reject_duplicates_links_encryption_bombs_and_limits(self):
        def descriptors():
            return [SimpleNamespace(filename=name, orig_filename=name, flag_bits=0,
                                    external_attr=(stat.S_IFREG | 0o600) << 16,
                                    compress_type=zipfile.ZIP_STORED, file_size=100, compress_size=100,
                                    is_dir=lambda: False) for name in stage.MEMBERS]
        def validate(rows):
            return stage.zip_members(SimpleNamespace(infolist=lambda: rows, comment=b""))
        self.assertEqual(len(validate(descriptors())), 3)
        for key, value in (("filename", "../disk.qcow2"), ("orig_filename", "disk.qcow2\x00hidden"),
                           ("flag_bits", 1), ("external_attr", (stat.S_IFLNK | 0o777) << 16),
                           ("external_attr", (stat.S_IFIFO | 0o600) << 16), ("compress_type", zipfile.ZIP_BZIP2),
                           ("file_size", stage.MEMBERS["disk.qcow2"] + 1), ("file_size", 0),
                           ("file_size", 10001), ("compress_size", 0)):
            rows = descriptors(); setattr(rows[0], key, value)
            with self.subTest(key=key, value=value), self.assertRaises(ValueError):
                validate(rows)
        rows = descriptors(); rows[2] = rows[0]
        with self.assertRaises(ValueError):
            validate(rows)
        with patch.object(stage, "MAX_ARCHIVE_BYTES", 1), self.assertRaises(ValueError):
            stage.extract(str(self.archive()), str(self.root / "bounded"))

    def test_directory_allocation_bound_precedes_zip_parser_and_accepts_zip64(self):
        raw = self.archive().read_bytes()
        stage.zip_directory_bound(io.BytesIO(raw), len(raw))
        end = list(stage.struct.unpack("<4s4H2IH", raw[-22:]))
        for index, value in ((3, 10000), (4, 10000), (5, 65537), (6, 0), (7, 1)):
            bad = end.copy(); bad[index] = value
            data = raw[:-22] + stage.struct.pack("<4s4H2IH", *bad)
            with self.subTest(index=index), self.assertRaises(ValueError):
                stage.zip_directory_bound(io.BytesIO(data), len(data))
        # Equivalent fixed-size ZIP64 directory records, even with a small fixture.
        position = len(raw) - 22
        record = stage.struct.pack("<4sQ2H2I4Q", b"PK\x06\x06", 44, 45, 45, 0, 0, 3, 3, end[5], end[6])
        locator = stage.struct.pack("<4sIQI", b"PK\x06\x07", 0, position, 1)
        end[5:7] = [0xffffffff, 0xffffffff]
        data = raw[:-22] + record + locator + stage.struct.pack("<4s4H2IH", *end)
        stage.zip_directory_bound(io.BytesIO(data), len(data))

    def test_only_explicit_unknown_registry_404_allows_new_tag(self):
        for code in ("MANIFEST_UNKNOWN", "NAME_UNKNOWN"):
            stage.absent_tag(404, contract.encode({"errors": [{"code": code}]}))
        for status, value in ((200, {"errors": [{"code": "MANIFEST_UNKNOWN"}]}),
                              (401, {"errors": [{"code": "UNAUTHORIZED"}]}), (403, {}), (500, {}),
                              (404, {"errors": []}), (404, {"errors": [{"code": "DENIED"}]}),
                              (404, {"errors": [{"code": "NAME_UNKNOWN"}, {"code": "DENIED"}]})):
            with self.subTest(status=status, value=value), self.assertRaises(ValueError):
                stage.absent_tag(status, contract.encode(value))


class WorkflowNameRegexTests(unittest.TestCase):
    """Pre-checkout inline regexes must accept exactly what NAME/K8S_NAME accept."""
    def test_inline_regexes_match_helper_selectors(self):
        import re
        samples = ["vm-export-%s-20261111000000-aaaaaaaaaaaa" % os_name
                   for os_name in ("ubuntu-2404", "ubuntu-2604", "debian-12", "debian-13", "debian-14")]
        samples += ["vm-k8s-export-%s-20261111000000-aaaaaaaaaaaa" % os_name
                    for os_name in ("ubuntu-2604", "ubuntu-2404", "debian-13")]
        samples += ["vm-export-ubuntu-2604-k8s-20261111000000-aaaaaaaaaaaa", "vm-k8s-export-ubuntu-2604-2026111100000-aaaaaaaaaaaa",
                    "vm-k8s-export-ubuntu-2604-20261111000000-AAAAAAAAAAAA", "x-vm-export-ubuntu-2604-20261111000000-aaaaaaaaaaaa"]
        for workflow in ("vm-artifact-stage.yml", "vm-artifacts.yml"):
            text = (IMAGES.parent / ".github/workflows" / workflow).read_text()
            found = re.findall(r"re\.fullmatch\(r'(\(\?:vm-export-[^']+)'", text)
            self.assertEqual(len(found), 1, workflow)
            for sample in samples:
                helper = any(re.fullmatch(p, sample, flags=re.ASCII) for p in (stage.NAME, stage.K8S_NAME))
                with self.subTest(workflow=workflow, sample=sample):
                    self.assertEqual(re.fullmatch(found[0], sample, flags=re.ASCII) is not None, helper)
            self.assertIn("strptime(found[1], '%Y%m%d%H%M%S')", text)  # Timestamp group index.


class WorkflowSourceTests(unittest.TestCase):
    def test_manual_hosted_fixed_source_no_cluster_or_guest_execution(self):
        text = (IMAGES.parent / ".github/workflows/vm-artifacts.yml").read_text()
        triggers = text.split("on:\n", 1)[1].split("permissions:", 1)[0]
        self.assertIn("  workflow_dispatch:", triggers)
        for event in ("pull_request:", "pull_request_target:", "workflow_run:", "push:", "schedule:", "release:"):
            self.assertNotIn(event, triggers)
        for item in ("staging_run_id:", "artifact_name:", "expected_manifest_sha256:", "expected_disk_sha256:",
                     "runs-on: ubuntu-24.04", "contents: read", "actions: read", "packages: write",
                     "ref: ${{ github.sha }}", "persist-credentials: false", "cancel-in-progress: false",
                     "workflows/vm-artifact-stage.yml", "cwd=payload", "stage.absent_tag(status, raw)",
                     "version: 1.3.0", "--password-stdin", "oras logout ghcr.io"):
            self.assertIn(item, text)
        for item in ("secrets.PVE", "self-hosted", "proxcloud-pve", "qemu-img ", "docker run", ":latest",
                     "matrix:", "environment:", "oras tag", "actions/download-artifact@"):
            self.assertNotIn(item, text)
        # Match a shell token, not the suffix of harmless prose such as "retrieval ".
        self.assertNotRegex(text, r"(?m)(?:^|[\s;|&])eval[\s(]")
        self.assertRegex(text, r"oras-project/setup-oras@[0-9a-f]{40}")
        self.assertRegex(text, r"actions/checkout@[0-9a-f]{40}")
        # Inputs enter env and then strict Python validation, never a shell run interpolation.
        for expression in ("${{ inputs.staging_run_id }}", "${{ inputs.artifact_name }}",
                           "${{ inputs.expected_manifest_sha256 }}", "${{ inputs.expected_disk_sha256 }}"):
            self.assertEqual(text.count(expression), 1)
        self.assertNotIn("--location-trusted ", text)
