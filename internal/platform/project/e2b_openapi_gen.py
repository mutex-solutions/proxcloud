"""Regenerates e2b_openapi.json: E2B's own OpenAPI document (e2b-dev/runtime spec/openapi.yml,
Apache-2.0) cut down to the operations ProxCloud serves, converted to OpenAPI 3.1, with ProxCloud's
differences added (docs/e2b-compat.md). The router parity test (e2b_openapi_test.go) fails when a
route and this document disagree.

    curl -fsSLo /tmp/e2b-openapi.yml https://raw.githubusercontent.com/e2b-dev/runtime/e85b9f5/spec/openapi.yml
    python3 -I internal/platform/project/e2b_openapi_gen.py /tmp/e2b-openapi.yml > internal/platform/project/e2b_openapi.json
"""

import copy
import json
import sys

import yaml

UPSTREAM = "e2b-dev/runtime@e85b9f5 spec/openapi.yml"

# (method, path) -> ProxCloud difference, appended to the description. None = same as E2B.
OPS = {
    ("get", "/health"): None,
    ("post", "/sandboxes"): "ProxCloud: `secure` is ignored (envd always needs its token); `volumeMounts` → 400; `mcp` must be an object (the SDK starts the gateway). `autoPause` pauses the filesystem only: `autoPauseMemory: true` → 400, absent or false is a filesystem pause; 400 where the location cannot take snapshots. `autoResume` (with `autoPause`) works on that filesystem pause: traffic to the paused sandbox's ports cold-boots it; 400 where the location has no wake token. Waits for phased startup (default total/scheduling/pull/runtime 900/120/720/600 s, hard total 1800), with a first-Ready runtime TTL and one outer cancellation/deadline envelope including final access. Failure cleanup is bounded to 30 s; no expired usable success. Passes the same checks as the native create: suspended project 409, suspended billing profile 403, provider DISABLED or a release gate not holding 503, project quota 429. Extensions: `gpu`, `cpuCount`, `memoryMB`.",
    ("post", "/v2/sandboxes"): "Same as POST /sandboxes.",
    ("get", "/sandboxes"): "ProxCloud: running and paused sandboxes; everything in one page.",
    ("get", "/v2/sandboxes"): "ProxCloud: everything in one page.",
    ("post", "/sandboxes/{sandboxID}/pause"): "ProxCloud: filesystem only. `memory: true` → 400; absent or false pauses the filesystem (the default here, unlike E2B's). `requireHealthyGuest` is implied: the capture runs in the sandbox. The paused snapshot is billed per GB-hour until the sandbox is resumed or killed.",
    ("post", "/sandboxes/{sandboxID}/resume"): "ProxCloud: always a cold boot from the filesystem snapshot. Passes the usage checks of the create (409, 403, 503) and the project quota (429). Default timeout 300 s.",
    ("post", "/sandboxes/{sandboxID}/fork"): "ProxCloud: filesystem only (the forks cold-boot from a snapshot of the source, with new ids and tokens); at most 10 forks; passes the usage checks of the create and the project quota.",
    ("post", "/sandboxes/{sandboxID}/snapshots"): "ProxCloud: filesystem only, `memory: true` → 400. Passes the usage checks of the create. Billed per GB-hour until DELETE /templates/{templateID}.",
    ("get", "/snapshots"): "ProxCloud: everything in one page.",
    ("get", "/sandboxes/metrics"): "ProxCloud: always empty.",
    ("get", "/sandboxes/{sandboxID}"): "ProxCloud: `diskSizeMB` is always 0.",
    ("delete", "/sandboxes/{sandboxID}"): None,
    ("post", "/sandboxes/{sandboxID}/timeout"): "ProxCloud: passes the usage checks of the create (409, 403, 503).",
    ("post", "/sandboxes/{sandboxID}/refreshes"): "ProxCloud: an extension passes the usage checks of the create.",
    ("post", "/sandboxes/{sandboxID}/connect"): "ProxCloud: a paused sandbox is resumed (201) as a cold boot whatever `memory` says, after the usage checks of the create; an extension of a running one passes them too.",
    ("post", "/v2/sandboxes/{sandboxID}/connect"): "Same as POST /sandboxes/{sandboxID}/connect.",
    ("put", "/sandboxes/{sandboxID}/network"): "ProxCloud: `allowOut`, `denyOut`, `dnsServers`; `allowPublicTraffic` only at create.",
    ("get", "/sandboxes/{sandboxID}/logs"): "ProxCloud: the newest 1000 lines; no `start` or `limit`.",
    ("get", "/v2/sandboxes/{sandboxID}/logs"): "ProxCloud: the newest 1000 lines; no cursor.",
    ("get", "/sandboxes/{sandboxID}/metrics"): "ProxCloud: always empty.",
    ("get", "/templates"): "ProxCloud: the project's templates, then the location's built-in ones.",
    ("get", "/templates/aliases/{alias}"): None,
    ("get", "/templates/{templateID}"): None,
    ("delete", "/templates/{templateID}"): None,
    ("get", "/templates/{templateID}/tags"): None,
    ("post", "/templates/tags"): None,
    ("delete", "/templates/tags"): None,
    ("post", "/v3/templates"): "ProxCloud: 503 where the location has no template registry or build bucket.",
    ("get", "/templates/{templateID}/files/{hash}"): "ProxCloud: the upload URL points at this API (PUT …/upload, authenticated by the token in it), up to 2 GiB.",
    ("post", "/v2/templates/{templateID}/builds/{buildID}"): "ProxCloud: steps RUN, COPY, ENV, WORKDIR, USER; `fromImageRegistry` types registry, gcp and aws (explicit caller keys exchanged for a region-matching ECR token); independent buildCpu/buildMemoryGiB/buildDiskGiB, not runtime size; stable stage-prefix paged diagnostics; no layer cache between builds; 30 min per build, provider-configured concurrency (default 2) per project; passes the usage checks of the sandbox create.",
    ("get", "/templates/{templateID}/builds/{buildID}/status"): None,
}

