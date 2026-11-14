"""Trusted parent launcher for one confined, no-network, unprivileged tool capsule (docker).

Port of the proven private launchers: pinned image by ID (--pull=never), confinement inspected
BEFORE start, only declared read-only binds plus one disk-backed scratch bind, exact-cid
stop/kill, cgroup emptiness proof, bounded stdout handed to a parent-side sink. Guest data is
never printed: a failed run reports only fixed reason codes.
"""
import hashlib
import json
import os
from pathlib import Path
import re
import selectors
import shutil
import stat
import subprocess
import time

IMAGE = "sha256:1c0e402e0ff1da2732b03f93ff61e2c1e16a05f0a2091749abffc4d5360367d9"
USER = "1000:1000"
GUEST_PATH = re.compile(r"/(?:input|bootstrap)/[A-Za-z0-9_.-]{1,64}")
CHECK_ID = re.compile(r"[a-z0-9][a-z0-9.-]{0,63}")
SIZE = re.compile(r"[1-9][0-9]{0,4}[mg]")
ENV = ("PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/scratch/home", "TMPDIR=/scratch/tmp", "LC_ALL=C", "LANG=C",
       "LIBGUESTFS_BACKEND=direct", "LIBGUESTFS_BACKEND_SETTINGS=force_tcg",
       "LIBGUESTFS_CACHEDIR=/scratch/cache", "LIBGUESTFS_TMPDIR=/scratch/tmp")
TMP_OPTS = ("rw", "exec", "nosuid", "nodev", "mode=0700", "uid=1000", "gid=1000")


class CapsuleError(RuntimeError):
    """Fixed reason text only; never guest data or tool output."""


class Unresolved(CapsuleError):
    """The exact container could not be proven stopped: block every later action (scratch is kept)."""


def need(ok, reason):
    if not ok:
        raise CapsuleError(reason)


def size_bytes(text):
    need(type(text) is str and SIZE.fullmatch(text) is not None, "invalid size")
    return int(text[:-1]) << (30 if text[-1] == "g" else 20)


def sha256_file(path):
    digest = hashlib.sha256()
    with open(path, "rb") as f:
        for block in iter(lambda: f.read(1 << 20), b""):
            digest.update(block)
    return digest.hexdigest()


def pinned_input(path):
    """A bind source must be an absolute, regular, single-link, non-symlink file; returns its sha256."""
    path = Path(path)
    need(path.is_absolute() and ".." not in path.parts, "input path refused")
    st = path.lstat()
    need(stat.S_ISREG(st.st_mode) and st.st_nlink == 1 and "docker.sock" not in path.name, "input file refused")
    return sha256_file(path)


def spec(check, binds, scratch, argv, memory="8g", cpus=2, pids=256, wall=2400, cpu_seconds=2400,
         fsize=30 << 30, tmp="1g", image=IMAGE):
    """Validated capsule description. binds: {absolute host file: /input|/bootstrap/<name>} read-only."""
    need(CHECK_ID.fullmatch(check) is not None, "invalid check id")
    need(re.fullmatch(r"sha256:[0-9a-f]{64}", image) is not None, "image must be pinned by id")
    need(type(binds) is dict and 0 < len(binds) <= 16, "invalid binds")
    need(all(GUEST_PATH.fullmatch(guest) for guest in binds.values()), "invalid guest path")
    need(len(set(binds.values())) == len(binds), "duplicate guest path")
    scratch = Path(scratch)
    need(scratch.is_absolute() and ".." not in scratch.parts, "invalid scratch")
    need(type(argv) is list and argv and all(type(a) is str and "\x00" not in a for a in argv), "invalid argv")
    for value in (cpus, pids, wall, cpu_seconds, fsize):
        need(type(value) is int and value > 0, "invalid limit")
    size_bytes(memory), size_bytes(tmp)
    return {"check": check, "image": image, "binds": {str(Path(h)): g for h, g in binds.items()},
            "scratch": str(scratch), "argv": list(argv), "memory": memory, "cpus": cpus, "pids": pids,
            "wall": wall, "cpu_seconds": cpu_seconds, "fsize": fsize, "tmp": tmp}


