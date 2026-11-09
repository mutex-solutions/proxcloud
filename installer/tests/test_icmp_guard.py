"""Checks for installer/playbooks/files/sandbox-icmp-guard.py against a fake /etc/pve tree."""
import importlib.util
import os
import tempfile

here = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location("guard", os.path.join(here, "..", "playbooks", "files", "sandbox-icmp-guard.py"))
g = importlib.util.module_from_spec(spec)
spec.loader.exec_module(g)

root = tempfile.mkdtemp()
g.PVE = root
FILES = {
    "user.cfg": "pool:proxcloud-sandbox:sbx:120,121,122,123:::\npool:pc-tenant::900:::\n",
    # 121 is local, 120's config sits under another node (live migration / HA restart onto this one).
    "nodes/pve2/qemu-server/121.conf": "net0: virtio=BC:24:11:DA:D8:E6,bridge=pcsbx,firewall=1\nnet1: virtio=BC:24:11:00:00:09,bridge=vmbr0\n"
                                       "\n[snap1]\nnet2: virtio=BC:24:11:00:00:08,bridge=pcsbx,firewall=1\n",
    "nodes/pve3/qemu-server/120.conf": "net0: virtio=BC:24:11:00:00:01,bridge=pcsbx,firewall=1\n",
    "nodes/pve1/qemu-server/122.conf": "net0: virtio=BC:24:11:00:00:02,bridge=pcsbx,firewall=1\n",
    "nodes/pve1/qemu-server/123.conf": "net0: virtio=BC:24:11:00:00:03,bridge=pcsbx,firewall=1\n",
    "nodes/pve1/qemu-server/900.conf": "net0: virtio=BC:24:11:00:00:04,bridge=pcsbx,firewall=1\n",
    # nomatch, an alias, a comment and overlapping CIDRs: none may make the nft invalid or allow more.
    "firewall/121.fw": "[OPTIONS]\nipfilter: 1\n\n[IPSET ipfilter-net0] # pinned\n10.110.0.10 # worker\n10.110.0.200/29\n"
                       "10.110.0.208/31\n10.110.0.201\n!10.110.0.205\ndc/somealias\n",
    "firewall/120.fw": "[IPSET ipfilter-net0]\n10.110.0.11\n",
    "firewall/123.fw": "[IPSET ipfilter-net0]\n",  # empty set: PVE drops every IPv4 frame
}
for path, body in FILES.items():
    os.makedirs(os.path.dirname(os.path.join(root, path)), exist_ok=True)
    with open(os.path.join(root, path), "w") as fh:
        fh.write(body)

rs = g.ruleset("proxcloud-sandbox", "pcsbx")
rules = [l.strip() for l in rs.splitlines() if l.startswith("    iifname")]
NOMATCH = 'iifname "tap121i0" ip saddr { 10.110.0.205/32 } counter drop'
ALLOW = 'iifname "tap121i0" ip saddr != { 10.110.0.10/32, 10.110.0.200/29, 10.110.0.208/31 } counter drop'

# Every pool worker is pinned on every node, wherever its config lives; nothing outside the pool or vnet.
assert 'iifname "tap120i0" ip saddr != { 10.110.0.11/32 } counter drop' in rules, rs
assert not any("tap900" in l or "tap121i1" in l or "tap121i2" in l for l in rules), rs
# Any forged IPv4 source is dropped, not only ICMP errors (PVE's ct established/related accept precedes ipfilter).
assert ALLOW in rules, rs
# A nomatch entry is excluded before the allow list; the alias is ignored (drops more, never less).
assert NOMATCH in rules and rules.index(NOMATCH) < rules.index(ALLOW), rs
assert "!" not in rs.replace("!=", "") and "somealias" not in rs, rs
# Not fenced yet (no set): PVE pins nothing, the guard still drops every ICMP error. Empty set: drop all IPv4.
assert any(l.startswith('iifname "tap122i0" meta l4proto icmp icmp type {') for l in rules), rs
assert not any(l.startswith('iifname "tap122i0" ip saddr') for l in rules), rs
assert 'iifname "tap123i0" meta protocol ip counter drop' in rules, rs
# Every worker's ICMPv6 errors are dropped; the table is replaced atomically.
assert any(l.startswith('iifname { "tap120i0", "tap121i0", "tap122i0", "tap123i0" } icmpv6 type') for l in rules), rs
assert rs.startswith("add table bridge proxcloud-sandbox\ndelete table bridge proxcloud-sandbox\n"), rs
assert "priority -300" in rs

# No workers: an empty chain, still valid.
assert "iifname" not in g.ruleset("no-such-pool", "pcsbx")
print("icmp guard: ok")
