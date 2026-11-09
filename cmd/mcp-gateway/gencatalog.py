"""Regenerates catalog.json: the MCP servers the ProxCloud mcp-gateway can run, keyed like the E2B
SDK's `mcp` option. Remote servers come from Docker's MCP catalog (E2B's servers are Docker's,
named by the slug in the SDK spec's x-dockerHubUrl); package servers are the hand-picked PACKAGES
below, installed in the mcp-gateway template image (deploy/templates/mcp-gateway/Dockerfile).

    curl -fsSLo /tmp/sdk.json https://raw.githubusercontent.com/e2b-dev/E2B/main/spec/mcp-server.json
    curl -fsSLo /tmp/docker.yaml https://desktop.docker.com/mcp/catalog/v3/catalog.yaml
    python3 -I cmd/mcp-gateway/gencatalog.py /tmp/sdk.json /tmp/docker.yaml > cmd/mcp-gateway/catalog.json

Placeholders: "{field}" is the SDK config field; "{paths...}" in cmd expands a list.
"""

import json
import re
import sys

import yaml

# SDK key -> how to run it from the packages baked into the template image.
PACKAGES = {
    "fetch": {"cmd": ["mcp-server-fetch"]},
    "time": {"cmd": ["mcp-server-time"]},
    "git": {"cmd": ["mcp-server-git"]},
    "filesystem": {"cmd": ["mcp-server-filesystem", "{paths...}"]},
    "memory": {"cmd": ["mcp-server-memory"]},
    "sequentialthinking": {"cmd": ["mcp-server-sequential-thinking"]},
    "duckduckgo": {"cmd": ["duckduckgo-mcp-server"]},
    "wikipedia": {"cmd": ["wikipedia-mcp"]},
    "markitdown": {"cmd": ["markitdown-mcp"]},
    "desktopCommander": {"cmd": ["desktop-commander"]},
    "notion": {"cmd": ["notion-mcp-server"],
               "env": {"OPENAPI_MCP_HEADERS": '{"Authorization": "Bearer {internalIntegrationToken}", "Notion-Version": "2022-06-28"}'}},
    "brave": {"cmd": ["brave-search-mcp-server"], "env": {"BRAVE_API_KEY": "{apiKey}", "BRAVE_MCP_TRANSPORT": "stdio"}},
    "exa": {"cmd": ["exa-mcp-server"], "env": {"EXA_API_KEY": "{apiKey}"}},
    "firecrawl": {"cmd": ["firecrawl-mcp"], "env": {"FIRECRAWL_API_KEY": "{apiKey}", "FIRECRAWL_API_URL": "{url}"}},
    "tavily": {"cmd": ["tavily-mcp"], "env": {"TAVILY_API_KEY": "{apiKey}"}},
    "mongodb": {"cmd": ["mongodb-mcp-server"], "env": {"MDB_MCP_CONNECTION_STRING": "{mdbMcpConnectionString}"}},
    "perplexityAsk": {"cmd": ["mcp-server-perplexity-ask"], "env": {"PERPLEXITY_API_KEY": "{perplexityApiKey}"}},
}
# Image servers whose vendor also runs a remote endpoint that takes the same credential.
REMOTE_OVERRIDES = {
    "githubOfficial": {"url": "https://api.githubcopilot.com/mcp/", "transport": "streamable-http",
                       "headers": {"Authorization": "Bearer {githubPersonalAccessToken}"}},
    "stripe": {"url": "https://mcp.stripe.com", "transport": "streamable-http",
               "headers": {"Authorization": "Bearer {secretKey}"}},
}


def camel(s):
    head, *rest = s.split("_")
    return head + "".join(w[:1].upper() + w[1:] for w in rest)


def main():
    sdk = json.load(open(sys.argv[1]))["properties"]
    cat = yaml.safe_load(open(sys.argv[2]))["registry"]
    out, skipped = {}, []
    for key, p in sorted(sdk.items()):
        slug = p.get("x-dockerHubUrl", "").rstrip("/").removesuffix("/overview").split("/")[-1]
        s = cat.get(slug) or {}
        fields = list((p.get("properties") or {}).keys())
        e = {"name": slug, "fields": fields, "required": p.get("required", [])}
        if key in PACKAGES:
            out[key] = {**e, **PACKAGES[key]}
            continue
        if key in REMOTE_OVERRIDES:
            out[key] = {**e, **REMOTE_OVERRIDES[key]}
            continue
        r = s.get("remote")
        if not r:
            continue  # an image server not in PACKAGES: not available
        envs = {x["env"]: camel(x["name"].split(".", 1)[1]) for x in s.get("secrets") or []}
        headers = {}
        for h, v in (r.get("headers") or {}).items():
            def sub(m):
                f = envs.get(m.group(1))
                if f not in fields:
                    f = fields[0] if len(fields) == 1 else None
                if f is None:
                    raise KeyError(m.group(1))
                return "{" + f + "}"
            try:
                headers[h] = re.sub(r"\$\{(\w+)\}", sub, v)
            except KeyError as k:
                skipped.append(f"{key}: header {h} needs {k}")
                break
        else:
            if fields and not headers:
                skipped.append(f"{key}: fields {fields} but the remote takes no header (OAuth?)")
                continue
            out[key] = {**e, "url": r["url"], "transport": r.get("transport_type", "streamable-http"), **({"headers": headers} if headers else {})}
    for s in skipped:
        print("skipped", s, file=sys.stderr)
    json.dump(out, sys.stdout, indent=1, sort_keys=True)
    print()


main()
