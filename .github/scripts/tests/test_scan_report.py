"""Offline stdlib fixtures for the fail-closed scan gate; no scanner execution."""

from contextlib import redirect_stderr, redirect_stdout
from datetime import datetime, timedelta, timezone
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / "check_scan_report.py"
SPEC = importlib.util.spec_from_file_location("check_scan_report", SCRIPT)
GATE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(GATE)
NOW = datetime(2026, 11, 12, 2, 0, tzinfo=timezone.utc)
IMAGE_ID = "sha256:" + "a" * 64


def database():
    return {"Version": 2, "UpdatedAt": NOW.isoformat(), "DownloadedAt": NOW.isoformat()}


def report(image=False):
    metadata = {"ImageID": IMAGE_ID, "OS": {"Family": "debian", "Name": "12.9"}} if image else {}
    return {"SchemaVersion": 2, "ArtifactName": "fixture", "Metadata": metadata,
            "ArtifactType": "container_image" if image else "filesystem",
            "Results": [{"Target": "fixture (debian 12.9)", "Class": "os-pkgs", "Type": "debian"}] if image else []}


def finding(severity="HIGH", fixed="1.2.4"):
    return {"VulnerabilityID": "CVE-2026-0001", "PkgName": "fixture-package",
            "InstalledVersion": "1.2.3", "FixedVersion": fixed, "Severity": severity}


class ReportTests(unittest.TestCase):
    def test_empty_results_valid_for_filesystem_and_identified_image(self):
        self.assertEqual(GATE.validate_report(report()), ([], False))
        self.assertEqual(GATE.validate_report(report(True), IMAGE_ID), ([], False))

    def test_image_requires_matching_os_package_scan_result(self):
        for results in ([], [{"Target": "binary", "Class": "lang-pkgs", "Type": "gobinary"}],
                        [{"Target": "other OS", "Class": "os-pkgs", "Type": "alpine"}]):
            with self.subTest(results=results):
                data = report(True)
                data["Results"] = results
                with self.assertRaises(GATE.InvalidReport):
                    GATE.validate_report(data, IMAGE_ID)

    def test_null_or_omitted_vulnerabilities_valid(self):
        data = report()
        data["Results"] = [{"Target": "go.mod"}, {"Target": "package-lock.json", "Vulnerabilities": None}]
        self.assertEqual(GATE.validate_report(data), ([], False))

    def test_all_high_and_critical_fail_including_unfixed(self):
        data = report()
        data["Results"] = [{"Target": "go.mod", "Vulnerabilities": [finding(), finding("CRITICAL", "")]}]
        findings, eol = GATE.validate_report(data)
        self.assertEqual(len(findings), 2)
        self.assertEqual(findings[1]["fixed"], "NO FIX AVAILABLE")
        self.assertFalse(eol)

    def test_lower_severities_are_not_gate_failures(self):
        data = report()
        data["Results"] = [{"Target": "go.mod", "Vulnerabilities": [finding(s) for s in ("UNKNOWN", "LOW", "MEDIUM")]}]
        self.assertEqual(GATE.validate_report(data), ([], False))

    def test_report_shape_fails_closed(self):
        for key, value in (("SchemaVersion", 1), ("SchemaVersion", True), ("ArtifactName", ""),
                           ("ArtifactType", "unknown"), ("Metadata", None), ("Results", None),
                           ("Results", {}), ("Results", [None])):
            with self.subTest(key=key, value=value):
                data = report()
                data[key] = value
                with self.assertRaises(GATE.InvalidReport):
                    GATE.validate_report(data)
        data = report()
        del data["Results"]
        with self.assertRaises(GATE.InvalidReport):
            GATE.validate_report(data)

    def test_malformed_vulnerability_fails_closed(self):
        for key, value in (("Severity", ""), ("Severity", "high"), ("VulnerabilityID", None),
                           ("PkgName", ""), ("InstalledVersion", None), ("FixedVersion", [])):
            with self.subTest(key=key):
                vuln = finding()
                vuln[key] = value
                data = report()
                data["Results"] = [{"Target": "go.mod", "Vulnerabilities": [vuln]}]
                with self.assertRaises(GATE.InvalidReport):
                    GATE.validate_report(data)
        for value in ({}, "not-an-array", [None]):
            data = report()
            data["Results"] = [{"Target": "go.mod", "Vulnerabilities": value}]
            with self.assertRaises(GATE.InvalidReport):
                GATE.validate_report(data)

    def test_image_identity_and_os_required(self):
        mutations = [lambda d: d["Metadata"].pop("ImageID"),
                     lambda d: d["Metadata"].update(ImageID="sha256:wrong"),
                     lambda d: d["Metadata"].pop("OS"),
                     lambda d: d["Metadata"]["OS"].update(Family="unknown"),
                     lambda d: d["Metadata"]["OS"].pop("Name"),
                     lambda d: d["Metadata"]["OS"].update(Eosl="false"),
                     lambda d: d.update(ArtifactType="filesystem")]
        for mutate in mutations:
            data = report(True)
            mutate(data)
            with self.assertRaises(GATE.InvalidReport):
                GATE.validate_report(data, IMAGE_ID)

    def test_end_of_life_image_fails_gate(self):
        data = report(True)
        data["Metadata"]["OS"]["Eosl"] = True
        self.assertEqual(GATE.validate_report(data, IMAGE_ID), ([], True))


