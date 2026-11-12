#!/usr/bin/env python3
"""Render bounded, untrusted Trivy evidence as Markdown; never a merge gate.

Input: eight security-<target>-<sha> artifact directories, with source-sha.txt
and db-metadata.json at each root. Product scan.json/image-id.txt live there;
template reports live in templates/<name>/. Only fixed allowlisted paths are read.
Run in the read-only-token sandbox, never in the privileged comment writer.
"""
import argparse
from datetime import datetime, timezone
import html
import json
import os
from pathlib import Path
import re
import stat
from urllib.parse import urlsplit

from check_scan_report import validate_db, validate_report

PRODUCTS = ("filesystem", "api", "web-client", "web-admin", "sandbox-proxy", "sandbox-runtime", "sandbox-import")
TEMPLATES = ("base", "mcp-gateway", "code-interpreter", "node", "browser", "desktop", "codex", "opencode", "claude", "amp", "gemini")
STATES = ("success", "failure", "cancelled", "skipped")
LIMIT = 55000


def safe(value):
    """Bound display fields and neutralize HTML, Markdown and notifications."""
    value = "".join(c for c in value[:512] if c.isprintable())
    value = value[:159] + "…" if len(value) > 160 else value
    value = html.escape(value, quote=True).replace("@", "&#64;")
    for char in "\\|`[]()*_!~":
        value = value.replace(char, f"&#{ord(char)};")
    return value


def code(value):
    return "<code>" + safe(value) + "</code>"


def advisory_url(value):
    if not isinstance(value, str) or len(value) > 512 or not re.fullmatch(r"https://[A-Za-z0-9./_-]+", value):
        return None
    parsed = urlsplit(value)
    if parsed.username or parsed.password or parsed.port or parsed.query or parsed.fragment or any(p in (".", "..") for p in parsed.path.split("/")):
        return None
    allowed = {"nvd.nist.gov": r"/vuln/detail/CVE-\d{4}-\d{4,}", "github.com": r"/(?:advisories/GHSA-[\w-]+|[\w.-]+/[\w.-]+/security/advisories/GHSA-[\w-]+)",
               "avd.aquasec.com": r"/[A-Za-z0-9/_-]+/?", "pkg.go.dev": r"/vuln/GO-\d{4}-\d+"}
    return value if parsed.scheme == "https" and parsed.netloc == parsed.hostname and re.fullmatch(allowed.get(parsed.hostname, r"(?!)"), parsed.path) else None


def advisory(finding):
    identifier = finding["VulnerabilityID"]
    url = advisory_url(finding.get("PrimaryURL"))
    known = re.fullmatch(r"(?:CVE-\d{4}-\d{4,}|GHSA-[23456789cfghjmpqrvwx]{4}(?:-[23456789cfghjmpqrvwx]{4}){2}|GO-\d{4}-\d+)", identifier)
    return f"[{safe(identifier)}]({url})" if known and url else code(identifier)


