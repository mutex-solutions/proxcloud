"""Trusted parent launcher for one confined, no-network, unprivileged tool capsule (docker).

Port of the proven private launchers: the CI-built tool image by local ID (--pull=never; pull() is the
explicit earlier step), confinement inspected
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

# The tool image built and pushed by .github/workflows/vm-export-tools.yml (main at 83ebd33). pull() fetches
# it by this registry digest; a capsule itself never pulls and starts from the local ID only.
IMAGE_REFERENCE = ("ghcr.io/mutex-solutions/proxcloud-vm-export-tools"
                   "@sha256:d3ab36a5efbe47a285be14356bcbac92ced6f6eeb69555aa1e3338a5f37328b3")
# The local ID of exactly that image: its manifest digest under Docker's containerd image store, its
# config digest under the classic store. Nothing else starts a capsule.
IMAGE_IDS = ("sha256:d3ab36a5efbe47a285be14356bcbac92ced6f6eeb69555aa1e3338a5f37328b3",
             "sha256:df1770b213fb1bb85f72659797f6123ccce8941f0cf1959f999728552454f917")
GUEST_PATH = re.compile(r"/(?:input|bootstrap)/[A-Za-z0-9_.-]{1,64}")
CHECK_ID = re.compile(r"[a-z0-9][a-z0-9.-]{0,63}")
SIZE = re.compile(r"[1-9][0-9]{0,4}[mg]")
ENV = ("PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/scratch/home", "TMPDIR=/scratch/tmp", "LC_ALL=C", "LANG=C",
       "LIBGUESTFS_BACKEND=direct", "LIBGUESTFS_BACKEND_SETTINGS=force_tcg",
       "LIBGUESTFS_CACHEDIR=/scratch/cache", "LIBGUESTFS_TMPDIR=/scratch/tmp")
TMP_OPTS = ("rw", "exec", "nosuid", "nodev", "mode=0700")  # Plus uid=/gid= of the capsule user and size=.
SCRATCH_FLOOR = 1 << 30  # Stop the capsule before its scratch writes can fill the runner's filesystem.


class CapsuleError(RuntimeError):
    """Fixed reason text only; never guest data or tool output."""


class Unresolved(CapsuleError):
    """The exact container could not be proven stopped: block every later action (scratch is kept)."""


def need(ok, reason):
    if not ok:
        raise CapsuleError(reason)


def host_user():
    """(uid, gid) the capsule runs as: the invoking host user, because the disk-backed scratch is a bind
    mount that user must own. Never root, a system account or a system group (docker, disk, kvm), never a
    setuid/setgid context."""
    uid, gid = os.getuid(), os.getgid()
    need(uid == os.geteuid() and gid == os.getegid() and uid >= 1000 and gid >= 1000,
         "capsule user must be a non-root host uid and gid >= 1000 (real == effective)")
    return uid, gid


def tmp_opts(s):
    return TMP_OPTS + ("uid=%d" % host_user()[0], "gid=%d" % host_user()[1], "size=" + s["tmp"])


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
    """A bind source must be an absolute, regular, single-link file with no symlink anywhere in its path
    (docker reports and follows the unresolved source); returns its sha256."""
    path = Path(path)
    need(path.is_absolute() and ".." not in path.parts and os.path.realpath(path) == str(path), "input path refused")
    st = path.lstat()
    need(stat.S_ISREG(st.st_mode) and st.st_nlink == 1 and "docker.sock" not in path.name, "input file refused")
    return sha256_file(path)


def pin_all(binds):
    return {guest: pinned_input(host) for host, guest in binds.items()}


def load_receipt(path):
    """The copy receipt (vm_export.copy) of a guarded template disk copy."""
    try:
        receipt = json.loads(Path(path).read_bytes().decode("ascii"))
    except (ValueError, UnicodeError):
        receipt = None
    need(type(receipt) is dict and receipt.get("kind") == "TemplateDiskCopy"
         and receipt.get("template_unchanged_stopped") is True and type(receipt.get("template")) is dict
         and type(receipt.get("raw_bytes")) is int and re.fullmatch(r"[0-9a-f]{64}", str(receipt.get("raw_sha256"))),
         "copy receipt refused")
    return receipt


def bound_to_receipt(receipt_path, source, pin):
    """The bound source is the very file the guarded copy wrote: same sha256 and size."""
    receipt = load_receipt(receipt_path)
    need(pin == receipt["raw_sha256"] and os.lstat(source).st_size == receipt["raw_bytes"],
         "source does not match the copy receipt")
    return receipt


def spec(check, binds, scratch, argv, memory="8g", cpus=2, pids=256, wall=2400, cpu_seconds=2400,
         fsize=30 << 30, tmp="1g", image=None):
    """Validated capsule description. binds: {absolute host file: /input|/bootstrap/<name>} read-only.
    image: a local ID of the pinned tool image (default: the one present, see local_image)."""
    need(CHECK_ID.fullmatch(check) is not None, "invalid check id")
    image = local_image() if image is None else image
    need(image in IMAGE_IDS, "image must be the pinned tool image")
    need(type(binds) is dict and 0 < len(binds) <= 16, "invalid binds")
    need(all(GUEST_PATH.fullmatch(guest) for guest in binds.values()), "invalid guest path")
    need(len(set(binds.values())) == len(binds), "duplicate guest path")
    scratch = Path(scratch)
    need(scratch.is_absolute() and ".." not in scratch.parts, "invalid scratch")
    # A ',' or '=' in a host path would inject --mount options.
    need(not any(c in p for p in (*binds, str(scratch)) for c in ",=\n"), "invalid mount path")
    need(type(argv) is list and argv and all(type(a) is str and "\x00" not in a for a in argv), "invalid argv")
    for value in (cpus, pids, wall, cpu_seconds, fsize):
        need(type(value) is int and value > 0, "invalid limit")
    size_bytes(memory), size_bytes(tmp)
    return {"check": check, "image": image, "binds": {str(Path(h)): g for h, g in binds.items()},
            "scratch": str(scratch), "argv": list(argv), "memory": memory, "cpus": cpus, "pids": pids,
            "wall": wall, "cpu_seconds": cpu_seconds, "fsize": fsize, "tmp": tmp}


def create_argv(s, name):
    argv = ["docker", "create", "--pull=never", "--name", name, "--log-driver=none", "--network=none",
            "--read-only", "--user=%d:%d" % host_user(), "--cap-drop=ALL", "--security-opt=no-new-privileges",
            "--pids-limit=%d" % s["pids"], "--memory=" + s["memory"], "--memory-swap=" + s["memory"],
            "--cpus=%d" % s["cpus"], "--shm-size=128m", "--ulimit", "core=0:0", "--ulimit", "nofile=256:256",
            "--ulimit", "fsize=%d:%d" % (s["fsize"], s["fsize"]),
            "--ulimit", "cpu=%d:%d" % (s["cpu_seconds"], s["cpu_seconds"]),
            "--tmpfs", "/tmp:" + ",".join(tmp_opts(s)),
            "--mount", "type=bind,src=" + s["scratch"] + ",dst=/scratch", "--entrypoint", "/usr/bin/env"]
    for host, guest in s["binds"].items():
        argv += ["--mount", "type=bind,src=" + host + ",dst=" + guest + ",readonly"]
    return argv + [s["image"], "-i", *ENV, *s["argv"]]


def check_confinement(info, s, image_env):
    """Pre-start proof from `docker inspect`: anything not exactly declared refuses the run.
    image_env is the pinned image's own Config.Env (the container may add nothing to it)."""
    hc = info["HostConfig"]
    need(info["Image"] == s["image"] and info["Config"]["User"] == "%d:%d" % host_user(), "image or user")
    need(info["Config"]["Env"] == image_env, "environment")
    need(info["Config"]["Entrypoint"] == ["/usr/bin/env"] and info["Config"]["Cmd"] == ["-i", *ENV, *s["argv"]],
         "command")
    need(hc["NetworkMode"] == "none" and hc["ReadonlyRootfs"] is True and not hc["Privileged"], "network or rootfs")
    need(hc["LogConfig"]["Type"] == "none" and hc["PidMode"] == "" and hc["IpcMode"] == "private", "namespaces")
    need(not hc.get("DeviceRequests") and not hc.get("Devices") and not hc.get("CapAdd"), "devices or caps")
    need(hc.get("CapDrop") == ["ALL"] and hc.get("SecurityOpt") == ["no-new-privileges"], "caps")
    need(hc.get("Runtime") == "runc" and hc.get("CgroupnsMode") == "private" and hc.get("UTSMode") == ""
         and hc.get("UsernsMode") == "" and not hc.get("GroupAdd") and not hc.get("Sysctls") and not hc.get("Init")
         and hc.get("MaskedPaths") and hc.get("ReadonlyPaths"), "runtime or namespaces")
    memory = size_bytes(s["memory"])
    need(hc["Memory"] == memory and hc["MemorySwap"] == memory and hc["NanoCpus"] == s["cpus"] * 10 ** 9, "memory")
    need(hc["PidsLimit"] == s["pids"] and hc["ShmSize"] == 128 << 20 and not hc.get("PortBindings"), "pids or ports")
    need(not hc.get("Binds") and not hc.get("VolumesFrom") and not hc.get("Links"), "extra mounts")
    need(set(hc.get("Tmpfs") or {}) == {"/tmp"}, "tmpfs")
    opts = hc["Tmpfs"]["/tmp"].split(",")
    need(sorted(opts) == sorted(tmp_opts(s)), "tmpfs options")
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


