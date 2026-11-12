"""Offline base registry resolver/scanner fixtures; subprocesses always mocked."""

import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch


SCRIPT = Path(__file__).resolve().parents[1] / "scan_base_images.py"
SPEC = importlib.util.spec_from_file_location("scan_base_images", SCRIPT)
SCAN = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(SCAN)
SHA = "a" * 40
INDEX = "sha256:" + "b" * 64
CHILD = "sha256:" + "c" * 64
CONFIG = "sha256:" + "d" * 64
IMAGE_TYPE = "application/vnd.oci.image.manifest.v1+json"
INDEX_TYPE = "application/vnd.oci.image.index.v1+json"


def manifest():
    return {"mediaType": IMAGE_TYPE, "config": {"digest": CONFIG}}


def entry(os="linux", arch="amd64", variant="", digest=CHILD):
    return {"mediaType": IMAGE_TYPE, "digest": digest,
            "platform": {"os": os, "architecture": arch, "variant": variant}}


class BaseTests(unittest.TestCase):
    def test_fixed_thirteen_source_refs_and_scope_exclusions(self):
        self.assertEqual(len(SCAN.TARGETS), 13)
        self.assertEqual(len({x[0] for x in SCAN.TARGETS}), 13)
        self.assertEqual(len({x[1] for x in SCAN.TARGETS}), 13)
        refs = dict((x[0], x[1]) for x in SCAN.TARGETS)
        self.assertEqual(refs["go-builder"], "docker.io/library/golang:1.27.4-alpine")
        self.assertEqual(refs["external-python"], "docker.io/library/python:3.12-slim@sha256:05a994d2344eb364cc186910f4fc8786a38215f39f00d0acc25bff33a08f4177")
        self.assertEqual(sum("@sha256:" in x for x in refs.values()), 7)
        for name, ref, role, scopes in SCAN.TARGETS:
            self.assertNotIn("gpu", name)
            self.assertNotIn("pytorch", ref)
            self.assertNotIn("security-scan.local", ref)
            self.assertTrue(role and scopes)

    def test_repository_strips_tag_but_preserves_registry_port(self):
        for ref, expected in (("host:5000/a/b:tag@" + INDEX, "host:5000/a/b"),
                              ("host:5000/a/b", "host:5000/a/b"),
                              ("docker.io/library/a@" + INDEX, "docker.io/library/a")):
            self.assertEqual(SCAN.repository(ref), expected)

    def test_platform_selection_excludes_attestations_and_other_arches(self):
        self.assertEqual(SCAN.select_manifest({"manifests": [entry("unknown", "unknown"),
                         entry(arch="arm64"), entry()]}), CHILD)
        self.assertEqual(SCAN.select_manifest({"manifests": [entry(variant="v1")]}), CHILD)
        for entries in ([], [entry(), entry()], [entry(arch="arm64")],
                        [entry(variant="v2")], [{}], [entry(digest="bad")],
                        [dict(entry(), mediaType=INDEX_TYPE)]):
            with self.subTest(entries=entries), self.assertRaises(ValueError):
                SCAN.select_manifest({"manifests": entries})

    def test_index_resolution_pins_before_raw_inspection(self):
        values = [{"digest": INDEX}, {"mediaType": INDEX_TYPE, "manifests": [entry()]}, manifest()]
        with patch.object(SCAN, "inspect_json", side_effect=values) as inspect:
            resolved = SCAN.resolve_image("docker.io/library/a:mutable", 999999999)
        self.assertEqual(resolved["index_digest"], INDEX)
        self.assertEqual(resolved["config_digest"], CONFIG)
        self.assertEqual(resolved["immutable_ref"], "docker.io/library/a@" + CHILD)
        self.assertEqual(inspect.call_args_list[1].args[0], "docker.io/library/a@" + INDEX)
        self.assertEqual(inspect.call_args_list[2].args[0], "docker.io/library/a@" + CHILD)

    def test_direct_manifest_platform_is_checked_not_assumed(self):
        for config, success in (({"os": "linux", "architecture": "amd64"}, True),
                                ({"os": "linux", "architecture": "arm64"}, False),
                                ({}, False), ({"os": "windows", "architecture": "amd64"}, False)):
            with patch.object(SCAN, "inspect_json", side_effect=[{"digest": CHILD}, manifest(), config]):
                if success:
                    result = SCAN.resolve_image("docker.io/library/a@" + CHILD, 999999999)
                    self.assertIsNone(result["index_digest"])
                    self.assertEqual(result["platform_manifest_digest"], CHILD)
                else:
                    with self.assertRaises(ValueError):
                        SCAN.resolve_image("docker.io/library/a@" + CHILD, 999999999)

    def test_missing_descriptor_source_pin_mismatch_and_invalid_config_fail(self):
        values = ([{}], [{"digest": CHILD}],
                  [{"digest": INDEX}, {"mediaType": "unknown"}],
                  [{"digest": INDEX}, {"mediaType": INDEX_TYPE, "manifests": [entry()]},
                   {"mediaType": IMAGE_TYPE, "config": {"digest": "bad"}}])
        for value in values:
            with patch.object(SCAN, "inspect_json", side_effect=value), self.assertRaises(ValueError):
                SCAN.resolve_image("docker.io/library/a@" + INDEX, 999999999)

    def test_inspect_rejects_errors_nonobject_duplicate_or_oversized_json(self):
        for rc, stdout in ((1, "{}"), (0, "[]"), (0, '{"digest":1,"digest":2}'),
                           (0, "x" * (8 * 1024 * 1024 + 1))):
            with patch.object(SCAN, "run_command", return_value=subprocess.CompletedProcess([], rc, stdout)), self.assertRaises(ValueError):
                SCAN.inspect_json("fixture", ["--raw"], 999999999)

    SUMMARY = ("Blocking HIGH/CRITICAL: 1; tracked without fix (non-blocking until 2026-12-12): 0; "
               "owner-approved VEX CVE-2025-47273 (expires 2026-12-31): 0; image OS EOL: False\n")

    def scan_fixture(self, scan_rc=0, gate_rc=0, timeout=False, gate_output=SUMMARY):
        calls = []
        resolved = {"ref": SCAN.TARGETS[0][1], "config_digest": CONFIG,
                    "immutable_ref": "docker.io/library/golang@" + CHILD}
        def fake_run(argv, deadline, limit, **kwargs):
            calls.append((argv, limit))
            if timeout:
                raise subprocess.TimeoutExpired(argv, limit)
            if argv[0] == SCAN.sys.executable:
                kwargs["stdout"].write(gate_output)
                return subprocess.CompletedProcess(argv, gate_rc)
            return subprocess.CompletedProcess(argv, scan_rc)
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            with patch.object(SCAN, "resolve_image", return_value=resolved), patch.object(SCAN, "run_command", side_effect=fake_run):
                status = SCAN.scan_image(root, SCAN.TARGETS[0], SHA, 999999999)
            target = root / "result/bases/go-builder"
            self.assertEqual((target / "status.txt").read_text().strip(), str(status))
            self.assertEqual(json.loads((target / "source.json").read_text())["source_sha"], SHA)
            self.assertTrue((target / "gate.txt").exists())
        return status, calls

    def test_statuses_remote_only_immutable_scan_and_config_identity_gate(self):
        for scan_rc, gate_rc, expected in ((0, 0, 0), (0, 1, 1), (0, 2, 2),
                                           (1, 0, 2), (1, 1, 1), (3, 1, 2)):
            with self.subTest(scan_rc=scan_rc, gate_rc=gate_rc):
                status, calls = self.scan_fixture(scan_rc, gate_rc)
                self.assertEqual(status, expected)
                self.assertEqual([limit for _, limit in calls], [600, 30])
                scan = calls[0][0]
                self.assertEqual(scan[scan.index("--image-src") + 1], "remote")
                self.assertIn("--list-all-pkgs", scan)
                self.assertIn("--ignore-unfixed=false", scan)
                self.assertTrue(scan[-1].endswith("@" + CHILD))
                self.assertIn(CONFIG, calls[1][0])
        self.assertEqual(self.scan_fixture(timeout=True)[0], 2)

    def test_gate_exit_without_completed_summary_is_incomplete_not_report_only(self):
        # A crashed gate (e.g. traceback, exit 1) must never become a report-only success.
        for gate_rc in (0, 1):
            for output in ("", "Traceback (most recent call last):\nRecursionError: maximum recursion depth\n",
                           self.SUMMARY + "trailing noise\n", "Blocking HIGH/CRITICAL: x; image OS EOL: maybe\n"):
                with self.subTest(gate_rc=gate_rc, output=output[:30]):
                    self.assertEqual(self.scan_fixture(0, gate_rc, gate_output=output)[0], 2)
        crashed, _ = self.scan_fixture(0, 1, gate_output="Traceback\n")
        final, _ = self.run_main([0] * 12 + [crashed])
        self.assertEqual(final, 2)

    def test_source_allowlist_cannot_be_overridden(self):
        with self.assertRaises(ValueError):
            SCAN.scan_image(Path("unused"), ("../escape", "bad", "", ()), SHA, 999999999)

    def run_main(self, side_effect):
        with tempfile.TemporaryDirectory() as directory, patch.object(SCAN.Path, "cwd", return_value=Path(directory)), patch.dict(SCAN.os.environ, SOURCE_SHA=SHA), patch.object(SCAN, "scan_image", side_effect=side_effect) as scan:
            final = SCAN.main()
            self.assertEqual(scan.call_count, 13)
            summary = json.loads((Path(directory) / "result/base-status.json").read_text())
        self.assertEqual(summary["status"], final)
        return final, summary

    def test_main_collects_all_thirteen_even_after_failures(self):
        final, summary = self.run_main([2, 1] + [0] * 11)
        self.assertEqual(final, 2)
        self.assertEqual(len(summary["targets"]), 13)
        self.assertEqual(summary["missing"], [])
        self.assertEqual(summary["report_only_findings"], ["web-builder"])

    def test_report_only_findings_and_eol_exit_zero(self):
        for statuses in ([1] * 13, [1] + [0] * 12, [0] * 13):
            with self.subTest(statuses=statuses):
                final, summary = self.run_main(statuses)
                self.assertEqual(final, 0)
                self.assertEqual(summary["targets"], {spec[0]: s for spec, s in zip(SCAN.TARGETS, statuses)})
                self.assertEqual(len(summary["report_only_findings"]), statuses.count(1))
                self.assertIn("report-only", summary["policy"])

    def test_incomplete_or_missing_base_scan_still_fails(self):
        final, summary = self.run_main([1] * 12 + [2])
        self.assertEqual(final, 2)
        final, summary = self.run_main([1] * 12 + [OSError("disk full")])
        self.assertEqual(final, 2)
        self.assertEqual(summary["missing"], ["external-python"])
        self.assertEqual(summary["errors"][0]["targetid"], "external-python")

    def test_gate_findings_recorded_per_target_but_job_report_only(self):
        # Gate exit 1 (findings/EOL) keeps per-target status/evidence; main exits 0.
        status, _ = self.scan_fixture(scan_rc=1, gate_rc=1)
        self.assertEqual(status, 1)
        final, summary = self.run_main([status] * 13)
        self.assertEqual(final, 0)
        self.assertEqual(len(summary["report_only_findings"]), 13)

    def test_wall_budget_and_source_sha_fail_closed(self):
        with patch.object(SCAN.time, "monotonic", return_value=100), patch.object(SCAN.subprocess, "run") as run:
            SCAN.run_command(["fixture"], 120, 600)
            self.assertEqual(run.call_args.kwargs["timeout"], 15)
            with self.assertRaises(TimeoutError):
                SCAN.run_command(["fixture"], 104, 600)
        for sha in ("", "A" * 40, SHA + ";echo bad"):
            with patch.dict(SCAN.os.environ, SOURCE_SHA=sha), patch.object(SCAN, "scan_image") as scan:
                self.assertEqual(SCAN.main(), 2)
                scan.assert_not_called()


if __name__ == "__main__":
    unittest.main()
