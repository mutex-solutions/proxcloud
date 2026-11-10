#!/usr/bin/env python3
"""Prepare pinned image-builder in a private run tree; never contacts PVE or prints secrets."""
import argparse
import copy
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import sys
import uuid


def fail(message):
    raise ValueError(message)


def settings(env):
    mode = env.get("PVE_BUILD_NETWORK_MODE", "dhcp")
    if mode not in ("static", "dhcp"):
        fail("PVE_BUILD_NETWORK_MODE must be static or dhcp")
    version = env.get("K8S_VERSION", "")
    if not re.fullmatch(r"[1-9][0-9]*\.[0-9]+\.[0-9]+", version):
        fail("K8S_VERSION must be a numeric major.minor.patch version")
    vlan = env.get("PVE_BUILD_VLAN") or "0"
    if not re.fullmatch(r"0|[1-9][0-9]{0,3}", vlan) or int(vlan) > 4094:
        fail("PVE_BUILD_VLAN must be 0 (untagged) or an integer from 1 to 4094")
    out = {"mode": mode, "version": version, "vlan": int(vlan)}
    if mode == "static":
        if env.get("DHCP_RANGE", ""):
            fail("Static build mode refuses DHCP_RANGE; no DHCP service may be started")
        for key in ("BUILD_IP", "BUILD_GATEWAY", "BUILD_NAMESERVER"):
            if not env.get(key, "").strip():
                fail("Missing required static build setting: " + key)
        try:
            raw = env["BUILD_IP"]
            if "/" not in raw or raw.strip() != raw:
                fail("BUILD_IP must be an IPv4 host CIDR")
            address = ipaddress.IPv4Interface(raw)
            gateway = ipaddress.IPv4Address(env["BUILD_GATEWAY"])
            dns_raw = env["BUILD_NAMESERVER"]
            if not re.fullmatch(r"[0-9.]+(?: [0-9.]+){0,2}", dns_raw):
                fail("BUILD_NAMESERVER must contain one to three space-separated IPv4 addresses")
            dns = [ipaddress.IPv4Address(value) for value in dns_raw.split(" ")]
            if address.ip in (address.network.network_address, address.network.broadcast_address) or address.network.prefixlen > 30:
                fail("BUILD_IP must be a usable IPv4 host CIDR with a gateway")
            if gateway not in address.network or gateway in (address.ip, address.network.network_address, address.network.broadcast_address):
                fail("BUILD_GATEWAY must be another usable host in the build subnet")
            if any(ip.is_multicast or ip.is_unspecified or ip.is_loopback for ip in [address.ip, gateway] + dns):
                fail("Build network addresses must be unicast, non-loopback IPv4 addresses")
        except ipaddress.AddressValueError:
            fail("Invalid IPv4 static build setting")
        except ipaddress.NetmaskValueError:
            fail("Invalid BUILD_IP prefix")
        out.update(ip=str(address), gateway=str(gateway), dns=[str(ip) for ip in dns])
    return out


def private_write(path, text):
    path = Path(path)
    if path.is_symlink():
        fail("Refusing symlink in private build preparation")
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    with open(path, "w", encoding="utf-8") as stream:
        stream.write(text)
    path.chmod(0o600)


def network(config, mac):
    return {"version": 2, "ethernets": {"build": {"match": {"macaddress": mac}, "set-name": "build0", "dhcp4": False, "dhcp6": False, "accept-ra": False, "addresses": [config["ip"]], "routes": [{"to": "default", "via": config["gateway"]}], "nameservers": {"addresses": config["dns"]}}}}


