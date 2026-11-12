#!/bin/sh
# EXPERIMENTAL: authored only; never executed during source review.
# This script is ONLY for a future isolated disposable Debian 13 hosted builder.
# It executes authenticated Debian packaging/source and runs the normal tests.
# It neither installs the resulting packages nor proves CVE/GSS runtime repair.
set -eu
umask 022
export LC_ALL=C.UTF-8
export DEBFULLNAME='Proxcloud experimental build'
export DEBEMAIL='proxcloud-build@invalid.example'
export QUILT_PATCHES=debian/patches
export QUILT_PATCH_OPTS='--fuzz=0'
SOURCE_VERSION='1:10.0p1-7+deb13u4'
BACKPORT_VERSION='1:10.0p1-7+deb13u4+proxcloud1'
PATCH_SHA256='97c677a7125ff9ab0670d95e7a690213dbc61fd799fd077047bf0dc7c655725d'
export SOURCE_VERSION BACKPORT_VERSION PATCH_SHA256

# No inherited test-skip profiles/options, injected build flags or alternate suites.
[ -z "${DEB_BUILD_OPTIONS:-}" ]
[ -z "${DEB_BUILD_PROFILES:-}" ]
[ -z "${DEB_CFLAGS_MAINT_APPEND:-}" ]
[ -z "${DEB_LDFLAGS_MAINT_APPEND:-}" ]
. /etc/os-release
[ "$ID" = debian ]
[ "$VERSION_CODENAME" = trixie ]
[ "$(dpkg --print-architecture)" = amd64 ]
# Pinning is a hard gate, not just an example in a comment.
case "${DEBIAN_BASE:-}" in
    debian:trixie-slim@sha256:*) ;;
    *) echo 'Set DEBIAN_BASE to an independently reviewed Debian 13 sha256 digest' >&2; exit 1 ;;
