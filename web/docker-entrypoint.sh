#!/bin/sh
# Generates the runtime config the SPA reads (window.__PROXCLOUD__) from the
# chart-injected env, then serves the static bundle.
set -e
if [ -n "${PROXCLOUD_API_HOST:-}" ]; then
  API_URL="${PROXCLOUD_API_HOST}${PROXCLOUD_API_PREFIX:-/api-v1}"
else
  API_URL="${PROXCLOUD_API_URL:-}"
fi
cat > /usr/share/nginx/html/config.js <<CFG
window.__PROXCLOUD__ = {
  apiUrl: "${API_URL}",
  authIssuer: "${PROXCLOUD_OAUTH2_ISSUER:-}",
  authClientId: "${PROXCLOUD_OAUTH2_CLIENT_ID:-}",
  authScope: "${PROXCLOUD_OAUTH2_SCOPE:-openid profile email}"
};
CFG
exec nginx -g 'daemon off;'
