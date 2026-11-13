#!/usr/bin/env python3
"""Offline tests of the sandbox mirror's installer side (docs/template-updates.md §4): the Zot mirror account (stage 55),
its hand-over to the portal (stage 60), the portal pin read with its fail-closed path and the merge with images.yaml
(stage 55), and the prepull RBAC + admission policy rendered next to the DaemonSet. No cluster, registry or API.

Run with the installer venv: python installer/tests/test_sandbox_mirror.py
"""
import copy
import json
import re
import sys
import tempfile
import unittest
from pathlib import Path

import yaml
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
STAGE55 = yaml.safe_load((INSTALLER / "playbooks/55-sandbox-registry.yml").read_text())
STAGE60 = yaml.safe_load((INSTALLER / "playbooks/60-register.yml").read_text())
REG = "192.0.2.226"
D1, D2, D3 = "sha256:" + "1" * 64, "sha256:" + "2" * 64, "sha256:" + "3" * 64


def task(play, name):
    for t in play["tasks"]:
        if t.get("name") == name:
            return t
        for sub in t.get("block", []) + t.get("rescue", []):
            if sub.get("name") == name:
                return sub
    raise KeyError(name)


def pin(name, tag, digest, previous="", action="mirror"):
    return {"id": "%s/library/sandbox-%s" % (REG, name), "template": name, "registry": REG, "tag": tag, "digest": digest,
            "ref": "%s/library/sandbox-%s:%s@%s" % (REG, name, tag, digest), "action": action, "previous": previous}


class MirrorAccountTests(unittest.TestCase):
    def test_stage55_generates_the_mirror_password_next_to_gc(self):
        users = STAGE55[1]["vars"]["pc_sbx_reg_users"]
        self.assertIn("mirror", users)
        self.assertIn("pc_gen ~ '/sandbox-registry-mirror ' ~ pc_alnum", users["mirror"])
        self.assertIn("ansible.builtin.password", users["mirror"])  # generated once, kept in generated/
        # Same lookup on a re-run: the password file persists, so the Secret and Zot's htpasswd do not change.
        with tempfile.TemporaryDirectory() as d:
            t = Templar(loader=DataLoader(), variables={"pc_gen": d, "pc_alnum": "chars=ascii_letters,digits length=32"})
            first, again = t.template(users["mirror"]), t.template(users["mirror"])
            self.assertEqual(first, again)
            self.assertEqual(len(first), 32)
            self.assertEqual((Path(d) / "sandbox-registry-mirror").read_text().split()[0], first)

    def test_registry_users_secret_carries_the_mirror_password(self):
        # The account sync reads one file per user from the Secret: the template writes the whole users dict.
        tpl = (INSTALLER / "playbooks/templates/sandbox-registry.yml.j2").read_text()
        self.assertIn("stringData: {{ pc_sbx_reg_users | to_json }}", tpl)
        self.assertIn("pc_sbx_reg_users | to_json", tpl.split("proxcloud.io/config")[1].split("\n")[0])  # restart on change

    def test_stage60_hands_it_over_write_only_and_idempotent(self):
        reg = task(STAGE60[1], "Register the sandbox provider")
        self.assertIn("'registryMirror': lookup('ansible.builtin.file', pc_gen ~ '/sandbox-registry-mirror') | trim", reg["proxcloud_api"]["body"]["secret"])
        hand = task(STAGE60[1], "Hand the registry mirror account to the sandbox provider")
        put = hand["block"][0]
        self.assertIs(put["no_log"], True)
        self.assertEqual(put["proxcloud_api"]["method"], "PUT")
        self.assertEqual(list(put["proxcloud_api"]["body"]), ["secret"])  # nothing but the secret is written
        self.assertEqual(list(put["proxcloud_api"]["body"]["secret"]), ["registryMirror"])
        when = " ".join(hand["when"])
        self.assertIn("sandbox-registry-mirror.handed", when)  # re-run: nothing written unless the password changed
        # The marker never holds the password itself.
        mark = hand["vars"]["pc_sbx_mirror_mark"]
        self.assertIn("hash('sha256')", mark)
        with tempfile.TemporaryDirectory() as d:
            (Path(d) / "sandbox-registry-mirror").write_text("MIRROR-PW-DO-NOT-STORE\n")
            t = Templar(loader=DataLoader(), variables={"pc_gen": d, "pc_sbx_row": {"id": "sbx-1"}})
            m1 = t.template(mark)
            self.assertNotIn("MIRROR-PW", m1)
            self.assertEqual(m1, t.template(mark))
            (Path(d) / "sandbox-registry-mirror").write_text("rotated\n")
            t = Templar(loader=DataLoader(), variables={"pc_gen": d, "pc_sbx_row": {"id": "sbx-1"}})
            self.assertNotEqual(m1, t.template(mark))
        rows = [{"id": "pve-1", "config": {"provider": "proxmox"}}, {"id": "sbx-1", "config": {"provider": "sandbox"}}, {"id": "x"}]
        t = Templar(loader=DataLoader(), variables={"pc_services": {"json": {"data": rows}}})
        self.assertEqual(t.template(hand["vars"]["pc_sbx_row"])["id"], "sbx-1")


