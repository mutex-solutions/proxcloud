"""No network/VM calls: static seed preparation and publication cleanup regressions."""
import contextlib
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("static_config", ROOT / "images/k8s-build-config.py")
config = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(config)


# Verbatim upstream OS metadata + shared 24.04/26.04 BIOS seed from commit
# 7ffb9b7f1f26cd66891874463cc9411e3633325f; no invented Debian preseed.
FIXTURES = ROOT / "images/tests/fixtures/image-builder-v0.1.55"


def environment(os_slug="ubuntu-2604"):
    return dict(PVE_BUILD_NETWORK_MODE="static", PVE_BUILD_VLAN="29", K8S_VERSION="1.35.10", K8S_OS=os_slug, BUILD_IP="192.0.2.40/24", BUILD_GATEWAY="192.0.2.1", BUILD_NAMESERVER="192.0.2.53 198.51.100.53", PVE_TEMPLATE_STORAGE="disk-store", PVE_ISO_STORAGE="iso-store")


def upstream(capi):
    file = capi / "packer/proxmox/packer.json.tmpl"
    file.parent.mkdir(parents=True)
    file.write_text(json.dumps({"builders": [{"network_adapters": [{"vlan_tag": "{{user `vlan_tag`}}"}], "iso_file": "{{user `iso_file`}}", "http_directory": "http-seed", "boot_command": ["http://{{ .HTTPIP }}:{{ .HTTPPort }}"]}], "variables": {"vlan_tag": "{{env `PROXMOX_VLAN`}}", "ssh_password": "$SSH_PASSWORD"}, "provisioners": [{"type": "ansible", "playbook_file": "firstboot.yml"}, {"type": "shell", "inline": ["sudo reboot"], "expect_disconnect": True}, {"type": "ansible", "pause_before": "10s", "playbook_file": "node.yml"}, {"type": "goss"}, {"type": "shell", "expect_disconnect": True, "inline": ["sudo shutdown"]}]}))
    for os_slug, image in config.K8S_IMAGES.items():
        shutil.copyfile(FIXTURES / (os_slug + ".json"), file.parent / (os_slug + ".json"))
        seed = file.parent / ("linux/ubuntu/http/" + image["release"] + "/user-data.tmpl")
        seed.parent.mkdir(parents=True)
        shutil.copyfile(FIXTURES / "ubuntu-bios-user-data.tmpl", seed)
    kubeadm = capi / config.KUBEADM_CONFIG
    kubeadm.parent.mkdir(parents=True)
    shutil.copyfile(FIXTURES / "kubeadm.yml", kubeadm)
    return file


