"""Data-only contracts for the EXPERIMENTAL OpenSSH artifact recipe.

Authored, NOT RUN in this source-only change. These checks do not apply patches,
execute source/packages, contact networks, compile, run Docker or prove CVE/GSS
repair. Independent patch review and real hosted native/security/GSS tests remain
mandatory before integration; no scanner suppression is authorized.
"""
import hashlib
from pathlib import Path
import re
import unittest

ROOT = Path(__file__).resolve().parents[2]
RECIPE = ROOT / 'deploy/security/openssh'


class OpenSSHBackportSourceTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.dockerfile = (RECIPE / 'Dockerfile').read_text()
        cls.script = (RECIPE / 'build-backport.sh').read_text()
        cls.patch = (RECIPE / 'CVE-2026-60002.patch').read_bytes()
        cls.commands = '\n'.join(line for line in cls.script.splitlines()
                                 if not line.lstrip().startswith('#'))

    def test_reviewed_candidate_exact_bytes_and_complete_four_file_scope(self):
        self.assertEqual(len(self.patch), 6196)
        self.assertEqual(hashlib.sha256(self.patch).hexdigest(),
                         '97c677a7125ff9ab0670d95e7a690213dbc61fd799fd077047bf0dc7c655725d')
        patch = self.patch.decode()
        self.assertEqual(re.findall(r'^\+\+\+ b/(.+)$', patch, re.M),
                         ['ssh.c', 'sshconnect.c', 'sshconnect.h', 'sshconnect2.c'])
        self.assertIn('+\txxx_host = xstrdup(host);', patch)
        self.assertIn('+\txxx_hostaddr = *hostaddr;', patch)
        self.assertIn('+\txxx_conn_info = ssh_conn_info_dup(cinfo);', patch)
        self.assertEqual(len(re.findall(r'^\+\tret->\w+ = xstrdup\(cinfo->\w+\);$',
                                        patch, re.M)), 12)
        self.assertIn('CANDIDATE', patch)

    def test_artifact_only_build_with_required_reviewed_debian13_pin(self):
        self.assertIn('ARG DEBIAN_BASE=debian:trixie-slim', self.dockerfile)
        self.assertIn('FROM ${DEBIAN_BASE} AS builder', self.dockerfile)
        self.assertIn('FROM scratch AS artifacts\nCOPY --from=builder /out/ /',
                      self.dockerfile)
        self.assertIn('RUN sh /bootstrap/build-backport.sh', self.dockerfile)
        self.assertIn("r'debian:trixie-slim@sha256:[0-9a-f]{64}'", self.script)
        self.assertIn('[ "$VERSION_CODENAME" = trixie ]', self.script)
        self.assertIn('[ "$(dpkg --print-architecture)" = amd64 ]', self.script)

    def test_exact_source_pins_size_bounds_and_mandatory_signature_gate(self):
        for sha, size in (
            ('73fed3fd77d60925ed342bcb0afd3c037e4ea0d39333107bf617aa90f859910f', 3763),
            ('021a2e709a0edf4250b1256bd5a9e500411a90dddabea830ed59cef90eb9d85c', 1972675),
            ('102e1065030c6002acabd7f896eeba1462bf54b4d7393bac34b0308312868ec6', 215600),
        ):
            self.assertIn(sha, self.script)
            self.assertIn(str(size), self.script)
        self.assertIn('response.read(size + 1)', self.script)
        self.assertIn('response.geturl() != url', self.script)
        self.assertIn('len(data) != size or hashlib.sha256(data).hexdigest() != expected', self.script)
        verify = 'apt-get source --download-only --only-source "openssh=$SOURCE_VERSION"'
        extract = 'dpkg-source -x openssh_10.0p1-7+deb13u4.dsc openssh-source'
        self.assertIn(verify, self.commands)
        self.assertLess(self.commands.index(verify), self.commands.index(extract))
        self.assertIn('Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg', self.script)
        self.assertIn('Suites: trixie trixie-updates', self.script)
        self.assertIn('Suites: trixie-security', self.script)
        self.assertIn('apt-get update --error-on=any', self.commands)
        self.assertIn('apt-cache\', \'showsrc\', \'--only-source\', \'openssh', self.script)
        self.assertIn("entries.get(filename) != identity", self.script)
        self.assertIn("exact pinned source missing from authenticated indexes", self.script)
        self.assertIn("debian-signed-repository-source-hash-chain", self.script)
        self.assertIn("NOT_VERIFIED_missing_public_key", self.script)
        self.assertIn('Acquire::Check-Valid-Until "true";', self.script)
        self.assertIn('Acquire::Check-Date "true";', self.script)
        self.assertLess(self.commands.index('apt-get update --error-on=any'), self.commands.index(verify))
        self.assertLess(self.commands.index('APT-downloaded source differs'), self.commands.index(extract))
        for bypass in ('--allow-unauthenticated', '--allow-insecure-repositories',
                       'trusted=yes', '--no-check', '|| true', 'nocheck', 'DEB_BUILD_OPTIONS='):
            self.assertNotIn(bypass, self.commands)

    def test_full_debian_stack_then_candidate_with_no_feature_pruning(self):
        self.assertIn('apt-get build-dep -y ./openssh-source', self.commands)
        self.assertIn("len(names) != 43 or names[0] != 'gssapi.patch' or applied != names", self.script)
        self.assertIn("export QUILT_PATCH_OPTS='--fuzz=0'", self.commands)
        self.assertIn('quilt import -P CVE-2026-60002.patch /bootstrap/CVE-2026-60002.patch\n', self.commands)
        self.assertIn('\nquilt push\n', self.commands)
        self.assertIn("len(names) != 44 or names[-1] != 'CVE-2026-60002.patch'", self.script)
        self.assertIn("series.startswith(original)", self.script)
        self.assertIn("'Debian GSS key exchange patch changed'", self.script)
        self.assertIn("BACKPORT_VERSION='1:10.0p1-7+deb13u4+proxcloud1'", self.script)
        self.assertIn('dpkg-buildpackage -b -us -uc', self.commands)
        for bypass in ('--nocheck', '-d -us', '--disable-gssapi', '--without-kerberos5'):
            self.assertNotIn(bypass, self.commands)

    def test_client_only_export_no_install_or_scanner_clearance_claim(self):
        self.assertIn("client='openssh-client_10.0p1-7+deb13u4+proxcloud1_amd64.deb'", self.script)
        self.assertIn('cp "$client" /out/', self.commands)
        self.assertIn('dpkg-deb --info "$client" > /out/openssh-client.info', self.commands)
        self.assertIn("'backport-manifest.json'", self.script)
        self.assertIn('NOT_INSTALLED_NOT_RUNTIME_SECURITY_VALIDATED', self.script)
        self.assertIn('Custom version may remain flagged', self.script)
        self.assertNotIn('dpkg -i', self.commands)
        self.assertNotIn('apt-get install -y openssh', self.commands)
        self.assertNotIn('openssh-server_', self.commands)
        self.assertNotIn('ssh-keygen -A', self.commands)


if __name__ == '__main__':
    unittest.main(verbosity=2)
