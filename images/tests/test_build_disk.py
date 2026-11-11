#!/usr/bin/env python3
"""Source-only disk-floor regressions. FakePVE only: no PVE, network or subprocesses.

Authored for later review/execution: python3 -m unittest discover -s images/tests -v
"""
import argparse
import contextlib
import copy
import importlib.util
import io
from pathlib import Path
import re
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("disk_image_build", Path(__file__).resolve().parents[1] / "build.py")
build = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(build)

UPID = "UPID:test-node:00000001:00000002:00000003:resize:9200:images@pve!packer:"


def arguments(os_slug="ubuntu-2604"):
    return argparse.Namespace(os=os_slug, node="test-node", storage="disk-store", import_storage="import-store",
                              bridge="build-bridge", vlan=29, build_ip="192.0.2.40/24", build_gateway="192.0.2.1",
                              nameserver="", keep=3, if_missing=False)


class FakePVE:
    def __init__(self, size="3G", result=None, fail="", change=None):
        self.size, self.result, self.fail, self.change = size, result, fail, change
        self.events = []
        # Both old tenant/system VMs and already published templates must survive every failure.
        self.rows = {vmid: {"name": "protected-%s" % vmid, "node": "test-node", "pool": "protected", "template": 0}
                     for vmid in (101, 102, 103, 110, 120)}
        for vmid in (9201, 9202):
            self.rows[vmid] = dict(name="pc-ubuntu-2604-old-%s" % vmid, node="test-node", pool=build.POOL, template=1,
                                   tags="proxcloud;pc-public;pc-img-ubuntu-2604;pc-v-old-%s" % vmid, scsi0="disk:old,size=3G")
        self.protected = copy.deepcopy(self.rows)

    def vms(self):
        self.events.append(("vms",))
        return [dict(row, vmid=vmid) for vmid, row in self.rows.items()]

    def wait(self, node, task):
        assert task, "must never wait on a synchronous null/empty response"
        self.events.append(("wait", task))
        if self.fail == "create-task" and task == "create-task":
            raise SystemExit("create task failed")
        if self.fail == "resize-task" and task == UPID:
            raise SystemExit("resize task failed")

    def destroy(self, node, vmid):
        self.events.append(("destroy", vmid))
        assert vmid not in self.protected, "must never destroy pre-existing VMs/templates"
        if self.fail == "cleanup":
            raise SystemExit("delete denied")
        self.rows.pop(vmid)

    def call(self, method, path, **params):
        self.events.append((method, path, params.copy()))
        if path == "/pools/" + build.POOL:
            return {"members": [dict(type="qemu", node=row["node"], vmid=vmid) for vmid, row in self.rows.items()
                                if row.get("pool") == build.POOL]}
        if path.endswith("/qemu"):
            vmid = params["vmid"]
            assert vmid not in self.rows
            if self.fail == "create":
                self.rows[vmid] = dict(name="foreign-race-winner", node="test-node", pool="protected")
                raise SystemExit("create conflict")
            self.rows[vmid] = dict(name="", node="test-node", pool=params["pool"], template=0)
            return "create-task"
        match = re.fullmatch(r"/nodes/test-node/qemu/(\d+)/(config|status/current|resize|template)", path)
        assert match, (method, path, params)
        vmid, endpoint = int(match[1]), match[2]
        row = self.rows[vmid]
        if endpoint == "status/current":
            return {"status": row.get("status", "stopped")}
        if endpoint == "config":
            if method == "GET":
                self.events.append(("readback", vmid, row.get("scsi0")))
                return dict(row, digest="fake-digest") if self.fail != "digest" else dict(row)
            if "scsi0" in params and self.fail == "config":
                raise SystemExit("import failed")
            row.update({key: value for key, value in params.items() if key != "delete"})
            for key in params.get("delete", "").split(","):
                row.pop(key, None)
            if "scsi0" in params:
                row["scsi0"] = "disk-store:vm-%s-disk-0,discard=on,size=%s" % (vmid, self.size)
                if self.change:
                    row.update(self.change)
            return "config-task"
        if endpoint == "resize":
            assert method == "PUT"
            assert params == {"disk": "scsi0", "size": "20G", "digest": "fake-digest"}
            assert row["template"] == 0
            if self.fail in ("resize", "cleanup"):
                raise SystemExit("resize failed")
            if self.fail != "readback":
                row["scsi0"] = "disk-store:vm-%s-disk-0,discard=on,size=20G" % vmid
            if self.fail == "disk-changed":
                row["scsi0"] = "disk-store:other-disk,size=20G"
            return self.result
        assert endpoint == "template" and method == "POST"
        assert build.disk_size_bytes(row["scsi0"]) >= 20 * 1024 ** 3
        if self.fail == "template":
            raise SystemExit("template conversion failed")
        row["template"] = 1
        return "template-task"