class StaticPreparationTests(unittest.TestCase):
    def test_reviewed_fixtures_and_both_os_specific_static_seeds(self):
        expected = {"ubuntu-2404.json": "91ce2a31ec82f4ffe989b73f515430022a5cefce455b2a3ee90160797e0d696a",
                    "ubuntu-2604.json": "2364a0cf0ccebae7091100d83f45efec364772081382cbed4cb09c13e11f998e",
                    "ubuntu-bios-user-data.tmpl": "9490369d61484077180cd9576a9e45ab9d0331f34ac477a36b737f69e5a9990d"}
        for filename, checksum in expected.items():
            self.assertEqual(hashlib.sha256((FIXTURES / filename).read_bytes()).hexdigest(), checksum)
        for os_slug in ("ubuntu-2404", "ubuntu-2604"):
            with self.subTest(os=os_slug), tempfile.TemporaryDirectory() as tmp:
                run = Path(tmp); capi = run / "image-builder/images/capi"; template = upstream(capi)
                before = json.loads(template.read_text())
                # Remove the other OS seed: a selector must not fall back to it.
                other = "26.04" if os_slug == "ubuntu-2404" else "24.04"
                (capi / ("packer/proxmox/linux/ubuntu/http/" + other + "/user-data.tmpl")).unlink()
                config.prepare(capi, run, environment(os_slug))
                variables = json.loads((run / "vars.json").read_text())
                self.assertEqual(variables["artifact_name"], os_slug + "-kube-v1.35.10")
                self.assertEqual((config.K8S_BUILD_DISK_GIB, variables["disk_size"]), (8, "8G"))  # It was 20G.
                self.assertEqual(variables["iso_checksum"], config.K8S_IMAGES[os_slug]["iso_sha256"])
                self.assertEqual(variables["iso_url"], config.K8S_IMAGES[os_slug]["iso_url"])
                self.assertIn("  network:", (run / "seed/user-data").read_text())
                self.assertIn("mirror-selection:", (run / "seed/user-data").read_text())
                after = json.loads((capi / "packer/proxmox/packer.json").read_text())
                expected = before["provisioners"][:-1]
                expected[2]["pause_before"] = "90s"  # Existing reconnect grace, not a shutdown delay.
                self.assertEqual(after["provisioners"][:-1], expected)  # Reboot + goss are not waived.
                final = after["provisioners"][-1]
                self.assertEqual(final["script"], str(run / "cleanup.sh"))
                self.assertEqual(final["execute_command"], "sudo -n bash '{{.Path}}'")
                self.assertFalse(final["expect_disconnect"])
                self.assertNotIn("inline", final)
                self.assertNotIn("shutdown_command", after["builders"][0])  # Preserve plugin-owned phase.
                self.assertEqual((run / "cleanup.sh").read_text(), (ROOT / "images/k8s-build-cleanup.sh").read_text())

    def test_selector_default_and_rejected_os_or_pin_do_not_disclose_values(self):
        self.assertEqual(config.settings({"K8S_VERSION": "1.35.10"})["os"], "ubuntu-2604")
        for key, values in {"K8S_OS": ("debian-12", "debian-13", "ubuntu-2604-efi", "ubuntu-2604\n", "ubuntu-2404;BAD", "", " Ubuntu-2604"),
                            "IMAGE_BUILDER_REF": ("main", "v0.1.54", "v0.1.55\n", ""),
                            "IMAGE_BUILDER_COMMIT": ("0" * 40, "7ffb9b7f1f26cd66891874463cc9411e3633325f\n", "")}.items():
            for value in values:
                with self.subTest(key=key, value=value), self.assertRaises(ValueError) as error:
                    config.settings(environment() | {key: value})
                if value: self.assertNotIn(value, str(error.exception))

    def test_selected_seed_and_metadata_drift_are_refused(self):
        for drift in ("missing", "network", "wrong-os"):
            with self.subTest(drift=drift), tempfile.TemporaryDirectory() as tmp:
                run = Path(tmp); capi = run / "image-builder/images/capi"; upstream(capi)
                seed = capi / "packer/proxmox/linux/ubuntu/http/26.04/user-data.tmpl"
                if drift == "missing": seed.unlink()
                elif drift == "network": seed.write_text(seed.read_text() + "  network: {}\n")
                else:
                    metadata = capi / "packer/proxmox/ubuntu-2604.json"
                    metadata.write_text((FIXTURES / "ubuntu-2404.json").read_text())
                with self.assertRaisesRegex(ValueError, "Pinned Ubuntu"):
                    config.prepare(capi, run, environment())

    def test_iso_overrides_are_complete_and_os_bound(self):
        good = dict(ISO_NAME="ubuntu-26.04.1-live-server-amd64.iso", ISO_URL="https://mirror.invalid/ubuntu-26.04.1-live-server-amd64.iso", ISO_SHA256="a" * 64)
        self.assertEqual(config.settings(environment() | good)["image"]["iso_url"], good["ISO_URL"])
        for change in ({"ISO_SHA256": ""}, {"ISO_NAME": "ubuntu-24.04.4-live-server-amd64.iso"}, {"ISO_NAME": good["ISO_NAME"] + "\n"}, {"ISO_URL": "http://mirror.invalid/" + good["ISO_NAME"]}, {"ISO_URL": "https://user:BAD@mirror.invalid/" + good["ISO_NAME"]}, {"ISO_SHA256": "a" * 64 + "\n"}):
            with self.subTest(change=change), self.assertRaises(ValueError):
                config.settings(environment() | good | change)
        with self.assertRaisesRegex(ValueError, "supplied together"):
            config.settings(environment() | {"ISO_NAME": good["ISO_NAME"]})

    def test_static_seed_and_final_json_have_no_http_or_dhcp_dependency(self):
        with tempfile.TemporaryDirectory() as tmp:
            run = Path(tmp); capi = run / "image-builder/images/capi"; upstream(capi)
            config.prepare(capi, run, environment())
            doc = json.loads((capi / "packer/proxmox/packer.json").read_text()); b = doc["builders"][0]
            self.assertEqual(b["ssh_host"], "192.0.2.40")
            self.assertNotIn("http_directory", b)
            self.assertNotIn("HTTPIP", str(b))
            self.assertIn("ds=nocloud", str(b["boot_command"]))
            self.assertNotIn("nocloud-net", str(b["boot_command"]))
            iso = b["additional_iso_files"][0]
            self.assertTrue(iso["unmount"]); self.assertTrue(iso["iso_file"].startswith("iso-store:iso/pc-build-seed-"))
            net = json.loads((run / "seed/network-config").read_text())
            self.assertFalse(net["ethernets"]["build"]["dhcp4"])
            self.assertFalse(net["ethernets"]["build"]["dhcp6"])
            userdata = (run / "seed/user-data").read_text()
            self.assertIn("  network: " + json.dumps(net), userdata)
            self.assertNotIn("$ENCRYPTED_SSH_PASSWORD", userdata)
            self.assertEqual(net["ethernets"]["build"]["match"]["macaddress"], b["network_adapters"][0]["mac_address"])
            self.assertEqual([p["type"] for p in doc["provisioners"]], ["ansible", "shell", "ansible", "goss", "shell"])
            self.assertEqual(doc["provisioners"][2]["pause_before"], "90s")
            self.assertEqual(doc["provisioners"][-1]["script"], str(run / "cleanup.sh"))
            self.assertNotIn("inline", doc["provisioners"][-1])
            for path in (run / "seed").iterdir(): self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            self.assertEqual((run / "seed").stat().st_mode & 0o777, 0o700)
            self.assertEqual((capi / "packer/proxmox/packer.json").stat().st_mode & 0o777, 0o600)
            self.assertIn('version = "1.2.4"', (capi / "packer/proxmox/config.pkr.hcl").read_text())

    def test_kubeadm_config_moves_to_v1beta4_and_refuses_drift(self):
        # kubeadm 1.37 refuses kubeadm.k8s.io/v1beta3; the pinned config gets exactly upstream's v1beta4 change.
        with tempfile.TemporaryDirectory() as tmp:
            run = Path(tmp); capi = run / "image-builder/images/capi"; upstream(capi)
            config.prepare(capi, run, environment())
            text = (capi / config.KUBEADM_CONFIG).read_text()
            self.assertEqual(text, (FIXTURES / "kubeadm.yml").read_text().replace("kubeadm.k8s.io/v1beta3", "kubeadm.k8s.io/v1beta4"))
            self.assertEqual(text.count("apiVersion: kubeadm.k8s.io/v1beta4"), 2)
            self.assertEqual((capi / config.KUBEADM_CONFIG).stat().st_mode & 0o777, 0o600)
        for mutate in (lambda p: p.write_text(p.read_text() + "# drift\n"), lambda p: p.unlink(),
                       lambda p: (p.unlink(), p.symlink_to(FIXTURES / "kubeadm.yml"))):
            with tempfile.TemporaryDirectory() as tmp:
                run = Path(tmp); capi = run / "image-builder/images/capi"; upstream(capi)
                mutate(capi / config.KUBEADM_CONFIG)
                with self.assertRaises(ValueError):
                    config.prepare(capi, run, environment())

    def test_missing_and_malicious_inputs_fail_without_disclosing_values(self):
        cases = [{key: ""} for key in ("BUILD_IP", "BUILD_GATEWAY", "BUILD_NAMESERVER")]
        cases += [{"BUILD_IP": "$(touch /tmp/not-run)/24"}, {"BUILD_GATEWAY": "192.0.2.1;echo BAD"}, {"BUILD_NAMESERVER": "1.1.1.1\necho BAD"}, {"BUILD_IP": "192.0.2.0/24"}, {"BUILD_GATEWAY": "198.51.100.1"}, {"DHCP_RANGE": "192.0.2.50,192.0.2.60"}, {"K8S_VERSION": "1.35.10;bad"}]
        for change in cases:
            with self.subTest(change=change), self.assertRaises(ValueError) as error:
                config.settings(environment() | change)
            for value in change.values():
                if value and ("BAD" in value or "$(" in value): self.assertNotIn(value, str(error.exception))

    def test_private_root_required_and_upstream_drift_refused(self):
        with tempfile.TemporaryDirectory() as tmp:
            run=Path(tmp); capi=run / "image-builder/images/capi"; file=upstream(capi)
            run.chmod(0o755)
            with self.assertRaisesRegex(ValueError,"private mode0700"): config.prepare(capi,run,environment())
            run.chmod(0o700)
            doc=json.loads(file.read_text());doc["builders"][0]["network_adapters"][0]["vlan_tag"]="wrong";file.write_text(json.dumps(doc))
            with self.assertRaisesRegex(ValueError,"VLAN input"):config.prepare(capi,run,environment())

    def test_unique_seed_identity_and_dhcp_default(self):
        self.assertEqual(config.settings({"K8S_VERSION":"1.35.10"})["mode"],"dhcp")
        identities=[]
        for _ in range(2):
            with tempfile.TemporaryDirectory() as tmp:
                run=Path(tmp);capi=run/"image-builder/images/capi";upstream(capi);config.prepare(capi,run,environment())
                identities.append((run/"seed/meta-data").read_text())
        self.assertNotEqual(*identities)

    def test_cleanup_returns_without_power_or_network_changes(self):
        text=(ROOT/"images/k8s-build-cleanup.sh").read_text()
        commands = "\n".join(line for line in text.splitlines() if not line.lstrip().startswith("#"))
        for phrase in ("netplan apply", "systemctl restart", "dhclient"):
            self.assertNotIn(phrase, commands)
        self.assertNotRegex(commands, r"\b(?:shutdown|reboot|poweroff|halt)\b|\binit\s+[06]\b|/proc/sysrq-trigger")
        self.assertNotIn("expect_disconnect", commands)
        for phrase in ("set -euo pipefail", "/var/log/installer", "subiquity-disable-cloudinit-networking.cfg", "cloud-init clean", "usermod -p '!' builder", "/home/builder/.ssh", "/etc/ssh/ssh_host_", "/etc/sudoers.d/*", "[ ! -d /var/lib/cloud/seed ]", "[ \"$(getent shadow builder | cut -d: -f2)\" = '!' ]"):
            self.assertIn(phrase, text)
        self.assertEqual(commands.rstrip().splitlines()[-1], "sync")


