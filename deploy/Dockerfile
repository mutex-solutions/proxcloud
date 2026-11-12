# syntax=docker/dockerfile:1
# proxcloud-api (Go). The runtime is debian-slim (NOT distroless) on purpose: the
# image ships `psql` (postgresql-client) so the config-as-code seed (deploy/seed)
# can run from this pod against PROXCLOUD_DB_URL, alongside the Go binary.

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

# Debian 13 carries maintained fixes absent from the older runtime base. This
# changes the API container only, not database/server major versions or data.
FROM debian:trixie-slim
# ca-certificates for outbound TLS; postgresql-client so the config-as-code seed
# (deploy/seed) can `psql` from this pod against PROXCLOUD_DB_URL; bash for exec/debug.
RUN apt-get update \
 && apt-get upgrade -y \
 && apt-get install -y --no-install-recommends bash ca-certificates postgresql-client \
 && rm -rf /var/lib/apt/lists/*

# api config mount point.
RUN mkdir -p /opt/proxcloud/api

COPY --from=build /out/proxcloud-api /usr/local/bin/proxcloud-api

# Non-root.
RUN useradd -r -u 10001 -g root proxcloud
USER 10001

EXPOSE 8080 8081
ENTRYPOINT ["/usr/local/bin/proxcloud-api"]
