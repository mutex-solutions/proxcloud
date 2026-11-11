#!/usr/bin/env python3
"""Prepare pinned image-builder in a private run tree; never contacts PVE or prints secrets."""
import argparse
import hashlib
import io
import ipaddress
import json
import os
from pathlib import Path, PurePosixPath
import re
import secrets
import subprocess
import sys
import uuid
from urllib.parse import urlsplit


# Reviewed together: v0.1.55 peeled to this commit, not a caller-supplied SHA.
IMAGE_BUILDER_REF = "v0.1.55"
IMAGE_BUILDER_COMMIT = "7ffb9b7f1f26cd66891874463cc9411e3633325f"
K8S_IMAGES = {
    "ubuntu-2404": {
        "release": "24.04",
        "iso_name": "ubuntu-24.04.4-live-server-amd64.iso",
        "iso_url": "https://releases.ubuntu.com/noble/ubuntu-24.04.4-live-server-amd64.iso",
        "iso_sha256": "e907d92eeec9df64163a7e454cbc8d7755e8ddc7ed42f99dbc80c40f1a138433",
    },
    "ubuntu-2604": {
        "release": "26.04",
        "iso_name": "ubuntu-26.04.1-live-server-amd64.iso",
        "iso_url": "https://releases.ubuntu.com/26.04/ubuntu-26.04.1-live-server-amd64.iso",
        "iso_sha256": "cc8a95cde20f6ced61a322420de00f10cc3c90ced545daa46cb9c1a117f1d927",
    },
}


def fail(message):
    raise ValueError(message)


def settings(env):
    os_slug = env.get("K8S_OS", "ubuntu-2604")
    if os_slug not in K8S_IMAGES:
        fail("K8S_OS must be ubuntu-2404 or ubuntu-2604; Debian Kubernetes targets are not supported")
    for key, expected in (("IMAGE_BUILDER_REF", IMAGE_BUILDER_REF), ("IMAGE_BUILDER_COMMIT", IMAGE_BUILDER_COMMIT)):
        if key in env and env[key] != expected:
            fail(key + " must match the reviewed image-builder pin")
    image = dict(K8S_IMAGES[os_slug])
    # A custom/mirrored installer must be a complete, checksummed OS-specific tuple.
    iso_keys = ("ISO_NAME", "ISO_URL", "ISO_SHA256")
    if any(key in env for key in iso_keys):
        if not all(env.get(key) for key in iso_keys):
            fail("ISO_NAME, ISO_URL and ISO_SHA256 overrides must be supplied together")
        name, url, checksum = (env[key] for key in iso_keys)
        if not re.fullmatch(r"ubuntu-" + re.escape(image["release"]) + r"(?:\.[0-9]+)?-live-server-amd64\.iso", name):
            fail("ISO_NAME must match the selected Ubuntu release and amd64 live-server image")
        try:
            parsed = urlsplit(url)
            valid_url = parsed.scheme == "https" and parsed.hostname and not parsed.username and not parsed.password and not parsed.fragment
        except ValueError:
            valid_url = False
        if not valid_url or re.search(r"\s", url) or not parsed.path.endswith("/" + name):
            fail("ISO_URL must be a credential-free HTTPS URL for ISO_NAME")
        if not re.fullmatch(r"[a-fA-F0-9]{64}", checksum):
            fail("ISO_SHA256 must be a 64-digit hexadecimal checksum")
        image.update(iso_name=name, iso_url=url, iso_sha256=checksum.lower())
    mode = env.get("PVE_BUILD_NETWORK_MODE", "dhcp")
    if mode not in ("static", "dhcp"):
        fail("PVE_BUILD_NETWORK_MODE must be static or dhcp")
    version = env.get("K8S_VERSION", "")
    if not re.fullmatch(r"[1-9][0-9]*\.[0-9]+\.[0-9]+", version):
        fail("K8S_VERSION must be a numeric major.minor.patch version")
    vlan = env.get("PVE_BUILD_VLAN") or "0"
    if not re.fullmatch(r"0|[1-9][0-9]{0,3}", vlan) or int(vlan) > 4094:
        fail("PVE_BUILD_VLAN must be 0 (untagged) or an integer from 1 to 4094")
    out = {"mode": mode, "version": version, "vlan": int(vlan), "os": os_slug, "image": image}
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


