# proxcloud-clusterctl: clusterctl, kubectl and the Cluster API provider manifests of the platform BOM, baked in as a
# clusterctl local repository, so the clusterctl Job of the proxcloud-platform chart makes no GitHub call at runtime.
# Built, scanned and published only by .github/workflows/clusterctl.yml, on a proxcloud-clusterctl-v* tag.
#
# Versions: those of installer/playbooks/group_vars/all/versions.yml (capi, capmox, capi_ipam, capi_kamaji, caaph),
# kubectl that of pc_defaults.services.k8s_version; deploy/tests/test_clusterctl_image.py keeps them equal. Every
# download is checked against the sha256 written here (computed from the upstream release assets, which publish no
# checksum file except for kubectl). A version change means new checksums, a new image version below and a new tag.

# alpine:3.24.4 (the runtime base of deploy/Dockerfile), multi-platform index digest. The downloads land in a stage of
# their own: ADD writes a downloaded file with mode 600 (and --chmod would apply to the directories it creates too),
# so the modes are set once there and the final stage copies a readable tree in one layer.
FROM alpine@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf AS fetch

ADD --checksum=sha256:c7301a8515ed329562b06c958463051dd55d9649fac591393f5df54a868fa79b \
    https://github.com/kubernetes-sigs/cluster-api/releases/download/v1.14.4/clusterctl-linux-amd64 /out/usr/local/bin/clusterctl
ADD --checksum=sha256:c49a28f1f4efc4d475ed05f47f2ce8e563bf7bef69ed77fe1b924ea6d83b6327 \
    https://dl.k8s.io/release/v1.35.10/bin/linux/amd64/kubectl /out/usr/local/bin/kubectl

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
    clusterctl version -o short; kubectl version --client; \
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
