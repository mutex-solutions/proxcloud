#!/usr/bin/env python3
"""Fixed PR209 upstream-base registry/blob scans, never image execution/build/push.

Hosted CI only. Exit 0: all 13 clean; 1: HIGH/CRITICAL or EOL; 2: incomplete.
Raw upstream findings are not proof of packages shipped by an upgraded final image.
GPU/CUDA and old published template pins are deliberately outside this allowlist.
"""

import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time


# Source-owned allowlist, transcribed from the PR209 remaining-base inventory.
# Never load refs, target paths or commands from a downloaded/user-supplied JSON.
TARGETS = (
    ("go-builder", "docker.io/library/golang:1.27.4-bookworm", "builder", ("deploy/Dockerfile:6", "deploy/sandbox-proxy.Dockerfile:5", "deploy/sandbox-import.Dockerfile:4", "deploy/sandbox-runtime.Dockerfile:6,14", "deploy/templates/mcp-gateway/Dockerfile:9")),
    ("web-builder", "docker.io/library/node:24.22.2-alpine", "builder", ("web/Dockerfile:2; APP=client|admin",)),
    ("web-runtime", "docker.io/library/nginx:1.30.6-alpine", "upstream runtime", ("web/Dockerfile:10; APP=client|admin",)),
    ("alpine-runtime-donor", "docker.io/library/alpine:3.24.4", "upstream runtime/donor", ("deploy/Dockerfile:19", "deploy/sandbox-runtime.Dockerfile:26")),
    ("proxy-distroless", "gcr.io/distroless/static-debian12:nonroot", "upstream runtime", ("deploy/sandbox-proxy.Dockerfile:14",)),
    ("import-distroless", "gcr.io/distroless/static:nonroot", "upstream runtime", ("deploy/sandbox-import.Dockerfile:11",)),
    ("template-e2b-base", "docker.io/e2bdev/base@sha256:8aebbf5e7d1209ebdc4bbc5999624556c2d9eebe91b9fded756ff2d688367065", "upstream runtime", ("deploy/templates/base/Dockerfile:3",)),
    ("template-cpu-python", "docker.io/library/python@sha256:bcaf104ef2827b827bdd0b8b351742783fc050296e1dc150113e6ec869e652d0", "upstream runtime", ("deploy/templates/code-interpreter/Dockerfile:6-7; CPU default", "deploy/templates/build.sh:43")),
    ("template-node", "docker.io/library/node@sha256:6b9d4ea31e614e29f364bf75758458731eb1e105b80c99667438dd6b3fe262e0", "upstream runtime", ("deploy/templates/node/Dockerfile:2",)),
    ("template-desktop-ubuntu", "docker.io/library/ubuntu@sha256:8a2321a92b438238f93b4346fff61bc4259c99fdd4d9b52ee86f153fee0c344f", "upstream runtime", ("deploy/templates/desktop/Dockerfile:5",)),
    ("template-playwright-python", "mcr.microsoft.com/playwright/python@sha256:7d030ac0638b279ecc72493f8b6706fecef80a893679a23b02f912905c4915c0", "upstream runtime", ("deploy/templates/browser/Dockerfile:4",)),
    ("mcp-node-donor", "docker.io/library/node:22-bookworm-slim@sha256:5ec23b7c622b00547753bc6ffd78085dd868a68aeb41c8e0a1418f033e13d96d", "binary donor (not shipped donor OS)", ("deploy/templates/mcp-gateway/Dockerfile:18,22; /usr/local/bin/node",)),
    ("external-python", "docker.io/library/python:3.12-slim@sha256:05a994d2344eb364cc186910f4fc8786a38215f39f00d0acc25bff33a08f4177", "external runtime (not PR rebuild)", ("deploy/templates/images.yaml:9",)),
)
BUDGET = 5100
INDEX_TYPES = {"application/vnd.oci.image.index.v1+json", "application/vnd.docker.distribution.manifest.list.v2+json"}
IMAGE_TYPES = {"application/vnd.oci.image.manifest.v1+json", "application/vnd.docker.distribution.manifest.v2+json"}