def local_image():
    """The local ID of the pinned tool image; refuses unless it is present under one of its two IDs."""
    done = _command(["docker", "image", "inspect", "--format", "{{.Id}}", IMAGE_REFERENCE], capture_output=True, text=True)
    image = done.stdout.strip() if done.returncode == 0 else ""
    need(image in IMAGE_IDS, "pinned tool image not present")
    return image


def pull(config_dir):
    """The explicit step before any capsule: fetch the pinned tool image by its registry digest. config_dir
    is an empty directory used as the Docker client configuration, so no stored registry credential is
    sent: the image is the one anyone can pull."""
    need(os.path.isdir(config_dir) and not os.listdir(config_dir), "docker config dir must be empty")
    done = _command(["docker", "pull", "--quiet", IMAGE_REFERENCE], timeout=1800, stdout=subprocess.DEVNULL,
                    stderr=subprocess.DEVNULL, env=dict(os.environ, DOCKER_CONFIG=str(config_dir)))
    need(done.returncode == 0, "tool image pull failed")
    return local_image()


def fs_type(path):
    """Filesystem type of the mount holding path, from /proc/self/mountinfo (last, longest match wins)."""
    path, best, kind = os.path.realpath(path), "", None
    for line in Path("/proc/self/mountinfo").read_text().splitlines():
        left, _, right = line.partition(" - ")
        mount = left.split()[4].encode().decode("unicode_escape")  # Octal escapes (a space is "\040").
        if (path == mount or path.startswith(mount.rstrip("/") + "/")) and len(mount) >= len(best):
            best, kind = mount, right.split()[0]
    return kind