def prepare(capi, run, env):
    config = settings(env)
    capi, run = Path(capi).resolve(), Path(run).resolve()
    if not capi.is_relative_to(run) or run.stat().st_mode & 0o077:
        fail("Preparation requires a private mode0700 per-run tree")
    path = capi / "packer/proxmox/packer.json.tmpl"
    doc = json.loads(path.read_text())
    b = doc["builders"][0]
    if (b["network_adapters"][0].get("vlan_tag") != "{{user `vlan_tag`}}" or doc["variables"].get("vlan_tag") != "{{env `PROXMOX_VLAN`}}"):
        fail("image-builder template does not expose the pinned VLAN input")
    b["pool"] = env.get("TEMPLATE_POOL", "pc-templates")
    if "boot_iso" not in b:
        b["boot_iso"] = {"iso_file": b.pop("iso_file"), "unmount": True}
        for key in ("iso_url", "iso_checksum", "iso_storage_pool", "unmount_iso"):
            b.pop(key, None)
    for provisioner in doc["provisioners"]:
        if provisioner.get("pause_before") == "10s":
            provisioner["pause_before"] = "90s"
    password = secrets.token_urlsafe(32)
    encrypted = subprocess.run(["openssl", "passwd", "-6", "-stdin"], input=password + "\n", text=True, capture_output=True, check=True).stdout.strip()
    # Upstream's set-ssh-password target tees rendered passwords into public logs.
    # Render all templates ourselves; make is invoked with that target explicitly skipped.
    for template in (capi / "packer").rglob("*.tmpl"):
        if template.is_symlink():
            fail("Refusing symlink in pinned template tree")
        content = template.read_text().replace("$ENCRYPTED_SSH_PASSWORD", encrypted).replace("$SSH_PASSWORD", password)
        private_write(template.with_suffix(""), content)
    doc = json.loads(json.dumps(doc).replace("$ENCRYPTED_SSH_PASSWORD", encrypted).replace("$SSH_PASSWORD", password))
    b = doc["builders"][0]
    vars = {"kubernetes_semver": "v" + config["version"], "kubernetes_series": "v" + config["version"].rsplit(".", 1)[0], "kubernetes_deb_version": config["version"] + "-1.1", "kubernetes_rpm_version": config["version"], "disk_format": "raw", "cores": "2", "sockets": "2", "memory": "4096", "disk_size": "20G", "cpu_type": "host"}
    if config["mode"] == "static":
        seed = run / "seed"
        seed.mkdir(mode=0o700)
        mac = "02:" + ":".join(f"{byte:02x}" for byte in secrets.token_bytes(5))
        net = network(config, mac)
        userdata_path = capi / "packer/proxmox/linux/ubuntu/http/24.04/user-data"
        userdata = userdata_path.read_text()
        if not userdata.startswith("#cloud-config\nautoinstall:\n") or "  network:" in userdata:
            fail("Pinned Ubuntu autoinstall seed contract changed")
        # JSON is valid YAML. The same Netplan mapping reaches the live ISO before
        # any HTTP dependency and the installed OS across its provisioning reboot.
        userdata = userdata.replace("autoinstall:\n", "autoinstall:\n  network: " + json.dumps(net) + "\n", 1)
        private_write(seed / "user-data", userdata)
        private_write(seed / "meta-data", json.dumps({"instance-id": "pc-build-" + str(uuid.uuid4()), "local-hostname": "pc-build"}) + "\n")
        private_write(seed / "network-config", json.dumps(net) + "\n")
        iso_name = "pc-build-seed-" + uuid.uuid4().hex + ".iso"
        iso_storage = env.get("PVE_ISO_STORAGE") or env["PVE_TEMPLATE_STORAGE"]
        private_write(run / "seed-volume", iso_storage + ":iso/" + iso_name + "\n")
        private_write(run / "seed-name", iso_name + "\n")
        # Attach local NoCloud media. No runner inbound HTTP port or NAT address is needed.
        b.pop("http_directory", None)
        b["boot_command"] = ["c<wait>linux /casper/vmlinuz autoinstall ds=nocloud ---<enter><wait10s>initrd /casper/initrd<enter><wait10s>boot<enter><wait10s>"]
        b["ssh_host"] = str(ipaddress.ip_interface(config["ip"]).ip)
        b["network_adapters"][0].update(model="virtio", mac_address=mac)
        b["additional_iso_files"] = [{"type": "ide", "index": "3", "iso_file": iso_storage + ":iso/" + iso_name, "unmount": True}]
        # Reuse a fresh PVE cloud-init drive on tenant boot, never the build CIDATA.
        b["cloud_init"] = True
        b["cloud_init_storage_pool"] = "{{user `storage_pool`}}"
        b["cloud_init_disable_upgrade_packages"] = True
        final = doc["provisioners"][-1]
        if final.get("type") != "shell" or not final.get("expect_disconnect") or not any("shutdown" in line for line in final.get("inline", [])):
            fail("Pinned final shutdown provisioner contract changed")
        # Final goss/reboot checks remain untouched. One root session cleans secrets
        # then shuts down; removing builder sudo cannot break a later sudo command.
        final.pop("inline", None)
        final.pop("inline_shebang", None)
        cleanup = Path(__file__).with_name("k8s-build-cleanup.sh").read_text()
        private_write(run / "cleanup.sh", cleanup)
        final["script"] = str(run / "cleanup.sh")
        final["execute_command"] = "sudo -n bash '{{.Path}}'"
    # The adaptation is validated against this plugin API, not whatever latest
    # version happens to satisfy upstream's open-ended >= constraint.
    private_write(capi / "packer/proxmox/config.pkr.hcl", 'packer {\n  required_plugins {\n    proxmox = {\n      version = "1.2.4"\n      source = "github.com/hashicorp/proxmox"\n    }\n  }\n}\n')
    private_write(capi / "packer/proxmox/packer.json", json.dumps(doc, indent=2) + "\n")
    private_write(run / "vars.json", json.dumps(vars) + "\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("check", "prepare"))
    parser.add_argument("--capi")
    parser.add_argument("--run")
    args = parser.parse_args()
    try:
        if args.command == "check":
            settings(os.environ)
        else:
            if not args.capi or not args.run:
                fail("prepare requires private run and capi paths")
            prepare(args.capi, args.run, os.environ)
    except (ValueError, KeyError) as error:
        # Error strings describe keys/contracts, never echo operator-provided values.
        print("Build configuration refused: " + str(error), file=sys.stderr)
        raise SystemExit(1)


if __name__ == "__main__":
    main()
