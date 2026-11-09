# amp — the base template plus the Amp CLI (bring your own Amp account), like E2B's "amp" agent template.
# Preinstalled only: licences, accounts and API keys are the customer's.
ARG BASE
FROM ${BASE}
USER root
RUN npm install -g @sourcegraph/amp@0.0.1793980834-gbaaead && npm cache clean --force
WORKDIR /home/user