class DatabaseTests(unittest.TestCase):
    def test_fresh_and_boundary_timestamps(self):
        data = database()
        self.assertEqual(GATE.validate_db(data, NOW), data)
        data["UpdatedAt"] = (NOW - timedelta(hours=24)).isoformat()
        data["DownloadedAt"] = (NOW + timedelta(minutes=5)).isoformat()
        GATE.validate_db(data, NOW)

    def test_stale_missing_invalid_future_or_naive_timestamp(self):
        for key in ("UpdatedAt", "DownloadedAt"):
            for value in (None, "", "not-a-date", "2026-11-12T02:00:00",
                          (NOW - timedelta(hours=24, seconds=1)).isoformat(),
                          (NOW + timedelta(minutes=5, seconds=1)).isoformat()):
                with self.subTest(key=key, value=value):
                    data = database()
                    data[key] = value
                    with self.assertRaises(GATE.InvalidReport):
                        GATE.validate_db(data, NOW)

    def test_db_schema_version_required(self):
        for value in (None, 1, True, "2"):
            data = database()
            data["Version"] = value
            with self.assertRaises(GATE.InvalidReport):
                GATE.validate_db(data, NOW)


class CliTests(unittest.TestCase):
    def test_exit_codes_for_clean_findings_and_invalid_evidence(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            db = root / "db.json"
            scan = root / "scan.json"
            current = datetime.now(timezone.utc).isoformat()
            db.write_text(json.dumps({"Version": 2, "UpdatedAt": current, "DownloadedAt": current}), encoding="utf-8")
            args = ["--db-metadata", str(db), str(scan)]
            with redirect_stdout(io.StringIO()), redirect_stderr(io.StringIO()):
                self.assertEqual(GATE.main(["--db-metadata", str(db)]), 0)
                self.assertEqual(GATE.main(args), 2)  # missing report
                scan.write_text(json.dumps(report()), encoding="utf-8")
                self.assertEqual(GATE.main(args), 0)
                data = report()
                data["Results"] = [{"Target": "go.mod", "Vulnerabilities": [finding("CRITICAL", "")]}]
                scan.write_text(json.dumps(data), encoding="utf-8")
                self.assertEqual(GATE.main(args), 1)
                scan.write_text('{"Results": [], "Results": []}', encoding="utf-8")
                self.assertEqual(GATE.main(args), 2)
                scan.write_text('{"Results":', encoding="utf-8")
                self.assertEqual(GATE.main(args), 2)
                self.assertEqual(GATE.main(["--db-metadata", str(db), "--expected-image-id", IMAGE_ID]), 2)


if __name__ == "__main__":
    unittest.main()
