#!/usr/bin/env python3
"""Offline check of the sandbox cluster stage 55 writes (templates/sandbox-cluster.yml.j2): where the scheduler may put
Cilium's operator. Its ClusterRole reads the Secrets of every namespace, so with a system group (a sandbox domain is
set) it runs there and nowhere else; it must still start on that group while every worker is tainted. No cluster.

Run with the installer venv: python installer/tests/test_sandbox_cluster.py
"""
import unittest
from pathlib import Path

import yaml
from ansible.parsing.dataloader import DataLoader
from ansible.template import Templar

TEMPLATE = Path(__file__).resolve().parent.parent / "playbooks/templates/sandbox-cluster.yml.j2"
SYSTEM = "node-restriction.kubernetes.io/proxcloud-system"


def values(edge):
    """The Application's values, rendered with a sandbox domain (edge) or without one."""
    machines = {"count": 1, "cores": 2, "memory": 4096, "disk": 30}
    variables = {
        "pc": {
            "sandbox": {"pool": "proxcloud-sandbox", "nodes": machines, "system_nodes": machines,
                        "network": {"vnet": "sbx", "gateway": "192.0.2.1", "subnet": "192.0.2.0/24", "nodes": "192.0.2.10-192.0.2.29",
                                    "dns": ["1.1.1.1"], "mtu": 1450, "metallb": "192.0.2.200-192.0.2.210"}},
            "services": {"templates": {"pve1": 9000}, "chart_repo": "ghcr.io/example/charts", "k8s_version": "1.35.10"},
            "provider": {"region": "lab"},
        },
        "pc_portal_pve_url": "https://192.0.2.2:8006", "pc_sandbox_token_id": "capmox@pve!pc-sandbox", "pc_sandbox_token_secret": "x",
        "pc_chart_kamaji": "0.1.20", "pc_sandbox_storage": "local", "pc_sandbox_edge_on": edge,
    }
    out = Templar(loader=DataLoader(), variables=variables).template(TEMPLATE.read_text(), convert_data=False)
    app = next(d for d in yaml.safe_load_all(out) if d and d["kind"] == "Application")
    return app["spec"]["source"]["helm"]["valuesObject"]


def fits(v, group, fenced):
    """Whether the operator, as the cluster chart renders it from these values, may be scheduled on a node of group."""
    labels = dict(group.get("machineLabels", {}))  # Cluster API copies them to the Node; a kubelet cannot claim them
    taints = {t["key"] for t in group["taints"]} - ({"proxcloud.io/unsecured"} if fenced else set())
    tolerated = {t["key"] for t in v.get("ciliumOperatorTolerations", [])}
    return all(labels.get(k) == want for k, want in v.get("ciliumOperatorNodeSelector", {}).items()) and taints <= tolerated


class SandboxClusterTests(unittest.TestCase):
    def test_operator_runs_on_the_system_group_alone(self):
        v = values(True)
        groups = {g["name"]: g for g in v["nodeGroups"]}
        self.assertEqual(set(groups), {"sandbox", "sandbox-system"})
        self.assertEqual(v.get("ciliumOperatorNodeSelector"), {SYSTEM: "true"})
        for fenced in (False, True):
            self.assertTrue(fits(v, groups["sandbox-system"], fenced), "the cluster does not come up: no node for the operator")
            self.assertFalse(fits(v, groups["sandbox"], fenced), "the operator may land beside tenant code")

    def test_without_a_system_group_the_operator_still_has_a_node(self):
        v = values(False)
        self.assertEqual([g["name"] for g in v["nodeGroups"]], ["sandbox"])
        self.assertNotIn("ciliumOperatorNodeSelector", v)
        self.assertTrue(fits(v, v["nodeGroups"][0], False))


if __name__ == "__main__":
    unittest.main()
