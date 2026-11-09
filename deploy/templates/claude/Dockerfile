# claude — the base template plus the Claude Code CLI (bring your own Anthropic API key), like E2B's "claude" agent template.
# Preinstalled only: licences, accounts and API keys are the customer's.
ARG BASE
FROM ${BASE}
USER root
RUN npm install -g @anthropic-ai/claude-code@2.1.319 && npm cache clean --force
WORKDIR /home/user
