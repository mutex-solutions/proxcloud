#!/usr/bin/env python3
"""Fail-closed Trivy JSON/DB gate; stdlib only, no suppressions or allowlist.

Exit 0: valid, fresh evidence and no HIGH/CRITICAL findings.
Exit 1: HIGH/CRITICAL findings or end-of-life image OS.
Exit 2: missing, malformed, stale, or unidentifiable scan evidence.
"""

import argparse
from datetime import datetime, timedelta, timezone
import json
from pathlib import Path
import sys


class InvalidReport(ValueError):
    """Evidence is insufficient to claim a successful scan."""


def _object(value, label):
    if not isinstance(value, dict):
        raise InvalidReport(f"{label} must be an object")
    return value


def _text(value, label):
    if not isinstance(value, str) or not value.strip():
        raise InvalidReport(f"{label} must be a nonempty string")
    return value


def _unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise InvalidReport(f"duplicate JSON key: {key!r}")
        result[key] = value
    return result


def load_json(path):
    with Path(path).open(encoding="utf-8") as handle:
        return json.load(handle, object_pairs_hook=_unique_object)


def validate_db(metadata, now=None):
    """Both publication and download must be within 24h; tolerate 5m skew."""
    metadata = _object(metadata, "database metadata")
    if type(metadata.get("Version")) is not int or metadata["Version"] != 2:
        raise InvalidReport("unsupported or missing database Version (expected 2)")
    now = now or datetime.now(timezone.utc)
    for key in ("UpdatedAt", "DownloadedAt"):
        raw = _text(metadata.get(key), f"database {key}")
        try:
            stamp = datetime.fromisoformat(raw.replace("Z", "+00:00"))
        except ValueError as exc:
            raise InvalidReport(f"invalid database {key} timestamp") from exc
        if stamp.tzinfo is None:
            raise InvalidReport(f"database {key} must include a timezone")
        age = now - stamp
        if age > timedelta(hours=24) or age < -timedelta(minutes=5):
            raise InvalidReport(f"database {key} is stale or in the future")
    return metadata


def validate_report(report, expected_image_id=None):
    """Validate evidence before deciding severity; return findings and OS EOL."""
    report = _object(report, "report")
    if type(report.get("SchemaVersion")) is not int or report["SchemaVersion"] != 2:
        raise InvalidReport("unsupported or missing report SchemaVersion")
    _text(report.get("ArtifactName"), "ArtifactName")
    artifact_type = _text(report.get("ArtifactType"), "ArtifactType")
    metadata = _object(report.get("Metadata"), "Metadata")
    eol = False
    if expected_image_id is not None:
        _text(expected_image_id, "expected image ID")
        if artifact_type != "container_image":
            raise InvalidReport("expected a container_image report")
        if metadata.get("ImageID") != expected_image_id:
            raise InvalidReport("report ImageID does not match the locally built image")
        os_info = _object(metadata.get("OS"), "image OS")
        _text(os_info.get("Family"), "image OS Family")
        _text(os_info.get("Name"), "image OS Name/version")
        if os_info["Family"].lower() == "unknown" or os_info["Name"].lower() == "unknown":
            raise InvalidReport("image OS coverage is unknown")
        if "Eosl" in os_info and type(os_info["Eosl"]) is not bool:
            raise InvalidReport("image OS Eosl must be boolean")
        # Trivy omits false Eosl in JSON; absence is not an unknown OS when
        # Family and Name are present. The scanner also uses --exit-on-eol 1.
        eol = os_info.get("Eosl", False)
    elif artifact_type != "filesystem":
        raise InvalidReport("expected a filesystem report")
    results = report.get("Results")
    if not isinstance(results, list):
        raise InvalidReport("Results must be an array (empty is valid)")
    if expected_image_id is not None:
        # OS identity alone is not evidence that the package scanner ran.
        os_results = [r for r in results if isinstance(r, dict) and r.get("Class") == "os-pkgs"]
        if not os_results or not any(r.get("Type") == metadata["OS"]["Family"] for r in os_results):
            raise InvalidReport("missing OS-package result for the identified image OS")
    findings = []
    severities = {"UNKNOWN", "LOW", "MEDIUM", "HIGH", "CRITICAL"}
    for index, result in enumerate(results):
        result = _object(result, f"Results[{index}]")
        target = _text(result.get("Target"), "result Target")
        vulnerabilities = result.get("Vulnerabilities")
        if vulnerabilities is None:
            continue  # Trivy omits or nulls this field on clean targets.
        if not isinstance(vulnerabilities, list):
            raise InvalidReport("Vulnerabilities must be an array or null")
        for vulnerability in vulnerabilities:
            vulnerability = _object(vulnerability, "vulnerability")
            severity = vulnerability.get("Severity")
            if severity not in severities:
                raise InvalidReport("missing or invalid vulnerability Severity")
            identifier = _text(vulnerability.get("VulnerabilityID"), "VulnerabilityID")
            package = _text(vulnerability.get("PkgName"), "PkgName")
            installed = _text(vulnerability.get("InstalledVersion"), "InstalledVersion")
            fixed = vulnerability.get("FixedVersion", "")
            if not isinstance(fixed, str):
                raise InvalidReport("FixedVersion must be a string when present")
            if severity in {"HIGH", "CRITICAL"}:
                findings.append({"target": target, "severity": severity,
                                 "id": identifier, "package": package,
                                 "installed": installed, "fixed": fixed or "NO FIX AVAILABLE"})
    return findings, eol


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("report", nargs="?", help="Trivy JSON report; omit for DB-only check")
    parser.add_argument("--db-metadata", required=True)
    parser.add_argument("--expected-image-id")
    args = parser.parse_args(argv)
    try:
        validate_db(load_json(args.db_metadata))
        if args.report is None:
            if args.expected_image_id is not None:
                raise InvalidReport("--expected-image-id requires a report")
            print("Database publication and download timestamps are fresh.")
            return 0
        findings, eol = validate_report(load_json(args.report), args.expected_image_id)
        for finding in findings:
            # JSON escaping keeps arbitrary package/target strings from becoming
            # workflow commands or injecting additional log lines.
            print(json.dumps(finding, sort_keys=True))
        print(f"HIGH/CRITICAL findings: {len(findings)}; image OS EOL: {eol}")
        return 1 if findings or eol else 0
    except (OSError, ValueError, TypeError, OverflowError) as exc:
        print("Invalid scan evidence: " + json.dumps(str(exc)), file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
