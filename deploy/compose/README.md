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
| PostgreSQL | `postgres:17.12-alpine@sha256:f9cf3e1f0066f0eb0c48707d864f0ac5941e9ff454a072077d97a4d278d07d73` | Databases `proxcloud` and `keycloak` |
| keycloak-config, seed | one-shot | Realm settings (the same the Helm chart derives) and the first-run documents |

The three ProxCloud images run at one release tag: the pinned release of this checkout, unless
you pass `--version`.

## What is not included

- **No Proxmox setup.** No provider, token, OS template or network is created. Connect your
  Proxmox VE cluster afterwards with the operator console's Platform setup page
  ([quickstart, A2](../../docs/quickstart.md#a2-connect-an-existing-proxmox-cluster)).
- **No managed services in the stack itself.** Managed Kubernetes, managed databases and sandboxes
  run on clusters outside this host: the services cluster of the [installer](../../docs/install.md),
  or, for Kubernetes and databases, a platform the operator console builds on your Proxmox VE
  cluster once you install with `--managed-services` (v0.8.0 or later,
  [Optional features](#optional-features), [managed-services.md](../../docs/managed-services.md)).
- **No mail.** Customers register without email verification until you add an SMTP integration
  (operator console, System, Integrations) and a mail server in Keycloak.
- **No high availability and no off-host backup.** One host, one database. `backup` writes to this
  host: copy the files elsewhere.
- **No object storage, no monitoring.**
- **No timers and no template updates until you turn them on.** The job scheduler (resource sync,
  charging, bills) and VM template updates are off after a plain install: see
  [Optional features](#optional-features).

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

With `internal`, tenant servers cannot fetch their seed (hostname, password, SSH keys) from the API over
HTTPS either, because they do not trust the certificate: choose **On an ISO** under Server seeds in Platform
setup, step 3 Network.

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

**Back up `.env` now, off this host.** `ENCRYPTION_KEY` decrypts the provider credentials and the
tenants' TLS private keys stored in the database. It cannot be recovered or regenerated: without it,
a database backup is not enough. With another key the stored certificates no longer open: load
balancers keep serving what they serve, but one with a TLS or HTTPS listener takes no change, and
says so, until its certificates are uploaded again and named on its listeners.

The install is idempotent. Run it again to resume after an error or to change a setting, for
example `./proxcloud-up.sh install --admin-host ops.example.com` or `--tls acme-http --acme-email
ops@example.com`. Secrets already in `.env` are never regenerated or printed. The release of a
finished install changes with `upgrade` only, which takes a backup first. A setting you do not
name keeps its value, the three [optional features](#optional-features) included, with one
exception: `--managed-services` also turns template updates on.

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
2. System, Platform setup (`/setup`): it walks from the Proxmox token script to Go live, one step
   per tab, and applies the flavors, the price plan and the quota from the console, so no checkout
   and no `seed-operator.py` run is needed ([quickstart, A2](../../docs/quickstart.md#a2-connect-an-existing-proxmox-cluster)).
   Template imports need the API started with template updates on, and on this stack nothing syncs
   or charges on a timer while the job scheduler is off: the page says so at Go live.
3. Then the first tenant: [After any install](../../docs/quickstart.md#5-after-any-install).

### Optional features

Three features are off after a plain install. Each has an install flag, and the API reads them only
when it starts, so the operator console cannot switch them:

```sh
./proxcloud-up.sh install --template-updates    # VM template updates on
./proxcloud-up.sh install --scheduler           # job scheduler on
./proxcloud-up.sh install --managed-services    # managed services on (and template updates)
./proxcloud-up.sh install --no-scheduler        # off again; --no-template-updates, --no-managed-services likewise
```

The flag is recorded in `.env` and the API container is recreated with it. A later `install`
without the flag, and every `upgrade`, keep the recorded value: only the `--no-` flag turns a
feature off. The last lines of `install` (also with `--dry-run`) say what is on.

- **`--template-updates`** starts the VM template update runner: import of a published OS image as
  a new staged template, smoke test, publish and withdraw, in the operator console under System,
  Template updates. Without it the page lists versions and states, and every action that changes
  Proxmox answers `409 vm_updates_disabled`. It also needs a GitHub token integration and a Proxmox
  store with `import` content: [template updates](../../docs/template-updates.md).
- **`--scheduler`** starts the timed jobs ([jobs and scheduling](../../docs/jobs-scheduling.md)).
  Without it nothing runs on a timer: resources are synchronised only when an operator starts a
  sync, and no new charge is started. **No project can be deleted without it**: the job that
  tears a project's cloud resources down is one of the timed jobs, so a delete (customer and
  operator console, with the five-minute grace period or "Delete now") is refused with a `409` that
  names the scheduler, and nothing is changed. A project scheduled for deletion under a release up
  to 0.11.1 stays scheduled, cancellable, until the scheduler runs; an organization cannot be
  deleted while it has a project. With it, among others:
  - resources and Proxmox state are synchronised every minute to every 15 minutes;
  - usage is charged every minute and every hour, at the rates of the enabled price plans;
  - the bills of the previous month are issued, cards are collected, unpaid profiles are
    suspended, and projects scheduled for deletion are deleted, with their cloud resources.

  Turn it on once the price plan and the billing settings are the ones you want invoiced. Times
  are UTC. It needs a ProxCloud release after v0.6.0: up to v0.6.0 the API also serves its
  unauthenticated job triggers (below) while the scheduler is on, so `install` and `upgrade` refuse
  to run one of those releases with it, the default release of a checkout still pinned to v0.6.0
  included. Pass a later one with `--version` on a first install, or `upgrade` to it.

  `--no-scheduler` stops new timed work from being created. It does not stop the API's job
  worker, which always runs: billing work already queued in the database is still processed when
  the API starts, and written to the bills.
- **`--managed-services`** sets `PROXCLOUD_PLATFORM_ENABLED`, which lets the operator console build
  managed Kubernetes and managed databases on your Proxmox VE cluster: a seed VM and one shared
  platform cluster with the node groups `system`, `kamaji` and `dbaas`, provisioned by a durable job
  of the API ([ADR 0009](../../docs/adr/0009-managed-services-platform-lifecycle.md)). It implies
  `--template-updates`, because the job imports the seed and node images through them:
  `--no-managed-services` leaves template updates on, and `--no-template-updates` is refused while
  managed services are on. It needs ProxCloud v0.8.0 or later, the first release with the platform
  API: `install` and `upgrade` refuse an older one with it. The API container must reach the
  platform network (the seed and the platform cluster's API address, port 6443) besides the Proxmox
  API. On this stack the platform has no tenant block volumes (no gateway token on the compute
  provider, with or without `--cluster-gateway`) and no database backups; the Managed services page lists both under **What this release does not do**
  once the platform is ready.

  The switch creates nothing on Proxmox. It makes the console's Managed services page work; the
  operator then saves the settings there, runs a script as root on one Proxmox node, pastes two
  token secrets and presses Install. The steps, the stages of the job, every failure code, Remove and
  the limits of this stack are in [managed-services.md](../../docs/managed-services.md).

  No release before v0.8.0 reads `PROXCLOUD_PLATFORM_ENABLED`. A platform was built from this stack
  on the published images of v0.8.0 once, on a lab (2026-11-18), and on those of the candidate
  v0.9.0-rc.2 once (the same lab, 2026-11-19); not on those of v0.8.1, v0.8.2, v0.9.0, v0.9.1, v0.10.0, v0.10.1, v0.10.2, v0.11.0 or v0.11.1 (the other
  lab runs used `dev-<sha>` builds of main, [managed-services.md](../../docs/managed-services.md#not-verified-on-a-lab-yet)).

  **Testing ahead of a release.** `--version dev-<sha>` and `upgrade dev-<sha>` take a build of main
  (CI publishes the images `dev-<first 7 characters of the commit>` on every push to main that
  changes them); `<sha>` is 7 to 40 lower-case hex digits and no other tag is accepted. The script
  prints a warning each time: such a build is not a release, has no release notes and no tested
  upgrade path, and is not for production. `--managed-services` is allowed on it. `--scheduler` is
  not: a build without a release number cannot be placed after v0.6.0. Use this checkout at the same
  commit, so that `compose.yaml` matches the images.

What the scheduler needs on this one host, and what it adds:

- **Locking: nothing to add.** Every timed job runs under a lock in the stack's own PostgreSQL
  ([ADR 0007](../../docs/adr/0007-distributed-job-locking.md)); the API creates its table
  (`shedLock`) on first use. The charge work is queued in the same database. No broker
  and no second service is needed, and with one API container no other process competes for a
  lock. Keep it at one API container: the lock limits a second one to one run per tick, but this
  stack is not tested with more than one.
- **A restart in the middle of a job** (an `install`, an `upgrade`, a reboot) leaves that job's
  lock in place until it expires: one minute to one hour, depending on the job. The job then runs
  again at its next tick.
- **No job triggers on the management port.** In the releases this script runs with the scheduler
  (after v0.6.0), the API's unauthenticated `POST /debug/*` job triggers (`run-charge`,
  `run-collect`, `send-test-mail` and the others,
  [the list](../../docs/jobs-scheduling.md#mgmt-debug-triggers)) come only with
  `PROXCLOUD_JOBS_DEBUG_TRIGGERS`, a development and lab setting this stack never sets. The
  scheduler does not mount them, so port 8081 serves health, readiness and build information only.
  The port is not published on the host and Traefik does not route it, but every process on the
  Docker host can reach it on the container address: do not add the debug triggers setting here,
  for example in a Compose override file. The authenticated operator job API on the API hostname
  (`POST /api/v1/admin/job/...` with an admin credential, see
  [operator job API](../../docs/jobs-scheduling.md#operator-job-api)) runs the charge, metrics,
  sync, collect, savings expiry, reminder and transaction scan jobs on demand. The other jobs,
  among them the bills, dunning and project deletion, have no admin endpoint and run only on the
  scheduler's timer here, and `send-test-mail` is not available on this stack.
- **Mail.** Bills and reminders are sent through the SMTP integration; this stack has none until
  you add it ([What is not included](#what-is-not-included)).
- **Keep the host clock synchronised** (NTP): job times and lock expiry use it.

Not verified on a Compose install: a full billing month with the scheduler on, what the bill and
reminder jobs do while no SMTP integration exists, and how charging catches up after the host
was down.

- **`--cluster-gateway <address>`** serves the gateway listener of the managed clusters' controllers:
  the call of a cluster's load balancer controller (`PUT /lb/v1/state`), and the CSI gateway. It is
  for v0.11.0's load balancers of tenant clusters, whose controller (cluster chart 0.1.17) runs this
  API's own image: the file sets `PROXCLOUD_LB_CONTROLLER_IMAGE` to
  `ghcr.io/mutex-solutions/proxcloud-api:<installed version>`. Block volumes stay off on this stack,
  as no gateway token is registered. `<address>` is
  an IPv4 address of this host that the platform cluster's nodes reach; the script refuses anything
  else, `0.0.0.0` included. It writes `CLUSTER_GATEWAY_ADDRESS` and adds
  `compose.cluster-gateway.yaml` to `COMPOSE_FILE` in `.env`: the API listens on 8444, Docker
  publishes it on `<address>:8444` and nowhere else, and on its first start the API writes an ECDSA
  key and a self-signed certificate for the address (ten years) to the volume `gateway`, which the
  clusters then trust. Keep that volume: a new certificate means a new gateway Secret for every
  cluster. Backups of the database do not hold it; `uninstall` keeps it, `--purge` deletes it.
  `--no-cluster-gateway` turns the listener off. It needs ProxCloud v0.11.0 or later (or a `dev-<sha>`
  build): an older API cannot make the certificate. Run on one lab (v0.11.0-rc.1, six switches, and
  v0.11.0-rc.2, two): on an existing install the command takes 14 to 19 s and recreates only the API container; nothing else
  listens on 8444 than `<address>:8444`; the certificate is ECDSA P-256 with the address as its SAN,
  valid ten years from one hour back, its key mode 600; a call without a credential or with a wrong one
  is answered 401 on the load balancer path and on the CSI paths; the platform cluster's nodes reached
  it over the same L2 with no route added; `--no-cluster-gateway` removes the listener and the two
  `.env` entries and keeps the volume.

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
Both hold secrets, and **together they open every secret the portal stores**: the dump holds provider
credentials and your tenants' TLS private keys (the certificates of their load balancers, and those their
Kubernetes clusters' Services end TLS with, read from the clusters' Secrets) encrypted,
and the copy of `.env` holds `ENCRYPTION_KEY`, which decrypts them. Whoever can read `backups/` can
read every tenant's private key. Keep the directory readable by root alone, encrypt what you copy off
the host, and store the two files apart if your tooling allows it. Every `upgrade` writes a backup
here too. Run it from cron and copy the files off the host. Not in the backup:

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

To start a restored database without new timed jobs, end with `./proxcloud-up.sh install
--no-scheduler`, and turn the scheduler on again when you have checked the data. That is not a
start without billing writes: billing work already queued in the database is still processed by
the API's job worker when it starts, and a backup taken while the scheduler was on (every
`upgrade` takes one) can hold such work. To read the data before anything is written, query the
restored database before the last `install`
(`docker compose exec postgres psql -U proxcloud -d proxcloud`).

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

Traefik, Keycloak and PostgreSQL are pinned to a version in `compose.yaml`; they move with the
checkout. PostgreSQL is pinned to its minor release and to one build of it, by digest
(`postgres:17.12-alpine@sha256:f9cf3e1f0066f0eb0c48707d864f0ac5941e9ff454a072077d97a4d278d07d73`): the
publisher rebuilds a version's tag, and by the tag alone a container made later would be another
build than the one that ran. An upgrade restarts the database only when the release changes that
line, and then it does: `upgrade` recreates the PostgreSQL container, after the backup it always
takes first. The API and Keycloak connect again by themselves; the volume and its data stay.
Measured once on a lab: the database was away for about 2 seconds, the API answered errors for
about 2 seconds, and Keycloak refused sign-ins for about 10 seconds. A new PostgreSQL major version needs a dump and
restore, not just a new image.

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
- VM template updates, the job scheduler and managed services are off unless you install with
  `--template-updates`, `--scheduler` or `--managed-services`. The sandbox flag and the debug
  triggers flag keep their defaults (off).
  The scheduler does not mount the unauthenticated job triggers on the management port; only the
  debug triggers flag does. That holds from the first release after v0.6.0, and the script runs no
  older release with the scheduler on: see [Optional features](#optional-features).

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
| `TEMPLATE_UPDATES_VM_ENABLED` | `true` or `false`: VM template updates (`--template-updates`, `--no-template-updates`) |
| `JOBS_SCHEDULER_ENABLED` | `true` or `false`: the job scheduler (`--scheduler`, `--no-scheduler`) |
| `PLATFORM_ENABLED` | `true` or `false`: managed services (`--managed-services`, `--no-managed-services`) |
| `CLUSTER_GATEWAY_ADDRESS` | The address of the gateway listener (`--cluster-gateway`); absent when it is off |
| `COMPOSE_FILE` | `compose.yaml`, plus `compose.cluster-gateway.yaml` with the gateway listener |
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
