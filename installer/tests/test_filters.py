#!/usr/bin/env python3
"""Runnable check of the installer's decisions: python3 installer/tests/test_filters.py"""
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "filter_plugins"))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "library"))
import proxcloud as f  # noqa: E402
import proxcloud_api as api  # noqa: E402

STD = [
    {"storage": "local", "type": "dir", "content": "backup,iso,vztmpl,import"},
    {"storage": "local-lvm", "type": "lvmthin", "content": "images,rootdir"},
    {"storage": "local-zfs", "type": "zfspool", "content": "rootdir,images"},
    {"storage": "nas-backup", "type": "nfs", "content": "backup"},
    {"storage": "nas-capacity", "type": "nfs", "content": "images"},
    {"storage": "nas-templates", "type": "nfs", "content": "iso,import,images"},
]
c = f.classify_storages(STD)
assert c["tier"] == "standard"
assert set(c["profiles"]) == {"std", "local-lvm", "capacity"}, c["profiles"]
assert c["profiles"]["std"]["storages"] == {"*": "local-zfs"}
assert c["profiles"]["capacity"]["storages"] == {"*": "nas-capacity"}
assert c["templates"] == ["nas-templates"] and c["backups"] == ["local", "nas-backup"]
assert c["imports"] == ["local", "nas-templates"]
assert f.default_profile(c["profiles"]) == "std" and f.default_profile(c["profiles"], "capacity") == "capacity"

c = f.classify_storages([{"storage": "ceph", "type": "rbd", "content": "images"}])
assert c["tier"] == "full" and c["profiles"]["ceph"]["shared"]
assert f.default_profile(c["profiles"]) == "ceph"

assert f.fio_class(0.5, 50000) == "performance"
assert f.fio_class(3, 50000) == "standard"
assert f.fio_class(9, 50000) == "capacity"
assert f.fio_class(0.5, 100) == "capacity"  # IOPS can only downgrade
assert f.fio_class(1.5, 900, 2, 5, 500) == "performance"  # thresholds are knobs

# A re-run keeps what the operator set on the provider, adds what is new, and a site value wins.
assert f.kept_profiles({"a": 1, "new": 2}, {}, {"a": 9}) == {"a": 9, "new": 2}
assert f.kept_profiles({"a": 1}, {"site": 3}, {"a": 9}) == {"site": 3}
assert f.kept_default_profile({"std": 1, "ceph": 2}, "ceph", "", "std") == "std"  # Ceph added later moves nothing
assert f.kept_default_profile({"std": 1, "ceph": 2}, "ceph", "", "") == "ceph"  # first run
assert f.kept_default_profile({"ceph": 2}, "ceph", "", "std") == "ceph"  # its profile is gone
assert f.kept_default_profile({"std": 1, "ceph": 2}, "ceph", "std", "ceph") == "std"
stored = {"a": {"class": "standard", "suggestedClass": "capacity"}}
assert f.storage_classes({"a": {"class": "performance"}, "b": {"class": "capacity"}}, {}, stored) == {
    "a": {"class": "standard", "suggestedClass": "performance"}, "b": {"suggestedClass": "capacity"}}
assert f.storage_classes({}, {"a": "capacity"}, stored)["a"]["class"] == "capacity"
assert stored["a"]["class"] == "standard"  # input untouched: the first run's `provider` is rendered again later

assert f.template_nodes(["b", "a"], False) == ["a", "b"]
assert f.template_nodes(["b", "a"], True) == ["a"]
assert f.template_nodes(["b", "a"], True, True) == ["a", "b"]
assert f.template_nodes(["b", "a"], False, False) == ["a"]

want = [{"path": "/pool", "role": "R", "propagate": 1, "user": "u@pve", "token": "u@pve!t"}]
rows = [{"path": "/pool", "roleid": "R", "type": "user", "ugid": "u@pve", "propagate": 1}]
assert f.pve_acl_missing(rows, want) == [{"path": "/pool", "roles": "R", "propagate": 1, "tokens": "u@pve!t"}]
rows.append({"path": "/pool", "roleid": "R", "type": "token", "ugid": "u@pve!t", "propagate": 1})
assert f.pve_acl_missing(rows, want) == []
# An entry for the user alone, or the token alone.
assert f.pve_acl_missing([], [{"path": "/s", "role": "R", "propagate": 1, "user": "u@pve", "token": ""}]) == [{"path": "/s", "roles": "R", "propagate": 1, "users": "u@pve"}]
assert f.pve_acl_missing([], [{"path": "/s", "role": "R", "propagate": 0, "user": "", "token": "u@pve!t"}]) == [{"path": "/s", "roles": "R", "propagate": 0, "tokens": "u@pve!t"}]

