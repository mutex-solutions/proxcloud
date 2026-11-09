# syntax=docker/dockerfile:1
# proxcloud-sandbox-runtime: carries the two static binaries every sandbox pod needs
# (E2B's envd and our sandbox-init). Run as an init container on each sandbox-cluster node
# to install them into /opt/proxcloud-sandbox/bin (pods mount that dir read-only at /.proxcloud).

FROM golang:1.26 AS envd
ARG E2B_INFRA_REF=e85b9f5f6de6c92b002f376b4f91b1652d3c942e
RUN git clone https://github.com/e2b-dev/infra /infra \
 && git -C /infra checkout "${E2B_INFRA_REF}"
# packages/envd has `replace ../shared`, so build inside the clone.
WORKDIR /infra/packages/envd
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/envd .

FROM golang:1.25-bookworm AS init
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY pkg ./pkg
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/sandbox-init ./cmd/sandbox-init \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/sandbox-netguard ./cmd/sandbox-netguard

FROM busybox:1.37
ARG E2B_INFRA_REF=e85b9f5f6de6c92b002f376b4f91b1652d3c942e
COPY --from=envd /out/envd /proxcloud-sandbox/bin/envd
COPY --from=init /out/sandbox-init /proxcloud-sandbox/bin/sandbox-init
COPY --from=init /out/sandbox-netguard /proxcloud-sandbox/bin/sandbox-netguard
# Operator capability evidence; node labels are granted only after canary verification.
RUN printf 'v1\n' > /proxcloud-sandbox/bin/prepull-runtime
RUN <<EOF
cat > /proxcloud-sandbox/THIRD_PARTY.md <<NOTICE
# Third-party software

## envd (bin/envd)

envd is the in-sandbox agent from E2B, https://github.com/e2b-dev/infra (packages/envd),
licensed under the Apache License, Version 2.0 (http://www.apache.org/licenses/LICENSE-2.0).
Copyright E2B. Built unmodified from commit ${E2B_INFRA_REF}.
Source: https://github.com/e2b-dev/infra/tree/${E2B_INFRA_REF}/packages/envd
NOTICE
EOF
COPY --chmod=0755 <<'EOF' /install.sh
#!/bin/sh
# Install envd + sandbox-init into $DEST atomically (tmp file, then mv).
set -eu
DEST="${DEST:-/host/opt/proxcloud-sandbox/bin}"
mkdir -p "$DEST"
for f in envd sandbox-init sandbox-netguard prepull-runtime; do
  cp "/proxcloud-sandbox/bin/$f" "$DEST/.$f.tmp"
  chmod 0755 "$DEST/.$f.tmp"
  mv -f "$DEST/.$f.tmp" "$DEST/$f"
done
echo "installed envd and sandbox-init into $DEST"
EOF
ENTRYPOINT ["/install.sh"]
