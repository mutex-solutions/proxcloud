# EXPERIMENTAL SOURCE-ONLY candidate; NOT referenced by runtime/base recipes.
# Future owner-authorized hosted job only, after independent patch review:
# docker buildx build --build-arg DEBIAN_BASE=debian:trixie-slim@sha256:<reviewed-digest> \
#   --target artifacts --output type=local,dest=result/ssh-backport \
#   -f deploy/security/openssh/Dockerfile deploy/security/openssh
# The tag default is for recipe development, NOT a reproducible/promotion pin.
# Parent must pin the Debian 13 digest, retain signed-repository/dependency
# buildinfo provenance, and impose an isolated 120-minute job timeout. Live apt
# dependencies are not an immutable snapshot or a reproducible-build claim.
ARG DEBIAN_BASE=debian:trixie-slim
FROM ${DEBIAN_BASE} AS builder
ARG DEBIAN_BASE
ENV DEBIAN_FRONTEND=noninteractive
WORKDIR /bootstrap
COPY build-backport.sh CVE-2026-60002.patch /bootstrap/
# Deliberately invoke through sh: the source helper need not be executable.
RUN sh /bootstrap/build-backport.sh

# Artifact export only: no client/server installation, runtime image or push.
FROM scratch AS artifacts
COPY --from=builder /out/ /
