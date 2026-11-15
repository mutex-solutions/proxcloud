#!/usr/bin/env python3
"""Offline check of the ArgoCD projects stage 50 leaves on the services cluster: `default` locked, and every Application
the installer or the portal puts there in a named project that the same bundle creates. No cluster.

Run with the installer venv: python installer/tests/test_services_argocd.py
"""
import re
import unittest
from pathlib import Path

import yaml

INSTALLER = Path(__file__).resolve().parent.parent
REPO = INSTALLER.parent
PLAYBOOKS = INSTALLER / "playbooks"
STAGE50 = yaml.safe_load((PLAYBOOKS / "50-services-cluster.yml").read_text())[0]
STAGE60 = yaml.safe_load((PLAYBOOKS / "60-register.yml").read_text())[1]
BUNDLE = next(t for t in STAGE50["tasks"] if t["name"] == "Copy the platform bundle")["loop"]


def applied_appprojects():
    """The AppProjects services-platform.sh applies: every bundle manifests/*.yaml except *health* (an argocd-cm patch)."""
    out = {}
    for item in BUNDLE:
        if not re.fullmatch(r"manifests/[^/]+\.yaml", item["dest"]) or "health" in item["dest"]:
            continue
        src = item["src"].replace("{{ pc_repo_root }}", str(REPO))
        path = Path(src) if src.startswith("/") else PLAYBOOKS / src
        for doc in yaml.safe_load_all(path.read_text()):
            if doc and doc["kind"] == "AppProject":
                out[doc["metadata"]["name"]] = doc
    return out


class ServicesArgoCDTests(unittest.TestCase):
    def test_default_project_is_locked(self):
        default = applied_appprojects()["default"]
        self.assertEqual(default["metadata"]["namespace"], "argocd")
        # Every list that ArgoCD's own default fills with "*" is set, and empty: a client-side apply replaces it.
        for key in ("sourceRepos", "destinations", "clusterResourceWhitelist"):
            self.assertEqual(default["spec"][key], [], key)

    def test_no_application_there_uses_default(self):
        named = set(applied_appprojects()) - {"default"}
        # Stage 55's pc-sandbox Application.
        sandbox = re.findall(r"^\s+project:\s*(\S+)", (PLAYBOOKS / "templates/sandbox-cluster.yml.j2").read_text(), re.M)
        # The projects stage 60 registers the providers with; the portal's tenant clusters and databases go there.
        task = next(t for t in STAGE60["tasks"] if t["name"] == "Register the managed-service providers")
        providers = [item["config"]["argocd"]["project"] for item in task["loop"]]
        self.assertEqual(sandbox, ["proxcloud-k8s"])
        self.assertEqual(providers, ["proxcloud-k8s", "proxcloud-dbaas"])
        self.assertLessEqual(set(sandbox + providers), named)


if __name__ == "__main__":
    unittest.main()
