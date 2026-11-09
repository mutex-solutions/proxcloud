# codex — the base template plus the Codex CLI (Apache-2.0), like E2B's "codex" agent template.
ARG BASE
FROM ${BASE}
USER root
RUN npm install -g @openai/codex@0.167.0 && npm cache clean --force
WORKDIR /home/user
