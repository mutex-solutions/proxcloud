"""Data-only NONDEFAULT prototype guards; authored, not run during source work.

Parse smoke source, never import/execute it, install packages, or build images.
These contracts do not prove runtime, inherited feature parity, or CVE acceptance.
"""
import ast
from pathlib import Path
import re
import unittest

ROOT = Path(__file__).resolve().parents[2]


class ControlledBaseSourceTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.docker = (ROOT / "deploy/templates/base/Dockerfile.controlled").read_text()
        cls.smoke = (ROOT / "deploy/templates/base/smoke-controlled.py").read_text()
        cls.gh_source = (ROOT / "deploy/security/gh/Dockerfile").read_text()
        cls.runtime = cls.docker.split("FROM docker.io/library/python:", 1)[1]
        cls.tree = ast.parse(cls.smoke)
        cls.constants = {node.targets[0].id: ast.literal_eval(node.value)
                         for node in cls.tree.body if isinstance(node, ast.Assign)
                         and isinstance(node.targets[0], ast.Name)}

    def test_nondefault_official_pins_and_metadata_not_runtime_claim(self):
        self.assertIn("SOURCE_CANDIDATE", self.docker)
        self.assertIn("NONDEFAULT prototype", self.docker)
        self.assertIn("no layer/runtime execution evidence", self.docker)
        self.assertIn("FROM docker.io/library/node:22-bookworm-slim@sha256:"
                      "47725aca53d3295ec2d3be8f62726599b3033cc262edc2c7e429909b3ac599fc AS node-toolchain", self.docker)
        self.assertIn("FROM docker.io/library/python:3.11-slim-trixie@sha256:"
                      "b701198cb708b68d9413ca67d6311bd97c9389a2c5fe4da0511fc652642366cd", self.docker)
        self.assertNotIn("e2bdev/base", self.docker)
        default = (ROOT / "deploy/templates/base/Dockerfile").read_text()
        self.assertNotIn("Dockerfile.controlled", default)
        self.assertNotIn("smoke-controlled.py", default)

    def test_explicit_core_packages_and_no_database_erasure(self):
        for package in ("bash", "ca-certificates", "sudo", "git", "build-essential",
                        "curl", "wget", "pkg-config", "libstdc++6", "openssh-client",
                        "passwd", "util-linux", "patch", "xz-utils", "unzip", "zip", "tar",
                        "bzip2", "libsqlite3-dev", "libssl-dev", "zlib1g-dev"):
            self.assertRegex(self.docker, r"(?<![\w-])" + re.escape(package) + r"(?![\w-])")
        self.assertIn("apt-get update && apt-get upgrade -y", self.docker)
        self.assertIn("--no-install-recommends", self.docker)
        self.assertEqual(re.findall(r"rm -rf ([^\n]+)", self.docker), ["/var/lib/apt/lists/*"])
        self.assertNotIn("apk ", self.runtime)
        self.assertNotIn("--ignore", self.docker)
        self.assertIn("NOT feature-equivalent", self.docker)
        self.assertIn("Owner approved splitting inherited image/codec", self.docker)

    def test_gh_is_patched_upstream_not_a_shadowed_distro_package(self):
        apt = re.search(r"apt-get install -y --no-install-recommends(.*?)&& rm", self.docker, re.S).group(1)
        self.assertNotIn("gh", apt.replace("\\\n", " ").split())
        self.assertNotIn("COPY base/install-gh.sh", self.docker)
        self.assertNotIn("proxcloud-install-gh", self.docker)
        self.assertEqual(re.findall(r"^COPY --from=gh-builder (.+)$", self.runtime, re.M),
                         ["/out/gh /usr/local/bin/gh",
                          "/out/LICENSE /usr/local/share/doc/gh/LICENSE"])
        self.assertIn('.split()[:3] == ["gh", "version", "2.103.3+proxcloud1"]', self.smoke)
        self.assertIn("CVE-2026-48501 vendor fix", self.runtime)
        self.assertIn("integrated runtime scan still required", self.runtime)

    def test_gh_builder_reuses_reviewed_source_stage_without_drift(self):
        source_stage = self.gh_source.split("FROM golang:", 1)[1].split("# Export only:", 1)[0].strip()
        controlled_stage = self.docker.split("FROM golang:", 1)[1].split(
            "FROM docker.io/library/python:", 1)[0].strip()
        self.assertEqual(controlled_stage, source_stage.replace(" AS builder\n", " AS gh-builder\n", 1))
        self.assertEqual(re.findall(r"^FROM (.+)$", self.docker, re.M)[1],
                         "golang:1.27.4-alpine AS gh-builder")
        self.assertTrue(re.findall(r"^FROM (.+)$", self.docker, re.M)[0].endswith(" AS node-toolchain"))
        for gate in ("GOTOOLCHAIN=local", "CGO_ENABLED=0",
                     "git clone --no-checkout https://github.com/cli/cli.git cli",
                     "git checkout --detach 63837fb778d6a39f02be0ce76ff89f2bc8d74bdc",
                     'test "$(git rev-parse HEAD)" = 63837fb778d6a39f02be0ce76ff89f2bc8d74bdc',
                     "db11ba3547fe6420c763b5fb208bb5e41a44598895f94481be01ea967fc421e2 go.mod",
                     "sha256sum -c -", "= v0.60.0;", "= v0.62.0;",
                     "go mod edit -require=golang.org/x/net@v0.62.0", "go mod tidy;",
                     "go mod verify > /out/module-verification.txt",
                     "go test -mod=readonly ./internal/build ./internal/ghrepo golang.org/x/net/html",
                     "go build -mod=readonly -trimpath -buildvcs=false",
                     "build.Version=2.103.3+proxcloud1", "go version -m /out/gh",
                     '$2 == "golang.org/x/net" && $3 == "v0.62.0"',
                     "cp LICENSE /out/LICENSE", "SOURCE_BUILD_NOTICE.txt > SHA256SUMS"):
            self.assertIn(gate, controlled_stage)

    def test_selective_donor_and_bootstrap_links_precede_npm(self):
        copies = re.findall(r"^COPY --from=node-toolchain (.+)$", self.docker, re.M)
        self.assertEqual(copies, ["/usr/local/bin/node /usr/local/bin/node",
                                 "/usr/local/lib/node_modules/npm /usr/local/lib/node_modules/npm"])
        install = self.docker.index("&& npm install -g --bin-links=false")
        for script, executable in (("npm-cli.js", "npm"), ("npx-cli.js", "npx")):
            link = "ln -sfn ../lib/node_modules/npm/bin/" + script + " /usr/local/bin/" + executable
            self.assertLess(self.docker.index(link), install)
        self.assertIn("npm@11.21.2 corepack@0.36.0 yarn@1.22.22", self.docker)
        after_install = self.docker[install:]
        for script, executable in (("npm-cli.js", "npm"), ("npx-cli.js", "npx")):
            link = "ln -sfn ../lib/node_modules/npm/bin/" + script + " /usr/local/bin/" + executable
            self.assertIn(link, after_install)
            self.assertLess(after_install.index(link), after_install.index("&& npm cache clean --force"))
        for script, executable in (("corepack/dist/corepack.js", "corepack"),
                                   ("yarn/bin/yarn.js", "yarn"), ("yarn/bin/yarn.js", "yarnpkg")):
            self.assertIn("ln -sfn ../lib/node_modules/" + script + " /usr/local/bin/" + executable, self.docker)
        self.assertNotIn("&& corepack enable", self.docker)
        self.assertIn("--only-binary=:all: --no-deps --upgrade", self.docker)
        self.assertIn("pip==26.3 setuptools==84.2.1 wheel==0.48.0 packaging==26.3", self.docker)

    def test_smoke_is_fixed_offline_and_bounded(self):
        self.assertLessEqual(len(self.smoke.splitlines()), 100)
        self.assertEqual(self.constants["MARKER"], "PROXCLOUD_CONTROLLED_BASE_OK")
        self.assertEqual(self.constants["VERSIONS"], {"node": "v22.23.5", "npm": "11.21.2",
                         "npx": "11.21.2", "corepack": "0.36.0", "yarn": "1.22.22", "yarnpkg": "1.22.22"})
        self.assertIn("sys.version_info[:3] == (3, 11, 18)", self.smoke)
        self.assertIn("check=True, capture_output=True, text=True", self.smoke)
        self.assertIn("timeout=25, stdin=subprocess.DEVNULL", self.smoke)
        for forbidden in ("shell=True", "sys.argv", "os.system", "requests", "urllib", "socket.connect"):
            self.assertNotIn(forbidden, self.smoke)
        self.assertIn('tempfile.TemporaryDirectory(prefix="proxcloud-controlled-")', self.smoke)
        for evidence in ("ssl.create_default_context()", 'sqlite3.connect(":memory:")',
                         'ctypes.CDLL(None)', '"c++"', '"cc", "-shared", "-fPIC"',
                         'sysconfig.get_path("include")', 'native.answer() == 42',
                         '["fs","http","crypto"]', '["git", "--version"]', '["gh", "--version"]',
                         '["ssh", "-V"]', 'pwd.getpwnam("user")', 'sudoers.read_text()',
                         '["su", "-s", "/bin/bash", "user", "-c"'):
            self.assertIn(evidence, self.smoke)
        ast.parse(self.constants["USER_CHECK"])
        self.assertIn('os.getuid() == 1000', self.constants["USER_CHECK"])
        self.assertIn('"/code"', self.constants["USER_CHECK"])
        self.assertIn('"/usr/local/lib/python3.11/site-packages"', self.constants["USER_CHECK"])

    def test_shared_setup_precedes_explicit_candidate_only_gate(self):
        self.assertIn("COPY common/setup-user.sh /usr/local/sbin/proxcloud-setup-user", self.docker)
        self.assertLess(self.docker.index("RUN proxcloud-setup-user"), self.docker.index("COPY base/smoke-controlled.py"))
        self.assertIn("RUN timeout --kill-after=5s 90s /usr/local/bin/python3 /tmp/proxcloud-smoke-controlled.py", self.docker)
        self.assertIn("&& rm /tmp/proxcloud-smoke-controlled.py\nWORKDIR /home/user", self.docker)
        self.assertIn("all seven derived builds, SDK/MCP", self.docker)
        self.assertIn("raw SBOM/scanner/security review", self.docker)


if __name__ == "__main__":
    unittest.main()
