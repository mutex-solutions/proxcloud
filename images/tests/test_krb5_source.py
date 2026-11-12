"""Data-only experimental krb5 recipe contracts; AUTHORED, NOT RUN here.

No compiler, image, package, source script, signature verifier or realm execution.
These checks cannot prove cryptographic trust, ABI compatibility or CVE repair.
"""
from pathlib import Path
import re
import unittest

ROOT = Path(__file__).resolve().parents[2]
RECIPE = ROOT / 'deploy/security/krb5'
BASE = ('debian:trixie-slim@sha256:'
        '7378260c0a098d27ac206225db5e7d92b8c7c84ea7afccc86eeb513756bbd392')


class Krb5SourceTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.docker = (RECIPE / 'Dockerfile').read_text()
        cls.script = (RECIPE / 'build-krb5.sh').read_text()
        cls.readme = (RECIPE / 'README.md').read_text()

    def test_nondefault_pinned_debian13_amd64_artifact_only(self):
        self.assertRegex(self.docker, r'(?m)^ARG DEBIAN_BASE$')
        self.assertNotRegex(self.docker, r'(?m)^ARG DEBIAN_BASE=')
        self.assertIn('FROM --platform=linux/amd64 ${DEBIAN_BASE} AS builder', self.docker)
        self.assertIn("EXPECTED_BASE='" + BASE + "'", self.script)
        self.assertIn('[ "$VERSION_CODENAME" = trixie ]', self.script)
        self.assertIn('[ "$(dpkg --print-architecture)" = amd64 ]', self.script)
        self.assertIn('FROM scratch AS artifacts\nCOPY --from=builder /out/ /', self.docker)

    def test_official_source_pins_and_mandatory_external_trust(self):
        for value in ('8758584', '833',
                      'fe90af8441ad9fa9b064b8e53077cefb782a96f8a68276b031dc54af1789ff0b',
                      '818d8e52d8602c6fb2a53b4d456fdb7caf217aab89f917a3a3d014610fcba193'):
            self.assertIn(value, self.script)
        self.assertIn('https://web.mit.edu/kerberos/dist/krb5/1.22/', self.script)
        self.assertIn('response.read(size + 1)', self.script)
        self.assertIn('response.geturl() != url', self.script)
        self.assertIn('len(data) != size or hashlib.sha256(data).hexdigest() != sha', self.script)
        self.assertIn('COPY trust/mit-krb5-release.gpg /trust/mit-krb5-release.gpg', self.docker)
        self.assertNotIn('--from=mit-trust', self.docker)
        self.assertIn("re.fullmatch(r'[0-9a-f]{64}', expected)", self.script)
        self.assertIn('hashlib.sha256(keyring.read_bytes()).hexdigest() != expected', self.script)
        self.assertIn('gpgv --homedir /build/gnupg --keyring /trust/mit-krb5-release.gpg', self.script)
        self.assertIn("valid[0][2] != 'C4493CB739F4A89F9852CBC20CBA08575F8372DF'", self.script)
        self.assertIn("'REVKEYSIG', 'EXPKEYSIG', 'EXPSIG'", self.script)
        self.assertIn("row[6] and int(row[6]) <= int(time.time())", self.script)
        self.assertIn("unexpected trust primary fingerprint", self.script)
        self.assertLess(self.script.index('\ngpgv '), self.script.index("archive.extractall("))
        self.assertLess(self.script.index("archive.extractall("), self.script.index('/src/configure --prefix='))
        self.assertIn("filter='data'", self.script)

    def test_signed_apt_full_tests_and_no_failure_waiver(self):
        self.assertIn('Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg', self.script)
        for package in ('gcc', 'g++', 'make', 'pkg-config', 'libssl-dev', 'bison', 'byacc',
                        'autoconf', 'gettext', 'flex', 'libcmocka-dev', 'liblmdb-dev'):
            self.assertIn(package, self.script)
        self.assertIn('make check > /out/check.log 2>&1', self.script)
        # Skips fail closed except an exact, reviewed, environment-inherent list.
        self.assertIn("raise SystemExit('unreviewed skipped tests: '", self.script)
        self.assertLess(self.script.index('make check >'), self.script.index("unreviewed skipped tests"))
        self.assertLess(self.script.index("unreviewed skipped tests"), self.script.index('make DESTDIR=/staging install'))
        reviewed = self.script.split('reviewed = {', 1)[1].split('}', 1)[0]
        self.assertEqual(reviewed.count("'Skipped "), 4)
        for allowed in ('keyring collection tests', 'keyring ccache tests', 't_locate_kdc test', 'LDAP KDB tests'):
            self.assertIn(allowed, reviewed)
        for required in ('softhsm2', 'opensc', 'python3-pyrad', 'python3-kdcproxy', 'libresolv-wrapper'):
            self.assertIn(required, self.script)
        self.assertIn('make DESTDIR=/staging install', self.script)
        self.assertIn('--disable-static --enable-shared --without-system-verto', self.script)
        for bypass in ('--disable-gssapi', '--without-krb5', '--disable-tests',
                       '--allow-unauthenticated', 'trusted=yes', '|| true',
                       '--recv-keys', 'pip install', 'dpkg -i', 'ldconfig'):
            self.assertNotIn(bypass, self.script)

    def test_complete_prefix_manifest_and_no_runtime_clearance(self):
        for name in ('libgssapi_krb5.so.2', 'libkrb5.so.3',
                     'libk5crypto.so.3', 'libkrb5support.so.0'):
            self.assertIn(name, self.script)
        self.assertIn("['readelf', '-d', str(target)]", self.script)
        self.assertIn('shutil.copytree(prefix, out, symlinks=True)', self.script)
        self.assertIn('EXPERIMENTAL_NOT_FOR_INSTALL_NOT_RUNTIME_VALIDATED', self.script)
        self.assertIn('FULL upstream install including KDC/admin/daemon tools', self.script)
        self.assertIn('NOT integrated; no dpkg replacement or old-library shadowing', self.script)
        self.assertIn('14 CVEs/56 rows are candidates only', self.script)
        self.assertIn('NOT_READY', self.readme)
        self.assertLessEqual(len(self.readme.encode()), 3000)


if __name__ == '__main__':
    unittest.main(verbosity=2)