def prepare_scratch(base, check, min_free):
    """Fresh private disk-backed scratch directory under base, owned by the capsule user (host_user)."""
    need(CHECK_ID.fullmatch(check) is not None, "invalid check id")  # Before any path is built from it.
    base = Path(base)
    need(base.is_absolute() and base.is_dir() and not base.is_symlink(), "scratch base refused")
    need(fs_type(base) not in (None, "tmpfs", "ramfs"), "scratch must be disk-backed, not tmpfs")
    host_user()
    vfs = os.statvfs(base)
    need(vfs.f_bavail * vfs.f_frsize >= min_free, "insufficient scratch space")
    path = base / (check + "-scratch")
    need(path.parent == base, "scratch path")
    os.mkdir(path, 0o700)  # Must not exist yet.
    return path


def remove_scratch(path):
    """After the exact stop and cgroup-empty proof only: no capsule process can still write here."""
    path = Path(path)
    st = path.lstat()
    need(stat.S_ISDIR(st.st_mode) and st.st_uid == os.getuid(), "scratch identity")

    def unlock(directory):  # A capsule-written chmod-000 directory must not keep guest data behind.
        os.chmod(directory, 0o700)
        with os.scandir(directory) as entries:
            for entry in entries:
                if entry.is_dir(follow_symlinks=False):
                    unlock(entry.path)
    unlock(path)
    shutil.rmtree(path)


def _cgroup_of(pid):
    """cgroup v2 path rows of a live pid (raises FileNotFoundError once it is gone)."""
    return [x.split(":", 2)[2] for x in (Path("/proc") / str(pid) / "cgroup").read_text().splitlines()
            if x.startswith("0::")]


def _cgroup_empty(known):
    cg = Path("/sys/fs/cgroup") / known.lstrip("/")
    return not cg.exists() or dict(x.split() for x in (cg / "cgroup.events").read_text().splitlines()
                                   ).get("populated") == "0"


def run(s, sink, pins, stderr_limit=65536):
    """Create, verify, start and fully stop one capsule. sink(chunk) -> reason|None consumes stdout.
    pins = pin_all(s["binds"]) taken by the caller; the inputs are re-pinned after the stop.

    Returns the termination record; raises Unresolved when the exact container cannot be proven stopped
    or its known cgroup is still populated (the caller then keeps the scratch)."""
    name = "proxcloud-vm-export-%s-%d" % (s["check"], os.getpid())
    cid = _command(create_argv(s, name), capture_output=True, text=True, check=True).stdout.strip()
    need(re.fullmatch(r"[0-9a-f]{64}", cid) is not None, "container id")
    proc = selector = state = None
    known, reason, cleanup_errors, stderr_bytes, code = "", None, [], 0, -1
    try:
        check_confinement(_inspect(cid), s, _inspect(s["image"])["Config"]["Env"])
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
                    rows = _cgroup_of(pid)
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
            vfs = os.statvfs(s["scratch"])
            if vfs.f_bavail * vfs.f_frsize < SCRATCH_FLOOR:
                reason = "scratch_space_floor"
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
    empty = bool(known) and _cgroup_empty(known)
    if known and not empty:
        raise Unresolved("UNRESOLVED capsule cgroup still populated " + cid)
    try:  # (An unknown cgroup already carries a stop reason; cgroup_empty_or_absent stays False.)
        if pin_all(s["binds"]) != pins:
            reason = reason or "input_changed_during_run"
    except (CapsuleError, OSError):
        reason = reason or "input_changed_during_run"
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
    if (term["stop_reason"] or not term["known_cgroup"] or exitcode != code or term["cleanup_errors"]
            or not term["cgroup_empty_or_absent"]):
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
