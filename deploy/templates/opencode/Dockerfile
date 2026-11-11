# opencode — the base template plus the OpenCode (MIT), like E2B's "opencode" agent template.
ARG BASE
FROM ${BASE}
USER root
RUN npm install -g opencode-ai@1.19.5 && npm cache clean --force
WORKDIR /home/user
