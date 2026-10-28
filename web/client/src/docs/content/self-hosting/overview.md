# Architecture and Deployment Overview

This section is for the people who run ProxCloud itself — installing it on their own infrastructure, wiring it to their Proxmox VE cluster(s), and keeping it healthy. Once ProxCloud is up, the ongoing product configuration (regions, pricing, plans, customers) happens in the admin console and is documented separately; these pages get you to that point.

## What a ProxCloud deployment is made of

ProxCloud is a cloud billing and self-service private-cloud portal sitting in front of Proxmox VE. A running deployment is a small set of workloads plus their data and identity dependencies:

- **proxcloud-api** — the Go backend that serves the REST API, the admin API, and the background jobs (billing, resource sync, usage metrics).
- **proxcloud-web** — the customer portal SPA.
- **proxcloud-admin** — the operator/admin SPA, served under `/proxcloud_admin`.
- **Dependencies** — **PostgreSQL** (the primary datastore, one JSONB document per aggregate), **RabbitMQ** (messaging), and, optionally, **Keycloak** (OpenID Connect identity — off by default; enable it or point at your own IdP).

The whole thing is one Helm chart, and every dependency can be bundled with the chart or pointed at an external instance you already run.

## Before you start

- **A Kubernetes cluster.** A single-node MicroK8s VM is plenty for evaluation — see the [MicroK8s Quickstart](/docs/self-hosting/quickstart).
- **An OpenID Connect provider.** The chart can bundle Keycloak (off by default — enable `keycloakx.enabled: true`), or you federate with an external OIDC provider. See [How Identity Works](/docs/concepts/identity).
- **One or more Proxmox VE clusters** with a ProxCloud API token (and optionally a Ceph RGW for object storage), added after installation from the admin console.

## The pages in this section

| Page | Covers |
|------|--------|
| [Installing on Kubernetes](/docs/self-hosting/install) | The Helm chart: values, ingress / Gateway API exposure, external dependencies |
| [MicroK8s Quickstart](/docs/self-hosting/quickstart) | A single-node evaluation install from a bare Ubuntu VM |
| [Single Sign-On](/docs/self-hosting/sso) | Realms, social and corporate logins on Keycloak |
| [Trusting a Custom CA](/docs/self-hosting/custom-ca) | Trusting privately-signed certificates on Proxmox, RGW or IdP endpoints |
| [Backup and Recovery](/docs/self-hosting/backup) | Which state must be protected, and how |

Identity itself — realms, bundled vs external OIDC — is explained conceptually in [How Identity Works](/docs/concepts/identity).

## A sensible order

1. Install on Kubernetes (or run the MicroK8s quickstart first).
2. Decide on identity: keep the bundled Keycloak, or switch to your own IdP.
3. Set up backups **before** onboarding real customers — the encryption secret in particular is gone for good if you lose it.
4. Nothing to wire for resource sync: ProxCloud keeps the customer dashboard current by polling the Proxmox VE API.
