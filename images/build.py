#!/usr/bin/env python3
"""Build a ProxCloud OS template on one Proxmox node (images/README.md). Stdlib only; the same entry
point serves the pipeline (.github/workflows/vm-templates.yml) and the installer (20-templates.yml).

    build.py build --os debian-13 --node pve1   base image -> Packer -> tags -> smoke test -> prune
    build.py base  --os debian-13 --node pve1   keep just a base template (a mgmt VM clones it), print its vmid

Reads PVE_URL, PVE_TOKEN_ID, PVE_TOKEN_SECRET; the rest comes from flags, see --help.
"""
import argparse
import datetime
import json
import os
import re
import ssl
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
POOL = "pc-templates"
# Virtual disk of every newly built native OS template: the floor a fresh vendor-image base is grown to before
# its first boot (docs/template-build-disk.md). It was 20 until 2026-11; 5 GiB still leaves the dist-upgrade room
# over the vendor images (3 to 3.5 GiB) and a server made from the template gets the size its user asks for.
# Kept equal to vm_sanitize.VIRTUAL_BYTES["os"][0] by images/tests/test_template_disk_sizes.py.
DEFAULT_BUILD_DISK_GIB = 5

# The vendor images. The sums file is fetched at build time and hands Proxmox the checksum to verify.
IMAGES = {
    "debian-13": {
        "url": "https://cloud.debian.org/images/cloud/trixie/latest/debian-13-genericcloud-amd64.qcow2",
        "sums": "https://cloud.debian.org/images/cloud/trixie/latest/SHA512SUMS",
        "algorithm": "sha512",
        "user": "debian",
    },
    "debian-12": {
        "url": "https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2",
        "sums": "https://cloud.debian.org/images/cloud/bookworm/latest/SHA512SUMS",
        "algorithm": "sha512",
        "user": "debian",
    },
    "ubuntu-2404": {
        "url": "https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img",
        "sums": "https://cloud-images.ubuntu.com/noble/current/SHA256SUMS",
        "algorithm": "sha256",
        "user": "ubuntu",
    },
    "ubuntu-2604": {
        "url": "https://cloud-images.ubuntu.com/releases/resolute/release-20261016/ubuntu-26.04-server-cloudimg-amd64.img",
        "sums": "https://cloud-images.ubuntu.com/releases/resolute/release-20261016/SHA256SUMS",
        "algorithm": "sha256",
        "user": "ubuntu",
    },
}


class PVE:
    def __init__(self):
        self.base = os.environ["PVE_URL"].rstrip("/") + "/api2/json"
        self.auth = "PVEAPIToken=%s=%s" % (os.environ["PVE_TOKEN_ID"], os.environ["PVE_TOKEN_SECRET"])
        self.ctx = ssl._create_unverified_context()

    def call(self, method, path, **params):
        data = urllib.parse.urlencode(params).encode() if params else None
        req = urllib.request.Request(self.base + path, data=data, method=method, headers={"Authorization": self.auth})
        try:
            with urllib.request.urlopen(req, context=self.ctx, timeout=120) as r:
                return json.load(r).get("data")
        except urllib.error.HTTPError as e:
            raise SystemExit("%s %s: %s %s %s" % (method, path, e.code, e.reason, e.read()[:400].decode(errors="replace")))

    def wait(self, node, upid):
        while True:
            st = self.call("GET", "/nodes/%s/tasks/%s/status" % (node, urllib.parse.quote(upid, safe="")))
            if st["status"] == "stopped":
                if st.get("exitstatus") != "OK":
                    raise SystemExit("task %s: %s" % (upid, st.get("exitstatus")))
                return
            time.sleep(3)

    def vms(self):
        return self.call("GET", "/cluster/resources?type=vm")

    def destroy(self, node, vmid):
        self.wait(node, self.call("DELETE", "/nodes/%s/qemu/%s?purge=1&destroy-unreferenced-disks=1" % (node, vmid)))


def checksum_of(img):
    with urllib.request.urlopen(img["sums"], timeout=60) as r:
        for line in r.read().decode().splitlines():
            parts = line.split()
            if len(parts) == 2 and parts[1].lstrip("*") == img["url"].rsplit("/", 1)[1]:
                return parts[0]
    raise SystemExit("no checksum for %s in %s" % (img["url"], img["sums"]))


