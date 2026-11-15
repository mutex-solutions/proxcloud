# proxcloud-clusterctl: clusterctl, kubectl and the Cluster API provider manifests of the platform BOM, baked in as a
# clusterctl local repository, so the clusterctl Job of the proxcloud-platform chart makes no GitHub call at runtime.
# Built, scanned and published only by .github/workflows/clusterctl.yml, on a proxcloud-clusterctl-v* tag.
#
# Versions: those of installer/playbooks/group_vars/all/versions.yml (capi, capmox, capi_ipam, capi_kamaji, caaph),
# kubectl that of pc_defaults.services.k8s_version; deploy/tests/test_clusterctl_image.py keeps them equal. Every
# download is checked against the sha256 written here (computed from the upstream release assets and source
# archives, which publish no checksum file). A version change means new checksums, new patches, a new image version
# below and a new tag.
#
# clusterctl and kubectl are built here from the upstream source of their release tags, not downloaded: the upstream
# release binaries carry golang.org/x/net and golang.org/x/text versions with fixed HIGH CVEs, which the Trivy gate
# refuses. patches/ changes only go.mod and go.sum (x/net, x/text and what they force); no Go source is changed.
# -mod=readonly makes the build fail instead of editing go.mod or go.sum, so every module download is verified
# against the go.sum lines of upstream and of the patch. GitHub generates the source archives on request: if their
# bytes ever change, the build fails on the sha256 and the pin is recomputed from the tag's commit.

# golang:1.26.10-bookworm, multi-platform index digest. Go 1.26.10 is the toolchain upstream builds both releases
# with (cluster-api Makefile GO_VERSION, kubernetes .go-version); GOTOOLCHAIN=local keeps go from fetching another.
FROM golang@sha256:5de7c99eb78ca5e515a4c340235d0955ec5964a5071c492c15503d7c267da473 AS go
ENV CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOTOOLCHAIN=local GOWORK=off GOFLAGS="-mod=readonly -trimpath" \
    GOPROXY=https://proxy.golang.org GOSUMDB=sum.golang.org GONOSUMDB= GONOSUMCHECK= GONOPROXY= GOPRIVATE= GOINSECURE= \
    X_NET=v0.62.0 X_TEXT=v0.43.0

# Version fields as upstream's hack/scripts/release/version.sh sets them, with the tag's commit and commit date, except
# gitTreeState: "dirty", because go.mod and go.sum differ from that commit.
FROM go AS clusterctl
ADD --checksum=sha256:0d22b58ffeb5b6bddb9085a1b35c059e2109507e8cfddec7b54b1fad87f49849 \
    https://github.com/kubernetes-sigs/cluster-api/archive/refs/tags/v1.14.4.tar.gz /src/cluster-api.tar.gz
COPY patches/cluster-api-v1.14.4.patch /src/
RUN set -eu; mkdir /src/cluster-api; cd /src/cluster-api; \
    tar -xzf ../cluster-api.tar.gz --strip-components=1; git apply ../cluster-api-v1.14.4.patch; \
    v=sigs.k8s.io/cluster-api/version commit=4356c154f4a76b8939748ef7ebc2d0793ffe47e8; \
    go build -o /out/clusterctl -ldflags "-s -w -X $v.gitVersion=v1.14.4 -X $v.gitMajor=1 -X $v.gitMinor=14 \
      -X $v.gitCommit=$commit -X $v.gitReleaseCommit=$commit -X $v.gitTreeState=dirty \
      -X $v.buildDate=2026-11-03T08:29:50Z" ./cmd/clusterctl; \
    go version -m /out/clusterctl > /out/clusterctl.modules; \
    grep -q "golang.org/x/net[[:space:]]$X_NET[[:space:]]" /out/clusterctl.modules; \
    grep -q "golang.org/x/text[[:space:]]$X_TEXT[[:space:]]" /out/clusterctl.modules

# Only ./cmd/kubectl, from the root module: its replace directives resolve k8s.io/* to staging/ without the workspace,
# and -mod=readonly ignores vendor/ (which holds the unpatched modules). Tags and version fields as upstream's
# hack/lib/golang.sh and hack/lib/version.sh set them for a release binary, gitTreeState as above.
FROM go AS kubectl
ADD --checksum=sha256:e58917e90074bf61fa5b5e2a2bd5555880f856367abe60f32a4fe01b6ac43ebd \
    https://github.com/kubernetes/kubernetes/archive/refs/tags/v1.35.10.tar.gz /src/kubernetes.tar.gz
