#!/usr/bin/env bash
# ProxCloud portal on one host with Docker Compose: install, upgrade, status, backup, uninstall.
# Operator guide: README.md next to this file.
set -euo pipefail
umask 077

# Release pin of the proxcloud-{api,web,admin} images. Kept equal to the chart appVersion by
# .github/scripts/tests/test_release_policy.py.
DEFAULT_VERSION="v0.6.0"

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
cd "$DIR"
ENV_FILE=.env
TLS_MODES="internal acme-http acme-dns-cloudflare"
SECRET_KEYS="PG_PASSWORD KEYCLOAK_DB_PASSWORD KC_ADMIN_PASSWORD SSO_CLIENT_SECRET ENCRYPTION_KEY"

usage() {
  cat <<'EOF'
Usage: proxcloud-up.sh [install] [options]
       proxcloud-up.sh upgrade <version> | status | backup | uninstall [--purge]

install (default) is idempotent: it asks only for what is missing, keeps the secrets already in
.env, and may be re-run to change a hostname, the TLS mode or the ports.

  --domain <d>                 base domain: app.<d>, admin.<d>, api.<d>, id.<d>
  --app-host <h> --admin-host <h> --api-host <h> --auth-host <h>
                               the four hostnames, one by one (each overrides --domain)
  --tls <mode>                 internal | acme-http | acme-dns-cloudflare
  --acme-email <e>             Let's Encrypt account email (acme modes)
  --cloudflare-token-file <f>  Cloudflare API token file, copied to ./cf-token (or CF_DNS_API_TOKEN)
  --operator-email <e>         first operator, created in Keycloak realm master
  --version <vX.Y.Z>           ProxCloud release of a first install (default: the pinned release).
                               Afterwards the release changes with: upgrade <version>
  --http-port <n> --https-port <n>   host ports (default 80 and 443)
  --project-name <n>           Compose project name (default proxcloud-prod; fixed after the first install)

Common:
  --yes        answer yes to confirmations (uninstall)
  --purge      uninstall only: also delete the volumes (database, Keycloak, certificates)
  --dry-run    print the docker commands and write what .env would become to .env.dry-run;
               runs nothing, checks no host state, leaves .env and the install untouched
  -h, --help

Environment, same meaning as the flags: PROXCLOUD_DOMAIN, PROXCLOUD_APP_HOST, PROXCLOUD_ADMIN_HOST,
PROXCLOUD_API_HOST, PROXCLOUD_AUTH_HOST, PROXCLOUD_TLS_MODE, PROXCLOUD_ACME_EMAIL,
PROXCLOUD_OPERATOR_EMAIL, PROXCLOUD_VERSION, CF_DNS_API_TOKEN. No other variable of the calling shell
reaches Compose: the stack runs with the values in .env.
EOF
}

die() { echo "error: $*" >&2; exit 1; }
warn() { echo "warning: $*" >&2; }

# ---- .env: plain KEY=VALUE lines. Every value is validated below, so none needs quoting. ----
env_get() { [ -f "$ENV_FILE" ] && sed -n "s/^$1=//p" "$ENV_FILE" | tail -n 1; return 0; }

env_set() { # KEY VALUE: replace the line or append it; every other line is kept
  local tmp
  tmp="$(mktemp "$ENV_FILE.XXXXXX")"
  awk -v k="$1" -v v="$2" '
    index($0, k "=") == 1 { print k "=" v; done = 1; next }
    { print }
    END { if (!done) print k "=" v }' "$ENV_FILE" > "$tmp"
  mv "$tmp" "$ENV_FILE"
}

env_del() { # KEY: drop its line
  local tmp
  tmp="$(mktemp "$ENV_FILE.XXXXXX")"
  grep -v "^$1=" "$ENV_FILE" > "$tmp" || true
  mv "$tmp" "$ENV_FILE"
}

gen_hex() { head -c "$1" /dev/urandom | od -An -tx1 | tr -d ' \n'; }

first() { # the first non-empty argument
  local v
  for v in "$@"; do
    if [ -n "$v" ]; then printf '%s' "$v"; return 0; fi
  done
}

ask() { # PROMPT [DEFAULT]: empty when stdin is not a terminal
  local answer=""
  [ -t 0 ] || return 0
  read -r -p "$1${2:+ [$2]}: " answer || true
  printf '%s' "${answer:-${2:-}}"
}

