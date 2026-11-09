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
import ssl
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
POOL = "pc-templates"

# The vendor images. The sums file is fetched at build time and hands Proxmox the checksum to verify.
IMAGES = {
    "debian-13": {
        "url": "https://cloud.debian.org/images/cloud/trixie/latest/debian-13-genericcloud-amd64.qcow2",
        "sums": "https://cloud.debian.org/images/cloud/trixie/latest/SHA512SUMS",
        "algorithm": "sha512",
        "user": "debian",
    },
    "ubuntu-2404": {
        "url": "https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img",
        "sums": "https://cloud-images.ubuntu.com/noble/current/SHA256SUMS",
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


def create_base(pve, a, img, name):
    """A template straight from the vendor image: cloud-init drive, serial console, agent flag."""
    volume = ensure_cloud_image(pve, a, img)
    vmid = next_vmid(pve, pve.vms())
    net = "virtio,bridge=%s%s" % (a.bridge, ",tag=%s" % a.vlan if a.vlan else "")
    # Empty first: the scoped token holds VM.Config.* only through the pool, so only a VM already in the
    # pool can be configured.
    pve.wait(a.node, pve.call("POST", "/nodes/%s/qemu" % a.node, vmid=vmid, pool=POOL))
    pve.wait(a.node, pve.call(
        "POST", "/nodes/%s/qemu/%s/config" % (a.node, vmid), name=name, ostype="l26", cores=2, memory=2048,
        cpu="host", agent=1, scsihw="virtio-scsi-single", serial0="socket", vga="serial0", net0=net, tags="proxcloud;pc-base",
        scsi0="%s:0,discard=on,import-from=%s" % (a.storage, volume), ide2="%s:cloudinit" % a.storage, boot="order=scsi0",
    ))
    pve.wait(a.node, pve.call("POST", "/nodes/%s/qemu/%s/template" % (a.node, vmid)))
    return vmid


def cmd_base(pve, a, img):
    name = "pc-base-%s-%s" % (a.os, a.node)
    for v in pve.vms():
        if v.get("name") == name and v.get("template") == 1:
            print(v["vmid"])
            return
    print(create_base(pve, a, img, name))


def versions_of(vms, os_slug, node):
    """(version, vmid) of this image's published templates on node, oldest first. A template without a
    pc-v-* tag was placed by hand: it is not ours to prune, so it is not listed."""
    out = []
    for v in vms:
        tags = str(v.get("tags", "")).split(";")
        if v.get("template") == 1 and v.get("node") == node and v.get("pool") == POOL and {"pc-public", "pc-img-" + os_slug} <= set(tags):
            ver = [t[len("pc-v-"):] for t in tags if t.startswith("pc-v-")]
            if ver:
                out.append((ver[0], v["vmid"]))
    return sorted(out)


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
    # Published only now: the portal never sees an untested template.
    pve.wait(a.node, pve.call(
        "POST", "/nodes/%s/qemu/%s/config" % (a.node, vmid),
        tags="proxcloud;pc-public;pc-img-%s;pc-v-%s" % (a.os, version),
    ))
    # Safe: customer VMs are full clones, nothing references an old template.
    pruned = [vm for _, vm in versions_of(pve.vms(), a.os, a.node)[:-a.keep]]
    for vm in pruned:
        pve.destroy(a.node, vm)
    print(json.dumps({"os": a.os, "node": a.node, "built": True, "vmid": vmid, "version": version, "pruned": pruned}))


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("command", choices=["build", "base"])
    p.add_argument("--os", required=True, choices=sorted(IMAGES))
    p.add_argument("--node", required=True)
    p.add_argument("--storage", required=True, help="where the template disk lives")
    p.add_argument("--import-storage", required=True, help="a store with `import` content for the vendor image")
    p.add_argument("--bridge", default="vmbr0")
    p.add_argument("--vlan", type=int, default=0)
    p.add_argument("--build-ip", help="free CIDR on the bridge for the build and smoke VMs")
    p.add_argument("--build-gateway")
    p.add_argument("--nameserver")
    p.add_argument("--keep", type=int, default=2, help="versions kept per OS and node")
    p.add_argument("--if-missing", action="store_true", help="do nothing when a version exists on this node")
    a = p.parse_args()
    if a.command == "build" and not (a.build_ip and a.build_gateway):
        p.error("build needs --build-ip and --build-gateway (vendor images ship no guest agent to discover DHCP with)")
    {"build": cmd_build, "base": cmd_base}[a.command](PVE(), a, IMAGES[a.os])


if __name__ == "__main__":
    main()