def create_argv(s, name):
    argv = ["docker", "create", "--pull=never", "--name", name, "--log-driver=none", "--network=none",
            "--read-only", "--user=" + USER, "--cap-drop=ALL", "--security-opt=no-new-privileges",
            "--pids-limit=%d" % s["pids"], "--memory=" + s["memory"], "--memory-swap=" + s["memory"],
            "--cpus=%d" % s["cpus"], "--shm-size=128m", "--ulimit", "core=0:0", "--ulimit", "nofile=256:256",
            "--ulimit", "fsize=%d:%d" % (s["fsize"], s["fsize"]),
            "--ulimit", "cpu=%d:%d" % (s["cpu_seconds"], s["cpu_seconds"]),
            "--tmpfs", "/tmp:" + ",".join(TMP_OPTS[:4] + ("size=" + s["tmp"],) + TMP_OPTS[4:]),
            "--mount", "type=bind,src=" + s["scratch"] + ",dst=/scratch", "--entrypoint", "/usr/bin/env"]
    for host, guest in s["binds"].items():
        argv += ["--mount", "type=bind,src=" + host + ",dst=" + guest + ",readonly"]
    return argv + [s["image"], "-i", *ENV, *s["argv"]]


def check_confinement(info, s):
    """Pre-start proof from `docker inspect`: anything not exactly declared refuses the run."""
    hc = info["HostConfig"]
    need(info["Image"] == s["image"] and info["Config"]["User"] == USER, "image or user")
    need(info["Config"]["Entrypoint"] == ["/usr/bin/env"] and info["Config"]["Cmd"] == ["-i", *ENV, *s["argv"]],
         "command")
    need(hc["NetworkMode"] == "none" and hc["ReadonlyRootfs"] is True and not hc["Privileged"], "network or rootfs")
    need(hc["LogConfig"]["Type"] == "none" and hc["PidMode"] == "" and hc["IpcMode"] == "private", "namespaces")
    need(not hc.get("DeviceRequests") and not hc.get("Devices") and not hc.get("CapAdd"), "devices or caps")
    need(set(hc.get("CapDrop") or ()) == {"ALL"} and "no-new-privileges" in (hc.get("SecurityOpt") or ()), "caps")
    memory = size_bytes(s["memory"])
    need(hc["Memory"] == memory and hc["MemorySwap"] == memory and hc["NanoCpus"] == s["cpus"] * 10 ** 9, "memory")
    need(hc["PidsLimit"] == s["pids"] and hc["ShmSize"] == 128 << 20 and not hc.get("PortBindings"), "pids or ports")
    need(not hc.get("Binds") and not hc.get("VolumesFrom") and not hc.get("Links"), "extra mounts")
    need(set(hc.get("Tmpfs") or {}) == {"/tmp"}, "tmpfs")
    opts = hc["Tmpfs"]["/tmp"].split(",")
    need(all(item in opts for item in TMP_OPTS + ("size=" + s["tmp"],)), "tmpfs options")
    expected = {(h, g, False) for h, g in s["binds"].items()} | {(s["scratch"], "/scratch", True)}
    mounts = {(m["Source"], m["Destination"], m["RW"]) for m in info["Mounts"]}
    need(len(info["Mounts"]) == len(expected) and mounts == expected, "mounts")
    need(all(m["Type"] == "bind" for m in info["Mounts"]), "mount type")
    need(not any("docker.sock" in m["Source"] for m in info["Mounts"]), "docker socket")
    limits = {x["Name"]: (x["Soft"], x["Hard"]) for x in hc["Ulimits"]}
    need(limits == {"core": (0, 0), "nofile": (256, 256), "fsize": (s["fsize"],) * 2,
                    "cpu": (s["cpu_seconds"],) * 2}, "ulimits")


def _command(args, timeout=20, **kw):
    return subprocess.run(args, stdin=subprocess.DEVNULL, close_fds=True, timeout=timeout, **kw)


def _inspect(cid):
    return json.loads(_command(["docker", "inspect", cid], capture_output=True, text=True, check=True).stdout)[0]