confirm() { # PROMPT EXPECTED: true with --yes, or when the terminal answer equals EXPECTED
  local answer=""
  [ "$ASSUME_YES" = 1 ] && return 0
  [ -t 0 ] || die "not a terminal: pass --yes to confirm"
  read -r -p "$1 " answer || true
  [ "$answer" = "$2" ]
}

lower() { printf '%s' "$1" | tr '[:upper:]' '[:lower:]'; }
is_host() { [[ "$1" =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$ ]]; }
is_email() { [[ "$1" =~ ^[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}$ ]]; }
is_port() { [[ "$1" =~ ^[0-9]{1,5}$ ]] && [ "$1" -ge 1 ] && [ "$1" -le 65535 ]; }
is_version() { [[ "$1" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.(0|[1-9][0-9]*))?$ ]]; }
is_project() { [[ "$1" =~ ^[a-z0-9][a-z0-9_-]*$ ]]; }

# ---- docker ----
# docker compose for this project. Compose lets the calling shell's environment win over .env, so
# every name .env sets (and three more that would change the model) is dropped from it first: an
# exported PROXCLOUD_VERSION or HTTP_PORT cannot start something else than .env says.
# PULL_VERSION is the one deliberate override (upgrade pulls before it writes .env).
compose() {
  local drop=(-u COMPOSE_FILE -u COMPOSE_ENV_FILES -u ACME_CA_SERVER) key
  while IFS= read -r key; do
    drop+=(-u "$key")
  done < <(sed -n 's/^\([A-Za-z_][A-Za-z0-9_]*\)=.*/\1/p' "$ENV_FILE")
  env "${drop[@]}" ${PULL_VERSION:+"PROXCLOUD_VERSION=$PULL_VERSION"} docker compose -p "$PROJECT" "$@"
}

dc() { # compose; printed, not run, with --dry-run
  if [ "$DRY_RUN" = 1 ]; then echo "+ docker compose -p $PROJECT $*"; return 0; fi
  compose "$@"
}

