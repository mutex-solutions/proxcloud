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

from check_scan_report import OWNER_VEX, UNFIXED_REVIEW_BY, utc_today, validate_db, validate_report

PRODUCTS = ("filesystem", "api", "web-client", "web-admin", "sandbox-proxy", "sandbox-runtime", "sandbox-import", "controlled-base-prototype")
TEMPLATES = ("base", "mcp-gateway", "code-interpreter", "node", "browser", "desktop", "codex", "opencode", "claude", "amp", "gemini")
BASES = ("go-builder", "web-builder", "web-runtime", "alpine-runtime-donor", "proxy-distroless", "import-distroless", "template-e2b-base", "template-cpu-python", "template-node", "template-desktop-ubuntu", "template-playwright-python", "mcp-node-donor", "external-python")
STATES = ("success", "failure", "cancelled", "skipped")
LIMIT = 55000
# Display order: what blocks the required gate first. Raw bases are report-only.
CLASS_ORDER = {"blocking": 0, "tracked": 1, "report-only": 2, "vex": 3}


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
    identifier = finding["id"]
    url = advisory_url(finding.get("url"))
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
            if self.files > 48 or self.total > 512 * 1024 * 1024:
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


def render(reports_dir, source_sha, run_url, scan_result, template_result, gate_result, now=None, base_result="skipped"):
    if not re.fullmatch(r"[0-9a-f]{40}", source_sha):
        raise ValueError("invalid source SHA")
    if not re.fullmatch(r"https://github\.com/[A-Za-z0-9_-]+/[A-Za-z0-9_.-]+/actions/runs/[0-9]+", run_url) or any(p in (".", "..") for p in urlsplit(run_url).path.split("/")):
        raise ValueError("invalid GitHub run URL")
    if any(s not in STATES for s in (scan_result, template_result, gate_result, base_result)):
        raise ValueError("invalid job result")
    now = now or datetime.now(timezone.utc)
    today = utc_today(now)
    review, vex_expiry = UNFIXED_REVIEW_BY.isoformat(), OWNER_VEX.expires.isoformat()
    labels = {"blocking": "⛔ blocks (fix available)" if today < UNFIXED_REVIEW_BY else "⛔ blocks",
              "tracked": f"⏳ tracked, no fix (until {review})",
              "report-only": "📄 report-only (raw base)"}
    evidence, rows, findings = Evidence(reports_dir), [], {}
    targets = ([(n, n, "") for n in PRODUCTS]
               + [("template/" + n, "template-images", "templates/" + n + "/") for n in TEMPLATES]
               + [("base/" + n, "base-images", "bases/" + n + "/") for n in BASES])
    for target, artifact, nested in targets:
        base = f"security-{artifact}-{source_sha}/"
        try:
            if evidence.read(base + "source-sha.txt") not in (source_sha, source_sha + "\n"):
                raise ValueError("source mismatch")
            validate_db(evidence.document(base + "db-metadata.json"), now)
            image_id = None if target == "filesystem" else evidence.read(base + nested + "image-id.txt").strip()
            if image_id is not None and not re.fullmatch(r"sha256:[0-9a-f]{64}", image_id):
                raise ValueError("invalid image ID")
            report = evidence.document(base + nested + "scan.json")
            gate = validate_report(report, image_id, now)
            raw_base = target.startswith("base/")
            local = {}
            for finding in (*gate.blocking, *gate.tracked, *gate.vex):
                # Raw bases are report-only: their findings never block the gate.
                kind = "report-only" if raw_base and finding["class"] != "vex" else finding["class"]
                key = (target, finding["id"], finding["package"], finding["installed"])
                # Collapse repeated binary occurrences within an output, conservatively:
                # the most gate-relevant class wins, then Critical, then no-fix.
                rank = (CLASS_ORDER[kind], finding["severity"] != "CRITICAL", bool(finding["fixed"]), finding["fixed"], finding["url"][:512])
                if key not in local or rank < local[key][0]:
                    local[key] = (rank, dict(finding, kind=kind))
            findings.update({k: v[1] for k, v in local.items()})
            kinds = [v[1]["kind"] for v in local.values()]
            critical = sum(v[1]["severity"] == "CRITICAL" for v in local.values())
            high, fixed = len(local) - critical, sum(bool(v[1]["fixed"]) for v in local.values())
            if "blocking" in kinds:
                state = "🔴 Blocking findings"
            elif "tracked" in kinds:
                state = "🟡 Tracked (no fix) only"
            elif "report-only" in kinds:
                state = "📄 Report-only findings"
            elif kinds:
                state = "✅ Owner-approved VEX only"
            else:
                state = "⚪ Scanned; gate separate"
            if gate.eol:
                state += "; OS EOL (report-only)" if raw_base else "; OS EOL"
            rows.append(f"| {target} | {critical} | {high} | {fixed} / {len(local) - fixed} | {kinds.count('vex')} | {state} |")
        except FileNotFoundError:
            rows.append(f"| {target} | N/A | N/A | N/A | N/A | ⚠️ Missing / incomplete |")
        except (OSError, ValueError, TypeError, OverflowError, RecursionError):
            rows.append(f"| {target} | N/A | N/A | N/A | N/A | ⚠️ INVALID evidence |")
    ordered = sorted(findings.items(), key=lambda kv: (CLASS_ORDER[kv[1]["kind"]], kv[1]["severity"] != "CRITICAL", *kv[0]))
    critical = sum(v["severity"] == "CRITICAL" for v in findings.values())
    counts = {kind: sum(v["kind"] == kind for v in findings.values()) for kind in CLASS_ORDER}
    # VEX matches get their own section so the bounded table can never truncate
    # them away; dedup keys allow at most one match per target (<= 32 rows).
    vex_rows = [f"| {key[0]} | {vuln['severity']} | {code(vuln['package'])} | {advisory(vuln)} | {code(vuln['installed'])} | {code(vuln['fixed'] or 'NO FIX AVAILABLE')} |"
                for key, vuln in ordered if vuln["kind"] == "vex"]
    ordered = [kv for kv in ordered if kv[1]["kind"] != "vex"]
    valid = sum("N/A" not in row for row in rows)
    repo_url = run_url.split("/actions/runs/")[0]
    status = "✅ Required gate accepted" if gate_result == "success" else "⛔ BLOCKED / INCOMPLETE"
    unfixed = (f"HIGH/CRITICAL without any fixed version are **tracked** (shown and counted here, not blocking) until {review} (UTC); from that date they block again until re-reviewed."
               if today < UNFIXED_REVIEW_BY else
               f"The {review} (UTC) re-review deadline has passed: HIGH/CRITICAL without a fixed version **block again** until re-reviewed.")
    vex = (f"Exactly one owner-approved VEX: {OWNER_VEX.vulnerability} for {OWNER_VEX.package} {OWNER_VEX.installed} present only as a component of pip {OWNER_VEX.carrier_version}'s vendored SBOM (expires {vex_expiry}); every match is listed below, never hidden."
           if today < OWNER_VEX.expires else
           f"The owner-approved VEX for {OWNER_VEX.vulnerability} expired on {vex_expiry} and is no longer applied.")
    lines = ["<!-- proxcloud-security-scan-v1 -->", "## 🛡️ Security scan · " + status, "",
             f"**🔴 Critical: {critical} · 🟠 High: {len(findings) - critical} · Coverage: {valid}/32 targets**",
             f"⛔ Blocking: {counts['blocking']} · ⏳ Tracked (no fix): {counts['tracked']} · 📄 Raw-base report-only: {counts['report-only']} · ✅ Owner-approved VEX (expires {vex_expiry}): {counts['vex']}", "",
             f"Source: [`{source_sha[:12]}`]({repo_url}/commit/{source_sha}) · [Workflow run & full JSON reports]({run_url})",
             f"Jobs: scan **{scan_result}** · templates **{template_result}** · upstream bases **{base_result}** · required gate **{gate_result}**", "",
             "**Policy (owner-approved):** final shipped targets (lockfiles, product images, controlled prototype, template outputs) block on any HIGH/CRITICAL with a fixed version and on an EOL OS. "
             + unfixed + " The thirteen raw upstream/builder/donor bases are report-only: their findings and EOL never block, but a missing, stale or incomplete base scan does. "
             + vex + " No other ignore, allowlist or severity change. Missing/invalid reports, stale DB and scanner failures block the required gate.",
             "Counts are deduplicated package findings (advisory + package + installed version per target), not unique CVEs or repeated binary occurrences. The same dependency inherited by multiple targets is counted in each target. Critical/High and fix counts include tracked, report-only and VEX findings. Totals cover only valid evidence below; N/A never means zero.", "",
             "| Source / target | 🔴 Critical | 🟠 High | Fix available / no fix | ✅ Owner VEX | Evidence status |", "|---|---:|---:|---:|---:|---|", *rows, "",
             "Evidence parsing is not gate validation: no per-report scanner exit status is inferred, and no scope/engine/DB-policy completeness is asserted by this comment. The required security-gate result above is authoritative.", "",
             f"### ✅ Owner-approved VEX matches (expires {vex_expiry}; listed, not blocking)",
             *(["| Target | Severity | Package | CVE / advisory | Installed | Fixed |", "|---|---|---|---|---|---|", *vex_rows]
               if vex_rows else ["No finding matched the owner-approved VEX."]), "",
             "### Package findings (blocking first, then Critical; VEX listed above)"]
    heading = ["| Target | Severity | Gate class | Package | CVE / advisory | Installed | Fixed |", "|---|---|---|---|---|---|---|"]
    lines += heading
    shown = 0
    for key, vuln in ordered[:150]:
        row = f"| {key[0]} | {vuln['severity']} | {labels[vuln['kind']]} | {code(vuln['package'])} | {advisory(vuln)} | {code(vuln['installed'])} | {code(vuln['fixed'] or 'NO FIX AVAILABLE')} |"
        extra = ["", "<details><summary>More vulnerability details (bounded)</summary>", "", *heading] if shown == 80 else []
        if len("\n".join(lines + extra + [row]).encode("utf-8")) > LIMIT - 1800:
            break
        lines += extra + [row]
        shown += 1
    if shown > 80:
        lines += ["", "</details>"]
    lines += ["", f"Showing {shown}/{len(ordered)} package findings; {len(ordered) - shown} omitted; {len(vex_rows)} owner-approved VEX matches listed separately above. Full JSON reports remain available from the workflow run.", "",
              "Coverage: source lockfiles + six product final images + one NONDEFAULT controlled-base prototype + eleven non-GPU template outputs + thirteen upstream/builder/donor/external-Python registry targets. The controlled prototype is evaluation only and does not change the selected SDK base. Raw base findings are not automatically shipped-final vulnerabilities; keep their role/target separate. GPU/CUDA image remains deferred, and this is not an all-clear for the whole supply chain."]
    return "\n".join(lines) + "\n"


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("reports-dir", "source-sha", "run-url", "output"):
        parser.add_argument("--" + name, required=True)
    for name in ("scan-result", "template-result", "gate-result", "base-result"):
        parser.add_argument("--" + name, required=True, choices=STATES)
    args = vars(parser.parse_args(argv))
    output = args.pop("output")
    Path(output).write_text(render(**args), encoding="utf-8")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