def git_read(checkout, *args, input=None, text=True):
    # Ignore process-level repository/config redirects and replacement objects.
    # Optional locks and fsmonitor are disabled; these are object/metadata reads.
    env = {key: value for key, value in os.environ.items() if not key.startswith("GIT_")}
    env.update(GIT_OPTIONAL_LOCKS="0", GIT_NO_REPLACE_OBJECTS="1", GIT_CONFIG_COUNT="1",
               GIT_CONFIG_KEY_0="core.fsmonitor", GIT_CONFIG_VALUE_0="false")
    return subprocess.run(["git", "-C", str(checkout), *args], env=env, input=input,
                          text=text, capture_output=True, check=True).stdout


def verify_checkout(checkout):
    """Fail closed on dirty, hidden-index, wrong-pin or non-upstream caches."""
    checkout = Path(checkout)
    message = "image-builder cache must be clean, unhidden and from the approved origin at the reviewed commit; use a new WORK directory (cache was not modified)"
    if checkout.is_symlink() or not (checkout / ".git").is_dir() or (checkout / ".git").is_symlink():
        fail(message)
    try:
        origins = git_read(checkout, "remote", "get-url", "--all", "origin").splitlines()
        if len(origins) != 1 or origins[0] not in ("https://github.com/kubernetes-sigs/image-builder", "https://github.com/kubernetes-sigs/image-builder.git"):
            fail(message)
        if git_read(checkout, "rev-parse", "--show-toplevel").rstrip("\n") != str(checkout.resolve()):
            fail(message)
        if git_read(checkout, "rev-parse", "--verify", "HEAD^{commit}").rstrip("\n") != IMAGE_BUILDER_COMMIT:
            fail(message)
        if git_read(checkout, "rev-parse", "--verify", "refs/tags/" + IMAGE_BUILDER_REF + "^{commit}").rstrip("\n") != IMAGE_BUILDER_COMMIT:
            fail(message)
        # Lowercase marks assume-unchanged; S marks skip-worktree. Neither is
        # evidence of pristine bytes, even when git status reports no changes.
        flags = git_read(checkout, "ls-files", "-v", "-z").split("\0")
        if any(entry and not entry.startswith("H ") for entry in flags):
            fail(message)
        # Include ignored generated files; never adopt a previously adapted tree.
        if git_read(checkout, "status", "--porcelain", "--untracked-files=all", "--ignored").strip():
            fail(message)
    except (OSError, subprocess.CalledProcessError, UnicodeError):
        fail(message)


