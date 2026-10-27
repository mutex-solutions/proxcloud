# Trusting a Custom CA

When your OpenStack endpoints (Keystone, Nova, Neutron, …) or your identity provider present certificates signed by a private CA, the ProxCloud API has to be told to trust that CA. Otherwise every HTTPS call it makes — Keystone authentication, resource sync, fetching the JWKS from your OIDC issuer to validate tokens — fails TLS verification.

There are really two separate trust problems, each solved in its own place:

1. **Outbound** — `proxcloud-api` calling Keystone / OpenStack / IdP endpoints. Fixed by mounting the CA into the API pod.
2. **Inbound** — browsers reaching the ProxCloud ingress. Fixed with a TLS certificate on the ingress (from your private CA or a public one).

## Trusting a private CA in the API

Create a secret in the ProxCloud namespace holding the CA certificate (PEM format; concatenate multiple CAs into a single file if you have a chain):

```sh
kubectl -n proxcloud create secret generic ca-root-secret \
  --from-file=ca.crt=/path/to/ca.crt
```

If the certificate came from cert-manager inside the cluster, a suitable secret with `ca.crt` may already exist — reuse it.

Then mount that secret into the API container as a directory and add it to the certificate search path with `SSL_CERT_DIR`, using the chart's `api` extension hooks:

```yaml
api:
  extraVolumes:
    - name: ca-cert-volume
      secret:
        secretName: ca-root-secret
  extraVolumeMounts:
    - name: ca-cert-volume
      mountPath: /etc/ssl/extra-certs
      readOnly: true
  extraEnv:
    - name: SSL_CERT_DIR
      value: /etc/ssl/certs:/etc/ssl/extra-certs
```

The mount surfaces each key in the secret (here `ca.crt`) as a file under `/etc/ssl/extra-certs`. `SSL_CERT_DIR` is the colon-separated list of directories the API's Go TLS stack scans for trusted certificates — keep the image's system directory `/etc/ssl/certs` first so the bundled public CAs still load, then your own directory. Every PEM found in either directory is trusted, so publicly-signed endpoints keep working right alongside your private ones. (No custom key is baked into the image; the runtime is `debian-slim` with `ca-certificates`, whose system trust store lives at `/etc/ssl/certs`.)

Apply with `helm upgrade … -f values.yaml`. The pod restarts with the CA mounted.

To verify, check the API log after the restart — Keystone connectivity errors of the `x509: certificate signed by unknown authority` variety vanish once the CA is trusted. You can confirm the file is in place from inside the pod:

```sh
kubectl -n proxcloud exec deploy/proxcloud-api -- ls -l /etc/ssl/extra-certs
```

## TLS on the ingress with a private CA

If your users' browsers should also see certificates from the private CA — common in air-gapped or lab setups:

- With **cert-manager**, create a `ClusterIssuer` of type `ca` backed by your CA key pair, reference it in each component's ingress annotations (`api.ingress.annotations` / `ui.ingress.annotations` / `admin.ingress.annotations`, e.g. `cert-manager.io/cluster-issuer: <issuer>`), and set the per-component `*.ingress.tls` list.
- Without cert-manager, create the TLS secret yourself and reference it by `secretName` in each component's `*.ingress.tls`.

Remember that clients — browsers, but also any machine running the OpenStack CLI against a federated Keystone — need the CA in their own trust stores.

## Where a custom CA usually comes up

- Keystone / OpenStack API endpoints on a private network with internally-issued certs.
- An external Keycloak or OIDC provider on an internal domain — without trust, JWT validation fails because the JWKS endpoint can't be fetched.
- Internal SMTP or webhook targets reached over HTTPS.