COPY patches/kubernetes-v1.35.10.patch /src/
RUN set -eu; mkdir /src/kubernetes; cd /src/kubernetes; \
    tar -xzf ../kubernetes.tar.gz --strip-components=1; git apply ../kubernetes-v1.35.10.patch; \
    x=""; for v in k8s.io/client-go/pkg/version k8s.io/component-base/version; do \
      x="$x -X $v.gitVersion=v1.35.10 -X $v.gitMajor=1 -X $v.gitMinor=35 -X $v.gitTreeState=dirty \
        -X $v.gitCommit=ae6fece6fea92ecfcccd13af7d5f2ea8cb9b33fe -X $v.buildDate=2026-10-27T12:02:52Z"; \
    done; \
    go build -o /out/kubectl -tags selinux,notest,grpcnotrace -ldflags "all=-s -w $x" ./cmd/kubectl; \
    go version -m /out/kubectl > /out/kubectl.modules; \
    grep -q "golang.org/x/net[[:space:]]$X_NET[[:space:]]" /out/kubectl.modules; \
    grep -q "golang.org/x/text[[:space:]]$X_TEXT[[:space:]]" /out/kubectl.modules

# alpine:3.24.4 (the runtime base of deploy/Dockerfile), multi-platform index digest. The downloads land in a stage of
# their own: ADD writes a downloaded file with mode 600 (and --chmod would apply to the directories it creates too),
# so the modes are set once there and the final stage copies a readable tree in one layer.
FROM alpine@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf AS fetch

COPY --from=clusterctl /out/clusterctl /out/usr/local/bin/clusterctl
COPY --from=kubectl /out/kubectl /out/usr/local/bin/kubectl

# The local repository, in clusterctl's layout {base}/{provider label}/{version}/{file}; clusterctl.yaml points every
# provider at it. Cluster API core, kubeadm bootstrap and kubeadm control plane share one metadata.yaml.
ADD --checksum=sha256:0030264eb0a0d90ebacc9da8f6bbb717bee32e45ffadc4a74784fb5ad7b9bb0b \
    https://github.com/kubernetes-sigs/cluster-api/releases/download/v1.14.4/core-components.yaml /out/opt/clusterctl/repository/cluster-api/v1.14.4/
ADD --checksum=sha256:c470906f551ac3e3e9aedc9a3e733b5a5994428693df43e95f65ea67c035f9fe \
    https://github.com/kubernetes-sigs/cluster-api/releases/download/v1.14.4/metadata.yaml /out/opt/clusterctl/repository/cluster-api/v1.14.4/
ADD --checksum=sha256:cf805775f24d197581b06e938bcd0eb9a9ed49d675f67e0f689ce7b71e5e397c \
    https://github.com/kubernetes-sigs/cluster-api/releases/download/v1.14.4/bootstrap-components.yaml /out/opt/clusterctl/repository/bootstrap-kubeadm/v1.14.4/
ADD --checksum=sha256:c470906f551ac3e3e9aedc9a3e733b5a5994428693df43e95f65ea67c035f9fe \
    https://github.com/kubernetes-sigs/cluster-api/releases/download/v1.14.4/metadata.yaml /out/opt/clusterctl/repository/bootstrap-kubeadm/v1.14.4/
ADD --checksum=sha256:7fecb41843dcff7e05ab539dbe497c050a9aa7c888d4fb68be12d3c1805be438 \
    https://github.com/kubernetes-sigs/cluster-api/releases/download/v1.14.4/control-plane-components.yaml /out/opt/clusterctl/repository/control-plane-kubeadm/v1.14.4/
ADD --checksum=sha256:c470906f551ac3e3e9aedc9a3e733b5a5994428693df43e95f65ea67c035f9fe \
    https://github.com/kubernetes-sigs/cluster-api/releases/download/v1.14.4/metadata.yaml /out/opt/clusterctl/repository/control-plane-kubeadm/v1.14.4/
ADD --checksum=sha256:b038237c4dfe0890e4fc90cc905ab33247b0680bc96e103f6d42b9553d63aa8d \
    https://github.com/clastix/cluster-api-control-plane-provider-kamaji/releases/download/v0.22.0/control-plane-components.yaml /out/opt/clusterctl/repository/control-plane-kamaji/v0.22.0/
