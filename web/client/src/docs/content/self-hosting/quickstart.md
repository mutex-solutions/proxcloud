# MicroK8s Quickstart

The quickest way to try ProxCloud is a single-node MicroK8s cluster on one VM. It gives you a fully working platform — API, portal, admin console, PostgreSQL, RabbitMQ — on a machine you can discard afterward. Identity is configured separately: enable the bundled Keycloak or point at your own IdP — see [How Identity Works](/docs/concepts/identity).

## What you'll need

- An Ubuntu LTS VM with at least 4 vCPU / 8 GB RAM and disk headroom for the bundled databases.
- A public IP and DNS records for `cloud.example.com`, `api.cloud.example.com` and `admin.cloud.example.com` pointing at it — Let's Encrypt has to reach the VM to issue certificates.

## 1. Install MicroK8s

```sh
sudo snap install microk8s --classic
microk8s status --wait-ready
```

Working as a non-root user? Add yourself to the group and re-login:

```sh
sudo usermod -a -G microk8s $USER
sudo chown -f -R $USER ~/.kube
su - $USER
```

## 2. Turn on the addons

```sh
microk8s enable dns
microk8s enable ingress
microk8s enable hostpath-storage
microk8s enable cert-manager
```

That gives the chart everything it expects: cluster DNS, an NGINX ingress controller, a default StorageClass on the local disk, and cert-manager for TLS.

Create a Let's Encrypt ClusterIssuer:

```sh
microk8s kubectl apply -f - <<'EOF'
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: letsencrypt
spec:
  acme:
    server: https://acme-v02.api.letsencrypt.org/directory
    email: you@example.com
    privateKeySecretRef:
      name: letsencrypt-account-key
    solvers:
      - http01:
          ingress:
            class: public
EOF
```

## 3. Install ProxCloud

Write a minimal `values.yaml` — note MicroK8s's ingress class is `public`. The chart ships no default secrets, so set the required ones (insecure dev values shown here — never reuse them for anything real):

```yaml
api:
  encryptionKey: "insecure-dev-key-change-me"
  # One host per component: the consoles are built for /, and the API serves
  # more than /api (/admin-api/v1, /mcp, /e2b, /.well-known).
  selfUrls:
    base: "https://cloud.example.com"
    api: "https://api.cloud.example.com"
    ui: "https://cloud.example.com"
    admin: "https://admin.cloud.example.com"
  ingress:
    enabled: true
    className: "public"
    host: "api.cloud.example.com"
    path: /
    annotations:
      cert-manager.io/cluster-issuer: letsencrypt
    tls:
      - hosts: ["api.cloud.example.com"]
        secretName: proxcloud-api-tls
ui:
  ingress:
    enabled: true
    className: "public"
    host: "cloud.example.com"
    path: /
    annotations:
      cert-manager.io/cluster-issuer: letsencrypt
    tls:
      - hosts: ["cloud.example.com"]
        secretName: proxcloud-tls
admin:
  ingress:
    enabled: true
    className: "public"
    host: "admin.cloud.example.com"
    path: /
    annotations:
      cert-manager.io/cluster-issuer: letsencrypt
    tls:
      - hosts: ["admin.cloud.example.com"]
        secretName: proxcloud-admin-tls
postgresql:
  auth:
    password: "proxcloud"
```

The chart's default PostgreSQL and RabbitMQ are run by their operators, so install those first (the versions the installer pins):

```sh
microk8s helm repo add cnpg https://cloudnative-pg.github.io/charts
microk8s helm upgrade --install cnpg cnpg/cloudnative-pg --version 0.29.1 \
  --namespace cnpg-system --create-namespace --wait
microk8s kubectl apply -f https://github.com/rabbitmq/cluster-operator/releases/download/v2.23.2/cluster-operator.yml
```

(Without the operators, add `-f deploy/charts/proxcloud/values-dev.yaml` from a checkout of the repository instead: it switches to the bitnami subcharts with insecure dev secrets.)

Then install straight from the OCI registry:

```sh
microk8s helm upgrade --install proxcloud oci://ghcr.io/mutex-solutions/charts/proxcloud \
  --namespace proxcloud --create-namespace \
  -f values.yaml
```

First boot takes a few minutes: PostgreSQL and RabbitMQ start, and the API restarts until PostgreSQL answers.

```sh
microk8s kubectl -n proxcloud get pods
```

## 4. First login

- Customer portal: `https://cloud.example.com/`
- Admin console: `https://admin.cloud.example.com/`

You need an OIDC provider before you can sign in — enable the bundled Keycloak (`keycloakx.enabled: true` plus realm provisioning) or point `auth.*.issuer` at your own IdP; see [How Identity Works](/docs/concepts/identity).

Before anything else, save the encryption key offline:

```sh
microk8s kubectl -n proxcloud get secret proxcloud-api \
  -o jsonpath='{.data.encryption-key}' | base64 -d
```

From here, move on to [Installing on Kubernetes](/docs/self-hosting/install) for the production topics — external databases, scaling, [backups](/docs/self-hosting/backup). The values file and commands are the same; only the cluster differs.
