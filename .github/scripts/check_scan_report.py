#!/usr/bin/env python3
"""Fail-closed Trivy JSON/DB gate; stdlib only, exactly one owner-approved VEX.

Owner-approved PR209 policy for final shipped targets (lockfiles, product images,
controlled-base prototype, template outputs), classifying HIGH/CRITICAL only:
  blocking  non-empty FixedVersion; or no fix once UNFIXED_REVIEW_BY is reached.
  tracked   no FixedVersion: printed and counted, not blocking until
            UNFIXED_REVIEW_BY (UTC date); on/after it they block again.
  vex       the single OWNER_VEX below, only while unexpired and every evidence
            condition matches; always printed, never silently dropped.
No other ignore, allowlist, suppression or severity change exists.

Exit 0: valid, fresh evidence and nothing blocking (tracked/vex may be present).
Exit 1: blocking findings or end-of-life image OS.
Exit 2: missing, malformed, stale, or unidentifiable scan evidence.
Raw upstream bases call this CLI too but treat exit 1 as report-only.
"""

import argparse
from collections import namedtuple
from datetime import date, datetime, timedelta, timezone
import json
from pathlib import Path
import re
import sys


# Owner decision 2026-11-12 (pr209-merge-policy): unfixed HIGH/CRITICAL are
# time-bounded tracking only. From this UTC date on they block to force re-review.
UNFIXED_REVIEW_BY = date(2026, 12, 12)

OwnerVex = namedtuple("OwnerVex", "vulnerability package installed carrier carrier_version expires summary")
# The ONLY VEX, approved by the owner for PR209 (2026-11-12). Evidence
# (pip263-vex-evidence.json): official wheel pip-26.3-py3-none-any.whl
# SHA256 f3b60b20ffaadd4415ed7957212a38ac63fa4c5fa375bd0f4a797777940fad13 ships
# pip/_vendor/bom.cdx.json SHA256
# 5c78ac0296d2e95805bdad6b8dfbdaa292eab2506bf0e519f4a4c1a6fc4310e8, a CycloneDX
# SBOM listing component pkg:pypi/setuptools@70.3.0. pip vendors only
# pkg_resources (LICENSE, __init__.py); the vulnerable setuptools/package_index.py
# (CVE-2025-47273 PackageIndex path traversal) is absent. Trivy reports that SBOM
# component as an installed package. It is matched only when (see _vex_applies):
#   - VulnerabilityID/PkgName/InstalledVersion are exactly the values below;
#   - the same Result's Packages entry with the vulnerability's
#     PkgIdentifier.UID is unique, AnalyzedBy "sbom", without FilePath, and the
#     vulnerability has no PkgPath;
#   - every FilePath-bearing "pip" in the report is 26.3, and one exists;
#   - no FilePath-bearing (physically installed) setuptools 70.3.0 exists.
# Layer identity is deliberately not used: Trivy may attribute SBOM components
# to other layers. On/after `expires` (UTC date) it no longer applies.
OWNER_VEX = OwnerVex(
    vulnerability="CVE-2025-47273", package="setuptools", installed="70.3.0",
    carrier="pip", carrier_version="26.3", expires=date(2026, 12, 31),
    summary="setuptools 70.3.0 listed only in pip 26.3 vendored SBOM "
            "(pip/_vendor/bom.cdx.json); vulnerable setuptools/package_index.py not shipped")

GateResult = namedtuple("GateResult", "blocking tracked vex eol")


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


def _now(now):
    now = now or datetime.now(timezone.utc)
    if now.tzinfo is None:
        raise ValueError("now must be timezone-aware")
    return now


def utc_today(now=None):
    return _now(now).astimezone(timezone.utc).date()


def validate_db(metadata, now=None):
    """Both publication and download must be within 24h; tolerate 5m skew."""
    metadata = _object(metadata, "database metadata")
    if type(metadata.get("Version")) is not int or metadata["Version"] != 2:
        raise InvalidReport("unsupported or missing database Version (expected 2)")
    now = _now(now)
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


def _normalized(name):
    # PEP 503 normalization widens the report-wide exclusions (fail closed).
    return re.sub(r"[-_.]+", "-", name).lower() if isinstance(name, str) else None


def _vex_report_preconditions(results):
    """Report-wide VEX conditions; any unexpected package shape disables the VEX.

    Only an SBOM component (AnalyzedBy "sbom" and no FilePath key) is treated as
    not physically installed. Everything else, including FilePath-less OS
    packages, counts as installed: a stricter reading never widens the VEX.
    """
    installed_pips = 0
    for result in results:
        if not isinstance(result, dict):
            return False
        packages = result.get("Packages")
        if packages is None:
            continue
        if not isinstance(packages, list):
            return False
        for package in packages:
            if not isinstance(package, dict):
                return False
            if "FilePath" not in package and package.get("AnalyzedBy") == "sbom":
                continue
            name = _normalized(package.get("Name"))
            if name == OWNER_VEX.carrier:
                if package.get("Version") != OWNER_VEX.carrier_version:
                    return False
                path = package.get("FilePath")
                if isinstance(path, str) and path.strip():
                    installed_pips += 1
            elif name == OWNER_VEX.package and package.get("Version") == OWNER_VEX.installed:
                return False
    return installed_pips > 0