ADD --checksum=sha256:281b1b1922dff2bea51db25373e28a9f9d5bb35d8393b22c0031a412057b7061 \
    https://github.com/clastix/cluster-api-control-plane-provider-kamaji/releases/download/v0.22.0/metadata.yaml /out/opt/clusterctl/repository/control-plane-kamaji/v0.22.0/
ADD --checksum=sha256:6817322435b42cfda07a8f94006bacb709afb83445ce24545af253c7904755d8 \
    https://github.com/ionos-cloud/cluster-api-provider-proxmox/releases/download/v0.9.2/infrastructure-components.yaml /out/opt/clusterctl/repository/infrastructure-proxmox/v0.9.2/
ADD --checksum=sha256:df2916a79503ac053f6a9866a8de025e780e24d6d472f00f59f84898b5a1c572 \
    https://github.com/ionos-cloud/cluster-api-provider-proxmox/releases/download/v0.9.2/metadata.yaml /out/opt/clusterctl/repository/infrastructure-proxmox/v0.9.2/
ADD --checksum=sha256:8a0656a39d9e85ee07219702254669b7e543d09957e86315f3d20b4bf6aebfa5 \
    https://github.com/kubernetes-sigs/cluster-api-ipam-provider-in-cluster/releases/download/v1.1.1/ipam-components.yaml /out/opt/clusterctl/repository/ipam-in-cluster/v1.1.1/
ADD --checksum=sha256:eb280ab315f478324973a7b65739a655d57e3c22a1356efd9f5127534d3502b7 \
    https://github.com/kubernetes-sigs/cluster-api-ipam-provider-in-cluster/releases/download/v1.1.1/metadata.yaml /out/opt/clusterctl/repository/ipam-in-cluster/v1.1.1/
ADD --checksum=sha256:a70f4eb77eac626231daca1e2a046b4b069bb84320efa327cc8c56a9c4ca03e6 \
    https://github.com/kubernetes-sigs/cluster-api-addon-provider-helm/releases/download/v0.6.4/addon-components.yaml /out/opt/clusterctl/repository/addon-helm/v0.6.4/
ADD --checksum=sha256:d50a44ee0db2f0368699d15ff011b9b5a61d9969483de1c4f6e4e6046390530f \
    https://github.com/kubernetes-sigs/cluster-api-addon-provider-helm/releases/download/v0.6.4/metadata.yaml /out/opt/clusterctl/repository/addon-helm/v0.6.4/

COPY clusterctl.yaml /out/opt/clusterctl/clusterctl.yaml
COPY clusterctl-init.sh /out/usr/local/bin/clusterctl-init
RUN chmod -R u=rwX,go=rX /out && chmod 755 /out/usr/local/bin/*

FROM alpine@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf
COPY --from=fetch /out/ /

# Any non-root uid works: clusterctl and kubectl write only below HOME, and the version check would call GitHub.
USER 65532:65532
ENV HOME=/tmp CLUSTERCTL_DISABLE_VERSIONCHECK=true

# Build-time proof that every provider resolves from the baked repository with no network at all.
RUN --network=none set -eu; \
    test "$(clusterctl version -o short)" = v1.14.4; kubectl version --client | grep -Fx "Client Version: v1.35.10"; \
    for provider in --core=cluster-api:v1.14.4 --bootstrap=kubeadm:v1.14.4 --control-plane=kubeadm:v1.14.4 \
        --control-plane=kamaji:v0.22.0 --infrastructure=proxmox:v0.9.2 --ipam=in-cluster:v1.1.1 --addon=helm:v0.6.4; do \
      clusterctl generate provider --config /opt/clusterctl/clusterctl.yaml "$provider" --describe; \
    done; \
    rm -rf /tmp/.[!.]* /tmp/*

ENTRYPOINT ["/usr/local/bin/clusterctl-init"]

ARG REVISION=unknown
LABEL org.opencontainers.image.source="https://github.com/mutex-solutions/proxcloud" \
      org.opencontainers.image.description="ProxCloud clusterctl: Cluster API providers of the platform BOM, installed from a baked local repository" \
      org.opencontainers.image.version="v1.14.4-1" \
      org.opencontainers.image.revision="$REVISION"
