"""Cold-boot acceptance of a sealed sanitized disk through the PVE API with a least-privilege token.

    cd images && PVE_URL=... PVE_TOKEN_ID=... PVE_TOKEN_SECRET=... python3 -m vm_export.coldboot run \\
        --sanitize-run DIR/sanitize-run.json --variant kubernetes --os ubuntu-2604 --kubernetes-version 1.37.2 \\
        --node NODE --storage pc-ci-images --bridge vmbr0 --vlan 242 \\
        --test-address 192.0.2.50/24 --test-address 192.0.2.51/24 --gateway 192.0.2.1 \\
        --nameserver 1.1.1.1 --nameserver 8.8.8.8 --state-dir DIR
    cd images && PVE_URL=... PVE_TOKEN_ID=... PVE_TOKEN_SECRET=... python3 -m vm_export.coldboot cleanup --state-dir DIR

No root, no pvesh, no SSH to the hypervisor and no guest-exec: the token reaches VMs only through the
pool ACL, and the guest checks run over SSH from this host to each clone's test address.

run: snapshot every guest the token can see (the protected set), upload the sealed disk as one exact new
import volume, create two empty VMs in the pool (lowest free reserved VMIDs), configure, boot with fresh
cloud-init and one ephemeral key, check both guests and that their identities differ, then clean up and
compare the protected set. Every step is recorded in <state-dir>/coldboot-state.json BEFORE it changes
anything, and cleanup touches only what that file names: a VM must also carry the exact name and the
pc-artifact-<sha12> tag and be in the pool (re-read before every mutation). Nothing is ever adopted or
deleted by prefix. `cleanup` repeats that guarded cleanup from the state file of an interrupted run.

Writes <state-dir>/coldboot-result.json with fixed reason codes only (never guest data). Exit 0 only when
every check passed on both clones, the identities differ, cleanup completed and the protected set is
unchanged.

TLS: the connection is build.PVE's, which does not verify the PVE certificate today (build.py uses an
unverified context); the result records pve_tls_verified so that this is visible.
"""
import argparse
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import signal
import ssl
import stat
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

from vm_export import capsule, coldboot_guest
import vm_sanitize

POOL = "pc-templates"
OSES = {"ubuntu-2404": "24.04", "ubuntu-2604": "26.04"}  # cloud-init default user `ubuntu`.
STATE, RESULT, KEY = "coldboot-state.json", "coldboot-result.json", "id_ed25519"
KEY_COMMENT = "proxcloud-export-coldboot"
NAME = "pc-export-test-%s-%s"  # sha256[:8], clone suffix
TAGS = ("proxcloud", "pc-artifact-test")
SUFFIXES = "ab"
IDENT = re.compile(r"[A-Za-z][A-Za-z0-9-]{0,62}")
DISK_KEY = re.compile(r"(?:ide|sata|scsi|virtio|unused|efidisk|tpmstate)[0-9]+")
HEX64 = re.compile(r"[0-9a-f]{64}")
STORAGE_TASK = r"[a-z]+:[^:]*"
GUEST_KEYS = {"checks", "machine_id_sha256", "host_key_sha256", "snapd_present", "snapd_serial_sha256",
              "snapd_device_key_sha256", "snapd_changes_error_lines", "cloud_init_recoverable_warnings"}
TASK_DEADLINE, SSH_DEADLINE, GUEST_DEADLINE = 1200, 600, 1500


class Refused(Exception):
    """A fixed reason code: never guest data, tool output or a PVE response body."""


class ApiError(Exception):
    def __init__(self, status):
        super().__init__("pve_http_%d" % status)
        self.status = status


def need(ok, code):
    if not ok:
        raise Refused(code)