def ensure_cloud_image(pve, a, img):
    """Download the vendor image once per upstream version: Proxmox refuses to overwrite a file, so the
    checksum is in the name, and superseded downloads of this OS are removed."""
    checksum = checksum_of(img)
    prefix = "pc-%s-" % a.os
    name = "%s%s.qcow2" % (prefix, checksum[:12])  # `import` content wants a known extension; a Ubuntu .img is a qcow2
    volid = "%s:import/%s" % (a.import_storage, name)
    content = "/nodes/%s/storage/%s/content" % (a.node, a.import_storage)
    have = [v["volid"] for v in pve.call("GET", content + "?content=import")]
    for old in have:
        if old != volid and old.startswith("%s:import/%s" % (a.import_storage, prefix)):
            pve.call("DELETE", "%s/%s" % (content, urllib.parse.quote(old, safe="")))
    if volid not in have:
        pve.wait(a.node, pve.call(
            "POST", "/nodes/%s/storage/%s/download-url" % (a.node, a.import_storage),
            content="import", filename=name, url=img["url"], checksum=checksum, **{"checksum-algorithm": img["algorithm"]},
        ))
    return volid


def next_vmid(pve, vms, start=9200):
    used = {v["vmid"] for v in vms}
    while start in used:
        start += 1
    return start


def disk_size_bytes(disk):
    """Read one PVE disk's size= in bytes, not rounded GiB (bare bytes or K/M/G/T)."""
    sizes = [part[5:] for part in str(disk).split(",")[1:] if part.startswith("size=")]
    match = re.fullmatch(r"([0-9]+(?:\.[0-9]+)?)([KMGT]?)", sizes[0]) if len(sizes) == 1 else None
    if not match:
        raise SystemExit("base disk: missing or invalid scsi0 size")
    whole, _, fraction = match[1].partition(".")
    # Exact decimal-to-byte conversion; never round an under-sized disk up to the floor.
    multiplier = {"": 1, "K": 1024, "M": 1024 ** 2, "G": 1024 ** 3, "T": 1024 ** 4}[match[2]]
    size, remainder = divmod(int(whole + fraction) * multiplier, 10 ** len(fraction))
    if remainder or size <= 0:
        raise SystemExit("base disk: invalid scsi0 byte size")
    return size


def new_base_config(pve, a, vmid, name, configured=True):
    """Only for the exact VM whose create task this call completed, never an adopted base.

    Pool membership is read directly: cluster/resources can lag or omit the pool on PVE 9.
    The remaining checks also protect failure cleanup from deleting a changed/converted VM.
    """
    members = pve.call("GET", "/pools/" + POOL).get("members", [])
    if not any(v.get("vmid") == vmid and v.get("node") == a.node and v.get("type") == "qemu" for v in members):
        raise SystemExit("base disk: new VM is not in the expected node/pool; refusing mutation")
    path = "/nodes/%s/qemu/%s" % (a.node, vmid)
    cfg = pve.call("GET", path + "/config")
    tags = set(filter(None, str(cfg.get("tags", "")).split(";")))
    allowed_names = (name,) if configured else ("", name)
    allowed_tags = ({"proxcloud", "pc-base"},) if configured else (set(), {"proxcloud", "pc-base"})
    if (cfg.get("template", 0) not in (0, "0") or cfg.get("lock")
            or cfg.get("name", "") not in allowed_names or tags not in allowed_tags):
        raise SystemExit("base disk: new VM identity/template/lock changed; refusing mutation")
    if pve.call("GET", path + "/status/current").get("status") != "stopped":
        raise SystemExit("base disk: new VM is not stopped; refusing mutation")
    return cfg


