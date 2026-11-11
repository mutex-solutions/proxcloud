# Dependency patch baseline: Vite and Ansible

This first dependency patch is deliberately narrow. The dated inventory found
outdated/support and advisory questions throughout the stack; changing two pins
is not a claim that ProxCloud or its runtime images are vulnerability-free.

## Exact changes

| Dependency | Before | This patch | Why |
| --- | --- | --- | --- |
| Both frontend Vite direct dev dependencies/locks | `8.1.5` (manifest `^8.1.5`) | exact `8.1.6` | Same-line fixes for five reviewed Vite advisories, without unrelated graph resolution |
| Installer `ansible-core` requirement | exact `2.18.12` | exact `2.18.18` | Same-line stable fix covering the reviewed Galaxy role-source argument issue |

Vite registry metadata, artifact SHA-512 integrity and SHA-1 shasum were verified
before using the archive as test input. Its dependency/optional/peer requirements,
engine declaration and bin mapping match the existing locked graph, so only the
root Vite declaration and `node_modules/vite` version/URL/integrity change in each
lock. No unrelated package, CLI or transitive lock entry is re-resolved. The Ansible
wheel is verified against its PyPI-published SHA-256; this does not add a complete
Python environment lock or certify cached transitives.

Official metadata: [Vite 8.1.6](https://registry.npmjs.org/vite/8.1.6),
[Ansible 2.18.18](https://pypi.org/pypi/ansible-core/2.18.18/json),
[Ansible advisory](https://github.com/advisories/GHSA-w8p5-mx5w-cpqj).
The Vite fixes include [middleware handling](https://github.com/advisories/GHSA-9jrq-w75r-8gcw),
[dev file serving](https://github.com/advisories/GHSA-rq7h-c2jc-7f22),
[editor handling](https://github.com/advisories/GHSA-r825-w8mm-5pxw),
[Windows filename handling](https://github.com/advisories/GHSA-587h-7wmj-h5pm) and
[Windows percent expansion](https://github.com/advisories/GHSA-698v-4r3x-3vcv).

## Exposure and test boundaries

Vite is a build/dev dependency, not the nginx static-serving process in production.
The advisory conditions involve specific development middleware, network exposure,
Windows/WSL or editor paths; matching the package version does not establish those
conditions in a deployed portal. Ansible's issue concerns Galaxy role installation;
the source installer does not thereby become a remotely exposed portal endpoint.
The fixes are still appropriate dependency hygiene.

Validation must execute the **new verified package bytes**, not reuse a cached
8.1.5 build or 2.18.12 syntax check and call it coverage of this patch. Parent tests
use read-only verified package payloads inside isolated no-external-network test
sandboxes, alongside lock-matched existing transitives and owned browser binaries.
No target install script or dependency code runs on the host. Native frontend
build/smoke and installer tests/syntax remain required.

## Explicit follow-ups

- Go supported compiler baseline, Go advisory import/symbol triage and compatible
  module updates are separate from this pin patch.
- Node/NGINX image support, OS package/SBOM closure, image digest pinning and all
  sandbox-template/global dependency graphs remain separate.
- PostgreSQL minor image maps, new-provider PG18 offerings, operator/CRD matrices,
  backup/restore and existing major migrations require their own review/gates.
- Other OSV matches, including packages needing major migrations or no maintained
  fix, are not declared closed here. Upstream advisory IDs include aliases and are
  not an application-exploit or unique-CVE count.
- `8.1.6` and `2.18.18` are minimal reviewed security-floor candidates, not a claim
  that every dependency now equals the newest feature release or that the older
  Ansible branch gains a new long-term-support promise.

No live installer apply, VM/cloud mutation, mail, payment, release dispatch or
existing data upgrade is authorized by this dependency-only source change.