preflight() {
  local engine compose major minor
  command -v docker >/dev/null || die "docker is not installed"
  engine="$(docker version --format '{{.Server.Version}}' 2>/dev/null)" \
    || die "cannot reach the Docker daemon: is it running, and may this user use it?"
  compose="$(docker compose version --short 2>/dev/null)" \
    || die "the Docker Compose plugin (docker compose, v2 or later) is not installed"
  compose="${compose#v}"
  IFS=. read -r major minor _ <<< "$engine"
  if ! [[ "$major" =~ ^[0-9]+$ && "$minor" =~ ^[0-9]+$ ]] || [ $((10#$major * 100 + 10#$minor)) -lt 2010 ]; then
    die "Docker Engine 20.10 or later is required (found $engine)"
  fi
  if ! [[ "${compose%%.*}" =~ ^[0-9]+$ ]] || [ "${compose%%.*}" -lt 2 ]; then
    die "Docker Compose v2 or later is required (found $compose)"
  fi
}

# ponytail: probes 127.0.0.1 only; a listener bound to one other address shows up as Docker's own bind error.
port_in_use() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }

# Never take over what something else owns. The development stack (docker-compose.yml in the
# repository root) has the same service and volume names, and its project is named after the
# checkout directory: under one project name each side would recreate, and with "down -v" delete,
# the other's database.
guard_project() {
  local other
  if [ -f ../../docker-compose.yml ] && [ "$PROJECT" = "$(basename "$(cd ../.. && pwd -P)")" ]; then
    die "'$PROJECT' is the Compose project of the development stack of this checkout (docker-compose.yml in its root); choose another --project-name"
  fi
  other="$(docker ps -a --filter "label=com.docker.compose.project=$PROJECT" \
    --format '{{.Label "com.docker.compose.project.working_dir"}}' | grep -vxF "$DIR" | head -n 1 || true)"
  [ -z "$other" ] || die "Compose project '$PROJECT' already has containers, created from '$other' and not from $DIR. Another stack: choose another --project-name. This checkout, moved: run ./proxcloud-up.sh uninstall first (the data is kept)."
  # The volume outlives "uninstall" and a deleted checkout; its database only opens with the
  # passwords, and its content only decrypts with the key, of the .env it was created with.
  if [ -z "$(env_get PG_PASSWORD)" ] && docker volume inspect "${PROJECT}_postgres-data" >/dev/null 2>&1; then
    die "the volume ${PROJECT}_postgres-data exists, but there is no .env with its passwords here. Restore your .env (backups/env-<time>) and run install again; or, for another install on this host, choose another --project-name."
  fi
}

load_env() { # for every command but install
  [ -f "$ENV_FILE" ] || die "no .env here: run ./proxcloud-up.sh install first"
  PROJECT="$(env_get COMPOSE_PROJECT_NAME)"
  is_project "$PROJECT" || die "COMPOSE_PROJECT_NAME is missing or invalid in .env"
  TLS_MODE="$(env_get COMPOSE_PROFILES)"
  HTTPS_PORT="$(env_get HTTPS_PORT)" URL_PORT="$(env_get URL_PORT)"
  APP_HOST="$(env_get APP_HOST)" ADMIN_HOST="$(env_get ADMIN_HOST)"
  API_HOST="$(env_get API_HOST)" AUTH_HOST="$(env_get AUTH_HOST)"
}

wait_for() { # SECONDS LABEL COMMAND...: false when the time is up
  local deadline=$((SECONDS + $1)) label=$2
  shift 2
  printf 'Waiting for %s ' "$label"
  until "$@" >/dev/null 2>&1; do
    if [ "$SECONDS" -ge "$deadline" ]; then
      echo ' not ready'
      return 1
    fi
    printf '.'
    sleep 5
  done
  echo ' ok'
}

wait_oneshot() { # SECONDS SERVICE LABEL: until its container ends; a failed run is fatal at once
  local deadline=$((SECONDS + $1)) service=$2 label=$3 id state
  [ "$DRY_RUN" = 1 ] && return 0
  printf 'Waiting for %s ' "$label"
  while :; do
    id="$(compose ps -aq "$service")"
    state="$(docker inspect -f '{{.State.Status}}:{{.State.ExitCode}}' "$id" 2>/dev/null || true)"
    case "$state" in
      exited:0) echo ' ok'; return 0 ;;
      exited:*)
        echo
        die "$service failed. Look at: docker compose -p $PROJECT logs $service (README.md, Troubleshooting)"
        ;;
    esac
    if [ "$SECONDS" -ge "$deadline" ]; then
      echo
      die "$label is not ready. Look at: docker compose -p $PROJECT logs $service"
    fi
    printf '.'
    sleep 5
  done
}

# The container health check (compose.yaml) asks the API's readiness endpoint; Traefik routes to the
# API only once Docker reports it healthy, so that is the state to wait for.
api_ready() {
  local id
  id="$(compose ps -q api)"
  [ -n "$id" ] && [ "$(docker inspect -f '{{.State.Health.Status}}' "$id")" = healthy ]
}

# Traefik takes a container up a moment after Docker reports it healthy: ask at the front door,
# on this host, whatever DNS says. Its own answer for an unknown route is 404.
api_routed() {
  local code
  code="$(curl -sk -o /dev/null -w '%{http_code}' --max-time 5 --resolve "$API_HOST:$HTTPS_PORT:127.0.0.1" \
    "https://$API_HOST:$HTTPS_PORT/api/v1/user")"
  case "$code" in 000|404|502|503|504) return 1 ;; esac
}

# Start or update the stack. Keycloak and its realms come first: the API reads their OIDC metadata
# when it starts, and a failed realm import stops here, before any running service is replaced.
bring_up() {
  local mode others=()
  dc up -d keycloak-config
  wait_oneshot 600 keycloak-config "Keycloak and its realms"
  # Each TLS mode is its own Traefik service and all three publish the same ports: one of another
  # mode has to give them back. Only now, so that a failed realm import leaves it serving.
  for mode in $TLS_MODES; do
    [ "$mode" = "$TLS_MODE" ] || others+=("traefik-$mode")
  done
  dc rm -sf "${others[@]}"
  dc up -d --remove-orphans
  [ "$DRY_RUN" = 1 ] && return 0
  wait_oneshot 300 seed "the first-run seed"
  wait_for 300 "the API" api_ready \
    || die "the API is not ready. Look at: docker compose -p $PROJECT ps -a, and: docker compose -p $PROJECT logs api"
  # Not fatal: where Docker publishes ports on one address only, 127.0.0.1 does not answer.
  if command -v curl >/dev/null && ! wait_for 120 "the API through Traefik" api_routed; then
    warn "no answer from the API at https://$API_HOST:$HTTPS_PORT on 127.0.0.1. Expected if Docker publishes ports on another address only; otherwise look at: docker compose -p $PROJECT logs traefik-$TLS_MODE"
  fi
}

