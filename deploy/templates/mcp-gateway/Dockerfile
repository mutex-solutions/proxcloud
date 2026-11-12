# syntax=docker/dockerfile:1
# mcp-gateway: what the E2B SDKs start for Sandbox.create(mcp={...}) (docs/managed-sandbox.md §9): the base template
# plus the ProxCloud mcp-gateway (cmd/mcp-gateway) and the MCP servers it runs as local processes. Remote servers need
# nothing here. Build context "src" is the repository root (build.sh).
# Preinstalled only: no account or API key of any server is in the image; the tenant passes them in the SDK's mcp
# config, which never reaches the portal's database or logs.
ARG BASE

FROM docker.io/library/golang:1.27.4-alpine AS gateway
WORKDIR /src
COPY --from=src go.mod go.sum ./
RUN go mod download
COPY --from=src cmd/mcp-gateway ./cmd/mcp-gateway
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mcp-gateway ./cmd/mcp-gateway

# The servers' own Node uses the same maintained Node 22 donor as the base.
# Keep its separate path so MCP server process requirements remain explicit.
FROM docker.io/library/node:22-bookworm-slim@sha256:47725aca53d3295ec2d3be8f62726599b3033cc262edc2c7e429909b3ac599fc AS node

FROM ${BASE}
USER root
COPY --from=node /usr/local/bin/node /opt/mcp/node/bin/node
# Package servers (cmd/mcp-gateway/gencatalog.py, PACKAGES), pinned. npm ones under /opt/mcp/npm (installed with the
# base's npm, which skips fsevents; run with /opt/mcp/node), Python ones as uv tools with their entry points in
# /opt/mcp/bin; the gateway puts all three first on its servers' PATH.
# The npm servers are a private npm project rooted at /opt/mcp/npm/lib, not `npm install -g`: only a project root can
# apply "overrides", which lift the transitive @modelcontextprotocol/sdk (exact 1.26.0/1.30.0 pins in tavily-mcp,
# brave and exa's mcp-handler peer) and basic-ftp (mongodb-mcp-server -> ... -> get-uri ^5) to fixed releases.
# --install-strategy=shallow is the layout `-g` uses: each server at lib/node_modules/<name> with its own dependency
# tree, and only the servers' own bins in lib/node_modules/.bin, which /opt/mcp/npm/bin links to (the gateway's PATH).
RUN <<'EOF'
set -eu
mkdir -p /opt/mcp/npm/lib
cat > /opt/mcp/npm/lib/package.json <<'JSON'
{
  "name": "proxcloud-mcp-servers",
  "private": true,
  "dependencies": {
    "@modelcontextprotocol/server-filesystem": "2.2.0",
    "@modelcontextprotocol/server-memory": "2.1.0",
    "@modelcontextprotocol/server-sequential-thinking": "2.1.1",
    "@wonderwhy-er/desktop-commander": "0.3.1",
    "@notionhq/notion-mcp-server": "2.6.1",
    "@brave/brave-search-mcp-server": "2.1.4",
    "exa-mcp-server": "3.4.3",
    "firecrawl-mcp": "3.38.0",
    "tavily-mcp": "0.2.22",
    "mongodb-mcp-server": "3.2.2",
    "server-perplexity-ask": "0.1.3"
  },
  "overrides": {
    "@modelcontextprotocol/sdk": "1.32.2",
    "basic-ftp": "6.2.6"
  }
}
JSON
npm install --prefix /opt/mcp/npm/lib --install-strategy=shallow --omit=dev --no-audit --no-fund
npm cache clean --force
ln -s lib/node_modules/.bin /opt/mcp/npm/bin
# Fail unless every installed copy has the overridden version (and each package is present at all).
find /opt/mcp/npm/lib/node_modules -path '*/node_modules/@modelcontextprotocol/sdk/package.json' \
  -o -path '*/node_modules/basic-ftp/package.json' | /opt/mcp/node/bin/node -e '
const fs = require("fs");
const want = { "@modelcontextprotocol/sdk": "1.32.2", "basic-ftp": "6.2.6" };
const seen = {};
for (const f of fs.readFileSync(0, "utf8").split("\n").filter(Boolean)) {
  const p = JSON.parse(fs.readFileSync(f, "utf8"));
  if (p.version !== want[p.name]) { console.error(`${f}: ${p.name}@${p.version}, want ${want[p.name]}`); process.exit(1); }
  seen[p.name] = (seen[p.name] || 0) + 1;
}
for (const n of Object.keys(want)) if (!seen[n]) { console.error(`no ${n} under /opt/mcp/npm/lib`); process.exit(1); }
console.log(JSON.stringify(seen));
'
EOF
ENV UV_TOOL_DIR=/opt/mcp/uv UV_TOOL_BIN_DIR=/opt/mcp/bin UV_PYTHON_INSTALL_DIR=/opt/mcp/python
RUN python3 -m pip install --no-cache-dir --break-system-packages uv==0.13.4 \
 && for p in mcp-server-fetch==2026.11.4 mcp-server-time==2026.11.4 mcp-server-git==2026.11.4 \
             duckduckgo-mcp-server==0.7.0 wikipedia-mcp==2.0.1 markitdown-mcp==0.0.1a8; do \
      uv tool install --no-cache "$p" || exit 1; done
# desktopCommander downloads Chrome (for its PDF tools) at every start unless its cache has one:
# fill the cache of root, the user the gateway runs as.
RUN cd /opt/mcp/npm/lib/node_modules/@wonderwhy-er/desktop-commander \
 && /opt/mcp/node/bin/node -e "import('@puppeteer/browsers').then(async (b) => { const p = b.detectBrowserPlatform(); \
      const id = await b.resolveBuildId(b.Browser.CHROME, p, 'stable'); \
      await b.install({ browser: b.Browser.CHROME, buildId: id, cacheDir: '/root/.claude-server-commander/puppeteer-cache' }); })"
COPY --from=gateway /out/mcp-gateway /usr/local/bin/mcp-gateway
# Fails the build unless every catalog package server starts here and answers MCP initialize and tools/list. The servers
# write state at start (desktopCommander a client id, mongodb a log, onnxruntime a device id and a session file): run
# them with a throwaway HOME and TMPDIR so none of it ships in the image and every sandbox starts clean. The Chrome
# cache above is linked in, or desktopCommander would download Chrome again. The test fails the build if state leaks.
RUN h=$(mktemp -d) && mkdir -p "$h/home/.claude-server-commander" "$h/tmp" \
 && ln -s /root/.claude-server-commander/puppeteer-cache "$h/home/.claude-server-commander/puppeteer-cache" \
 && HOME="$h/home" TMPDIR="$h/tmp" mcp-gateway pull $(mcp-gateway list) >/dev/null \
 && rm -rf "$h" \
 && for f in /root/.claude-server-commander/config.json /root/.mongodb /root/.cache/Microsoft /tmp/.ses; do \
      if [ -e "$f" ]; then echo "server state left in the image: $f" >&2; exit 1; fi; done \
 && mkdir -p /var/log/mcp-gateway && chmod 700 /var/log/mcp-gateway
WORKDIR /home/user