class PinMergeTests(unittest.TestCase):
    YAML = {"python": "docker.io/library/python:3.12-slim@" + D1,
            "base": "ghcr.io/mutex-solutions/sandbox-base:20261101-aaaaaaa@" + D1}

    def test_newer_pin_wins_and_older_never_silently_rolls_back(self):
        out = f.sandbox_portal_pins(self.YAML, [pin("base", "20261112-9d00563", D2)], REG)
        self.assertEqual(out["pins"]["base"], "ghcr.io/mutex-solutions/sandbox-base:20261112-9d00563@" + D2)
        self.assertEqual(out["pins"]["python"], self.YAML["python"])
        self.assertEqual(out["commit"], ["  base: ghcr.io/mutex-solutions/sandbox-base:20261112-9d00563@" + D2])
        # images.yaml moved past the pin: images.yaml wins, nothing to commit.
        newer = dict(self.YAML, base="ghcr.io/mutex-solutions/sandbox-base:20261120-bbbbbbb@" + D3)
        out = f.sandbox_portal_pins(newer, [pin("base", "20261112-9d00563", D2)], REG)
        self.assertEqual(out["pins"]["base"], newer["base"])
        self.assertEqual(out["commit"], [])
        # Same digest: no change.
        same = dict(self.YAML, base="ghcr.io/mutex-solutions/sandbox-base:20261112-9d00563@" + D2)
        self.assertEqual(f.sandbox_portal_pins(same, [pin("base", "20261112-9d00563", D2)], REG)["pins"], same)

    def test_a_portal_rollback_of_the_yaml_pin_is_kept(self):
        # images.yaml = D2 (the newer one); the portal rolled back to D1 from exactly D2: the rollback stays.
        yml = dict(self.YAML, base="ghcr.io/mutex-solutions/sandbox-base:20261112-9d00563@" + D2)
        rb = pin("base", "20261101-aaaaaaa", D1, previous="%s/library/sandbox-base:20261112-9d00563@%s" % (REG, D2), action="rollback")
        out = f.sandbox_portal_pins(yml, [rb], REG)
        self.assertEqual(out["pins"]["base"], "ghcr.io/mutex-solutions/sandbox-base:20261101-aaaaaaa@" + D1)

    def test_unorderable_or_malformed_pins_stop_the_upgrade(self):
        with self.assertRaisesRegex(ValueError, "cannot be told"):
            f.sandbox_portal_pins(self.YAML, [pin("base", "20261101-bbbbbbb", D2)], REG)  # same date, other digest
        bad = pin("base", "20261112-9d00563", D2)
        bad["id"] = REG + "/library/proxcloud-api"
        with self.assertRaisesRegex(ValueError, "malformed"):
            f.sandbox_portal_pins(self.YAML, [bad], REG)
        with self.assertRaisesRegex(ValueError, "malformed"):
            f.sandbox_portal_pins(self.YAML, [dict(pin("base", "latest", D2))], REG)

    def test_other_registries_ignored_and_new_images_added(self):
        other = dict(pin("base", "20261112-9d00563", D2), registry="198.51.100.9")
        self.assertEqual(f.sandbox_portal_pins(self.YAML, [other], REG)["pins"], self.YAML)
        out = f.sandbox_portal_pins(self.YAML, [pin("node", "20261112-9d00563", D2)], REG)
        self.assertEqual(out["pins"]["node"], "ghcr.io/mutex-solutions/sandbox-node:20261112-9d00563@" + D2)
        # The merged list still renders through the mirror list (same digest, library/ target).
        imgs = {i["name"]: i for i in f.sandbox_images(out["pins"], REG)}
        self.assertEqual(imgs["node"]["target"], "%s/library/sandbox-node:20261112-9d00563@%s" % (REG, D2))

    def test_stage55_reads_pins_and_fails_closed(self):
        play = STAGE55[1]
        self.assertIn("pc_sbx_pins_merged.pins | pc_sandbox_images(host)", play["vars"]["images"])
        read = task(play, "Read the portal's sandbox image pins")
        get = read["block"][1]
        self.assertTrue(get["proxcloud_api"]["url"].startswith("http://{{ pc_sbx_api_ip.stdout }}:8080/api/v1/admin/template-updates/sandbox/pins"))
        self.assertNotIn("method", get["proxcloud_api"])  # GET only: the read-only path
        self.assertEqual(read["when"], "pc_sbx_key.stat.exists")
        stop = read["rescue"][0]
        self.assertIn("ansible.builtin.fail", stop)
        self.assertIn("stops here", stop["ansible.builtin.fail"]["msg"])
        self.assertIn("pc_sandbox_portal_pins_unavailable_ok", stop["when"])
        # The pin read happens before anything is copied or applied.
        names = [t.get("name") for t in play["tasks"]]
        self.assertLess(names.index("Merge the pins with images.yaml"), names.index("Copy the pinned images in"))
        self.assertLess(names.index("Merge the pins with images.yaml"), names.index("Apply the prepull and the registry's address on the sandbox cluster"))

    def test_merge_task_renders(self):
        merge = task(STAGE55[1], "Merge the pins with images.yaml")["ansible.builtin.set_fact"]["pc_sbx_pins_merged"]
        ok = {"json": {"data": {"pins": [pin("base", "20261112-9d00563", D2)]}}, "failed": False, "changed": False}
        for portal, want in ((ok, D2), (None, D1)):
            v = {"pc_sandbox_pins": self.YAML, "host": REG}
            if portal is not None:
                v["pc_sbx_portal_pins"] = portal
            t = Templar(loader=DataLoader(), variables=v)
            out = t.template(merge)
            self.assertTrue(out["pins"]["base"].endswith(want), out)


