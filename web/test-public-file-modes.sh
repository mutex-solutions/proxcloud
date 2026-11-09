#!/bin/sh
# Build with APP=client and APP=admin from a checkout made under umask 077, then:
# sh web/test-public-file-modes.sh <client-image> <admin-image>
set -eu
[ "$#" -gt 0 ] || { echo "usage: $0 <image> [image ...]" >&2; exit 2; }

for image do
  for config in existing missing; do
    docker run --rm -i --entrypoint sh \
      -e PROXCLOUD_API_URL=http://api.test:39800/api-v1 \
      -e PROXCLOUD_OAUTH2_ISSUER=http://auth.test:39890/realms/clients \
      "$image" -es -- "$config" <<'SH'
# Inspect the actual final image before exercising its real nginx entrypoint.
test -z "$(find /usr/share/nginx/html -type d ! -perm 0755 -print)"
test -z "$(find /usr/share/nginx/html -type f ! -perm 0644 -print)"
umask 077
printf 'private\n' > /tmp/private-file
case "$1" in
  existing) chmod 0600 /usr/share/nginx/html/config.js ;;
  missing)
    test "$(readlink -f /usr/share/nginx/html/config.js)" = /usr/share/nginx/html/config.js
    rm /usr/share/nginx/html/config.js
    ;;
esac
/docker-entrypoint-proxcloud.sh > /tmp/nginx-test.log 2>&1 &
pid=$!
trap 'status=$?; kill -TERM "$pid" 2>/dev/null || :; wait "$pid" || :; if [ "$status" -ne 0 ]; then cat /tmp/nginx-test.log >&2; fi' EXIT

# nginx workers, not the root test process, must be able to serve each file.
tries=0
until wget -qO /tmp/config.http http://127.0.0.1/config.js; do
  kill -0 "$pid"
  tries=$((tries + 1))
  test "$tries" -lt 50
  sleep 0.1
done
test "$(stat -c %a /usr/share/nginx/html/config.js)" = 644
test "$(stat -c %a /tmp/private-file)" = 600
case "$(cat /tmp/config.http)" in
  *'apiUrl: "http://api.test:39800/api-v1"'*'authIssuer: "http://auth.test:39890/realms/clients"'*) ;;
  *) echo 'runtime config did not reach HTTP' >&2; exit 1 ;;
esac
for asset in config.js index.html brand/logo.svg brand/favicon.svg; do
  wget -qO /tmp/asset.http "http://127.0.0.1/$asset"
  cmp "/usr/share/nginx/html/$asset" /tmp/asset.http
done
SH
    printf 'PASS %s: public modes + nginx HTTP (config %s, umask 077)\n' "$image" "$config"
  done
done
