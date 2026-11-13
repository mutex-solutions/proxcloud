#!/usr/bin/env python3
"""Offline regressions: no PVE, network, Packer binary or credentials are used.

Run: python3 -m unittest discover -s images/tests -v
"""
import argparse
import contextlib
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import textwrap
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("image_build", ROOT / "images/build.py")
build = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(build)


def workflow_run(name, step):
    text = (ROOT / ".github/workflows" / name).read_text()
    block = text.split("- name: " + step + "\n", 1)[1].split("        run: |\n", 1)[1]
    lines = []
    for line in block.splitlines():
        if line.strip() and not line.startswith("          "):
            break
        lines.append(line)
    return textwrap.dedent("\n".join(lines))


def arguments(vlan=29):
    return argparse.Namespace(os="debian-13", node="test-node", storage="disk-store", import_storage="import-store",
                              bridge="test-bridge", vlan=vlan, build_ip="192.0.2.40/24", build_gateway="192.0.2.1",
                              nameserver="", keep=2, if_missing=False)


class FakePVE:
    base, auth, ctx = "https://pve.invalid/api2/json", "fake", None

    def __init__(self, fail_cleanup=False, fail_smoke=False, stale_cleanup=False):
        self.rows, self.events = {}, []
        self.fail_cleanup, self.fail_smoke, self.stale_cleanup = fail_cleanup, fail_smoke, stale_cleanup

    def vms(self):
        return [dict(value, vmid=key) for key, value in self.rows.items()]

    def wait(self, node, task):
        self.events.append(("wait", task))
        if task == "cleanup" and self.fail_cleanup:
            raise SystemExit("cleanup task failed")

    def destroy(self, node, vmid):
        self.events.append(("destroy", vmid))
        self.rows.pop(vmid, None)

    def call(self, method, path, **params):
        self.events.append((method, path, params.copy()))
        vm = re.search(r"/qemu/(\d+)", path)
        vmid = int(vm[1]) if vm else params.get("vmid")
        if path == "/pools/" + build.POOL:
            return {"members": [dict(value, vmid=key, type="qemu") for key, value in self.rows.items() if value.get("pool") == build.POOL]}
        if path.endswith("/qemu"):
            self.rows[vmid] = {"node": "test-node", "pool": build.POOL}
        elif path.endswith("/status/current"):
            return {"status": "stopped"}
        elif path.endswith("/resize"):
            assert method == "PUT" and params == {"disk": "scsi0", "size": "20G", "digest": "fake-digest"}
            self.rows[vmid]["scsi0"] = "disk-store:vm-%s-disk-0,discard=on,size=20G" % vmid
            return None  # synchronous PVE response; wait(None) is invalid
        elif path.endswith("/clone"):
            self.rows[params["newid"]] = dict(self.rows[vmid])
        elif path.endswith("/config"):
            if method == "GET":
                return dict(self.rows[vmid], digest="fake-digest")
            if params.get("delete") == "net0":
                if not self.fail_cleanup and not self.stale_cleanup:
                    self.rows[vmid].pop("net0", None)
                return "cleanup"
            row = self.rows[vmid]
            row.update({key: value for key, value in params.items() if key != "delete"})
            if "import-from=" in params.get("scsi0", ""):
                row["scsi0"] = "disk-store:vm-%s-disk-0,discard=on,size=3G" % vmid
            for key in params.get("delete", "").split(","):
                row.pop(key, None)
            if "pc-public" in params.get("tags", ""):
                assert "net0" not in row, "publication inherited build NIC"
        elif path.endswith("/status/start"):
            row = self.rows[vmid]
            assert row["net0"] == "virtio,bridge=test-bridge,tag=29", "smoke must keep build VLAN"
            self.events.append(("smoke-start", vmid))
            if self.fail_smoke:
                raise SystemExit("smoke boot failed")
        elif path.endswith("/template"):
            self.rows[vmid]["template"] = 1
        return "ok"


