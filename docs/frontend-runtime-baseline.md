# Frontend builder and static runtime baseline

## Bounded selection

The shared [frontend Dockerfile](../web/Dockerfile) selects Node **24.22.2-alpine**
for the builder and NGINX stable **1.30.6-alpine** for the final static server.
Both client/admin builds use it. The [SPA test workflow](../.github/workflows/test.yml)
selects exact Node `24.22.2`; its prior floating `24` choice did not identify a patch.
This removes an end-of-support Node 20 builder and the older NGINX 1.27 series from
these source defaults without changing runtime configuration or frontend packages.

These are compatibility candidates within supported lines, not a claim that all
runtime OS packages/dependencies or every sandbox template are latest or safe.
The separate MCP and template runtimes are not changed. The existing Vite 8.1.6
lock graph, application sources, NGINX configuration, entrypoint, public-config
format, permissions normalization and port 80 are unchanged.

## Metadata and immutable identity observations

Parent retrieved official Node release metadata and checksums on **2026-11-11
Asia/Saigon**. Node `v24.22.2` is Krypton LTS, dated `2026-10-27`. The official
Linux/x64 archive SHA256 is
`48cc0e21e048790e54c0cf5749c2725005313d652651c40b7a21feb9697aaf75`.
These sources are [Node releases](https://nodejs.org/download/release/index.json)
and [release checksums](https://nodejs.org/download/release/v24.22.2/SHASUMS256.txt).

Public registry index metadata was read without pulling or running image layers:

| Candidate image | Observed index digest |
| --- | --- |
| `node:24.22.2-alpine` | `sha256:a34ab6d7f6f4813f4d0c5469d74685604b962fbdbf5793a7bc7b4998ecb867c8` |
| `nginx:1.30.6-alpine` | `sha256:583b7f2fce1cdb8da5ad5c39ebd3c33b98b87d493190e548bdee1fa8c619c311` |

The Dockerfile still uses exact version tags, not digest references. Metadata
availability is not image contents, Alpine native-module compatibility or a
successful NGINX startup. The separately staged Node archive is a glibc Linux
build; tests using that archive must not be described as Alpine builder coverage.

## Required validation distinctions

[The data-only baseline test](../web/tests/test_runtime_baseline.py) checks exact
Dockerfile versions, preserved multi-app copy/entrypoint contract and the hosted
Node selection. The installer workflow runs it using its existing Python/PyYAML
environment. This is a declaration test, not a Docker or browser execution.

Before merge, record independently reviewed source identity and all repository
local gates with actual Node 24.22.2 and Vite 8.1.6 bytes. Cached Node 22 builds do
not count toward the new runtime. Preserve exact client/admin locks and use
bounded isolated output/caches with no host package installs.

Separately verify both actual Alpine builder and final NGINX image: native
module selection, bundle build, NGINX `-t`/version, generated public config,
index and asset content/types, SPA deep-link fallback and cache headers. A Vite
preview login smoke cannot substitute for that actual static-server boundary.
No live identity login, provider mutation or new workload is required to review
this source change. Missing image-runtime evidence stays an explicit release
prerequisite rather than an invented pass.

No image publication, release dispatch or deployment is performed by changing
these source defaults. Dependency advisories and OS image scanning remain separate
from source-version selection; no zero-vulnerability or all-components-latest
assurance is given.
