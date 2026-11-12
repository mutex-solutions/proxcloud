"""Offline stdlib fixtures for the fail-closed scan gate; no scanner execution."""

from contextlib import redirect_stderr, redirect_stdout
from datetime import date, datetime, timedelta, timezone
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch


SCRIPT = Path(__file__).resolve().parents[1] / "check_scan_report.py"
SPEC = importlib.util.spec_from_file_location("check_scan_report", SCRIPT)
GATE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(GATE)
NOW = datetime(2026, 11, 12, 2, 0, tzinfo=timezone.utc)
IMAGE_ID = "sha256:" + "a" * 64
CLEAN = GATE.GateResult([], [], [], False)
REVIEW = datetime(2026, 12, 12, tzinfo=timezone.utc)
EXPIRY = datetime(2026, 12, 31, tzinfo=timezone.utc)


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


LAYER_A = {"DiffID": "sha256:" + "1" * 64}
LAYER_B = {"DiffID": "sha256:" + "2" * 64}


def vex_report():
    """Shape copied from real Trivy 0.76.2 --list-all-pkgs reports (livedee4,
    lived862 templates/base): pip 26.3 installed, its vendored bom.cdx.json
    surfaced as an SBOM-only setuptools 70.3.0 package, a newer real setuptools."""
    data = report(True)
    data["Results"][0]["Packages"] = [  # dpkg packages carry no FilePath.
        {"Name": "python3.11", "Version": "3.11.2-6", "Identifier": {"UID": "0d1"}, "AnalyzedBy": "dpkg", "Layer": LAYER_A}]
    data["Results"].append({"Target": "Python", "Class": "lang-pkgs", "Type": "python-pkg", "Packages": [
        {"Name": "pip", "Version": "26.3", "Identifier": {"PURL": "pkg:pypi/pip@26.3", "UID": "b6dc6f46963a9078"},
         "FilePath": "usr/local/lib/python3.11/site-packages/pip-26.3.dist-info/METADATA",
         "AnalyzedBy": "python-pkg", "Layer": LAYER_B},
        {"ID": "setuptools@70.3.0", "Name": "setuptools", "Version": "70.3.0",
         "Identifier": {"PURL": "pkg:pypi/setuptools@70.3.0", "UID": "171ad461d2f40666", "BOMRef": "pkg:pypi/setuptools@70.3.0"},
         "AnalyzedBy": "sbom", "Layer": LAYER_A},  # Trivy may attribute it to another layer.
        {"Name": "setuptools", "Version": "84.2.1", "Identifier": {"PURL": "pkg:pypi/setuptools@84.2.1", "UID": "620e0db5408cd993"},
         "FilePath": "usr/local/lib/python3.11/site-packages/setuptools-84.2.1.dist-info/METADATA",
         "AnalyzedBy": "python-pkg", "Layer": LAYER_B}],
        "Vulnerabilities": [{
            "VulnerabilityID": "CVE-2025-47273", "PkgID": "setuptools@70.3.0", "PkgName": "setuptools",
            "PkgIdentifier": {"PURL": "pkg:pypi/setuptools@70.3.0", "UID": "171ad461d2f40666", "BOMRef": "pkg:pypi/setuptools@70.3.0"},
            "InstalledVersion": "70.3.0", "FixedVersion": "78.1.1", "Status": "fixed", "Layer": LAYER_A,
            "PrimaryURL": "https://avd.aquasec.com/nvd/cve-2025-47273", "Severity": "HIGH"}]})
    return data


def python_result(data):
    return data["Results"][1]


def package(data, name, version):
    return next(p for p in python_result(data)["Packages"] if p["Name"] == name and p["Version"] == version)


def installed_package(name, version, uid):
    return {"Name": name, "Version": version, "Identifier": {"UID": uid}, "AnalyzedBy": "python-pkg",
            "FilePath": f"opt/venv/lib/python3.12/site-packages/{name}-{version}.dist-info/METADATA"}