class BuildTests(unittest.TestCase):
    def test_os_catalog_urls_checksums_and_packer_users(self):
        expected = {
            "debian-13": ("https://cloud.debian.org/images/cloud/trixie/latest/debian-13-genericcloud-amd64.qcow2", "sha512", "debian"),
            "debian-12": ("https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2", "sha512", "debian"),
            "ubuntu-2404": ("https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img", "sha256", "ubuntu"),
            "ubuntu-2604": ("https://cloud-images.ubuntu.com/releases/resolute/release-20261016/ubuntu-26.04-server-cloudimg-amd64.img", "sha256", "ubuntu"),
        }
        self.assertEqual(set(build.IMAGES), set(expected))
        for os_slug, (url, algorithm, user) in expected.items():
            image = build.IMAGES[os_slug]
            self.assertEqual((image["url"], image["algorithm"], image["user"]), (url, algorithm, user))
            self.assertEqual(image["sums"], url.rsplit("/", 1)[0] + "/" + algorithm.upper() + "SUMS")
            variables = ROOT / ("images/packer/os/" + os_slug + ".pkrvars.hcl")
            self.assertEqual(variables.read_text(), 'ssh_username = "' + user + '"\n')
        defaults = (ROOT / "installer/playbooks/group_vars/all/defaults.yml").read_text()
        self.assertIn("os: [debian-13, ubuntu-2404]", defaults)

    def run_build(self, pve):
        a = arguments()
        def packer_stub(args, base, vmid, name, version):
            self.assertEqual(args.vlan, 29)
            pve.rows[vmid] = dict(pve.rows[base], name=name, tags="pc-building", template=1)
            pve.events.append(("packer", vmid))
        response = contextlib.closing(io.BytesIO(json.dumps({"data": {"ip": "192.0.2.40"}}).encode()))
        with patch.object(build, "ensure_cloud_image", return_value="import-store:import/source.qcow2"), \
             patch.object(build, "packer", side_effect=packer_stub), \
             patch.object(build.urllib.request, "urlopen", return_value=response), \
             contextlib.redirect_stdout(io.StringIO()):
            build.cmd_build(pve, a, build.IMAGES[a.os])

    def test_smoke_keeps_vlan_then_cleanup_precedes_publication(self):
        pve = FakePVE()
        self.run_build(pve)
        smoke = next(i for i, e in enumerate(pve.events) if e[0] == "smoke-start")
        clean = next(i for i, e in enumerate(pve.events) if e[0] == "POST" and e[2].get("delete") == "net0")
        publish = next(i for i, e in enumerate(pve.events) if e[0] == "POST" and "pc-public" in e[2].get("tags", ""))
        self.assertLess(smoke, clean)
        self.assertLess(clean, publish)
        self.assertNotIn("net0", pve.rows[9201])

    def test_failed_cleanup_task_prevents_publication(self):
        pve = FakePVE(fail_cleanup=True)
        with self.assertRaisesRegex(SystemExit, "cleanup task failed"):
            self.run_build(pve)
        self.assertFalse(any(e[0] == "POST" and "pc-public" in e[2].get("tags", "") for e in pve.events))

    def test_successful_cleanup_task_with_stale_nic_prevents_publication(self):
        pve = FakePVE(stale_cleanup=True)
        with self.assertRaisesRegex(SystemExit, "build NIC remains"):
            self.run_build(pve)
        self.assertFalse(any(e[0] == "POST" and "pc-public" in e[2].get("tags", "") for e in pve.events))

    def test_failed_smoke_prevents_cleanup_and_publication(self):
        pve = FakePVE(fail_smoke=True)
        with self.assertRaisesRegex(SystemExit, "smoke boot failed"):
            self.run_build(pve)
        self.assertFalse(any(e[0] == "POST" and (e[2].get("delete") == "net0" or "pc-public" in e[2].get("tags", "")) for e in pve.events))

    def test_prune_never_takes_a_portal_import_or_the_only_version(self):
        def row(vmid, version, imported=False, public=True, legacy=False):
            tags = ["proxcloud", "pc-img-debian-13", "pc-v-" + version] + (["pc-public"] if public else [])
            if imported:
                tags += ["pc-tu-import", "pc-src-aaaaaaaaaaaa"]
            if legacy:
                tags += ["pc-src-bbbbbbbbbbbb"]  # imported before the marker existed
            return {"vmid": vmid, "template": 1, "node": "test-node", "pool": build.POOL, "tags": ";".join(tags)}
        vms = [row(9201, "202601010000"), row(9202, "202602010000", imported=True), row(9203, "202603010000"),
               row(9204, "202604010000"), row(9205, "202605010000", imported=True, public=False),
               row(9206, "202601150000", legacy=True)]
        # An import still counts as an existing version for --if-missing ...
        self.assertIn(("202602010000", 9202), build.versions_of(vms, "debian-13", "test-node"))
        self.assertIn(("202601150000", 9206), build.versions_of(vms, "debian-13", "test-node"))
        # ... but is never a prune candidate (marker or legacy pc-src-) and takes no --keep slot.
        self.assertEqual(build.prune_candidates(vms, "debian-13", "test-node", 2), [9201])
        self.assertEqual(build.prune_candidates(vms, "debian-13", "test-node", 1), [9201, 9203])
        # The only published version is never pruned; keep < 1 is refused, never "prune everything".
        only = [row(9301, "202601010000")]
        self.assertEqual(build.prune_candidates(only, "debian-13", "test-node", 1), [])
        for keep in (0, -1):
            with self.assertRaises(SystemExit):
                build.prune_candidates(vms, "debian-13", "test-node", keep)

    def test_keep_argument_rejects_below_one(self):
        for value in ("1", "2", "999"):
            self.assertEqual(build.keep_arg(value), int(value))
        for value in ("0", "-1", "", "01", "1000", "two", " 2"):
            with self.subTest(value=value), self.assertRaises(argparse.ArgumentTypeError):
                build.keep_arg(value)
        with patch.object(build.sys, "argv", ["build.py", "build", "--os", "debian-13", "--node", "n", "--storage", "s", "--import-storage", "i",
                                               "--build-ip", "192.0.2.40/24", "--build-gateway", "192.0.2.1", "--keep", "0"]), \
             patch.object(build, "PVE") as api, contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit):
                build.main()
            api.assert_not_called()

    def test_build_prune_leaves_imports(self):
        pve = FakePVE()
        pve.rows[9300] = {"node": "test-node", "pool": build.POOL, "template": 1, "name": "pc-ghcr-debian-13-os-20260101000000",
                          "tags": "proxcloud;pc-public;pc-img-debian-13;pc-v-202601010000;pc-tu-import;pc-src-aaaaaaaaaaaa"}
        pve.rows[9301] = {"node": "test-node", "pool": build.POOL, "template": 1, "name": "pc-debian-13-202601020000",
                          "tags": "proxcloud;pc-public;pc-img-debian-13;pc-v-202601020000"}
        a = arguments()
        a.keep = 1
        def packer_stub(args, base, vmid, name, version):
            pve.rows[vmid] = dict(pve.rows[base], name=name, tags="pc-building", template=1)
        response = contextlib.closing(io.BytesIO(json.dumps({"data": {"ip": "192.0.2.40"}}).encode()))
        with patch.object(build, "ensure_cloud_image", return_value="import-store:import/source.qcow2"), \
             patch.object(build, "packer", side_effect=packer_stub), \
             patch.object(build.urllib.request, "urlopen", return_value=response), \
             contextlib.redirect_stdout(io.StringIO()):
            build.cmd_build(pve, a, build.IMAGES[a.os])
        self.assertIn(9300, pve.rows, "a portal import was pruned")
        self.assertNotIn(9301, pve.rows, "the older pipeline version beyond --keep was not pruned")

    def test_vlan_argument_and_packer_propagation(self):
        for value in ("0", "1", "4094"):
            self.assertEqual(build.vlan_arg(value), int(value))
        for value in ("-1", "4095", "01", " 2", "2.0", "bad", "", "0x10"):
            with self.subTest(value=value), self.assertRaises(argparse.ArgumentTypeError):
                build.vlan_arg(value)
        env = {"PVE_URL": "https://pve.invalid", "PVE_TOKEN_ID": "fake", "PVE_TOKEN_SECRET": "fake"}
        with patch.dict(os.environ, env), patch.object(build.subprocess, "run") as command:
            build.packer(arguments(), 9200, 9201, "image", "version")
        self.assertEqual(command.call_args.kwargs["env"]["PKR_VAR_vlan"], "29")

    def test_cli_rejects_bad_vlan_before_api(self):
        with patch.object(build.sys, "argv", ["build.py", "base", "--os", "debian-13", "--node", "test-node", "--storage", "disk", "--import-storage", "import", "--vlan", "4095"]), \
             patch.object(build, "PVE") as api, contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit):
                build.main()
            api.assert_not_called()