class Client:
    """build.PVE's connection (PVE_URL, token header, TLS context) plus what build.py lacks: errors that
    carry the HTTP status and no response body, and a streaming upload."""
    UPLOAD_TIMEOUT = 600  # Per socket operation, not for the whole transfer.

    def __init__(self, pve):
        self.base, self.auth, self.ctx = pve.base, pve.auth, pve.ctx
        self.tls_verified = pve.ctx.verify_mode == ssl.CERT_REQUIRED

    def _open(self, request, timeout=120):
        try:
            with urllib.request.urlopen(request, context=self.ctx, timeout=timeout) as response:
                return json.load(response).get("data")
        except urllib.error.HTTPError as exc:
            exc.close()
            raise ApiError(exc.code) from None

    def call(self, method, path, **params):
        query = urllib.parse.urlencode(params) if params else None
        if query and method in ("GET", "DELETE"):
            path, query = path + "?" + query, None
        return self._open(urllib.request.Request(self.base + path, data=query.encode() if query else None,
                                                 method=method, headers={"Authorization": self.auth}))

    def upload(self, node, storage, name, path, sha256, size):
        """Streamed multipart POST of one `import` volume. PVE verifies the sha256 itself; the bytes sent
        are also hashed here, and a file that changed since it was sealed ends the request short."""
        boundary = "proxcloud" + os.urandom(16).hex()

        def field(key, value):
            return '--%s\r\nContent-Disposition: form-data; name="%s"\r\n\r\n%s\r\n' % (boundary, key, value)
        # The plain fields precede the file part: PVE reads them from the headers before the file data.
        head = (field("content", "import") + field("checksum-algorithm", "sha256") + field("checksum", sha256)
                + '--%s\r\nContent-Disposition: form-data; name="filename"; filename="%s"\r\n'
                  "Content-Type: application/octet-stream\r\n\r\n" % (boundary, name)).encode("ascii")
        tail = ("\r\n--%s--\r\n" % boundary).encode("ascii")

        def body():
            yield head
            sent, digest = 0, hashlib.sha256()
            with os.fdopen(os.open(path, os.O_RDONLY | os.O_NOFOLLOW), "rb") as disk:
                for block in iter(lambda: disk.read(1 << 20), b""):
                    sent += len(block)
                    need(sent <= size, "upload_source_changed")
                    digest.update(block)
                    yield block
            need(sent == size and digest.hexdigest() == sha256, "upload_source_changed")
            yield tail
        return self._open(urllib.request.Request(
            "%s/nodes/%s/storage/%s/upload" % (self.base, node, storage), data=body(), method="POST",
            headers={"Authorization": self.auth, "Content-Type": "multipart/form-data; boundary=" + boundary,
                     "Content-Length": str(len(head) + size + len(tail))}), timeout=self.UPLOAD_TIMEOUT)


def wait(api, node, upid, task, allow_sync=False):
    """Bounded wait for exactly the task this job started: its node, kind and id (`task` = "kind:id" regex)."""
    if upid in (None, ""):
        need(allow_sync, "task_id_missing")
        return
    need(type(upid) is str and re.fullmatch(r"UPID:%s:[0-9A-Fa-f]+:[0-9A-Fa-f]+:[0-9A-Fa-f]+:%s:[^:]+:"
                                            % (re.escape(node), task), upid) is not None, "task_id_unexpected")
    end = time.monotonic() + TASK_DEADLINE
    while time.monotonic() < end:
        state = api.call("GET", "/nodes/%s/tasks/%s/status" % (node, urllib.parse.quote(upid, safe="")))
        if state.get("status") == "stopped":
            need(state.get("exitstatus") == "OK", "task_failed")
            return
        time.sleep(2)
    raise Refused("task_deadline")  # No blind retry.


def protected(api):
    """Every guest the token can see: ({vmid: [node, type, status, config digest]}, {vmid: config})."""
    snapshot, configs = {}, {}
    for row in api.call("GET", "/cluster/resources", type="vm"):
        vmid, node, kind = row.get("vmid"), str(row.get("node")), row.get("type")
        need(type(vmid) is int and kind in ("qemu", "lxc") and IDENT.fullmatch(node) is not None,
             "protected_set_unreadable")
        config = api.call("GET", "/nodes/%s/%s/%d/config" % (node, kind, vmid))
        need(re.fullmatch(r"[0-9a-f]{40}", str(config.get("digest"))) is not None, "protected_set_unreadable")
        snapshot[str(vmid)] = [node, kind, row.get("status"), config["digest"]]
        configs[vmid] = config
    return snapshot, configs


def address_in_use(configs, addresses):
    """A test address already named by any visible guest's ipconfigN/netN."""
    wanted = {a.split("/")[0] for a in addresses}
    return any(part.split("=", 1)[1].split("/")[0] in wanted
               for config in configs.values() for key, value in config.items()
               if re.fullmatch(r"(?:ipconfig|net)[0-9]+", key)
               for part in str(value).split(",") if part.startswith(("ip=", "ip6=")))