# Bootstrap ISOs hold kubeadm join tokens: node-local only, and the only iso store the CAPMOX token sees.
assert f.boot_iso_problem(STD, "local", "local-zfs") == ""
assert "is shared" in f.boot_iso_problem(STD, "nas-templates", "local-zfs")
assert "is shared" in f.boot_iso_problem(STD + [{"storage": "d", "type": "dir", "content": "iso", "shared": 1}], "d", "local-zfs")
assert "does not take ISO" in f.boot_iso_problem(STD, "local-zfs", "local-zfs")
assert "does not exist" in f.boot_iso_problem(STD, "gone", "local-zfs")
assert "on either" in f.boot_iso_problem(STD, "local", "nas-templates")
assert f.iso_stores(STD + [{"storage": "off", "type": "dir", "content": "iso", "disable": 1}]) == [s["storage"] for s in STD if "iso" in s.get("content", "")]
# Stage 10's check: a store is in the token's view with Datastore.Audit or AllocateSpace there, as PVE answers it.
res = [{"item": "local", "json": {"data": {"/storage/local": {"Datastore.Audit": 1, "Datastore.AllocateTemplate": 1}}}},
       {"item": "nas-templates", "json": {"data": {"/storage/nas-templates": {}}}},
       {"item": "cephfs", "json": {"data": {"/storage/cephfs": {"Datastore.AllocateSpace": 1}}}},
       {"item": "gone", "json": {"data": {}}}]
assert f.visible_stores(res) == ["local", "cephfs"]
assert "playbooks/10-pve-identity.yml playbooks/30" in open(os.path.join(os.path.dirname(__file__), "..", "Makefile")).read()  # make upgrade runs stage 10

# The services cluster's CAPMOX token sees only its boot and ISO stores, so CAPMOX cannot pick another iso store;
# its user keeps /storage for the boot stores of the tenant clusters' tokens.
assert "{path: /storage, role: ProxCloudCAPI, propagate: 1, user: capmox@pve, token: \"\"}" in open(
    os.path.join(os.path.dirname(__file__), "..", "playbooks", "10-pve-identity.yml")).read()

# An ACL on a path replaces what its parent passes down: wherever split mode grants the portal token the mint role on
# a path it works under, the same path must keep its platform role.
stage = open(os.path.join(os.path.dirname(__file__), "..", "playbooks", "10-pve-identity.yml")).read()
for path in ("/pool", "/storage", "/sdn/zones"):
    for role in ("ProxCloudKubeMint", "ProxCloudPlatform"):
        assert f"{{path: {path}, role: {role}, propagate: 1, user: proxcloud@pve, token: proxcloud@pve!platform}}" in stage, (path, role)

# SigV4 against the worked example the Go verifier also accepts (pkg/auth/sigv4_test.go signs the same way).
h = api.sigv4_headers("POST", "http://10.0.0.1:8080/api/v1/admin/service?b=1&a=2", b'{"x":1}', "pkA", "skB", "20261107T010203Z")
assert h["Host"] == "10.0.0.1:8080"
assert h["Authorization"].startswith("AWS4-HMAC-SHA256 Credential=pkA/20261107/us-east-1/execute-api/aws4_request, SignedHeaders=host;x-amz-date, Signature=")
assert h["Authorization"] != api.sigv4_headers("POST", "http://10.0.0.1:8080/api/v1/admin/service?a=2&b=1", b"{}", "pkA", "skB", "20261107T010203Z")["Authorization"]

# The CSI gateway admits the services nodes: a range covers exactly its addresses, nothing next to it.
assert f.ip_cidrs(["192.0.2.201-192.0.2.209"]) == ["192.0.2.201/32", "192.0.2.202/31", "192.0.2.204/30", "192.0.2.208/31"]
assert f.ip_cidrs(["192.0.2.7", "198.51.100.0/28"]) == ["192.0.2.7/32", "198.51.100.0/28"]

# The gateway's URL names the first mgmt node by address; the API is pinned to the node that has it.
nodes = {"items": [
    {"metadata": {"name": "m2"}, "status": {"addresses": [{"type": "InternalIP", "address": "192.0.2.11"}]}},
    {"metadata": {"name": "m1"}, "status": {"addresses": [{"type": "InternalIP", "address": "192.0.2.10"}, {"type": "Hostname", "address": "m1"}]}},
]}
assert f.node_with_address(nodes, "192.0.2.10") == "m1"
try:
    f.node_with_address(nodes, "192.0.2.99")
    raise AssertionError("an address no node has must fail")