esac
[ "${#DEBIAN_BASE}" -eq 90 ]
# The fixed base is required to contain only its standard Debian source file.
# Do not silently retain a foreign/unsigned repository from another base image.
[ ! -s /etc/apt/sources.list ]
for entry in /etc/apt/sources.list.d/*; do
    [ "$entry" = /etc/apt/sources.list.d/debian.sources ]
done
cat > /etc/apt/sources.list.d/debian.sources <<'SOURCES'
Types: deb deb-src
URIs: http://deb.debian.org/debian
Suites: trixie trixie-updates
Components: main
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg

Types: deb deb-src
URIs: http://deb.debian.org/debian-security
Suites: trixie-security
Components: main
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg
SOURCES
# No insecure/unauthenticated switches; signed Release checks remain mandatory.
apt-get update
apt-get install -y --no-install-recommends \
    ca-certificates python3 debian-keyring debian-archive-keyring \
    build-essential devscripts dpkg-dev debhelper quilt fakeroot gnupg
mkdir /build /out
cd /build

# Bounded Python stdlib HTTPS only; no curl pipe, network code or tar execution.
# These TLS-observed consistency pins are NOT a replacement for the DSC trust gate.
python3 - <<'PY'
import hashlib
import os
from pathlib import Path
import re
import urllib.request

base = os.environ['DEBIAN_BASE']
if not re.fullmatch(r'debian:trixie-slim@sha256:[0-9a-f]{64}', base):
    raise SystemExit('invalid pinned Debian base digest')
files = [
    ('openssh_10.0p1-7+deb13u4.dsc', 'openssh_10.0p1-7%2Bdeb13u4.dsc', 3763,
     '73fed3fd77d60925ed342bcb0afd3c037e4ea0d39333107bf617aa90f859910f'),
    ('openssh_10.0p1.orig.tar.gz', 'openssh_10.0p1.orig.tar.gz', 1972675,
     '021a2e709a0edf4250b1256bd5a9e500411a90dddabea830ed59cef90eb9d85c'),
    ('openssh_10.0p1-7+deb13u4.debian.tar.xz', 'openssh_10.0p1-7%2Bdeb13u4.debian.tar.xz', 215600,
     '102e1065030c6002acabd7f896eeba1462bf54b4d7393bac34b0308312868ec6'),
    ('openssh_10.0p1.orig.tar.gz.asc', 'openssh_10.0p1.orig.tar.gz.asc', 833,
     'f71cf3240516ce59c2240532143982fcd97e2e286e6eb1eb6c3080a1ec012a04'),
]
for name, encoded, size, expected in files:
    url = 'https://deb.debian.org/debian/pool/main/o/openssh/' + encoded
    with urllib.request.urlopen(url, timeout=60) as response:
        if response.geturl() != url:
            raise SystemExit('unexpected redirect: ' + name)
        data = response.read(size + 1)
    if len(data) != size or hashlib.sha256(data).hexdigest() != expected:
        raise SystemExit('source size/SHA256 mismatch: ' + name)
    Path(name).write_bytes(data)
Path('source-pins.sha256').write_text(''.join(
    f'{sha}  {name}\n' for name, _, _, sha in files))
patch = Path('/bootstrap/CVE-2026-60002.patch').read_bytes()
if len(patch) != 6196 or hashlib.sha256(patch).hexdigest() != os.environ['PATCH_SHA256']:
    raise SystemExit('candidate patch size/SHA256 mismatch')
dsc = Path(files[0][0]).read_text()
if not re.search(r'^Version: ' + re.escape(os.environ['SOURCE_VERSION']) + r'$', dsc, re.M):
    raise SystemExit('wrong DSC version')
PY

# Required cryptographic source-chain gate BEFORE extraction/patch/build.
# debian-keyring is authenticated by signed apt metadata. An absent uploader key,
# bad/expired signature or archive checksum is a hard failure: NEVER bypass it.
# Orig .asc is DSC-bound data; no independent upstream signature claim is made.
dscverify --keyring /usr/share/keyrings/debian-keyring.gpg openssh_10.0p1-7+deb13u4.dsc
dpkg-source -x openssh_10.0p1-7+deb13u4.dsc openssh-source
# Resolve ALL dependencies from the exact authenticated source control, not a
# potentially newer mirror control. Local directory build-dep is supported by apt.
apt-get build-dep -y ./openssh-source
cd /build/openssh-source
python3 - <<'PY'
import hashlib
from pathlib import Path
series = Path('debian/patches/series').read_bytes()
names = [line.split()[0] for line in series.decode().splitlines()
         if line.strip() and not line.lstrip().startswith('#')]
applied = Path('.pc/applied-patches').read_text().splitlines()
if len(names) != 43 or names[0] != 'gssapi.patch' or applied != names:
    raise SystemExit('expected complete 43-patch Debian quilt stack already applied')
Path('/build/original-series.sha256').write_text(hashlib.sha256(series).hexdigest())
Path('/build/original-series').write_bytes(series)
Path('/build/gssapi.sha256').write_text(
    hashlib.sha256(Path('debian/patches/gssapi.patch').read_bytes()).hexdigest())
PY
quilt import -P CVE-2026-60002.patch /bootstrap/CVE-2026-60002.patch
# Fuzz zero; any rejected hunk/nonzero exit stops the build.
quilt push
python3 - <<'PY'
import hashlib
from pathlib import Path
original = Path('/build/original-series').read_bytes()
series = Path('debian/patches/series').read_bytes()
if not series.startswith(original):
    raise SystemExit('existing Debian series changed')
names = [line.split()[0] for line in series.decode().splitlines()
         if line.strip() and not line.lstrip().startswith('#')]
if (len(names) != 44 or names[-1] != 'CVE-2026-60002.patch'
        or Path('.pc/applied-patches').read_text().splitlines() != names):
    raise SystemExit('candidate is not the sole appended/applied patch')
if hashlib.sha256(Path('debian/patches/gssapi.patch').read_bytes()).hexdigest() != Path('/build/gssapi.sha256').read_text():
    raise SystemExit('Debian GSS key exchange patch changed')
if hashlib.sha256(Path('debian/patches/CVE-2026-60002.patch').read_bytes()).hexdigest() != __import__('os').environ['PATCH_SHA256']:
    raise SystemExit('quilt changed the reviewed candidate patch')
PY
# Keep Debian rules/features unchanged (including GSSKEX/Kerberos/FIDO/libedit).
dch --newversion "$BACKPORT_VERSION" --distribution trixie --force-distribution \
    'EXPERIMENTAL CVE-2026-60002 backport of upstream e8bdfb151a356d0171fea4194dd205fbb252be23; retain Debian GSSKEX and all 43 distro patches. Not a vendor fixed release or runtime proof.'
dpkg-buildpackage -b -us -uc

# May compile server binaries internally, but export ONLY the client package and
# review metadata. Never dpkg -i/apt install a produced client/server package here.
cd /build
client='openssh-client_10.0p1-7+deb13u4+proxcloud1_amd64.deb'
[ "$(dpkg-deb -f "$client" Package)" = openssh-client ]
[ "$(dpkg-deb -f "$client" Version)" = "$BACKPORT_VERSION" ]
[ "$(dpkg-deb -f "$client" Architecture)" = amd64 ]
cp "$client" /out/
dpkg-deb --info "$client" > /out/openssh-client.info
cp /bootstrap/CVE-2026-60002.patch /out/
cp source-pins.sha256 /out/
cp openssh_10.0p1-7+deb13u4+proxcloud1_amd64.buildinfo /out/
python3 - <<'PY'
import hashlib
import json
import os
from pathlib import Path
out = Path('/out')
manifest = {
    'status': 'EXPERIMENTAL_BUILT_NOT_INSTALLED_NOT_RUNTIME_SECURITY_VALIDATED',
    'cve': 'CVE-2026-60002',
    'source_version': os.environ['SOURCE_VERSION'],
    'backport_version': os.environ['BACKPORT_VERSION'],
    'debian_base': os.environ['DEBIAN_BASE'],
    'source_authentication': 'dscverify required; Debian keyring from signed Debian apt',
    'upstream_repair': 'e8bdfb151a356d0171fea4194dd205fbb252be23',
    'debian_original_series_sha256': Path('/build/original-series.sha256').read_text(),
    'debian_gssapi_patch_sha256': Path('/build/gssapi.sha256').read_text(),
    'existing_debian_patch_count': 43,
    'files': {p.name: {'sha256': hashlib.sha256(p.read_bytes()).hexdigest(),
                       'bytes': p.stat().st_size} for p in sorted(out.iterdir())},
    'remaining_gates': [
        'independent lifetime/security review and complete source diff',
        'native package/client/helper/config and changed-host-key rekey regression',
        'IPv4/IPv6/proxy coverage and full disposable GSS realm/renewal/rekey parity',
        'SBOM/provenance and fresh full-image High/Critical scan after approved integration',
        'explicit granular owner VEX only after actual build/patch/security proof',
    ],
    'scanner': 'Custom version may remain flagged; NO fixed-version, ignore or VEX claim',
}
(out / 'backport-manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
PY