def exists(api, vmid):
    """Whether ANY guest holds this VMID, including one outside the pool that the token cannot read
    (its config answers 403 exactly like a missing one; /cluster/nextid refuses a taken id with 400)."""
    try:
        api.call("GET", "/cluster/nextid", vmid=vmid)
        return False
    except ApiError as exc:
        need(exc.status == 400, "vmid_probe_failed")
        return True


def free_vmids(api, cfg):
    return (vmid for vmid in range(cfg["vmid_low"], cfg["vmid_high"] + 1) if not exists(api, vmid))


def in_pool(api, cfg, vmid):
    members = api.call("GET", "/pools/" + cfg["pool"]).get("members", [])
    return any(m.get("vmid") == vmid and m.get("node") == cfg["node"] and m.get("type") == "qemu" for m in members)


def artifact_tag(cfg):
    return "pc-artifact-" + cfg["sha256"][:12]


def owned(api, cfg, clone, expected=None):
    """The ownership guard, re-read before every mutation: this run's VM only while it is in the pool AND
    has the exact name AND the artifact tag, is no template, unlocked, SeaBIOS without EFI/TPM state, and
    every disk is its own volume. expected=None accepts running or stopped. Returns (config, status)."""
    vmid = clone["vmid"]
    path = "/nodes/%s/qemu/%d" % (cfg["node"], vmid)
    need(in_pool(api, cfg, vmid), "not_owned_pool")
    config, status = api.call("GET", path + "/config"), api.call("GET", path + "/status/current").get("status")
    need(config.get("name") == clone["name"]
         and artifact_tag(cfg) in str(config.get("tags", "")).split(";"), "not_owned_identity")
    need(not config.get("template") and not config.get("lock"), "not_owned_template_or_lock")
    need(status == expected if expected else status in ("running", "stopped"), "unexpected_status")
    need("efidisk0" not in config and "tpmstate0" not in config and config.get("bios", "seabios") == "seabios",
         "unexpected_firmware")
    own = re.compile(r"%s:%d/vm-%d-(?:disk-[0-9]+|cloudinit)\.(?:qcow2|raw)" % (re.escape(cfg["storage"]), vmid, vmid))
    need(all(own.fullmatch(str(config[key]).split(",")[0]) for key in config if DISK_KEY.fullmatch(key)),
         "unexpected_disk")
    return config, status


def save(sdir, state):
    """Atomic rewrite of the run record (written before each step it describes)."""
    tmp = sdir / (STATE + ".tmp")
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "wb") as f:
        f.write((json.dumps(state, indent=2, sort_keys=True) + "\n").encode("ascii"))
        f.flush()
        os.fsync(f.fileno())
    os.replace(tmp, sdir / STATE)


def load_state(sdir):
    """The record of THIS run, strictly re-validated: cleanup acts on nothing it does not name."""
    try:
        state = json.loads((sdir / STATE).read_bytes().decode("ascii"))
        cfg, clones = state["cfg"], state["clones"]
        ok = (all(IDENT.fullmatch(str(cfg[k])) for k in ("node", "storage", "pool")) and HEX64.fullmatch(cfg["sha256"])
              and cfg["volid"] == "%s:import/pc-export-%s.qcow2" % (cfg["storage"], cfg["sha256"][:12])
              and type(cfg["vmid_low"]) is int and type(cfg["vmid_high"]) is int
              and type(state["import_uploaded"]) is bool and type(state["protected"]) is dict
              and type(clones) is list and len(clones) <= len(SUFFIXES)
              and all(type(c["vmid"]) is int and cfg["vmid_low"] <= c["vmid"] <= cfg["vmid_high"]
                      and c["suffix"] in SUFFIXES and c["name"] == NAME % (cfg["sha256"][:8], c["suffix"])
                      for c in clones))
    except (OSError, ValueError, KeyError, TypeError):
        ok = False
    need(ok, "state_refused")
    return state


def sealed_disk(path, variant, os_name, k8s_version):
    """(disk path, sha256, bytes) of a SEALED sanitize run of exactly this variant; the file is re-hashed."""
    try:
        run = json.loads(Path(path).read_bytes().decode("ascii"))
        facts, disk = run["facts"], Path(run["disk"])
        sha, size = facts["disk_sha256"], facts["disk_bytes"]
        ok = (run["sealed_after_cgroup_stop"] is True and facts["variant"] == variant and facts["os"] == os_name
              and facts.get("kubernetes_version") == k8s_version and HEX64.fullmatch(sha) is not None
              and type(size) is int and size > 0 and disk.is_absolute())
        st = disk.lstat()
    except (OSError, ValueError, KeyError, TypeError):
        raise Refused("sanitize_run_refused") from None
    need(ok, "sanitize_run_refused")
    need(stat.S_ISREG(st.st_mode) and st.st_nlink == 1 and not st.st_mode & 0o222 and st.st_size == size,
         "sealed_disk_refused")
    need(capsule.sha256_file(disk) == sha, "sealed_disk_refused")
    return str(disk), sha, size


