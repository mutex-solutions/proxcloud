#!/usr/bin/env python3
"""Offline check of the ArgoCD projects stage 50 leaves on the services cluster: `default` locked, every Application the
installer puts there (and the providers it registers for the portal) in a named project that the same bundle creates,
and stage 50 stopping while an Application there is still in `default`. No cluster.

Run with the installer venv: python installer/tests/test_services_argocd.py
"""
import os
import re
import subprocess
import tempfile
import unittest
from pathlib import Path

import yaml
from ansible.parsing.dataloader import DataLoader
from ansible.template import Templar

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


    def test_stage50_stops_while_an_application_is_still_in_default(self):
        # Locked, `default` leaves such an Application unsynced, and its delete then removes nothing it made (ArgoCD
        # only deletes what the project permits), so stage 50 lists them and stops before the script applies the lock.
        names = [t["name"] for t in STAGE50["tasks"]]
        find = "List ArgoCD Applications on the services cluster still in project default"
        stop = "Stop while any of them is in project default"
        self.assertLess(names.index(find), names.index(stop))
        self.assertLess(names.index(stop), names.index("Run the platform script"))
        listed = STAGE50["tasks"][names.index(find)]
        self.assertEqual(listed["environment"]["KUBECONFIG"], "/root/services.kubeconfig")
        self.assertEqual(listed["until"], "pc_default_apps.rc == 0")
        apps = "default\targocd/old\nproxcloud-k8s\targocd/c-1\n\targocd/blank\nproxcloud-dbaas\targocd/db-1\n"
        self.assertEqual(self.run_listing(listed, crd=True, apps=apps), ["argocd/old", "argocd/blank"])
        self.assertEqual(self.run_listing(listed, crd=True, apps="proxcloud-k8s\targocd/c-1\n"), [])
        self.assertEqual(self.run_listing(listed, crd=False, apps=apps), [])  # first run: no ArgoCD yet
        # An API that does not answer fails the task (and its retries), never reads as "no ArgoCD, nothing to list".
        self.assertIsNone(self.run_listing(listed, crd="down", apps=apps))
        fail = STAGE50["tasks"][names.index(stop)]
        for stdout, want in (("argocd/old", True), ("", False)):
            t = Templar(loader=DataLoader(), variables={"pc_default_apps": {"stdout": stdout, "stdout_lines": stdout.split()}})
            self.assertEqual(t.template("{{ " + fail["when"] + " }}"), want)
            if want:
                msg = t.template(fail["ansible.builtin.fail"]["msg"])
                self.assertIn("argocd/old", msg)
                self.assertIn("config.argocd.project", msg)

    @staticmethod
    def run_listing(task, crd, apps):
        """Runs the task's shell with a kubectl that answers the CRD lookup and `get applications` from the arguments.
        The listed names, or None when the shell fails."""
        with tempfile.TemporaryDirectory() as d:
            kubectl = Path(d) / "kubectl"
            kubectl.write_text('#!/bin/bash\ncase "$*" in\n'
                               '  "get crd applications.argoproj.io --ignore-not-found -o name")\n'
                               '    [ "$CRD" != down ] || exit 1; [ -z "$CRD" ] || echo "$CRD" ;;\n'
                               '  "get applications.argoproj.io -A -o jsonpath="*) printf "$APPS" ;;\n'
                               '  *) echo "unexpected: kubectl $*" >&2; exit 9 ;;\nesac\n')
            kubectl.chmod(0o755)
            crd = {True: "customresourcedefinition.apiextensions.k8s.io/applications.argoproj.io", False: ""}.get(crd, crd)
            env = dict(os.environ, PATH=d + ":" + os.environ["PATH"], APPS=apps, CRD=crd)
            r = subprocess.run(["bash", "-c", task["ansible.builtin.shell"]], env=env, capture_output=True, text=True)
            return r.stdout.split() if r.returncode == 0 else None


if __name__ == "__main__":
    unittest.main()