print_urls() {
  cat <<EOF
  Customer console  https://$APP_HOST$URL_PORT    customers register themselves (realm clients)
  Operator console  https://$ADMIN_HOST$URL_PORT  operators only (realm master)
  API               https://$API_HOST$URL_PORT
  Keycloak          https://$AUTH_HOST$URL_PORT   user admin, password KC_ADMIN_PASSWORD in $DIR/.env
EOF
}

# The first operator: a plain user of realm master with a temporary password. Any user of realm
# master that signs in to the operator console is a ProxCloud super admin; no role is needed.
create_operator() {
  local email=$1 password result
  if [ "$DRY_RUN" = 1 ]; then
    echo "+ docker compose -p $PROJECT exec -T keycloak kcadm.sh (create user $email in realm master, temporary password)"
    return 0
  fi
  password="$(gen_hex 10)"
  # No password is ever a command argument, here or in the container (whose processes any local
  # user of this host can list): the admin password reaches kcadm in its environment, and the user
  # with its password as one JSON document on stdin. One call creates both, so a failure cannot
  # leave an operator without a password. The email is validated: it needs no JSON escaping.
  # The quoted script is expanded by the shell in the container, not by this one:
  # shellcheck disable=SC2016
  result="$(printf '%s\n{"username":"%s","email":"%s","emailVerified":true,"enabled":true,"credentials":[{"type":"password","value":"%s","temporary":true}]}\n' \
      "$email" "$email" "$email" "$password" | compose exec -T keycloak sh -c '
    set -e
    read -r email
    kcadm=/opt/keycloak/bin/kcadm.sh
    config=/tmp/pc-kcadm.config
    trap "rm -f $config" EXIT
    KC_CLI_PASSWORD="$KC_BOOTSTRAP_ADMIN_PASSWORD" "$kcadm" config credentials --config "$config" \
      --server http://localhost:8080 --realm master --user "$KC_BOOTSTRAP_ADMIN_USERNAME" </dev/null >/dev/null 2>&1
    if "$kcadm" get users --config "$config" -r master -q "username=$email" -q exact=true --fields username </dev/null \
        | grep -qF "\"$email\""; then
      echo exists
      exit 0
    fi
    "$kcadm" create users --config "$config" -r master -f - >/dev/null 2>&1
    echo created
  ')" || die "could not create the operator in Keycloak. If the admin password was changed in Keycloak, add the operator there (realm master)."
  case "$result" in
    *created)
      printf '%s\n' "$password" > operator-password
      chmod 600 operator-password
      echo "Operator $email created. Temporary password: $DIR/operator-password (Keycloak asks for a new one at first login; delete the file then)."
      ;;
    *) echo "Operator $email already exists in realm master: password left unchanged." ;;
  esac
  env_set OPERATOR_EMAIL "$email"
}

