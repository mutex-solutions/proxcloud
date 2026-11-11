# Supported Go build compiler baseline

## Decision and scope

The selected build/test/release compiler is **Go 1.26.11**. Parent-refreshed
[official Go release metadata](https://go.dev/dl/?mode=json&include=all) dated
**2026-11-11** identifies **Go 1.27.4** as globally latest stable and **1.26.11** as
the supported 1.26-line patch. Staying on 1.26 is an explicit compatibility choice,
not an assertion that this is the latest compiler series. Support follows the
[Go release policy](https://go.dev/doc/devel/release#policy).

This source-only patch pins all six Go builder stages in five Dockerfiles to
`1.26.11-bookworm`, preserving existing registry prefixes. The
[test workflow](../.github/workflows/test.yml) selects exact `go-version: '1.26.11'`.
These are exact version tags, **not digest pins**: registry tag existence, image
platforms, digests and OS package contents still require separate verification.
No unverified digest is introduced.

The [root module](../go.mod) retains **`go 1.25.0`**. That directive sets the module
language/minimum Go version; it does not identify the compiler used to produce a
binary or the version of the linked standard library/runtime. It is not a support
promise for building or releasing with Go 1.25. No module versions, checksums or
`toolchain` directive change in this compiler-selection patch.

## Six builder stages, seven output files

| Dockerfile / stage | Selected builder | Output / source |
|---|---|---|
| [API](../deploy/Dockerfile), `build` | `golang:1.26.11-bookworm` | `proxcloud-api` / `./cmd/api` |
| [Sandbox proxy](../deploy/sandbox-proxy.Dockerfile), `build` | `golang:1.26.11-bookworm` | `sandbox-proxy` / `./cmd/sandbox-proxy` |
| [Sandbox importer](../deploy/sandbox-import.Dockerfile), `build` | `golang:1.26.11-bookworm` | `sandbox-import` / `./cmd/sandbox-import` |
| [Sandbox runtime](../deploy/sandbox-runtime.Dockerfile), `init` | `golang:1.26.11-bookworm` | `sandbox-init` **and** `sandbox-netguard` / their root-module command packages |
| [MCP gateway](../deploy/templates/mcp-gateway/Dockerfile), `gateway` | `docker.io/library/golang:1.26.11-bookworm` | `mcp-gateway` / `./cmd/mcp-gateway` |
| [Sandbox runtime](../deploy/sandbox-runtime.Dockerfile), `envd` | `golang:1.26.11-bookworm` | External `envd` / pinned upstream `packages/envd` |

There are **six root-module binaries** (API, proxy, importer, init, netguard and MCP
gateway), plus **external envd**, for **seven outputs**. Stages, Dockerfiles and
output files are deliberately counted separately.

The external source remains pinned to E2B infra commit
`e85b9f5f6de6c92b002f376b4f91b1652d3c942e`. Its separately recorded Go minimum of
**1.26.10** is satisfied by 1.26.11. Its `packages/envd` module replaces a dependency
with sibling `packages/shared`; neither that source tree nor its complete module
inputs are provided by the root module's cache. The pin does not prove envd has
compiled. Keep the raw commit and sibling replacement intact, and obtain and
validate the separate inputs before claiming that seventh output.

Existing build flags, target choices (including the importer's explicit
Linux/amd64), artifact copy paths, runtime OS images, users and entrypoints remain
unchanged. The MCP named repository build context is preserved. Node, NGINX,
frontend dependency graphs and other runtime images are outside this patch.

## What the static test proves

[The baseline unit test](../internal/config/go_build_baseline_test.go) reads source
files only. It verifies that its package-relative `../../` root identifies this
module, checks all six expected Go `FROM` stages and exact image tags, rejects
additional unaccounted Go stages in those five Dockerfiles, parses the test
workflow's Go setup selections, and guards the separate root minimum. It does not
invoke Docker, Git, a compiler, a downloaded program or a network request.

A passing static test is **not** compiler compatibility, an artifact build, a
runtime test, an envd test or proof that deployed binaries changed. The ordinary
root unit suite does not include the separate external envd module.

The [regular Docker matrix](../.github/workflows/docker.yml) builds API,
sandbox-proxy and sandbox-runtime (alongside the two SPAs). It does **not** build
sandbox-import or MCP gateway. MCP gateway uses the separate
[sandbox-template workflow](../.github/workflows/sandbox-templates.yml) and
[named-context build path](../deploy/templates/build.sh). A green regular matrix
must not be reported as seven-output Go coverage.

## Validation gates and evidence limits

**No command, build, test, download, installation, publication or deployment was
performed while making this source-only change.** The new static test is added,
not reported as executed. Parent-owned validation must record its actual results
separately.

Before reporting compiler validation complete:

1. Verify the exact compiler/archive and candidate image identities. Keep target
   execution inside the approved isolated environment with read-only source and
   prepared inputs, no external network, no cloud credentials and no host Docker
   socket. Use `GOTOOLCHAIN=local`, `GOPROXY=off`, `GOSUMDB=off`, read-only module
   mode and isolated writable scratch/caches to prevent implicit substitutions or
   downloads.
2. Compile all six root outputs with the selected Go 1.26.11 compiler, preserving
   each existing Linux/CGO/architecture choice. Record source identity, output
   hash, size, target platform and `go version -m` compiler stamp for each file.
3. Separately prepare envd/shared sources, manifests, sums and selected modules at
   the unchanged commit. Compile envd under Go 1.26.11 and collect the same output
   evidence. Missing inputs leave external envd validation **pending**.
4. Run the root unit suite and relevant integration suites, including API-tagged
   tests, under that actual compiler in the approved profile. Report race tests
   separately; static `CGO_ENABLED=0` artifact builds are not race coverage.
5. Treat final Docker image builds/runtime checks as a separate gate. Existing
   recipes include Git clone, module download, apt/npm/uv and Chrome resolution;
   preparing a Go archive alone does not make complete images buildable offline.

Old Go 1.25 results, cached test passes and already-built artifacts cannot be
credited to Go 1.26.11. This version selection alone does **not** establish that
standard-library vulnerabilities are closed, that unrelated dependencies are
remediated, that all components are latest, or that any release/deployment has
changed. No registry push, importer enablement or cluster rollout is needed to
review this source patch.