def keygen(sdir):
    """ONE ephemeral ed25519 key for this job; the private half stays in the state dir, mode 0600."""
    key = sdir / KEY
    need(not os.path.lexists(key) and not os.path.lexists(str(key) + ".pub"), "key_exists")
    done = subprocess.run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", KEY_COMMENT, "-f", str(key)],
                          stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=60)
    st = key.lstat()
    need(done.returncode == 0 and stat.S_ISREG(st.st_mode) and not st.st_mode & 0o077, "keygen_failed")
    public = Path(str(key) + ".pub").read_text(encoding="ascii").strip()
    need(re.fullmatch(r"ssh-ed25519 [A-Za-z0-9+/=]+ " + KEY_COMMENT, public) is not None, "keygen_failed")
    return public


def ssh(sdir, suffix, address, remote, stdin=b"", timeout=30):
    """BatchMode SSH as the cloud-init user with the job key only. The per-clone known_hosts file starts
    empty: the first contact records the host key (accept-new) and every later one must match it."""
    argv = ["ssh", "-F", "/dev/null", "-T", "-i", str(sdir / KEY), "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes",
            "-o", "IdentityAgent=none", "-o", "UserKnownHostsFile=" + str(sdir / ("known_hosts-" + suffix)),
            "-o", "GlobalKnownHostsFile=/dev/null", "-o", "StrictHostKeyChecking=accept-new",
            "-o", "HostKeyAlgorithms=ssh-ed25519", "-o", "PasswordAuthentication=no",
            "-o", "KbdInteractiveAuthentication=no", "-o", "ForwardAgent=no", "-o", "ClearAllForwardings=yes",
            "-o", "ConnectTimeout=10", "-o", "LogLevel=ERROR", "ubuntu@" + address, "--", *remote]
    # ponytail: stdout is held in memory until the timeout and size-checked after; the peer is this job's
    # own clone. Switch to a bounded pipe read if the guest ever becomes less trusted than the runner.
    return subprocess.run(argv, input=stdin, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=timeout)


def guest_record(raw, variant):
    """Strict gate on the guest's single line: exact keys, booleans, counters and sha256 values only."""
    need(len(raw) <= 65536, "guest_output_refused")
    try:
        row = json.loads(raw.decode("ascii"))
    except (ValueError, UnicodeError):
        row = None
    names = set(coldboot_guest.COMMON + (coldboot_guest.K8S if variant == "kubernetes" else ()))
    count = lambda v: type(v) is int and 0 <= v < 1 << 20  # noqa: E731
    sha = lambda v: type(v) is str and HEX64.fullmatch(v) is not None  # noqa: E731
    need(type(row) is dict and set(row) == GUEST_KEYS and type(row["checks"]) is dict
         and set(row["checks"]) == names and all(type(v) is bool for v in row["checks"].values())
         and sha(row["machine_id_sha256"]) and sha(row["host_key_sha256"]) and type(row["snapd_present"]) is bool
         and all(row[k] is None or sha(row[k]) for k in ("snapd_serial_sha256", "snapd_device_key_sha256"))
         and count(row["snapd_changes_error_lines"]) and count(row["cloud_init_recoverable_warnings"]),
         "guest_output_refused")
    return row


