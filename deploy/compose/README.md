# ProxCloud on one host with Docker Compose

One command installs the ProxCloud portal on a single Linux host, on your own domain, with TLS:

```sh
cd deploy/compose
./proxcloud-up.sh
```

It asks for what it needs (domain, TLS mode, two email addresses), generates every secret, starts
the stack, creates the first operator and prints the URLs.

| Service | Image | Role |
|---|---|---|
| Traefik | `traefik:v3.7.18` | The only published ports (80, 443). TLS, HTTP to HTTPS redirect |
| API | `ghcr.io/mutex-solutions/proxcloud-api` | The portal API and its durable jobs |
| Customer console | `ghcr.io/mutex-solutions/proxcloud-web` | |
| Operator console | `ghcr.io/mutex-solutions/proxcloud-admin` | |
| Keycloak | `quay.io/keycloak/keycloak:26.8.2` | Production mode, behind Traefik. Realm `clients` for customers, `master` for operators |
| PostgreSQL | `postgres:17-alpine` | Databases `proxcloud` and `keycloak` |
| keycloak-config, seed | one-shot | Realm settings (the same the Helm chart derives) and the first-run documents |

The three ProxCloud images run at one release tag: the pinned release of this checkout, unless
you pass `--version`.

## What is not included

- **No Proxmox setup.** No provider, token, OS template or network is created. Register your
  Proxmox VE cluster in the operator console afterwards
  ([quickstart, A2](../../docs/quickstart.md#a2-connect-an-existing-proxmox-cluster)).
- **No managed services.** Managed Kubernetes, managed databases and sandboxes need the services
  cluster of the [installer](../../docs/install.md); they are not part of this stack.
- **No mail.** Customers register without email verification until you add an SMTP integration
  (operator console, System, Integrations) and a mail server in Keycloak.
- **No high availability and no off-host backup.** One host, one database. `backup` writes to this
  host: copy the files elsewhere.
- **No object storage, no monitoring.**

## Requirements

- A Linux host (x86-64) used for nothing else, with Docker Engine 20.10 or later and the Docker
  Compose plugin v2 or later. `curl` is used for a last check when present.
- Ports 80 and 443 free (or others, with `--http-port` and `--https-port`).
- Four DNS names for this host, see below.
- The host must reach `ghcr.io`, `quay.io` and Docker Hub to pull the images.

## DNS and TLS modes

The portal uses four hostnames. With `--domain cloud.example.com` they are:

| Hostname | What |
|---|---|
| `app.cloud.example.com` | Customer console |
| `admin.cloud.example.com` | Operator console |
| `api.cloud.example.com` | API |
| `id.cloud.example.com` | Keycloak |

Or name each one: `--app-host`, `--admin-host`, `--api-host`, `--auth-host`. Create four `A` (or
`AAAA`) records pointing at this host, here `192.0.2.10`:

| TLS mode | Certificates | DNS records | Also needed |
|---|---|---|---|
| `acme-http` | Let's Encrypt, HTTP-01 | Public records to the host's public address | Port 80 reachable from the internet. Port 443 for your users |
| `acme-dns-cloudflare` | Let's Encrypt, DNS-01 | Records in a Cloudflare zone, **DNS only** (not proxied). They may point to a private address: nothing has to reach the host from the internet | A Cloudflare API token with `Zone / DNS / Edit` on that zone. Outbound access from the host to Let's Encrypt, the Cloudflare API and the DNS resolvers `1.1.1.1` and `1.0.0.1` (port 53) |
| `internal` | Traefik's self-signed certificate | Any records your users resolve (a LAN DNS server, or `hosts` files) | Every browser must accept the certificate once per hostname. For evaluation on a LAN |

Let's Encrypt renews by itself. Its rate limits are low: while you experiment, add
`ACME_CA_SERVER=https://acme-staging-v02.api.letsencrypt.org/directory` to `.env` (untrusted test
certificates). When done, remove the line and the stored test certificates:

```sh
docker compose rm -sf traefik-<mode>
docker volume rm <project>_traefik-acme
./proxcloud-up.sh install
```

The Cloudflare token is read from `--cloudflare-token-file <file>`, from the environment variable
`CF_DNS_API_TOKEN`, or typed at the prompt. It is stored in `./cf-token` (mode 600) and reaches
Traefik as a Docker secret file, never as a command argument or in `.env`. To replace it, run
`install` with the new token, then `docker compose restart traefik-acme-dns-cloudflare`: Traefik
reads the file when it starts.

## Install

Interactive: `./proxcloud-up.sh` and answer the questions.

Without questions, with flags (every flag also has an environment variable, see `--help`):

```sh
./proxcloud-up.sh install \
  --domain cloud.example.com \
  --tls acme-http --acme-email ops@example.com \
  --operator-email admin@example.com
```

```sh
CF_DNS_API_TOKEN=... ./proxcloud-up.sh install \
  --domain cloud.example.com \
  --tls acme-dns-cloudflare --acme-email ops@example.com \
  --operator-email admin@example.com
```

`--dry-run` prints the docker commands without running any, and writes what `.env` would become
to `.env.dry-run`. It changes nothing else: not `.env`, not the running install.

What the install leaves next to the script (all mode 600, none in git):

| File | Content |
|---|---|
| `.env` | Settings and the generated secrets: database passwords, Keycloak admin password, SSO client secret, **encryption key** |
| `operator-password` | Temporary password of the first operator. Keycloak asks for a new one at the first login: delete the file then |
| `cf-token` | The Cloudflare API token (`acme-dns-cloudflare` only) |
| `backups/` | Written by `backup` |
| `.env.dry-run` | Only after `--dry-run`: the preview, with the same secrets. Delete it when read |

**Back up `.env` now, off this host.** `ENCRYPTION_KEY` decrypts the provider credentials stored in
the database. It cannot be recovered or regenerated: without it, a database backup is not enough.

The install is idempotent. Run it again to resume after an error or to change a setting, for
example `./proxcloud-up.sh install --admin-host ops.example.com` or `--tls acme-http --acme-email
ops@example.com`. Secrets already in `.env` are never regenerated or printed. The release of a
finished install changes with `upgrade` only, which takes a backup first.

The Compose project name (`--project-name`, default `proxcloud-prod`) cannot change afterwards: the
containers and volumes carry it. The install refuses a name that something else on the host uses:

- the development stack of this checkout (`docker compose up` in the repository root), whose
  project is named after the checkout directory and whose services and volumes have the same names;
- containers that another directory created under that name;
- an existing database volume `<project>_postgres-data` when there is no `.env` here. That database
  only opens with the passwords of the `.env` it was created with: restore that file.

The stack runs with the values in `.env`. Variables exported in your shell (`PROXCLOUD_VERSION`,
`HTTP_PORT`, ...) do not reach Compose through the script; with a plain `docker compose` command
they do, and they win over `.env`.

### After the install

1. Open the operator console and sign in with the operator email and the temporary password.
2. System, Cloud providers: register the Proxmox provider, then **Test connection**.
3. Continue with [After any install](../../docs/quickstart.md#5-after-any-install): price plan,
   flavors, quota, first tenant.

## Operate

```sh
./proxcloud-up.sh status          # containers, API readiness, URLs
docker compose logs -f api        # in this directory; any service name
./proxcloud-up.sh backup
./proxcloud-up.sh upgrade vX.Y.Z
./proxcloud-up.sh uninstall       # stops and removes the containers, keeps the data
```

In `docker compose` commands the Traefik service is named after the TLS mode:
`traefik-internal`, `traefik-acme-http` or `traefik-acme-dns-cloudflare`. A plain
`docker compose up -d` starts the stack but does not apply the Keycloak realm settings
(`keycloak-config`, profile `setup`): `./proxcloud-up.sh install` does, before anything else.

### Backup

`./proxcloud-up.sh backup` writes `backups/proxcloud-<UTC time>.sql.gz` (a `pg_dumpall` of both
databases: the portal and Keycloak with its users) and `backups/env-<UTC time>` (a copy of `.env`).
Both hold secrets. Run it from cron and copy the files off the host. Not in the backup:

- the Let's Encrypt certificates: they are issued again;
- `cf-token`: keep the Cloudflare token elsewhere, and pass it again when you restore on a new host.

Nothing deletes old backups, and every `upgrade` adds one. Prune them yourself, for example
`find backups -type f -mtime +30 -delete` in the same cron job.

### Restore

Into an **empty** database, on this host or a new one (same checkout, the saved `.env` in place):

```sh
./proxcloud-up.sh uninstall                         # only if the stack is running here
docker volume rm <project>_postgres-data            # only to replace an existing database: this deletes it
docker compose up -d --wait postgres                # --wait: until the new database accepts connections
gunzip -c backups/proxcloud-<UTC time>.sql.gz | docker compose exec -T postgres psql -U proxcloud -d postgres
./proxcloud-up.sh install
```

`<project>` is `COMPOSE_PROJECT_NAME` in `.env`. Four errors are expected: roles `keycloak` and
`proxcloud` and databases `keycloak` and `proxcloud` "already exist" (the empty database starts
with them). Any other error is a real one.

### Upgrade

Read the release notes, update this checkout to the release (`git fetch --tags && git checkout
vX.Y.Z`), then:

```sh
./proxcloud-up.sh upgrade vX.Y.Z
```

It takes a backup, pulls the three images at that tag, restarts what changed and waits for the API.
A tag that cannot be pulled leaves the running install untouched.

To go back to the previous release, restore the backup the upgrade took, with its own `.env`:

1. Check out the previous release (`git checkout <previous version>`): `compose.yaml` belongs to it.
2. Copy `backups/env-<UTC time>` of that backup over `.env`. It still names the previous release;
   the current `.env` would start the new one on the restored database.
3. Follow [Restore](#restore) with `backups/proxcloud-<UTC time>.sql.gz` of the same backup. Its
   last step, `install`, starts the previous release.

Traefik and Keycloak are pinned to a version in `compose.yaml`, PostgreSQL to its major version
(`17-alpine` follows the minor releases); they move with the checkout. A new PostgreSQL major
version needs a dump and restore, not just a new image.

### Uninstall

`./proxcloud-up.sh uninstall` asks, then removes the containers and the network. The volumes
(database, certificates) stay, and `install` starts everything again.

`./proxcloud-up.sh uninstall --purge` also deletes the volumes, after you type the project name.
The first operator goes with them: `operator-password` is deleted, and the next `install` asks for
the operator email and creates it again. `.env` (the secrets) and `backups/` are never deleted by
the script.

## Security notes

- **Realm `master` is the operators' realm.** Every user of `master` who signs in to the operator
  console is a ProxCloud super admin. Never create a customer there. Customers live in realm
  `clients` and register themselves on the customer console.
- **Keycloak's own administrator** is the user `admin`; its password is `KC_ADMIN_PASSWORD` in
  `.env`. It is also a user of realm `master`, so it can use the operator console. Use it to add
  operators (Keycloak, realm `master`, Users) and to reset a lost operator password. If you change
  its password in Keycloak, write the new one to `.env` too: the realm settings are applied with
  it on every start.
- **The Keycloak admin console is reachable** at `https://<Keycloak hostname>/admin`, like the rest
  of Keycloak. Restrict it with your firewall if operators only work from known networks.
- **Operator sign-in is a password on a public endpoint.** Both realms have Keycloak's brute force
  detection on (repeated failed logins lock the account for a while, the user `admin` included).
  That slows guessing; it does not replace a second factor, and it lets anyone who can reach
  Keycloak lock `admin` out for a while by guessing, which also stops `install` and `upgrade` at
  the realm settings until the lock ends (the running portal is not affected). Limiting who can
  reach the Keycloak hostname avoids both. For realm `master`, set a password
  policy (Authentication, Policies) and require an OTP (Authentication, Required actions,
  Configure OTP as a default action) before the portal is open to the internet.
- **Traefik reads the Docker socket** to find this project's containers. The mount is read-only,
  but that protects the socket file, not the Docker API behind it: a compromised Traefik controls
  Docker, and so the host. Run nothing else on this host.
- **`.env`, `.env.dry-run`, `cf-token`, `operator-password` and `backups/` are secrets.** They are
  created with mode 600; keep the directory private to the account that runs the script.
- There are no default credentials: every secret is generated on this host at the first install.
- PostgreSQL and the API's management port are not published; only Traefik listens on the host.
- Feature flags (template updates, sandboxes), the job scheduler and debug triggers keep their
  defaults (off).

## Settings in `.env`

Written by the script; change them by re-running `install` with flags.

| Key | Meaning |
|---|---|
| `COMPOSE_PROJECT_NAME` | Compose project: prefix of the containers and volumes. Fixed |
| `COMPOSE_PROFILES` | The TLS mode |
| `PROXCLOUD_VERSION` | Release tag of the three ProxCloud images |
| `APP_HOST`, `ADMIN_HOST`, `API_HOST`, `AUTH_HOST` | The four hostnames |
| `HTTP_PORT`, `HTTPS_PORT` | Host ports. `URL_PORT` is derived (empty on 443) |
| `ACME_EMAIL` | Let's Encrypt account email |
| `ACME_CA_SERVER` | Optional, by hand: another ACME directory, for example Let's Encrypt staging |
| `OPERATOR_EMAIL` | The first operator, once created |
| `PG_PASSWORD`, `KEYCLOAK_DB_PASSWORD`, `KC_ADMIN_PASSWORD`, `SSO_CLIENT_SECRET`, `ENCRYPTION_KEY` | Generated secrets. The two database passwords only take effect when the database is first created |

## Troubleshooting

| Symptom | Look at |
|---|---|
| The install stops at "Waiting for ..." | `docker compose ps -a`, then `docker compose logs <service>` |
| The browser shows Traefik's self-signed certificate in an `acme-*` mode | The install does not wait for certificates. `docker compose logs traefik-<mode>`: the DNS record, port 80 (HTTP-01) or the Cloudflare token (DNS-01). Fix the cause first, then `docker compose restart traefik-<mode>` to try again: every failed attempt counts against the Let's Encrypt rate limits |
| "does not resolve on this host" (warning) | The hostname has no DNS record yet, as seen from this host. Create it before Let's Encrypt is asked again |
| "already has containers, created from ..." or "the development stack" | The project name is in use on this host, see [Install](#install). Choose another `--project-name`; after moving this checkout, run `uninstall` and `install` once |
| "the volume ..._postgres-data exists, but there is no .env" | The database of an earlier install is still there and its `.env` is not. Restore the `.env` (a copy is in every backup: `backups/env-<UTC time>`) |
| "no answer from the API ... on 127.0.0.1" (warning) | Docker publishes the ports on another address only, or Traefik is not up: `docker compose logs traefik-<mode>` |
| "port 80 is in use" | Another web server on the host. Stop it, or use `--http-port` and `--https-port` |
| "keycloak-config failed" | `docker compose logs keycloak-config`. Most often the Keycloak `admin` password was changed in Keycloak: write the new one to `KC_ADMIN_PASSWORD` in `.env` and run `install` again. The realm settings are applied first, so the running portal was not touched. If the password is right and the log still shows `invalid_grant`, the `admin` account is locked after failed logins (yours, or someone guessing): it unlocks by itself, at most 15 minutes after the last failure |

`keycloak-config/` and `seed/` are generated from the Helm chart (`deploy/charts/proxcloud`) and
must not be edited: `deploy/tests/test_compose_production.py` fails when they differ from it, and
says how to regenerate them.
