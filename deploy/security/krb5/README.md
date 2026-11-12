# MIT krb5 1.22.3 — experimental source-only candidate

Reviewed public keyring supplied; offline release signature verified. Full build,
ABI/GSS and runtime integration remain NOT_READY; no default image uses this.

## Verified public source metadata (2026-11-12)

[MIT listing](https://web.mit.edu/kerberos/dist/krb5/1.22/) and
[release notes](https://web.mit.edu/kerberos/krb5-1.22/) independently identify
1.22.3. Official source files:

- `krb5-1.22.3.tar.gz`: 8758584 bytes, SHA256
  `fe90af8441ad9fa9b064b8e53077cefb782a96f8a68276b031dc54af1789ff0b`
- `.tar.gz.asc`: 833 bytes, SHA256
  `818d8e52d8602c6fb2a53b4d456fdb7caf217aab89f917a3a3d014610fcba193`

Hashes are independently computed HTTPS content observations, NOT published MIT
checksums or verified signatures. [MIT trust page](https://web.mit.edu/kerberos/dist/)
publishes `C4493CB739F4A89F9852CBC20CBA08575F8372DF`; the signature's issuer
metadata matches. Offline isolated GnuPG verified self/binding signatures, key
validity and the release signature. The builder repeats exact fingerprint,
keyring hash, validity and gpgv checks before extraction. No keyserver import, signature bypass or secret key needed.

## Future authorized hosted build only

Pass explicit `DEBIAN_BASE=debian:trixie-slim@sha256:7378260c0a098d27ac206225db5e7d92b8c7c84ea7afccc86eeb513756bbd392`
(verified Debian13 amd64),
a separately reviewed **local** `trust/mit-krb5-release.gpg` file inside the
explicitly prepared build context, and
`--build-arg MIT_KEYRING_SHA256=<reviewed-64hex>`. Missing local key input must fail;
no named-context registry fallback is used.
Select `--target artifacts --output type=local,dest=<disposable-artifacts>`.
Bound hosted jobs; no PVE, real realm or production credentials.
Live apt: recorded, not snapshot-pinned.

Full generated configure, shared libraries, clients, modules and support data
are retained. `make check` is mandatory; failures and unreviewed skips fail
closed (only 4 reviewed environment skips, listed in the build script). Temporary local
KDC/realm test processes are expected. Full DESTDIR staging is exported under
`EXPERIMENTAL_PREFIX` with file hashes and manifest: **NOT_FOR_INSTALL**, includes
KDC/admin/daemon tools, not a runtime image or Debian package. Never copy this
prefix into controlled images, shadow old libraries, or manipulate apt metadata.

Static source majors match required `libgssapi_krb5.so.2`, `libkrb5.so.3`,
`libk5crypto.so.3`, `libkrb5support.so.0`; builder readelf checks are necessary,
NOT ABI proof. Proper Debian13 source-packaged custom variants must preserve
dpkg dependencies, plugins and GSS CLI/auth. Promotion requires independent source
and trust review, ABI/symbol checks, disposable OpenSSH GSS realm/renewal/rekey
regressions, SBOM/provenance and fresh full-image Trivy. No integration now.

Evidence group has 14 High CVEs x four binaries = 56 rows; MIT release notes name
all 14 fixes. Candidate only: zero rows cleared; no VEX, ignores or feature drops.