class PrepullPolicyTests(unittest.TestCase):
    def render(self):
        pc = copy.deepcopy(DEFAULTS)
        pc["sandbox"]["registry"]["address"] = REG
        imgs = f.sandbox_images({"base": "ghcr.io/mutex-solutions/sandbox-base:20261101-aaaaaaa@" + D1}, REG)
        t = Templar(loader=DataLoader(), variables={"pc": pc, "pc_sbx_images": imgs, "pc_sbx_reg_ca": "CA",
                                                    "pc_ver": {"pause_image": "pause"}, "pc_sbx_reg_system_ns": "sandbox-system"})
        text = t.template((INSTALLER / "playbooks/templates/sandbox-prepull.yml.j2").read_text())
        return {(d["kind"], d["metadata"]["name"]): d for d in yaml.safe_load_all(text)}

    def test_role_is_get_patch_on_the_two_daemonsets_only(self):
        docs = self.render()
        role = docs[("Role", "proxcloud-template-prepull")]
        self.assertEqual(role["metadata"]["namespace"], "sandbox-system")
        self.assertEqual(role["rules"], [{"apiGroups": ["apps"], "resources": ["daemonsets"],
                                          "resourceNames": ["template-prepull", "template-prepull-gpu"], "verbs": ["get", "patch"]}])
        rb = docs[("RoleBinding", "proxcloud-template-prepull")]
        # the sandbox provider's kubeconfig identity (deploy/sandbox-cluster/rbac.yaml, stage 55 sandbox-portal.kubeconfig)
        self.assertEqual(rb["subjects"], [{"kind": "ServiceAccount", "name": "proxcloud", "namespace": "proxcloud-system"}])
        rbac = (INSTALLER.parent / "deploy/sandbox-cluster/rbac.yaml").read_text()
        self.assertIn("name: proxcloud\n  namespace: proxcloud-system", rbac)

    def test_policy_shape(self):
        docs = self.render()
        vap = docs[("ValidatingAdmissionPolicy", "proxcloud-template-prepull-images-only")]
        binding = docs[("ValidatingAdmissionPolicyBinding", "proxcloud-template-prepull-images-only")]
        self.assertEqual(binding["spec"], {"policyName": "proxcloud-template-prepull-images-only", "validationActions": ["Deny"]})
        self.assertEqual(vap["spec"]["failurePolicy"], "Fail")
        self.assertEqual(vap["spec"]["matchConditions"][0]["expression"],
                         'request.userInfo.username == "system:serviceaccount:proxcloud-system:proxcloud"')
        rules = vap["spec"]["matchConstraints"]["resourceRules"]
        self.assertEqual(rules[0]["resources"], ["daemonsets"])
        self.assertEqual(set(rules[0]["operations"]), {"CREATE", "UPDATE", "DELETE"})
        # The digest-ref regex, rendered with this registry, accepts exactly mirrored digest refs.
        ref = vap["spec"]["variables"][0]["expression"].strip()
        m = re.fullmatch(r"r'(.*)'", ref)
        self.assertIsNotNone(m, ref)
        rx = re.compile(m.group(1))
        for good in ["%s/library/sandbox-base:20261112-9d00563@%s" % (REG, D2), "%s/library/sandbox-code-interpreter-gpu@%s" % (REG, D2)]:
            self.assertTrue(rx.fullmatch(good), good)
        for bad in ["%s/library/sandbox-base:20261112-9d00563" % REG, "%s/library/proxcloud-sandbox-import@%s" % (REG, D2),
                    "%s/t/p/x@%s" % (REG, D2), "192.0.2.2266/library/sandbox-base@%s" % D2, "192x0x2x226/library/sandbox-base@%s" % D2,
                    "ghcr.io/mutex-solutions/sandbox-base@%s" % D2, "%s/library/sandbox-base@sha256:%s" % (REG, "A" * 64),
                    "%s/library/sandbox-base:latest@%s" % (REG, D2), "%s/library/sandbox-base@%s\nx" % (REG, D2)]:
            self.assertIsNone(rx.fullmatch(bad), bad)

    def test_policy_compares_every_pod_and_container_field(self):
        vap = self.render()[("ValidatingAdmissionPolicy", "proxcloud-template-prepull-images-only")]
        exprs = "\n".join(v["expression"] for v in vap["spec"]["validations"])
        for field in ["containers", "volumes", "imagePullSecrets", "tolerations", "nodeSelector", "hostNetwork", "securityContext",
                      "serviceAccountName", "automountServiceAccountToken", "runtimeClassName"]:
            self.assertIn("object.spec.template.spec.%s" % field, exprs)
        for field in ["command", "args", "env", "volumeMounts", "securityContext", "resources"]:
            self.assertIn("c.%s == o.%s" % (field, field), exprs)
        for field in ["selector", "updateStrategy", "template.metadata"]:
            self.assertIn("object.spec.%s == oldObject.spec.%s" % (field, field), exprs)
        for field in ["labels", "ownerReferences", "finalizers"]:
            self.assertIn("object.metadata.%s == oldObject.metadata.%s" % (field, field), exprs)
        # annotations: all compared but the API server's template-generation bookkeeping
        self.assertIn('k == "deprecated.daemonset.template.generation"', exprs)
        self.assertIn("variables.oldAnnotations[k] == variables.annotations[k]", exprs)
        # Lightweight syntax check of every CEL expression: balanced brackets and quotes.
        for v in vap["spec"]["validations"] + vap["spec"]["variables"] + vap["spec"]["matchConditions"]:
            e = v["expression"]
            for o, c in ("()", "[]", "{}"):
                self.assertEqual(e.count(o), e.count(c), e)
            self.assertEqual(e.count('"') % 2, 0, e)
        self.assertIn('object.metadata.name in ["template-prepull", "template-prepull-gpu"]', exprs)
        self.assertIn('request.operation == "UPDATE"', exprs)
        # The policy text is shared with the Go policy test (internal/cloud/sandbox/prepull_policy_test.go).
        self.assertIn("variables.newInit.size() <= variables.oldInit.size() + 1", exprs)


if __name__ == "__main__":
    unittest.main(verbosity=2)
