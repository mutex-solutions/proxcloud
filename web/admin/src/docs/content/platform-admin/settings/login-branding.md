# Branding the Login Pages

The login and registration screens your clients see are served by the bundled Keycloak instance, not by the portal itself. Branding them therefore means installing a custom Keycloak theme. You bake the theme into a small Docker image and copy it into the Keycloak pod at startup, using an init container declared in the Helm values.

## Getting a theme

In rising order of effort:

- Take an open-source theme like Keywind and restyle it.
- Use a theme generator or marketplace (Keycloakify-based tooling, for instance).
- Write your own from scratch, following the Keycloak theming docs.

## Packaging the theme as an image

A multi-stage Dockerfile keeps the final image tiny — all it has to carry is the compiled theme files under `/theme`:

```dockerfile
# Stage 1: fetch the theme source
FROM alpine/git AS source
WORKDIR /src
RUN git clone https://github.com/your-org/your-keycloak-theme.git .

# Stage 2: build it
FROM node:18 AS builder
WORKDIR /build
COPY --from=source /src .
RUN npm install && npm run build

# Stage 3: final image = just the theme files
FROM alpine
COPY --from=builder /build/dist /theme
```

Push the image to a registry your cluster can pull from.

## Mounting the theme into the pod

In your ProxCloud Helm values, add an init container that copies the theme into a shared `emptyDir` volume, and mount that volume at Keycloak's theme directory:

```yaml
keycloakx:
  extraInitContainers: |
    - name: theme-provider
      image: registry.example.com/proxcloud-keycloak-theme:latest
      imagePullPolicy: Always
      command: ["sh", "-c", "cp -R /theme/* /keycloak-theme/"]
      volumeMounts:
        - name: custom-theme
          mountPath: /keycloak-theme
  extraVolumeMounts: |
    - name: custom-theme
      mountPath: /opt/keycloak/themes/proxcloud
  extraVolumes: |
    - name: custom-theme
      emptyDir: {}
```

Apply the change:

```bash
helm --namespace proxcloud upgrade proxcloud deploy/charts/proxcloud -f values.yaml
```

Since the volume is an `emptyDir`, the copy runs on every pod start — bump the image tag (or leave `imagePullPolicy: Always` in place) to roll out theme updates.

## Selecting the theme in the realm

Once the Keycloak pod restarts, the new theme shows up in Keycloak's theme dropdowns. In the Keycloak admin console, open your realm, go to **Realm settings > Themes**, and pick the theme (e.g. `proxcloud`) as the login theme. Save, then load the client portal login page to confirm.

<!-- screenshot: /docs-img/customize-keycloak-theme-realm.png — Keycloak admin console: Realm settings > Themes with the custom proxcloud theme selected as login theme -->