def prepare_scratch(base, check, min_free):
    """Fresh private disk-backed scratch directory under base; the capsule user (uid 1000) owns it."""
    base = Path(base)
    need(base.is_absolute() and base.is_dir() and not base.is_symlink(), "scratch base refused")
    need(os.getuid() == 1000, "scratch owner must be the capsule uid 1000")
    vfs = os.statvfs(base)
    need(vfs.f_bavail * vfs.f_frsize >= min_free, "insufficient scratch space")
    path = base / (check + "-scratch")
    os.mkdir(path, 0o700)  # Must not exist yet.
    return path


def remove_scratch(path):
    """After the exact stop proof only: no capsule process can still write here."""
    path = Path(path)
    st = path.lstat()
    need(stat.S_ISDIR(st.st_mode) and st.st_uid == os.getuid(), "scratch identity")
    shutil.rmtree(path)


def run(s, sink, stderr_limit=65536):
    """Create, verify, start and fully stop one capsule. sink(chunk) -> reason|None consumes stdout.

    Returns the termination record; raises CapsuleError when the exact container cannot be proven stopped."""
    name = "proxcloud-vm-export-%s-%d" % (s["check"], os.getpid())
    cid = _command(create_argv(s, name), capture_output=True, text=True, check=True).stdout.strip()
    need(re.fullmatch(r"[0-9a-f]{64}", cid) is not None, "container id")
    proc = selector = state = None
    known, reason, cleanup_errors, stderr_bytes, code = "", None, [], 0, -1
    try:
        check_confinement(_inspect(cid), s)
        started = time.monotonic()
        proc = subprocess.Popen(["docker", "start", "--attach", cid], stdin=subprocess.DEVNULL,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, close_fds=True)
        selector = selectors.DefaultSelector()
        selector.register(proc.stdout, selectors.EVENT_READ, "stdout")
        selector.register(proc.stderr, selectors.EVENT_READ, "stderr")
        for _ in range(8):
            live = _inspect(cid)
            pid = live["State"]["Pid"]
            if pid > 0:
                try:
                    rows = [x.split(":", 2)[2] for x in (Path("/proc") / str(pid) / "cgroup").read_text().splitlines()
                            if x.startswith("0::")]
                except FileNotFoundError:
                    continue
                need(len(rows) == 1 and cid in rows[0] and ".." not in rows[0].split("/"), "cgroup identity")
                known = rows[0]
                break
            if live["State"]["Status"] in ("exited", "dead"):
                break
        if not known:
            reason = "exact_live_cgroup_not_observed"
        while selector.get_map() and reason is None:
            left = s["wall"] - (time.monotonic() - started)
            if left <= 0:
                reason = "parent_wall_limit"
                break
            for key, _ in selector.select(min(left, 1)):
                data = os.read(key.fileobj.fileno(), 65536)
                if not data:
                    selector.unregister(key.fileobj)
                    continue
                if key.data == "stderr":
                    stderr_bytes += len(data)  # Counted, never retained.
                    if stderr_bytes > stderr_limit:
                        reason = "stderr_limit"
                        break
                    continue
                reason = sink(data)
                if reason:
                    break
    except CapsuleError as exc:
        reason = reason or "confinement_" + str(exc).replace(" ", "_")
    except Exception as exc:  # noqa: BLE001 - class name only
        reason = reason or "parent_error_" + type(exc).__name__
    finally:
        if selector is not None:
            selector.close()
        try:
            if _command(["docker", "stop", "--time", "10", cid], stdout=subprocess.DEVNULL,
                        stderr=subprocess.DEVNULL).returncode:
                cleanup_errors.append("docker-stop-exit")
        except (subprocess.SubprocessError, OSError) as exc:
            cleanup_errors.append("docker-stop-" + type(exc).__name__)
        try:
            state = _inspect(cid)
        except (subprocess.SubprocessError, OSError, ValueError) as exc:
            cleanup_errors.append("post-stop-inspect-" + type(exc).__name__)
        if state is None or state["State"]["Running"]:
            try:
                _command(["docker", "kill", cid], capture_output=True, timeout=15)
                state = _inspect(cid)
            except (subprocess.SubprocessError, OSError, ValueError) as exc:
                cleanup_errors.append("kill-" + type(exc).__name__)
        if proc is not None:
            try:
                code = proc.wait(timeout=20)
            except subprocess.TimeoutExpired:
                proc.kill()
                code = proc.wait(timeout=10)
                cleanup_errors.append("attach-cli-killed")
            proc.stdout.close()
            proc.stderr.close()
    if state is None or state["Id"] != cid or state["State"]["Running"] or state["State"]["Pid"] != 0:
        raise Unresolved("UNRESOLVED exact capsule cleanup " + cid)
    empty = False
    if known:
        cg = Path("/sys/fs/cgroup") / known.lstrip("/")
        empty = not cg.exists() or dict(x.split() for x in (cg / "cgroup.events").read_text().splitlines()
                                        ).get("populated") == "0"
    if not empty:
        reason = reason or "cgroup_not_proven_empty"
    try:
        _command(["docker", "rm", cid], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)  # Exact id only.
    except (subprocess.SubprocessError, OSError) as exc:
        cleanup_errors.append("docker-rm-" + type(exc).__name__)
    return {"container_id": cid, "exit_code": state["State"]["ExitCode"], "attach_exit_code": code,
            "stop_reason": reason, "known_cgroup": known, "cgroup_empty_or_absent": empty,
            "cleanup_errors": cleanup_errors, "stderr_bytes_discarded": stderr_bytes}


