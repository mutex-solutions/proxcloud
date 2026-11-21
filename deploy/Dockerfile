# syntax=docker/dockerfile:1
# proxcloud-api (Go). The Alpine runtime is NOT distroless on purpose: it ships
# bash and PostgreSQL 18's `psql` CLI so the config-as-code seed (deploy/seed)
# can run from this pod against PROXCLOUD_DB_URL, alongside the static Go binary.

FROM golang:1.27.4-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
# Only the Go source the binary needs. deploy/seed is seed.go and the JSON it embeds (.dockerignore keeps the rest out);
# deploy/embed.go embeds the platform BOM in deploy/platform and the two argocd-health.yaml files.
COPY cmd ./cmd
COPY internal ./internal
COPY pkg ./pkg
COPY deploy/seed ./deploy/seed
COPY deploy/embed.go ./deploy/
COPY deploy/platform ./deploy/platform
COPY deploy/mgmt-cluster/argocd-health.yaml ./deploy/mgmt-cluster/
COPY deploy/dbaas-cluster/argocd-health.yaml ./deploy/dbaas-cluster/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/proxcloud-api ./cmd/api
# The load balancer controller of the managed clusters (cmd/lb-controller): a second binary of this image, so it has no
# image, tag or workflow of its own and runs the portal's version. The cluster chart starts it by its command.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/lb-controller ./cmd/lb-controller

# CGO_ENABLED=0 keeps the Go binary independent of the runtime's libc. This
# changes the API container only, not database/server major versions or data.
FROM alpine:3.24.4
# ca-certificates for outbound TLS; bash and psql for the existing seed SQL.
# This psql client supports password/TLS; Kerberos/GSSAPI seeding is not provided
# by this client. For that authentication, use a suitable psql in the DB pod.
# The application's static Go/pgx database authentication is unchanged.
RUN apk upgrade --no-cache \
 && apk add --no-cache bash ca-certificates postgresql18-client

# api config mount point.
RUN mkdir -p /opt/proxcloud/api

COPY --from=build /out/proxcloud-api /usr/local/bin/proxcloud-api
COPY --from=build /out/lb-controller /usr/local/bin/lb-controller

# Non-root. /var/lib/proxcloud/gateway is where the Compose stack mounts the gateway volume, in which the API makes its own
# gateway certificate (--cluster-gateway): a new named volume takes the owner of the image's directory.
RUN adduser -S -D -H -u 10001 -G root proxcloud \
 && install -d -o 10001 -g 0 -m 700 /var/lib/proxcloud/gateway
USER 10001

EXPOSE 8080 8081
ENTRYPOINT ["/usr/local/bin/proxcloud-api"]
