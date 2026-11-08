"""Pause, resume, fork and snapshots with the stock E2B SDK against a live location (docs/managed-sandbox.md §8).

    export E2B_API_URL=https://api.example.org/e2b   E2B_API_KEY=e2b_...   E2B_OTHER_API_KEY=e2b_... (another project)
    export E2B_AUTOPAUSE_URL=http://<mgmt>:<mgmt port>/debug/run-sandbox-autopause   # optional: trigger the job
    python pause.py [steps]      steps: all (default) or a comma list of pause,fork,snapshot,autopause,wake,other

Strict on purpose: a pause must answer within the SDK's request timeout, the sandbox must stop once its image is ready,
and a connect only counts as a resume if the sandbox was paused right before it; every resumed, forked or snapshot
sandbox must hold the file written before. Every sandbox made here is killed at the end. Exit code 0 = all passed.

Fork and create from a snapshot answer 503 (ServiceBusyException) while the snapshot image is still assembled (more than
60 s on a slow site); like a client should, the drill retries them (BUSY_TRIES, with backoff), each try still within
the SDK's timeout. autopause pauses at the timeout and comes back with connect; wake is autoResume, woken by an HTTP
hit through the proxy (the SDK refuses auto_resume with mode "filesystem", so it leaves the mode out).
"""
import os
import sys
import time

import httpx
from e2b import Sandbox, ServiceBusyException

STEPS = set((sys.argv[1] if len(sys.argv) > 1 else "all").split(","))
on = lambda s: "all" in STEPS or s in STEPS
OTHER = os.environ.get("E2B_OTHER_API_KEY", "")
AUTOPAUSE_URL = os.environ.get("E2B_AUTOPAUSE_URL", "")
SDK_TIMEOUT = 120  # e2b 2.63: request_timeout 60 s, applied by pyqwest as one deadline of read + write
BUSY_TRIES = 5  # tries of a call answered 503 (snapshot still assembled); waits 5, 10, 20, 40 s between them
failures, made, snaps = [], [], []
MARK = "/home/user/sb7-mark.txt"


def check(name, cond, detail=""):
    print(("ok   " if cond else "FAIL ") + name + (f"  ({detail})" if detail and not cond else ""), flush=True)
    if not cond:
        failures.append(name)


def t(label, fn):
    t0 = time.time()
    try:
        r = fn()
        print(f"     {label}: {time.time() - t0:.1f}s", flush=True)
        return r
    except Exception as e:  # noqa: BLE001 - report and go on
        print(f"     {label}: {type(e).__name__}: {str(e)[:300]} after {time.time() - t0:.1f}s", flush=True)
        return e


def timed(label, fn):
    """t() plus whether it answered within the SDK's request timeout (a server that answers later is cut off)."""
    t0 = time.time()
    r = t(label, fn)
    check(f"{label} answers within {SDK_TIMEOUT}s", time.time() - t0 < SDK_TIMEOUT - 5, f"{time.time() - t0:.0f}s")
    return r


def busy_retry(label, fn):
    """timed(fn), tried again on ServiceBusyException (503: the snapshot is still being assembled; retry)."""
    for i in range(BUSY_TRIES):
        r = timed(f"{label} (try {i + 1})" if i else label, fn)
        if not isinstance(r, ServiceBusyException):
            return r
        if i + 1 < BUSY_TRIES:
            time.sleep(5 * 2**i)
    return r


def has_mark(sb, want):
    try:
        return sb.files.read(MARK).strip() == want
    except Exception as e:  # noqa: BLE001
        print("     read:", type(e).__name__, str(e)[:200])
        return False


def refused(fn):
    try:
        fn()
        return False
    except Exception as e:  # noqa: BLE001
        print("     refused:", type(e).__name__, str(e)[:160])
        return True


def state(sid):
    try:
        return str(getattr(Sandbox.get_info(sid), "state", "")).lower()
    except Exception as e:  # noqa: BLE001
        return f"error {type(e).__name__}: {str(e)[:120]}"


def wait_stopped(sb, limit=900):
    """True once envd stops answering (502: the paused sandbox is deleted); it must list as paused all along."""
    deadline = time.time() + limit
    while time.time() < deadline:
        st = state(sb.sandbox_id)
        if "paused" not in st:
            return f"state {st!r} while the pause completes"
        try:
            if not sb.is_running(request_timeout=10):
                return True
        except Exception as e:  # noqa: BLE001 - only is_running's False (a 502) counts as stopped
            print("     is_running:", type(e).__name__, str(e)[:120])
        time.sleep(5)
    return f"still running after {limit}s"


