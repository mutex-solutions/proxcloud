#!/bin/sh
# SOURCE_CANDIDATE: used only by the explicitly selected NONDEFAULT prototype.
# Future image-build installer; not run as part of this source-only change.
# CVE-2026-48501 / GHSA-8xvp-7hj6-mcj9: vendor fix starts at v2.93.0.
# https://github.com/cli/cli/security/advisories/GHSA-8xvp-7hj6-mcj9
# Official metadata checked 2026-11-12 (public HTTPS; no credentials):
# https://api.github.com/repos/cli/cli/releases/424663522
# v2.103.3: immutable=true, commit 63837fb778d6a39f02be0ce76ff89f2bc8d74bdc.
# Archive asset 679195621: SHA256 agrees with both API digest and checksum file:
# https://github.com/cli/cli/releases/download/v2.103.3/gh_2.103.3_checksums.txt
# Checksum-file SHA256: f756e7859d90dbb5e85aae5194206c37da220bec9bac23d3d659078a8e766fdc
# This is an upstream installation, not a Debian security backport or a VEX waiver.
# Keep dpkg/SBOM records intact; future scanner must inventory this Go binary too.
set -eu

[ "$(id -u)" = 0 ]
[ "$(dpkg --print-architecture)" = amd64 ]
# Never shadow a vulnerable distro installation or erase its package metadata.
[ ! -e /usr/bin/gh ]
[ "$(dpkg-query -W -f='${Status}' gh 2>/dev/null || true)" != 'install ok installed' ]

workdir=$(mktemp -d /tmp/proxcloud-gh.XXXXXXXXXX)
cleanup() {
    # mktemp returns an absolute private directory; guard the only recursive delete.
    case "$workdir" in
        /tmp/proxcloud-gh.??????????) [ ! -L "$workdir" ] && rm -rf -- "$workdir" ;;
        *) return 1 ;;
    esac
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
archive="$workdir/gh.tar.gz"
curl --fail --silent --show-error --location \
    --proto '=https' --proto-redir '=https' --tlsv1.2 \
    --connect-timeout 15 --max-time 120 --max-filesize 20000000 \
    --output "$archive" \
    https://github.com/cli/cli/releases/download/v2.103.3/gh_2.103.3_linux_amd64.tar.gz
[ "$(wc -c < "$archive")" -eq 15031972 ]
printf '%s  %s\n' \
    4c806978ffce1cec70853302a56d70ac6da4f19bae7adca4e1a8ab00633dd91d \
    "$archive" | sha256sum --check --strict -
# Hash precedes all archive parsing; stream only the two reviewed regular members.
# No archive paths/permissions, scripts, or other payloads are installed.
tar -xOzf "$archive" gh_2.103.3_linux_amd64/bin/gh > "$workdir/gh"
tar -xOzf "$archive" gh_2.103.3_linux_amd64/LICENSE > "$workdir/LICENSE"
printf '%s  %s\n' \
    b768184570b12536be4ac1528893f6340d1c6a7b40b56ad45bca9cd0e87ad34d "$workdir/gh" \
    6da4adc42392c8485e40b4251c7e332fc3352df1947c9ffade71dd60b14a7a4f "$workdir/LICENSE" \
    | sha256sum --check --strict -
install -m 0755 "$workdir/gh" /usr/local/bin/gh
install -d -m 0755 /usr/local/share/doc/gh
install -m 0644 "$workdir/LICENSE" /usr/local/share/doc/gh/LICENSE
# No gh invocation/authentication here. Exact-version offline smoke and complete
# OS + Go-module/stdlib SBOM/scanning remain mandatory future acceptance gates.
