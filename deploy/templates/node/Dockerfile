# node — Node.js 24 LTS (the base template has Node 20) with npm, yarn, corepack, git.
FROM docker.io/library/node@sha256:6b9d4ea31e614e29f364bf75758458731eb1e105b80c99667438dd6b3fe262e0
USER root
RUN apt-get update && apt-get install -y --no-install-recommends sudo \
 && rm -rf /var/lib/apt/lists/*
COPY common/setup-user.sh /usr/local/sbin/proxcloud-setup-user
RUN proxcloud-setup-user
WORKDIR /home/user