def guest_checks(sdir, cfg, clone, address, public):
    """Wait for SSH on the clone's test address, then run the read-only checks (piped, nothing written)."""
    suffix = clone["suffix"]
    known = sdir / ("known_hosts-" + suffix)
    capsule.write_new(known, b"")  # Fresh: must not exist yet.
    end = time.monotonic() + SSH_DEADLINE
    while True:
        try:
            if ssh(sdir, suffix, address, ["true"]).returncode == 0:
                break
        except subprocess.TimeoutExpired:
            pass
        need(time.monotonic() < end, "ssh_unreachable")
        time.sleep(5)
    remote = ["sudo", "-n", "/usr/bin/python3", "-I", "-", cfg["variant"], OSES[cfg["os"]],
              cfg["kubernetes_version"] or "-", hashlib.sha256(public.split()[1].encode()).hexdigest()]
    done = ssh(sdir, suffix, address, remote, Path(coldboot_guest.__file__).read_bytes(), GUEST_DEADLINE)
    need(done.returncode == 0, "guest_checks_unreadable")
    row = guest_record(done.stdout, cfg["variant"])
    # The host key this job talked to is the one the guest reports: the record came from that machine.
    hosts = [line.split() for line in known.read_text(encoding="ascii").splitlines() if line.strip()]
    need(len(hosts) == 1 and len(hosts[0]) == 3 and hosts[0][1] == "ssh-ed25519"
         and hashlib.sha256(hosts[0][2].encode()).hexdigest() == row["host_key_sha256"], "host_key_mismatch")
    return dict(row, suffix=suffix, vmid=clone["vmid"], name=clone["name"], all_passed=all(row["checks"].values()))


def distinct(a, b):
    """Two cold boots of one image must not share an identity (hashes only)."""
    proof = {key: a[key] != b[key] for key in ("machine_id_sha256", "host_key_sha256")}
    if a["snapd_present"] or b["snapd_present"]:
        for key in ("snapd_serial_sha256", "snapd_device_key_sha256"):
            proof[key] = None not in (a[key], b[key]) and a[key] != b[key]
    return proof


def create_clone(api, cfg, sdir, state, suffix, address, public):
    node = cfg["node"]
    vmid = next(free_vmids(api, cfg), None)
    need(vmid is not None, "vmid_range_exhausted")
    clone = {"suffix": suffix, "vmid": vmid, "name": NAME % (cfg["sha256"][:8], suffix), "state": "reserved"}
    path = "/nodes/%s/qemu/%d" % (node, vmid)

    def mark(step):
        clone["state"] = step
        save(sdir, state)
    state["clones"].append(clone)
    save(sdir, state)  # Before the create: a crash leaves a record of the VMID this run took.
    # Empty and pool-only. The token reaches a VM only through the pool ACL and the VM joins the pool when
    # the create task ends: a create carrying a name or tags is checked on /vms/<id> and answers 403.
    wait(api, node, api.call("POST", "/nodes/%s/qemu" % node, vmid=vmid, pool=cfg["pool"]), "qmcreate:%d" % vmid)
    config = api.call("GET", path + "/config")
    need(not config.get("name") and not config.get("tags") and not config.get("template") and not config.get("lock")
         and not any(DISK_KEY.fullmatch(key) for key in config), "fresh_vm_not_empty")
    need(in_pool(api, cfg, vmid), "fresh_vm_not_in_pool")
    mark("created")

    def write(**params):  # Each write names the digest it read: a concurrent change refuses, never merges.
        wait(api, node, api.call("POST", path + "/config", digest=config["digest"], **params),
             "qmconfig:%d" % vmid, allow_sync=True)
        return owned(api, cfg, clone, "stopped")[0]
    # Name and tags first and alone: from here on the guarded cleanup can recognise this VM.
    config = write(name=clone["name"], tags=";".join(TAGS + (artifact_tag(cfg),)))
    mark("named")
    net = "virtio,bridge=%s%s" % (cfg["bridge"], ",tag=%d" % cfg["vlan"] if cfg["vlan"] else "")
    config = write(ostype="l26", cores=2, memory=4096, cpu="host", agent=1, scsihw="virtio-scsi-single",
                   serial0="socket", vga="serial0", boot="order=scsi0", net0=net,
                   scsi0="%s:0,discard=on,import-from=%s" % (cfg["storage"], cfg["volid"]),
                   ide2="%s:cloudinit" % cfg["storage"])
    need("size=20G" in str(config.get("scsi0")).split(","), "import_not_20g")
    need(set(net.split(",")[1:]) <= set(str(config.get("net0")).split(",")), "network_readback")
    mark("imported")
    wait(api, node, api.call("PUT", path + "/resize", digest=config["digest"], disk="scsi0", size="30G"),
         "resize:%d" % vmid, allow_sync=True)
    config = owned(api, cfg, clone, "stopped")[0]
    need("size=30G" in str(config.get("scsi0")).split(","), "resize_not_30g")
    ipconfig = "ip=%s,gw=%s" % (address, cfg["gateway"])
    config = write(ciuser="ubuntu", sshkeys=urllib.parse.quote(public, safe=""), ipconfig0=ipconfig,
                   nameserver=" ".join(cfg["nameservers"]))
    need("cipassword" not in config and config.get("ciuser") == "ubuntu" and config.get("ipconfig0") == ipconfig
         and urllib.parse.unquote(str(config.get("sshkeys"))).strip() == public, "cloudinit_readback")
    need({key for key in config if DISK_KEY.fullmatch(key)} == {"scsi0", "ide2"}, "unexpected_disk")
    mark("preboot")
    wait(api, node, api.call("POST", path + "/status/start"), "qmstart:%d" % vmid)
    owned(api, cfg, clone, "running")
    mark("started")
    return clone


