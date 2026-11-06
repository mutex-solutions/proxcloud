# C1: System context

ProxCloud is a multi-tenant private-cloud portal and billing system for Proxmox
VE. Tenants sign up, organize into organizations and projects, and provision
servers, networks, volumes, public IPs and buckets; ProxCloud meters that usage,
rates it against price plans, bills it and collects payment. Operators configure
providers, price plans and quotas.

Status legend and maintenance rule: [README.md](README.md).

```mermaid
flowchart LR
  classDef impl fill:#e6f4ea,stroke:#1e7e34,color:#0b3d1a
  classDef planned fill:#f2f2f2,stroke:#8a8a8a,stroke-dasharray:5 5,color:#555555

  tenant["Tenant user<br/>client console"]:::impl
  orgadmin["Tenant org admin<br/>members, roles, billing"]:::impl
  operator["Platform operator<br/>admin console and admin API"]:::impl
  agent["AI agent<br/>MCP client"]:::impl

  pc["ProxCloud<br/>portal, API, metering, billing"]:::impl

  subgraph ext["External systems"]
    pve["Proxmox VE cluster(s)"]:::impl
    pbs["Proxmox Backup Server"]:::planned
    kc["Keycloak<br/>OIDC issuer"]:::impl
    idp["Tenant IdPs<br/>per-org SSO"]:::planned
    stripe["Stripe"]:::impl
    airwallex["Airwallex"]:::planned
    smtp["SMTP relay"]:::impl
    rgw["S3 store: Ceph RGW"]:::impl
    rustfs["S3 store: RustFS"]:::planned
    nas["TrueNAS NAS"]:::planned
    prom["Prometheus-compatible metrics"]:::impl
    dns["DNS and ACME"]:::impl
    reg["Container registry<br/>ghcr.io"]:::impl
  end

  tenant -->|"HTTPS, OIDC"| pc
  orgadmin -->|"HTTPS, OIDC"| pc
  operator -->|"HTTPS, OIDC or SigV4"| pc
  agent -->|"MCP over HTTPS"| pc

  pc -->|"token validation, JWKS"| kc
  kc -.->|"identity brokering"| idp
  pc -->|"REST :8006, API token"| pve
  pve -.->|"backups"| pbs
  pc -->|"deposits, cards, collect"| stripe
  pc -.->|"payments"| airwallex
  pc -->|"transactional mail"| smtp
  pc -->|"RGW admin ops and S3"| rgw
  pc -.->|"admin API and S3"| rustfs
  pc -.->|"WebSocket API: shares"| nas
  pve -.->|"NFS, SMB, iSCSI"| nas
  pc -->|"PromQL traffic counters"| prom
  dns -->|"hostnames and TLS"| pc
  reg -->|"images and charts"| pc
```

## Actors

| Actor | Interaction with ProxCloud | Status |
|---|---|---|
| Tenant user | Uses the client console (`web/client`): servers, networks, security groups, volumes, public IPs, images, buckets, bills and payments. Signs in through Keycloak (OIDC, PKCE). | Implemented |
| Tenant user, per-org SSO login | Email-first login that redirects to the organization's own IdP through Keycloak. | Planned |
| Tenant org admin | Manages organization members, custom roles, project members and invites, billing profiles. Members are matched by an IdP-verified email only. | Implemented |
| Tenant org admin, SSO self-service | Configures the organization's OIDC/SAML IdP, verified domains and group-to-role mapping. | Planned |
| Platform operator | Uses the admin console (`web/admin`) and the admin routes under `/api/v1/admin/**`; automates through the public admin API `/admin-api/v1` (AWS SigV4 keys or the admin-api realm). Configures Proxmox providers, flavors, price plans, quotas, integrations. | Implemented |
| Platform operator, installer | One-command install, add-node and upgrade of the whole stack. | Implemented |
| AI agent | Calls `/mcp` with the client toolset (customer realm token or personal access key) or the admin toolset (admin realm token or SigV4 key). Tools dispatch in-process through the same router as the HTTP API. | Implemented |

## External systems

| System | Interaction | Status |
|---|---|---|
| Proxmox VE cluster(s) | REST API on `:8006` with a least-privilege API token (see [proxmox-permissions.md](../../proxmox-permissions.md)). ProxCloud drives pools, full clones, VM config, SDN zones/vnets/subnets, the cluster and VM firewall, storage and PCI (GPU) mappings, and reads tasks and cluster resources. | Implemented |
| Proxmox Backup Server | Scheduled VM backups and tenant restore. | Planned |
| Keycloak | OIDC issuer with one realm per audience (customer, admin, admin API). ProxCloud is a resource server: it validates bearer tokens against the realm JWKS and reads `sub`, `email`, `email_verified`, `azp`. The chart can bundle Keycloak or point at an external one. | Implemented |
| Tenant IdPs | Per-organization OIDC/SAML IdPs brokered through Keycloak. | Planned |
| Stripe | Deposits, saved cards, off-session collect, refunds. | Implemented |
| Airwallex | Second payment gateway. | Planned |
| SMTP relay | Bills, reminders, invites, suspension notices. | Implemented |
| S3 store: Ceph RGW | Tenant buckets on sites with Ceph: RGW Admin Ops for per-project users, keys and quotas, S3 for bucket and object operations. | Implemented |
| S3 store: RustFS | Tenant buckets and internal backups on sites without Ceph. | Planned |
| TrueNAS NAS | Capacity storage for Proxmox (NFS, SMB, iSCSI) and tenant file shares created through the TrueNAS WebSocket API. | Planned |
| Prometheus-compatible metrics | Optional per-provider source for per-VM network traffic counters, used to bill traffic. Proxmox providers default to no metrics source. | Implemented |
| DNS and ACME | Hostnames and TLS certificates for the portal, the API and Keycloak, provided by the hosting cluster (the chart renders the routes, not the certificates). | Implemented |
| DNS TXT verification | Domain ownership checks for per-org SSO. | Planned |
| Container registry (ghcr.io) | CI publishes the `proxcloud-api`, `proxcloud-web` and `proxcloud-admin` images and the Helm charts; deployments pull from it. | Implemented |
| VM template pipelines | Image builds that publish OS and Kubernetes node templates into Proxmox. | Planned |
