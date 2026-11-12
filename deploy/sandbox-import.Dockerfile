# syntax=docker/dockerfile:1
# Trusted no-build OCI importer. Operator must pin the resulting digest in
# sandbox-system/sandbox-import-worker; this file does not enable or publish it.
FROM golang:1.27.4-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/sandbox-import ./cmd/sandbox-import
COPY internal/sandboximport ./internal/sandboximport
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/sandbox-import ./cmd/sandbox-import
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/sandbox-import /sandbox-import
USER 65532:65532
ENTRYPOINT ["/sandbox-import"]