def cleanup(api, sdir, state):
    """Guarded cleanup of exactly what the state file names; returns the cleanup facts (fixed codes)."""
    cfg, node = state["cfg"], state["cfg"]["node"]
    facts = {"clones": {}, "import_volume": "not_uploaded", "private_key_deleted": False}
    for clone in state["clones"]:
        vmid = clone["vmid"]
        path = "/nodes/%s/qemu/%d" % (node, vmid)
        try:
            if not exists(api, vmid):
                outcome = "absent"
            else:
                running = owned(api, cfg, clone)[1] == "running"
                need([s.get("name") for s in api.call("GET", path + "/snapshot")] == ["current"],
                     "unexpected_snapshot")
                if running:
                    wait(api, node, api.call("POST", path + "/status/stop"), "qmstop:%d" % vmid)
                owned(api, cfg, clone, "stopped")  # Re-read: still ours, now stopped.
                wait(api, node, api.call("DELETE", path, purge=1), "qmdestroy:%d" % vmid)
                need(not exists(api, vmid), "destroy_not_confirmed")
                outcome = "destroyed"
        except (Refused, ApiError) as exc:
            outcome = "refused_" + str(exc)
        except Exception as exc:  # noqa: BLE001 - class name only
            outcome = "error_" + type(exc).__name__
        facts["clones"][str(vmid)] = outcome
    if state["import_uploaded"]:  # Only the exact volume this run uploaded; a pre-existing name refused earlier.
        content = "/nodes/%s/storage/%s/content" % (node, cfg["storage"])
        listed = lambda: cfg["volid"] in [v.get("volid") for v in api.call("GET", content, content="import")]  # noqa: E731
        try:
            if not listed():
                facts["import_volume"] = "absent"
            else:
                configs = protected(api)[1]
                need(not any(cfg["volid"] in str(value) for config in configs.values() for value in config.values()),
                     "import_volume_referenced")
                wait(api, node, api.call("DELETE", content + "/" + urllib.parse.quote(cfg["volid"], safe="")),
                     STORAGE_TASK, allow_sync=True)
                need(not listed(), "import_volume_remains")
                facts["import_volume"] = "deleted"
        except (Refused, ApiError) as exc:
            facts["import_volume"] = "refused_" + str(exc)
        except Exception as exc:  # noqa: BLE001 - class name only
            facts["import_volume"] = "error_" + type(exc).__name__
    try:
        for name in (KEY, KEY + ".pub", *("known_hosts-" + s for s in SUFFIXES)):
            (sdir / name).unlink(missing_ok=True)
    except OSError:
        pass
    facts["private_key_deleted"] = not os.path.lexists(sdir / KEY)
    return facts


def finish(api, sdir, result):
    """Guarded cleanup from the state file, then the protected-set comparison. True when nothing is left
    and every guest that was visible before is still there with the same status and config digest."""
    state = load_state(sdir)
    result["cleanup"] = facts = cleanup(api, sdir, state)
    try:
        unchanged = protected(api)[0] == state["protected"]  # Also fails on anything new, a leftover included.
    except (Refused, ApiError):
        unchanged = False
    result["protected_set"] = {"guests": len(state["protected"]), "unchanged": unchanged}
    clean = (all(v in ("destroyed", "absent") for v in facts["clones"].values())
             and facts["import_volume"] in ("deleted", "absent", "not_uploaded") and facts["private_key_deleted"])
    if not clean:
        result["reason"] = result.get("reason") or "cleanup_incomplete"
    elif not unchanged:
        result["reason"] = result.get("reason") or "protected_set_changed"
    state["phase"] = "finished"
    save(sdir, state)
    return clean and unchanged