GPU = {"type": "object", "description": "ProxCloud extension: whole GPUs of a model the location offers.",
       "required": ["model"], "properties": {"model": {"type": "string", "examples": ["nvidia-3090"]},
                                             "count": {"type": "integer", "minimum": 1, "default": 1}}}


def to31(node):
    """OpenAPI 3.0 -> 3.1: nullable becomes a "null" type (`example` is still valid in 3.1)."""
    if isinstance(node, dict):
        node = {k: to31(v) for k, v in node.items()}
        if node.pop("nullable", False):
            if "type" in node:
                t = node["type"]
                node["type"] = (t if isinstance(t, list) else [t]) + ["null"]
            elif "$ref" in node or "allOf" in node or "oneOf" in node or "anyOf" in node:
                node = {"anyOf": [{k: v for k, v in node.items() if k not in ("description",)}, {"type": "null"}],
                        **({"description": node["description"]} if "description" in node else {})}
        return node
    if isinstance(node, list):
        return [to31(x) for x in node]
    return node


def refs(node, out):
    if isinstance(node, dict):
        r = node.get("$ref")
        if isinstance(r, str) and r.startswith("#/components/"):
            out.add(tuple(r.split("/")[2:4]))
        for v in node.values():
            refs(v, out)
    elif isinstance(node, list):
        for v in node:
            refs(v, out)


def main():
    up = yaml.safe_load(open(sys.argv[1]))
    paths = {}
    for (m, p), note in OPS.items():
        op = copy.deepcopy(up["paths"][p][m])
        if note:
            op["description"] = (op.get("description", "") + "\n\n" + note).strip()
        paths.setdefault(p, {})
        for k in ("parameters",):
            if k in up["paths"][p]:
                paths[p][k] = copy.deepcopy(up["paths"][p][k])
        # ProxCloud's auth: the sandbox API key in X-API-Key or as a bearer; /health is open.
        op["security"] = [] if p == "/health" else [{"ApiKeyAuth": []}, {"ApiKeyBearer": []}]
        if p == "/health":
            op["responses"] = {"200": {"description": "The service is healthy"}}
        paths[p][m] = op
    comps = up.get("components", {})
    keep, todo = set(), set()
    refs(paths, todo)
    while todo:
        k = todo.pop()
        if k in keep or k[1] not in comps.get(k[0], {}):
            continue
        keep.add(k)
        refs(comps[k[0]][k[1]], todo)
    components = {"securitySchemes": {
        "ApiKeyAuth": comps["securitySchemes"]["ApiKeyAuth"],
        "ApiKeyBearer": {"type": "http", "scheme": "bearer", "description": "The same sandbox API key as a bearer token."},
    }}
    for kind, name in sorted(keep):
        components.setdefault(kind, {})[name] = comps[kind][name]
    # ProxCloud create extensions.
    nsb = components.get("schemas", {}).get("NewSandbox")
    if nsb:
        nsb.setdefault("properties", {}).update({
            "gpu": GPU,
            "cpuCount": {"type": "integer", "minimum": 1, "description": "ProxCloud extension: vCPUs within the location's limit."},
            "memoryMB": {"type": "integer", "minimum": 1, "description": "ProxCloud extension: memory, rounded up to whole GiB."},
        })
    doc = {
        "openapi": "3.1.0",
        "info": {
            "title": "ProxCloud E2B-compatible API",
            "version": "1",
            "description": "The E2B API as ProxCloud serves it, for the stock E2B SDKs and CLI. Generated from "
                           + UPSTREAM + " (Copyright E2B, Apache License 2.0), cut down to the operations ProxCloud "
                           "serves; ProxCloud's differences are noted per operation and in the docs (E2B API page).",
            "license": {"name": "Apache-2.0", "identifier": "Apache-2.0"},
        },
        "servers": [{"url": "https://api.{domain}/e2b", "variables": {"domain": {"default": "example.org",
                                                                            "description": "the portal's API host"}}}],
        "paths": paths,
        "components": components,
    }
    json.dump(to31(doc), sys.stdout, indent=1, sort_keys=True)
    print()


main()