cmd_install() {
  local domain missing="" key value previous_project previous_version recorded_operator token="" operator_email

  # Nothing is written before every input is validated and guard_project has passed: a run that
  # stops early leaves no .env behind.
  [ "$DRY_RUN" = 1 ] || preflight

  previous_project="$(env_get COMPOSE_PROJECT_NAME)"
  PROJECT="$(first "$F_PROJECT" "$previous_project" proxcloud-prod)"
  is_project "$PROJECT" || die "invalid --project-name '$PROJECT' (lowercase letters, digits, - and _)"
  [ -z "$previous_project" ] || [ "$PROJECT" = "$previous_project" ] \
    || die "this install is Compose project '$previous_project': the project name cannot change (its volumes carry that name)"

  TLS_MODE="$(first "$F_TLS" "${PROXCLOUD_TLS_MODE:-}" "$(env_get COMPOSE_PROFILES)")"
  [ -n "$TLS_MODE" ] || TLS_MODE="$(ask "TLS mode (${TLS_MODES// / | })" acme-http)"

  domain="$(lower "$(first "$F_DOMAIN" "${PROXCLOUD_DOMAIN:-}")")"
  resolve_hosts() {
    APP_HOST="$(lower "$(first "$F_APP_HOST" "${PROXCLOUD_APP_HOST:-}" "${domain:+app.$domain}" "$(env_get APP_HOST)")")"
    ADMIN_HOST="$(lower "$(first "$F_ADMIN_HOST" "${PROXCLOUD_ADMIN_HOST:-}" "${domain:+admin.$domain}" "$(env_get ADMIN_HOST)")")"
    API_HOST="$(lower "$(first "$F_API_HOST" "${PROXCLOUD_API_HOST:-}" "${domain:+api.$domain}" "$(env_get API_HOST)")")"
    AUTH_HOST="$(lower "$(first "$F_AUTH_HOST" "${PROXCLOUD_AUTH_HOST:-}" "${domain:+id.$domain}" "$(env_get AUTH_HOST)")")"
  }
  resolve_hosts
  if [ -z "$APP_HOST" ] || [ -z "$ADMIN_HOST" ] || [ -z "$API_HOST" ] || [ -z "$AUTH_HOST" ]; then
    domain="$(lower "$(ask "Base domain (hosts app., admin., api. and id. under it; empty to type each hostname)")")"
    resolve_hosts
  fi
  [ -n "$APP_HOST" ] || APP_HOST="$(lower "$(ask "Customer console hostname")")"
  [ -n "$ADMIN_HOST" ] || ADMIN_HOST="$(lower "$(ask "Operator console hostname")")"
  [ -n "$API_HOST" ] || API_HOST="$(lower "$(ask "API hostname")")"
  [ -n "$AUTH_HOST" ] || AUTH_HOST="$(lower "$(ask "Keycloak hostname")")"

  HTTP_PORT="$(first "$F_HTTP_PORT" "$(env_get HTTP_PORT)" 80)"
  HTTPS_PORT="$(first "$F_HTTPS_PORT" "$(env_get HTTPS_PORT)" 443)"
  previous_version="$(env_get PROXCLOUD_VERSION)"
  VERSION="$(first "$F_VERSION" "${PROXCLOUD_VERSION:-}" "$previous_version" "$DEFAULT_VERSION")"

  ACME_EMAIL="$(first "$F_ACME_EMAIL" "${PROXCLOUD_ACME_EMAIL:-}" "$(env_get ACME_EMAIL)")"
  case "$TLS_MODE" in
    acme-*) [ -n "$ACME_EMAIL" ] || ACME_EMAIL="$(ask "Let's Encrypt account email")" ;;
  esac

  # Once created, the operator is not asked for again; more operators are added in Keycloak.
  # Lower case, as Keycloak stores a username.
  recorded_operator="$(env_get OPERATOR_EMAIL)"
  operator_email="$recorded_operator"
  if [ -z "$operator_email" ]; then
    operator_email="$(lower "$(first "$F_OPERATOR_EMAIL" "${PROXCLOUD_OPERATOR_EMAIL:-}")")"
    [ -n "$operator_email" ] || operator_email="$(lower "$(ask "Email of the first operator")")"
  elif [ -n "$F_OPERATOR_EMAIL" ] && [ "$(lower "$F_OPERATOR_EMAIL")" != "$operator_email" ]; then
    warn "the first operator ($operator_email) already exists; add more operators in Keycloak, realm master"
  fi

  [ -n "$TLS_MODE" ] || missing="$missing --tls"
  [ -n "$APP_HOST" ] || missing="$missing --app-host"
  [ -n "$ADMIN_HOST" ] || missing="$missing --admin-host"
  [ -n "$API_HOST" ] || missing="$missing --api-host"
  [ -n "$AUTH_HOST" ] || missing="$missing --auth-host"
  [ -n "$operator_email" ] || missing="$missing --operator-email"
  case "$TLS_MODE" in
    acme-*) [ -n "$ACME_EMAIL" ] || missing="$missing --acme-email" ;;
  esac
  [ -z "$missing" ] || die "missing:$missing (the four hostnames may come from --domain). See --help."

  case " $TLS_MODES " in
    *" $TLS_MODE "*) ;;
    *) die "invalid TLS mode '$TLS_MODE' (one of: $TLS_MODES)" ;;
  esac
  for value in "$APP_HOST" "$ADMIN_HOST" "$API_HOST" "$AUTH_HOST"; do
    is_host "$value" || die "invalid hostname '$value' (a full DNS name, for example app.example.com)"
  done
  [ "$(printf '%s\n' "$APP_HOST" "$ADMIN_HOST" "$API_HOST" "$AUTH_HOST" | sort -u | wc -l)" -eq 4 ] \
    || die "the four hostnames must differ"
  is_port "$HTTP_PORT" || die "invalid --http-port '$HTTP_PORT'"
  is_port "$HTTPS_PORT" || die "invalid --https-port '$HTTPS_PORT'"
  [ "$HTTP_PORT" != "$HTTPS_PORT" ] || die "--http-port and --https-port must differ"
  is_version "$VERSION" || die "invalid version '$VERSION' (vX.Y.Z or vX.Y.Z-rc.N)"
  # An install that completed (its operator exists) changes release with "upgrade": backup, then pull.
  if [ -n "$recorded_operator" ] && [ -n "$previous_version" ] && [ "$VERSION" != "$previous_version" ]; then
    die "this install runs $previous_version: change the release with ./proxcloud-up.sh upgrade $VERSION (it takes a backup first)"
  fi
  is_email "$operator_email" || die "invalid operator email '$operator_email'"
  case "$TLS_MODE" in
    acme-*) is_email "$ACME_EMAIL" || die "invalid ACME email '$ACME_EMAIL'" ;;
  esac
  if [ "$TLS_MODE" = acme-http ] && [ "$HTTP_PORT" != 80 ]; then
    warn "acme-http: Let's Encrypt validates on public port 80. Forward it to this host's port $HTTP_PORT, or no certificate is issued."
  fi

  [ -z "$F_CF_TOKEN_FILE" ] || [ "$TLS_MODE" = acme-dns-cloudflare ] \
    || die "--cloudflare-token-file belongs to --tls acme-dns-cloudflare"
  if [ "$TLS_MODE" = acme-dns-cloudflare ]; then
    if [ -n "$F_CF_TOKEN_FILE" ]; then
      [ -r "$F_CF_TOKEN_FILE" ] || die "cannot read --cloudflare-token-file $F_CF_TOKEN_FILE"
      token="$(tr -d '[:space:]' < "$F_CF_TOKEN_FILE")"
    elif [ -n "${CF_DNS_API_TOKEN:-}" ]; then
      token="$CF_DNS_API_TOKEN"
    elif [ ! -s cf-token ] && [ -t 0 ]; then
      read -r -s -p "Cloudflare API token (Zone:DNS:Edit on the zone; not shown): " token || true
      echo >&2
    fi
    [ -z "$token" ] || [[ "$token" =~ ^[A-Za-z0-9_-]+$ ]] || die "the Cloudflare API token has unexpected characters"
    [ -n "$token" ] || [ -s cf-token ] || die "missing: --cloudflare-token-file (or CF_DNS_API_TOKEN)"
  fi

  if [ "$DRY_RUN" = 0 ]; then
    guard_project
    case "$TLS_MODE" in
      acme-*)
        if command -v getent >/dev/null; then
          for value in "$APP_HOST" "$ADMIN_HOST" "$API_HOST" "$AUTH_HOST"; do
            getent hosts "$value" >/dev/null \
              || warn "$value does not resolve on this host. Without its DNS record no certificate is issued for it, and failed attempts count against the Let's Encrypt rate limits (README.md, Troubleshooting)."
          done
        fi
        ;;
    esac
    if [ "$TLS_MODE" = acme-dns-cloudflare ]; then
      [ -z "$token" ] || printf '%s' "$token" > cf-token
      chmod 600 cf-token
    fi
    # A checkout made under umask 077 is unreadable to the containers' own users, and PostgreSQL
    # would skip its init script without a word. These three directories hold no secret.
    chmod -R a+rX postgres-init keycloak-config seed
  fi

  if [ ! -f "$ENV_FILE" ]; then
    cat > "$ENV_FILE" <<'EOF'
