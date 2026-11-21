#!/usr/bin/env python3
"""The sandbox cluster's node-setup (deploy/sandbox-cluster/node-setup.yaml): the pinned runsc package and installer
stage 55's edits of the file. Standard library plus PyYAML.

  python3 deploy/tests/test_sandbox_node_setup.py

No node: setup.sh runs with bash against fake ip, dpkg-query, curl, dpkg, sysctl and systemctl, with its /etc and
/usr/share paths moved into a temporary directory. `shellcheck` reads the script when installed, and is required when
CI is set.
"""
import hashlib
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import unittest

import yaml

ROOT = Path(__file__).resolve().parents[2]
NODE_SETUP = (ROOT / "deploy/sandbox-cluster/node-setup.yaml").read_text()
STAGE55 = (ROOT / "installer/playbooks/55-sandbox-cluster.yml").read_text()

# The gVisor release's own package: the SHA256 the signed suite index (dists/release/main/binary-amd64/Packages,
# Release signed by https://gvisor.dev/archive.key) gives for it, checked against the downloaded file. A bump changes
# all three, and node-setup.yaml with them.
VERSION = "20261116.0"
SHA256 = "495796a56d811a43ea218bd0af3d89ea471df1cbd684edcb21c4a00356caeb1e"
URL = "https://storage.googleapis.com/gvisor/releases/pool/20261116.0/binary-amd64/runsc.deb"


def parse(text):
    docs = {d["kind"]: d for d in yaml.safe_load_all(text)}
    setup = next(c for c in docs["DaemonSet"]["spec"]["template"]["spec"]["containers"] if c["name"] == "setup")
    return docs["ConfigMap"]["data"]["setup.sh"], {e["name"]: e.get("value") for e in setup["env"]}, setup, docs


SCRIPT, ENV, SETUP, DOCS = parse(NODE_SETUP)


def stage55_edits():
    """(pattern, replacement) of every `sed -i "s#...#...#" node-setup.yaml` in stage 55, shell quoting undone."""
    found = re.findall(r'sed -i "s#((?:[^#\\]|\\.)*)#((?:[^#\\]|\\.)*)#" node-setup\.yaml', STAGE55)
    return [(a.replace('\\"', '"'), b.replace('\\"', '"')) for a, b in found]


class PinTests(unittest.TestCase):
    def test_version_and_sha256_are_pinned_together(self):
        self.assertEqual(ENV["RUNSC_VERSION"], VERSION)
        self.assertEqual(ENV["RUNSC_SHA256"], SHA256)
        self.assertIn('RUNSC_SHA256="$RUNSC_SHA256"', SETUP["args"][0], "nsenter must pass the checksum to the host")
        self.assertIn('"https://storage.googleapis.com/gvisor/releases/pool/$RUNSC_VERSION/binary-amd64/runsc.deb"',
                      SCRIPT)

    def test_no_apt_suite(self):
        for gone in ("apt-get", "releases release main", "--allow-downgrades"):
            self.assertNotIn(gone, SCRIPT)

    def test_drop_in_names_the_gvisor_handler(self):
        # image-builder v0.1.55 (containerd 2.3.2) writes config version 3 and imports conf.d, without a gvisor
        # handler, on both the 24.04 and the 26.04 node images.
        for line in ("version = 3", "[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gvisor]",
                     "runtime_type = 'io.containerd.runsc.v1'", 'ConfigPath = "/etc/containerd/runsc.toml"'):
            self.assertIn(line, SCRIPT)


class Stage55Tests(unittest.TestCase):
    def test_every_sed_target_is_in_the_file_once(self):
        edits = stage55_edits()
        self.assertEqual(len(edits), 2, edits)
        for pattern, _ in edits:
            self.assertEqual(NODE_SETUP.count(pattern), 1, pattern)

    def test_edits_set_the_runtime_image_and_allow_suid(self):
        text = NODE_SETUP
        for pattern, replacement in stage55_edits():
            text = text.replace(pattern, replacement.replace("$RUNTIME", "registry.test/runtime@sha256:" + "a" * 64)
                                .replace("$ALLOW_SUID", "false"))
        script, env, _, docs = parse(text)
        init = docs["DaemonSet"]["spec"]["template"]["spec"]["initContainers"][0]
        self.assertEqual(init["image"], "registry.test/runtime@sha256:" + "a" * 64)
        self.assertEqual(env["RUNSC_ALLOW_SUID"], "false")
        self.assertEqual((env["RUNSC_VERSION"], env["RUNSC_SHA256"], script), (VERSION, SHA256, SCRIPT))