def reason_of(exc):
    if isinstance(exc, (Refused, ApiError)):
        return str(exc)
    if isinstance(exc, subprocess.TimeoutExpired):
        return "ssh_timeout"
    return "error_" + type(exc).__name__  # Class name only.


def run(api, cfg, sdir):
    result = {"kind": "ColdBootAcceptance", "accepted": False, "reason": None, "step": "validate",
              "variant": cfg["variant"], "os": cfg["os"], "kubernetes_version": cfg["kubernetes_version"],
              "pve_tls_verified": bool(getattr(api, "tls_verified", False)), "clones": [], "distinct_identity": None,
              "cleanup": None, "protected_set": None}
    recorded = False
    try:
        need(not os.path.lexists(sdir / STATE), "state_exists")  # A previous run's record is never overwritten.
        disk, sha, size = sealed_disk(cfg["sanitize_run"], cfg["variant"], cfg["os"], cfg["kubernetes_version"])
        name = "pc-export-%s.qcow2" % sha[:12]
        cfg = dict(cfg, sha256=sha, volid="%s:import/%s" % (cfg["storage"], name))
        result.update(disk_sha256=sha, disk_bytes=size, import_volume=cfg["volid"], step="preflight")
        before, configs = protected(api)
        need(not address_in_use(configs, cfg["addresses"]), "test_address_in_use")
        content = "/nodes/%s/storage/%s/content" % (cfg["node"], cfg["storage"])
        need(cfg["volid"] not in [v.get("volid") for v in api.call("GET", content, content="import")],
             "import_volume_exists")
        free = free_vmids(api, cfg)
        need(all(next(free, None) is not None for _ in SUFFIXES), "vmid_range_exhausted")
        state = {"cfg": {k: cfg[k] for k in ("node", "storage", "pool", "sha256", "volid", "vmid_low", "vmid_high")},
                 "protected": before, "import_uploaded": False, "clones": [], "phase": "running"}
        save(sdir, state)
        recorded = True
        result["step"] = "keygen"
        public = keygen(sdir)
        result["step"] = "upload"
        state["import_uploaded"] = True
        save(sdir, state)  # Before the upload: from here on the exact volume is this run's to delete.
        wait(api, cfg["node"], api.upload(cfg["node"], cfg["storage"], name, disk, sha, size), STORAGE_TASK)
        need(cfg["volid"] in [v.get("volid") for v in api.call("GET", content, content="import")],
             "import_volume_missing")
        result["step"] = "create"
        clones = [create_clone(api, cfg, sdir, state, suffix, address, public)
                  for suffix, address in zip(SUFFIXES, cfg["addresses"])]
        result["step"] = "guest_checks"
        for clone, address in zip(clones, cfg["addresses"]):
            result["clones"].append(guest_checks(sdir, cfg, clone, address.split("/")[0], public))
        result["distinct_identity"] = distinct(*result["clones"])
        need(all(row["all_passed"] for row in result["clones"]), "guest_checks_failed")
        need(all(result["distinct_identity"].values()), "identity_not_distinct")
        result["step"] = "cleanup"
    except BaseException as exc:  # noqa: BLE001 - every failure still gets the guarded cleanup and a result
        result["reason"] = reason_of(exc)
    clean = False
    if recorded:
        try:
            clean = finish(api, sdir, result)
        except BaseException as exc:  # noqa: BLE001
            result["reason"] = result["reason"] or "cleanup_" + reason_of(exc)
    result["accepted"] = result["reason"] is None and clean
    if result["accepted"]:
        result["step"] = "done"
    return result


def cleanup_only(api, sdir):
    """The guarded cleanup of an interrupted run, from its state file alone."""
    result = {"kind": "ColdBootCleanup", "reason": None, "cleanup": None, "protected_set": None}
    if not os.path.lexists(sdir / STATE):
        return dict(result, clean=True, reason="no_state")  # Nothing was ever created by a run here.
    try:
        clean = finish(api, sdir, result)
    except BaseException as exc:  # noqa: BLE001
        clean, result["reason"] = False, reason_of(exc)
    return dict(result, clean=clean)


