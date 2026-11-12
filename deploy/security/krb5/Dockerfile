# EXPERIMENTAL SOURCE-ONLY; never referenced by controlled/default images.
# No default: parent must pass the independently verified Debian 13 amd64 digest.
# Supply a separately reviewed local build-context file trust/mit-krb5-release.gpg
# and its independently reviewed SHA256. Missing local input fails without trying
# to resolve an unprovided named context as a registry image.
ARG DEBIAN_BASE
FROM --platform=linux/amd64 ${DEBIAN_BASE} AS builder
ARG DEBIAN_BASE
ARG MIT_KEYRING_SHA256
ENV DEBIAN_FRONTEND=noninteractive
WORKDIR /bootstrap
COPY build-krb5.sh /bootstrap/
COPY trust/mit-krb5-release.gpg /trust/mit-krb5-release.gpg
RUN sh /bootstrap/build-krb5.sh

# Artifact transport ONLY, NOT a runtime image or an installable Debian package.
FROM scratch AS artifacts
COPY --from=builder /out/ /
