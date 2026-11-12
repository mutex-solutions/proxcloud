# syntax=docker/dockerfile:1
# proxcloud-api (Go). The Alpine runtime is NOT distroless on purpose: it ships
# bash and PostgreSQL 18's `psql` CLI so the config-as-code seed (deploy/seed)
# can run from this pod against PROXCLOUD_DB_URL, alongside the static Go binary.

FROM golang:1.27.4-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
# Copy ONLY the Go source the binary needs (not the whole repo): no values overlays, seeds,
# docs, or tests reach the build context/image.
COPY cmd ./cmd
COPY internal ./internal
COPY pkg ./pkg
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/proxcloud-api ./cmd/api

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

# Non-root.
RUN adduser -S -D -H -u 10001 -G root proxcloud
USER 10001

EXPOSE 8080 8081
ENTRYPOINT ["/usr/local/bin/proxcloud-api"]
