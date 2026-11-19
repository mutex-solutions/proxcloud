# Security Policy

## Reporting a vulnerability

Use [GitHub Issues](https://github.com/mutex-solutions/proxcloud/issues/new/choose)
for ordinary bugs and support requests.

**For a suspected security vulnerability, open only a minimal contact-request
issue**, for example "Security report: requesting a private contact channel".
Mention the affected component and version/commit only if safe to disclose, and
ask a maintainer how to share the full report privately. A verified security email
and GitHub private vulnerability reporting are not configured yet; do not use a
placeholder address or assume a public issue is confidential.

**Do not post exploit steps, a proof of concept, credentials/tokens, private
endpoints, customer data, database dumps or unredacted logs in that public issue.**
Once the maintainer supplies a private channel, send the impact, reproduction
steps, affected versions and suggested remediation there. If you already exposed
a credential, revoke/rotate it immediately; deleting an issue does not revoke a
secret or erase copies.

We will acknowledge your report, keep you updated on our assessment, and
coordinate a disclosure timeline with you. Please give us a reasonable window to
release a fix before any public disclosure.

## Supported versions

ProxCloud is under active development. `v0.9.0` is the current stable release;
its tag publishes the app images and chart. Fixes land on `main` first. There is no
support SLA, and live installer and split/managed-service acceptance have not been
performed for this release. Evaluate the exact tagged commit or a reviewed commit
with successful checks, not an assumed `latest` image tag, and verify that the
published artifacts are pullable before relying on them.

| Version / source | Status |
|------------------|--------|
| `v0.9.0` | Current stable release; report issues against it; fixes land on `main` for a later release; no SLA |
| Current `main` | Development fixes; no stable support/SLA promise |
| `v0.8.2` | Previous stable release; no stated backport support; upgrade to `v0.9.0` (see the changelog) |
| `v0.5.0`, `v0.5.1`, `v0.5.2`, `v0.6.0`, `v0.7.0`, `v0.8.0`, `v0.8.1` | Older stable releases; no stated backport support; upgrade to `v0.9.0` |
| `v0.4.0` | Older stable release; no stated backport support; upgrade to `v0.9.0` (breaking API changes since `v0.4.0`, see the changelog) |
| `v0.4.0-rc.1`, `v0.7.0-rc.1`, `v0.9.0-rc.1`, `v0.9.0-rc.2` | Superseded prereleases; upgrade to `v0.9.0` |
| Older / untagged downstream builds | No stated backport support; report the exact commit |

Every tag from `v0.4.0-rc.1` to `v0.6.0` mounts the unauthenticated job triggers
(`POST /debug/*`) on the API's management port whenever the job scheduler is on.
`v0.7.0` mounts them only with the debug triggers setting; see the Security entry
of the [changelog](CHANGELOG.md) for what was affected and how to migrate.

Unfixed HIGH/CRITICAL findings in shipped images are tracked by the security-scan
gate until **2027-03-12**, after which they block again; the GPU/CUDA template is
not scanned. The default `python` sandbox template, a public image pinned and run
as is, is scanned only as a report-only base: none of its findings blocks, with or
without a fixed version, before or after that date (on 2026-11-15 it had 2 HIGH
zlib findings with a fixed version and 47 without). The date moved from 2026-12-12
after the [review of 2026-11-15](docs/security-unfixed-review.md), which records
what was and was not checked. The single owner-approved VEX (CVE-2025-47273) still
expires on 2026-12-31.

See [release readiness](docs/release-readiness.md) for validation limits.

## Handling secrets and configuration

- **Never commit secret values.** Credentials, encryption keys, OIDC client
  secrets, Stripe keys, and Proxmox API tokens are supplied at runtime via
  environment variables or Kubernetes Secrets — not source.
- Deployment value overlays that may carry secrets are already excluded by
  `.gitignore` (the local value-overlay files under `deploy/` listed there). The
  tracked `deploy/charts/proxcloud/values.yaml` holds secret-free chart defaults — keep it
  that way.
- Sensitive config values can be encrypted at rest using the built-in
  `pkg/textcrypt` support and decrypted with a key injected at runtime
  (`PROXCLOUD_ENCRYPTION_DEFAULT_KEY`); the key itself must never be committed.
- The operator API (`/admin-api/v1`) accepts configured OIDC credentials or
  SigV4-signed requests; treat HMAC key pairs and bearer credentials as secrets and
  rotate/revoke leaked credentials immediately.
- Actions variables are not secret-masked. The privileged template workflows read
  cluster settings from repository Actions **Secrets**; see the placeholder-only
  [.github/pve-actions.env.example](.github/pve-actions.env.example). Secrets masking
  is best-effort and does not sanitize artifact contents or transformed output.
- Public-PR code must not run on a persistent runner with PVE credentials or
  production network access. Runner labels and two users/containers on one host are
  not a security boundary. Restrict privileged workflows to reviewed trusted code;
  prefer a separate private operations repository for that trust domain.

If you find a committed secret, treat it as compromised: revoke/rotate it first,
then request a private maintainer channel as described above without reposting
the secret in an issue.
