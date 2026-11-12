#!/usr/bin/python3
"""Future hosted disposable-container checks; never run on a host/runtime image.

Requires --network none (including IPv4 AND IPv6 loopback), read-only root,
UID/GID 1000, no capabilities, no-new-privileges, and writable /tmp tmpfs.
All keys/data are generated here, never read from a real user's SSH directory.
Rekey smoke is NOT a changed-hostkey UAF reproduction or GSS realm proof.
The parent must authenticate the input .deb via the retained build manifest.
"""
import contextlib
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import tempfile
import time

VERSION = '1:10.0p1-7+deb13u4+proxcloud1'
PROCESSES = []
RESULTS = []


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def stop(process):
    # The leader can exit before its children. Always kill the owned group.
    with contextlib.suppress(ProcessLookupError):
        os.killpg(process.pid, signal.SIGKILL)
    process.wait(timeout=5)


def run(args, *, data=None, env=None, timeout=20):
    # No shell or inherited SSH agent/config. Each command has a bounded lifetime
    # and its own process group so a timed-out SSH/SCP child cannot be orphaned.
    process = subprocess.Popen(args, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                               stderr=subprocess.PIPE, env=env, start_new_session=True)
    try:
        out, err = process.communicate(data, timeout=timeout)
    finally:
        stop(process)
    require(process.returncode == 0,
            f'{args[0]} failed ({process.returncode}): {err.decode(errors="replace")[-6000:]}')
    return out, err


def start(args, env, log):
    process = subprocess.Popen(args, stdin=subprocess.DEVNULL, stdout=log,
                               stderr=log, env=env, start_new_session=True)
    PROCESSES.append(process)
    return process


def bounded_total(signum, frame):
    raise TimeoutError('180-second total test deadline exceeded')


def sandbox_contract():
    require(os.getuid() == 1000 and os.getgid() == 1000, 'requires UID/GID 1000')
    status = dict(line.split(':', 1) for line in Path('/proc/self/status').read_text().splitlines()
                  if ':' in line)
    require(int(status['CapEff'].strip(), 16) == 0, 'effective capabilities must be zero')
    require(status['NoNewPrivs'].strip() == '1', 'requires no-new-privileges')
    require({p.name for p in Path('/sys/class/net').iterdir()} == {'lo'},
            'requires an isolated network namespace containing ONLY lo')
    require(os.statvfs('/').f_flag & os.ST_RDONLY, 'requires read-only root filesystem')
    # Presence of ONLY lo is not a replacement for the parent's --network none
    # enforcement. No external address is probed, and no host mount is needed.