def settings(a):
    """Validated run settings; anything odd refuses before the first API call."""
    try:
        interfaces = [ipaddress.ip_interface(x) for x in a.test_address]
        gateway = ipaddress.ip_address(a.gateway)
        nameservers = [str(ipaddress.ip_address(x)) for x in a.nameserver]
        low, high = (int(x) for x in re.fullmatch(r"([1-9][0-9]{2,8})-([1-9][0-9]{2,8})", a.vmid_range).groups())
    except (ValueError, AttributeError, TypeError):
        raise Refused("invalid_settings") from None
    network = interfaces[0].network if interfaces else None
    need(len(interfaces) == len(SUFFIXES) and len({i.ip for i in interfaces}) == len(interfaces)
         and all(i.version == 4 and i.network == network and i.network.prefixlen <= 30 and i.ip != gateway
                 and i.ip not in (network.network_address, network.broadcast_address) for i in interfaces)
         and gateway in network and nameservers, "invalid_settings")
    need(all(IDENT.fullmatch(x) for x in (a.node, a.storage, a.pool, a.bridge)) and 0 <= a.vlan <= 4094
         and low <= high < low + 100, "invalid_settings")
    need(a.variant in ("os", "kubernetes") and a.os in OSES and (
        (a.os == "ubuntu-2604" and a.kubernetes_version in vm_sanitize.K8S_VERSIONS) if a.variant == "kubernetes"
        else a.kubernetes_version is None), "invalid_settings")
    return {"sanitize_run": a.sanitize_run, "variant": a.variant, "os": a.os, "kubernetes_version": a.kubernetes_version,
            "node": a.node, "storage": a.storage, "pool": a.pool, "bridge": a.bridge, "vlan": a.vlan,
            "addresses": [str(i) for i in interfaces], "gateway": str(gateway), "nameservers": nameservers,
            "vmid_low": low, "vmid_high": high}


def state_dir(path):
    """Private directory of this user: it holds the run record and, during the run, the private key."""
    path = Path(path)
    st = path.lstat() if path.is_absolute() and os.path.lexists(path) else None
    need(st is not None and stat.S_ISDIR(st.st_mode) and st.st_uid == os.getuid() and not st.st_mode & 0o077,
         "state_dir_refused")
    return path


def _terminated(signum, frame):
    """A cancelled job still gets its guarded cleanup; a second signal cannot interrupt that."""
    signal.signal(signal.SIGTERM, signal.SIG_IGN)
    signal.signal(signal.SIGINT, signal.SIG_IGN)
    raise Refused("terminated")


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)
    r = sub.add_parser("run")
    for name in ("--sanitize-run", "--node", "--storage", "--gateway"):
        r.add_argument(name, required=True)
    r.add_argument("--variant", required=True, choices=("os", "kubernetes"))
    r.add_argument("--os", required=True, choices=sorted(OSES))
    r.add_argument("--kubernetes-version")
    r.add_argument("--pool", default=POOL)
    r.add_argument("--bridge", default="vmbr0")
    r.add_argument("--vlan", type=int, default=0, help="0 untagged, else 1..4094")
    r.add_argument("--test-address", action="append", default=[], help="CIDR of one clone; exactly two")
    r.add_argument("--nameserver", action="append", default=[])
    r.add_argument("--vmid-range", default="9290-9299", help="reserved clone VMIDs, LOW-HIGH")
    c = sub.add_parser("cleanup")
    for p in (r, c):
        p.add_argument("--state-dir", required=True)
        p.add_argument("--out", help="result file (default: <state-dir>/coldboot-result.json, "
                                     "cleanup: coldboot-cleanup-result.json)")
    a = parser.parse_args(argv)
    try:
        sdir = state_dir(a.state_dir)
        out = Path(a.out) if a.out else sdir / (RESULT if a.command == "run" else "coldboot-cleanup-result.json")
        cfg = settings(a) if a.command == "run" else None
        from build import PVE  # Token client from PVE_URL/PVE_TOKEN_ID/PVE_TOKEN_SECRET.
        api = Client(PVE())
        signal.signal(signal.SIGTERM, _terminated)
        signal.signal(signal.SIGINT, _terminated)
        result = run(api, cfg, sdir) if a.command == "run" else cleanup_only(api, sdir)
        tmp = Path(str(out) + ".tmp")
        fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, "wb") as f:
            f.write((json.dumps(result, indent=2, sort_keys=True) + "\n").encode("ascii"))
        os.replace(tmp, out)
        ok = result["accepted"] if a.command == "run" else result["clean"]
        print(json.dumps({"kind": result["kind"], "ok": ok, "reason": result["reason"]}, sort_keys=True))
        return 0 if ok else 1
    except (Refused, OSError, KeyError) as exc:
        print("coldboot refused: " + reason_of(exc), file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
