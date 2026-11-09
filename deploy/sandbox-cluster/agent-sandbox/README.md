# agent-sandbox (vendored)

`sandbox-with-extensions.yaml` is the release asset of
[kubernetes-sigs/agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox) **v1.0.9**, unchanged below a
three-line header: the `Sandbox` CRD (`agents.x-k8s.io`), the extension CRDs (`extensions.agents.x-k8s.io`) and the
controller in `agent-sandbox-system`.

- Upstream: `https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.9/sandbox-with-extensions.yaml`
- sha256 of the upstream file (this file without its first three lines):
  `4bf9ee5558e9a663f91cfa5963e96f2591bfaa1dcf79f03bfab65bc5d65cc699`
- Licence: Apache License 2.0, copyright The Kubernetes Authors; the text is in `LICENSE` next to this file.

Check: `tail -n +4 sandbox-with-extensions.yaml | sha256sum`.

Upgrade: download the new release asset, put the header back on top with the new version and checksum, update this
file, and run the sandbox drill again (docs/managed-sandbox.md). Apply it with `kubectl apply --server-side`: the
CRDs are too large for the client-side `last-applied-configuration` annotation.

The file contains the full Kubernetes pod template schema, which names in-tree volume plugins of other platforms;
that is why this directory is excluded from the repository's previous-backend term gate.