class DiskFloorTests(unittest.TestCase):
    def create(self, pve, os_slug="ubuntu-2604"):
        a = arguments(os_slug)
        with patch.object(build, "ensure_cloud_image", return_value="import-store:import/reviewed.qcow2"):
            return build.create_base(pve, a, build.IMAGES[a.os], "pc-tmp-%s-test-node" % a.os)

    def run_build(self, pve, os_slug="ubuntu-2604"):
        a = arguments(os_slug)

        def packer_stub(args, base, vmid, name, version):
            self.assertEqual(build.disk_size_bytes(pve.rows[base]["scsi0"]), max(build.disk_size_bytes("disk,size=" + pve.size), 20 * 1024 ** 3))
            self.assertEqual(pve.rows[base]["template"], 1)
            pve.rows[vmid] = dict(pve.rows[base], name=name, tags="pc-building", template=1)
            pve.events.append(("packer", vmid))

        def smoke_stub(*args):
            pve.events.append(("smoke",))

        with patch.object(build, "ensure_cloud_image", return_value="import-store:import/reviewed.qcow2"), \
             patch.object(build, "packer", side_effect=packer_stub) as packer, \
             patch.object(build, "smoke_test", side_effect=smoke_stub), contextlib.redirect_stdout(io.StringIO()):
            build.cmd_build(pve, a, build.IMAGES[a.os])
        return packer

    def test_exact_byte_units_and_fractional_gib(self):
        for value, expected in (("3G", 3 << 30), ("20G", 20 << 30), ("20480M", 20 << 30),
                                ("20971520K", 20 << 30), ("21474836480", 20 << 30), ("1T", 1 << 40),
                                ("20.5G", (20 << 30) + (1 << 29)), ("20971519K", (20 << 30) - 1024)):
            with self.subTest(size=value):
                self.assertEqual(build.disk_size_bytes("disk:root,discard=on,size=" + value), expected)
        for raw in ("", "disk:root", "disk:size=20G", "disk,size=0G", "disk,size=-1G", "disk,size=20GiB",
                    "disk,size=NaN", "disk,size=1e3", "disk,size=20G,size=20G", "disk,size=0.1"):
            with self.subTest(raw=raw), self.assertRaises(SystemExit):
                build.disk_size_bytes(raw)

    def test_every_os_floor_readback_precedes_template_and_packer(self):
        self.assertEqual(build.DEFAULT_BUILD_DISK_GIB, 20)
        self.assertEqual(set(build.IMAGES), {"debian-12", "debian-13", "ubuntu-2404", "ubuntu-2604"})
        for os_slug in build.IMAGES:
            with self.subTest(os=os_slug):
                pve = FakePVE()
                self.run_build(pve, os_slug)
                imported = next(i for i, e in enumerate(pve.events) if e[0] == "POST" and "scsi0" in e[2])
                resized = next(i for i, e in enumerate(pve.events) if e[0] == "PUT")
                readback = next(i for i, e in enumerate(pve.events) if e[0] == "readback" and e[2].endswith("size=20G"))
                template = next(i for i, e in enumerate(pve.events) if e[0] == "POST" and e[1].endswith("/template"))
                packer = next(i for i, e in enumerate(pve.events) if e[0] == "packer")
                smoke = next(i for i, e in enumerate(pve.events) if e[0] == "smoke")
                published = next(i for i, e in enumerate(pve.events) if e[0] == "POST" and "pc-public" in e[2].get("tags", ""))
                self.assertLess(imported, resized)
                self.assertLess(resized, readback)
                self.assertLess(readback, template)
                self.assertLess(template, packer)
                self.assertLess(packer, smoke)
                self.assertLess(smoke, published)
                self.assertEqual({vmid: pve.rows[vmid] for vmid in pve.protected}, pve.protected)
                self.assertFalse(any(e[0] == "POST" and e[1].endswith("/status/start") for e in pve.events))

    def test_at_or_above_floor_never_resets_or_shrinks(self):
        for size in ("20G", "20480M", "21474836480", "20.5G", "40G", "1T"):
            with self.subTest(size=size):
                pve = FakePVE(size=size)
                vmid = self.create(pve)
                self.assertTrue(pve.rows[vmid]["scsi0"].endswith("size=" + size))
                self.assertFalse(any(e[0] == "PUT" for e in pve.events))

    def test_unreadable_import_size_fails_before_resize_and_conversion(self):
        for size in ("", "0G", "garbage", "20G,size=30G"):
            with self.subTest(size=size):
                pve = FakePVE(size=size)
                with self.assertRaises(SystemExit):
                    self.create(pve)
                self.assertEqual(pve.rows, pve.protected)
                self.assertFalse(any(e[0] == "PUT" or (e[0] == "POST" and e[1].endswith("/template")) for e in pve.events))

    def test_one_kib_below_floor_is_not_rounded_up(self):
        pve = FakePVE(size="20971519K")
        self.create(pve)
        self.assertEqual(sum(e[0] == "PUT" for e in pve.events), 1)

    def test_synchronous_null_or_empty_never_waits_but_still_reads_back(self):
        for result in (None, ""):
            with self.subTest(result=result):
                pve = FakePVE(result=result)
                self.create(pve)
                self.assertNotIn(("wait", result), pve.events)
                self.assertIn(("readback", 9200, "disk-store:vm-9200-disk-0,discard=on,size=20G"), pve.events)

    def test_valid_async_upid_is_waited_before_readback(self):
        pve = FakePVE(result=UPID)
        self.create(pve)
        waited = pve.events.index(("wait", UPID))
        readback = pve.events.index(("readback", 9200, "disk-store:vm-9200-disk-0,discard=on,size=20G"))
        self.assertLess(waited, readback)

    def test_bad_resize_response_fails_without_wait_or_publication(self):
        for result in ("ok", "UPID:", UPID.replace("test-node", "other-node"), UPID.replace(":9200:", ":101:"), 1, {}):
            with self.subTest(result=result):
                pve = FakePVE(result=result)
                with self.assertRaisesRegex(SystemExit, "invalid task ID"):
                    self.run_build(pve)
                self.assertNotIn(("wait", result), pve.events)
                self.assertNotIn("packer", [e[0] for e in pve.events])
                self.assertFalse(any(e[0] == "POST" and e[1].endswith("/template") for e in pve.events))

    def test_resize_import_or_readback_failure_cleans_only_exact_new_base(self):
        for fail, result in (("config", None), ("digest", None), ("resize", None), ("resize-task", UPID),
                             ("readback", None), ("readback", UPID), ("disk-changed", None)):
            with self.subTest(fail=fail, result=result):
                pve = FakePVE(fail=fail, result=result)
                with self.assertRaises(SystemExit):
                    self.run_build(pve)
                self.assertEqual(pve.rows, pve.protected)
                self.assertEqual([e for e in pve.events if e[0] == "destroy"], [("destroy", 9200)])
                self.assertNotIn("packer", [e[0] for e in pve.events])
                self.assertFalse(any(e[0] == "POST" and (e[1].endswith("/template") or "pc-public" in e[2].get("tags", "")) for e in pve.events))

    def test_identity_pool_node_template_lock_or_power_change_refuses_mutation(self):
        for change in ({"pool": "protected"}, {"node": "other-node"}, {"name": "foreign"}, {"template": 1},
                       {"tags": "proxcloud;pc-public"}, {"lock": "backup"}, {"status": "running"}):
            with self.subTest(change=change):
                pve = FakePVE(change=change)
                with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
                    self.create(pve)
                self.assertFalse(any(e[0] in ("PUT", "destroy") for e in pve.events))
                self.assertEqual({vmid: pve.rows[vmid] for vmid in pve.protected}, pve.protected)

    def test_unconfirmed_create_failure_never_claims_or_deletes_race_winner(self):
        pve = FakePVE(fail="create")
        with self.assertRaisesRegex(SystemExit, "create conflict"):
            self.create(pve)
        self.assertEqual(pve.rows[9200]["name"], "foreign-race-winner")
        self.assertFalse(any(e[0] in ("PUT", "destroy") for e in pve.events))

    def test_unconfirmed_create_task_never_cleans_or_configures(self):
        pve = FakePVE(fail="create-task")
        with self.assertRaisesRegex(SystemExit, "create task failed"):
            self.create(pve)
        self.assertFalse(any(e[0] in ("PUT", "destroy") or (e[0] == "POST" and e[1].endswith("/config")) for e in pve.events))

    def test_cleanup_failure_preserves_original_error_and_warns_exact_id(self):
        pve = FakePVE(fail="cleanup")
        with contextlib.redirect_stderr(io.StringIO()) as warning, self.assertRaisesRegex(SystemExit, "resize failed"):
            self.create(pve)
        self.assertIn("newly created VM 9200: delete denied", warning.getvalue())
        self.assertEqual([e for e in pve.events if e[0] == "destroy"], [("destroy", 9200)])
        self.assertEqual({vmid: pve.rows[vmid] for vmid in pve.protected}, pve.protected)

    def test_conversion_failure_cleans_only_unconverted_new_vm(self):
        pve = FakePVE(fail="template")
        with self.assertRaisesRegex(SystemExit, "template conversion failed"):
            self.create(pve)
        self.assertEqual(pve.rows, pve.protected)
        self.assertEqual([e for e in pve.events if e[0] == "destroy"], [("destroy", 9200)])

    def test_existing_cmd_base_reuse_is_never_resized(self):
        pve = FakePVE()
        pve.rows[9201].update(name="pc-base-ubuntu-2604-test-node", scsi0="disk:old,size=3G")
        before = copy.deepcopy(pve.rows)
        with patch.object(build, "ensure_cloud_image") as download, contextlib.redirect_stdout(io.StringIO()) as output:
            build.cmd_base(pve, arguments(), build.IMAGES["ubuntu-2604"])
        self.assertEqual(output.getvalue().strip(), "9201")
        download.assert_not_called()
        self.assertEqual(pve.rows, before)
        self.assertEqual(pve.events, [("vms",)])


if __name__ == "__main__":
    unittest.main()
