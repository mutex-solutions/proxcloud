#!/usr/bin/env python3
"""Read-only checks inside one owned cold-boot clone. vm_export.coldboot pipes this file over SSH to

    sudo -n /usr/bin/python3 -I - <os|kubernetes> <VERSION_ID> <kubernetes version|-> <sha256 of the job key>

Prints ONE line of bounded facts: booleans, counters and identities as sha256 only. No credential, raw
log or guest text, and no package or configuration write. Stdlib only; never imported inside a guest.
"""
import hashlib
import json
import os
import pathlib
import re
import subprocess
import sys
import time

COMMON = ("os_release", "cloud_init_done_no_errors", "default_user_exactly_job_key", "build_user_only_own_group",
          "build_user_no_sudo", "build_user_locked", "build_user_no_keys", "machine_id_valid",
          "qemu_guest_agent_active", "snapd_active", "snapd_reregistered_new_key", "no_active_swap", "disk_30g",
          "root_grown")
K8S = ("builder_account_retained", "kubelet_enabled", "containerd_active", "k8s_binaries_exact",
       "crictl_talks_to_containerd")
BUILD_USERS = ("builder", "packer")
SERIAL_WAIT = 300  # snapd asks for its new serial asynchronously after first boot.


def run(args, timeout=60):
    r = subprocess.run(args, capture_output=True, text=True, timeout=timeout)
    if len(r.stdout) + len(r.stderr) > 262144:
        raise RuntimeError("bounded result refused")
    return r


def sha(text):
    return hashlib.sha256(text.encode()).hexdigest()


def active(unit):
    return run(["/usr/bin/systemctl", "is-active", unit]).stdout.strip() == "active"


def build_user(name, home, shadow):
    """(only own group, no sudo, locked, no keys) of one build account that exists."""
    line = run(["/usr/bin/id", name]).stdout.strip()
    groups = re.findall(r"\d+\(([^)]+)\)", line.split("groups=", 1)[1]) if "groups=" in line else []
    listed = run(["/usr/bin/sudo", "-n", "-l", "-U", name])
    text = (listed.stdout + listed.stderr).lower()
    no_sudo = ("not allowed" in text or "may not run" in text) and "(all" not in text
    return (groups == [name], no_sudo, shadow.get(name, "").startswith("!"),
            not pathlib.Path(home, ".ssh", "authorized_keys").exists())


def snapd():
    """(active, serial, device key) once snapd is registered again; read-only presence and values to hash."""
    ok = run(["/usr/bin/snap", "version"]).returncode == 0
    end = time.monotonic() + SERIAL_WAIT
    while True:
        model = run(["/usr/bin/snap", "model", "--serial", "--assertion"], timeout=120)
        assertion = model.stdout if model.returncode == 0 else ""
        serial = re.search(r"(?m)^serial: (\S+)$", assertion)
        key = re.search(r"(?m)^device-key-sha3-384: (\S+)$", assertion)
        if (serial and key) or time.monotonic() >= end:
            break
        time.sleep(10)
    errors = sum(1 for row in run(["/usr/bin/snap", "changes"]).stdout.splitlines() if " Error " in row)
    return ok and active("snapd"), serial and serial.group(1), key and key.group(1), errors