class Evidence:
    def __init__(self, root):
        root = Path(root).absolute()
        if any(p.is_symlink() for p in (root, *root.parents)) or not root.is_dir():
            raise ValueError("reports root must be a real directory")
        self.root, self.cache, self.total, self.files = root, {}, 0, 0

    def read(self, relative, document=False):
        if relative in self.cache:
            return self.cache[relative]
        path = self.root / relative
        if not path.is_relative_to(self.root) or ".." in path.parts or any(p.is_symlink() for p in (path, *path.parents)):
            raise ValueError("unsafe evidence path")
        ceiling = 32 * 1024 * 1024 if document else 512
        fd = os.open(self.root, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        try:
            for part in path.relative_to(self.root).parts[:-1]:
                child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
                os.close(fd)
                fd = child
            leaf = os.open(path.name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=fd)
        finally:
            os.close(fd)
        with os.fdopen(leaf, "rb") as handle:
            info = os.fstat(handle.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_size > ceiling:
                raise ValueError("evidence size/type invalid")
            self.files += int(document)
            self.total += info.st_size
            if self.files > 30 or self.total > 256 * 1024 * 1024:
                raise ValueError("evidence budget exceeded")
            raw = handle.read(ceiling + 1)
        if len(raw) > ceiling:
            raise ValueError("evidence grew beyond limit")
        self.cache[relative] = raw.decode("utf-8")
        return self.cache[relative]

    def document(self, relative):
        def unique(pairs):
            result = {}
            for key, value in pairs:
                if key in result:
                    raise ValueError("duplicate JSON key")
                result[key] = value
            return result
        return json.loads(self.read(relative, True), object_pairs_hook=unique)


def render(reports_dir, source_sha, run_url, scan_result, template_result, gate_result, now=None):
    if not re.fullmatch(r"[0-9a-f]{40}", source_sha):
        raise ValueError("invalid source SHA")
    if not re.fullmatch(r"https://github\.com/[A-Za-z0-9_-]+/[A-Za-z0-9_.-]+/actions/runs/[0-9]+", run_url) or any(p in (".", "..") for p in urlsplit(run_url).path.split("/")):
        raise ValueError("invalid GitHub run URL")
    if any(s not in STATES for s in (scan_result, template_result, gate_result)):
        raise ValueError("invalid job result")
    evidence, rows, findings = Evidence(reports_dir), [], {}
    targets = [(n, n, "") for n in PRODUCTS] + [("template/" + n, "template-images", "templates/" + n + "/") for n in TEMPLATES]
    for target, artifact, nested in targets:
        base = f"security-{artifact}-{source_sha}/"
        try:
            if evidence.read(base + "source-sha.txt") not in (source_sha, source_sha + "\n"):
                raise ValueError("source mismatch")
            validate_db(evidence.document(base + "db-metadata.json"), now or datetime.now(timezone.utc))
            image_id = None if target == "filesystem" else evidence.read(base + nested + "image-id.txt").strip()
            if image_id is not None and not re.fullmatch(r"sha256:[0-9a-f]{64}", image_id):
                raise ValueError("invalid image ID")
            report = evidence.document(base + nested + "scan.json")
            _, eol = validate_report(report, image_id)
            local = {}
            for result in report["Results"]:
                for vuln in result.get("Vulnerabilities") or []:
                    if vuln["Severity"] not in ("HIGH", "CRITICAL"):
                        continue
                    key = (target, vuln["VulnerabilityID"], vuln["PkgName"], vuln["InstalledVersion"])
                    # Collapse repeated binary occurrences within an output, conservatively.
                    rank = (vuln["Severity"] != "CRITICAL", bool(vuln.get("FixedVersion")), vuln.get("FixedVersion", ""), str(vuln.get("PrimaryURL", ""))[:512])
                    if key not in local or rank < local[key][0]:
                        local[key] = (rank, vuln)
            findings.update({k: v[1] for k, v in local.items()})
            critical = sum(v[1]["Severity"] == "CRITICAL" for v in local.values())
            high, fixed = len(local) - critical, sum(bool(v[1].get("FixedVersion")) for v in local.values())
            state = "🔴 Findings" if local else "⚪ Scanned; gate separate"
            if eol:
                state += "; OS EOL"
            rows.append(f"| {target} | {critical} | {high} | {fixed} / {len(local) - fixed} | {state} |")
        except FileNotFoundError:
            rows.append(f"| {target} | N/A | N/A | N/A | ⚠️ Missing / incomplete |")
        except (OSError, ValueError, TypeError, OverflowError, RecursionError):
            rows.append(f"| {target} | N/A | N/A | N/A | ⚠️ INVALID evidence |")
    ordered = sorted(findings.items(), key=lambda kv: (kv[1]["Severity"] != "CRITICAL", *kv[0]))
    critical = sum(v["Severity"] == "CRITICAL" for v in findings.values())
    valid = sum("N/A" not in row for row in rows)
    repo_url = run_url.split("/actions/runs/")[0]
    status = "✅ Required gate accepted" if gate_result == "success" else "⛔ BLOCKED / INCOMPLETE"
    lines = ["<!-- proxcloud-security-scan-v1 -->", "## 🛡️ Security scan · " + status, "",
             f"**🔴 Critical: {critical} · 🟠 High: {len(findings) - critical} · Coverage: {valid}/18 outputs**", "",
             f"Source: [`{source_sha[:12]}`]({repo_url}/commit/{source_sha}) · [Workflow run & full JSON reports]({run_url})",
             f"Jobs: scan **{scan_result}** · templates **{template_result}** · required gate **{gate_result}**", "",
             "**Policy:** every HIGH/CRITICAL package finding blocks, including unfixed findings; no ignores. Missing/invalid reports, stale DB and scanner failures block the required gate.",
             "Counts are deduplicated package findings (advisory + package + installed version per output), not unique CVEs or repeated binary occurrences. Totals cover only valid evidence below; N/A never means zero.", "",
             "| Source / target | 🔴 Critical | 🟠 High | Fix available / no fix | Evidence status |", "|---|---:|---:|---:|---|", *rows, "",
             "Evidence parsing is not gate validation: no per-report scanner exit status is inferred, and no scope/engine/DB-policy completeness is asserted by this comment. The required security-gate result above is authoritative.", "",
             "### Package findings (Critical first)"]
    heading = ["| Target | Severity | Package | CVE / advisory | Installed | Fixed |", "|---|---|---|---|---|---|"]
    lines += heading
    shown = 0
    for key, vuln in ordered[:150]:
        row = f"| {key[0]} | {vuln['Severity']} | {code(vuln['PkgName'])} | {advisory(vuln)} | {code(vuln['InstalledVersion'])} | {code(vuln.get('FixedVersion') or 'NO FIX AVAILABLE')} |"
        extra = ["", "<details><summary>More vulnerability details (bounded)</summary>", "", *heading] if shown == 80 else []
        if len("\n".join(lines + extra + [row]).encode("utf-8")) > LIMIT - 1800:
            break
        lines += extra + [row]
        shown += 1
    if shown > 80:
        lines += ["", "</details>"]
    lines += ["", f"Showing {shown}/{len(ordered)} package findings; {len(ordered) - shown} omitted. Full JSON reports remain available from the workflow run.", "",
              "Coverage: source lockfiles + six product final images + eleven non-GPU template outputs. Excludes builder-stage bases, the external Python image and GPU images. This is not an all-clear for the whole supply chain."]
    return "\n".join(lines) + "\n"


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("reports-dir", "source-sha", "run-url", "output"):
        parser.add_argument("--" + name, required=True)
    for name in ("scan-result", "template-result", "gate-result"):
        parser.add_argument("--" + name, required=True, choices=STATES)
    args = vars(parser.parse_args(argv))
    output = args.pop("output")
    Path(output).write_text(render(**args), encoding="utf-8")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
