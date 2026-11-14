"""Derive the per-run Kubernetes k8s-review.json from a gated inventory and a committed reviewed baseline.

    cd images && python3 -m vm_export.review derive --inventory inventory.json \
        --baseline vm_export/baselines/ubuntu-2604-k8s.json --kubernetes-version 1.37.2 \
        --source-raw-sha256 <64 hex> --out k8s-review.json
    cd images && python3 -m vm_export.review project --inventory inventory.json   # baseline "inventory" draft

The inventory must equal the baseline's reviewed facts EXCEPT the allowlisted version fields:
kubeadm/kubelet/kubectl == the requested K8S_VERSIONS member (deb revision -1.1) and kubernetes-cni
from the baseline's explicit allowlist. Any other difference refuses with the differing field paths
only (never values). package_policy_sha256 = sha256 of the compact sorted JSON of the package policy.
"""
import argparse
import copy
import hashlib
import json
from pathlib import Path
import re
import sys

from vm_sanitize import K8S_DEB_RELEASE, K8S_VERSIONS

K8S_PACKAGES = ("kubeadm", "kubelet", "kubectl")
# Inventory paths that may differ between reviewed builds; each is checked separately below.
VERSION_FIELDS = (("facts", "k8s", "version"), ("facts", "k8s", "kubernetes_packages"),
                  *(("packages", name) for name in K8S_PACKAGES + ("kubernetes-cni",)))
BASELINE_KEYS = {"kind", "os", "basis", "kubernetes_cni_versions", "review", "package_policy", "inventory"}
REVIEW_KEYS = {"bios", "cloud_base_config_reviewed", "cloud_default_user", "kubelet_active", "network_backend",
               "retained_package_policy_reviewed", "runtime_version", "reviewed_dropins", "sudoers_sha256",
               "sudoers_readme_sha256"}
POLICY_KEYS = {"containerd", "runc", "image_builder"}


class ReviewRefused(ValueError):
    def __init__(self, fields):
        self.fields = sorted(fields)
        super().__init__("review refused: " + ", ".join(self.fields))


def compact(obj):
    return json.dumps(obj, sort_keys=True, separators=(",", ":"))


def project(inventory):
    """The inventory without the allowlisted version fields."""
    out = copy.deepcopy(inventory)
    for path in VERSION_FIELDS:
        node = out
        for key in path[:-1]:
            node = node.get(key, {}) if type(node) is dict else {}
        if type(node) is dict:
            node.pop(path[-1], None)
    return out


def diff(a, b, path=""):
    """Field paths where a and b differ (dict keys recursively, lists/scalars compared whole)."""
    if type(a) is dict and type(b) is dict:
        found = []
        for key in sorted(set(a) | set(b)):
            sub = path + "." + key if path else key
            found += [sub] if (key in a) != (key in b) else diff(a[key], b[key], sub)
        return found
    return [] if (type(a), a) == (type(b), b) else [path or "<root>"]


def package_policy(inventory, baseline):
    packages = inventory["packages"]
    return {"kubernetes_packages": {name: packages[name] for name in K8S_PACKAGES + ("kubernetes-cni",)},
            "held": sorted(name for name, word in inventory["package_status_word"].items() if word == "hold"),
            "containerd": baseline["package_policy"]["containerd"], "runc": baseline["package_policy"]["runc"],
            "containerd_config_sha256": inventory["runtime"]["containerd_config_sha256"],
            "containerd_enabled": inventory["runtime"]["containerd_unit_enabled"],
            "crictl_present": inventory["runtime"]["crictl_present"],
            "image_builder": baseline["package_policy"]["image_builder"]}


def derive(inventory, baseline, k8s_version, source_raw_sha256):
    if not (type(baseline) is dict and set(baseline) == BASELINE_KEYS and baseline["kind"] == "K8sReviewBaseline"
            and set(baseline["review"]) == REVIEW_KEYS and set(baseline["package_policy"]) == POLICY_KEYS):
        raise ReviewRefused(["baseline"])
    refused = []
    if k8s_version not in K8S_VERSIONS:
        refused.append("kubernetes_version")
    if re.fullmatch(r"[0-9a-f]{64}", str(source_raw_sha256)) is None:
        refused.append("source_raw_sha256")
    if type(inventory) is not dict or inventory.get("policy_result") != "PASS_WITH_PLACEHOLDER_REVIEW":
        raise ReviewRefused(refused + ["policy_result"])
    try:
        k8s = inventory["facts"]["k8s"]
        if k8s["version"] != k8s_version:
            refused.append("facts.k8s.version")
        if k8s["kubernetes_packages"] != {name: k8s_version for name in K8S_PACKAGES}:
            refused.append("facts.k8s.kubernetes_packages")
        packages = inventory["packages"]
        refused += ["packages." + name for name in K8S_PACKAGES
                    if packages.get(name) != k8s_version + "-" + K8S_DEB_RELEASE]
        if packages.get("kubernetes-cni") not in baseline["kubernetes_cni_versions"]:
            refused.append("packages.kubernetes-cni")
    except (KeyError, TypeError):
        raise ReviewRefused(refused + ["inventory"]) from None
    refused += diff(baseline["inventory"], project(inventory))
    # The reviewed decisions must cover exactly the observed drop-ins and sudo bytes (baseline self-consistency).
    reviewed = baseline["review"]
    if ({(n, d) for n, d, _ in reviewed["reviewed_dropins"]} !=
            {(d["name"], d["sha256"]) for d in inventory["facts"]["cloud_dropins"]}):
        refused.append("review.reviewed_dropins")
    if inventory["sudoers"] != {k: reviewed[k] for k in ("sudoers_sha256", "sudoers_readme_sha256")}:
        refused.append("review.sudoers")
    if refused:
        raise ReviewRefused(refused)
    policy = package_policy(inventory, baseline)
    review = dict(baseline["review"], kubernetes_version=k8s_version, source_raw_sha256=source_raw_sha256,
                  source_hash_verified=True, source_stopped=True,
                  package_policy_sha256=hashlib.sha256(compact(policy).encode()).hexdigest())
    return review, policy


def load(path):
    def unique(pairs):
        if len(dict(pairs)) != len(pairs):
            raise ValueError("duplicate key")
        return dict(pairs)
    return json.loads(Path(path).read_bytes().decode("ascii"), object_pairs_hook=unique)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)
    d = sub.add_parser("derive")
    d.add_argument("--baseline", required=True)
    d.add_argument("--kubernetes-version", required=True)
    d.add_argument("--source-raw-sha256", required=True)
    d.add_argument("--out", required=True)
    p = sub.add_parser("project")
    for s in (d, p):
        s.add_argument("--inventory", required=True)
    a = parser.parse_args(argv)
    try:
        inventory = load(a.inventory)
        if a.command == "project":
            print(json.dumps(project(inventory), indent=1, sort_keys=True))
            return 0
        review, policy = derive(inventory, load(a.baseline), a.kubernetes_version, a.source_raw_sha256)
        with open(a.out, "x", encoding="ascii") as f:
            f.write(compact(review))  # Exactly the bytes the sanitizer capsule reads; no trailing newline.
        print(json.dumps({"package_policy": policy, "package_policy_sha256": review["package_policy_sha256"]},
                         sort_keys=True))
        return 0
    except ReviewRefused as exc:
        print(str(exc), file=sys.stderr)
        return 1
    except (OSError, ValueError, UnicodeError) as exc:
        print("review refused: unreadable input (" + type(exc).__name__ + ")", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
