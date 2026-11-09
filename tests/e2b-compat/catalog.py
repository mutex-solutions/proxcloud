"""Built-in template catalogue check: each template starts through the stock E2B SDKs and does
what it is for. Same setup as compat.py (E2B_API_URL, E2B_API_KEY, SSL_CERT_FILE for a private CA).
Needs `pip install e2b e2b-code-interpreter e2b-desktop`; name the templates the location offers.

    python catalog.py [template ...]
"""

import sys
import time
import urllib.request

from e2b import Sandbox

failures = []


def check(name, cond, detail=""):
    print(("ok   " if cond else "FAIL ") + name + (f"  ({detail})" if detail and not cond else ""))
    if not cond:
        failures.append(name)


def timed_create(factory, *args, **kw):
    t0 = time.time()
    s = factory(*args, **kw)
    print(f"     created in {time.time() - t0:.1f}s")
    return s


def run(s, cmd, **kw):
    r = s.commands.run(cmd, timeout=120, **kw)
    return r.stdout.strip()


def t_base():
    s = timed_create(Sandbox.create, timeout=300)  # no template: "base"
    try:
        out = run(s, "whoami; pwd; echo $SHELL; node -v; git --version | cut -d' ' -f3; gh --version | head -1 | cut -d' ' -f3")
        lines = out.splitlines()
        check("base: Sandbox.create() with no template", lines[:3] == ["user", "/home/user", "/bin/bash"], out)
        check("base: node, git, gh", len(lines) >= 6 and lines[3].startswith("v"), out)
        check("base: passwordless sudo", run(s, "sudo -n id -u") == "0")
        check("base: sudo apt-get works", s.commands.run("sudo apt-get update -qq >/dev/null 2>&1 && sudo apt-get install -y -qq cowsay >/dev/null 2>&1 && test -x /usr/games/cowsay", timeout=300).exit_code == 0)
    finally:
        s.kill()


def t_code_interpreter(template="code-interpreter-v1", gpu=False):
    from e2b_code_interpreter import Sandbox as CI

    kw = {} if template == "code-interpreter-v1" else {"template": template}
    s = timed_create(CI.create, timeout=300, envs={"FOO": "bar"}, **kw)
    try:
        ex = s.run_code("x = 21 * 2\nx")
        check(f"{template}: run_code result", ex.text == "42", str(ex))
        ex = s.run_code("import matplotlib.pyplot as plt\nplt.plot([1, 3, 2])\nplt.show()")
        check(f"{template}: chart comes back as png", any(r.png for r in ex.results), str(ex))
        ex = s.run_code("console.log(6 * 7)", language="js")
        check(f"{template}: javascript kernel", "42" in "".join(ex.logs.stdout), str(ex))
        ex = s.run_code("import os; os.environ.get('FOO')")
        check(f"{template}: envs reach the kernel", ex.text in ("bar", "'bar'"), str(ex))
        ctx = s.create_code_context(cwd="/tmp")
        ex = s.run_code("import os; os.getcwd()", context=ctx)
        check(f"{template}: code context with cwd", ex.text in ("/tmp", "'/tmp'"), str(ex))
        out = []
        s.run_code("print('streamed')", on_stdout=lambda m: out.append(m.line if hasattr(m, "line") else str(m)))
        check(f"{template}: stdout streams", any("streamed" in o for o in out), str(out))
        if gpu:
            ex = s.run_code("import torch; (torch.cuda.is_available(), torch.cuda.get_device_name(0))")
            check(f"{template}: CUDA in the kernel", ex.text and "True" in ex.text and "3090" in ex.text, str(ex))
    finally:
        s.kill()


def t_desktop():
    from e2b_desktop import Sandbox as Desktop

    s = timed_create(Desktop.create, timeout=300)
    try:
        img = s.screenshot()
        check("desktop: screenshot is a PNG", bytes(img[:8]) == b"\x89PNG\r\n\x1a\n" and len(img) > 5000, f"{len(img)} bytes")
        s.move_mouse(200, 200)
        s.left_click()
        s.write("hello")
        s.press("enter")
        check("desktop: mouse and keyboard", True)
        s.stream.start()
        url = s.stream.get_url()
        body = urllib.request.urlopen(url.split("?")[0], timeout=20).read(2000).decode(errors="ignore")
        check("desktop: noVNC live view loads", "noVNC" in body or "<html" in body.lower(), url)
        s.stream.stop()
        out = run(s, "command -v google-chrome code firefox-esr | wc -l")
        check("desktop: Chrome, VS Code, Firefox installed", out == "3", out)
    finally:
        s.kill()


def t_cli(template, cmd, expect):
    s = timed_create(Sandbox.create, template, timeout=300)
    try:
        out = run(s, cmd)
        check(f"{template}: {cmd}", expect in out, out)
    finally:
        s.kill()


def t_browser():
    s = timed_create(Sandbox.create, "browser", timeout=300)
    try:
        code = (
            "from playwright.sync_api import sync_playwright\n"
            "with sync_playwright() as p:\n"
            "    b = p.chromium.launch(args=['--no-sandbox'])\n"
            "    pg = b.new_page(); pg.goto('https://example.com'); print(pg.title()); b.close()\n"
        )
        s.files.write("/home/user/pw.py", code)
        out = run(s, "python3 /home/user/pw.py")
        check("browser: Chromium loads a page", "Example Domain" in out, out)
    finally:
        s.kill()


TESTS = {
    "base": t_base,
    "code-interpreter-v1": t_code_interpreter,
    "code-interpreter-gpu": lambda: t_code_interpreter("code-interpreter-gpu", gpu=True),
    "desktop": t_desktop,
    "codex": lambda: t_cli("codex", "codex --version", "codex-cli"),
    "opencode": lambda: t_cli("opencode", "opencode --version", "."),
    "claude": lambda: t_cli("claude", "claude --version", "Claude Code"),
    "amp": lambda: t_cli("amp", "amp --version", "released"),
    "gemini": lambda: t_cli("gemini", "gemini --version", "."),
    "node": lambda: t_cli("node", "node -v", "v24"),
    "browser": t_browser,
    "mcp-gateway": lambda: t_cli("mcp-gateway", "mcp-gateway list | wc -l", "47"),  # the gateway itself: mcp.py
}

for name in sys.argv[1:] or list(TESTS):
    print(f"== {name}")
    try:
        TESTS[name]()
    except Exception as e:  # keep going: report every template
        check(f"{name}: no exception", False, repr(e)[:300])
print("all catalogue checks passed" if not failures else f"{len(failures)} failed: {failures}")
sys.exit(1 if failures else 0)
