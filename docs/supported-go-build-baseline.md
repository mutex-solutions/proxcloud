# Supported Go build compiler baseline

## Decision and scope

The selected build/test/release compiler target is **Go 1.27.4**. Parent-refreshed
[official Go release metadata](https://go.dev/dl/?mode=json) dated **2026-11-11**
identifies **Go 1.27.4** as the latest stable release and **1.26.11** as the
supported 1.26-line patch. This change intentionally selects the newer stable
series rather than retaining 1.26.11. Support follows the
[Go release policy](https://go.dev/doc/devel/release#policy); this selection does
not authorize automatically moving to a later release candidate or compiler.

This source-only change pins all six Go builder stages in five Dockerfiles to
`1.27.4-bookworm`, preserving existing registry prefixes. The
[test workflow](../.github/workflows/test.yml) selects exact `go-version: '1.27.4'`.
These are exact version tags, **not digest pins**: registry tag existence, image
platforms, digests and OS package contents still require separate verification.
No unverified digest is introduced. The pins select a validation target; they do
not establish that the target has passed compatibility tests or produced images.

The [root module](../go.mod) intentionally raises its minimum to **`go 1.26.0`**.
The updated dependency graph selects **`golang.org/x/crypto v0.58.0`**, which
requires Go 1.26.0. The minimum is dependency-driven, not copied from the compiler
patch. That directive sets the module language/minimum Go version; it does not
identify the compiler used to produce a binary or the version of the linked
standard library/runtime. No `toolchain` directive is added. Dependency versions
and checksums are a separate part of the upgrade and need their own complete
module-graph and checksum validation.

Source builders and consumers of the Go module must account for the raised
minimum; Go 1.25 is no longer an eligible source-build toolchain. The selected
release/CI target is exact Go 1.27.4, not every compiler above the minimum. API and
SPA clients do not acquire a Go toolchain requirement from this source change.
Runtime behavior, published artifacts and deployed standard libraries change only
after the relevant binaries/images are actually rebuilt, validated, published and
rolled out; none of those results is implied here.

## Six builder stages, seven output files

| Dockerfile / stage | Selected builder | Output / source |
|---|---|---|
| [API](../deploy/Dockerfile), `build` | `golang:1.27.4-bookworm` | `proxcloud-api` / `./cmd/api` |
| [Sandbox proxy](../deploy/sandbox-proxy.Dockerfile), `build` | `golang:1.27.4-bookworm` | `sandbox-proxy` / `./cmd/sandbox-proxy` |
| [Sandbox importer](../deploy/sandbox-import.Dockerfile), `build` | `golang:1.27.4-bookworm` | `sandbox-import` / `./cmd/sandbox-import` |
| [Sandbox runtime](../deploy/sandbox-runtime.Dockerfile), `init` | `golang:1.27.4-bookworm` | `sandbox-init` **and** `sandbox-netguard` / their root-module command packages |
| [MCP gateway](../deploy/templates/mcp-gateway/Dockerfile), `gateway` | `docker.io/library/golang:1.27.4-bookworm` | `mcp-gateway` / `./cmd/mcp-gateway` |
| [Sandbox runtime](../deploy/sandbox-runtime.Dockerfile), `envd` | `golang:1.27.4-bookworm` | External `envd` / pinned upstream `packages/envd` |

There are **six root-module binaries** (API, proxy, importer, init, netguard and MCP
gateway), plus **external envd**, for **seven outputs**. Stages, Dockerfiles and
output files are deliberately counted separately.

The external source remains pinned to E2B infra commit
`e85b9f5f6de6c92b002f376b4f91b1652d3c942e`. Its `packages/envd` module replaces a
dependency with sibling `packages/shared`; neither that source tree nor its
complete module inputs are provided by the root module's cache. Previously
recorded upstream minimum information is not a substitute for reading both
manifests at that exact commit. Check their actual `go`/`toolchain` requirements
and selected dependencies separately before claiming compatibility with 1.27.4.
If an upstream module requires a future/newer compiler, record the Go 1.27.4 build
as blocked by that requirement (or failed if attempted), not as a successful
seventh output. Do not lower its directive, skip the stage, change the raw commit
or silently obtain another toolchain to manufacture a passing baseline. External
envd compatibility and artifact validation remain **pending**.

Existing build flags, target choices (including the importer's explicit
Linux/amd64), artifact copy paths, runtime OS images, users and entrypoints remain
unchanged. The MCP named repository build context is preserved. Node, NGINX,
frontend dependency graphs and other runtime images are outside this compiler
policy change.

## What the static tests prove

[The baseline unit tests](../internal/config/go_build_baseline_test.go) keep the
exact compiler **1.27.4** and separate module minimum **1.26.0** explicit. The
version-policy test validates both with standard-library `go/version`, checks
that the minimum does not exceed the compiler and rejects conflating the minimum
with the exact compiler patch.

The source-declaration test verifies that its package-relative `../../` root
identifies this module, checks all six expected Go `FROM` stages and exact image
tags, rejects additional unaccounted Go stages in those five Dockerfiles, parses
the test workflow's Go setup selections, and requires the exact separate root
minimum. It does not invoke Docker, Git, a compiler, a downloaded program or a
network request. Existing stage-count, missing/duplicate-stage and CI assertions
remain in place.

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
performed while making this source-only change.** The static tests are updated,
not reported as executed. Parent-owned validation must record its actual results
separately; source inspection and selected pins are not a tested-support claim.

Before reporting compiler validation complete:

1. Verify the exact Go 1.27.4 compiler/archive and candidate image identities. Keep
   target execution inside the approved isolated environment with read-only
   source and prepared inputs, no external network, no cloud credentials and no
   host Docker socket. Use `GOTOOLCHAIN=local`, `GOPROXY=off`, `GOSUMDB=off`,
   read-only module mode and isolated writable scratch/caches to prevent implicit
   substitutions or downloads. Confirm the complete dependency graph and
   checksums without rewriting the intentional `go 1.26.0` directive.
2. Compile all six root outputs with the selected Go 1.27.4 compiler, preserving
   each existing Linux/CGO/architecture choice. Record source identity, output
   hash, size, target platform and `go version -m` compiler stamp for each file.
3. Separately prepare envd/shared sources, manifests, sums and selected modules at
   the unchanged commit. Check the actual upstream minimums, then compile envd
   under Go 1.27.4 and collect the same output evidence. Missing inputs or a
   newer/future compiler requirement leave this gate explicitly pending or failed
   as applicable; they do not justify reporting external envd as validated.
4. Run the updated baseline tests, root unit suite and relevant integration
   suites, including API-tagged tests, under that actual compiler in the approved
   profile. Report race tests separately; static `CGO_ENABLED=0` artifact builds
   are not race coverage.
5. Treat final Docker image builds/runtime checks as a separate gate. Existing
   recipes include Git clone, module download, apt/npm/uv and Chrome resolution;
   preparing a Go archive alone does not make complete images buildable offline.

Old Go 1.25 or 1.26.11 results, cached test passes and already-built artifacts
cannot be credited to Go 1.27.4. This version selection alone does **not** establish
that standard-library vulnerabilities are closed, that unrelated dependencies
are remediated, that all components are latest, or that any release/deployment has
changed. No registry push, importer enablement or cluster rollout is needed to
review this source patch.
