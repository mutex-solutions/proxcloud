"""Publisher helper and reusable publisher workflow: the only producer is the export job of the same run.

Only synthetic data, stdlib ZIPs and temporary directories. No commands, HTTP,
registry authentication, image parser, actual QCOW2, GitHub Actions or PVE.
"""
import copy
import datetime
import importlib.util
import io
import os
from pathlib import Path
import re
import stat
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch
import zipfile

IMAGES = Path(__file__).resolve().parents[1]
WORKFLOWS = IMAGES.parent / ".github" / "workflows"
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
                       source_commit="d" * 40, upstream_checksum="b" * 64, build_version="20261111000000")
        receipt = dict(kind="CleanExportReceipt", source_identity="sha256:" + "c" * 64,
                       source_commit=request["source_commit"],
                       run_ref="https://github.com/mutex-solutions/proxcloud/actions/runs/123",
                       request_sha256=contract.sha(contract.encode(request)), disk_sha256=contract.sha(self.disk),
                       disk_bytes=len(self.disk), virtual_size_bytes=20 << 30,
                       checks={key: True for key in contract.CHECKS})
        self.manifest = dict(request, schema_version=1, image_tag=contract.image_tag(request),
                             clean_export_receipt=receipt,
                             disk=dict(filename="disk.qcow2", format="qcow2", sha256=contract.sha(self.disk),
                                       bytes=len(self.disk), virtual_size_bytes=20 << 30))
        raw = contract.encode(self.manifest)
        self.payload = {"disk.qcow2": self.disk, "manifest.json": raw, "SHA256SUMS": contract.sums(self.manifest, raw)}
        self.name = "vm-export-ubuntu-2604-20261111000000-dddddddddddd"
        self.args = ["123", "2", self.name, contract.sha(raw), contract.sha(self.disk), "d" * 40, stage.REPOSITORY,
                     stage.WORKFLOW_REF, "os", "ubuntu-2604", "none"]
        self.now = datetime.datetime(2026, 11, 11, 3, 0, 0)
        self.selected = stage.inputs(*self.args)
        self.workflow = dict(id=99, path=stage.EXPORT_WORKFLOW, state="active")
        repo = dict(id=42, full_name=stage.REPOSITORY)
        # The publisher runs INSIDE the export run: the run is in progress, its export job concluded success.
        self.run = dict(id=123, workflow_id=99, path=stage.EXPORT_WORKFLOW, event="workflow_dispatch", run_attempt=2,
                        status="in_progress", conclusion=None, head_branch="main", head_sha="d" * 40,
                        created_at="2026-11-10T23:59:30Z", repository=repo, head_repository=repo.copy())
        export = dict(id=7001, run_id=123, run_attempt=2, name="export", head_sha="d" * 40, status="completed",
                      conclusion="success", labels=["self-hosted", "linux", "x64", "proxcloud-vm-artifact-export"])
        publish = dict(id=7002, run_id=123, run_attempt=2, name="publish / publish", head_sha="d" * 40,
                       status="in_progress", conclusion=None, labels=["ubuntu-24.04"])
        self.jobs = dict(total_count=2, jobs=[export, publish])
        self.artifact = dict(id=456, name=self.name, expired=False, size_in_bytes=4096,
                             archive_download_url="https://api.github.com/repos/mutex-solutions/proxcloud/actions/artifacts/456/zip",
                             workflow_run=dict(id=123, repository_id=42, head_repository_id=42,
                                               head_branch="main", head_sha="d" * 40))
        self.listing = dict(total_count=1, artifacts=[self.artifact])

    def inputs(self, name, raw, version="none"):
        """Dispatch inputs that agree with the name: the test is about the name or about the manifest."""
        variant = "kubernetes" if name.startswith("vm-k8s-") and version != "none" else "os"
        os_name = name.split("export-", 1)[1][:11]
        return stage.inputs("123", "2", name, contract.sha(raw), contract.sha(self.disk), "d" * 40, stage.REPOSITORY,
                            stage.WORKFLOW_REF, variant, os_name, version)

    def authorize(self, now=None, **changed):
        return stage.authorize(*(changed.get(name, getattr(self, name)) for name in ("workflow", "run", "jobs", "listing")),
                               self.selected, now or self.now)

    def archive(self, payload=None):
        path = self.root / "payload.zip"
        with zipfile.ZipFile(path, "w", compression=zipfile.ZIP_STORED) as archive:
            for name, data in (self.payload if payload is None else payload).items():
                archive.writestr(name, data)
        return path

    def test_inputs_are_literal_bounded_selectors(self):
        for index, values in {
            0: ["", "0", "01", "-1", "1\n", "1/../../2", "1" * 21],
            1: ["", "0", "01", "1\n", "1" * 5, "latest"],
            2: [self.name + "\n", "../" + self.name, self.name.replace("ubuntu-2604", "ubuntu-2604-k8s"),
                self.name.replace("20261111000000", "20260230000000"), self.name.replace("dddddddddddd", "DDDDDDDDDDDD")],
            3: ["a" * 63, "A" * 64, "sha256:" + "a" * 64, "a" * 64 + "\n"],
            4: ["", "$(command)", "0" * 65],
            6: ["attacker/proxcloud", stage.REPOSITORY + "\n"],
            # The caller must be the export workflow on main: not the old producer, the publisher itself,
            # another workflow, another ref or a fork.
            7: ["", stage.REPOSITORY + "/.github/workflows/vm-artifact-stage.yml@refs/heads/main",
                stage.REPOSITORY + "/.github/workflows/vm-artifacts.yml@refs/heads/main",
                stage.REPOSITORY + "/.github/workflows/vm-templates.yml@refs/heads/main",
                stage.WORKFLOW_REF.replace("refs/heads/main", "refs/heads/feature"),
                stage.WORKFLOW_REF.replace("refs/heads/main", "refs/tags/v1"),
                stage.WORKFLOW_REF.replace("mutex-solutions", "attacker"), stage.WORKFLOW_REF + "\n"],
            # What was dispatched must be what the name stands for: a native dispatch never publishes a
            # Kubernetes bundle, another OS, or a version nobody chose (and the commit is this one).
            8: ["kubernetes", "", "OS"], 9: ["ubuntu-2404", ""], 10: ["1.37.2", "None", "1.38.0"],
            5: ["a" * 39, "A" * 40, "a" * 40],
        }.items():
            for value in values:
                args = self.args.copy(); args[index] = value
                with self.subTest(index=index, value=value), self.assertRaises(ValueError):
                    stage.inputs(*args)
        self.assertEqual(self.selected["kubernetes_version"], None)
        self.assertEqual(stage.inputs(*self.args[:10], ""), self.selected)  # "none" is the form's empty choice.
        k8s = self.args.copy()
        k8s[2], k8s[8] = "vm-k8s-export-ubuntu-2604-20261111000000-dddddddddddd", "kubernetes"
        for version in ("none", "", "1.38.0", "1.37"):
            with self.subTest(version=version), self.assertRaises(ValueError):
                stage.inputs(*k8s[:10], version)
        for version in stage.K8S_VERSIONS:
            self.assertEqual(stage.inputs(*k8s[:10], version)["kubernetes_version"], version)
        with self.assertRaises(ValueError):  # A Kubernetes name for a native dispatch.
            stage.inputs(*self.args[:2], k8s[2], *self.args[3:])

    def test_producer_authority_binds_workflow_repository_run_revision_job_and_artifact(self):
        self.assertEqual(self.authorize(), 456)
        self.assertEqual(stage.EXPORT_WORKFLOW, ".github/workflows/vm-artifact-export.yml")
        self.assertFalse(hasattr(stage, "STAGING_WORKFLOW"))
        cases = [
            ("workflow", (), "path", ".github/workflows/attacker.yml"),
            ("workflow", (), "path", ".github/workflows/vm-artifact-stage.yml"),  # The old hand-fed producer.
            ("workflow", (), "path", ".github/workflows/vm-artifacts.yml"),
            ("workflow", (), "state", "disabled_manually"), ("workflow", (), "id", True),
            ("run", (), "id", 124), ("run", (), "workflow_id", 100),
            ("run", (), "path", ".github/workflows/attacker.yml"),
            ("run", (), "path", ".github/workflows/vm-artifact-stage.yml"),
            ("run", (), "event", "pull_request"), ("run", (), "event", "push"), ("run", (), "event", "workflow_run"),
            # Not this running attempt: a finished run, another attempt, or one that already has a conclusion.
            ("run", (), "status", "completed"), ("run", (), "status", "queued"), ("run", (), "conclusion", "success"),
            ("run", (), "conclusion", "failure"), ("run", (), "run_attempt", 1), ("run", (), "run_attempt", 3),
            ("run", (), "run_attempt", "2"),
            ("run", (), "head_branch", "feature"), ("run", (), "head_sha", "a" * 40),
            # The build time in the name is this run's: not from before the run existed (5 minutes of clock
            # difference allowed), and the field is GitHub's exact format.
            ("run", (), "created_at", "2026-11-11T00:05:01Z"), ("run", (), "created_at", "2026-11-12T00:00:00Z"),
            ("run", (), "created_at", None), ("run", (), "created_at", "2026-11-11 00:00:00"),
            ("run", (), "created_at", 1794355200), ("run", (), "repository", None), ("run", (), "repository", [42]),
            ("run", (), "head_repository", None), ("run", (), "head_repository", "mutex-solutions/proxcloud"),
            ("run", ("repository",), "full_name", "attacker/proxcloud"),
            ("run", ("head_repository",), "id", 43),
            # The export JOB of this attempt must have concluded success, on the export runner, at this commit.
            ("jobs", ("jobs", 0), "conclusion", "failure"), ("jobs", ("jobs", 0), "conclusion", "cancelled"),
            ("jobs", ("jobs", 0), "conclusion", "skipped"), ("jobs", ("jobs", 0), "conclusion", None),
            ("jobs", ("jobs", 0), "status", "in_progress"), ("jobs", ("jobs", 0), "status", "queued"),
            ("jobs", ("jobs", 0), "name", "export / export"), ("jobs", ("jobs", 0), "name", "Export"),
            ("jobs", ("jobs", 0), "run_id", 124), ("jobs", ("jobs", 0), "head_sha", "a" * 40),
            ("jobs", ("jobs", 0), "run_attempt", 3), ("jobs", ("jobs", 0), "run_attempt", 0),
            ("jobs", ("jobs", 0), "run_attempt", True), ("jobs", ("jobs", 0), "id", 0),
            ("jobs", ("jobs", 0), "labels", ["ubuntu-24.04"]), ("jobs", ("jobs", 0), "labels", ["self-hosted"]),
            ("jobs", ("jobs", 0), "labels", ["self-hosted", "linux", "x64", "proxcloud-pve"]),
            ("jobs", ("jobs", 1), "name", "export"),  # Two jobs called export.
            ("jobs", (), "total_count", 3), ("jobs", (), "total_count", 101), ("jobs", (), "total_count", True),
            ("jobs", (), "jobs", None),
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
            objects = dict(workflow=copy.deepcopy(self.workflow), run=copy.deepcopy(self.run),
                           jobs=copy.deepcopy(self.jobs), listing=copy.deepcopy(self.listing))
            target = objects[which]
            for part in path:
                target = target[part]
            target[key] = value
            with self.subTest(which=which, key=key, value=value), self.assertRaises(ValueError):
                stage.authorize(objects["workflow"], objects["run"], objects["jobs"], objects["listing"], self.selected,
                                self.now)
        for name in ("workflow", "run", "listing"):  # A response that is not an object at all.
            for value in (None, [], "x"):
                with self.subTest(name=name, value=value), self.assertRaises(ValueError):
                    self.authorize(**{name: value})
        # Not a time in the future either: the name's build time lies between the run's creation and now.
        self.assertEqual(self.authorize(now=datetime.datetime(2026, 11, 10, 23, 55, 0)), 456)
        self.assertEqual(self.authorize(run=dict(self.run, created_at="2026-11-11T00:05:00Z")), 456)
        with self.assertRaises(ValueError):
            self.authorize(now=datetime.datetime(2026, 11, 10, 23, 54, 59))
        self.assertEqual(stage.CLOCK_SKEW, datetime.timedelta(minutes=5))
        with patch.object(stage.datetime, "datetime", wraps=datetime.datetime) as clock:  # The default is the UTC clock.
            clock.now.return_value = datetime.datetime(2026, 11, 11, 3, 0, 0, tzinfo=datetime.timezone.utc)
            self.assertEqual(stage.authorize(self.workflow, self.run, self.jobs, self.listing, self.selected), 456)
            clock.now.return_value = datetime.datetime(2026, 11, 10, 3, 0, 0, tzinfo=datetime.timezone.utc)
            with self.assertRaises(ValueError):
                stage.authorize(self.workflow, self.run, self.jobs, self.listing, self.selected)
        self.listing["artifacts"].append(self.artifact.copy())
        with self.assertRaises(ValueError):
            self.authorize()

    def test_export_job_of_this_attempt_or_kept_from_an_earlier_one(self):
        """Re-running only the failed jobs keeps the successful export job of an earlier attempt of this run."""
        self.assertEqual(stage.export_job(self.jobs, self.selected), 7001)
        kept = copy.deepcopy(self.jobs)
        kept["jobs"][0]["run_attempt"] = 1
        self.assertEqual(stage.export_job(kept, self.selected), 7001)
        for jobs in ({}, {"total_count": 0, "jobs": []}, {"total_count": 1, "jobs": [self.jobs["jobs"][1]]},
                     {"total_count": 1, "jobs": ["export"]}, None):
            with self.subTest(jobs=jobs), self.assertRaises(ValueError):
                stage.export_job(jobs, self.selected)

    def test_public_reference_and_final_summary_come_from_the_artifact_name(self):
        digest = "sha256:" + "e" * 64
        package = "ghcr.io/mutex-solutions/proxcloud-vm-ubuntu-2604"
        self.assertEqual(stage.public_reference(self.selected, package, digest), package + "@" + digest)
        k8s = dict(self.selected, variant="kubernetes")
        self.assertEqual(stage.public_reference(k8s, package + "-k8s", digest), package + "-k8s@" + digest)
        for chosen, name, value in ((self.selected, package + "-k8s", digest), (k8s, package, digest),
                                    (self.selected, "ghcr.io/attacker/proxcloud-vm-ubuntu-2604", digest),
                                    (self.selected, package + ":latest", digest), (self.selected, package, "e" * 64),
                                    (self.selected, package, "sha256:" + "E" * 64), (self.selected, package, digest + "\n"),
                                    (self.selected, "", digest), (self.selected, package, "")):
            with self.subTest(name=name, value=value), self.assertRaises(ValueError):
                stage.public_reference(chosen, name, value)
        destination = self.root / "pulled"
        stage.extract(str(self.archive()), str(destination))
        plan = stage.publication_plan(str(destination), self.selected)
        text = stage.final_summary(plan, package + "@" + digest, '{"kind": "VmExportEvidence"}\n')
        # The printed command accepts only an attestation signed by the export workflow on main: with
        # `--repo` alone, a copy of the workflow on any branch of the repository would satisfy it.
        strict = (" --repo mutex-solutions/proxcloud --signer-workflow mutex-solutions/proxcloud/.github/workflows/"
                  "vm-artifact-export.yml --source-ref refs/heads/main\n")
        for item in ("gh attestation verify oci://" + package + "@" + digest + strict,
                     "gh attestation verify vm-export-evidence.json" + strict,
                     "oras pull --output pull " + package + "@" + digest,
                     "--expected-manifest-sha256 " + self.selected["manifest_sha"],
                     "--expected-disk-sha256 " + self.selected["disk_sha"], "| OCI digest | `" + digest + "` |",
                     "ubuntu-2604-os-20261111000000-dddddddddddd", "no registry credential",
                     '```json\n{"kind": "VmExportEvidence"}\n```\n'):
            self.assertIn(item, text)
        self.assertEqual(text.count("gh attestation verify"), 2)

    def evidence(self, plan, **changes):
        record = {"kind": "VmExportEvidence", "reference": plan["reference"], "run_id": 123, "source_commit": "d" * 40,
                  "bundle": {"manifest_sha256": self.selected["manifest_sha"], "disk_sha256": self.selected["disk_sha"]}}
        record.update(changes)
        raw = contract.encode(record)
        path = self.root / "vm-export-evidence.json"
        path.write_bytes(raw)
        return str(path), contract.sha(raw), raw

    def test_evidence_is_printed_only_when_it_is_the_attested_record_of_this_publication(self):
        destination = self.root / "pulled"
        stage.extract(str(self.archive()), str(destination))
        plan = stage.publication_plan(str(destination), self.selected)
        path, digest, raw = self.evidence(plan)
        self.assertEqual(stage.evidence_text(path, digest, plan, self.selected), raw.decode())
        for bad in ("0" * 64, "", digest.upper(), "sha256:" + digest):
            with self.subTest(sha=bad), self.assertRaises(ValueError):
                stage.evidence_text(path, bad, plan, self.selected)
        with self.assertRaises(ValueError):
            stage.evidence_text("vm-export-evidence.json", digest, plan, self.selected)  # Not an absolute path.
        for changes in ({"kind": "Other"}, {"reference": plan["reference"] + "x"}, {"run_id": 124}, {"run_id": "123"},
                        {"source_commit": "a" * 40}, {"bundle": {"manifest_sha256": "0" * 64, "disk_sha256": self.selected["disk_sha"]}},
                        {"bundle": {"manifest_sha256": self.selected["manifest_sha"], "disk_sha256": "0" * 64}}, {"bundle": None}):
            path, digest, _ = self.evidence(plan, **changes)
            with self.subTest(changes=changes), self.assertRaises((ValueError, TypeError, KeyError)):
                stage.evidence_text(path, digest, plan, self.selected)
        Path(path).write_bytes(b"x" * (stage.MAX_METADATA_BYTES + 1))
        with self.assertRaises(ValueError):
            stage.evidence_text(path, contract.sha(Path(path).read_bytes()), plan, self.selected)
        link = self.root / "link.json"
        path, digest, _ = self.evidence(plan)
        link.symlink_to(path)
        with self.assertRaises(OSError):
            stage.evidence_text(str(link), digest, plan, self.selected)
        # The CLI prints the summary and the record together, and nothing when the record is another one.
        values = dict(zip(stage.ENVIRONMENT, self.args), PACKAGE=plan["package"], DIGEST="sha256:" + "e" * 64,
                      EVIDENCE_SHA256=digest)
        out = io.StringIO()
        with patch.dict(os.environ, values, clear=True), patch("sys.stdout", out):
            self.assertEqual(stage.main(["pulled", "--staging-dir", str(destination), "--evidence", path]), 0)
        self.assertTrue(out.getvalue().endswith("```json\n" + Path(path).read_text() + "```\n"))
        for bad in (dict(values, EVIDENCE_SHA256="0" * 64), {k: v for k, v in values.items() if k != "EVIDENCE_SHA256"}):
            out = io.StringIO()
            with patch.dict(os.environ, bad, clear=True), patch("sys.stdout", out), patch("sys.stderr", io.StringIO()):
                self.assertEqual(stage.main(["pulled", "--staging-dir", str(destination), "--evidence", path]), 2)
                self.assertEqual(stage.main(["pulled", "--staging-dir", str(destination)]), 2)
            self.assertEqual(out.getvalue(), "")

    def test_the_attested_digest_is_the_one_of_the_manifest_this_push_sent(self):
        destination = self.root / "pushed"
        stage.extract(str(self.archive()), str(destination))
        plan = stage.publication_plan(str(destination), self.selected)
        exported = str(self.root / "vm-pushed-manifest.json")
        argv = stage.push_argv(plan, exported)
        self.assertEqual(argv[:4], ["oras", "push", "--export-manifest", exported])
        self.assertEqual(argv[4:], plan["argv"][2:])
        with self.assertRaises(ValueError):
            stage.push_argv(plan, "vm-pushed-manifest.json")
        with self.assertRaises(ValueError):
            stage.push_argv(dict(plan, argv=["oras", "attach"] + plan["argv"][2:]), exported)

        def manifest(*digests, **more):
            layers = [{"mediaType": "application/octet-stream", "digest": "sha256:" + value, "size": 1} for value in digests]
            raw = contract.encode(dict({"schemaVersion": 2, "layers": layers}, **more))
            Path(exported).write_bytes(raw)
            return raw
        wanted = (self.selected["disk_sha"], self.selected["manifest_sha"], "7" * 64)
        raw = manifest(*wanted)
        self.assertEqual(stage.pushed_digest(exported, plan), "sha256:" + contract.sha(raw))
        # Not the manifest of this bundle: another disk, a missing or an extra layer, no layers, not JSON, too big.
        for digests in (("0" * 64,) + wanted[1:], wanted[:2], wanted + ("8" * 64,), (wanted[0], "0" * 64, wanted[2])):
            manifest(*digests)
            with self.subTest(digests=digests), self.assertRaises(ValueError):
                stage.pushed_digest(exported, plan)
        for content in (b"", b"not json", contract.encode({"layers": None}), contract.encode({"layers": ["x"] * 3}),
                        contract.encode({"schemaVersion": 2}), manifest(*wanted, padding="x" * 65536)):
            Path(exported).write_bytes(content)
            with self.subTest(content=content[:20]), self.assertRaises((ValueError, TypeError, KeyError)):
                stage.pushed_digest(exported, plan)
        # The bound is exact: 64 KiB of manifest is read and accepted, one byte more is not.
        size = len(manifest(*wanted, padding=""))
        self.assertEqual(len(manifest(*wanted, padding="x" * (65536 - size))), 65536)
        stage.pushed_digest(exported, plan)
        self.assertEqual(len(manifest(*wanted, padding="x" * (65537 - size))), 65537)
        with self.assertRaises(ValueError):
            stage.pushed_digest(exported, plan)
        os.unlink(exported)
        os.symlink(str(destination / "manifest.json"), exported)
        with self.assertRaises(OSError):
            stage.pushed_digest(exported, plan)

    def test_cli_reads_one_environment_and_refuses_another_caller(self):
        values = dict(zip(stage.ENVIRONMENT, self.args), PACKAGE="ghcr.io/mutex-solutions/proxcloud-vm-ubuntu-2604",
                      DIGEST="sha256:" + "e" * 64)
        self.assertEqual(stage.ENVIRONMENT, ("EXPORT_RUN_ID", "EXPORT_RUN_ATTEMPT", "ARTIFACT_NAME", "EXPECTED_MANIFEST_SHA256",
                                             "EXPECTED_DISK_SHA256", "PUBLISHER_SHA", "PUBLISHER_REPOSITORY",
                                             "PUBLISHER_WORKFLOW_REF", "EXPORT_VARIANT", "EXPORT_OS",
                                             "EXPORT_KUBERNETES_VERSION"))
        out = io.StringIO()
        with patch.dict(os.environ, values, clear=True), patch("sys.stdout", out):
            self.assertEqual(stage.main(["check-inputs"]), 0)
            self.assertEqual(stage.main(["reference"]), 0)
        self.assertEqual(out.getvalue(), values["PACKAGE"] + "@" + values["DIGEST"] + "\n")
        for key, value in (("PUBLISHER_WORKFLOW_REF", "mutex-solutions/proxcloud/.github/workflows/vm-artifact-stage.yml@refs/heads/main"),
                           ("EXPORT_RUN_ATTEMPT", ""), ("PUBLISHER_REPOSITORY", "attacker/proxcloud"),
                           ("EXPORT_VARIANT", "kubernetes"), ("EXPORT_OS", "ubuntu-2404"),
                           ("EXPORT_KUBERNETES_VERSION", "1.37.2")):
            with self.subTest(key=key), patch.dict(os.environ, dict(values, **{key: value}), clear=True), \
                    patch("sys.stderr", io.StringIO()):
                self.assertEqual(stage.main(["check-inputs"]), 2)

    def test_valid_archive_pins_and_fixed_plan_are_data_not_boot_acceptance(self):
        destination = self.root / "out"
        stage.extract(str(self.archive()), str(destination))
        self.assertEqual(set(os.listdir(destination)), stage.MEMBERS.keys())
        plan = stage.publication_plan(str(destination), self.selected)
        self.assertEqual(plan["reference"], "ghcr.io/mutex-solutions/proxcloud-vm-ubuntu-2604:ubuntu-2604-os-20261111000000-dddddddddddd")
        self.assertEqual(plan["source_commit"], "d" * 40)  # The commit of the export run: the publisher's own.
        self.assertIn("org.opencontainers.image.source=https://github.com/mutex-solutions/proxcloud", plan["argv"])
        self.assertIn("org.opencontainers.image.revision=" + "d" * 40, plan["argv"])
        self.assertEqual(plan["argv"][-3:], ["disk.qcow2:application/x-qemu-disk",
                         "manifest.json:application/vnd.proxcloud.vm-template.manifest.v1+json", "SHA256SUMS:text/plain"])
        self.assertFalse(plan["publish_eligible"] or plan["execute"] or any(plan["release_gates"].values()))
        # The manifest must say what was dispatched, at the commit and in the run that publish it.
        for changes in ({"manifest_sha": "0" * 64}, {"disk_sha": "0" * 64}, {"os": "debian-13"},
                        {"publisher_sha": "d" * 12 + "a" * 28}, {"build_version": "20261112000000"}, {"run_id": 12},
                        {"run_id": 1234}, {"kubernetes_version": "1.37.2"}, {"variant": "kubernetes"}):
            with self.subTest(changes=changes), self.assertRaises(ValueError):
                stage.publication_plan(str(destination), dict(self.selected, **changes))
        (destination / "request.json").write_bytes(b"private")
        with self.assertRaises(ValueError):
            stage.publication_plan(str(destination), self.selected)

    def kubernetes_payload(self, version="1.35.10"):
        request = dict(kind="vm-template", os="ubuntu-2604", arch="amd64", variant="kubernetes",
                       kubernetes_version=version, source_commit="d" * 40, upstream_checksum="b" * 64,
                       build_version="20261111000000")
        receipt = dict(self.manifest["clean_export_receipt"], request_sha256=contract.sha(contract.encode(request)))
        manifest = dict(request, schema_version=1, image_tag=contract.image_tag(request), clean_export_receipt=receipt,
                        disk=self.manifest["disk"])
        raw = contract.encode(manifest)
        return {"disk.qcow2": self.disk, "manifest.json": raw, "SHA256SUMS": contract.sums(manifest, raw)}, raw

    def test_kubernetes_selector_publishes_only_to_k8s_package(self):
        payload, raw = self.kubernetes_payload()
        name = "vm-k8s-export-ubuntu-2604-20261111000000-dddddddddddd"
        selected = self.inputs(name, raw, "1.35.10")
        self.assertEqual((selected["os"], selected["variant"]), ("ubuntu-2604", "kubernetes"))
        destination = self.root / "k8s"
        stage.extract(str(self.archive(payload)), str(destination))
        plan = stage.publication_plan(str(destination), selected)
        self.assertEqual(plan["reference"], "ghcr.io/mutex-solutions/proxcloud-vm-ubuntu-2604-k8s:"
                                            "ubuntu-2604-k8s-1.35.10-20261111000000-dddddddddddd")
        self.assertFalse(plan["publish_eligible"] or plan["execute"] or any(plan["release_gates"].values()))
        # A native-OS name never admits a K8s manifest (and vice versa).
        native = self.inputs(self.name, raw)
        with self.assertRaises(ValueError): stage.publication_plan(str(destination), native)
        destination = self.root / "os"
        stage.extract(str(self.archive()), str(destination))
        with self.assertRaises(ValueError):
            stage.publication_plan(str(destination), dict(self.selected, variant="kubernetes"))
        for bad in ("vm-k8s-export-ubuntu-2404-20261111000000-dddddddddddd",
                    "vm-k8s-export-debian-13-20261111000000-dddddddddddd",
                    "vm-k8s-export-ubuntu-2604-20260230000000-aaaaaaaaaaaa", name + "\n"):
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                self.inputs(bad, raw, "1.35.10")

    def test_kubernetes_other_version_refused(self):
        """A bundle of a version nobody dispatched, and a version outside the allowlist."""
        payload, raw = self.kubernetes_payload("1.35.11")
        name = "vm-k8s-export-ubuntu-2604-20261111000000-dddddddddddd"
        selected = self.inputs(name, raw, "1.35.10")
        destination = self.root / "k8s-other"
        stage.extract(str(self.archive(payload)), str(destination))
        with self.assertRaises(ValueError): stage.publication_plan(str(destination), selected)
        with self.assertRaises(ValueError): self.inputs(name, raw, "1.35.11")
        # An accepted version, but not the dispatched one: a 1.36.6 dispatch never publishes a 1.37.2 bundle.
        payload, raw = self.kubernetes_payload("1.37.2")
        destination = self.root / "k8s-swapped"
        stage.extract(str(self.archive(payload)), str(destination))
        with self.assertRaises(ValueError): stage.publication_plan(str(destination), self.inputs(name, raw, "1.36.6"))
        stage.publication_plan(str(destination), self.inputs(name, raw, "1.37.2"))

    def test_kubernetes_accepted_versions_publish_to_their_own_tag(self):
        name = "vm-k8s-export-ubuntu-2604-20261111000000-dddddddddddd"
        for version in ("1.36.6", "1.37.2"):
            with self.subTest(version=version):
                payload, raw = self.kubernetes_payload(version)
                selected = self.inputs(name, raw, version)
                destination = self.root / ("k8s-" + version)
                stage.extract(str(self.archive(payload)), str(destination))
                plan = stage.publication_plan(str(destination), selected)
                self.assertEqual(plan["reference"], "ghcr.io/mutex-solutions/proxcloud-vm-ubuntu-2604-k8s:"
                                                    "ubuntu-2604-k8s-" + version + "-20261111000000-dddddddddddd")
                self.assertFalse(plan["publish_eligible"] or plan["execute"] or any(plan["release_gates"].values()))
        for version in ("1.36.5", "1.37.3", "1.38.0", "1.34.10"):
            with self.subTest(refused=version):
                payload, raw = self.kubernetes_payload(version)
                with self.assertRaises(ValueError): self.inputs(name, raw, version)
                destination = self.root / ("k8s-refused-" + version)
                stage.extract(str(self.archive(payload)), str(destination))
                with self.assertRaises(ValueError):
                    stage.publication_plan(str(destination), self.inputs(name, raw, "1.37.2"))

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
    """The pre-checkout inline regex must accept exactly what NAME/K8S_NAME accept."""
    def test_inline_regex_matches_helper_selectors(self):
        import re
        samples = ["vm-export-%s-20261111000000-dddddddddddd" % os_name
                   for os_name in ("ubuntu-2404", "ubuntu-2604", "debian-12", "debian-13", "debian-14")]
        samples += ["vm-k8s-export-%s-20261111000000-dddddddddddd" % os_name
                    for os_name in ("ubuntu-2604", "ubuntu-2404", "debian-13")]
        samples += ["vm-export-ubuntu-2604-k8s-20261111000000-dddddddddddd", "vm-k8s-export-ubuntu-2604-2026111100000-aaaaaaaaaaaa",
                    "vm-k8s-export-ubuntu-2604-20261111000000-DDDDDDDDDDDD", "x-vm-export-ubuntu-2604-20261111000000-dddddddddddd",
                    "vm-export-evidence-123-1"]
        text = (WORKFLOWS / "vm-artifacts.yml").read_text()
        found = re.findall(r"re\.fullmatch\(r'(\(\?:vm-export-[^']+)'", text)
        self.assertEqual(len(found), 1)
        for sample in samples:
            helper = any(re.fullmatch(p, sample, flags=re.ASCII) for p in (stage.NAME, stage.K8S_NAME))
            with self.subTest(sample=sample):
                self.assertEqual(re.fullmatch(found[0], sample, flags=re.ASCII) is not None, helper)
        self.assertIn("strptime(found[1], '%Y%m%d%H%M%S')", text)  # Timestamp group index.


class WorkflowSourceTests(unittest.TestCase):
    def setUp(self):
        self.text = (WORKFLOWS / "vm-artifacts.yml").read_text()

    def test_the_hand_fed_path_is_gone(self):
        self.assertFalse((WORKFLOWS / "vm-artifact-stage.yml").exists())
        self.assertFalse((IMAGES / "vm_stage_local.py").exists())
        self.assertFalse((IMAGES / "tests" / "test_vm_stage_local.py").exists())
        for path in [*WORKFLOWS.glob("*.yml"), *IMAGES.glob("*.py"), *(IMAGES / "vm_export").glob("*.py")]:
            for item in ("vm_stage_local", "vm-artifact-stage", "proxcloud-vm-artifact-stage", "staging_run_id",
                         "STAGING_RUN_ID"):
                self.assertNotIn(item, path.read_text(), path.name)

    def test_reusable_only_hosted_fixed_source_no_cluster_or_guest_execution(self):
        text = self.text
        triggers = text.split("\non:\n", 1)[1].split("\npermissions:", 1)[0]
        self.assertTrue(triggers.startswith("  workflow_call:\n"))
        # No way to start the publisher by hand or from an event: only the export workflow calls it.
        for event in ("workflow_dispatch:", "pull_request:", "pull_request_target:", "workflow_run:", "push:",
                      "schedule:", "release:", "repository_dispatch:"):
            self.assertNotIn(event, triggers)
        self.assertEqual(re.findall(r"^  ([a-z_]+):$", triggers, flags=re.M), ["workflow_call"])
        self.assertNotIn("workflow_dispatch", text)
        self.assertEqual(re.findall(r"^      ([a-z0-9_]+):$", triggers, flags=re.M),
                         ["artifact_name", "expected_manifest_sha256", "expected_disk_sha256", "variant", "os",
                          "kubernetes_version", "evidence_sha256", "package", "digest"])
        # Inputs and outputs only: the caller passes no secret, and no workflow-level env hands a value to
        # every step.
        self.assertEqual(re.findall(r"^    ([a-z_]+):$", triggers, flags=re.M), ["inputs", "outputs"])
        self.assertEqual(re.findall(r"^([A-Za-z_-]+):", text, flags=re.M), ["name", "on", "permissions", "concurrency", "jobs"])
        self.assertNotIn("secrets:", text)
        self.assertEqual(text.count("required: true"), 7)
        self.assertEqual(sorted(re.findall(r"^      ([A-Z_0-9]+): ", text, flags=re.M)), sorted(stage.ENVIRONMENT + ("EVIDENCE_SHA256",)))
        # The job's own guard, and the caller check before any checkout or API call (the helper repeats both).
        self.assertIn("jobs:\n  publish:\n    if: github.repository == 'mutex-solutions/proxcloud' && github.ref == 'refs/heads/main'\n", text)
        self.assertEqual(len(re.findall(r"(?m)^  [a-z]+:$", text.split("\njobs:\n", 1)[1])), 1)
        self.assertIn("          valid &= os.environ['PUBLISHER_WORKFLOW_REF'] == '%s'\n" % stage.WORKFLOW_REF, text)
        # A fixed API host and repository: never the runner's environment, which a caller could not set here
        # but which must not become the habit.
        self.assertEqual(text.count("          api='https://api.github.com/repos/mutex-solutions/proxcloud/actions'\n"), 1)
        self.assertEqual(text.count('"https://api.github.com/repos/mutex-solutions/proxcloud/actions/artifacts/$ARTIFACT_ID/zip" \\\n'), 1)
        self.assertEqual(len(re.findall(r"https://api\.github\.com", text)), 2)
        for item in ("GITHUB_API_URL", "GITHUB_REPOSITORY", "GITHUB_SERVER_URL", "github.api_url", "github.server_url"):
            self.assertNotIn(item, text)
        # An empty export output (GitHub drops one that contains a masked value) has its own message.
        self.assertIn("          if not all(os.environ[key] for key in ('ARTIFACT_NAME', 'EXPECTED_MANIFEST_SHA256', 'EXPECTED_DISK_SHA256', 'EVIDENCE_SHA256')):\n"
                      "              raise SystemExit('Publication refused: an output of the export job is empty "
                      "(GitHub drops an output that contains a masked value)')\n", text)
        for item in ("runs-on: ubuntu-24.04", "contents: read", "actions: read", "packages: write",
                     "ref: ${{ github.sha }}", "persist-credentials: false", "cancel-in-progress: false",
                     "workflows/vm-artifact-export.yml", "cwd=payload", "stage.absent_tag(status, raw)",
                     "version: 1.3.0", "--password-stdin", "oras logout ghcr.io",
                     "EXPORT_RUN_ID: ${{ github.run_id }}", "EXPORT_RUN_ATTEMPT: ${{ github.run_attempt }}",
                     "PUBLISHER_WORKFLOW_REF: ${{ github.workflow_ref }}", "PUBLISHER_SHA: ${{ github.sha }}",
                     '"$api/runs/$EXPORT_RUN_ID/attempts/$EXPORT_RUN_ATTEMPT/jobs?per_page=100" --output "$metadata/jobs.json"',
                     '"$api/runs/$EXPORT_RUN_ID/artifacts?per_page=100&name=$ARTIFACT_NAME" --output "$metadata/artifacts.json"',
                     "stage.inputs(*(os.environ[key] for key in stage.ENVIRONMENT))",
                     "EXPORT_VARIANT: ${{ inputs.variant }}", "EXPORT_OS: ${{ inputs.os }}",
                     "EXPORT_KUBERNETES_VERSION: ${{ inputs.kubernetes_version }}",
                     "exported = str(Path(os.environ['RUNNER_TEMP']) / 'vm-pushed-manifest.json')",
                     "subprocess.run(stage.push_argv(plan, exported), cwd=payload, check=True, timeout=900,",
                     "          digest = stage.pushed_digest(exported, plan)\n          reference = plan['package'] + '@' + digest\n",
                     "output.write('package=' + plan['package'] + '\\ndigest=' + digest + '\\n')",
                     "      package: ${{ steps.push.outputs.package }}\n      digest: ${{ steps.push.outputs.digest }}\n",
                     "        value: ${{ jobs.publish.outputs.digest }}\n"):
            self.assertIn(item, text)
        self.assertEqual(text.count("permissions:"), 2)
        self.assertIn("\npermissions: {}\n", text)
        self.assertIn("    permissions:\n      contents: read\n      actions: read\n      packages: write\n", text)
        self.assertEqual(re.findall(r"^\s+[a-z-]+: write$", text, flags=re.M), ["      packages: write"])
        # The digest handed to the attestation is never read back from the tag: a tag can be moved.
        for item in ("secrets.", "self-hosted", "proxcloud-pve", "qemu-img ", "docker run", ":latest",
                     "matrix:", "environment:", "oras tag", "actions/download-artifact@", "id-token", "attestations",
                     "'resolve'", "oras resolve", "oras manifest", "private until"):
            self.assertNotIn(item, text)
        self.assertEqual(text.count("digest = "), 1)
        # Match a shell token, not the suffix of harmless prose such as "retrieval ".
        self.assertNotRegex(text, r"(?m)(?:^|[\s;|&])eval[\s(]")
        self.assertRegex(text, r"oras-project/setup-oras@[0-9a-f]{40}")
        self.assertRegex(text, r"actions/checkout@[0-9a-f]{40}")
        self.assertEqual(len(re.findall(r"uses: ", text)), 2)
        # Inputs enter env and then strict Python validation, never a shell run interpolation; the run id
        # and attempt are GitHub's own, not inputs.
        for expression in ("${{ inputs.artifact_name }}", "${{ inputs.expected_manifest_sha256 }}",
                           "${{ inputs.expected_disk_sha256 }}", "${{ github.run_id }}", "${{ github.run_attempt }}"):
            self.assertEqual(text.count(expression), 1)
        self.assertNotIn("--location-trusted ", text)

    def test_authority_precedes_download_login_probe_and_push(self):
        order = ["Publication refused: an output of the export job is empty", "Publication refused: invalid caller or inputs",
                 "actions/checkout@", "vm_publish_stage.py check-inputs",
                 "/workflows/vm-artifact-export.yml\"", "vm_publish_stage.py authorize --metadata-dir",
                 "/actions/artifacts/$ARTIFACT_ID/zip", "vm_publish_stage.py unpack --archive", "oras login ghcr.io",
                 "stage.publication_plan(payload, selected)", "stage.absent_tag(status, raw)",
                 "subprocess.run(stage.push_argv(plan, exported), cwd=payload", "stage.pushed_digest(exported, plan)",
                 "os.environ['GITHUB_OUTPUT']",
                 "oras logout ghcr.io"]
        positions = [self.text.index(item) for item in order]
        self.assertEqual(positions, sorted(positions))
        self.assertIn("      - name: Remove registry login\n        if: always()\n", self.text)


if __name__ == "__main__":
    unittest.main(verbosity=2)