class ReportTests(unittest.TestCase):
    def test_empty_results_valid_for_filesystem_and_identified_image(self):
        self.assertEqual(GATE.validate_report(report(), now=NOW), CLEAN)
        self.assertEqual(GATE.validate_report(report(True), IMAGE_ID, NOW), CLEAN)

    def test_filesystem_git_checkout_repository_type_valid(self):
        data = report()
        data["ArtifactType"] = "repository"
        self.assertEqual(GATE.validate_report(data, now=NOW), CLEAN)

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
        self.assertEqual(GATE.validate_report(data, now=NOW), CLEAN)

    def test_fixed_high_and_critical_block(self):
        data = report()
        data["Results"] = [{"Target": "go.mod", "Vulnerabilities": [finding(), finding("CRITICAL", "9")]}]
        for now in (NOW, REVIEW - timedelta(seconds=1), REVIEW, REVIEW + timedelta(days=400)):
            with self.subTest(now=now):
                gate = GATE.validate_report(data, now=now)
                self.assertEqual([f["severity"] for f in gate.blocking], ["HIGH", "CRITICAL"])
                self.assertEqual({f["class"] for f in gate.blocking}, {"blocking"})
                self.assertEqual((gate.tracked, gate.vex, gate.eol), ([], [], False))

    def test_unfixed_tracked_before_review_deadline(self):
        data = report()
        data["Results"] = [{"Target": "go.mod", "Vulnerabilities": [finding("CRITICAL", ""), finding(fixed=None)]}]
        del data["Results"][0]["Vulnerabilities"][1]["FixedVersion"]  # Trivy omits it when unfixed.
        # 2026-12-12T06:59+07:00 is still 2026-12-11 in UTC.
        for now in (NOW, REVIEW - timedelta(seconds=1),
                    datetime(2026, 12, 12, 6, 59, tzinfo=timezone(timedelta(hours=7)))):
            with self.subTest(now=now):
                gate = GATE.validate_report(data, now=now)
                self.assertEqual(gate.blocking, [])
                self.assertEqual(len(gate.tracked), 2)
                self.assertEqual({f["class"] for f in gate.tracked}, {"tracked"})
                self.assertEqual({f["fixed"] for f in gate.tracked}, {""})
                self.assertIn("2026-12-12", gate.tracked[0]["reason"])

    def test_unfixed_block_on_and_after_review_deadline(self):
        self.assertEqual(GATE.UNFIXED_REVIEW_BY, date(2026, 12, 12))
        data = report()
        data["Results"] = [{"Target": "go.mod", "Vulnerabilities": [finding("CRITICAL", ""), finding("HIGH", "")]}]
        for now in (REVIEW, REVIEW + timedelta(hours=23), datetime(2027, 6, 1, tzinfo=timezone.utc),
                    datetime(2026, 12, 12, 7, 0, tzinfo=timezone(timedelta(hours=7)))):
            with self.subTest(now=now):
                gate = GATE.validate_report(data, now=now)
                self.assertEqual(len(gate.blocking), 2)
                self.assertEqual(gate.tracked, [])
                self.assertIn("re-review deadline", gate.blocking[0]["reason"])

    def test_naive_now_is_rejected(self):
        with self.assertRaises(ValueError):
            GATE.validate_report(report(), now=datetime(2026, 11, 12))

    def test_lower_severities_are_not_gate_failures(self):
        data = report()
        data["Results"] = [{"Target": "go.mod", "Vulnerabilities": [finding(s, f) for s in ("UNKNOWN", "LOW", "MEDIUM") for f in ("", "9")]}]
        for now in (NOW, REVIEW):
            self.assertEqual(GATE.validate_report(data, now=now), CLEAN)

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
        self.assertEqual(GATE.validate_report(data, IMAGE_ID, NOW), GATE.GateResult([], [], [], True))


