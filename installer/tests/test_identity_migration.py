#!/usr/bin/env python3
"""Stage 10 against a fake Proxmox API on localhost (mode: split): a new site ends with the narrow mint grant, and a
site an earlier installer set up is moved to it without a moment where the portal token cannot create a token for
capmox@pve or csi@pve. The fake works out effective privileges the way PVE::AccessControl::roles does (the most specific
row wins, user ∩ token) and the token check the way PVE::RPCEnvironment 'userid-group' does. No Proxmox.

Run with the installer venv: python installer/tests/test_identity_migration.py
"""
import json
import subprocess
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlparse

INSTALLER = Path(__file__).resolve().parent.parent
PORTAL, TOKEN = "proxcloud@pve", "proxcloud@pve!platform"
BUILTIN = {
    "NoAccess": set(),
    "PVEAuditor": {"Datastore.Audit", "Sys.Audit", "VM.Audit"},
    "PVETemplateUser": {"VM.Audit", "VM.Clone"},
    "PVEDatastoreAdmin": {"Datastore.Allocate", "Datastore.AllocateSpace", "Datastore.AllocateTemplate", "Datastore.Audit"},
}


class PVE:
    def __init__(self):
        self.roles = {k: set(v) for k, v in BUILTIN.items()}
        self.users, self.groups, self.pools, self.acl = {}, {}, [], []  # users: id → token names; groups: id → members
        self.log = []  # (write, could the portal token create a token for each service user afterwards)

    def privs(self, ugid, path):
        if "!" in ugid and ugid.split("!")[1] not in self.users.get(ugid.split("!")[0], []):
            return set()
        parts, found = [p for p in path.split("/") if p], {}
        for i in range(len(parts) + 1):
            at = "/" + "/".join(parts[:i])
            new = {r["roleid"] for r in self.acl if r["path"] == at and r["ugid"] == ugid and (i == len(parts) or r["propagate"])}
            found = new or found
        out = set() if "NoAccess" in found else set().union(*[self.roles[r] for r in found])
        return out & self.privs(ugid.split("!")[0], path) if "!" in ugid else out

    def can_mint(self, token, user):
        return "User.Modify" in self.privs(token, "/access/groups") or any(
            user in members and "User.Modify" in self.privs(token, "/access/groups/" + g) for g, members in self.groups.items())

    def get(self, path, q):
        if path == "storage":
            return [{"storage": "local", "type": "dir", "content": "iso,import"}, {"storage": "local-zfs", "type": "zfspool", "content": "images"}]
        if path == "nodes":
            return [{"node": "pve1", "status": "online"}]
        if path == "access/roles":
            return [{"roleid": k, "privs": ",".join(sorted(v))} for k, v in self.roles.items()]
        if path == "access/users":
            return [{"userid": u, "tokens": [{"tokenid": t} for t in toks], "groups": ",".join(g for g, m in self.groups.items() if u in m)}
                    for u, toks in self.users.items()]
        if path == "pools":
            return [{"poolid": p} for p in self.pools]
        if path == "access/acl":
            return self.acl
        if path == "access/groups":
            return [{"groupid": g} for g in self.groups]
        if path == "access/permissions":
            return {q["path"]: {p: 1 for p in self.privs(q["userid"], q["path"])}}
        raise KeyError(path)

    def write(self, method, path, f):
        seg = path.split("/")
        if (method, path) == ("POST", "access/roles"):
            self.roles[f["roleid"]] = set(f["privs"].split(","))
        elif method == "PUT" and path.startswith("access/roles/"):
            self.roles[seg[2]] = set(f["privs"].split(","))
        elif (method, path) == ("POST", "pools"):
            self.pools.append(f["poolid"])
        elif (method, path) == ("POST", "access/users"):
            self.users[f["userid"]] = []
        elif (method, path) == ("POST", "access/groups"):
            self.groups[f["groupid"]] = set()
        elif method == "PUT" and path.startswith("access/users/"):
            assert f.get("append") == "1", "a user's other groups must stay"
            self.groups[f["groups"]].add(seg[2])
        elif method == "POST" and len(seg) == 5 and seg[3] == "token":
            self.users[seg[2]].append(seg[4])
            return {"value": "secret"}
        elif (method, path) == ("PUT", "access/acl"):
            for kind in ("users", "tokens"):
                for ugid in filter(None, f.get(kind, "").split(",")):
                    for role in f["roles"].split(","):
                        self.acl = [r for r in self.acl if (r["path"], r["ugid"], r["roleid"]) != (f["path"], ugid, role)]
                        if f.get("delete") != "1":
                            self.acl.append({"path": f["path"], "type": kind[:-1], "ugid": ugid, "roleid": role, "propagate": int(f.get("propagate", 1))})
        else:
            raise KeyError(method + " " + path)
        self.log.append((method + " " + path + " " + json.dumps(f, sort_keys=True),
                         all(self.can_mint(TOKEN, u) for u in ("capmox@pve", "csi@pve"))))