FAKES = {
    "ip": 'if [ -n "${FAKE_IPV6:-}" ] && [ "$2" = addr ]; then echo "inet6 2001:db8::1/64 scope global"; fi\n',
    "dpkg-query": '[ -n "${FAKE_INSTALLED:-}" ] || exit 1\nprintf %s "$FAKE_INSTALLED"\n',
    "curl": 'echo "curl $*" >> "$LOG"\nwhile [ $# -gt 1 ]; do [ "$1" = -o ] && out=$2; shift; done\n'
            'cp "$FAKE_DEB" "$out"\n',
    "dpkg": 'echo "dpkg $*" >> "$LOG"\n',
    "sysctl": 'echo "sysctl $*" >> "$LOG"\n',
    "systemctl": 'echo "systemctl $*" >> "$LOG"\n',
}


class ScriptTests(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.dir = Path(tmp.name)
        self.root, bin_ = self.dir / "root", self.dir / "bin"
        for d in (bin_, self.dir / "tmp", self.root / "etc/sysctl.d", self.root / "etc/apt/sources.list.d",
                  self.root / "usr/share/keyrings"):
            d.mkdir(parents=True)
        for name, body in FAKES.items():
            (bin_ / name).write_text("#!/bin/sh\n" + body)
            (bin_ / name).chmod(0o755)
        self.deb = self.dir / "runsc.deb"
        self.deb.write_bytes(b"not a real package")
        self.log = self.dir / "log"
        self.log.touch()
        self.old_list = self.root / "etc/apt/sources.list.d/gvisor.list"
        self.old_list.write_text("deb https://storage.googleapis.com/gvisor/releases release main\n")
        self.env = dict(PATH=f"{bin_}:{os.environ.get('PATH', os.defpath)}", TMPDIR=str(self.dir / "tmp"),
                        LOG=str(self.log), FAKE_DEB=str(self.deb), RUNSC_VERSION=VERSION,
                        RUNSC_SHA256=hashlib.sha256(self.deb.read_bytes()).hexdigest(),
                        RUNSC_PLATFORM="systrap", RUNSC_ALLOW_SUID="true")

    def run_setup(self, **env):
        script = SCRIPT.replace("/etc/", f"{self.root}/etc/").replace("/usr/share/", f"{self.root}/usr/share/")
        result = subprocess.run(["bash", "-c", script], env={**self.env, **env}, capture_output=True, text=True)
        return result, self.log.read_text().splitlines()

    def test_new_node_installs_the_checked_package(self):
        result, log = self.run_setup()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(f"curl -fsSL -o {self.dir}/tmp/", log[0])
        self.assertTrue(log[0].endswith(" " + URL), log[0])
        dpkg = [line for line in log if line.startswith("dpkg ")]
        self.assertEqual(len(dpkg), 1, log)
        self.assertTrue(dpkg[0].startswith(f"dpkg -i --force-confdef --force-confold {self.dir}/tmp/"), dpkg)
        self.assertEqual(list((self.dir / "tmp").iterdir()), [], "the downloaded package is removed")
        self.assertFalse(self.old_list.exists(), "the apt source of earlier versions is removed")
        self.assertIn("runtimes.gvisor]", (self.root / "etc/containerd/conf.d/10-gvisor.toml").read_text())
        self.assertEqual((self.root / "etc/containerd/runsc.toml").read_text(),
                         '[runsc_config]\n  platform = "systrap"\n  allow-suid = "true"\n')
        self.assertIn("systemctl restart containerd", log)

    def test_wrong_checksum_installs_nothing(self):
        result, log = self.run_setup(RUNSC_SHA256="0" * 64)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual([line for line in log if not line.startswith("curl ")], [])
        self.assertEqual(list((self.dir / "tmp").iterdir()), [])
        self.assertFalse((self.root / "etc/containerd/conf.d").exists())

    def test_installed_version_downloads_nothing(self):
        result, log = self.run_setup(FAKE_INSTALLED=VERSION)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual([line for line in log if line.split()[0] in ("curl", "dpkg")], [])
        self.assertIn(f"node-setup done: runsc {VERSION}", result.stdout)

    def test_ipv6_node_is_refused_before_any_install(self):
        result, log = self.run_setup(FAKE_IPV6="1")
        self.assertEqual(result.returncode, 1)
        self.assertIn("REFUSED", result.stderr)
        self.assertEqual(log, [])

    def test_script_is_shellcheck_clean(self):
        if not shutil.which("shellcheck"):
            if os.environ.get("CI"):
                self.fail("shellcheck is required in CI")
            self.skipTest("shellcheck not installed")
        path = self.dir / "setup.sh"
        path.write_text(SCRIPT)
        result = subprocess.run(["shellcheck", "--shell=bash", str(path)], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stdout)


if __name__ == "__main__":
    unittest.main(verbosity=2)
