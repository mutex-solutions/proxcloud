"""Source-only contracts for the future hosted custom-client test image.

AUTHORED, NOT EXECUTED in this change. These tests parse/read source only; they
never execute the client, packages, image, server or test driver. A future hosted
container result is required; neither these checks nor smoke tests prove the
changed-hostkey UAF fix, GSS realm operation, or CVE/scanner clearance.
"""
import ast
from pathlib import Path
import re
import unittest

ROOT = Path(__file__).resolve().parents[2]
RECIPE = ROOT / 'deploy/security/openssh'
BASE = 'debian:trixie-slim@sha256:7378260c0a098d27ac206225db5e7d92b8c7c84ea7afccc86eeb513756bbd392'
VERSION = '1:10.0p1-7+deb13u4+proxcloud1'


class SSHClientRegressionSourceTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.dockerfile = (RECIPE / 'Dockerfile.client-test').read_text()
        cls.script = (RECIPE / 'test-client.py').read_text()
        cls.tree = ast.parse(cls.script)
        cls.instructions = '\n'.join(line for line in cls.dockerfile.splitlines()
                                     if not line.lstrip().startswith('#'))

    def test_fixed_base_input_and_entrypoint_not_runtime_or_named_context(self):
        self.assertEqual(re.findall(r'^FROM (.+)$', self.instructions, re.M), [BASE])
        copies = re.findall(r'^COPY (.+)$', self.instructions, re.M)
        self.assertEqual(copies, ['client-package/openssh-client.deb /tmp/openssh-client.deb',
                                 'test-client.py /opt/openssh-client-test.py'])
        self.assertIn('USER 1000:1000', self.instructions)
        self.assertIn('ENTRYPOINT ["/usr/bin/python3", "-I", "/opt/openssh-client-test.py"]',
                      self.instructions)
        self.assertNotIn('--from=', self.instructions)
        self.assertNotIn('ARG ', self.instructions)
        self.assertNotIn('EXPOSE ', self.instructions)
        self.assertNotIn('VOLUME ', self.instructions)

    def test_autostart_denied_before_apt_then_complete_custom_package(self):
        self.assertLess(self.instructions.index('exit 101'),
                        self.instructions.index('apt-get update --error-on=any'))
        self.assertIn('openssh-server openssh-client python3 passwd', self.instructions)
        self.assertIn('&& /usr/sbin/useradd ', self.instructions)
        self.assertIn('useradd --uid 1000 --create-home --shell /bin/sh --password NP testclient',
                      self.instructions)
        self.assertIn('PATH=/usr/sbin:/usr/bin:/sbin:/bin dpkg -i /tmp/openssh-client.deb', self.instructions)
        self.assertIn('dpkg-deb -f /tmp/openssh-client.deb Package', self.instructions)
        self.assertIn('dpkg-deb -f /tmp/openssh-client.deb Architecture', self.instructions)
        self.assertEqual(self.instructions.count("'" + VERSION + "'"), 2)
        self.assertNotIn('apt-get', self.instructions.split('dpkg -i', 1)[1])
        self.assertNotIn('--allow-unauthenticated', self.instructions)

    def test_documented_network_none_cap_zero_readonly_limits_and_enforcement(self):
        for flag in ('--network none', '--user 1000:1000', '--cap-drop ALL',
                     '--security-opt no-new-privileges', '--pids-limit 128',
                     '--memory 1g', '--cpus 2', '--read-only',
                     '--tmpfs /tmp:rw,nosuid,nodev,mode=1777'):
            self.assertIn(flag, self.dockerfile)
        for contract in ('os.getuid() == 1000', 'os.getgid() == 1000', "status['CapEff']",
                         "status['NoNewPrivs']", "== {'lo'}", 'os.ST_RDONLY',
                         'signal.alarm(180)', 'start_new_session=True',
                         'os.killpg(process.pid, signal.SIGKILL)', 'process.wait(timeout=5)',
                         'timeout=20', 'for process in reversed(PROCESSES)'):
            self.assertIn(contract, self.script)
        self.assertIn('cleanup_errors = []', self.script)
        self.assertIn('all(process.poll() is not None for process in PROCESSES)', self.script)
        self.assertIn('owned_fixture_groups_killed_and_leaders_reaped', self.script)
        stop = next(node for node in self.tree.body if isinstance(node, ast.FunctionDef) and node.name == 'stop')
        self.assertFalse(any(isinstance(node, ast.If) for node in ast.walk(stop)))
        self.assertNotIn('shell=True', self.script)
        self.assertNotIn('os.environ', self.script)

    def test_both_loopbacks_pinned_keys_no_password_no_external_probe(self):
        for item in ('socket.AF_INET6', "probe.bind(('::1', 0))", 'ListenAddress 127.0.0.1',
                     'ListenAddress ::1', 'UsePAM no', 'PermitRootLogin no',
                     'PasswordAuthentication no', 'KbdInteractiveAuthentication no',
                     'AuthenticationMethods publickey', 'AllowUsers testclient',
                     '-oStrictHostKeyChecking=yes', '-oGlobalKnownHostsFile=/dev/null',
                     '-oUserKnownHostsFile=', '-oUpdateHostKeys=no',
                     "TemporaryDirectory(prefix='ssh-client-', dir='/tmp')"):
            self.assertIn(item, self.script)
        self.assertNotIn('ssh-keyscan', self.script)
        self.assertNotIn('urllib', self.script)
        self.assertNotIn('StrictHostKeyChecking=no', self.script)
        self.assertNotIn('unittest.skip', self.script)

    def test_smoke_coverage_and_honest_nonproof_summary(self):
        for item in ('exact_custom_package_version', 'public_key_exec_',
                     'same_hostkey_rekey_smoke_', 'sftp_batch_roundtrip',
                     'scp_roundtrip_', "('legacy_scp', ['-O'])", '-oIdentityFile=none',
                     'ssh_agent_add_and_authenticate', '-oRekeyLimit=1K',
                     "debug.count(b'SSH2_MSG_NEWKEYS sent') >= 2",
                     "debug.count(b'SSH2_MSG_NEWKEYS received') >= 2",
                     'debian_gss_option_parsing_only', '-oGSSAPIKeyExchange=yes',
                     '-oGSSAPIRenewalForcesRekey=yes', '-oGSSAPIAuthentication=yes',
                     'changed_hostkey_UAF_regression', 'GSS_realm_auth_or_rekey',
                     'CVE_clearance', 'production_integration', "'status': 'FAIL'",
                     'raise SystemExit(1)'):
            self.assertIn(item, self.script)
        # Parse only: never import/execute the runtime driver in a source check.
        imports = [node.module for node in ast.walk(self.tree) if isinstance(node, ast.ImportFrom)]
        self.assertNotIn('test_client', imports)


if __name__ == '__main__':
    unittest.main(verbosity=2)
