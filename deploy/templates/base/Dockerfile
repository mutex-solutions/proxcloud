# base — what Sandbox.create() with no template starts (E2B's default "base"): E2B's own base
# image (Debian, Python 3, Node, git, gh, build tools) plus the ProxCloud conventions.
FROM docker.io/e2bdev/base@sha256:8aebbf5e7d1209ebdc4bbc5999624556c2d9eebe91b9fded756ff2d688367065
USER root
# Apply available fixes without changing the upstream Debian release or toolchain.
RUN apt-get update && apt-get upgrade -y \
 && apt-get install -y --no-install-recommends sudo \
 && rm -rf /var/lib/apt/lists/*
# The upstream image provides /usr/local Python 3.11 (not Debian's managed Python).
# Refresh the confirmed global setuptools package and pip's bundled dependencies.
# pip 26.3 updates its vendored msgpack/urllib3; upgrading standalone packages alone
# would not remediate those embedded copies. Keep package/SBOM metadata for scanning.
RUN /usr/local/bin/python3 -m pip install --no-cache-dir --only-binary=:all: --no-deps --upgrade \
      pip==26.3 setuptools==78.1.1
COPY common/setup-user.sh /usr/local/sbin/proxcloud-setup-user
RUN proxcloud-setup-user
WORKDIR /home/user
