#!/usr/bin/env python3
"""Build-only known-answer check of the installed kernels, not the E2B API server.

Run only inside the target image. Connection keys and kernel working files live in
private temporary directories; HOME is preserved so root's user kernelspec works.
"""
import os
from pathlib import Path
from queue import Empty
from subprocess import DEVNULL
import tempfile
import time

from jupyter_client import KernelManager

MARKER = "PROXCLOUD_KERNEL_OK"
KERNEL_SECONDS = 20
TOTAL_SECONDS = 75
CASES = (
    ("python3", "import sys; print('PROXCLOUD_KERNEL_OK'); print(sys.version)"),
    ("javascript", "console.log('PROXCLOUD_KERNEL_OK'); console.log(process.version);"),
    ("bash", "printf 'PROXCLOUD_KERNEL_OK\\n'"),
)


def remaining(deadline):
    seconds = deadline - time.monotonic()
    if seconds <= 0:
        raise TimeoutError()
    return seconds


def smoke(kernel_name, code, overall_deadline):
    deadline = min(overall_deadline, time.monotonic() + KERNEL_SECONDS)
    stage, ok = "prepare", False
    with tempfile.TemporaryDirectory(prefix="proxcloud-kernel-") as directory:
        # Explicit connection path and loopback binding: no persistent runtime keys.
        km = KernelManager(kernel_name=kernel_name, ip="127.0.0.1",
                           connection_file=str(Path(directory) / "kernel.json"))
        client = None
        try:
            stage = "start"
            remaining(deadline)
            env = dict(os.environ, JUPYTER_RUNTIME_DIR=directory, IPYTHONDIR=directory)
            km.start_kernel(cwd=directory, env=env, stdout=DEVNULL, stderr=DEVNULL)
            client = km.client()
            client.start_channels()
            stage = "ready"
            client.wait_for_ready(timeout=remaining(deadline))
            stage = "execute"
            msg_id = client.execute(code, store_history=False, allow_stdin=False,
                                    stop_on_error=True)
            output, idle, error = [], False, False
            while not idle:
                try:
                    message = client.get_iopub_msg(timeout=min(1, remaining(deadline)))
                except Empty:
                    continue
                if message.get("parent_header", {}).get("msg_id") != msg_id:
                    continue
                kind, content = message["msg_type"], message["content"]
                if kind == "stream":
                    output.append(content.get("text", ""))
                elif kind == "error":
                    error = True
                elif kind == "status" and content.get("execution_state") == "idle":
                    idle = True
            stage = "reply"
            while True:
                try:
                    reply = client.get_shell_msg(timeout=min(1, remaining(deadline)))
                except Empty:
                    continue
                if reply.get("parent_header", {}).get("msg_id") == msg_id:
                    break
            stage = "answer"
            if (reply["msg_type"] != "execute_reply" or reply["content"].get("status") != "ok"
                    or error or MARKER not in "".join(output).splitlines()):
                raise ValueError()
            ok = True
        except Exception as exc:
            # Never echo kernel output, exception messages, connection data or keys.
            print(f"{kernel_name}: {stage}: {type(exc).__name__}", flush=True)
        finally:
            try:
                if client is not None:
                    client.stop_channels()
            except Exception as exc:
                print(f"{kernel_name}: channels: {type(exc).__name__}", flush=True)
                ok = False
            finally:
                try:
                    km.shutdown_kernel(now=True)
                except Exception as exc:
                    print(f"{kernel_name}: shutdown: {type(exc).__name__}", flush=True)
                    ok = False
    if ok:
        print(f"{kernel_name}: known-answer OK", flush=True)
    return ok


def main():
    overall_deadline = time.monotonic() + TOTAL_SECONDS
    for kernel_name, code in CASES:
        try:
            if not smoke(kernel_name, code, overall_deadline):
                return 1
        except Exception as exc:
            print(f"{kernel_name}: prepare/cleanup: {type(exc).__name__}", flush=True)
            return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
