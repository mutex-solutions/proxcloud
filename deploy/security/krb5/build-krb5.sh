#!/bin/sh
# SOURCE-ONLY authored candidate. Execute ONLY in a future authorized disposable
# hosted builder; never on a workstation, controlled runtime, PVE or real realm.
set -eu
umask 022
export LC_ALL=C.UTF-8
VERSION=1.22.3
PREFIX=/opt/proxcloud-krb5
export VERSION PREFIX
EXPECTED_BASE='debian:trixie-slim@sha256:7378260c0a098d27ac206225db5e7d92b8c7c84ea7afccc86eeb513756bbd392'
[ "${DEBIAN_BASE:?explicit pinned base required}" = "$EXPECTED_BASE" ]
[ -n "${MIT_KEYRING_SHA256:?reviewed offline MIT keyring SHA256 required}" ]
export DEBIAN_BASE MIT_KEYRING_SHA256
. /etc/os-release
[ "$ID" = debian ]
[ "$VERSION_CODENAME" = trixie ]
[ "$(dpkg --print-architecture)" = amd64 ]
# No caller-supplied test skips, linker injection or configure substitutions.
[ -z "${DEB_BUILD_OPTIONS:-}" ]
[ -z "${DEB_BUILD_PROFILES:-}" ]
[ -z "${MAKEFLAGS:-}" ]
[ -z "${CONFIG_SITE:-}" ]
[ -z "${LD_PRELOAD:-}" ]
[ -z "${LD_LIBRARY_PATH:-}" ]
[ ! -s /etc/apt/sources.list ]
for entry in /etc/apt/sources.list.d/*; do
    [ "$entry" = /etc/apt/sources.list.d/debian.sources ]
done
# Signed apt Release validation bootstraps HTTPS CA/key/build dependencies.
cat > /etc/apt/sources.list.d/debian.sources <<'SOURCES'
Types: deb
URIs: http://deb.debian.org/debian
Suites: trixie trixie-updates
Components: main
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg

Types: deb
URIs: http://deb.debian.org/debian-security
Suites: trixie-security
Components: main
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg
SOURCES
apt-get update
apt-get install -y --no-install-recommends \
    ca-certificates debian-archive-keyring python3 gnupg gpgv \
    gcc g++ make pkg-config libc6-dev binutils libssl-dev bison byacc \
    autoconf gettext flex dejagnu expect tcl libcmocka-dev liblmdb-dev \
    libldap2-dev libsasl2-dev libkeyutils-dev \
    softhsm2 opensc python3-pyrad python3-kdcproxy libresolv-wrapper systemd
mkdir /build /staging /out
# BuildKit discards failed-layer files: expose bounded fixed diagnostics in its
# retained outer log without suppressing the original failure or skipping tests.
report_failure() {
    status=$?
    trap - EXIT
    if [ "$status" -ne 0 ]; then
        for log in /out/configure.log /out/build.log /out/check.log /out/install.log; do
            if [ -f "$log" ]; then
                printf '\n--- failed stage diagnostic: %s ---\n' "$log" >&2
                tail -c 24576 "$log" >&2
            fi
        done
    fi
    exit "$status"
}
trap report_failure EXIT
cd /build
# Official HTTPS listing and release independently read 2026-11-12. These are
# observed content hashes, NOT vendor-published checksum/signature verification.
python3 - <<'PY'
import hashlib
import os
from pathlib import Path
import re
import urllib.request
keyring = Path('/trust/mit-krb5-release.gpg')
expected = os.environ['MIT_KEYRING_SHA256']
if not re.fullmatch(r'[0-9a-f]{64}', expected):
    raise SystemExit('NOT_READY: invalid independently reviewed keyring SHA256')
if hashlib.sha256(keyring.read_bytes()).hexdigest() != expected:
    raise SystemExit('MIT trust keyring hash mismatch')
files = [
    ('krb5-1.22.3.tar.gz', 8758584,
     'fe90af8441ad9fa9b064b8e53077cefb782a96f8a68276b031dc54af1789ff0b'),
    ('krb5-1.22.3.tar.gz.asc', 833,
     '818d8e52d8602c6fb2a53b4d456fdb7caf217aab89f917a3a3d014610fcba193'),
]
for name, size, sha in files:
    url = 'https://web.mit.edu/kerberos/dist/krb5/1.22/' + name
    with urllib.request.urlopen(url, timeout=60) as response:
        if response.geturl() != url:
            raise SystemExit('unexpected source redirect')
        data = response.read(size + 1)
    if len(data) != size or hashlib.sha256(data).hexdigest() != sha:
        raise SystemExit('source size/SHA256 mismatch: ' + name)
    Path(name).write_bytes(data)
Path('/out/source-pins.sha256').write_text(''.join(
    f'{sha}  {name}\n' for name, size, sha in files))
PY
# Independent official trust anchor: https://web.mit.edu/kerberos/dist/ publishes
# C4493CB739F4A89F9852CBC20CBA08575F8372DF. No online keyserver auto-import.
# Mandatory signature + exact signer check BEFORE extraction or source execution.
mkdir /build/gnupg
chmod 700 /build/gnupg
gpgv --homedir /build/gnupg --keyring /trust/mit-krb5-release.gpg \
    --status-fd 1 krb5-1.22.3.tar.gz.asc krb5-1.22.3.tar.gz \
    > /out/signature-status.txt
python3 - <<'PY'
from pathlib import Path
import subprocess
import time
listing = subprocess.check_output(['gpg', '--batch', '--no-options', '--homedir', '/build/gnupg',
    '--no-auto-key-retrieve', '--auto-key-locate', 'clear', '--with-colons',
    '--with-fingerprint', '--show-keys', '/trust/mit-krb5-release.gpg'], text=True)
rows = [line.split(':') for line in listing.splitlines()]
if len([r for r in rows if r[0] == 'pub']) != 1 or any(r[0] in ('sec', 'ssb') for r in rows):
    raise SystemExit('unexpected trust key contents')
if next(r[9] for r in rows if r[0] == 'fpr') != 'C4493CB739F4A89F9852CBC20CBA08575F8372DF':
    raise SystemExit('unexpected trust primary fingerprint')
for row in rows:
    if row[0] in ('pub', 'sub') and (row[1] in ('r', 'e', 'd', 'i') or (row[6] and int(row[6]) <= int(time.time()))):
        raise SystemExit('revoked, expired or invalid trust key')
Path('/out/key-validity.txt').write_text(listing)
records = Path('/out/signature-status.txt').read_text().splitlines()
if any(line.startswith('[GNUPG:] ' + code) for line in records for code in
       ('BADSIG', 'ERRSIG', 'REVKEYSIG', 'EXPKEYSIG', 'EXPSIG', 'KEYEXPIRED', 'SIGEXPIRED', 'NO_PUBKEY', 'FAILURE')):
    raise SystemExit('invalid release signature status')
valid = [s.split() for s in records if s.startswith('[GNUPG:] VALIDSIG ')]
if len(valid) != 1 or valid[0][2] != 'C4493CB739F4A89F9852CBC20CBA08575F8372DF':
    raise SystemExit('unexpected MIT release signer')
PY
# Reject path traversal/special files before extraction; never extract as root
# into /usr or another live runtime prefix. Source code stays confined to /build.
python3 - <<'PY'
from pathlib import PurePosixPath
import tarfile
with tarfile.open('krb5-1.22.3.tar.gz', 'r:gz') as archive:
    members = archive.getmembers()
    for m in members:
        p = PurePosixPath(m.name)
        if p.is_absolute() or '..' in p.parts or p.parts[0] != 'krb5-1.22.3':
            raise SystemExit('unsafe archive path')
        if not (m.isfile() or m.isdir() or m.issym() or m.islnk()):
            raise SystemExit('unexpected archive special file')
    archive.extractall('/build', filter='data')
PY
mkdir /build/obj
cd /build/obj
# Generated upstream configure; no autoreconf/pip and no GSS/client/plugin pruning.
# Bundled verto keeps the complete default source build self-contained.
/build/krb5-1.22.3/src/configure --prefix="$PREFIX" \
    --disable-static --enable-shared --without-system-verto > /out/configure.log 2>&1
make -j2 > /out/build.log 2>&1
# Full upstream suite may spawn temporary loopback KDCs/realms inside this
# disposable builder. No existing realm, credentials or external services allowed.
make check > /out/check.log 2>&1
# Upstream can return success while omitting optional tests: fail closed instead.
# Test dependencies available in Debian 13 are installed above (SoftHSM/OpenSC
# PKCS11, pyrad OTP, kdcproxy, resolv_wrapper URI discovery, socket activation).
# Only these exact skips are reviewed as environment-inherent; any other skip fails:
#  - keyring ccache/collection: container seccomp denies keyctl/add_key, so keyctl
#    is deliberately not installed (installing it turns the skip into false failures);
#  - t_locate_kdc: needs public DNS SRV records for ATHENA.MIT.EDU (not hermetic);
#  - LDAP KDB: server-side KDB plugin (not built here), irrelevant to client libraries.
[ -f skiptests ]
cp skiptests /out/skiptests.txt
python3 - <<'PY'
from pathlib import Path
reviewed = {
    'Skipped keyring collection tests: keyring support not built',
    'Skipped keyring ccache tests: keyring support not built',
    'Skipped t_locate_kdc test: known DNS name not found',
    'Skipped LDAP KDB tests: LDAP KDB module not built',
}
lines = [line.strip() for line in Path('/out/skiptests.txt').read_text().splitlines() if line.strip()]
unexpected = sorted(set(lines) - reviewed)
if unexpected:
    raise SystemExit('unreviewed skipped tests: ' + '; '.join(unexpected))
PY
make DESTDIR=/staging install > /out/install.log 2>&1
# Static ELF inspection, not execution of the newly built programs/libraries.
python3 - <<'PY'
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
prefix = Path('/staging/opt/proxcloud-krb5')
required = ['libgssapi_krb5.so.2', 'libkrb5.so.3',
            'libk5crypto.so.3', 'libkrb5support.so.0']
for name in required:
    library = prefix / 'lib' / name
    target = library.resolve(strict=True)
    if not target.is_relative_to(prefix):
        raise SystemExit('library escapes staging prefix')
    dynamic = subprocess.check_output(['readelf', '-d', str(target)], text=True)
    if f'Library soname: [{name}]' not in dynamic:
        raise SystemExit('required Debian client SONAME missing: ' + name)
# Export COMPLETE upstream tree including modules, support data, client tools,
# KDC/admin programs: artifact NOT_FOR_INSTALL, never a runtime copy allowlist.
out = Path('/out/EXPERIMENTAL_PREFIX')
shutil.copytree(prefix, out, symlinks=True)
files = {}
for p in sorted(out.rglob('*')):
    name = str(p.relative_to(out))
    if p.is_symlink():
        files[name] = {'symlink': os.readlink(p)}
    elif p.is_file():
        files[name] = {'bytes': p.stat().st_size,
                       'sha256': hashlib.sha256(p.read_bytes()).hexdigest()}
packages = subprocess.check_output(
    ['dpkg-query', '-W', '-f=${binary:Package}\t${Version}\t${Architecture}\n'], text=True)
Path('/out/build-packages.tsv').write_text(packages)
manifest = {
    'status': 'EXPERIMENTAL_NOT_FOR_INSTALL_NOT_RUNTIME_VALIDATED',
    'upstream': 'MIT krb5 1.22.3', 'debian_base': os.environ['DEBIAN_BASE'],
    'prefix': '/opt/proxcloud-krb5',
    'keyring_sha256': os.environ['MIT_KEYRING_SHA256'],
    'release_signer': 'C4493CB739F4A89F9852CBC20CBA08575F8372DF',
    'required_sonames': required, 'files': files,
    'tests': 'make check passed; only reviewed environment skips (see skiptests.txt); NOT Debian/OpenSSH ABI or GSS proof',
    'contents': 'FULL upstream install including KDC/admin/daemon tools; artifact only',
    'promotion': 'BLOCKED: proper Debian13 source packaging + ABI/GSS/Trivy gates',
    'runtime': 'NOT integrated; no dpkg replacement or old-library shadowing',
    'security': '14 CVEs/56 rows are candidates only; no VEX/ignore/gate subtraction',
}
Path('/out/build-manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
PY
