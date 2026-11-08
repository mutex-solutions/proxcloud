"""MCP gateway check with the stock E2B SDK. Same setup as compat.py (E2B_API_URL, E2B_API_KEY,
SSL_CERT_FILE for a private CA). Needs the location's `mcp-gateway` template. Uses only servers that
need no key, and brave with a dummy key for where keys come from (docs/managed-sandbox.md §9).

    python mcp.py
"""

import json
import sys
import uuid

import httpx
from e2b import Sandbox, SandboxException

failures = []


def check(name, cond, detail=""):
    print(("ok   " if cond else "FAIL ") + name + (f"  ({detail})" if detail and not cond else ""))
    if not cond:
        failures.append(name)


class Client:
    """Just enough streamable-HTTP MCP: JSON or SSE answers, the session header."""

    def __init__(self, url, token):
        self.url, self.n, self.sid = url, 0, None
        self.h = {"Authorization": f"Bearer {token}", "Accept": "application/json, text/event-stream"}

    def rpc(self, method, params=None, notify=False):
        self.n += 1
        body = {"jsonrpc": "2.0", "method": method, **({"params": params} if params is not None else {})}
        if not notify:
            body["id"] = self.n
        h = dict(self.h, **({"Mcp-Session-Id": self.sid} if self.sid else {}))
        r = httpx.post(self.url, json=body, headers=h, timeout=60)
        r.raise_for_status()
        self.sid = r.headers.get("Mcp-Session-Id", self.sid)
        if notify:
            return None
        if r.headers.get("content-type", "").startswith("text/event-stream"):
            data = [ln[5:].strip() for ln in r.text.splitlines() if ln.startswith("data:")]
            return json.loads(data[-1])["result"]
        return r.json()["result"]


s = Sandbox.create(mcp={"deepwiki": {}, "fetch": {}, "time": {}}, timeout=300)
try:
    url, tok = s.get_mcp_url(), s.get_mcp_token()
    check("get_mcp_url / get_mcp_token", url.endswith("/mcp") and "50005-" in url and bool(tok))
    c = Client(url, tok)
    c.rpc("initialize", {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "t", "version": "1"}})
    c.rpc("notifications/initialized", notify=True)
    names = [t["name"] for t in c.rpc("tools/list")["tools"]]
    check("tools named <server>-<tool>", any(n.startswith("deepwiki-") for n in names)
          and "fetch-fetch" in names and any(n.startswith("time-") for n in names), str(names))
    out = c.rpc("tools/call", {"name": "time-get_current_time", "arguments": {"timezone": "UTC"}})
    check("a local server's tool runs", "UTC" in json.dumps(out), json.dumps(out)[:300])
    r = httpx.post(url, json={}, headers={"Accept": "application/json, text/event-stream"}, timeout=20)
    check("the gateway needs its bearer", r.status_code == 401, str(r.status_code))
    check("token file is root-only", s.commands.run("stat -c %a /etc/mcp-gateway/.token", user="root").stdout.strip() == "600")
finally:
    s.kill()

# A server that needs a key gets the tenant's own, from the mcp config, in that server's environment; nothing of the
# platform's reaches it (no PROXCLOUD_ or Kubernetes variables, no service account token). A dummy key: brave starts
# and lists its tools without calling Brave.
KEY = "mcp-check-" + uuid.uuid4().hex
s = Sandbox.create(mcp={"brave": {"apiKey": KEY}}, timeout=300)
try:
    root = lambda cmd: s.commands.run(cmd, user="root", timeout=60).stdout
    pids = root("for p in /proc/[0-9]*; do [ ${p#/proc/} = $$ ] && continue; tr '\\0' ' ' < $p/cmdline 2>/dev/null | grep -q brave-search && echo ${p#/proc/}; done; true").split()
    env = "\n".join(root(f"tr '\\0' '\\n' < /proc/{p}/environ 2>/dev/null || true") for p in pids)
    check("a keyed server gets the tenant's key", f"BRAVE_API_KEY={KEY}" in env, str(pids))
    check("no platform variable reaches it", "PROXCLOUD_" not in env and "KUBERNETES_" not in env, env[:300])
    check("no service account token in the sandbox",
          root("test -e /var/run/secrets/kubernetes.io/serviceaccount/token && echo present || echo missing").strip() == "missing")
finally:
    s.kill()

try:
    Sandbox.create(mcp={"brave": {}}, timeout=60)
    check("a keyed server without the tenant's key fails the create", False, "created")
except SandboxException as e:
    check("a keyed server without the tenant's key fails the create", "apiKey" in str(e), str(e)[:300])

try:
    Sandbox.create(mcp={"notAServer": {}}, timeout=60)
    check("unknown server fails the create", False, "created")
except SandboxException as e:
    check("unknown server fails the create", "not available on ProxCloud" in str(e), str(e)[:300])

print("all mcp checks passed" if not failures else f"{len(failures)} failed: {failures}")
sys.exit(1 if failures else 0)
