"""E2B SDK compatibility check against a live ProxCloud sandbox location (docs/e2b-compat.md).

Runs the stock E2B Python SDKs end to end:

    export E2B_API_URL=https://api.example.org/e2b   # the portal's API host + /e2b
    export E2B_API_KEY=e2b_...                        # a sandbox API key of a test project
    export E2B_OTHER_API_KEY=e2b_...                  # optional: a key of ANOTHER project (cross-tenant checks)
    pip install -r requirements.txt
    python compat.py [template] [code-interpreter template]   # defaults: base, code-interpreter-v1 ("-" skips it)

The sandbox domain comes back in the create answer. A site with a private CA: SSL_CERT_FILE=<bundle with that CA>.
Every sandbox it starts is killed at the end, also on failure. Exit code 0 = all checks passed.
"""

import os
import sys
import time
import urllib.error
import urllib.request

from e2b import Sandbox, SandboxQuery

TEMPLATE = sys.argv[1] if len(sys.argv) > 1 else "base"
CI_TEMPLATE = sys.argv[2] if len(sys.argv) > 2 else "code-interpreter-v1"
OTHER_KEY = os.environ.get("E2B_OTHER_API_KEY", "")
RUN = str(int(time.time()))
started = []


def check(name, cond, detail=""):
    print(("ok   " if cond else "FAIL ") + name + (f"  ({detail})" if detail and not cond else ""), flush=True)
    if not cond:
        raise SystemExit(1)


def blocked(sbx, host, port):
    r = sbx.commands.run(f"python3 -c \"import socket; socket.create_connection(('{host}', {port}), 5)\" && echo open || echo blocked")
    return r.stdout.strip().endswith("blocked"), r.stdout


def main():
    t0 = time.time()
    sbx = Sandbox.create(TEMPLATE, timeout=300, metadata={"compat-run": RUN}, envs={"COMPAT_VAR": "42"})
    started.append(sbx)
    check("create returns a ready sandbox", sbx.sandbox_id.startswith("sbx-"), sbx.sandbox_id)
    print(f"     create took {time.time() - t0:.1f}s", flush=True)

    r = sbx.commands.run("whoami && pwd && echo $COMPAT_VAR")
    check("commands.run as user in /home/user with envVars", r.stdout.split() == ["user", "/home/user", "42"], r.stdout)

    chunks = []
    sbx.commands.run("for i in 1 2 3; do echo tick $i; sleep 1; done", on_stdout=lambda d: chunks.append((time.time(), d)))
    spread = chunks[-1][0] - chunks[0][0] if len(chunks) > 1 else 0
    check("output streams while the command runs", len(chunks) >= 3 and spread >= 1.5, f"{len(chunks)} chunks over {spread:.1f}s")

    try:
        sbx.commands.run("echo err >&2; exit 3")
        check("non-zero exit raises", False)
    except Exception as e:  # CommandExitException
        check("non-zero exit raises with the exit code", getattr(e, "exit_code", None) == 3, repr(e))

    bg = sbx.commands.run("sleep 300", background=True)
    check("background command listed", any(p.pid == bg.pid for p in sbx.commands.list()))
    bg.kill()

    r = sbx.commands.run("python3 -c 'print(6*7)'")
    check("run code (python3 in the sandbox)", r.stdout.strip() == "42", r.stdout)

    sbx.files.write("/home/user/compat/hello.txt", "hello from the files API")
    check("files.write + read", sbx.files.read("/home/user/compat/hello.txt") == "hello from the files API")
    check("files.list", any(e.name == "hello.txt" for e in sbx.files.list("/home/user/compat")))
    sbx.files.remove("/home/user/compat/hello2.txt") if sbx.files.exists("/home/user/compat/hello2.txt") else None
    sbx.files.write("/home/user/compat/hello2.txt", b"\x00\x01binary")
    check("files binary round trip", bytes(sbx.files.read("/home/user/compat/hello2.txt", format="bytes")) == b"\x00\x01binary")

    sbx.commands.run("python3 -m http.server 8000 --directory /home/user/compat", background=True)
    time.sleep(2)
    url = f"https://{sbx.get_host(8000)}/hello.txt"
    body = urllib.request.urlopen(url, timeout=20).read().decode()
    check("preview URL serves a port of the sandbox", body == "hello from the files API", url)

    sbx.set_timeout(600)
    info = sbx.get_info()
    check("get_info + metadata", info.metadata.get("compat-run") == RUN, str(info.metadata))

    listed = Sandbox.list(query=SandboxQuery(metadata={"compat-run": RUN})).next_items()
    check("list filtered by metadata", [s.sandbox_id for s in listed] == [sbx.sandbox_id], str(listed))

    again = Sandbox.connect(sbx.sandbox_id)
    check("connect from a second client", again.commands.run("echo again").stdout.strip() == "again")

    closed = Sandbox.create(TEMPLATE, timeout=120, allow_internet_access=False)
    started.append(closed)
    ok, out = blocked(closed, "1.1.1.1", 443)
    check("allow_internet_access=False blocks egress", ok, out)
    ok, out = blocked(sbx, "1.1.1.1", 443)
    check("default sandbox reaches the internet", not ok, out)
    ok, out = blocked(sbx, "169.254.169.254", 80)
    check("metadata service stays unreachable", ok, out)

    if OTHER_KEY:
        other = {"api_key": OTHER_KEY}
        try:
            Sandbox.connect(sbx.sandbox_id, **other)
            check("another project's key cannot connect", False)
        except Exception as e:
            check("another project's key cannot connect", "404" in str(e) or "not found" in str(e).lower(), repr(e))
        theirs = Sandbox.list(query=SandboxQuery(metadata={"compat-run": RUN}), **other).next_items()
        check("another project's key lists nothing of ours", theirs == [], str(theirs))
        try:
            Sandbox.kill(sbx.sandbox_id, **other)
        except Exception:
            pass
        check("another project's key cannot kill", sbx.is_running())

    sbx.kill()
    closed.kill()
    time.sleep(3)
    check("kill removes it", not Sandbox.list(query=SandboxQuery(metadata={"compat-run": RUN})).next_items())
    try:
        urllib.request.urlopen(url, timeout=20)
        check("a killed sandbox's port is gone", False)
    except urllib.error.HTTPError as e:
        check("a killed sandbox's port is gone", e.code in (404, 502), str(e.code))

    # Last, on an otherwise idle project: Jupyter's start counts towards the create's 50 s.
    if CI_TEMPLATE != "-":
        from e2b_code_interpreter import Sandbox as CodeSandbox

        t0 = time.time()
        ci = CodeSandbox.create(CI_TEMPLATE, timeout=300)
        started.append(ci)
        print(f"     code interpreter create took {time.time() - t0:.1f}s", flush=True)
        ex = ci.run_code("x = 6 * 7\nx")
        check("run_code (code interpreter, Python kernel)", ex.text == "42", repr(ex))
        ex = ci.run_code("import sys; print('out'); print('err', file=sys.stderr)")
        check("run_code stdout and stderr", ex.logs.stdout == ["out\n"] and ex.logs.stderr == ["err\n"], repr(ex.logs))
        ex = ci.run_code("1/0")
        check("run_code reports an error", ex.error is not None and ex.error.name == "ZeroDivisionError", repr(ex.error))
        ci.kill()
    print("all checks passed", flush=True)


try:
    main()
finally:
    for s in started:
        try:
            s.kill()
        except Exception:
            pass