class JsonSink:
    """Bounded stdout capture for a single-line JSON probe."""

    def __init__(self, limit=65536):
        self.limit, self.data = limit, bytearray()

    def __call__(self, chunk):
        self.data.extend(chunk)
        return "parent_output_limit" if len(self.data) > self.limit else None


FORBIDDEN = re.compile(r"10\.200\.|PRIVATE KEY|ssh-(?:rsa|ed25519) |\$[0-9a-z]{1,2}\$|(?:[0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}")
FRAME = (re.compile(r"[A-Za-z0-9_.<>-]{1,64}"), re.compile(r"[A-Za-z0-9_<>]{1,64}"))


def gate_json(term, data, validator, limit=60000):
    """Strict parent schema gate. Returns (clean, record); record is the parsed probe result only when it
    passed the validator, or one of the bounded message-free diagnostics (probe_error/layout_refused)."""
    exitcode, code = term["exit_code"], term["attach_exit_code"]
    refused = {"phase": "inspection_refused", "exit_code": exitcode, "raw_output_retained": False}
    if term["stop_reason"] or not term["known_cgroup"] or exitcode != code:
        return False, refused
    try:
        lines = bytes(data).decode("ascii").splitlines()
        line = lines[0] if len(lines) == 1 else ""
        parsed = json.loads(line) if line else None
    except (ValueError, UnicodeError):
        return False, refused
    if type(parsed) is not dict or len(line) > limit or FORBIDDEN.search(line):
        return False, refused
    if exitcode == 0:
        try:
            return (True, parsed) if validator(parsed) is True else (False, refused)
        except (KeyError, TypeError, ValueError):
            return False, refused
    if (exitcode == 4 and set(parsed) == {"phase", "layout"} and parsed["phase"] == "layout_refused"
            and len(line) <= 8192 and re.fullmatch(r'[{}\[\]",:0-9A-Za-z_/ .<>-]*', line)):
        return False, parsed
    frames = parsed.get("frames")
    if (exitcode == 3 and set(parsed) == {"phase", "error_class", "frames"} and parsed["phase"] == "probe_error"
            and re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]{0,60}", str(parsed["error_class"]))
            and type(frames) is list and len(frames) <= 12
            and all(type(f) is list and len(f) == 3 and FRAME[0].fullmatch(str(f[0])) and type(f[1]) is int
                    and FRAME[1].fullmatch(str(f[2])) for f in frames)):
        return False, parsed
    return False, refused


def write_new(path, data, mode=0o600):
    """Exclusive, no-follow create of a parent-owned result file."""
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, mode)
    with os.fdopen(fd, "wb") as f:
        f.write(data)
        f.flush()
        os.fsync(f.fileno())