except ValueError:
    pass

# The sandbox subnet: outside the tenant pool, every cluster address inside it.
sbx = {"subnet": "10.110.0.0/24", "gateway": "10.110.0.1", "nodes": "10.110.0.10-10.110.0.19", "metallb": "10.110.0.200-10.110.0.209",
       "snat": "198.51.100.7"}
assert f.sandbox_net_problem(sbx, "10.100.0.0/16") == ""
# Its own egress address (D14): required with no default, outside the subnet and the tenant pool; "node" shares
# the exit node's. The empty-case message says what to set and why, not just "invalid".
empty = f.sandbox_net_problem(dict(sbx, snat=""), "10.100.0.0/16")
assert "sandbox.network.snat is required and has no default" in empty and "abuse reports" in empty, empty
assert "sandbox.network.snat must be" in f.sandbox_net_problem(dict(sbx, snat="198.51.100.0/24"), "10.100.0.0/16")
assert "outside" in f.sandbox_net_problem(dict(sbx, snat="10.110.0.250"), "10.100.0.0/16")
assert "outside" in f.sandbox_net_problem(dict(sbx, snat="10.100.7.1"), "10.100.0.0/16")
assert "outside" in f.sandbox_net_problem(dict(sbx, snat="2001:db8::1"), "10.100.0.0/16")
assert f.sandbox_net_problem(dict(sbx, snat="node"), "10.100.0.0/16") == ""
assert "overlaps" in f.sandbox_net_problem(dict(sbx, subnet="10.100.9.0/24", gateway="10.100.9.1",
                                                nodes="10.100.9.10-10.100.9.19", metallb="10.100.9.200-10.100.9.209"), "10.100.0.0/16")
assert "IPv4 network" in f.sandbox_net_problem(dict(sbx, subnet=""), "10.100.0.0/16")
assert "IPv4" in f.sandbox_net_problem(dict(sbx, subnet="fd00::/64"), "10.100.0.0/16")
assert "outside" in f.sandbox_net_problem(dict(sbx, gateway="10.111.0.1"), "10.100.0.0/16")
assert "nodes" in f.sandbox_net_problem(dict(sbx, nodes="10.110.0.1-10.110.0.9"), "10.100.0.0/16")  # holds the gateway
assert "metallb" in f.sandbox_net_problem(dict(sbx, metallb="10.111.0.200-10.111.0.209"), "10.100.0.0/16")
assert "metallb" in f.sandbox_net_problem(dict(sbx, metallb=""), "10.100.0.0/16")
# Nor any other network the isolation rules or the guardrail rely on: a services network equal to the sandbox subnet
# would cut tenants off their control planes, a blocked or public one would cut it off the nodes.
avoid = {"services.network": "10.110.0.1/24", "network.blocked_cidrs": [], "network.public.cidr": ""}
assert "services.network 10.110.0.1/24" in f.sandbox_net_problem(sbx, "10.100.0.0/16", avoid)
assert "network.blocked_cidrs" in f.sandbox_net_problem(sbx, "10.100.0.0/16", {"network.blocked_cidrs": ["10.0.0.0/8"]})
assert "network.public.cidr" in f.sandbox_net_problem(sbx, "10.100.0.0/16", {"network.public.cidr": "10.110.0.128/25"})
assert f.sandbox_net_problem(sbx, "10.100.0.0/16", {"services.network": "10.89.0.1/24", "network.blocked_cidrs": [],
                                                    "network.public.cidr": "203.0.113.0/24"}) == ""

# The guardrail must deny the services network, and its list must be the one network-policy.yaml denies.
assert f.sandbox_guardrail_problem({"services.network": "10.89.0.1/24"}) == ""
assert "services.network 192.0.2.0/24" in f.sandbox_guardrail_problem({"services.network": "192.0.2.1/24"})
assert "outside" in f.sandbox_guardrail_problem({"x": ["10.0.0.0/8", "11.0.0.0/8"]})  # partly outside
assert "outside" in f.sandbox_guardrail_problem({"x": "10.0.0.0/7"})
import re  # noqa: E402

policy = open(os.path.join(os.path.dirname(__file__), "..", "..", "deploy", "sandbox-cluster", "network-policy.yaml")).read()
assert set(re.findall(r"- cidr: (\S+)", policy)) - {"0.0.0.0/0"} == set(f.SANDBOX_GUARDRAIL_DENY), "guardrail drift"