class WorkflowTests(unittest.TestCase):
    def env(self, vlan="0"):
        return dict(os.environ, PVE_BUILD_NETWORK_MODE="dhcp", PVE_URL="https://pve.invalid", PVE_TOKEN_ID="fake", PVE_TOKEN_SECRET="fake", PVE_NODES="test-node",
                    PVE_TEMPLATE_STORAGE="disk", PVE_IMPORT_STORAGE="import", PVE_BRIDGE="bridge", BUILD_IP="192.0.2.40/24",
                    BUILD_GATEWAY="192.0.2.1", BUILD_NAMESERVER="", PVE_BUILD_VLAN=vlan, DHCP_RANGE="", DHCP_GATEWAY="", K8S_VERSION="1.35.10", K8S_OS="ubuntu-2604")

    def test_secret_default_and_validation_both_workflows(self):
        for name in ("vm-templates.yml", "k8s-node-templates.yml"):
            text = (ROOT / ".github/workflows" / name).read_text()
            self.assertIn("PVE_BUILD_VLAN: ${{ secrets.PVE_BUILD_VLAN || '0' }}", text)
            script = workflow_run(name, "Validate required configuration")
            for vlan in ("0", "1", "4094", "-1", "4095", "01", "", " 2", "2.0", "$(bad)"):
                with self.subTest(workflow=name, vlan=vlan):
                    result = subprocess.run(["bash", "-c", script], env=self.env(vlan), capture_output=True, text=True)
                    self.assertEqual(result.returncode, 0 if vlan in ("0", "1", "4094") else 1, result.stderr)
                    if result.returncode:
                        self.assertIn("PVE_BUILD_VLAN must be", result.stdout)
                        self.assertNotIn("$(bad)", result.stdout)

    def test_os_workflow_choice_defaults_and_bounded_serial_selection(self):
        workflow = (ROOT / ".github/workflows/vm-templates.yml").read_text()
        self.assertIn("options: [debian-13, debian-12, ubuntu-2404, ubuntu-2604, all]", workflow)
        self.assertIn("default: debian-13", workflow)
        with tempfile.TemporaryDirectory() as tmp:
            stub = Path(tmp) / "python3"
            stub.write_text('#!/bin/bash\nprintf "%s\\n" "$4" >> "$CAPTURE"\nprintf "{}\\n"\n')
            stub.chmod(0o755)
            for choice, expected in (("", ["debian-13", "ubuntu-2404"]), ("all", ["debian-13", "debian-12", "ubuntu-2404", "ubuntu-2604"]), ("debian-12", ["debian-12"]), ("ubuntu-2604", ["ubuntu-2604"]), ("ubuntu-2604\n", []), ("debian-12 ubuntu-2604", [])):
                capture = Path(tmp) / "captured"
                capture.write_text("")
                env = self.env() | dict(PATH=tmp + ":" + os.environ["PATH"], CAPTURE=str(capture), OS_CHOICE=choice)
                result = subprocess.run(["bash", "-c", workflow_run("vm-templates.yml", "Build")], cwd=tmp, env=env, capture_output=True, text=True)
                self.assertEqual(result.returncode, 0 if expected else 1, result.stderr)
                self.assertEqual(capture.read_text().splitlines(), expected)

    def test_k8s_workflow_selector_defaults_and_invalid_values(self):
        workflow = (ROOT / ".github/workflows/k8s-node-templates.yml").read_text()
        self.assertIn("K8S_OS: ${{ inputs.os || 'ubuntu-2604' }}", workflow)
        self.assertIn("options: [ubuntu-2604, ubuntu-2404]", workflow)
        self.assertIn("PVE_BUILD_NETWORK_MODE: ${{ inputs.network_mode || 'static' }}", workflow)
        for value in ("ubuntu-2604", "ubuntu-2404", "debian-12", "debian-13", "ubuntu-2604\n", "", "ubuntu-2404;BAD"):
            env = self.env() | dict(K8S_OS=value)
            result = subprocess.run(["bash", "-c", workflow_run("k8s-node-templates.yml", "Validate required configuration")], env=env, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0 if value in ("ubuntu-2604", "ubuntu-2404") else 1)
            self.assertNotIn("BAD", result.stdout + result.stderr)

    def test_vm_workflow_passes_vlan_to_build_command(self):
        with tempfile.TemporaryDirectory() as tmp:
            binpath = Path(tmp) / "python3"
            binpath.write_text('#!/bin/bash\nprintf "%s\\n" "$@" > "$CAPTURE"\nprintf "{\\"os\\":\\"test\\",\\"built\\":true}\\n"\n')
            binpath.chmod(0o755)
            env = self.env("29")
            env.update(PATH=tmp + ":" + env["PATH"], CAPTURE=tmp + "/args", OS_CHOICE="debian-13")
            result = subprocess.run(["bash", "-c", workflow_run("vm-templates.yml", "Build")], cwd=tmp, env=env, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            args = Path(tmp + "/args").read_text().splitlines()
            self.assertEqual(args[args.index("--vlan") + 1], "29")


class KubernetesTests(unittest.TestCase):
    def run_pipeline(self, vlan="29", iso=None, fail="", existing=False, extra_env=None):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            bindir = root / "bin"
            bindir.mkdir()
            work = root / "work"
            capi = work / "image-builder/images/capi"
            upstream = capi / "packer/proxmox/packer.json.tmpl"
            upstream.parent.mkdir(parents=True)
            upstream.write_text(json.dumps({
                "builders": [{"network_adapters": [{"vlan_tag": "{{user `vlan_tag`}}"}], "iso_file": "{{user `iso_file`}}"}],
                "variables": {"vlan_tag": "{{env `PROXMOX_VLAN`}}", "ssh_password": "$SSH_PASSWORD"},
                "provisioners": [{"pause_before": "10s"}, {"type": "goss"}, {"type": "shell", "expect_disconnect": True, "inline": ["sudo shutdown"]}],
            }))
            fixtures = ROOT / 'images/tests/fixtures/image-builder-v0.1.55'
            for os_slug, release in (("ubuntu-2404", "24.04"), ("ubuntu-2604", "26.04")):
                (upstream.parent / (os_slug + '.json')).write_text((fixtures / (os_slug + '.json')).read_text())
                seed = capi / ('packer/proxmox/linux/ubuntu/http/' + release + '/user-data.tmpl')
                seed.parent.mkdir(parents=True)
                seed.write_text((fixtures / 'ubuntu-bios-user-data.tmpl').read_text())
            kubeadm = capi / 'ansible/roles/kubernetes/templates/etc/kubeadm.yml'
            kubeadm.parent.mkdir(parents=True)
            kubeadm.write_text((fixtures / 'kubeadm.yml').read_text())
            # No real git process/network: a stub presents a pristine reviewed cache.
            (work / 'image-builder/.git').mkdir()
            # Fixed object bytes are separate from the mutable cached working tree.
            (capi / 'Makefile').write_text('# reviewed object fixture\n')
            object_rows = {}
            for file in (work / 'image-builder').rglob('*'):
                if file.is_file():
                    data = file.read_bytes()
                    oid = hashlib.sha1(b'blob ' + str(len(data)).encode() + b'\0' + data).hexdigest()
                    object_rows[str(file.relative_to(work / 'image-builder'))] = dict(oid=oid, text=data.decode())
            objects = root / 'objects.json'
            objects.write_text(json.dumps(object_rows))
            if (extra_env or {}).get('CACHED_WORKTREE_EDIT'):
                (capi / 'Makefile').write_text('UNREVIEWED working-tree content\n')
            activate = work / "venv/bin/activate"
            activate.parent.mkdir(parents=True)
            activate.write_text("# Stub virtualenv; tests use host Python only.\n")
            state = root / "state.json"
            initial = {"events": [], "vms": {}, "nextid": 9201}
            if existing or (extra_env or {}).get("PREEXISTING_BUILD_ID"):
                os_slug = (extra_env or {}).get("K8S_OS", "ubuntu-2604")
                initial["vms"]["9200"] = {"name": os_slug + "-kube-v1.35.10" if existing else "packer-old",
                                          "node": "test-node", "template": 1 if existing else 0,
                                          "net0": "virtio=02:00:00:00:00:01,bridge=test-bridge,tag=29,firewall=1,mtu=1450"}
            state.write_text(json.dumps(initial))
            curl_stub = r'''#!/usr/bin/env python3
import json, os, re, sys
from pathlib import Path
path = Path(os.environ['STATE'])
s = json.loads(path.read_text())
args = sys.argv[1:]
method = args[args.index('-X') + 1] if '-X' in args else 'GET'
url = next(arg for arg in args if arg.startswith('https://'))
params = {}
for i, arg in enumerate(args):
    if arg in ('-d', '--data-urlencode'):
        key, value = args[i+1].split('=', 1)
        params[key] = value
s['events'].append({'method': method, 'url': url, 'params': params})
data = None
match = re.search(r'/nodes/([^/]+)/qemu/(\d+)', url)
if '/cluster/resources?' in url:
    data = [dict(v, vmid=int(k)) for k, v in s['vms'].items()]
    if s.get('made'):
        n = s.get('discovery_reads', 0); s['discovery_reads'] = n + 1
        mode = os.environ.get('DISCOVERY', '')
        if mode == 'read-error':
            path.write_text(json.dumps(s)); sys.exit(22)
        if mode == 'malformed': data = None
        if mode == 'absent' or (mode == 'lag' and n == 0): data = []
        if mode == 'lag' and n == 1:
            data = [dict(data[0], name='packer-current-run', template=0)]
        if mode == 'ambiguous': data.append(dict(data[0], vmid=9299))
elif '/pools/' in url:
    data = {'members': [dict(v, vmid=int(k), type='qemu') for k, v in s['vms'].items() if v.get('pool') == url.rsplit('/', 1)[1]]}
    if os.environ.get('DISCOVERY') == 'wrong-pool': data['members'] = []
elif url.endswith('/status/current'):
    data = {'status': 'running' if os.environ.get('DISCOVERY') == 'running' else 'stopped'}
elif url.endswith('/cluster/nextid'):
    data = s['nextid']; s['nextid'] += 1
elif '/tasks/' in url:
    data = {'status': 'stopped', 'exitstatus': 'OK'}
elif '/download-url' in url:
    data = 'download-task'
elif url.endswith('/clone'):
    assert 'net0' not in s['vms'][match[2]], 'clone inherited build NIC'
    s['vms'][params['newid']] = dict(s['vms'][match[2]], name=params['name'], node=params['target'])
    data = 'clone-task'
elif url.endswith('/template'):
    s['vms'][match[2]]['template'] = 1
elif url.endswith('/config'):
    vm = s['vms'][match[2]]
    if method == 'GET':
        data = dict(vm, digest='fake-digest')
        if os.environ.get('DISCOVERY') == 'wrong-name': data['name'] = 'packer-not-final'
        if os.environ.get('DISCOVERY') == 'not-template': data['template'] = 0
    else:
        if params.get('delete') == 'net0':
            assert params['digest'] == 'fake-digest'
            if os.environ.get('FAIL') == 'cleanup':
                path.write_text(json.dumps(s)); sys.exit(22)
            if os.environ.get('FAIL') != 'readback':
                vm.pop('net0', None)
        if 'tags' in params:
            assert 'net0' not in vm, 'publication inherited build NIC'
            vm['tags'] = params['tags']
else:
    raise AssertionError(url)
path.write_text(json.dumps(s))
print(json.dumps({'data': data}))
'''
            make_stub = r'''#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
path = Path(os.environ['STATE']); s = json.loads(path.read_text())
s['events'].append({'make': dict({key: os.environ.get(key) for key in ('PROXMOX_VLAN','PROXMOX_ISO_POOL','PROXMOX_STORAGE_POOL','ISO_FILE')}, target=sys.argv[-1], vars=json.loads(Path(os.environ['PACKER_VAR_FILES']).read_text()))})
path.write_text(json.dumps(s))
if os.environ.get('FAIL') == 'make': sys.exit(37)
assert Path('Makefile').read_text() == '# reviewed object fixture\n', 'Build must use pinned object bytes'
assert not Path('../../.git').exists(), 'Build must not inherit cached git config/hooks'
template = json.loads(Path('packer/proxmox/packer.json').read_text())
assert template['variables']['vlan_tag'] == '{{env `PROXMOX_VLAN`}}'
assert template['builders'][0]['network_adapters'][0]['vlan_tag'] == '{{user `vlan_tag`}}'
vlan = os.environ['PROXMOX_VLAN']
net = 'virtio=02:00:00:00:00:01,bridge=test-bridge,firewall=1,mtu=1450' + (',tag='+vlan if vlan else '')
s['vms']['9200'] = {'name': os.environ['K8S_OS']+'-kube-v'+os.environ['K8S_VERSION'], 'node': 'test-node','template': 1, 'net0':net, 'pool': os.environ.get('TEMPLATE_POOL', 'pc-templates')}
s['made'] = True
path.write_text(json.dumps(s))
'''
            git_stub = r'''#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
args = sys.argv[1:]
assert args[0] == '-C', 'Tests must not clone/fetch or mutate git'
assert args[2] in ('remote', 'rev-parse', 'ls-files', 'status', 'ls-tree', 'cat-file'), 'Only read-only git checks/object reads are allowed'
if args[2] == 'remote':
    print(os.environ.get('CACHED_ORIGIN', 'https://github.com/kubernetes-sigs/image-builder'))
elif '--show-toplevel' in args:
    print(Path(args[1]).resolve())
elif args[2] == 'rev-parse':
    print(os.environ.get('CACHED_COMMIT', '7ffb9b7f1f26cd66891874463cc9411e3633325f'))
elif args[2] == 'ls-files':
    sys.stdout.write(os.environ.get('CACHED_FLAGS', 'H') + ' images/capi/Makefile\0')
elif args[2] == 'status':
    if os.environ.get('CACHED_DIRTY'): print(' M private-change')
else:
    rows = json.loads(Path(os.environ['OBJECTS']).read_text())
    if args[2] == 'ls-tree':
        assert args[3:] == ['-r', '-z', '7ffb9b7f1f26cd66891874463cc9411e3633325f']
        for name, row in sorted(rows.items()):
            sys.stdout.buffer.write(('100644 blob ' + row['oid'] + '\t' + name).encode() + b'\0')
    else:
        assert args[3:] == ['--batch']
        blobs = {row['oid']: row['text'].encode() for row in rows.values()}
        for oid in sys.stdin.buffer.read().splitlines():
            data = blobs[oid.decode()]
            sys.stdout.buffer.write(oid + b' blob ' + str(len(data)).encode() + b'\n' + data + b'\n')
'''
            for tool, script in (("curl", curl_stub), ("make", make_stub), ("git", git_stub),
                                 ("xorriso", "#!/bin/bash\nexit 0\n")):
                target = bindir / tool
                target.write_text(script)
                target.chmod(0o755)
            # Source the production wrapper with only sleep replaced: advance its
            # SECONDS deadline without wall-clock waits in timeout cases.
            env = WorkflowTests().env(vlan)
            env.update(PATH=str(bindir) + ":" + env["PATH"], WORK=str(work), STATE=str(state), OBJECTS=str(objects), FAIL=fail,
                       PVE_NODES="test-node other-node", PVE_TEMPLATE_STORAGE="disk-store", PVE_BRIDGE="test-bridge", HOME=str(root))
            env.pop("PVE_ISO_STORAGE", None)
            if iso is not None:
                env["PVE_ISO_STORAGE"] = iso
            if extra_env:
                env.update(extra_env)
            result = subprocess.run(["bash", "-c", 'sleep() { SECONDS=$((SECONDS + $1)); }; source "$1"',
                                     "test-wrapper", str(ROOT / "images/k8s-node-template.sh")], env=env,
                                    capture_output=True, text=True, timeout=15)
            return result, json.loads(state.read_text())

    def test_both_os_selectors_keep_name_target_iso_and_version_tags_consistent(self):
        for os_slug, iso_name, checksum in (("ubuntu-2404", "ubuntu-24.04.4-live-server-amd64.iso", "e907d92eeec9df64163a7e454cbc8d7755e8ddc7ed42f99dbc80c40f1a138433"),
                                            ("ubuntu-2604", "ubuntu-26.04.1-live-server-amd64.iso", "cc8a95cde20f6ced61a322420de00f10cc3c90ced545daa46cb9c1a117f1d927")):
            with self.subTest(os=os_slug):
                result, state = self.run_pipeline(extra_env={"K8S_OS": os_slug})
                self.assertEqual(result.returncode, 0, result.stderr)
                make = next(e["make"] for e in state["events"] if "make" in e)
                self.assertEqual(make["target"], "build-proxmox-" + os_slug)
                self.assertTrue(make["ISO_FILE"].endswith("/" + iso_name))
                self.assertEqual(make["vars"]["iso_checksum"], checksum)
                for vm in state["vms"].values():
                    self.assertEqual(vm["name"], os_slug + "-kube-v1.35.10")
                    self.assertIn("pc-k8s-1-35-10", vm["tags"].split(";"))
                    self.assertIn("pc-k8s-os-" + os_slug, vm["tags"].split(";"))
                self.assertTrue(result.stdout.rstrip().endswith("1.35.10=test-node=9200,other-node=9201"))

    def test_invalid_os_pin_and_cached_ref_fail_before_api_or_build(self):
        cases = [{"K8S_OS": value} for value in ("debian-12", "debian-13", "ubuntu-2604\n", "ubuntu-2404;BAD", "")]
        cases += [{"IMAGE_BUILDER_REF": "main"}, {"IMAGE_BUILDER_COMMIT": "0" * 40}, {"CACHED_COMMIT": "0" * 40}, {"CACHED_DIRTY": "yes"}]
        cases += [{"CACHED_ORIGIN": origin} for origin in ("https://private.invalid/secret-origin", "https://github.com/kubernetes-sigs/image-builder\nhttps://github.com/kubernetes-sigs/image-builder.git", "")]
        cases += [{"CACHED_FLAGS": flag} for flag in ("h", "S", "s")]
        for extra in cases:
            with self.subTest(extra=extra):
                result, state = self.run_pipeline(extra_env=extra)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(state["events"], [])
                self.assertNotIn("BAD", result.stdout + result.stderr)
                self.assertNotIn("secret-origin", result.stdout + result.stderr)
                if any(key.startswith("CACHED_") for key in extra):
                    self.assertIn("use a new WORK", result.stderr)

    def test_export_ignores_changed_worktree_and_accepts_approved_dot_git_origin(self):
        result, state = self.run_pipeline(extra_env={"CACHED_WORKTREE_EDIT": "yes", "CACHED_ORIGIN": "https://github.com/kubernetes-sigs/image-builder.git"})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(any("make" in event for event in state["events"]))
        # The make stub asserts its Makefile came from the pinned object fixture.

    def test_tagged_build_uses_upstream_vlan_and_separate_iso_store(self):
        result, state = self.run_pipeline(iso="iso-store")
        self.assertEqual(result.returncode, 0, result.stderr)
        make = next(e['make'] for e in state['events'] if 'make' in e)
        self.assertEqual(make['PROXMOX_VLAN'], '29')
        self.assertEqual(make['PROXMOX_ISO_POOL'], 'iso-store')
        self.assertEqual(make['PROXMOX_STORAGE_POOL'], 'disk-store')
        self.assertTrue(make['ISO_FILE'].startswith('iso-store:iso/'))
        self.assertTrue(any('/storage/iso-store/download-url' in e.get('url','') for e in state['events']))
        cleaned = next(i for i,e in enumerate(state['events']) if e.get('params',{}).get('delete') == 'net0')
        cloned = next(i for i,e in enumerate(state['events']) if e.get('url','').endswith('/clone'))
        published = next(i for i,e in enumerate(state['events']) if 'pc-k8s' in e.get('params',{}).get('tags',''))
        self.assertLess(cleaned, cloned)
        self.assertLess(cleaned, published)
        for vm in state['vms'].values():
            self.assertNotIn('net0', vm)

    def test_untagged_default_and_iso_storage_fallback(self):
        result, state = self.run_pipeline(vlan="")
        self.assertEqual(result.returncode, 0, result.stderr)
        make = next(e['make'] for e in state['events'] if 'make' in e)
        self.assertEqual(make['PROXMOX_VLAN'], '')
        self.assertEqual(make['PROXMOX_ISO_POOL'], 'disk-store')
        self.assertTrue(make['ISO_FILE'].startswith('disk-store:iso/'))
        self.assertTrue(any(e.get('params',{}).get('delete') == 'net0' for e in state['events']))

    def test_existing_source_healed_before_clone(self):
        result, state = self.run_pipeline(existing=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(any('make' in e for e in state['events']))
        self.assertNotIn('net0', state['vms']['9200'])

    def test_post_build_discovery_waits_for_empty_and_old_packer_cache(self):
        result, state = self.run_pipeline(extra_env={'DISCOVERY': 'lag'})
        self.assertEqual(result.returncode, 0, result.stderr)
        events = state['events']
        made = next(i for i, e in enumerate(events) if 'make' in e)
        cleaned = next(i for i, e in enumerate(events) if e.get('params', {}).get('delete') == 'net0')
        polls = [e for e in events[made + 1:cleaned] if '/cluster/resources?' in e.get('url', '')]
        self.assertEqual(len(polls), 3)
        checks = [e['url'] for e in events[made + 1:cleaned] if e.get('method') == 'GET']
        self.assertTrue(any('/pools/pc-templates' in url for url in checks))
        self.assertTrue(any('/qemu/9200/config' in url for url in checks))
        self.assertTrue(any('/qemu/9200/status/current' in url for url in checks))
        self.assertNotIn('net0', state['vms']['9200'])

    def test_persistent_discovery_absence_is_bounded_before_mutation(self):
        result, state = self.run_pipeline(extra_env={'DISCOVERY': 'absent'})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('discovery timed out after 120s', result.stderr)
        # Fake sleep advances 3 seconds per empty lookup; wall time may reduce this.
        self.assertGreater(state['discovery_reads'], 1)
        self.assertLessEqual(state['discovery_reads'], 40)
        self.assertFalse(any(e.get('url', '').endswith('/clone') or
                             e.get('params', {}).get('delete') == 'net0' or
                             'tags' in e.get('params', {}) for e in state['events']))

    def test_discovery_read_ambiguity_and_identity_errors_are_not_retried(self):
        for mode in ('read-error', 'malformed', 'ambiguous', 'wrong-pool', 'wrong-name', 'not-template', 'running'):
            with self.subTest(mode=mode):
                result, state = self.run_pipeline(extra_env={'DISCOVERY': mode})
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(state['discovery_reads'], 1)
                self.assertNotIn('timed out', result.stderr)
                self.assertFalse(any(e.get('url', '').endswith('/clone') or
                                     e.get('params', {}).get('delete') == 'net0' or
                                     'tags' in e.get('params', {}) for e in state['events']))

    def test_build_discovery_refuses_any_baseline_vmid(self):
        result, state = self.run_pipeline(extra_env={'PREEXISTING_BUILD_ID': 'yes'})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('refuses a preexisting VMID', result.stderr)
        self.assertEqual(state['discovery_reads'], 1)
        self.assertFalse(any(e.get('url', '').endswith('/clone') or
                             e.get('params', {}).get('delete') == 'net0' or
                             'tags' in e.get('params', {}) for e in state['events']))

    def test_static_existing_name_refused_without_build_or_wait(self):
        result, state = self.run_pipeline(existing=True, extra_env={
            'PVE_BUILD_NETWORK_MODE': 'static', 'BUILD_NAMESERVER': '192.0.2.53'})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('Static build refuses existing same-name templates', result.stderr)
        self.assertEqual(len(state['events']), 1)
        self.assertFalse(any('make' in e for e in state['events']))

    def test_failed_cleanup_or_readback_prevents_clone_and_publish(self):
        for fail in ('cleanup','readback'):
            with self.subTest(fail=fail):
                result, state = self.run_pipeline(fail=fail)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(any(e.get('url','').endswith('/clone') or 'tags' in e.get('params',{}) for e in state['events']))

    def test_failed_build_prevents_cleanup_and_publish(self):
        result, state = self.run_pipeline(fail='make')
        self.assertEqual(result.returncode, 1)
        self.assertIn('Image-builder failed', result.stderr)
        self.assertNotIn('discovery_reads', state)
        self.assertFalse(any(e.get('params',{}).get('delete') == 'net0' or 'tags' in e.get('params',{}) for e in state['events']))

    def test_static_missing_fields_fail_before_api_and_existing_name_is_not_adopted(self):
        for key in ('BUILD_IP', 'BUILD_GATEWAY', 'BUILD_NAMESERVER'):
            extra = {'PVE_BUILD_NETWORK_MODE': 'static', 'BUILD_NAMESERVER': '192.0.2.53', key: ''}
            result, state = self.run_pipeline(extra_env=extra)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(state['events'], [])
            self.assertIn('Missing required static build setting', result.stderr)

    def test_invalid_vlan_fails_before_any_api_or_build(self):
        for vlan in ('-1','4095','01','bad',' 2','2.0'):
            with self.subTest(vlan=vlan):
                result, state = self.run_pipeline(vlan=vlan)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(state['events'], [])
                self.assertIn('PVE_BUILD_VLAN must be', result.stderr)


if __name__ == "__main__":
    unittest.main()
