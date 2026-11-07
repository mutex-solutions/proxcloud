# base — what Sandbox.create() with no template starts (E2B's default "base"): E2B's own base
# image (Debian, Python 3, Node, git, gh, build tools) plus the ProxCloud conventions.
FROM docker.io/e2bdev/base@sha256:8aebbf5e7d1209ebdc4bbc5999624556c2d9eebe91b9fded756ff2d688367065
USER root
RUN apt-get update && apt-get install -y --no-install-recommends sudo \
 && rm -rf /var/lib/apt/lists/*
COPY common/setup-user.sh /usr/local/sbin/proxcloud-setup-user
RUN proxcloud-setup-user
WORKDIR /home/user