class CheckoutTests(unittest.TestCase):
    def test_exact_reviewed_head_tag_and_pristine_tree_are_required(self):
        pin = config.IMAGE_BUILDER_COMMIT
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp); (root / ".git").mkdir()
            for head, tag, dirty in ((pin, pin, ""), ("0" * 40, pin, ""), (pin, "0" * 40, ""), (pin, pin, " M private-name"), (pin, pin, "!! ignored-file")):
                def command(args, **kwargs):
                    if args[3] == "remote": value = "https://github.com/kubernetes-sigs/image-builder"
                    elif args[3] == "ls-files": value = "H Makefile\0"
                    else:
                        value = {"--show-toplevel": str(root.resolve()), "HEAD^{commit}": head,
                                 "refs/tags/v0.1.55^{commit}": tag}.get(args[-1], dirty)
                    return subprocess.CompletedProcess(args, 0, value + ("" if args[3] == "ls-files" else "\n"), "")
                with self.subTest(head=head, tag=tag, dirty=dirty), patch.object(config.subprocess, "run", side_effect=command) as git:
                    if (head, tag, dirty) == (pin, pin, ""):
                        config.verify_checkout(root)
                    else:
                        with self.assertRaisesRegex(ValueError, "use a new WORK") as error:
                            config.verify_checkout(root)
                        self.assertNotIn("private-name", str(error.exception))
                    self.assertTrue(all(call.args[0][3] in ("remote", "rev-parse", "ls-files", "status") for call in git.call_args_list))
                    self.assertTrue(all(call.kwargs["env"]["GIT_OPTIONAL_LOCKS"] == "0" for call in git.call_args_list))
                    self.assertTrue(all(call.kwargs["env"]["GIT_NO_REPLACE_OBJECTS"] == "1" for call in git.call_args_list))

    def test_origin_and_hidden_index_flags_fail_without_disclosing_details(self):
        good_origin = "https://github.com/kubernetes-sigs/image-builder"
        cases = [(good_origin, "H Makefile\0", True), (good_origin + ".git", "H Makefile\0", True),
                 ("", "H Makefile\0", False), ("https://private.invalid/secret-origin", "H Makefile\0", False),
                 (good_origin + "\n" + good_origin + ".git", "H Makefile\0", False),
                 (good_origin + "\nprivate-secret", "H Makefile\0", False)]
        cases += [(good_origin, flag + " secret-path\0", False) for flag in ("h", "S", "s")]
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp); (root / ".git").mkdir()
            for origin, flags, allowed in cases:
                def command(args, **kwargs):
                    if args[3] == "remote": value = origin + "\n"
                    elif args[3] == "ls-files": value = flags
                    elif args[-1] == "--show-toplevel": value = str(root.resolve()) + "\n"
                    elif args[3] == "status": value = ""
                    else: value = config.IMAGE_BUILDER_COMMIT + "\n"
                    return subprocess.CompletedProcess(args, 0, value, "")
                with self.subTest(origin=origin, flags=flags), patch.object(config.subprocess, "run", side_effect=command):
                    if allowed:
                        config.verify_checkout(root)
                    else:
                        with self.assertRaisesRegex(ValueError, "use a new WORK") as error:
                            config.verify_checkout(root)
                        for value in ("secret-origin", "secret-path", "private-secret"):
                            self.assertNotIn(value, str(error.exception))

    def test_export_uses_only_pinned_object_bytes_and_modes_not_worktree(self):
        blobs = {"images/capi/Makefile": (b"100644", b"# reviewed Makefile\n"),
                 "images/capi/hack/check.sh": (b"100755", b"#!/bin/sh\nexit 0\n"),
                 "images/capi/hack/check-link": (b"120000", b"check.sh")}
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp); checkout = root / "cache"; checkout.mkdir()
            run = root / "run"; run.mkdir(mode=0o700)
            (checkout / "Makefile").write_text("UNREVIEWED\n")
            objects = {}
            tree = b""
            for name, (mode, data) in blobs.items():
                oid = hashlib.sha1(b"blob " + str(len(data)).encode() + b"\0" + data).hexdigest().encode()
                objects[oid] = data
                tree += mode + b" blob " + oid + b"\t" + name.encode() + b"\0"
            def read(checkout, *args, **kwargs):
                if args[0] == "ls-tree":
                    self.assertEqual(args[-1], config.IMAGE_BUILDER_COMMIT)
                    return tree
                self.assertEqual(args, ("cat-file", "--batch"))
                return b"".join(oid + b" blob " + str(len(objects[oid])).encode() + b"\n" + objects[oid] + b"\n" for oid in kwargs["input"].splitlines())
            with patch.object(config, "verify_checkout") as verify, patch.object(config, "git_read", side_effect=read):
                config.export_checkout(checkout, run)
                verify.assert_called_once_with(checkout)
            destination = run / "image-builder"
            self.assertEqual((destination / "images/capi/Makefile").read_bytes(), blobs["images/capi/Makefile"][1])
            self.assertEqual((destination / "images/capi/Makefile").stat().st_mode & 0o777, 0o600)
            self.assertEqual((destination / "images/capi/hack/check.sh").stat().st_mode & 0o777, 0o700)
            self.assertTrue((destination / "images/capi/hack/check-link").is_symlink())
            self.assertEqual((destination / "images/capi/hack/check-link").resolve(), destination / "images/capi/hack/check.sh")
            self.assertFalse((destination / ".git").exists())
            self.assertEqual((checkout / "Makefile").read_text(), "UNREVIEWED\n")

    def test_export_rejects_bad_hash_and_unsafe_entries(self):
        data = b"reviewed\n"
        oid = hashlib.sha1(b"blob " + str(len(data)).encode() + b"\0" + data).hexdigest().encode()
        for name, mode, corrupt in (("../escape", b"100644", False), ("/absolute", b"100644", False),
                                    (".git/config", b"100644", False), ("submodule", b"160000", False),
                                    ("Makefile", b"100644", True)):
            with self.subTest(name=name, corrupt=corrupt), tempfile.TemporaryDirectory() as tmp:
                run = Path(tmp)
                def read(checkout, *args, **kwargs):
                    if args[0] == "ls-tree": return mode + b" blob " + oid + b"\t" + name.encode() + b"\0"
                    body = b"tampered\n" if corrupt else data
                    return oid + b" blob " + str(len(body)).encode() + b"\n" + body + b"\n"
                with patch.object(config, "verify_checkout"), patch.object(config, "git_read", side_effect=read), self.assertRaisesRegex(ValueError, "object export refused"):
                    config.export_checkout("cache", run)
                self.assertFalse((run / "image-builder").exists())


