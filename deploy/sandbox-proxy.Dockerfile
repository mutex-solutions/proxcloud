# syntax=docker/dockerfile:1
# proxcloud-sandbox-proxy: routes https://<port>-<sandboxID>.<domain> to sandbox pods (sandbox cluster, sandbox-system),
# and its cert-sync mode copies the wildcard certificate from the services cluster (docs/managed-sandbox.md §4).

FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY pkg ./pkg
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/sandbox-proxy ./cmd/sandbox-proxy

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/sandbox-proxy /sandbox-proxy
EXPOSE 8443 8081
ENTRYPOINT ["/sandbox-proxy"]
