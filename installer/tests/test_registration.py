#!/usr/bin/env python3
"""Offline stage-60 payload tests. No PVE/API requests, inventory secrets or generated site files.

Run with the existing installer venv: python installer/tests/test_registration.py
"""
import copy
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

import yaml
from ansible.errors import AnsibleError
from ansible.parsing.dataloader import DataLoader
from ansible.plugins.loader import filter_loader, init_plugin_loader
from ansible.template import Templar

HERE = Path(__file__).resolve().parent
INSTALLER = HERE.parent
sys.path.insert(0, str(INSTALLER / "filter_plugins"))
import proxcloud as f  # noqa: E402

init_plugin_loader()
filter_loader.add_directory(str(INSTALLER / "filter_plugins"))
DEFAULTS = yaml.safe_load((INSTALLER / "playbooks/group_vars/all/defaults.yml").read_text())["pc_defaults"]
PLAY = yaml.safe_load((INSTALLER / "playbooks/60-register.yml").read_text())[1]
TASKS = {task["name"]: task for task in PLAY["tasks"]}
CA = (HERE / "fixtures/pve-ca.pem").read_text()  # public, disposable self-signed fixture; private key discarded
PIN = ":".join(["AB"] * 32)
URL = "https://pve1.example.test:8006"
URLS = ["https://pve2.example.test:8006", "https://pve3.example.test:8006/api2/json"]


class ConnectionTests(unittest.TestCase):
    def test_legacy_leaf_fingerprint(self):
        self.assertEqual(f.pve_connection({}, URL, fingerprint=PIN), {"apiUrl": URL, "fingerprint": PIN})
        self.assertEqual(f.pve_connection({"fingerprint": PIN.lower()}, URL, fingerprint="wrong")["fingerprint"], PIN.lower())

    def test_ha_urls_and_explicit_ca(self):
        site = {"portal_api_urls": URLS, "cluster_ca": CA}
        with patch("socket.create_connection", side_effect=AssertionError("no endpoint probe")):
            out = f.pve_connection(site, URL)
        self.assertEqual(out, {"apiUrl": URL, "apiUrls": URLS, "caCert": CA, "insecureTls": False})
        self.assertNotIn("clusterCA", out)  # ignored spelling must never be emitted
        self.assertEqual(f.pve_connection(site, URL, out), out)
        out["apiUrls"].append("https://not-in-input.test")
        self.assertEqual(site["portal_api_urls"], URLS)

    def test_omitted_and_null_preserve_installed_trust(self):
        for trust in ({}, {"insecureTls": False}, {"insecureTls": True}, {"fingerprint": PIN},
                      {"fingerprint": ""}, {"caCert": CA, "fingerprint": PIN, "insecureTls": False}):
            stored = {"apiUrl": URL, "apiUrls": URLS, **trust, "tokenId": "rotated@pve!keep"}
            before = copy.deepcopy(stored)
            for site in ({}, {"portal_api_urls": None, "cluster_ca": None}):
                out = f.pve_connection(site, URL, stored, "not-used")
                self.assertEqual(out, {"apiUrl": URL, "apiUrls": URLS, **trust})
            self.assertEqual(stored, before)

    def test_explicit_empty_list_clears_only_failover(self):
        stored = {"apiUrls": URLS, "caCert": CA, "fingerprint": PIN, "insecureTls": False}
        self.assertEqual(f.pve_connection({"portal_api_urls": []}, URL, stored),
                         {"apiUrl": URL, **stored, "apiUrls": []})

    def test_explicit_ca_disables_old_insecure_bypass(self):
        out = f.pve_connection({"cluster_ca": CA}, URL, {"insecureTls": True, "fingerprint": PIN})
        self.assertIs(out["insecureTls"], False)
        self.assertEqual(out["fingerprint"], PIN)
        self.assertEqual(out["caCert"], CA)

    def test_fresh_missing_trust_never_becomes_insecure(self):
        for site in ({}, {"portal_api_urls": URLS}, {"cluster_ca": None}):
            with self.assertRaisesRegex(ValueError, "no pinned PVE fingerprint"):
                f.pve_connection(site, URL)
        with self.assertRaisesRegex(ValueError, "SHA-256"):
            f.pve_connection({}, URL, fingerprint="invalid")

    def test_explicit_invalid_ca_never_falls_back_to_pin(self):
        for bad in ("", " ", "missing-file.pem", "-----BEGIN CERTIFICATE-----\nbad\n-----END CERTIFICATE-----",
                    CA + "\n-----BEGIN PRIVATE KEY-----\nDO-NOT-RENDER\n-----END PRIVATE KEY-----", CA * 100, False, 1):
            with self.assertRaises(ValueError) as failure:
                f.pve_connection({"cluster_ca": bad}, URL, {"caCert": CA, "fingerprint": PIN}, PIN)
            self.assertNotIn("DO-NOT-RENDER", str(failure.exception))
            self.assertNotIn(CA, str(failure.exception))

    def test_urls_validated_without_echoing_credentials(self):
        for bad in ("not-a-list", False, [None], ["https://u:DO-NOT-RENDER@pve.test"],
                    ["https://pve.test?token=DO-NOT-RENDER"], ["https://pve.test#DO-NOT-RENDER"],
                    ["https://pve.test?"], ["https://pve.test#"],
                    ["ftp://pve.test"], ["https://pve.test:bad"], ["https://pve.test:0"],
                    ["https://pve.test/wrong/path"], ["https://pve.test/ with-space"]):
            with self.assertRaises(ValueError) as failure:
                f.pve_connection({"portal_api_urls": bad}, URL, fingerprint=PIN)
            self.assertNotIn("DO-NOT-RENDER", str(failure.exception))
        self.assertEqual(f.pve_connection({"portal_api_urls": ["http://pve.test:8006"]}, URL, fingerprint=PIN)["apiUrls"],
                         ["http://pve.test:8006"])  # no global TLS-policy migration in this bounded fix


