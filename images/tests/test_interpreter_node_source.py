"""Data-only interpreter Node/NAN contracts; do not install or execute packages.

Authored, not run here. Native compilation and all three kernel known-answer
checks remain mandatory in the image build; these source checks cannot prove ABI
compatibility. Upstream zeromq 5.3.1 pins NAN 2.17.0 and Linux C++17:
https://github.com/zeromq/zeromq.js/blob/v5.3.1/package.json
https://github.com/zeromq/zeromq.js/blob/v5.3.1/binding.gyp
NAN 2.30.0 explicitly supports Node 22 and handles its V8 accessor changes:
https://github.com/nodejs/nan/blob/v2.30.0/nan.h
https://registry.npmjs.org/nan/2.30.0
npm 11.21.2 supports Node >=22.9.0 and updates its bundled tar independently:
https://registry.npmjs.org/npm/11.21.2
"""
import json
from pathlib import Path
import re
import unittest

ROOT = Path(__file__).resolve().parents[2]
TEMPLATE = ROOT / "deploy/templates/code-interpreter"


class InterpreterNodeSourceTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.dockerfile = (TEMPLATE / "Dockerfile").read_text()
        cls.manifest = json.loads((TEMPLATE / "kernel-package.json").read_text())
        cls.instructions = "\n".join(
            line for line in cls.dockerfile.splitlines()
            if not line.lstrip().startswith("#")
        )

    def test_maintained_node22_and_independent_fixed_npm(self):
        self.assertEqual(re.findall(r"deb\.nodesource\.com/setup_(\d+)\.x", self.instructions), ["22"])
        self.assertIn("&& npm install -g npm@11.21.2", self.instructions)
        self.assertNotIn("CXXFLAGS", self.instructions)
        self.assertNotIn("-std=c++", self.instructions)

    def test_exact_original_kernel_and_only_scoped_nan_override(self):
        self.assertTrue(self.manifest["private"])
        self.assertEqual(self.manifest["dependencies"], {
            "ijavascript": "git+https://github.com/e2b-dev/ijavascript.git#79cb7d56dcfe0df9ff55eff0ca4dc920a6dcc361",
        })
        self.assertEqual(self.manifest["overrides"], {"zeromq": {"nan": "2.30.0"}})
        self.assertNotIn("scripts", self.manifest)
        self.assertNotIn("devDependencies", self.manifest)

    def test_override_is_used_by_local_root_install_not_global(self):
        copy = "COPY code-interpreter/kernel-package.json /opt/proxcloud-kernel/package.json"
        install = "RUN npm install --prefix /opt/proxcloud-kernel --omit=dev --no-audit --no-fund"
        self.assertIn(copy, self.instructions)
        self.assertIn(install, self.instructions)
        self.assertLess(self.instructions.index(copy), self.instructions.index(install))
        self.assertIn('ENV PATH="/opt/proxcloud-kernel/node_modules/.bin:${PATH}"', self.instructions)
        self.assertNotRegex(self.instructions, r"npm install -g.*ijavascript")
        for bypass in ("--ignore-scripts", "ignore-scripts=true", "|| true", "--force"):
            self.assertNotIn(bypass, self.instructions)

    def test_kernel_install_and_fail_closed_known_answer_gate_stay_mandatory(self):
        self.assertIn("&& ijsinstall --install=global", self.instructions)
        self.assertIn("&& pip install --no-cache-dir bash_kernel && python -m bash_kernel.install", self.instructions)
        gate = "RUN timeout --kill-after=5s 85s python /tmp/proxcloud-smoke-kernels.py && rm /tmp/proxcloud-smoke-kernels.py"
        self.assertIn("COPY code-interpreter/smoke-kernels.py /tmp/proxcloud-smoke-kernels.py", self.instructions)
        self.assertIn(gate, self.instructions)
        self.assertLess(self.instructions.index("&& ijsinstall --install=global"), self.instructions.index(gate))
        self.assertLess(self.instructions.index(gate), self.instructions.index("COPY common/setup-user.sh"))
        self.assertIn("RUN proxcloud-setup-user\nWORKDIR /home/user", self.instructions)
        self.assertIn("RUN apt-get update && apt-get upgrade -y", self.instructions)
        self.assertEqual(self.instructions.count("pip install --upgrade pip==26.3"), 2)


if __name__ == "__main__":
    unittest.main(verbosity=2)