class SeedCleanupTests(unittest.TestCase):
    def run_cleanup(self, behavior="ok"):
        script=(ROOT/"images/k8s-node-template.sh").read_text()
        functions=script[script.index("wait_task() {"):script.index("vmid_of() {")]+script[script.index("seed_detached() {"):script.index("finish_run() {")]
        fixture='''
set -euo pipefail
api=https://pve.invalid/api2/json
first=test-node
PVE_ISO_STORAGE=iso-store
SEED_VOLUME=iso-store:iso/pc-build-seed-123.iso
SEED_UPLOADED=1
pve() {
 case "$*" in
  *cluster/resources*)
   [ "$BEHAVIOR" != resources-fail ] || return 22
   if [ "$BEHAVIOR" = malformed ]; then printf '{"data":null}'; else printf '{"data":[{"node":"test-node","type":"qemu","vmid":9001}]}'; fi ;;
  */qemu/9001/config*)
   [ "$BEHAVIOR" != config-fail ] || return 22
   if [ "$BEHAVIOR" = attached ]; then printf '{"data":{"ide3":"iso-store:iso/pc-build-seed-123.iso,media=cdrom"}}'; else printf '{"data":{}}'; fi ;;
  *DELETE*)
   [ "$BEHAVIOR" != delete-fail ] || return 22
   printf '{"data":"UPID:test"}' ;;
  */tasks/*)
   if [ "$BEHAVIOR" = task-fail ]; then printf '{"data":{"status":"stopped","exitstatus":"ERROR"}}'; else printf '{"data":{"status":"stopped","exitstatus":"OK"}}'; fi ;;
  *content?content=iso*)
   [ "$BEHAVIOR" != list-fail ] || return 22
   if [ "$BEHAVIOR" = still-present ]; then printf '{"data":[{"volid":"iso-store:iso/pc-build-seed-123.iso"}]}'; else printf '{"data":[]}'; fi ;;
  *) return 98 ;;
 esac
}
if remove_seed; then printf 'publish-allowed'; else printf 'publication-refused'; fi
printf '\nowned=%s\n' "$SEED_UPLOADED"
'''
        return subprocess.run(["bash","-c",functions+fixture],env=dict(os.environ,BEHAVIOR=behavior),capture_output=True,text=True)

    def test_only_verified_delete_allows_publish(self):
        good=self.run_cleanup();self.assertEqual(good.returncode,0,good.stderr);self.assertIn("publish-allowed",good.stdout);self.assertIn("owned=0",good.stdout)
        for behavior in ("resources-fail","malformed","config-fail","attached","delete-fail","task-fail","list-fail","still-present"):
            with self.subTest(behavior=behavior):
                bad=self.run_cleanup(behavior);self.assertEqual(bad.returncode,0,bad.stderr);self.assertIn("publication-refused",bad.stdout);self.assertIn("owned=1",bad.stdout)


if __name__=="__main__":unittest.main()
