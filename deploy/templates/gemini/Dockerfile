# gemini — the base template plus the Gemini CLI (bring your own Google API key), like E2B's "gemini" agent template.
# Preinstalled only: licences, accounts and API keys are the customer's.
ARG BASE
FROM ${BASE}
USER root
RUN npm install -g @google/gemini-cli@0.67.0 && npm cache clean --force
WORKDIR /home/user