def ensure_build_disk(pve, a, vmid, name):
    """Enlarge only a confirmed fresh base, before template conversion or any guest boot."""
    path = "/nodes/%s/qemu/%s" % (a.node, vmid)
    cfg = new_base_config(pve, a, vmid, name)
    disk = cfg.get("scsi0", "")
    before = disk_size_bytes(disk)
    floor = DEFAULT_BUILD_DISK_GIB * 1024 ** 3
    if before < floor:
        if not cfg.get("digest"):
            raise SystemExit("base disk: missing config digest; refusing resize")
        result = pve.call("PUT", path + "/resize", disk="scsi0", size="%sG" % DEFAULT_BUILD_DISK_GIB, digest=cfg["digest"])
        # PVE can return null for synchronous completion. Never pass null/garbage to wait().
        if result not in (None, ""):
            pattern = r"UPID:%s:[0-9A-Fa-f]+:[0-9A-Fa-f]+:[0-9A-Fa-f]+:[^:]+:%s:[^:]+:" % (re.escape(a.node), vmid)
            if not isinstance(result, str) or not re.fullmatch(pattern, result):
                raise SystemExit("base disk: resize returned an invalid task ID")
            pve.wait(a.node, result)
    after = new_base_config(pve, a, vmid, name).get("scsi0", "")
    if after.split(",", 1)[0] != disk.split(",", 1)[0] or disk_size_bytes(after) < max(floor, before):
        raise SystemExit("base disk: scsi0 readback below floor or disk changed; refusing template conversion")


def create_base(pve, a, img, name):
    """A fresh vendor-image template with a verified build disk, cloud-init, console and agent flag."""
    volume = ensure_cloud_image(pve, a, img)
    vmid = next_vmid(pve, pve.vms())
    net = "virtio,bridge=%s%s" % (a.bridge, ",tag=%s" % a.vlan if a.vlan else "")
    # Empty first: the scoped token holds VM.Config.* only through the pool, so only a VM already in the
    # pool can be configured. Do not claim/clean up this ID unless its create task succeeds.
    pve.wait(a.node, pve.call("POST", "/nodes/%s/qemu" % a.node, vmid=vmid, pool=POOL))
    try:
        pve.wait(a.node, pve.call(
            "POST", "/nodes/%s/qemu/%s/config" % (a.node, vmid), name=name, ostype="l26", cores=2, memory=2048,
            cpu="host", agent=1, scsihw="virtio-scsi-single", serial0="socket", vga="serial0", net0=net, tags="proxcloud;pc-base",
            scsi0="%s:0,discard=on,import-from=%s" % (a.storage, volume), ide2="%s:cloudinit" % a.storage, boot="order=scsi0",
        ))
        ensure_build_disk(pve, a, vmid, name)
        pve.wait(a.node, pve.call("POST", "/nodes/%s/qemu/%s/template" % (a.node, vmid)))
        return vmid
    except (Exception, SystemExit):
        # cmd_build cannot clean a base that never returned. Limit this recovery to the
        # exact successfully created ID; no prefix search, old template, unlock or forced stop.
        try:
            new_base_config(pve, a, vmid, name, configured=False)
            pve.destroy(a.node, vmid)
        except (Exception, SystemExit) as cleanup_error:
            print("base cleanup refused/failed for newly created VM %s: %s" % (vmid, cleanup_error), file=sys.stderr)
        raise


def cmd_base(pve, a, img):
    name = "pc-base-%s-%s" % (a.os, a.node)
    for v in pve.vms():
        if v.get("name") == name and v.get("template") == 1:
            print(v["vmid"])
            return
    print(create_base(pve, a, img, name))


# The portal's Template updates import marker (docs/template-updates.md): such a template's lifecycle
# (smoke test, publish, withdraw) belongs to the portal, so build.py never prunes it. A template with a
# pc-src-<digest> tag was imported by the portal before the marker existed: never pruned either.
IMPORT_MARKER = "pc-tu-import"
IMPORT_SOURCE_PREFIX = "pc-src-"


def portal_import(tags):
    """A template the portal's Template updates imported (marker, or a pre-marker pc-src- tag)."""
    return IMPORT_MARKER in tags or any(t.startswith(IMPORT_SOURCE_PREFIX) for t in tags)