class StageRenderTests(unittest.TestCase):
    def render(self, directory, site=None, row=None, pin=PIN, rows=None):
        # Only synthetic files created here; never point this renderer at a real site's generated directory.
        root = Path(directory)
        if pin is not None:
            (root / "pve-fingerprint").write_text(pin + "\n")
        pc = copy.deepcopy(DEFAULTS)
        pc["pve"].update({"api_url": "https://installer-only.test:8006", "portal_api_url": URL, **(site or {})})
        pc["provider"].update(name="Fixture", region="test", region_name="Test")
        variables = copy.deepcopy(PLAY["vars"])
        variables.update(pc=pc, pc_gen=str(root), pc_portal_pve_url=URL, pc_sandbox_on=False,
                         pc_sandbox_registry_on=False, pc_services={"json": {"data": rows if rows is not None else ([row] if row else [])}},
                         hostvars={"localhost": {"pc_status": {"json": {"data": [{"type": "node", "ip": "192.0.2.11"}]}},
                                                 "pc_nodes": ["pve1"], "pc_profiles": {"std": {"storages": {"*": "local-zfs"}}},
                                                 "pc_default_profile": "std"}},
                         fio={}, api_key={"id": "pk-fixture", "secretKey": "ADMIN-SECRET-NOT-IN-CONFIG"},
                         pc_svc_api="http://api.fixture.invalid/api/v1")
        templar = Templar(loader=DataLoader(), variables=variables)
        variables["pc_service"] = templar.template(TASKS["Note the existing provider before rendering its configuration"]["ansible.builtin.set_fact"]["pc_service"])
        variables["pc_provider_connection"] = templar.template(TASKS["Resolve the provider endpoints and certificate trust locally"]["ansible.builtin.set_fact"]["pc_provider_connection"])
        # Updating available_variables clears Ansible's template cache after the set_fact step.
        templar.available_variables = variables
        return templar, templar.template(variables["provider"])

    def test_fresh_payload_and_create_retry_noop(self):
        with tempfile.TemporaryDirectory() as directory:
            templar, provider = self.render(directory)
            self.assertEqual(provider["config"]["apiUrl"], URL)
            self.assertEqual(provider["config"]["fingerprint"], PIN)
            self.assertNotIn("apiUrls", provider["config"])
            self.assertNotIn("caCert", provider["config"])
            self.assertNotIn("insecureTls", provider["config"])
            self.assertNotIn("secret", provider)
            row = {"id": "fixture-provider", **provider}
            templar, _ = self.render(directory, row=row, pin=None)
            self.assertFalse(templar.template("{{ " + TASKS["Keep the configuration of an existing provider in line"]["when"] + " }}"))

    def test_new_ca_without_generated_fingerprint_file(self):
        with tempfile.TemporaryDirectory() as directory:
            templar, provider = self.render(directory, {"portal_api_urls": URLS, "cluster_ca": CA}, pin=None)
            self.assertEqual(provider["config"]["apiUrls"], URLS)
            self.assertEqual(provider["config"]["caCert"], CA)
            self.assertIs(provider["config"]["insecureTls"], False)
            self.assertNotIn("fingerprint", provider["config"])
            self.assertNotIn("secret", provider)

    def test_existing_provider_merge_keeps_unrelated_config_secret_and_token(self):
        with tempfile.TemporaryDirectory() as directory:
            _, initial = self.render(directory)
            config = initial["config"]
            config.update(apiUrls=URLS, caCert=CA, fingerprint=PIN.lower(), insecureTls=False,
                          tokenId="rotated@pve!token", services={"compute": {"test": False}},
                          custom={"retain": [1, 2]}, capabilities={"detectedAt": "keep"}, timeoutSeconds=41)
            row = {"id": "fixture-provider", **initial, "secret": {"tokenSecret": "CIPHERTEXT-DO-NOT-RENDER"}}
            before = copy.deepcopy(row)
            templar, provider = self.render(directory, row=row, pin=None)
            task = TASKS["Keep the configuration of an existing provider in line"]
            self.assertFalse(templar.template("{{ " + task["when"] + " }}"))
            body = templar.template(task["proxcloud_api"]["body"])
            self.assertNotIn("secret", body)
            self.assertNotIn("tokenId", body["config"])
            self.assertNotIn("services", body["config"])
            self.assertNotIn("CIPHERTEXT-DO-NOT-RENDER", json.dumps(body))
            self.assertNotIn("ADMIN-SECRET-NOT-IN-CONFIG", json.dumps(body))
            self.assertEqual({**row["config"], **body["config"]}, row["config"])
            self.assertEqual(row, before)
            self.assertEqual(provider["config"]["apiUrls"], URLS)
            self.assertEqual(provider["config"]["caCert"], CA)

    def test_connection_only_update_then_second_retry_noop(self):
        with tempfile.TemporaryDirectory() as directory:
            _, initial = self.render(directory)
            initial["config"].update(insecureTls=True, custom={"retained": True})
            row = {"id": "fixture-provider", **initial}
            site = {"portal_api_urls": URLS, "cluster_ca": CA}
            templar, _ = self.render(directory, site, row=row, pin=None)
            task = TASKS["Keep the configuration of an existing provider in line"]
            self.assertTrue(templar.template("{{ " + task["when"] + " }}"))
            body = templar.template(task["proxcloud_api"]["body"])
            # applyServiceConnectionBody overlays top-level config keys, leaving the encrypted secret untouched.
            row["config"].update(body["config"])
            self.assertEqual(row["config"]["custom"], {"retained": True})
            self.assertEqual(row["config"]["apiUrls"], URLS)
            self.assertEqual(row["config"]["caCert"], CA)
            self.assertIs(row["config"]["insecureTls"], False)
            templar, _ = self.render(directory, site, row=row, pin=None)
            self.assertFalse(templar.template("{{ " + task["when"] + " }}"))

    def test_same_name_collision_duplicate_and_malformed_rows_stop_before_connection_render(self):
        good = {"id": "fixture-id", "name": "Fixture", "type": "CLOUD", "config": {"provider": "proxmox"}}
        cases = [[{**good, "config": {"provider": kind}}] for kind in ("ceph-s3", "sandbox")]
        cases += [[good, copy.deepcopy(good)], [{**good, "type": "OTHER"}], [{**good, "id": ""}],
                  [{**good, "id": "../bad?DO-NOT-RENDER"}], [{**good, "config": None}],
                  [{**good, "config": "DO-NOT-RENDER"}], ["DO-NOT-RENDER"]]
        with tempfile.TemporaryDirectory() as directory:
            for rows in cases:
                with patch.object(f, "pve_connection", side_effect=AssertionError("must reject before connection")):
                    with self.assertRaises((AnsibleError, ValueError)) as failure:
                        self.render(directory, rows=rows)
                self.assertNotIn("DO-NOT-RENDER", str(failure.exception))
        self.assertEqual(f.registered_pve_provider([], "Fixture"), {})
        with self.assertRaisesRegex(ValueError, "uniquely named"):
            f.registered_pve_provider([], "Fixture", required=True)
        self.assertEqual(f.registered_pve_provider([good], "Fixture", required=True), good)
        self.assertIn("pc_registered_pve_provider(pc.provider.name, true)",
                      TASKS["Note the provider"]["ansible.builtin.set_fact"]["pc_service"])

    def test_secret_bearing_tasks_and_optional_defaults(self):
        for name in ("Note the existing provider before rendering its configuration", "Note the provider",
                     "Resolve the provider endpoints and certificate trust locally", "Register the Proxmox provider",
                     "Keep the configuration of an existing provider in line"):
            self.assertIs(TASKS[name]["no_log"], True, name)
        self.assertIsNone(DEFAULTS["pve"]["portal_api_urls"])
        self.assertIsNone(DEFAULTS["pve"]["cluster_ca"])
        self.assertEqual(DEFAULTS["pve"]["portal_api_url"], "")
        self.assertEqual(DEFAULTS["pve"]["fingerprint"], "")


if __name__ == "__main__":
    unittest.main(verbosity=2)