# Written by proxcloud-up.sh. Keep it mode 600 and BACK IT UP together with the database:
# ENCRYPTION_KEY decrypts the provider secrets stored there and cannot be recovered.
# Change settings by re-running ./proxcloud-up.sh install with flags, not by hand.
EOF
  fi
  chmod 600 "$ENV_FILE"
  env_set COMPOSE_PROJECT_NAME "$PROJECT"
  env_set COMPOSE_PROFILES "$TLS_MODE"
  env_set PROXCLOUD_VERSION "$VERSION"
  env_set APP_HOST "$APP_HOST"
  env_set ADMIN_HOST "$ADMIN_HOST"
  env_set API_HOST "$API_HOST"
  env_set AUTH_HOST "$AUTH_HOST"
  env_set HTTP_PORT "$HTTP_PORT"
  env_set HTTPS_PORT "$HTTPS_PORT"
  # Suffix of every public URL: empty on 443, else ":<https port>". Derived, do not edit.
  URL_PORT=""
  [ "$HTTPS_PORT" = 443 ] || URL_PORT=":$HTTPS_PORT"
  env_set URL_PORT "$URL_PORT"
  [ -z "$ACME_EMAIL" ] || env_set ACME_EMAIL "$ACME_EMAIL"

  # Secrets: generated once, never regenerated, never printed.
  for key in $SECRET_KEYS; do
    if [ -z "$(env_get "$key")" ]; then
      case "$key" in
        ENCRYPTION_KEY)
          env_set "$key" "$(gen_hex 32)"
          [ "$DRY_RUN" = 1 ] \
            || warn "new ENCRYPTION_KEY written to $DIR/.env. Back that file up now: without the key, stored provider secrets are lost."
          ;;
        *) env_set "$key" "$(gen_hex 24)" ;;
      esac
    fi
  done

  # Our own running stack already holds the ports on a re-run.
  if [ "$DRY_RUN" = 0 ] && [ -z "$(compose ps -q 2>/dev/null)" ]; then
    for value in "$HTTP_PORT" "$HTTPS_PORT"; do
      ! port_in_use "$value" || die "port $value is in use on this host; free it or pass --http-port / --https-port"
    done
  fi

  bring_up
  [ -n "$recorded_operator" ] || create_operator "$operator_email"

  echo
  echo "ProxCloud $VERSION is installed (TLS mode $TLS_MODE)."
  print_urls
  cat <<EOF