# Stage 60 keeps svc/pve-api while any cluster still runs its tenant-side proxmox-csi controller on its own token.
import base64  # noqa: E402
import importlib.util  # noqa: E402

_spec = importlib.util.spec_from_file_location(
    "pve_forward", os.path.join(os.path.dirname(__file__), "..", "playbooks", "files", "pve-forward.py"))
fwd = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(fwd)


def _sec(name, csi_yaml):
    return {"metadata": {"namespace": "p1", "name": name}, "data": {"csi.yaml": base64.b64encode(csi_yaml.encode()).decode()}}


def _app(name, csi, managed=True):
    labels = {"app.kubernetes.io/managed-by": "proxcloud"} if managed else {}
    return {"metadata": {"namespace": "argocd", "name": name, "labels": labels},
            "spec": {"source": {"helm": {"valuesObject": {"proxmox": {"csi": csi}}}}}}


SC = [{"name": "std", "storage": "local-zfs"}]
GW = {"secretName": "c1-csi-gateway", "kubeconfigSecretName": "c1-admin-kubeconfig"}
assert fwd.users([_sec("c1-csi-config", "token_secret: x"), _sec("c2-csi-config", "namespace: csi-proxmox"),
                  _sec("c1-csi-gateway", "token_secret: x"), {"metadata": {"namespace": "p1", "name": "c3-csi-config"}}],
                 []) == ["p1/c1-csi-config"]
# A half-finished migration: the config Secret was rewritten but the Application patch failed.
assert fwd.users([_sec("c1-csi-config", "namespace: csi-proxmox")], [_app("c1", {"storageClasses": SC})]) == ["argocd/c1"]
assert fwd.users([], [_app("c1", {"storageClasses": SC, "gateway": GW}), _app("c2", {"storageClasses": []}),
                      _app("c3", {}), _app("x", {"storageClasses": SC}, managed=False)]) == []

# The sandbox's public entry (stage 56): the proxy on a MetalLB address, the appliance off every cluster address.
edge = {"domain": "sbx.example.org", "network": sbx, "proxy": {"address": "10.110.0.200"},
        "tls": {"issuer": "acme", "acme": {"email": "ops@example.org"}},
        "powerdns": {"api_url": "http://192.0.2.53:8081", "nameserver": "192.0.2.53:53"},
        "entry": {"address": "10.110.0.5", "public": {"bridge": "vmbr1", "address": "203.0.113.10/24", "gateway": "203.0.113.1"}}}
assert f.sandbox_edge_problem(edge) == "", f.sandbox_edge_problem(edge)
assert "sandbox.domain" in f.sandbox_edge_problem(dict(edge, domain=""))
assert "sandbox.domain" in f.sandbox_edge_problem(dict(edge, domain="*.sbx.example.org"))
assert "metallb" in f.sandbox_edge_problem(dict(edge, proxy={"address": "10.110.0.50"}))
assert "proxy.address is required" in f.sandbox_edge_problem(dict(edge, proxy={}))
assert "issuer" in f.sandbox_edge_problem(dict(edge, tls={"issuer": "letsencrypt"}))
assert "tls.secret" in f.sandbox_edge_problem(dict(edge, tls={"issuer": "secret"}))
assert f.sandbox_edge_problem(dict(edge, tls={"issuer": "secret", "secret": "wild"}, powerdns={})) == ""
assert "PowerDNS" in f.sandbox_edge_problem(dict(edge, powerdns={}))
assert f.sandbox_edge_problem(dict(edge, tls={"issuer": "selfsigned"}, powerdns={})) == ""
for bad in ["10.110.0.1", "10.110.0.12", "10.110.0.205", "10.111.0.5", ""]:  # gateway, worker, MetalLB, outside, unset
    assert "entry.address" in f.sandbox_edge_problem(dict(edge, entry=dict(edge["entry"], address=bad))), bad
pub = edge["entry"]["public"]
assert "public.address" in f.sandbox_edge_problem(dict(edge, entry=dict(edge["entry"], public=dict(pub, address="203.0.113.10"))))
assert "public.address" in f.sandbox_edge_problem(dict(edge, entry=dict(edge["entry"], public=dict(pub, gateway="198.51.100.1"))))
assert "public.address" in f.sandbox_edge_problem(dict(edge, entry=dict(edge["entry"], public=dict(pub, address="10.110.0.6/24"))))
assert f.sandbox_edge_problem(dict(edge, entry={"public": {"bridge": ""}})) == ""  # the operator forwards 443 itself
print("ok")
