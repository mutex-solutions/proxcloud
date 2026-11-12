# node — Node.js 24 LTS (the base template has Node 20) with npm, yarn, corepack, git.
FROM docker.io/library/node@sha256:6b9d4ea31e614e29f364bf75758458731eb1e105b80c99667438dd6b3fe262e0
USER root
# Keep the full upstream toolchain; apply fixes available in this Debian release.
RUN apt-get update && apt-get upgrade -y \
 && apt-get install -y --no-install-recommends sudo \
 && rm -rf /var/lib/apt/lists/*
# The image's npm 11.19.1 bundles vulnerable undici 6.28.0 and brace-expansion 5.0.9; use the
# same fixed npm as the base template (11.21.2 bundles undici 6.29.1 and brace-expansion 5.0.12).
RUN npm install -g npm@11.21.2 \
 && npm cache clean --force \
 && test "$(npm --version)" = 11.21.2
COPY common/setup-user.sh /usr/local/sbin/proxcloud-setup-user
RUN proxcloud-setup-user
WORKDIR /home/user
