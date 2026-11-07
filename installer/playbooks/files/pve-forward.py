#!/usr/bin/env python3
"""Retire svc/pve-api on the services cluster once no tenant proxmox-csi controller reaches PVE through it.
Prints "absent", "removed", or "kept, still used by:" and one object per line. Env: KUBECONFIG."""
import base64
import json
import subprocess
import sys

NS = "proxcloud-system"


def users(secrets, apps):
    """Objects that show a cluster still runs its tenant-side controller on its own token: a resource-set config
    holding a token, or a managed Application with StorageClasses but no gateway. The second covers a migration
    whose config Secret was rewritten but whose Application patch failed."""
    out = []
    for s in secrets:
        m = s["metadata"]
        if m["name"].endswith("-csi-config") and b"token_secret" in base64.b64decode((s.get("data") or {}).get("csi.yaml", "")):
            out.append(m["namespace"] + "/" + m["name"])
    for a in apps:
        m = a["metadata"]
        if (m.get("labels") or {}).get("app.kubernetes.io/managed-by") != "proxcloud":
            continue
        values = (((a.get("spec") or {}).get("source") or {}).get("helm") or {}).get("valuesObject") or {}
        csi = (values.get("proxmox") or {}).get("csi") or {}
        if csi.get("storageClasses") and not (csi.get("gateway") or {}).get("secretName"):
            out.append(m["namespace"] + "/" + m["name"])
    return out


def kubectl(*args):
    return subprocess.run(["kubectl", *args], check=True, capture_output=True, text=True).stdout


def main():
    if subprocess.run(["kubectl", "-n", NS, "get", "svc", "pve-api"], capture_output=True).returncode != 0:
        print("absent")
        return
    left = users(json.loads(kubectl("get", "secret", "-A", "-o", "json"))["items"],
                 json.loads(kubectl("get", "applications.argoproj.io", "-A", "-o", "json"))["items"])
    if left:
        print("kept, still used by:")
        print("\n".join(left))
        return
    kubectl("-n", NS, "delete", "svc/pve-api", "endpointslice/pve-api", "--ignore-not-found")
    print("removed")


if __name__ == "__main__":
    try:
        main()
    except subprocess.CalledProcessError as e:
        sys.exit(f"kubectl {' '.join(e.cmd[1:])}: {e.stderr.strip()}")