def serve(pve):
    class H(BaseHTTPRequestHandler):
        def do(self):
            u = urlparse(self.path)
            path = u.path.removeprefix("/api2/json/")
            flat = lambda s: {k: v[0] for k, v in parse_qs(s).items()}  # noqa: E731
            try:
                if self.command == "GET":
                    data = pve.get(path, flat(u.query))
                else:
                    data = pve.write(self.command, path, flat(self.rfile.read(int(self.headers.get("Content-Length") or 0)).decode()))
            except Exception as e:  # an endpoint the fake lacks fails the play
                self.send_response(501)
                self.end_headers()
                self.wfile.write(repr(e).encode())
                return
            body = json.dumps({"data": data}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        do_GET = do_POST = do_PUT = do
        log_message = lambda *a: None  # noqa: E731
    srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv


def stage10(pve, d):
    """Runs stage 10 for the site directory d, which keeps generated/ (the token secrets) between runs."""
    srv = serve(pve)
    try:
        site = Path(d)
        (site / "generated").mkdir(exist_ok=True)
        (site / "hosts.yml").write_text("all: {hosts: {localhost: {ansible_connection: local}}}\n")
        extra = {"pc_site_dir": d, "proxcloud": {"mode": "split", "pve": {"api_url": "http://127.0.0.1:%d" % srv.server_port}},
                 "proxcloud_vault": {"pve_token_id": "root@pam!installer", "pve_token_secret": "x"}}
        return subprocess.run([str(Path(sys.executable).parent / "ansible-playbook"), "-i", str(site / "hosts.yml"),
                               "-e", json.dumps(extra), "playbooks/10-pve-identity.yml"],
                              cwd=INSTALLER, capture_output=True, text=True, env={"ANSIBLE_CONFIG": str(INSTALLER / "ansible.cfg"), "PATH": "/usr/bin:/bin", "HOME": d})
    finally:
        srv.shutdown()
        srv.server_close()


def older(pve, *more):
    """Turns a site this stage set up into one an earlier installer did: the mint role with User.Modify, on
    /access/groups and /pool for the portal's user and token (and for more), and no group."""
    pve.roles["ProxCloudKubeMint"] = {"User.Modify", "Permissions.Modify"}
    pve.groups, pve.log = {}, []
    pve.acl = [r for r in pve.acl if not r["path"].startswith("/access/")]
    for ugid in (PORTAL, TOKEN) + more:
        for path in ("/access/groups", "/pool"):
            pve.acl.append({"path": path, "type": "token" if "!" in ugid else "user", "ugid": ugid, "roleid": "ProxCloudKubeMint", "propagate": 1})


class IdentityMigration(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.site = tmp.name

    def narrow(self, pve):
        for path in ("/", "/access", "/access/groups", "/pool"):
            self.assertFalse({"User.Modify", "Permissions.Modify"} & pve.privs(TOKEN, path), path)
        self.assertTrue(pve.can_mint(TOKEN, "capmox@pve") and pve.can_mint(TOKEN, "csi@pve"))
        for user in ("root@pam", PORTAL, "images@pve"):
            self.assertFalse(pve.can_mint(TOKEN, user), user)
        # A cluster token's pool rows pass on Pool.Allocate and the role's own privileges (PVE::API2::ACL).
        self.assertLessEqual({"Pool.Allocate"} | pve.roles["ProxCloudCAPI"], pve.privs(TOKEN, "/pool/pc-p1"))

    def test_new_site_then_an_older_one(self):
        pve = PVE()
        r = stage10(pve, self.site)
        self.assertEqual(r.returncode, 0, r.stdout[-3000:] + r.stderr)
        self.narrow(pve)
        self.assertEqual(pve.roles["ProxCloudKubeMint"], {"Permissions.Modify"})

        older(pve)
        self.assertTrue(pve.can_mint(TOKEN, "root@pam"))  # what the lab measured
        r = stage10(pve, self.site)
        self.assertEqual(r.returncode, 0, r.stdout[-3000:] + r.stderr)
        self.assertEqual([w for w, ok in pve.log if not ok], [], "the portal token could not create cluster tokens after these writes")
        self.narrow(pve)
        self.assertEqual(pve.roles["ProxCloudKubeMint"], {"Permissions.Modify"})
        self.assertNotIn("NOT FINISHED", r.stdout)

    def test_a_row_on_a_single_store_or_zone_gets_the_mint_role(self):
        """Production, 2026-11-17: older rows of the portal on single stores hid the mint role /storage passes down, and
        PVE refused every tenant cluster's token its row there."""
        pve = PVE()
        self.assertEqual(stage10(pve, self.site).returncode, 0)
        row = lambda path, ugid, role, prop: {"path": path, "type": "token" if "!" in ugid else "user", "ugid": ugid, "roleid": role, "propagate": prop}  # noqa: E731
        pve.acl += [row("/storage/nvme-u2", PORTAL, "ProxCloudPlatform", 1), row("/storage/nvme-u2", TOKEN, "ProxCloudPlatform", 1),
                    row("/storage/nvme", PORTAL, "PVEAuditor", 0),  # the user alone: the token has no row of its own there
                    row("/sdn/zones/lan", PORTAL, "ProxCloudPlatform", 0), row("/sdn/zones/lan", TOKEN, "ProxCloudPlatform", 0)]
        self.assertNotIn("Permissions.Modify", pve.privs(TOKEN, "/storage/nvme-u2"))  # the defect
        self.assertNotIn("Permissions.Modify", pve.privs(TOKEN, "/storage/nvme"))
        before = len(pve.acl)
        r = stage10(pve, self.site)
        self.assertEqual(r.returncode, 0, r.stdout[-3000:] + r.stderr)
        for path in ("/storage/nvme-u2", "/storage/nvme", "/sdn/zones/lan", "/storage/other", "/sdn/zones/pc/vnet1"):
            self.assertIn("Permissions.Modify", pve.privs(TOKEN, path), path)
        # Nothing the rows gave is lost, and nothing is added where no row hid the role.
        self.assertIn("Datastore.AllocateSpace", pve.privs(TOKEN, "/storage/nvme-u2"))
        self.assertIn("SDN.Use", pve.privs(TOKEN, "/sdn/zones/lan/vnet1"))  # still from /sdn/zones: the zone's rows do not propagate
        added = sorted((x["path"], x["ugid"], x["propagate"]) for x in pve.acl[before:])
        self.assertEqual(added, [("/sdn/zones/lan", PORTAL, 0), ("/sdn/zones/lan", TOKEN, 0), ("/storage/nvme", PORTAL, 0),
                                 ("/storage/nvme-u2", PORTAL, 1), ("/storage/nvme-u2", TOKEN, 1)])
        self.assertTrue(all(x["roleid"] == "ProxCloudKubeMint" for x in pve.acl[before:]))
        # A second run writes nothing more.
        r = stage10(pve, self.site)
        self.assertEqual(r.returncode, 0, r.stdout[-3000:] + r.stderr)
        self.assertEqual(len(pve.acl), before + 5)

    def test_a_run_that_stops_half_way_leaves_minting_on(self):
        pve = PVE()
        self.assertEqual(stage10(pve, self.site).returncode, 0)
        older(pve)
        (Path(self.site) / "generated" / "token-gateway").unlink()  # "Check the tokens the installer needs are known" fails
        r = stage10(pve, self.site)
        self.assertNotEqual(r.returncode, 0)
        self.assertEqual([w for w, ok in pve.log if not ok], [])
        self.assertTrue(pve.can_mint(TOKEN, "capmox@pve") and pve.can_mint(TOKEN, "csi@pve"))

    def test_another_portal_token_keeps_minting(self):
        pve = PVE()
        self.assertEqual(stage10(pve, self.site).returncode, 0)
        pve.users[PORTAL].append("m0")
        older(pve, PORTAL + "!m0")
        r = stage10(pve, self.site)
        self.assertEqual(r.returncode, 0, r.stdout[-3000:] + r.stderr)
        self.narrow(pve)
        # The role is the cluster's: it keeps User.Modify while the other token's row needs it, and the run says so.
        self.assertIn("User.Modify", pve.roles["ProxCloudKubeMint"])
        self.assertTrue(pve.can_mint(PORTAL + "!m0", "capmox@pve"))
        self.assertFalse(pve.can_mint(PORTAL + "!m0", "root@pam"))  # its user's row on /access/groups is gone
        self.assertRegex(r.stdout, r"NOT FINISHED: ProxCloudKubeMint keeps User.Modify, because \[proxcloud@pve!m0\]")
        # Once that token's row is gone (its own script run), the next run finishes.
        pve.acl = [x for x in pve.acl if x["ugid"] != PORTAL + "!m0"]
        r = stage10(pve, self.site)
        self.assertEqual(r.returncode, 0, r.stdout[-3000:] + r.stderr)
        self.assertEqual(pve.roles["ProxCloudKubeMint"], {"Permissions.Modify"})
        self.narrow(pve)


if __name__ == "__main__":
    unittest.main()
