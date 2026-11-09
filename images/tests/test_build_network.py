#!/usr/bin/env python3
"""Offline regressions: no PVE, network, Packer binary or credentials are used.

Run: python3 -m unittest discover -s images/tests -v
"""
import argparse
import contextlib
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
        if path.endswith("/qemu"):
            self.rows[vmid] = {"node": "test-node", "pool": build.POOL}
        elif path.endswith("/clone"):
            self.rows[params["newid"]] = dict(self.rows[vmid])
        elif path.endswith("/config"):
            if method == "GET":
                return dict(self.rows[vmid])
            if params.get("delete") == "net0":
                if not self.fail_cleanup and not self.stale_cleanup:
                    self.rows[vmid].pop("net0", None)
                return "cleanup"
            row = self.rows[vmid]
            row.update({key: value for key, value in params.items() if key != "delete"})
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
        return dict(os.environ, PVE_URL="https://pve.invalid", PVE_TOKEN_ID="fake", PVE_TOKEN_SECRET="fake", PVE_NODES="test-node",
                    PVE_TEMPLATE_STORAGE="disk", PVE_IMPORT_STORAGE="import", PVE_BRIDGE="bridge", BUILD_IP="192.0.2.40/24",
                    BUILD_GATEWAY="192.0.2.1", BUILD_NAMESERVER="", PVE_BUILD_VLAN=vlan, DHCP_RANGE="", DHCP_GATEWAY="", K8S_VERSION="1.35.10")

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

    def test_vm_workflow_passes_vlan_to_build_command(self):
        with tempfile.TemporaryDirectory() as tmp:
            binpath = Path(tmp) / "python3"
            binpath.write_text('#!/bin/bash\nprintf "%s\\n" "$@" > "$CAPTURE"\nprintf "{\\"os\\":\\"test\\",\\"built\\":true}\\n"\n')
            binpath.chmod(0o755)
            env = self.env("29")
            env.update(PATH=tmp + ":" + env["PATH"], CAPTURE=tmp + "/args", OS_LIST="debian-13")
            result = subprocess.run(["bash", "-c", workflow_run("vm-templates.yml", "Build")], cwd=tmp, env=env, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            args = Path(tmp + "/args").read_text().splitlines()
            self.assertEqual(args[args.index("--vlan") + 1], "29")


class KubernetesTests(unittest.TestCase):
    def run_pipeline(self, vlan="29", iso=None, fail="", existing=False):
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
                "variables": {"vlan_tag": "{{env `PROXMOX_VLAN`}}"},
                "provisioners": [{"pause_before": "10s"}],
            }))
            activate = work / "venv/bin/activate"
            activate.parent.mkdir(parents=True)
            activate.write_text("# Stub virtualenv; tests use host Python only.\n")
            state = root / "state.json"
            initial = {"events": [], "vms": {}, "nextid": 9201}
            if existing:
                initial["vms"]["9200"] = {"name": "ubuntu-2404-kube-v1.35.10", "node": "test-node", "template": 1,
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
s['events'].append({'make': {key: os.environ.get(key) for key in ('PROXMOX_VLAN','PROXMOX_ISO_POOL','PROXMOX_STORAGE_POOL','ISO_FILE')}})
path.write_text(json.dumps(s))
if os.environ.get('FAIL') == 'make': sys.exit(37)
template = json.loads(Path('packer/proxmox/packer.json.tmpl').read_text())
assert template['variables']['vlan_tag'] == '{{env `PROXMOX_VLAN`}}'
assert template['builders'][0]['network_adapters'][0]['vlan_tag'] == '{{user `vlan_tag`}}'
vlan = os.environ['PROXMOX_VLAN']
net = 'virtio=02:00:00:00:00:01,bridge=test-bridge,firewall=1,mtu=1450' + (',tag='+vlan if vlan else '')
s['vms']['9200'] = {'name': 'ubuntu-2404-kube-v'+os.environ['K8S_VERSION'], 'node': 'test-node','template': 1, 'net0':net}
path.write_text(json.dumps(s))
'''
            for tool, script in (("curl", curl_stub), ("make", make_stub)):
                target = bindir / tool
                target.write_text(script)
                target.chmod(0o755)
            env = WorkflowTests().env(vlan)
            env.update(PATH=str(bindir) + ":" + env["PATH"], WORK=str(work), STATE=str(state), FAIL=fail,
                       PVE_NODES="test-node other-node", PVE_TEMPLATE_STORAGE="disk-store", PVE_BRIDGE="test-bridge", HOME=str(root))
            env.pop("PVE_ISO_STORAGE", None)
            if iso is not None:
                env["PVE_ISO_STORAGE"] = iso
            result = subprocess.run(["bash", str(ROOT / "images/k8s-node-template.sh")], env=env,
                                    capture_output=True, text=True, timeout=15)
            return result, json.loads(state.read_text())

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

    def test_failed_cleanup_or_readback_prevents_clone_and_publish(self):
        for fail in ('cleanup','readback'):
            with self.subTest(fail=fail):
                result, state = self.run_pipeline(fail=fail)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(any(e.get('url','').endswith('/clone') or 'tags' in e.get('params',{}) for e in state['events']))

    def test_failed_build_prevents_cleanup_and_publish(self):
        result, state = self.run_pipeline(fail='make')
        self.assertEqual(result.returncode, 37)
        self.assertFalse(any(e.get('params',{}).get('delete') == 'net0' or 'tags' in e.get('params',{}) for e in state['events']))

    def test_invalid_vlan_fails_before_any_api_or_build(self):
        for vlan in ('-1','4095','01','bad',' 2','2.0'):
            with self.subTest(vlan=vlan):
                result, state = self.run_pipeline(vlan=vlan)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(state['events'], [])
                self.assertIn('PVE_BUILD_VLAN must be', result.stderr)


if __name__ == "__main__":
    unittest.main()
