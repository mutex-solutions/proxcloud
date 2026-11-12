"""Offline fixtures only; renderer never downloads or executes scanner artifacts."""
from datetime import datetime, timedelta, timezone
import importlib.util
import json
from pathlib import Path
import sys
import tempfile
import unittest

SCRIPTS = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(SCRIPTS))
SPEC = importlib.util.spec_from_file_location("security_comment", SCRIPTS / "render_security_comment.py")
R = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(R)
SHA, IMAGE = "a" * 40, "sha256:" + "b" * 64
NOW = datetime(2026, 11, 12, tzinfo=timezone.utc)
URL = "https://github.com/owner/repo/actions/runs/123"


class CommentTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)

    def fixture(self, target="filesystem", vulns=(), source=SHA, age=0):
        artifact = "base-images" if target.startswith("base/") else ("template-images" if target.startswith("template/") else target)
        base = self.root / f"security-{artifact}-{SHA}"
        folder = base / ("bases" if artifact == "base-images" else "templates") / target.split("/")[1] if artifact in ("base-images", "template-images") else base
        folder.mkdir(parents=True, exist_ok=True)
        (base / "source-sha.txt").write_text(source + "\n")
        stamp = (NOW - timedelta(hours=age)).isoformat()
        (base / "db-metadata.json").write_text(json.dumps({"Version": 2, "UpdatedAt": stamp, "DownloadedAt": stamp}))
        image = target != "filesystem"
        data = {"SchemaVersion": 2, "ArtifactName": "ignored", "ArtifactType": "container_image" if image else "repository",
                "Metadata": {"ImageID": IMAGE, "OS": {"Family": "alpine", "Name": "3.23"}} if image else {},
                "Results": [{"Target": "untrusted/binary", "Class": "os-pkgs", "Type": "alpine", "Vulnerabilities": list(vulns)}]}
        (folder / "scan.json").write_text(json.dumps(data))
        (folder / "image-id.txt").write_text(IMAGE)
        return folder, data

    def vuln(self, identifier="CVE-2026-1001", severity="HIGH", package="pkg"):
        return {"VulnerabilityID": identifier, "Severity": severity, "PkgName": package,
                "InstalledVersion": "1", "FixedVersion": "2", "PrimaryURL": "https://nvd.nist.gov/vuln/detail/" + identifier}

    def render(self, **kwargs):
        return R.render(self.root, SHA, URL, "failure", "failure", "failure", now=NOW, **kwargs)

    def test_missing_and_source_mismatch_are_not_zero(self):
        self.assertIn("| filesystem | N/A", self.render())
        self.fixture(source="c" * 40)
        self.assertIn("INVALID evidence", self.render())

    def test_all_outputs_and_nested_template_paths(self):
        for name in (*R.PRODUCTS, *("template/" + n for n in R.TEMPLATES), *("base/" + n for n in R.BASES)):
            self.fixture(name)
        body = self.render()
        self.assertIn("Coverage: 32/32", body)
        self.assertIn("| template/gemini | 0 | 0", body)
        self.assertIn("| base/external-python | 0 | 0", body)
        self.assertNotIn("all-clear for the whole supply chain.\n0", body)

    def test_dedup_and_critical_first_deterministic(self):
        high, critical = self.vuln(), self.vuln("CVE-2026-1002", "CRITICAL")
        self.fixture(vulns=[high, critical, high])
        body = self.render()
        self.assertIn("| filesystem | 1 | 1 | 2 / 0", body)
        self.assertLess(body.index("[CVE-2026-1002]"), body.index("[CVE-2026-1001]"))
        self.fixture(vulns=[critical, high])
        self.assertEqual(body, self.render())

    def test_html_mentions_markdown_and_unknown_ids_are_text(self):
        item = self.vuln("unknown@everyone", package="<script>@team`|[x](evil)\\\n" + "z" * 600)
        item["Title"] = "DO NOT PUBLISH TITLE"
        item["PrimaryURL"] = "https://evil.invalid/owned"
        self.fixture(vulns=[item])
        body = self.render()
        for forbidden in ("<script>", "@team", "@everyone", "DO NOT PUBLISH TITLE", "evil.invalid", "[x](evil)"):
            self.assertNotIn(forbidden, body)
        self.assertIn("&#64;", body)
        self.assertIn("&#96;", body)
        self.assertIn("…", body)

    def test_invalid_os_id_and_stale_database(self):
        folder, data = self.fixture("api", age=25)
        self.assertIn("| api | N/A", self.render())
        self.fixture("api")
        for metadata in ({"ImageID": IMAGE}, {"ImageID": "wrong", "OS": {"Family": "alpine", "Name": "3"}}):
            data["Metadata"] = metadata
            (folder / "scan.json").write_text(json.dumps(data))
            self.assertIn("| api | N/A", self.render())

    def test_symlink_and_size_limits(self):
        folder, _ = self.fixture()
        (folder / "scan.json").unlink()
        (folder / "scan.json").symlink_to(self.root / "outside.json")
        self.assertIn("INVALID evidence", self.render())
        self.assertRaises(ValueError, R.Evidence(self.root).read, "../outside")

    def test_large_template_inventory_keeps_findings_visible(self):
        folder, data = self.fixture("template/base", vulns=[self.vuln()])
        # Actual package-inclusive reports from the inherited base exceed 20 MiB.
        data["Results"][0]["Packages"] = [{"Name": "fixture", "Description": "x" * (20 * 1024 * 1024)}]
        (folder / "scan.json").write_text(json.dumps(data))
        body = self.render()
        self.assertIn("| template/base | 0 | 1 |", body)
        self.assertIn("CVE-2026-1001", body)
        self.assertNotIn("| template/base | N/A", body)

    def test_advisory_allowlist_rejects_injection(self):
        for url in ("https://github.com@evil.invalid/advisories/X", "https://nvd.nist.gov:443/vuln/detail/CVE-2026-1001", "https://github.com/advisories/x?q=a", "https://github.com/advisories/x\n", "https://github.com/../advisories/x", "https://pkg.go.dev/evil"):
            self.assertIsNone(R.advisory_url(url))
        self.assertEqual(R.advisory_url("https://pkg.go.dev/vuln/GO-2026-123"), "https://pkg.go.dev/vuln/GO-2026-123")
        self.assertRaises(ValueError, R.render, self.root, SHA, "https://evil.invalid/run", "success", "success", "success")

    def vex_fixture(self, target="template/base"):
        """pip 26.3 vendored-SBOM setuptools component, as in real template reports."""
        folder, data = self.fixture(target)
        data["Results"].append({"Target": "Python", "Class": "lang-pkgs", "Type": "python-pkg", "Packages": [
            {"Name": "pip", "Version": "26.3", "Identifier": {"UID": "b6dc6f46963a9078"}, "AnalyzedBy": "python-pkg",
             "FilePath": "usr/local/lib/python3.11/site-packages/pip-26.3.dist-info/METADATA"},
            {"Name": "setuptools", "Version": "70.3.0", "Identifier": {"UID": "171ad461d2f40666"}, "AnalyzedBy": "sbom"}],
            "Vulnerabilities": [{"VulnerabilityID": "CVE-2025-47273", "PkgName": "setuptools", "InstalledVersion": "70.3.0",
                                 "PkgIdentifier": {"UID": "171ad461d2f40666"}, "FixedVersion": "78.1.1", "Severity": "HIGH",
                                 "PrimaryURL": "https://avd.aquasec.com/nvd/cve-2025-47273"}]})
        (folder / "scan.json").write_text(json.dumps(data))
        return folder, data

    def test_policy_sentence_describes_owner_policy(self):
        body = self.render()
        self.assertNotIn("including unfixed findings", body)
        for text in ("block on any HIGH/CRITICAL with a fixed version", "tracked", "until 2026-12-12 (UTC)",
                     "raw upstream/builder/donor bases are report-only", "missing, stale or incomplete base scan does",
                     "Exactly one owner-approved VEX: CVE-2025-47273", "expires 2026-12-31",
                     "No other ignore", "Missing/invalid reports, stale DB and scanner failures block"):
            self.assertIn(text, body)
        late = R.render(self.root, SHA, URL, "failure", "failure", "failure", now=datetime(2027, 1, 1, tzinfo=timezone.utc))
        self.assertIn("re-review deadline has passed", late)
        self.assertIn("expired on 2026-12-31 and is no longer applied", late)

    def test_unfixed_tracked_before_deadline_and_blocking_after(self):
        unfixed = dict(self.vuln("CVE-2026-2001", "CRITICAL"), FixedVersion="")
        self.fixture(vulns=[unfixed, self.vuln()])
        body = self.render()
        self.assertIn("| filesystem | 1 | 1 | 1 / 1 | 0 | 🔴 Blocking findings |", body)
        self.assertIn("⛔ Blocking: 1 · ⏳ Tracked (no fix): 1", body)
        self.assertIn("| filesystem | CRITICAL | ⏳ tracked, no fix (until 2026-12-12) |", body)
        self.assertLess(body.index("[CVE-2026-1001]"), body.index("[CVE-2026-2001]"))  # blocking first
        self.fixture(vulns=[unfixed])
        self.assertIn("| filesystem | 1 | 0 | 0 / 1 | 0 | 🟡 Tracked (no fix) only |", self.render())
        later = datetime(2026, 12, 12, tzinfo=timezone.utc)
        self.fixture(vulns=[unfixed])  # Same evidence; DB refreshed on the review date.
        stamp = later.isoformat()
        (self.root / f"security-filesystem-{SHA}" / "db-metadata.json").write_text(json.dumps({"Version": 2, "UpdatedAt": stamp, "DownloadedAt": stamp}))
        body = R.render(self.root, SHA, URL, "failure", "failure", "failure", now=later)
        self.assertIn("| filesystem | 1 | 0 | 0 / 1 | 0 | 🔴 Blocking findings |", body)
        self.assertIn("| filesystem | CRITICAL | ⛔ blocks |", body)

    def test_owner_vex_is_listed_and_counted_never_hidden(self):
        self.vex_fixture()
        body = self.render()
        self.assertIn("| template/base | 0 | 1 | 1 / 0 | 1 | ✅ Owner-approved VEX only |", body)
        self.assertIn("✅ Owner-approved VEX (expires 2026-12-31): 1", body)
        section = body.index("### ✅ Owner-approved VEX matches (expires 2026-12-31; listed, not blocking)")
        row = body.index("| template/base | HIGH | <code>setuptools</code> | [CVE-2025-47273](https://avd.aquasec.com/nvd/cve-2025-47273) | <code>70.3.0</code> | <code>78.1.1</code> |")
        self.assertLess(section, row)
        self.assertLess(row, body.index("### Package findings"))
        self.assertIn("1 owner-approved VEX matches listed separately above", body)

    def test_vex_section_survives_large_finding_tables(self):
        self.fixture(vulns=[self.vuln(f"CVE-2026-{1000 + i}") for i in range(200)])
        for name in R.TEMPLATES:
            self.vex_fixture("template/" + name)
        body = self.render()
        self.assertIn("Showing 150/200", body)
        self.assertEqual(body.count("| <code>setuptools</code> | [CVE-2025-47273]"), 11)
        self.assertIn("✅ Owner-approved VEX (expires 2026-12-31): 11", body)
        self.assertLessEqual(len(body), 55000)

    def test_vex_lookalike_without_sbom_evidence_blocks(self):
        folder, data = self.vex_fixture()
        data["Results"][1]["Packages"][1]["FilePath"] = "usr/lib/python3/dist-packages/setuptools-70.3.0.dist-info/METADATA"
        (folder / "scan.json").write_text(json.dumps(data))
        body = self.render()
        self.assertIn("| template/base | 0 | 1 | 1 / 0 | 0 | 🔴 Blocking findings |", body)
        self.assertIn("No finding matched the owner-approved VEX.", body)
        self.assertIn("| template/base | HIGH | ⛔ blocks (fix available) | <code>setuptools</code> |", body)

    def test_raw_base_findings_are_report_only(self):
        unfixed = dict(self.vuln("CVE-2026-3001", "CRITICAL"), FixedVersion="")
        folder, data = self.fixture("base/go-builder", vulns=[self.vuln(), unfixed])
        data["Metadata"]["OS"]["Eosl"] = True
        (folder / "scan.json").write_text(json.dumps(data))
        body = self.render()
        self.assertIn("| base/go-builder | 1 | 1 | 1 / 1 | 0 | 📄 Report-only findings; OS EOL (report-only) |", body)
        self.assertIn("⛔ Blocking: 0 · ⏳ Tracked (no fix): 0 · 📄 Raw-base report-only: 2", body)
        self.assertIn("| base/go-builder | HIGH | 📄 report-only (raw base) |", body)

    def test_row_and_comment_limits(self):
        self.fixture(vulns=[self.vuln(f"CVE-2026-{1000 + i}") for i in range(200)])
        body = self.render()
        self.assertIn("Showing 150/200", body)
        self.assertIn("<details>", body)
        self.assertLessEqual(len(body), 55000)


if __name__ == "__main__":
    unittest.main()