def export_checkout(checkout, run):
    """Export pinned object bytes, never the cached worktree, filters or .git."""
    verify_checkout(checkout)
    run = Path(run).resolve()
    destination = run / "image-builder"
    if run.stat().st_mode & 0o077 or destination.exists() or destination.is_symlink():
        fail("Export requires a private mode0700 run tree and an absent image-builder destination")
    message = "Pinned image-builder object export refused; no cached working-tree files were copied"
    try:
        entries = []
        seen = set()
        for row in git_read(checkout, "ls-tree", "-r", "-z", IMAGE_BUILDER_COMMIT, text=False).split(b"\0"):
            if not row:
                continue
            header, raw_name = row.split(b"\t", 1)
            mode, kind, oid = header.split(b" ")
            name = raw_name.decode("utf-8")
            if (kind != b"blob" or mode not in (b"100644", b"100755", b"120000")
                    or not re.fullmatch(rb"[0-9a-f]{40}", oid)
                    or any(part in ("", ".", "..", ".git") for part in name.split("/"))
                    or name in seen):
                fail(message)
            seen.add(name)
            entries.append((name, mode, oid))
        if not entries:
            fail(message)
        # A file/symlink cannot be another entry's parent. Links are created last.
        for name, _, _ in entries:
            if any(str(parent) in seen for parent in PurePosixPath(name).parents if str(parent) != "."):
                fail(message)
        oids = sorted({oid for _, _, oid in entries})
        stream = io.BytesIO(git_read(checkout, "cat-file", "--batch", input=b"\n".join(oids) + b"\n", text=False))
        blobs = {}
        for oid in oids:
            actual, kind, raw_size = stream.readline().rstrip(b"\n").split(b" ")
            size = int(raw_size)
            data = stream.read(size)
            if (actual != oid or kind != b"blob" or size < 0 or len(data) != size
                    or stream.read(1) != b"\n"
                    or hashlib.sha1(b"blob " + str(size).encode() + b"\0" + data).hexdigest().encode() != oid):
                fail(message)
            blobs[oid] = data
        if stream.read(1):
            fail(message)
        destination.mkdir(mode=0o700)
        links = []
        for name, mode, oid in entries:
            path = destination.joinpath(*PurePosixPath(name).parts)
            path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
            if mode == b"120000":
                target = blobs[oid].decode("utf-8")
                if not target or PurePosixPath(target).is_absolute() or "\0" in target:
                    fail(message)
                links.append((path, target))
            else:
                with path.open("xb") as file:
                    file.write(blobs[oid])
                path.chmod(0o700 if mode == b"100755" else 0o600)
        for path, target in links:
            path.symlink_to(target)
        for path, _ in links:
            if not path.resolve().is_relative_to(destination):
                fail(message)
    except (OSError, ValueError, subprocess.CalledProcessError, UnicodeError, RuntimeError):
        fail(message)


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
    # Select an exact reviewed path; do not fall back to another OS's seed/target.
    metadata_path = capi / ("packer/proxmox/" + config["os"] + ".json")
    metadata = json.loads(metadata_path.read_text())
    release = config["image"]["release"]
    if (metadata.get("build_name") != config["os"] or metadata.get("distro_name") != "ubuntu"
            or metadata.get("distribution_version") != config["os"].split("-", 1)[1]
            or metadata.get("version") != release or "/" + release + "/" not in metadata.get("boot_command_prefix", "")):
        fail("Pinned Ubuntu OS metadata contract changed")
    userdata_template = capi / ("packer/proxmox/linux/ubuntu/http/" + release + "/user-data.tmpl")
    if userdata_template.is_symlink() or not userdata_template.is_file():
        fail("Pinned Ubuntu autoinstall seed is missing or unsafe")
    userdata = userdata_template.read_text()
    if not userdata.startswith("#cloud-config\nautoinstall:\n") or "  network:" in userdata:
        fail("Pinned Ubuntu autoinstall seed contract changed")
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
    vars = {"kubernetes_semver": "v" + config["version"], "kubernetes_series": "v" + config["version"].rsplit(".", 1)[0], "kubernetes_deb_version": config["version"] + "-1.1", "kubernetes_rpm_version": config["version"], "disk_format": "raw", "cores": "2", "sockets": "2", "memory": "4096", "disk_size": "20G", "cpu_type": "host",
            "artifact_name": config["os"] + "-kube-v" + config["version"],
            "iso_url": config["image"]["iso_url"], "iso_checksum_type": "sha256", "iso_checksum": config["image"]["iso_sha256"]}
    if config["mode"] == "static":
        seed = run / "seed"
        seed.mkdir(mode=0o700)
        mac = "02:" + ":".join(f"{byte:02x}" for byte in secrets.token_bytes(5))
        net = network(config, mac)
        userdata = userdata_template.with_suffix("").read_text()
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
    parser.add_argument("command", choices=("check", "select", "verify-checkout", "export-checkout", "prepare"))
    parser.add_argument("--checkout")
    parser.add_argument("--capi")
    parser.add_argument("--run")
    args = parser.parse_args()
    try:
        if args.command in ("check", "select"):
            config = settings(os.environ)
            if args.command == "select":
                for value in (config["os"], config["image"]["iso_name"], config["image"]["iso_url"], config["image"]["iso_sha256"], IMAGE_BUILDER_REF, IMAGE_BUILDER_COMMIT):
                    print(value)
        elif args.command in ("verify-checkout", "export-checkout"):
            settings(os.environ)
            if not args.checkout:
                fail("Checkout verification/export requires a checkout path")
            if args.command == "verify-checkout":
                verify_checkout(args.checkout)
            else:
                if not args.run:
                    fail("export-checkout requires a private run path")
                export_checkout(args.checkout, args.run)
        else:
            if not args.capi or not args.run:
                fail("prepare requires private run and capi paths")
            prepare(args.capi, args.run, os.environ)
    except OSError:
        print("Build configuration refused: required private/pinned build file unavailable", file=sys.stderr)
        raise SystemExit(1)
    except (ValueError, KeyError) as error:
        # Error strings describe keys/contracts, never echo operator-provided values.
        print("Build configuration refused: " + str(error), file=sys.stderr)
        raise SystemExit(1)


if __name__ == "__main__":
    main()