class VexTests(unittest.TestCase):
    def assertVex(self, data, now=NOW):
        gate = GATE.validate_report(data, IMAGE_ID, now)
        self.assertEqual((len(gate.vex), gate.blocking, gate.tracked), (1, [], []))
        self.assertEqual(gate.vex[0]["class"], "vex")
        self.assertIn("owner-approved VEX (expires 2026-12-31)", gate.vex[0]["reason"])
        return gate

    def assertBlocked(self, data, now=NOW):
        gate = GATE.validate_report(data, IMAGE_ID, now)
        self.assertEqual(gate.vex, [])
        self.assertEqual([(f["id"], f["class"]) for f in gate.blocking], [(python_result(data)["Vulnerabilities"][0]["VulnerabilityID"], "blocking")])

    def test_exactly_one_owner_vex_constant(self):
        self.assertEqual(GATE.OWNER_VEX[:5], ("CVE-2025-47273", "setuptools", "70.3.0", "pip", "26.3"))
        self.assertEqual(GATE.OWNER_VEX.expires, date(2026, 12, 31))
        source = SCRIPT.read_text(encoding="utf-8")
        for evidence in ("5c78ac0296d2e95805bdad6b8dfbdaa292eab2506bf0e519f4a4c1a6fc4310e8",
                         "f3b60b20ffaadd4415ed7957212a38ac63fa4c5fa375bd0f4a797777940fad13",
                         "pip/_vendor/bom.cdx.json", "package_index.py"):
            self.assertIn(evidence, source)
        self.assertEqual(source.count("OwnerVex("), 1)

    def test_realistic_vendored_sbom_component_is_visible_vex(self):
        gate = self.assertVex(vex_report())
        self.assertEqual((gate.vex[0]["package"], gate.vex[0]["installed"], gate.vex[0]["fixed"]), ("setuptools", "70.3.0", "78.1.1"))
        # Still applies when unfixed findings start blocking, and across layers.
        self.assertVex(vex_report(), REVIEW)
        self.assertVex(vex_report(), EXPIRY - timedelta(seconds=1))

    def test_pip_in_other_result_and_multiple_pip_26_3_still_match(self):
        data = vex_report()
        pip = package(data, "pip", "26.3")
        python_result(data)["Packages"].remove(pip)
        data["Results"].append({"Target": "venv", "Class": "lang-pkgs", "Type": "python-pkg",
                                "Packages": [pip, installed_package("pip", "26.3", "e960d930f09f06ce")]})
        self.assertVex(data)

    def test_vex_expires(self):
        for now in (EXPIRY, EXPIRY + timedelta(hours=12), datetime(2027, 1, 1, tzinfo=timezone.utc),
                    datetime(2026, 12, 31, 7, 0, tzinfo=timezone(timedelta(hours=7)))):
            with self.subTest(now=now):
                self.assertBlocked(vex_report(), now)

    def test_identity_fields_must_match_exactly(self):
        for key, value in (("VulnerabilityID", "CVE-2025-47274"), ("PkgName", "Setuptools"),
                           ("PkgName", "pip"), ("InstalledVersion", "70.3.1"),
                           ("InstalledVersion", "70.3.0.post1")):
            with self.subTest(key=key, value=value):
                data = vex_report()
                python_result(data)["Vulnerabilities"][0][key] = value
                self.assertBlocked(data)

    def test_unfixed_lookalike_is_tracked_not_vex(self):
        data = vex_report()
        vuln = python_result(data)["Vulnerabilities"][0]
        vuln["VulnerabilityID"], vuln["FixedVersion"] = "CVE-2026-9999", ""
        gate = GATE.validate_report(data, IMAGE_ID, NOW)
        self.assertEqual((gate.vex, gate.blocking, len(gate.tracked)), ([], [], 1))

    def test_vulnerability_with_pkgpath_is_not_vex(self):
        for value in ("usr/local/lib/python3.11/site-packages/setuptools", "", None):
            with self.subTest(value=value):
                data = vex_report()
                python_result(data)["Vulnerabilities"][0]["PkgPath"] = value
                self.assertBlocked(data)

    def test_package_must_be_same_result_unique_uid_and_sbom_derived(self):
        mutations = {
            "no PkgIdentifier": lambda d: python_result(d)["Vulnerabilities"][0].pop("PkgIdentifier"),
            "unknown UID": lambda d: python_result(d)["Vulnerabilities"][0]["PkgIdentifier"].update(UID="ffff"),
            "empty UID": lambda d: python_result(d)["Vulnerabilities"][0]["PkgIdentifier"].update(UID=""),
            "UID only in other Result": lambda d: d["Results"][0]["Packages"].append(
                python_result(d)["Packages"].pop(1)),
            "no Packages (no --list-all-pkgs)": lambda d: python_result(d).pop("Packages"),
            "duplicate UID": lambda d: python_result(d)["Packages"].append(dict(package(d, "setuptools", "70.3.0"))),
            "AnalyzedBy python-pkg": lambda d: package(d, "setuptools", "70.3.0").update(AnalyzedBy="python-pkg"),
            "AnalyzedBy missing": lambda d: package(d, "setuptools", "70.3.0").pop("AnalyzedBy"),
            "has FilePath": lambda d: package(d, "setuptools", "70.3.0").update(
                FilePath="usr/lib/python3/dist-packages/setuptools-70.3.0.dist-info/METADATA"),
            "empty FilePath key": lambda d: package(d, "setuptools", "70.3.0").update(FilePath=""),
            "UID names other package": lambda d: package(d, "setuptools", "70.3.0").update(Name="wheel"),
        }
        for label, mutate in mutations.items():
            with self.subTest(label):
                data = vex_report()
                mutate(data)
                self.assertBlocked(data)

    def test_every_installed_pip_must_be_26_3_and_one_must_exist(self):
        mutations = {
            "older pip elsewhere (browser virtualenv cache)": lambda d: d["Results"].append(
                {"Target": "venv", "Packages": [installed_package("pip", "26.2.1", "fdc8aa74281b32ff")]}),
            "installed pip has other version": lambda d: package(d, "pip", "26.3").update(Version="26.4"),
            "no installed pip": lambda d: python_result(d)["Packages"].remove(package(d, "pip", "26.3")),
            "pip only as SBOM component": lambda d: [package(d, "pip", "26.3").update(AnalyzedBy="sbom"),
                                                     package(d, "pip", "26.3").pop("FilePath")],
            "pip without FilePath from a real analyzer": lambda d: package(d, "pip", "26.3").pop("FilePath"),
            "non-normalized pip name": lambda d: d["Results"].append(
                {"Target": "venv", "Packages": [installed_package("PIP", "25.0", "x1")]}),
            "malformed Packages": lambda d: d["Results"].append({"Target": "venv", "Packages": {}}),
        }
        for label, mutate in mutations.items():
            with self.subTest(label):
                data = vex_report()
                mutate(data)
                self.assertBlocked(data)

    def test_physically_installed_setuptools_70_3_0_anywhere_disables_vex(self):
        for target_result in (1, 0, 2):
            with self.subTest(result=target_result):
                data = vex_report()
                data["Results"].append({"Target": "extra", "Packages": []})
                data["Results"][target_result].setdefault("Packages", []).append(
                    installed_package("setuptools", "70.3.0", "4d5fb115b3c71d02"))
                self.assertBlocked(data)

    def test_vex_is_never_silently_dropped_in_cli_output(self):
        with tempfile.TemporaryDirectory() as temp:
            db, scan = self.write(Path(temp), vex_report())
            out = io.StringIO()
            with redirect_stdout(out), redirect_stderr(io.StringIO()):
                code = GATE.main(["--db-metadata", str(db), "--expected-image-id", IMAGE_ID, str(scan)], now=NOW)
            self.assertEqual(code, 0)
            lines = out.getvalue().splitlines()
            record = json.loads(lines[0])
            self.assertEqual((record["class"], record["id"], record["fixed"]), ("vex", "CVE-2025-47273", "78.1.1"))
            self.assertIn("owner-approved VEX CVE-2025-47273 (expires 2026-12-31): 1", lines[-1])
        with tempfile.TemporaryDirectory() as temp:
            db, scan = self.write(Path(temp), vex_report(), EXPIRY)  # Fresh DB on expiry day.
            out = io.StringIO()
            with redirect_stdout(out), redirect_stderr(io.StringIO()):
                code = GATE.main(["--db-metadata", str(db), "--expected-image-id", IMAGE_ID, str(scan)], now=EXPIRY)
            self.assertEqual(code, 1)
            self.assertEqual(json.loads(out.getvalue().splitlines()[0])["class"], "blocking")

    @staticmethod
    def write(root, data, stamp=NOW):
        db, scan = root / "db.json", root / "scan.json"
        db.write_text(json.dumps({"Version": 2, "UpdatedAt": stamp.isoformat(), "DownloadedAt": stamp.isoformat()}), encoding="utf-8")
        scan.write_text(json.dumps(data), encoding="utf-8")
        return db, scan


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
                data["Results"] = [{"Target": "go.mod", "Vulnerabilities": [finding("CRITICAL", "1.2.4")]}]
                scan.write_text(json.dumps(data), encoding="utf-8")
                self.assertEqual(GATE.main(args), 1)
                scan.write_text('{"Results": [], "Results": []}', encoding="utf-8")
                self.assertEqual(GATE.main(args), 2)
                scan.write_text('{"Results":', encoding="utf-8")
                self.assertEqual(GATE.main(args), 2)
                self.assertEqual(GATE.main(["--db-metadata", str(db), "--expected-image-id", IMAGE_ID]), 2)

    def test_any_crash_is_invalid_evidence_never_findings(self):
        # Deep nesting raises RecursionError inside json; it must be exit 2, not 1.
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            db = root / "db.json"
            scan = root / "scan.json"
            current = datetime.now(timezone.utc).isoformat()
            db.write_text(json.dumps({"Version": 2, "UpdatedAt": current, "DownloadedAt": current}), encoding="utf-8")
            scan.write_text("[" * 200000 + "]" * 200000, encoding="utf-8")
            with redirect_stdout(io.StringIO()), redirect_stderr(io.StringIO()):
                self.assertEqual(GATE.main(["--db-metadata", str(db), str(scan)]), 2)
                deep_db = root / "deep-db.json"
                deep_db.write_text("{\"a\":" * 200000 + "1" + "}" * 200000, encoding="utf-8")
                self.assertEqual(GATE.main(["--db-metadata", str(deep_db)]), 2)
            scan.write_text(json.dumps(report()), encoding="utf-8")
            for error in (RecursionError("depth"), MemoryError(), KeyError("x"), AttributeError("y")):
                with self.subTest(error=type(error).__name__), patch.object(GATE, "validate_report", side_effect=error), \
                        redirect_stdout(io.StringIO()), redirect_stderr(io.StringIO()):
                    self.assertEqual(GATE.main(["--db-metadata", str(db), str(scan)]), 2)

    def run_cli(self, data, now):
        with tempfile.TemporaryDirectory() as temp:
            db, scan = VexTests.write(Path(temp), data, now)
            out = io.StringIO()
            with redirect_stdout(out), redirect_stderr(io.StringIO()):
                code = GATE.main(["--db-metadata", str(db), str(scan)], now=now)
        lines = out.getvalue().splitlines()
        return code, [json.loads(line) for line in lines[:-1]], lines[-1]

    def test_tracked_exit_zero_before_deadline_and_one_after(self):
        data = report()
        data["Results"] = [{"Target": "go.mod", "Vulnerabilities": [finding("CRITICAL", "")]}]
        code, records, summary = self.run_cli(data, NOW)
        self.assertEqual(code, 0)
        self.assertEqual([(r["class"], r["fixed"]) for r in records], [("tracked", "NO FIX AVAILABLE")])
        self.assertIn("tracked without fix (non-blocking until 2026-12-12): 1", summary)
        code, records, summary = self.run_cli(data, REVIEW)
        self.assertEqual(code, 1)
        self.assertEqual([r["class"] for r in records], ["blocking"])
        self.assertIn("Blocking HIGH/CRITICAL: 1", summary)

    def test_each_finding_is_one_json_line_with_class(self):
        data = report()
        data["Results"] = [{"Target": "go.mod\n::error::injected", "Vulnerabilities": [
            finding("CRITICAL", ""), finding("HIGH", "1.2.4"), finding("LOW", "")]}]
        code, records, _ = self.run_cli(data, NOW)
        self.assertEqual(code, 1)
        self.assertEqual([r["class"] for r in records], ["blocking", "tracked"])
        self.assertTrue(all(r["target"] == "go.mod\n::error::injected" for r in records))

    def test_eol_blocks_even_without_findings(self):
        data = report(True)
        data["Metadata"]["OS"]["Eosl"] = True
        with tempfile.TemporaryDirectory() as temp:
            db, scan = VexTests.write(Path(temp), data)
            with redirect_stdout(io.StringIO()), redirect_stderr(io.StringIO()):
                self.assertEqual(GATE.main(["--db-metadata", str(db), "--expected-image-id", IMAGE_ID, str(scan)], now=NOW), 1)


if __name__ == "__main__":
    unittest.main()
