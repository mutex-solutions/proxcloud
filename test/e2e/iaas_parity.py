#!/usr/bin/env python3
"""Live end-to-end check of the IaaS parity actions on a real Proxmox cluster, through the API.

Creates a network and servers in an existing project, asserts, and removes what it made, on any exit.
Inside-the-guest checks run with `qm guest exec` when E2E_SSH is set.

  E2E_API=https://api.example.com/api/v1 E2E_TOKEN=<bearer or pk.sk> E2E_PROJECT=<id> \
  E2E_FLAVOR=<slug> E2E_IMAGE=<slug> E2E_IMAGE2=<slug> \
  E2E_SSH="ssh root@{node}"  python3 test/e2e/iaas_parity.py

Optional: E2E_PUBLIC_NETWORK (attach a public IP), E2E_PASSWORD_PRIV=1 (the token holds
VM.GuestAgent.Unrestricted), E2E_ISO_PRIV=1 (it holds Datastore.AllocateTemplate: user-data on a vnet
without DHCP).
"""
import json
import os
import shlex
import subprocess
import time
import urllib.error
import urllib.request

API = os.environ["E2E_API"].rstrip("/")
TOKEN = os.environ["E2E_TOKEN"]
PID = os.environ["E2E_PROJECT"]
FLAVOR = os.environ["E2E_FLAVOR"]
IMAGE = os.environ["E2E_IMAGE"]
IMAGE2 = os.environ["E2E_IMAGE2"]
SSH = os.environ.get("E2E_SSH", "")  # a command template, {node} = the VM's node
PUBLIC = os.environ.get("E2E_PUBLIC_NETWORK", "")
PASSWORD_PRIV = os.environ.get("E2E_PASSWORD_PRIV") == "1"
ISO_PRIV = os.environ.get("E2E_ISO_PRIV") == "1"
PREFIX = "e2e-" + hex(int(time.time()))[-5:]

made = []
failures = []


def call(method, path, body=None):
    req = urllib.request.Request(
        API + path, method=method, data=json.dumps(body).encode() if body is not None else None,
        headers={"Authorization": "Bearer " + TOKEN, "Content-Type": "application/json"},
    )
    try:
        res = urllib.request.urlopen(req, timeout=900)
        raw, status = res.read(), res.status
    except urllib.error.HTTPError as e:
        raw, status = e.read(), e.code
    try:
        doc = json.loads(raw) if raw else {}
    except ValueError:
        doc = {}
    return status, doc.get("data", doc) if isinstance(doc, dict) else doc


def check(name, ok, detail=""):
    print(("ok   " if ok else "FAIL ") + name + (f"  [{detail}]" if detail and not ok else ""), flush=True)
    if not ok:
        failures.append(name)


def create(typ, **data):
    status, row = call("POST", f"/project/{PID}/cloud", {"type": typ, "data": data})
    if status != 200:
        raise SystemExit(f"create {typ} {data.get('name')}: {status} {row}")
    made.append(row["id"])
    return row


def action(rid, verb, **data):
    return call("POST", f"/project/{PID}/cloud/{rid}/action", {"action": verb, "data": data})


def server(name, **kw):
    return create("SERVER", name=f"{PREFIX}-{name}", flavorId=FLAVOR, imageId=IMAGE, **kw)


def node_of(row):
    _, cur = call("GET", f"/project/{PID}/cloud/{row['id']}")
    return cur["data"]["server"]["host"]


def guest(row, cmd, timeout=240):
    """Output of cmd in the guest, waiting for its agent."""
    vmid, node = row["externalId"], node_of(row)
    end = time.time() + timeout
    ssh = shlex.split(SSH.format(node=node))
    while time.time() < end:
        r = subprocess.run(ssh + [f"qm guest cmd {vmid} ping"], capture_output=True, text=True)
        if r.returncode == 0:
            break
        time.sleep(5)
    r = subprocess.run(ssh + [f"qm guest exec {vmid} --timeout 60 -- sh -c {shlex.quote(cmd)}"], capture_output=True, text=True)
    try:
        d = json.loads(r.stdout)
        return (d.get("out-data", "") + d.get("err-data", "")).strip()
    except ValueError:
        return "guest exec failed: " + r.stdout + r.stderr