Next steps:
  1. Sign in to the operator console as $operator_email.
  2. Operator console -> System -> Cloud providers: register your Proxmox provider.
  3. Back up $DIR/.env off this host, and schedule: ./proxcloud-up.sh backup
EOF
  if [ "$TLS_MODE" = internal ]; then
    echo "  TLS mode internal: the certificate is self-signed. A browser must accept it for each of the four hostnames."
  fi
}

cmd_backup() {
  local stamp out
  load_env
  stamp="$(date -u +%Y%m%dT%H%M%SZ)"
  out="backups/proxcloud-$stamp.sql.gz"
  if [ "$DRY_RUN" = 1 ]; then
    echo "+ docker compose -p $PROJECT exec -T postgres pg_dumpall -U proxcloud | gzip > $out"
    return 0
  fi
  mkdir -p backups
  # pipefail: a failed dump fails the pipe, and the partial file is not kept under the final name.
  if ! compose exec -T postgres pg_dumpall -U proxcloud | gzip > "$out.partial"; then
    rm -f "$out.partial"
    die "the database dump failed; nothing was written"
  fi
  mv "$out.partial" "$out"
  cp -p "$ENV_FILE" "backups/env-$stamp"
  echo "Backup written: $DIR/$out and $DIR/backups/env-$stamp (secrets: keep both together, off this host)."
}

cmd_upgrade() {
  local version=${1:-}
  [ -n "$version" ] || die "usage: proxcloud-up.sh upgrade <version>"
  is_version "$version" || die "invalid version '$version' (vX.Y.Z or vX.Y.Z-rc.N)"
  load_env
  echo "Upgrading $(env_get PROXCLOUD_VERSION) -> $version (read the release notes first)."
  cmd_backup
  # Pull before .env changes, so a release that cannot be pulled leaves the running install as it is.
  PULL_VERSION="$version" dc pull
  env_set PROXCLOUD_VERSION "$version"
  bring_up
  echo "ProxCloud is now $version."
}

cmd_status() {
  load_env
  dc ps
  [ "$DRY_RUN" = 1 ] && return 0
  if api_ready; then echo "API: ready"; else echo "API: NOT ready"; fi
  echo "ProxCloud $(env_get PROXCLOUD_VERSION), TLS mode $(env_get COMPOSE_PROFILES):"
  print_urls
}