def main(argv):
    variant, version_id, k8s_version, key_sha256 = argv
    osr = dict(row.split("=", 1) for row in pathlib.Path("/etc/os-release").read_text().splitlines() if "=" in row)
    cloud = json.loads(run(["/usr/bin/cloud-init", "status", "--format", "json", "--wait"], timeout=600).stdout)
    errors = list(cloud.get("errors", []))
    for stage in ("init-local", "init", "modules-config", "modules-final"):
        if isinstance(cloud.get(stage), dict):
            errors += cloud[stage].get("errors", [])
    rows = [row.split(":") for row in pathlib.Path("/etc/passwd").read_text().splitlines()]
    homes = {row[0]: row[5] for row in rows if len(row) == 7}
    shadow = {row.split(":")[0]: row.split(":")[1] for row in pathlib.Path("/etc/shadow").read_text().splitlines()
              if row.count(":") >= 8}
    keys = pathlib.Path("/home/ubuntu/.ssh/authorized_keys")
    lines = [row.split() for row in keys.read_text().splitlines() if row.strip() and not row.startswith("#")] \
        if keys.is_file() else []
    # Present build accounts must be unusable; the kubernetes image keeps a locked `builder` by review.
    build = [build_user(name, homes[name], shadow) for name in BUILD_USERS if name in homes]
    machine_id = pathlib.Path("/etc/machine-id").read_text().strip()
    host_key = pathlib.Path("/etc/ssh/ssh_host_ed25519_key.pub").read_text().split()[1]
    snapd_present = os.path.exists("/usr/bin/snap")
    snapd_ok, serial, device_key, snap_errors = snapd() if snapd_present else (True, None, None, 0)
    key_dir = pathlib.Path("/var/lib/snapd/device/private-keys-v1")
    lsblk = json.loads(run(["/usr/bin/lsblk", "-b", "-J", "-o", "NAME,TYPE,SIZE,MOUNTPOINTS"]).stdout)
    disk = next((d for d in lsblk.get("blockdevices", []) if d.get("type") == "disk"), {})
    root = os.statvfs("/")
    checks = {
        "os_release": osr.get("ID") == "ubuntu" and osr.get("VERSION_ID", "").strip('"') == version_id,
        "cloud_init_done_no_errors": cloud.get("status") == "done" and not errors,
        "default_user_exactly_job_key": len(lines) == 1 and len(lines[0]) >= 2 and lines[0][0] == "ssh-ed25519"
        and sha(lines[0][1]) == key_sha256,
        "build_user_only_own_group": all(row[0] for row in build),
        "build_user_no_sudo": all(row[1] for row in build),
        "build_user_locked": all(row[2] for row in build),
        "build_user_no_keys": all(row[3] for row in build),
        "machine_id_valid": re.fullmatch("[0-9a-f]{32}", machine_id) is not None,
        "qemu_guest_agent_active": active("qemu-guest-agent"),
        "snapd_active": snapd_ok,
        "snapd_reregistered_new_key": not snapd_present or (
            bool(serial) and bool(device_key) and key_dir.is_dir() and any(key_dir.iterdir())),
        "no_active_swap": not pathlib.Path("/proc/swaps").read_text().splitlines()[1:],
        "disk_30g": disk.get("size") == 30 << 30,
        "root_grown": root.f_blocks * root.f_frsize >= 28 << 30,
    }
    if variant == "kubernetes":
        want = "v" + k8s_version
        kubectl = run(["/usr/bin/kubectl", "version", "--client", "-o", "json"])
        client = json.loads(kubectl.stdout).get("clientVersion", {}).get("gitVersion") if kubectl.returncode == 0 else None
        crictl = run(["/usr/local/bin/crictl", "--runtime-endpoint", "unix:///run/containerd/containerd.sock", "version"])
        checks.update({
            "builder_account_retained": "builder" in homes,
            "kubelet_enabled": run(["/usr/bin/systemctl", "is-enabled", "kubelet"]).stdout.strip() == "enabled",
            "containerd_active": active("containerd"),
            "k8s_binaries_exact": run(["/usr/bin/kubeadm", "version", "-o", "short"]).stdout.strip() == want
            and run(["/usr/bin/kubelet", "--version"]).stdout.split()[-1:] == [want] and client == want,
            "crictl_talks_to_containerd": crictl.returncode == 0 and "containerd" in crictl.stdout,
        })
    print(json.dumps({
        "checks": checks, "machine_id_sha256": sha(machine_id), "host_key_sha256": sha(host_key),
        "snapd_present": snapd_present, "snapd_serial_sha256": sha(serial) if serial else None,
        "snapd_device_key_sha256": sha(device_key) if device_key else None,
        "snapd_changes_error_lines": snap_errors,
        "cloud_init_recoverable_warnings": sum(
            len(v["recoverable_errors"].get("WARNING", [])) for v in cloud.values()
            if isinstance(v, dict) and isinstance(v.get("recoverable_errors"), dict))}, sort_keys=True), flush=True)


if __name__ == "__main__":
    main(sys.argv[1:])
