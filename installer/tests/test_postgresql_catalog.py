#!/usr/bin/env python3
"""Data-only checks of the fresh DBaaS provider catalog in stage 60.

Reads repository YAML only: no Ansible/Jinja evaluation, installer imports,
subprocesses, API requests, generated site files, or existing-provider mutations.
The name-absence skip is pinned structurally, not exercised against a live API.
"""
from pathlib import Path
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[2]


def task_nodes(tasks):
    """Visit nested task blocks without evaluating or executing any task."""
    for task in tasks:
        yield task
        for key in ("block", "rescue", "always"):
            yield from task_nodes(task.get(key, []))


def database_payload(body):
    """Identify the literal database provider bodies carried by this playbook."""
    if not isinstance(body, dict):
        return False
    config = body.get("config", {})
    if not isinstance(config, dict):
        return False
    return (config.get("provider") == "dbaas" or "database" in config
            or config.get("argocd", {}).get("chartName") == "database-cluster")


class PostgreSQLCatalogTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        plays = yaml.safe_load((ROOT / "installer/playbooks/60-register.yml").read_text(encoding="utf-8"))
        cls.tasks = list(task_nodes([task for play in plays for task in play.get("tasks", [])]))
        matches = [task for task in cls.tasks if task.get("name") == "Register the managed-service providers"]
        if len(matches) != 1:
            raise AssertionError("expected one managed-service registration task")
        cls.registration = matches[0]
        providers = [item for item in cls.registration["loop"] if database_payload(item)]
        if len(providers) != 1:
            raise AssertionError("expected one literal DBaaS provider entry")
        cls.provider = providers[0]

    def test_new_provider_curates_17_and_18_default_18(self):
        self.assertEqual(self.provider["name"], "{{ pc.provider.name }} Databases")
        self.assertEqual(self.provider["config"]["provider"], "dbaas")
        self.assertEqual(self.provider["config"]["database"]["engines"], {
            "postgresql": {"versions": ["17", "18"], "default": "18", "replicas": [1, 3]},
        })

    def test_registration_remains_name_absence_guarded_post_only(self):
        # Preserve skip-by-name for existing rows; do not replace with an update,
        # upsert, or a guard based on the existing provider's versions/default.
        self.assertEqual(self.registration["when"],
                         "pc.mode == 'split' and item.name not in (pc_services.json.data | map(attribute='name') | list)")
        request = self.registration["proxcloud_api"]
        self.assertEqual(request["method"], "POST")
        self.assertEqual(request["url"], "{{ pc_svc_api }}/admin/service")
        self.assertEqual(request["body"], "{{ item }}")
        self.assertIs(self.registration["no_log"], True)
        self.assertEqual(self.registration["loop_control"], {"label": "{{ item.name }}"})

    def test_literal_database_payload_has_no_separate_update_task(self):
        writes = []
        for task in self.tasks:
            request = task.get("proxcloud_api")
            if not isinstance(request, dict):
                continue
            payloads = [request.get("body")]
            loop = task.get("loop", [])
            if isinstance(loop, list):
                payloads.extend(loop)
            if any(database_payload(body) for body in payloads):
                writes.append((task["name"], request.get("method", "GET")))
        self.assertEqual(writes, [(self.registration["name"], "POST")])

    def test_new_provider_chart_pin_still_comes_from_checkout_metadata(self):
        chart = yaml.safe_load((ROOT / "deploy/charts/database-cluster/Chart.yaml").read_text(encoding="utf-8"))
        self.assertEqual(chart["version"], "0.5.1")
        self.assertEqual(self.provider["config"]["argocd"]["chartName"], "database-cluster")
        self.assertEqual(self.provider["config"]["argocd"]["chartVersion"], "{{ pc_chart_database }}")
        computed = yaml.safe_load((ROOT / "installer/playbooks/group_vars/all/computed.yml").read_text(encoding="utf-8"))
        self.assertEqual(computed["pc_chart_database"],
                         "{{ (lookup('ansible.builtin.file', pc_repo_root ~ '/deploy/charts/database-cluster/Chart.yaml') | from_yaml).version }}")


if __name__ == "__main__":
    unittest.main(verbosity=2)