def versions_of(vms, os_slug, node, prunable=False):
    """(version, vmid) of this image's published templates on node, oldest first. A template without a
    pc-v-* tag was placed by hand: it is not ours to prune, so it is not listed. With prunable, a
    portal import (portal_import) is left out too: it still counts as a version (--if-missing), but
    is never pruned and does not take a --keep slot."""
    out = []
    for v in vms:
        tags = str(v.get("tags", "")).split(";")
        if v.get("template") == 1 and v.get("node") == node and v.get("pool") == POOL and {"pc-public", "pc-img-" + os_slug} <= set(tags):
            if prunable and portal_import(tags):
                continue
            ver = [t[len("pc-v-"):] for t in tags if t.startswith("pc-v-")]
            if ver:
                out.append((ver[0], v["vmid"]))
    return sorted(out)


def prune_candidates(vms, os_slug, node, keep):
    """The published pipeline versions beyond the newest keep (keep >= 1, enforced by the CLI): never a
    portal import, never the newest version."""
    if keep < 1:
        raise SystemExit("--keep must be at least 1")
    return [vm for _, vm in versions_of(vms, os_slug, node, prunable=True)[:-keep]]


def packer(a, base_vmid, vmid, name, version):
    env = dict(
        os.environ,
        PKR_VAR_proxmox_url=os.environ["PVE_URL"].rstrip("/") + "/api2/json",
        PKR_VAR_token_id=os.environ["PVE_TOKEN_ID"],
        PKR_VAR_token_secret=os.environ["PVE_TOKEN_SECRET"],
        PKR_VAR_node=a.node,
        PKR_VAR_base_vmid=str(base_vmid),
        PKR_VAR_vmid=str(vmid),
        PKR_VAR_name=name,
        PKR_VAR_tags="proxcloud;pc-building;pc-img-%s;pc-v-%s" % (a.os, version),
        PKR_VAR_storage=a.storage,
        PKR_VAR_bridge=a.bridge,
        PKR_VAR_vlan=str(a.vlan),
        PKR_VAR_build_ip=a.build_ip,
        PKR_VAR_build_gateway=a.build_gateway,
        PKR_VAR_nameserver=a.nameserver or a.build_gateway,
    )
    d = os.path.join(HERE, "packer")
    subprocess.run(["packer", "init", "."], cwd=d, env=env, check=True)
    subprocess.run(
        ["packer", "build", "-force", "-var-file", "os/%s.pkrvars.hcl" % a.os, "."],
        cwd=d, env=env, check=True, stdout=sys.stderr,
    )


def smoke_test(pve, a, vmid, name):
    """Clone, boot, wait for the guest agent to report the address cloud-init configured, destroy."""
    test_id = next_vmid(pve, pve.vms(), vmid + 1)
    pve.wait(a.node, pve.call(
        "POST", "/nodes/%s/qemu/%s/clone" % (a.node, vmid),
        newid=test_id, name="smoke-" + name, full=1, storage=a.storage, pool=POOL,
    ))
    try:
        pve.wait(a.node, pve.call(
            "POST", "/nodes/%s/qemu/%s/config" % (a.node, test_id),
            ipconfig0="ip=%s,gw=%s" % (a.build_ip, a.build_gateway), ciuser="smoke",
        ))
        pve.wait(a.node, pve.call("POST", "/nodes/%s/qemu/%s/status/start" % (a.node, test_id)))
        want = a.build_ip.split("/")[0]
        deadline = time.time() + 300
        while time.time() < deadline:
            req = urllib.request.Request(
                pve.base + "/nodes/%s/qemu/%s/agent/network-get-interfaces" % (a.node, test_id),
                headers={"Authorization": pve.auth},
            )
            try:
                with urllib.request.urlopen(req, context=pve.ctx, timeout=30) as r:
                    if want in json.dumps(json.load(r)):
                        return
            except urllib.error.HTTPError:
                pass  # 500 until the agent is up
            time.sleep(5)
        raise SystemExit("smoke test: the guest agent never reported %s" % want)
    finally:
        try:
            pve.wait(a.node, pve.call("POST", "/nodes/%s/qemu/%s/status/stop" % (a.node, test_id)))
        except SystemExit:
            pass
        pve.destroy(a.node, test_id)