def _vex_applies(vulnerability, result, report_ok, today):
    if not report_ok or today >= OWNER_VEX.expires:
        return False
    if (vulnerability.get("VulnerabilityID"), vulnerability.get("PkgName"),
            vulnerability.get("InstalledVersion")) != (
            OWNER_VEX.vulnerability, OWNER_VEX.package, OWNER_VEX.installed):
        return False
    if "PkgPath" in vulnerability:
        return False
    identifier = vulnerability.get("PkgIdentifier")
    uid = identifier.get("UID") if isinstance(identifier, dict) else None
    packages = result.get("Packages")
    if not isinstance(uid, str) or not uid or not isinstance(packages, list):
        return False
    matches = [p for p in packages if isinstance(p, dict) and isinstance(p.get("Identifier"), dict)
               and p["Identifier"].get("UID") == uid]
    if len(matches) != 1:
        return False
    package = matches[0]
    return (package.get("AnalyzedBy") == "sbom" and "FilePath" not in package
            and package.get("Name") == OWNER_VEX.package
            and package.get("Version") == OWNER_VEX.installed)


def validate_report(report, expected_image_id=None, now=None):
    """Validate evidence before deciding severity; return a classified GateResult."""
    today = utc_today(now)
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
    elif artifact_type not in {"filesystem", "repository"}:
        # Trivy identifies a git checkout scanned with `fs` as a repository.
        raise InvalidReport("expected a filesystem or repository report")
    results = report.get("Results")
    if not isinstance(results, list):
        raise InvalidReport("Results must be an array (empty is valid)")
    if expected_image_id is not None:
        # OS identity alone is not evidence that the package scanner ran.
        os_results = [r for r in results if isinstance(r, dict) and r.get("Class") == "os-pkgs"]
        if not os_results or not any(r.get("Type") == metadata["OS"]["Family"] for r in os_results):
            raise InvalidReport("missing OS-package result for the identified image OS")
    report_ok = _vex_report_preconditions(results)
    review_due = today >= UNFIXED_REVIEW_BY
    classified = {"blocking": [], "tracked": [], "vex": []}
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
            if severity not in {"HIGH", "CRITICAL"}:
                continue
            if _vex_applies(vulnerability, result, report_ok, today):
                kind = "vex"
                reason = f"owner-approved VEX (expires {OWNER_VEX.expires.isoformat()}): {OWNER_VEX.summary}"
            elif fixed:
                kind, reason = "blocking", "fixed version available"
            elif review_due:
                kind = "blocking"
                reason = f"no fixed version; re-review deadline {UNFIXED_REVIEW_BY.isoformat()} reached"
            else:
                kind = "tracked"
                reason = f"no fixed version; tracked, non-blocking until {UNFIXED_REVIEW_BY.isoformat()}"
            url = vulnerability.get("PrimaryURL")
            classified[kind].append({
                "class": kind, "reason": reason, "target": target, "severity": severity,
                "id": identifier, "package": package, "installed": installed,
                "fixed": fixed, "url": url if isinstance(url, str) else ""})
    return GateResult(classified["blocking"], classified["tracked"], classified["vex"], eol)


def main(argv=None, now=None):
    parser = argparse.ArgumentParser(description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("report", nargs="?", help="Trivy JSON report; omit for DB-only check")
    parser.add_argument("--db-metadata", required=True)
    parser.add_argument("--expected-image-id")
    args = parser.parse_args(argv)
    try:
        now = _now(now)
        validate_db(load_json(args.db_metadata), now)
        if args.report is None:
            if args.expected_image_id is not None:
                raise InvalidReport("--expected-image-id requires a report")
            print("Database publication and download timestamps are fresh.")
            return 0
        gate = validate_report(load_json(args.report), args.expected_image_id, now)
        for finding in (*gate.blocking, *gate.vex, *gate.tracked):
            # JSON escaping keeps arbitrary package/target strings from becoming
            # workflow commands or injecting additional log lines.
            line = {k: v for k, v in finding.items() if k != "url"}
            print(json.dumps(dict(line, fixed=line["fixed"] or "NO FIX AVAILABLE"), sort_keys=True))
        print(f"Blocking HIGH/CRITICAL: {len(gate.blocking)}; "
              f"tracked without fix (non-blocking until {UNFIXED_REVIEW_BY.isoformat()}): {len(gate.tracked)}; "
              f"owner-approved VEX {OWNER_VEX.vulnerability} (expires {OWNER_VEX.expires.isoformat()}): {len(gate.vex)}; "
              f"image OS EOL: {gate.eol}")
        return 1 if gate.blocking or gate.eol else 0
    except Exception as exc:  # noqa: BLE001 - any crash is invalid evidence (exit 2), never "findings" (1)
        print("Invalid scan evidence: " + json.dumps(f"{type(exc).__name__}: {exc}"[:2000]), file=sys.stderr)
        return 2


if __name__ == "__main__":
    try:
        sys.exit(main())
    except SystemExit:
        raise
    except BaseException:  # noqa: BLE001 - interpreter-level failure is never a findings result
        sys.exit(2)