cmd_uninstall() {
  load_env
  if [ "$PURGE" = 1 ]; then
    confirm "This DELETES the database, Keycloak and the certificates of project '$PROJECT'. Type the project name to confirm:" "$PROJECT" \
      || die "not confirmed: nothing was removed"
    dc --profile setup down -v --remove-orphans
    # The first operator went with the Keycloak database: the next install creates it again.
    env_del OPERATOR_EMAIL
    [ "$DRY_RUN" = 1 ] || rm -f operator-password
    echo "Removed the containers and volumes. .env (the secrets) and backups/ are kept: delete them by hand."
  else
    confirm "Remove the containers of project '$PROJECT'? The volumes (database, certificates) are kept. Type yes:" yes \
      || die "not confirmed: nothing was removed"
    dc --profile setup down --remove-orphans
    echo "Removed the containers. The volumes are kept: ./proxcloud-up.sh install starts it again, uninstall --purge deletes them."
  fi
}

# ---- arguments ----
COMMAND=install
case "${1:-}" in
  install|upgrade|status|backup|uninstall) COMMAND=$1; shift ;;
  help) usage; exit 0 ;;
esac

DRY_RUN=0 ASSUME_YES=0 PURGE=0 POSITIONAL=""
F_DOMAIN="" F_APP_HOST="" F_ADMIN_HOST="" F_API_HOST="" F_AUTH_HOST="" F_TLS="" F_ACME_EMAIL=""
F_CF_TOKEN_FILE="" F_OPERATOR_EMAIL="" F_VERSION="" F_HTTP_PORT="" F_HTTPS_PORT="" F_PROJECT=""
PROJECT="" URL_PORT="" TLS_MODE="" PULL_VERSION="" INSTALL_OPTIONS=""

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY_RUN=1; shift; continue ;;
    --yes|-y) ASSUME_YES=1; shift; continue ;;
    --purge) PURGE=1; shift; continue ;;
    -h|--help) usage; exit 0 ;;
    --*=*) set -- "${1%%=*}" "${1#*=}" "${@:2}"; continue ;;
    --*) [ $# -ge 2 ] || die "$1 needs a value" ;;
    *)
      [ -z "$POSITIONAL" ] || die "unexpected argument '$1'"
      POSITIONAL=$1; shift; continue ;;
  esac
  case "$1" in
    --domain) F_DOMAIN=$2 ;;
    --app-host) F_APP_HOST=$2 ;;
    --admin-host) F_ADMIN_HOST=$2 ;;
    --api-host) F_API_HOST=$2 ;;
    --auth-host) F_AUTH_HOST=$2 ;;
    --tls) F_TLS=$2 ;;
    --acme-email) F_ACME_EMAIL=$2 ;;
    --cloudflare-token-file) F_CF_TOKEN_FILE=$2 ;;
    --operator-email) F_OPERATOR_EMAIL=$2 ;;
    --version) F_VERSION=$2 ;;
    --http-port) F_HTTP_PORT=$2 ;;
    --https-port) F_HTTPS_PORT=$2 ;;
    --project-name) F_PROJECT=$2 ;;
    *) die "unknown option '$1' (see --help)" ;;
  esac
  INSTALL_OPTIONS="$INSTALL_OPTIONS $1"
  shift 2
done

[ "$PURGE" = 0 ] || [ "$COMMAND" = uninstall ] || die "--purge belongs to uninstall"
[ -z "$POSITIONAL" ] || [ "$COMMAND" = upgrade ] || die "unexpected argument '$POSITIONAL'"
[ -z "$INSTALL_OPTIONS" ] || [ "$COMMAND" = install ] || die "$COMMAND does not take$INSTALL_OPTIONS (install only)"

# A dry run works on a copy of .env, left behind as the preview of what the real run would write.
if [ "$DRY_RUN" = 1 ]; then
  rm -f .env.dry-run
  [ ! -f .env ] || cp -p .env .env.dry-run
  ENV_FILE=.env.dry-run
fi

case "$COMMAND" in
  install) cmd_install ;;
  upgrade) cmd_upgrade "$POSITIONAL" ;;
  status) cmd_status ;;
  backup) cmd_backup ;;
  uninstall) cmd_uninstall ;;
esac