def main():
    sandbox_contract()
    signal.signal(signal.SIGALRM, bounded_total)
    signal.alarm(180)
    with tempfile.TemporaryDirectory(prefix='ssh-client-', dir='/tmp') as tmp:
        work = Path(tmp)
        env = {'PATH': '/usr/bin:/bin', 'LC_ALL': 'C.UTF-8', 'HOME': tmp,
               'TMPDIR': tmp, 'SSH_AUTH_SOCK': str(work / 'agent.sock')}
        try:
            version, _ = run(['/usr/bin/dpkg-query', '-W', '-f=${Version}', 'openssh-client'], env=env)
            require(version.decode() == VERSION, 'wrong client package version')
            RESULTS.append('exact_custom_package_version')
            for key in ('host', 'identity'):
                run(['/usr/bin/ssh-keygen', '-q', '-t', 'ed25519', '-N', '',
                     '-f', str(work / key)], env=env)
            (work / 'authorized_keys').write_bytes((work / 'identity.pub').read_bytes())
            host_public = ' '.join((work / 'host.pub').read_text().split()[:2])
            # Fail (never skip) if IPv6 loopback is unavailable on the hosted runner.
            with socket.socket(socket.AF_INET6) as probe:
                probe.bind(('::1', 0))
            with socket.socket(socket.AF_INET) as probe:
                probe.bind(('127.0.0.1', 0))
                port = probe.getsockname()[1]
            known = work / 'known_hosts'
            known.write_text(f'[127.0.0.1]:{port} {host_public}\n[::1]:{port} {host_public}\n')
            config = work / 'sshd_config'
            config.write_text(f'''Port {port}
ListenAddress 127.0.0.1
ListenAddress ::1
HostKey {work / 'host'}
PidFile {work / 'sshd.pid'}
AuthorizedKeysFile {work / 'authorized_keys'}
StrictModes no
UsePAM no
PermitRootLogin no
PasswordAuthentication no
KbdInteractiveAuthentication no
PubkeyAuthentication yes
AuthenticationMethods publickey
AllowUsers testclient
AllowAgentForwarding no
AllowTcpForwarding no
X11Forwarding no
PermitUserEnvironment no
PrintMotd no
Subsystem sftp internal-sftp
LogLevel VERBOSE
''')
            run(['/usr/sbin/sshd', '-t', '-f', str(config)], env=env)
            with (work / 'sshd.log').open('wb') as server_log, (work / 'agent.log').open('wb') as agent_log:
                server = start(['/usr/sbin/sshd', '-D', '-e', '-f', str(config)], env, server_log)
                agent = start(['/usr/bin/ssh-agent', '-D', '-a', env['SSH_AUTH_SOCK']], env, agent_log)
                deadline = time.monotonic() + 10
                while True:
                    require(server.poll() is None and agent.poll() is None,
                            'fixture exited: ' + (work / 'sshd.log').read_text(errors='replace')[-6000:])
                    ready = True
                    for family, address in ((socket.AF_INET, '127.0.0.1'), (socket.AF_INET6, '::1')):
                        try:
                            with socket.socket(family) as probe:
                                probe.settimeout(0.2)
                                probe.connect((address, port))
                        except OSError:
                            ready = False
                    if ready and Path(env['SSH_AUTH_SOCK']).exists():
                        break
                    require(time.monotonic() < deadline, 'fixture startup timeout')
                    time.sleep(0.05)
                common = ['-F', '/dev/null', '-oBatchMode=yes', '-oConnectTimeout=5',
                          '-oConnectionAttempts=1', '-oStrictHostKeyChecking=yes',
                          '-oGlobalKnownHostsFile=/dev/null', '-oUserKnownHostsFile=' + str(known),
                          '-oUpdateHostKeys=no', '-oPasswordAuthentication=no',
                          '-oKbdInteractiveAuthentication=no', '-oGSSAPIAuthentication=no',
                          '-oIdentitiesOnly=yes', '-i', str(work / 'identity')]
                for family, address in (('-4', '127.0.0.1'), ('-6', '::1')):
                    ssh = ['/usr/bin/ssh', family, *common, '-p', str(port), 'testclient@' + address]
                    out, _ = run([*ssh, "printf 'CUSTOM_CLIENT_EXEC_OK\\n'"], env=env)
                    require(out == b'CUSTOM_CLIENT_EXEC_OK\n', 'SSH exec output mismatch')
                    RESULTS.append('public_key_exec_' + family[1:])
                    out, debug = run([ssh[0], '-vv', '-oRekeyLimit=1K', *ssh[1:],
                                      'head -c 262144 /dev/zero'], env=env)
                    require(out == bytes(262144), 'rekey transfer data mismatch')
                    require(debug.count(b'SSH2_MSG_NEWKEYS sent') >= 2 and
                            debug.count(b'SSH2_MSG_NEWKEYS received') >= 2,
                            'did not observe a completed second key exchange')
                    RESULTS.append('same_hostkey_rekey_smoke_' + family[1:])
                payload = bytes(range(256)) * 1024
                (work / 'upload').write_bytes(payload)
                batch = f'put {work}/upload {work}/sftp-remote\nget {work}/sftp-remote {work}/sftp-return\n'
                run(['/usr/bin/sftp', *common, '-P', str(port), '-b', '-',
                     'testclient@127.0.0.1'], data=batch.encode(), env=env)
                require((work / 'sftp-return').read_bytes() == payload, 'SFTP roundtrip mismatch')
                RESULTS.append('sftp_batch_roundtrip')
                for mode, extra in (('default_sftp', []), ('legacy_scp', ['-O'])):
                    remote = f'testclient@127.0.0.1:{work}/scp-{mode}'
                    run(['/usr/bin/scp', *common, '-P', str(port), *extra,
                         str(work / 'upload'), remote], env=env)
                    returned = work / ('scp-return-' + mode)
                    run(['/usr/bin/scp', *common, '-P', str(port), *extra,
                         remote, str(returned)], env=env)
                    require(returned.read_bytes() == payload, 'SCP roundtrip mismatch: ' + mode)
                    RESULTS.append('scp_roundtrip_' + mode)
                run(['/usr/bin/ssh-add', str(work / 'identity')], env=env)
                # Force agent-only authentication: no -i or default private files.
                agent_opts = common[:-3] + ['-oIdentitiesOnly=no', '-oIdentityFile=none']
                out, _ = run(['/usr/bin/ssh', *agent_opts, '-p', str(port),
                              'testclient@127.0.0.1', "printf 'AGENT_OK\\n'"], env=env)
                require(out == b'AGENT_OK\n', 'agent-only authentication mismatch')
                RESULTS.append('ssh_agent_add_and_authenticate')
                out, _ = run(['/usr/bin/ssh', '-G', '-F', '/dev/null',
                              '-oGSSAPIKeyExchange=yes', '-oGSSAPIRenewalForcesRekey=yes',
                              '-oGSSAPIAuthentication=yes', 'testclient@127.0.0.1'], env=env)
                options = dict(line.split(None, 1) for line in out.decode().splitlines() if ' ' in line)
                for key in ('gssapikeyexchange', 'gssapirenewalforcesrekey', 'gssapiauthentication'):
                    require(options.get(key) in ('yes', 'true'), 'GSS option missing: ' + key)
                RESULTS.append('debian_gss_option_parsing_only')
        finally:
            signal.alarm(0)
            cleanup_errors = []
            for process in reversed(PROCESSES):
                try:
                    stop(process)
                except (OSError, subprocess.TimeoutExpired) as error:
                    cleanup_errors.append(type(error).__name__)
            require(not cleanup_errors, 'fixture process cleanup failed: ' + ','.join(cleanup_errors))
            require(all(process.poll() is not None for process in PROCESSES),
                    'fixture leader still running after group teardown')
            RESULTS.append('owned_fixture_groups_killed_and_leaders_reaped')
    print(json.dumps({'status': 'PASS', 'package_version': VERSION, 'checks': RESULTS,
                      'not_proven': ['changed_hostkey_UAF_regression', 'GSS_realm_auth_or_rekey',
                                     'CVE_clearance', 'production_integration']}, sort_keys=True))


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        print(json.dumps({'status': 'FAIL', 'completed_checks': RESULTS, 'error': str(error)}), flush=True)
        raise SystemExit(1)
