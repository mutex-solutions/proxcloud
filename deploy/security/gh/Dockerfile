# syntax=docker/dockerfile:1
# SOURCE-ONLY, artifact evaluation candidate. NOT a runtime installation.
# Future owner-authorized isolated CI only (not executed during source review):
# docker buildx build --target artifacts --output type=local,dest=result/gh-source \
#   -f deploy/security/gh/Dockerfile deploy/security/gh
# Pin the builder digest and review APK/module provenance before promotion.
# The fixed tag is policy-selected, not a reproducible-build/digest claim.
# Official source inspected 2026-11-12:
# https://api.github.com/repos/cli/cli/git/ref/tags/v2.103.3
# https://raw.githubusercontent.com/cli/cli/63837fb778d6a39f02be0ce76ff89f2bc8d74bdc/go.mod
# Source: go 1.27.0, toolchain go1.27.3, x/net v0.60.0 (indirect).
# x/net v0.62.0 requires the SAME crypto/sys/term/text versions as this source.
# https://raw.githubusercontent.com/golang/net/v0.62.0/go.mod
# CLI source already includes the v2.93.0 CVE-2026-48501 vendor fix.
# This local dependency rebuild is NOT the official v2.103.3 release binary.
FROM golang:1.27.4-alpine AS builder
ENV GOTOOLCHAIN=local \
    CGO_ENABLED=0 \
    GOPROXY=https://proxy.golang.org \
    GOSUMDB=sum.golang.org \
    GOPRIVATE= \
    GONOSUMDB= \
    GONOPROXY= \
    GIT_TERMINAL_PROMPT=0
RUN apk add --no-cache ca-certificates git
WORKDIR /src
# No ARG/ENV source selector: neither a build argument nor a moving tag can
# substitute a different commit. Verify the checkout before executing Go tools.
RUN set -eu; \
    git clone --no-checkout https://github.com/cli/cli.git cli; \
    cd cli; \
    git checkout --detach 63837fb778d6a39f02be0ce76ff89f2bc8d74bdc; \
    test "$(git rev-parse HEAD)" = 63837fb778d6a39f02be0ce76ff89f2bc8d74bdc; \
    printf '%s  %s\n' db11ba3547fe6420c763b5fb208bb5e41a44598895f94481be01ea967fc421e2 go.mod \
        | sha256sum -c -; \
    test -z "$(git status --porcelain)"; \
    mkdir -p /out/upstream /out/modified; \
    git rev-parse HEAD > /out/source-commit.txt; \
    cp go.mod go.sum /out/upstream/
WORKDIR /src/cli
# The hash above locks the reviewed v0.60.0 graph. Fail rather than silently
# downgrade/change a different source graph. MVS changes remain in the diff.
RUN set -eu; \
    test "$(go list -m -f '{{.Version}}' golang.org/x/net)" = v0.60.0; \
    go mod edit -require=golang.org/x/net@v0.62.0; \
    go mod tidy; \
    test "$(go list -m -f '{{.Version}}' golang.org/x/net)" = v0.62.0; \
    go mod verify > /out/module-verification.txt; \
    cp go.mod go.sum /out/modified/; \
    git diff --exit-code -- . ':!go.mod' ':!go.sum'; \
    git diff -- go.mod go.sum > /out/source-dependency-diff.patch; \
    test -s /out/source-dependency-diff.patch; \
    go list -mod=readonly -m -json all > /out/modules.json; \
    go version > /out/toolchain.txt
# Limited upstream unit gates, not the full suite or CVE/network acceptance.
# These commands run ONLY in a future authorized image build.
RUN set -eu; \
    go test -mod=readonly ./internal/build ./internal/ghrepo golang.org/x/net/html > /out/unit-tests.txt; \
    go build -mod=readonly -trimpath -buildvcs=false \
        -ldflags='-s -w -X github.com/cli/cli/v2/internal/build.Version=2.103.3+proxcloud1' \
        -o /out/gh ./cmd/gh; \
    go version -m /out/gh > /out/go-buildinfo.txt; \
    awk '$1 == "dep" && $2 == "golang.org/x/net" && $3 == "v0.62.0" { found=1 } END { exit !found }' \
        /out/go-buildinfo.txt; \
    mkdir -p /tmp/gh-empty-home; \
    HOME=/tmp/gh-empty-home GH_CONFIG_DIR=/tmp/gh-empty-home GH_NO_UPDATE_NOTIFIER=1 \
        GH_NO_EXTENSION_UPDATE_NOTIFIER=1 /out/gh --version > /out/gh-version.txt; \
    case "$(head -n 1 /out/gh-version.txt)" in \
        'gh version 2.103.3+proxcloud1'*) ;; \
        *) exit 1 ;; \
    esac; \
    cp LICENSE /out/LICENSE; \
    printf '%s\n' \
        'SOURCE_BUILD_CANDIDATE_NOT_RUNTIME_SECURITY_VALIDATED' \
        'Upstream: https://github.com/cli/cli.git' \
        'Commit: 63837fb778d6a39f02be0ce76ff89f2bc8d74bdc (v2.103.3)' \
        'Local version: 2.103.3+proxcloud1; not the official release binary.' \
        'Modification: x/net v0.60.0 -> v0.62.0, followed by go mod tidy.' \
        'Review source-dependency-diff.patch for all resulting module changes.' \
        'Upstream MIT LICENSE retained. Module/license inventory review required.' \
        'modules.json is a module inventory, NOT a complete SPDX/CycloneDX SBOM.' \
        'Fresh artifact/stdlib/module and integrated runtime scans are required.' \
        'No scanner suppressions, VEX waiver, runtime install, or promotion.' \
        > /out/SOURCE_BUILD_NOTICE.txt; \
    cd /out; \
    sha256sum gh LICENSE source-commit.txt source-dependency-diff.patch \
        upstream/go.mod upstream/go.sum modified/go.mod modified/go.sum \
        modules.json go-buildinfo.txt toolchain.txt unit-tests.txt \
        module-verification.txt gh-version.txt SOURCE_BUILD_NOTICE.txt > SHA256SUMS

# Export only: no runtime/base/workflow/helper is changed by this candidate.
FROM scratch AS artifacts
COPY --from=builder /out/ /