def cmd_build(pve, a, img):
    vms = pve.vms()
    if a.if_missing and versions_of(vms, a.os, a.node):
        print(json.dumps({"os": a.os, "node": a.node, "built": False}))
        return
    now = datetime.datetime.now(datetime.timezone.utc)
    version = now.strftime("%Y%m%d%H%M")
    # An earlier failed run must not leave its base or half-built template behind.
    base_name = "pc-tmp-%s-%s" % (a.os, a.node)
    for v in vms:
        if v.get("pool") == POOL and v.get("node") == a.node and (
                v.get("name") == base_name
                or (str(v.get("name", "")).startswith("pc-%s-" % a.os) and "pc-public" not in str(v.get("tags", "")))):
            pve.destroy(a.node, v["vmid"])
    name = "pc-%s-%s" % (a.os, version)
    base = create_base(pve, a, img, base_name)
    vmid = next_vmid(pve, pve.vms(), base + 1)
    try:
        packer(a, base, vmid, name, version)
    finally:
        pve.destroy(a.node, base)
    # Packer leaves its build identity on the template; a clone must not inherit it.
    pve.wait(a.node, pve.call(
        "POST", "/nodes/%s/qemu/%s/config" % (a.node, vmid), agent=1,
        delete="ipconfig0,sshkeys,ciuser,cipassword,nameserver",
    ))
    smoke_test(pve, a, vmid, name)
    # The smoke clone must use the real build network first. Only then remove its NIC
    # from the template: tenant/server creation supplies its own net0 and must never
    # inherit the build bridge, MAC or access VLAN. Failed cleanup cannot publish.
    pve.wait(a.node, pve.call(
        "POST", "/nodes/%s/qemu/%s/config" % (a.node, vmid), delete="net0",
    ))
    if "net0" in pve.call("GET", "/nodes/%s/qemu/%s/config" % (a.node, vmid)):
        raise SystemExit("template cleanup: build NIC remains; refusing publication")
    # Published only now: the portal never sees an untested template.
    pve.wait(a.node, pve.call(
        "POST", "/nodes/%s/qemu/%s/config" % (a.node, vmid),
        tags="proxcloud;pc-public;pc-img-%s;pc-v-%s" % (a.os, version),
    ))
    # Safe: customer VMs are full clones, nothing references an old template.
    pruned = prune_candidates(pve.vms(), a.os, a.node, a.keep)
    for vm in pruned:
        pve.destroy(a.node, vm)
    print(json.dumps({"os": a.os, "node": a.node, "built": True, "vmid": vmid, "version": version, "pruned": pruned}))


def keep_arg(value):
    """--keep: versions kept per OS and node, at least 1 (the newest published version is never pruned)."""
    if not re.fullmatch(r"[1-9][0-9]{0,2}", value):
        raise argparse.ArgumentTypeError("--keep must be a whole number from 1 to 999 (the newest published version always stays)")
    return int(value)


def vlan_arg(value):
    """A build-only access VLAN: 0 is untagged; 1..4094 are IEEE 802.1Q IDs."""
    if not re.fullmatch(r"0|[1-9][0-9]{0,3}", value) or int(value) > 4094:
        raise argparse.ArgumentTypeError("VLAN must be 0 (untagged) or an integer from 1 to 4094")
    return int(value)


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("command", choices=["build", "base"])
    p.add_argument("--os", required=True, choices=sorted(IMAGES))
    p.add_argument("--node", required=True)
    p.add_argument("--storage", required=True, help="where the template disk lives")
    p.add_argument("--import-storage", required=True, help="a store with `import` content for the vendor image")
    p.add_argument("--bridge", default="vmbr0")
    p.add_argument("--vlan", type=vlan_arg, default=0, help="build-guest VLAN: 0 untagged (default), 1..4094 tagged")
    p.add_argument("--build-ip", help="free CIDR on the bridge for the build and smoke VMs")
    p.add_argument("--build-gateway")
    p.add_argument("--nameserver")
    p.add_argument("--keep", type=keep_arg, default=2, help="versions kept per OS and node (at least 1)")
    p.add_argument("--if-missing", action="store_true", help="do nothing when a version exists on this node")
    a = p.parse_args()
    if a.command == "build" and not (a.build_ip and a.build_gateway):
        p.error("build needs --build-ip and --build-gateway (vendor images ship no guest agent to discover DHCP with)")
    {"build": cmd_build, "base": cmd_base}[a.command](PVE(), a, IMAGES[a.os])


if __name__ == "__main__":
    main()
