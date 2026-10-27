# Security Policy

## Reporting a vulnerability

**Please do not open a public issue for security vulnerabilities.**

Report privately using GitHub's
[private vulnerability reporting](https://github.com/mutex-solutions/proxcloud/security/advisories/new)
("Report a vulnerability" under the repository's **Security** tab), or email
**security@proxcloud.example**.
<!-- TODO(owner): replace security@proxcloud.example with the real ProxCloud security contact before publishing. -->

Please include:

- a description of the issue and its impact,
- steps to reproduce (a proof of concept if you have one),
- affected version(s) or commit, and
- any suggested remediation.

We will acknowledge your report, keep you updated on our assessment, and
coordinate a disclosure timeline with you. Please give us a reasonable window to
release a fix before any public disclosure.

## Supported versions

ProxCloud is pre-1.0 and under active development. Security fixes land on `main`
and in the latest `0.x` release. We recommend running the most recent tagged
release / image.

| Version | Supported |
|---------|-----------|
| `0.1.x` | ✅ |
| `< 0.1` | ❌ |

## Handling secrets and configuration

- **Never commit secret values.** Credentials, encryption keys, OIDC client
  secrets, Stripe keys, and OpenStack passwords are supplied at runtime via
  environment variables or Kubernetes Secrets — not source.
- Deployment value overlays that may carry secrets are already excluded by
  `.gitignore` (the local value-overlay files under `deploy/` listed there). The
  tracked `deploy/charts/proxcloud/values.yaml` holds secret-free chart defaults — keep it
  that way.
- Sensitive config values can be encrypted at rest using the built-in
  `pkg/textcrypt` support and decrypted with a key injected at runtime
  (`PROXCLOUD_ENCRYPTION_DEFAULT_KEY`); the key itself must never be committed.
- The operator API (`/admin-api/v1`) is SigV4-signed; treat its HMAC key pairs
  like any other credential and rotate leaked keys immediately.

If you find a committed secret, treat it as compromised: rotate it and report it
through the private channel above.
