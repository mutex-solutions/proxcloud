# syntax=docker/dockerfile:1
# mcp-gateway: what the E2B SDKs start for Sandbox.create(mcp={...}) (docs/managed-sandbox.md §9): the base template
# plus the ProxCloud mcp-gateway (cmd/mcp-gateway) and the MCP servers it runs as local processes. Remote servers need
# nothing here. Build context "src" is the repository root (build.sh).
# Preinstalled only: no account or API key of any server is in the image; the tenant passes them in the SDK's mcp
# config, which never reaches the portal's database or logs.
ARG BASE

FROM docker.io/library/golang:1.25-bookworm AS gateway
WORKDIR /src
COPY --from=src go.mod go.sum ./
RUN go mod download
COPY --from=src cmd/mcp-gateway ./cmd/mcp-gateway
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mcp-gateway ./cmd/mcp-gateway

# The servers' own Node: the base image's Node 20.9 cannot run brave, firecrawl and mongodb (import attributes,
# process.loadEnvFile). Same Debian release as the base; the sandbox user's node stays the base's.
FROM docker.io/library/node:22-bookworm-slim@sha256:5ec23b7c622b00547753bc6ffd78085dd868a68aeb41c8e0a1418f033e13d96d AS node

FROM ${BASE}
USER root
COPY --from=node /usr/local/bin/node /opt/mcp/node/bin/node
# Package servers (cmd/mcp-gateway/gencatalog.py, PACKAGES), pinned. npm ones under /opt/mcp/npm (installed with the
# base's npm, which skips fsevents; run with /opt/mcp/node), Python ones as uv tools with their entry points in
# /opt/mcp/bin; the gateway puts all three first on its servers' PATH.
RUN npm install -g --prefix /opt/mcp/npm \
      @modelcontextprotocol/server-filesystem@2.2.0 \
      @modelcontextprotocol/server-memory@2.1.0 \
      @modelcontextprotocol/server-sequential-thinking@2.1.1 \
      @wonderwhy-er/desktop-commander@0.3.1 \
      @notionhq/notion-mcp-server@2.6.1 \
      @brave/brave-search-mcp-server@2.1.4 \
      exa-mcp-server@3.4.3 \
      firecrawl-mcp@3.38.0 \
      tavily-mcp@0.2.22 \
      mongodb-mcp-server@3.2.2 \
      server-perplexity-ask@0.1.3 \
 && npm cache clean --force
ENV UV_TOOL_DIR=/opt/mcp/uv UV_TOOL_BIN_DIR=/opt/mcp/bin UV_PYTHON_INSTALL_DIR=/opt/mcp/python
RUN python3 -m pip install --no-cache-dir --break-system-packages uv==0.13.0 \
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
