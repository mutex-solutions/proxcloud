"""Data-only base toolchain guards, not compiler/native or CVE acceptance."""
import json
import pathlib
import re
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[2]


class BaseToolchainTests(unittest.TestCase):
    def setUp(self):
        self.base = (ROOT / "deploy/templates/base/Dockerfile").read_text()
        self.mcp = (ROOT / "deploy/templates/mcp-gateway/Dockerfile").read_text()

    def test_donor_is_exact_same_glibc_release_and_shared(self):
        ref = "docker.io/library/node:22-bookworm-slim@sha256:47725aca53d3295ec2d3be8f62726599b3033cc262edc2c7e429909b3ac599fc"
        self.assertIn("FROM " + ref + " AS node-toolchain", self.base)
        self.assertIn("FROM " + ref + " AS node", self.mcp)
        self.assertIn("FROM docker.io/e2bdev/base@sha256:8aebbf5e7d1209ebdc4bbc5999624556c2d9eebe91b9fded756ff2d688367065", self.base)

    def test_only_old_npm_tree_is_replaced_not_the_whole_global_directory(self):
        removals = re.findall(r"rm -rf ([^\n]+)", self.base)
        self.assertEqual(removals, ["/var/lib/apt/lists/*", "/usr/local/lib/node_modules/npm"])
        self.assertLess(self.base.index("RUN rm -rf /usr/local/lib/node_modules/npm"),
                        self.base.index("COPY --from=node-toolchain /usr/local/lib/node_modules/npm"))
        self.assertIn("COPY --from=node-toolchain /usr/local/bin/node /usr/local/bin/node", self.base)

    def test_cli_entrypoints_and_exact_versions_fail_closed(self):
        for package, executable in (("corepack/dist/corepack.js", "corepack"),
                                    ("yarn/bin/yarn.js", "yarn"), ("yarn/bin/yarn.js", "yarnpkg")):
            self.assertIn("ln -sfn ../lib/node_modules/" + package + " /usr/local/bin/" + executable, self.base)
        for command, version in (("node", "v22.23.5"), ("npm", "11.21.2"),
                                 ("corepack", "0.36.0"), ("yarn", "1.22.22")):
            self.assertIn('test "$(' + command + ' --version)" = ' + version, self.base)
        self.assertNotIn("&& corepack enable", self.base)
        self.assertIn("--bin-links=false corepack@0.36.0 yarn@1.22.22", self.base)

    def test_packaging_and_user_conventions_preserved(self):
        self.assertIn("pip==26.3 setuptools==84.2.1 wheel==0.48.0 packaging==26.3", self.base)
        self.assertIn("--only-binary=:all: --no-deps --upgrade", self.base)
        self.assertIn("apt-get update && apt-get upgrade -y", self.base)
        self.assertIn("RUN proxcloud-setup-user\nWORKDIR /home/user", self.base)

    def test_mcp_servers_install_from_the_committed_lock(self):
        directory = ROOT / "deploy/templates/mcp-gateway"
        manifest = json.loads((directory / "package.json").read_text())
        lock = json.loads((directory / "package-lock.json").read_text())
        self.assertEqual(lock["lockfileVersion"], 3)
        # The lock is the one of this package.json: same exact pins, every server and override resolved.
        self.assertEqual(lock["packages"][""]["dependencies"], manifest["dependencies"])
        for name, version in manifest["dependencies"].items():
            self.assertRegex(version, r"^\d+\.\d+\.\d+$")
            self.assertEqual(lock["packages"]["node_modules/" + name]["version"], version)
        for name, version in manifest["overrides"].items():
            found = [entry["version"] for path, entry in lock["packages"].items()
                     if path.endswith("node_modules/" + name)]
            self.assertTrue(found)
            self.assertEqual(set(found), {version})
        for path, entry in lock["packages"].items():
            if path and not entry.get("inBundle"):  # a bundled package ships inside its parent's tarball
                self.assertTrue(entry["resolved"].startswith("https://registry.npmjs.org/"), path)
                self.assertTrue(entry["integrity"].startswith("sha512-"), path)
        self.assertIn("COPY mcp-gateway/package.json mcp-gateway/package-lock.json /opt/mcp/npm/lib/\n", self.mcp)
        self.assertIn("\nnpm ci --prefix /opt/mcp/npm/lib --install-strategy=shallow --omit=dev ", self.mcp)
        self.assertNotIn("npm install --prefix", self.mcp)
        # The build's own check of the overrides names the same versions.
        self.assertIn('const want = { "@modelcontextprotocol/sdk": "%s", "basic-ftp": "%s" };'
                      % (manifest["overrides"]["@modelcontextprotocol/sdk"], manifest["overrides"]["basic-ftp"]),
                      self.mcp)


if __name__ == "__main__":
    unittest.main()