def run():
    net = create("NETWORK", name=f"{PREFIX}-net")
    netid = net["id"]

    # Subnets: a second one, SNAT switch, a server on it, refusals.
    st, _ = action(netid, "ADD_SUBNET")
    check("add a subnet", st == 200, st)
    _, cur = call("GET", f"/project/{PID}/cloud/{netid}")
    details = cur["data"]["network"]["subnetDetails"]
    check("network lists two subnets", len(details) == 2, details)
    primary, second = cur["data"]["network"]["cidr"], [d["cidr"] for d in details if d["cidr"] != cur["data"]["network"]["cidr"]][0]
    st, _ = action(netid, "SET_SNAT", cidr=second, snat=False)
    _, cur = call("GET", f"/project/{PID}/cloud/{netid}")
    check("SNAT off on the second subnet", [d["snat"] for d in cur["data"]["network"]["subnetDetails"] if d["cidr"] == second] == [False], cur)
    a = server("a", networkInterfaces=[{"uuid": netid, "subnet": second}], spreadGroup="e2e", adminPass="E2e-pass-1234")
    st, body = action(netid, "REMOVE_SUBNET", cidr=second)
    check("a subnet in use cannot go", st == 409, (st, body))
    st, _ = action(netid, "REMOVE_SUBNET", cidr=primary)
    check("an unused subnet can go", st == 200, st)
    st, _ = action(netid, "REMOVE_SUBNET", cidr=second)
    check("the last subnet cannot go", st == 409, st)

    # Spread: as many members as nodes are apart; a strict one more is refused or lands alone.
    b = server("b", networkIds=[netid], spreadGroup="e2e", adminPass="E2e-pass-1234")
    check("a spread group's second member takes another node", node_of(a) != node_of(b), (node_of(a), node_of(b)))

    # Snapshots on the first server.
    if SSH:
        guest(a, "echo before > /var/tmp/e2e; sync")
    st, _ = action(a["id"], "CREATE_SNAPSHOT", name="s1")
    check("create a snapshot", st == 200, st)
    if SSH:
        guest(a, "echo after > /var/tmp/e2e; sync")
    action(a["id"], "CREATE_SNAPSHOT", name="s2")
    st, res = action(a["id"], "LIST_SNAPSHOTS")
    names = [s["name"] for s in res["result"]]
    check("lists both, newest first", names == ["s2", "s1"], names)
    st, _ = action(a["id"], "ROLLBACK_SNAPSHOT", name="s1")
    check("an older snapshot is refused on ZFS", st == 409, st)
    st, _ = action(a["id"], "REBUILD", imageId=IMAGE2)
    check("rebuild with snapshots is refused", st == 409, st)
    action(a["id"], "DELETE_SNAPSHOT", name="s2")
    st, _ = action(a["id"], "ROLLBACK_SNAPSHOT", name="s1")
    check("roll back to the newest", st == 200, st)
    if SSH:
        check("the disk is back", guest(a, "cat /var/tmp/e2e") == "before")
    action(a["id"], "DELETE_SNAPSHOT", name="s1")

    # Backups, when the provider has a store: E2E_BACKUP=1 (needs VM.Backup on the token).
    if os.environ.get("E2E_BACKUP") == "1":
        if SSH:
            guest(a, "echo before-backup > /var/tmp/e2e; sync")
        st, _ = action(a["id"], "CREATE_BACKUP")
        check("create a backup", st == 200, st)
        st, res = action(a["id"], "LIST_BACKUPS")
        ids = [b["id"] for b in res["result"]]
        check("lists it", len(ids) == 1, ids)
        st, _ = action(a["id"], "DELETE_BACKUP", id="nas-backup:backup/not-yours")
        check("an archive not listed is refused", st == 409, st)
        if SSH:
            guest(a, "echo after-backup > /var/tmp/e2e; sync")
        st, _ = action(a["id"], "RESTORE_BACKUP", id=ids[0])
        check("restore", st == 200, st)
        if SSH:
            check("the disk is back", guest(a, "cat /var/tmp/e2e") == "before-backup")
        st, _ = action(a["id"], "DELETE_BACKUP", id=ids[0])
        check("delete it", st == 200, st)

    # Rebuild keeps the identity.
    _, before = call("GET", f"/project/{PID}/cloud/{a['id']}")
    st, res = action(a["id"], "REBUILD", imageId=IMAGE2)
    check("rebuild", st == 200, (st, res))
    _, after = call("GET", f"/project/{PID}/cloud/{a['id']}")
    check("same server, new image", after["externalId"] == before["externalId"] and after["data"]["server"]["image"]["id"] == IMAGE2, after["data"]["server"].get("image"))
    if SSH:
        check("the guest runs the new image, marker gone", "cannot" in guest(a, "cat /var/tmp/e2e") or "No such" in guest(a, "cat /var/tmp/e2e"))

    # Password, public IP.
    st, res = action(b["id"], "SET_PASSWORD", password="Another-pass-1")
    check("set password" if PASSWORD_PRIV else "set password says which privilege is missing", st == (200 if PASSWORD_PRIV else 409), (st, res))
    if PUBLIC:
        st, res = action(b["id"], "ATTACH_PUBLIC_NETWORK", networkId=PUBLIC)
        check("attach a public IP", st == 200 and res["result"].get("rebootRequired"), (st, res))
        st, res = action(b["id"], "DETACH_PUBLIC_NETWORK")
        check("detach it", st == 200, (st, res))

    # User-data on a vnet without DHCP needs the seed CD.
    st, res = call("POST", f"/project/{PID}/cloud", {"type": "SERVER", "data": {
        "name": f"{PREFIX}-seed", "flavorId": FLAVOR, "imageId": IMAGE, "networkIds": [netid],
        "userData": "#cloud-config\nwrite_files:\n  - path: /var/tmp/e2e-ud\n    content: from-seed\n",
        "metadata": {"env": "e2e"},
    }})
    if ISO_PRIV:
        check("server with user-data", st == 200, (st, res))
        made.append(res["id"])
        if SSH:
            check("user-data ran", guest(res, "cat /var/tmp/e2e-ud") == "from-seed")
    else:
        check("user-data without the privilege says so", st == 409 and "Datastore.AllocateTemplate" in json.dumps(res), (st, res))


def cleanup():
    for rid in reversed(made):
        st, _ = call("DELETE", f"/project/{PID}/cloud/{rid}")
        print(f"cleanup {rid}: {st}", flush=True)


try:
    run()
finally:
    cleanup()
print(f"\n{len(failures)} failed" if failures else "\nall passed")
raise SystemExit(1 if failures else 0)
