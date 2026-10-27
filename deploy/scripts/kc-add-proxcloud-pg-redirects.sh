#!/usr/bin/env bash
# Add a ProxCloud deployment's redirect URIs / web origins to a Keycloak's clients so its client +
# admin UIs can complete OIDC login. Idempotent (unique), additive-only, safe to re-run. The admin
# password is read from a k8s Secret and never printed.
#
# Nothing cluster-specific is baked in — configure via env:
#   KC_URL         Keycloak base URL                          (required, e.g. https://auth.example.com)
#   NAMESPACE      namespace holding the Keycloak admin Secret (required)
#   UI_HOST        ProxCloud client UI hostname                 (required, e.g. proxcloud.example.com)
#   ADMIN_HOST     ProxCloud admin UI hostname                  (required, e.g. proxcloud-admin.example.com)
#   KUBE_CONTEXT   kube context                (default: current-context)
#   KC_SECRET      admin-password Secret name  (default: <NAMESPACE>-keycloak)
#   KC_SECRET_KEY  key within that Secret      (default: admin-password)
#   KC_ADMIN_USER  master-realm admin user     (default: admin)
#   UI_REALM       realm of the client UI      (default: clients)
#   ADMIN_REALM    realm of the admin UI       (default: master)
#   UI_CLIENT      client id of the client UI  (default: proxcloud-ui)
#   ADMIN_CLIENT   client id of the admin UI   (default: proxcloud-admin)
#
# Example:
#   KC_URL=https://auth.example.com NAMESPACE=proxcloud \
#   UI_HOST=proxcloud.example.com ADMIN_HOST=proxcloud-admin.example.com \
#     deploy/scripts/kc-add-proxcloud-pg-redirects.sh
set -euo pipefail
: "${KC_URL:?set KC_URL (Keycloak base URL)}"
: "${NAMESPACE:?set NAMESPACE (namespace of the Keycloak admin Secret)}"
: "${UI_HOST:?set UI_HOST (ProxCloud client UI hostname)}"
: "${ADMIN_HOST:?set ADMIN_HOST (ProxCloud admin UI hostname)}"
KUBE_CONTEXT="${KUBE_CONTEXT:-$(kubectl config current-context)}"
KC_SECRET="${KC_SECRET:-${NAMESPACE}-keycloak}"
KC_SECRET_KEY="${KC_SECRET_KEY:-admin-password}"
KC_ADMIN_USER="${KC_ADMIN_USER:-admin}"
UI_REALM="${UI_REALM:-clients}"
ADMIN_REALM="${ADMIN_REALM:-master}"
UI_CLIENT="${UI_CLIENT:-proxcloud-ui}"
ADMIN_CLIENT="${ADMIN_CLIENT:-proxcloud-admin}"

PW=$(kubectl --context "$KUBE_CONTEXT" -n "$NAMESPACE" get secret "$KC_SECRET" -o "jsonpath={.data.${KC_SECRET_KEY}}" | base64 -d)
TOK=$(curl -s "$KC_URL/realms/master/protocol/openid-connect/token" \
  -d grant_type=password -d client_id=admin-cli -d "username=$KC_ADMIN_USER" \
  --data-urlencode "password=$PW" | jq -r .access_token)
if [ -z "$TOK" ] || [ "$TOK" = null ]; then echo "TOKEN FAIL"; exit 1; fi
echo "admin token acquired: ${#TOK} chars"

patch_client() {
  local realm=$1 clientId=$2 host=$3 cj uuid updated
  cj=$(curl -s -H "Authorization: Bearer $TOK" "$KC_URL/admin/realms/$realm/clients?clientId=$clientId")
  uuid=$(echo "$cj" | jq -r '.[0].id')
  if [ -z "$uuid" ] || [ "$uuid" = null ]; then echo "$realm/$clientId NOT FOUND"; return 1; fi
  echo "== $realm/$clientId (uuid=$uuid) =="
  echo "before: $(echo "$cj" | jq -c '{redirectUris:.[0].redirectUris, webOrigins:.[0].webOrigins}')"
  updated=$(echo "$cj" | jq --arg r "https://$host/*" --arg o "https://$host" '.[0]
    | .redirectUris = ((.redirectUris // []) + [$r] | unique)
    | .webOrigins   = ((.webOrigins   // []) + [$o] | unique)
    | .attributes = ((.attributes // {}) + {"post.logout.redirect.uris":
        (((.attributes."post.logout.redirect.uris" // "") | split("##") | map(select(length>0))) + [$r] | unique | join("##")))}')
  curl -s -o /dev/null -w "PUT status: %{http_code}\n" -X PUT \
    -H "Authorization: Bearer $TOK" -H "Content-Type: application/json" \
    "$KC_URL/admin/realms/$realm/clients/$uuid" -d "$updated"
  echo "after:  $(curl -s -H "Authorization: Bearer $TOK" "$KC_URL/admin/realms/$realm/clients/$uuid" | jq -c '{redirectUris, webOrigins}')"
}

patch_client "$UI_REALM"    "$UI_CLIENT"    "$UI_HOST"
patch_client "$ADMIN_REALM" "$ADMIN_CLIENT" "$ADMIN_HOST"
echo "DONE"
