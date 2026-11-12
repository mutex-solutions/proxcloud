#!/usr/bin/env python3
"""Build once, scan each emitted local template immediately; 0 clean, 1 findings, 2 incomplete.

Only run in the disposable hosted CI checkout. GPU, builder stages and the external
Python image are deliberately not built/scanned here. No publishing or login.
"""

import json
import os
from pathlib import Path
import re
import selectors
import signal
import subprocess
import sys
import time


NAMES = ("base", "mcp-gateway", "code-interpreter", "node", "browser", "desktop",
         "codex", "opencode", "claude", "amp", "gemini")
REGISTRY = "security-scan.local"
BUDGET = 6600


def expected_refs(sha):
    if not re.fullmatch(r"[0-9a-f]{40}", sha):
        raise ValueError("SOURCE_SHA must be a lowercase 40-character commit SHA")
    return {name: f"{REGISTRY}/sandbox-{name}:{sha}" for name in NAMES}


def parse_ref(line, expected, seen):
    match = re.fullmatch(r"([a-z-]+) ([a-z0-9./:-]+)", line)
    if not match or expected.get(match[1]) != match[2]:
        raise ValueError("unknown name, malformed output, or unexpected image ref")
    name, ref = match.groups()
    if name in seen:
        raise ValueError("duplicate template output")
    seen.add(name)
    return name, ref


def run_command(argv, deadline, timeout, **kwargs):
    remaining = deadline - time.monotonic() - 5
    if remaining <= 0:
        raise TimeoutError("template scan wall-clock budget exhausted")
    return subprocess.run(argv, timeout=min(timeout, remaining), check=False, **kwargs)


def scan_image(root, name, ref, deadline):
    target = root / "result" / "templates" / name
    target.mkdir(parents=True, exist_ok=True)
    (target / "ref.txt").write_text(ref + "\n", encoding="utf-8")
    status = 2
    try:
        inspected = run_command(["docker", "image", "inspect", ref, "--format", "{{.Id}}"],
                                deadline, 30, capture_output=True, text=True)
        image_id = inspected.stdout.strip()
        if inspected.returncode or not re.fullmatch(r"sha256:[0-9a-f]{64}", image_id):
            raise ValueError("local image inspect failed or returned an invalid ID")
        (target / "image-id.txt").write_text(image_id + "\n", encoding="utf-8")
        with (target / "trivy.log").open("w", encoding="utf-8") as log:
            scanned = run_command([
                str((root / "tools/trivy").resolve()), "image", "--config", "",
                "--ignorefile", "", "--disable-telemetry", "--skip-version-check",
                "--ignore-unfixed=false", "--exit-on-eol", "1", "--image-src", "docker",
                "--platform", "linux/amd64", "--scanners", "vuln", "--skip-db-update",
                "--list-all-pkgs", "--severity", "HIGH,CRITICAL", "--exit-code", "0",
                "--format", "json", "--output", str(target / "scan.json"), image_id,
            ], deadline, 600, stdout=log, stderr=subprocess.STDOUT)
        # Always run the existing evidence gate, even after a scanner engine error.
        with (target / "gate.txt").open("w", encoding="utf-8") as log:
            gated = run_command([
                sys.executable, str(root / ".github/scripts/check_scan_report.py"),
                "--db-metadata", str(root / "result/db-metadata.json"),
                "--expected-image-id", image_id, str(target / "scan.json"),
            ], deadline, 30, stdout=log, stderr=subprocess.STDOUT)
        # Trivy uses exit 1 for EOL as well as engine failures; the JSON gate
        # independently identifies EOL. Never downgrade an unexplained scan error.
        status = gated.returncode if gated.returncode in (0, 1) else 2
        if scanned.returncode and not (scanned.returncode == 1 and status == 1):
            status = 2
    except (OSError, ValueError, TimeoutError, subprocess.TimeoutExpired) as exc:
        with (target / "gate.txt").open("a", encoding="utf-8") as log:
            log.write("Incomplete scan: " + json.dumps(str(exc)) + "\n")
    (target / "status.txt").write_text(str(status) + "\n", encoding="utf-8")
    print(f"Template {name}: status {status}", flush=True)
    return status


def main():
    root = Path.cwd().resolve()
    try:
        expected = expected_refs(os.environ.get("SOURCE_SHA", ""))
    except ValueError as exc:
        print(str(exc), file=sys.stderr)
        return 2
    result = root / "result"
    result.mkdir(exist_ok=True)
    deadline = time.monotonic() + BUDGET
    env = dict(os.environ, PUSH="0", REGISTRY=REGISTRY, TAG=os.environ["SOURCE_SHA"])
    seen, statuses, errors = set(), {}, []
    build, build_rc = None, None
    try:
        with (result / "build.log").open("wb") as log, (result / "refs.txt").open("w", encoding="utf-8") as refs:
            build = subprocess.Popen([str(root / "deploy/templates/build.sh"), *NAMES],
                                     cwd=root, env=env, stdout=subprocess.PIPE,
                                     stderr=log, start_new_session=True, bufsize=0)
            # Read available bytes, not blocking readline(): even a silent/hung
            # builder or a partial line must respect the total wall-clock budget.
            with selectors.DefaultSelector() as selector:
                selector.register(build.stdout, selectors.EVENT_READ)
                pending = b""
                while selector.get_map():
                    remaining = deadline - time.monotonic() - 5
                    if remaining <= 0:
                        raise TimeoutError("template build/scan budget exhausted")
                    if not selector.select(timeout=min(1, remaining)):
                        continue
                    chunk = os.read(build.stdout.fileno(), 4096)
                    if not chunk:
                        selector.unregister(build.stdout)
                        if pending:
                            errors.append("unterminated builder output")
                        break
                    pending += chunk
                    while b"\n" in pending:
                        line, pending = pending.split(b"\n", 1)
                        try:
                            name, ref = parse_ref(line.decode("ascii"), expected, seen)
                        except (ValueError, UnicodeError) as exc:
                            errors.append(str(exc))
                            continue
                        refs.write(f"{name} {ref}\n")
                        refs.flush()
                        # Synchronous scan here starts before consuming the next
                        # ref, while the independent builder keeps building.
                        statuses[name] = scan_image(root, name, ref, deadline)
                    if len(pending) > 8192:
                        raise ValueError("oversized builder output line")
            build_rc = build.wait(timeout=max(1, deadline - time.monotonic() - 5))
            if build_rc:
                errors.append("template builder returned nonzero")
    except (OSError, ValueError, TimeoutError, subprocess.TimeoutExpired) as exc:
        errors.append(str(exc))
    finally:
        if build is not None:
            if build.poll() is None:
                # Terminate the complete build process group, including buildx.
                try:
                    os.killpg(build.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                build.wait(timeout=5)
            if build.stdout is not None:
                build.stdout.close()
    missing = sorted(set(expected) - statuses.keys())
    final = 2 if errors or missing or 2 in statuses.values() else (1 if 1 in statuses.values() else 0)
    summary = {"status": final, "build_returncode": build_rc, "targets": statuses,
               "missing": missing, "errors": errors}
    (result / "template-status.json").write_text(json.dumps(summary, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(summary, sort_keys=True), flush=True)
    return final


if __name__ == "__main__":
    sys.exit(main())
