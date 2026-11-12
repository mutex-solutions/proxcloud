# base — what Sandbox.create() with no template starts (E2B's default "base"): E2B's own base
# image (Debian, Python 3, git, gh, build tools) with maintained Node 22 and the ProxCloud conventions.
# Same Debian/glibc family; replace only the obsolete Node/npm payloads.
FROM docker.io/library/node:22-bookworm-slim@sha256:47725aca53d3295ec2d3be8f62726599b3033cc262edc2c7e429909b3ac599fc AS node-toolchain
FROM docker.io/e2bdev/base@sha256:8aebbf5e7d1209ebdc4bbc5999624556c2d9eebe91b9fded756ff2d688367065
USER root
# Apply available fixes without changing the upstream Debian release or toolchain.
RUN apt-get update && apt-get upgrade -y \
 && apt-get install -y --no-install-recommends sudo \
 && rm -rf /var/lib/apt/lists/*
# The upstream image provides /usr/local Python 3.11 (not Debian's managed Python).
# Refresh global packaging tools and pip's bundled dependencies; retain SBOM metadata.
# setuptools 84.2.1 vendors jaraco.context 6.1.0 and wheel 0.46.3 (not pkg_resources).
# Pin wheel's packaging dependency explicitly so --no-deps remains coherent.
RUN /usr/local/bin/python3 -m pip install --no-cache-dir --only-binary=:all: --no-deps --upgrade \
      pip==26.3 setuptools==84.2.1 wheel==0.48.0 packaging==26.3
# Remove the old npm payload before copying its replacement so stale vendored
# files are not merged into the new tree. Keep every unrelated global CLI.
RUN rm -rf /usr/local/lib/node_modules/npm
COPY --from=node-toolchain /usr/local/bin/node /usr/local/bin/node
COPY --from=node-toolchain /usr/local/lib/node_modules/npm /usr/local/lib/node_modules/npm
# Keep canonical package paths and explicitly bind the new CLI entrypoints.
# Do not use `corepack enable`, which would replace Yarn's own entrypoints.
RUN npm install -g npm@11.21.2 \
 && npm install -g --bin-links=false corepack@0.36.0 yarn@1.22.22 \
 && ln -sfn ../lib/node_modules/corepack/dist/corepack.js /usr/local/bin/corepack \
 && ln -sfn ../lib/node_modules/yarn/bin/yarn.js /usr/local/bin/yarn \
 && ln -sfn ../lib/node_modules/yarn/bin/yarn.js /usr/local/bin/yarnpkg \
 && npm cache clean --force \
 && test "$(node --version)" = v22.23.5 \
 && test "$(npm --version)" = 11.21.2 \
 && test "$(corepack --version)" = 0.36.0 \
 && test "$(yarn --version)" = 1.22.22
COPY common/setup-user.sh /usr/local/sbin/proxcloud-setup-user
RUN proxcloud-setup-user
WORKDIR /home/user
