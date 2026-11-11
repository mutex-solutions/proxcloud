# Architecture and Deployment Overview

This section is for the people who run ProxCloud itself — installing it on their own infrastructure, wiring it to their Proxmox VE cluster(s), and keeping it healthy. Once ProxCloud is up, the ongoing product configuration (regions, pricing, plans, customers) happens in the admin console and is documented separately; these pages get you to that point.

## What a ProxCloud deployment is made of

ProxCloud is a cloud billing and self-service private-cloud portal sitting in front of Proxmox VE. A running deployment is a small set of workloads plus their data and identity dependencies:

- **proxcloud-api** — the Go backend that serves the REST API, the admin API, and the background jobs (billing, resource sync, usage metrics).
- **proxcloud-web** — the customer portal SPA.
- **proxcloud-admin** — the operator/admin SPA, on its own host.
- **Dependencies** — **PostgreSQL** (the primary datastore, JSONB documents plus durable work/financial receipts), and, optionally, **Keycloak** (OpenID Connect identity — off by default; enable it or point at your own IdP). No RabbitMQ connection or broker is required by the current API.

The Helm chart can provision PostgreSQL through an existing CloudNativePG operator, optionally bundle that operator, use the development PostgreSQL subchart, or point at an external writable primary. Keycloak is optional when an external IdP is configured. Before installing from a registry, verify that the exact image/chart version has actually been published; source-tree versions are not publication evidence.

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

1. Choose and configure identity before expecting a console login: explicitly enable/configure bundled Keycloak, or provide the external issuer/client settings. Bundled Keycloak is off in the base chart defaults.
2. Install on Kubernetes (or follow the MicroK8s evaluation guide with that identity setup).
3. Set up backups **before** onboarding real customers — the encryption secret in particular is gone for good if you lose it.
4. Configure the Proxmox provider and deliberately enable scheduled work for your site. The scheduler and debug routes default off; automatic polling/billing does not start merely because the API is running. Bounded PostgreSQL workers still process already-enqueued work, so scheduler-off is not a queue pause.