try:
    sb = t("create base", lambda: Sandbox.create("base", timeout=600))
    check("create", isinstance(sb, Sandbox), str(sb))
    if not isinstance(sb, Sandbox):
        raise SystemExit(1)
    made.append(sb.sandbox_id)
    want = f"sb7 {sb.sandbox_id} {time.time():.0f}"
    sb.files.write(MARK, want)
    check("write a file", has_mark(sb, want))

    if on("pause"):
        r = timed("pause", lambda: sb.pause(mode="filesystem"))
        check("pause", r is True, str(r))
        check("state paused after the pause", "paused" in state(sb.sandbox_id), state(sb.sandbox_id))
        if on("other"):
            check("other project cannot connect", refused(lambda: Sandbox.connect(sb.sandbox_id, api_key=OTHER)))
        # The pause answered once the capture was stored; the sandbox runs until its image is ready, then stops.
        stopped = t("pause completes", lambda: wait_stopped(sb))
        check("the paused sandbox stops once its image is ready", stopped is True, str(stopped))
        st = state(sb.sandbox_id)
        check("paused right before connect", "paused" in st, st)
        if "paused" in st:
            sb2 = timed("connect (resume)", lambda: Sandbox.connect(sb.sandbox_id, timeout=600))
            check("connect resumes the same sandbox", isinstance(sb2, Sandbox) and sb2.sandbox_id == sb.sandbox_id, str(sb2))
            check("running after the resume", "running" in state(sb.sandbox_id), state(sb.sandbox_id))
            check("the resumed sandbox has the file", isinstance(sb2, Sandbox) and has_mark(sb2, want))
            if isinstance(sb2, Sandbox):
                sb = sb2
        else:
            print("     connect skipped: a connect to a sandbox that never paused proves no resume", flush=True)

    if on("fork"):
        if on("other"):
            check("other project cannot fork", refused(lambda: Sandbox.fork(sb.sandbox_id, api_key=OTHER)))
        forks = busy_retry("fork", lambda: sb.fork(count=1, timeout=600))
        f = forks[0] if isinstance(forks, list) and forks else forks
        if isinstance(f, Sandbox):
            made.append(f.sandbox_id)
        check("fork is a new sandbox", isinstance(f, Sandbox) and f.sandbox_id != sb.sandbox_id, str(f))
        check("the fork has the file", isinstance(f, Sandbox) and has_mark(f, want))
        check("the original runs on", has_mark(sb, want))
        if isinstance(f, Sandbox):
            t("kill fork", f.kill)

    if on("snapshot"):
        s = timed("named snapshot", lambda: sb.create_snapshot(name="sb7-drill", mode="filesystem"))
        sid = getattr(s, "snapshot_id", None)
        if sid:
            snaps.append(sid)
        check("named snapshot", bool(sid), str(s))
        if sid:
            c = busy_retry("create from snapshot", lambda: Sandbox.create(sid, timeout=600))
            if isinstance(c, Sandbox):
                made.append(c.sandbox_id)
            check("the snapshot sandbox has the file", isinstance(c, Sandbox) and has_mark(c, want), str(c))
            if isinstance(c, Sandbox):
                t("kill snapshot sandbox", c.kill)
            if on("other"):
                check("other project cannot create from snapshot", refused(lambda: Sandbox.create(sid, api_key=OTHER, timeout=120)))

    t("kill first", sb.kill)

    def auto_paused(a):
        """Write the mark, wait for the job to pause the sandbox at its timeout and for the pause to complete."""
        made.append(a.sandbox_id)
        a.files.write(MARK, want)
        print("     waiting for the auto-pause (job sandboxAutoPause, every minute)", flush=True)
        deadline, st = time.time() + 900, ""
        while time.time() < deadline:
            time.sleep(20)
            if AUTOPAUSE_URL:
                httpx.post(AUTOPAUSE_URL, timeout=900)
            if "paused" in (st := state(a.sandbox_id)):
                break
        check("auto-paused", "paused" in st, st)
        stopped = t("auto-pause completes", lambda: wait_stopped(a))
        check("the auto-paused sandbox stops once its image is ready", stopped is True, str(stopped))
        return stopped is True

    if on("autopause"):
        # mode "filesystem": the SDK refuses auto_resume with it, so this one comes back with connect only.
        a = t("create autoPause (filesystem) 60s", lambda: Sandbox.create(
            "base", timeout=60, lifecycle={"on_timeout": {"action": "pause", "mode": "filesystem"}}))
        check("create with autoPause", isinstance(a, Sandbox), str(a))
        if isinstance(a, Sandbox) and auto_paused(a):
            st = state(a.sandbox_id)
            check("paused right before connect", "paused" in st, st)
            a2 = timed("connect (resume)", lambda: Sandbox.connect(a.sandbox_id, timeout=300))
            check("connect resumes the auto-paused sandbox", isinstance(a2, Sandbox) and a2.sandbox_id == a.sandbox_id, str(a2))
            check("the auto-paused sandbox has the file", isinstance(a2, Sandbox) and has_mark(a2, want))

    if on("wake"):
        # autoResume: no mode, so the SDK sends no autoPauseMemory (ProxCloud pauses the filesystem) and accepts
        # auto_resume. The wake goes through sandbox-proxy (docs/managed-sandbox.md §8.3).
        a = t("create autoPause+autoResume 60s", lambda: Sandbox.create(
            "base", timeout=60, lifecycle={"on_timeout": "pause", "auto_resume": True}))
        check("create with autoResume", isinstance(a, Sandbox), str(a))
        if isinstance(a, Sandbox) and auto_paused(a):
            url = f"https://{a.get_host(8000)}/"
            r = t("HTTP hit through the proxy", lambda: httpx.get(url, timeout=120,
                  headers={"e2b-traffic-access-token": a.traffic_access_token or ""}))
            print("     hit:", getattr(r, "status_code", r))
            time.sleep(5)
            st = state(a.sandbox_id)
            check("woken by the hit (autoResume)", "running" in st, st)
            a2 = timed("connect", lambda: Sandbox.connect(a.sandbox_id, timeout=300))
            check("the woken sandbox has the file", isinstance(a2, Sandbox) and has_mark(a2, want))
finally:
    for i in made:
        t(f"kill {i}", lambda i=i: Sandbox.kill(i))
    for s in snaps:
        t(f"delete snapshot {s}", lambda s=s: Sandbox.delete_snapshot(s))
    print(f"{len(failures)} failed: {failures}" if failures else "all pause checks passed")
sys.exit(1 if failures else 0)