def digest(value):
    if not isinstance(value, str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", value):
        raise ValueError("missing or invalid sha256 digest")
    return value


def repository(ref):
    base = ref.split("@", 1)[0]
    # A registry port is not an image tag.
    return base.rsplit(":", 1)[0] if base.rfind(":") > base.rfind("/") else base


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON key")
        result[key] = value
    return result


def run_command(argv, deadline, timeout, **kwargs):
    remaining = deadline - time.monotonic() - 5
    if remaining <= 0:
        raise TimeoutError("base scan wall-clock budget exhausted")
    return subprocess.run(argv, timeout=min(timeout, remaining), check=False, **kwargs)


def inspect_json(ref, option, deadline):
    argv = ["docker", "buildx", "imagetools", "inspect", ref, *option]
    result = run_command(argv, deadline, 60, capture_output=True, text=True)
    if result.returncode or not result.stdout or len(result.stdout) > 8 * 1024 * 1024:
        raise ValueError("registry metadata inspection failed or exceeded byte budget")
    value = json.loads(result.stdout, object_pairs_hook=unique_object)
    if not isinstance(value, dict):
        raise ValueError("registry metadata must be an object")
    return value


def select_manifest(index):
    manifests = index.get("manifests")
    if not isinstance(manifests, list) or not manifests or len(manifests) > 256:
        raise ValueError("missing or oversized platform manifest list")
    candidates = []
    for entry in manifests:
        if not isinstance(entry, dict) or not isinstance(entry.get("platform"), dict):
            raise ValueError("invalid platform descriptor")
        platform = entry["platform"]
        if (platform.get("os") == "linux" and platform.get("architecture") == "amd64"
                and platform.get("variant", "") in ("", "v1")):
            if entry.get("mediaType") not in IMAGE_TYPES:
                raise ValueError("selected descriptor is not an image manifest")
            candidates.append(digest(entry.get("digest")))
    if len(candidates) != 1:
        raise ValueError("expected exactly one linux/amd64 image, excluding attestations")
    return candidates[0]


def resolve_image(ref, deadline):
    # Resolve once, then only inspect immutable refs: no mutable-tag scan race.
    descriptor = inspect_json(ref, ["--format", "{{json .Manifest}}"], deadline)
    source_digest = digest(descriptor.get("digest"))
    if "@" in ref and digest(ref.split("@", 1)[1]) != source_digest:
        raise ValueError("registry descriptor differs from source pin")
    repo = repository(ref)
    source_manifest = inspect_json(repo + "@" + source_digest, ["--raw"], deadline)
    media_type = source_manifest.get("mediaType")
    index_digest = None
    if media_type in INDEX_TYPES:
        index_digest = source_digest
        platform_digest = select_manifest(source_manifest)
        manifest = inspect_json(repo + "@" + platform_digest, ["--raw"], deadline)
    elif media_type in IMAGE_TYPES:
        platform_digest = source_digest
        manifest = source_manifest
        # A single manifest has no platform field; inspect its immutable config.
        # Unsupported CLI shapes fail closed, never guess the host platform.
        config = inspect_json(repo + "@" + platform_digest,
                              ["--format", "{{json .Image}}"], deadline)
        if (config.get("os") != "linux" or config.get("architecture") != "amd64"
                or config.get("variant", "") not in ("", "v1")):
            raise ValueError("single manifest config is not linux/amd64")
    else:
        raise ValueError("unsupported registry manifest media type")
    if manifest.get("mediaType") not in IMAGE_TYPES or not isinstance(manifest.get("config"), dict):
        raise ValueError("selected manifest lacks image config")
    config_digest = digest(manifest["config"].get("digest"))
    return {"ref": ref, "source_digest": source_digest, "index_digest": index_digest,
            "platform_manifest_digest": platform_digest, "config_digest": config_digest,
            "immutable_ref": repo + "@" + platform_digest, "platform": "linux/amd64"}


def scan_image(root, target_spec, sha, deadline):
    # Only tuples from our constant allowlist can name directories/registry inputs.
    if target_spec not in TARGETS:
        raise ValueError("target is not in the source allowlist")
    name, ref, role, scopes = target_spec
    target = root / "result" / "bases" / name
    target.mkdir(parents=True, exist_ok=False)
    provenance = {"targetid": name, "ref": ref, "role": role, "scopes": scopes,
                  "source_sha": sha, "coverage": "upstream registry blobs; no image execution"}
    status = 2
    try:
        provenance.update(resolve_image(ref, deadline))
        (target / "source.json").write_text(json.dumps(provenance, indent=2) + "\n", encoding="utf-8")
        (target / "image-id.txt").write_text(provenance["config_digest"] + "\n", encoding="utf-8")
        with (target / "trivy.log").open("w", encoding="utf-8") as log:
            scanned = run_command([
                str((root / "tools/trivy").resolve()), "image", "--config", "",
                "--ignorefile", "", "--disable-telemetry", "--skip-version-check",
                "--ignore-unfixed=false", "--exit-on-eol", "1", "--image-src", "remote",
                "--platform", "linux/amd64", "--scanners", "vuln", "--skip-db-update",
                "--list-all-pkgs", "--severity", "HIGH,CRITICAL", "--exit-code", "0",
                "--format", "json", "--output", str(target / "scan.json"), provenance["immutable_ref"],
            ], deadline, 600, stdout=log, stderr=subprocess.STDOUT)
        with (target / "gate.txt").open("w", encoding="utf-8") as log:
            gated = run_command([
                sys.executable, str(root / ".github/scripts/check_scan_report.py"),
                "--db-metadata", str(root / "result/db-metadata.json"),
                "--expected-image-id", provenance["config_digest"], str(target / "scan.json"),
            ], deadline, 30, stdout=log, stderr=subprocess.STDOUT)
        status = gated.returncode if gated.returncode in (0, 1) else 2
        if scanned.returncode and not (scanned.returncode == 1 and status == 1):
            status = 2
    except (OSError, ValueError, TypeError, TimeoutError, subprocess.TimeoutExpired) as exc:
        with (target / "gate.txt").open("a", encoding="utf-8") as log:
            log.write("Incomplete scan: " + json.dumps(str(exc)) + "\n")
    (target / "source.json").write_text(json.dumps(provenance, indent=2) + "\n", encoding="utf-8")
    (target / "status.txt").write_text(str(status) + "\n", encoding="utf-8")
    print(f"Base {name}: status {status}", flush=True)
    return status


def main():
    sha = os.environ.get("SOURCE_SHA", "")
    if not re.fullmatch(r"[0-9a-f]{40}", sha):
        print("SOURCE_SHA must be a lowercase 40-character commit SHA", file=sys.stderr)
        return 2
    root = Path.cwd().resolve()
    (root / "result").mkdir(exist_ok=True)
    deadline = time.monotonic() + BUDGET
    statuses, errors = {}, []
    for spec in TARGETS:
        try:
            statuses[spec[0]] = scan_image(root, spec, sha, deadline)
        except (OSError, ValueError) as exc:
            errors.append({"targetid": spec[0], "error": str(exc)})
    missing = sorted({spec[0] for spec in TARGETS} - statuses.keys())
    final = 2 if errors or missing or 2 in statuses.values() else (1 if 1 in statuses.values() else 0)
    summary = {"status": final, "source_sha": sha, "targets": statuses,
               "missing": missing, "errors": errors, "deferred": ["gpu-pytorch-cuda"],
               "attribution": "Raw upstream bases, not proof of final installed package versions."}
    (root / "result/base-status.json").write_text(json.dumps(summary, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(summary, sort_keys=True), flush=True)
    return final


if __name__ == "__main__":
    sys.exit(main())
