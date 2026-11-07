#!/usr/bin/env python3
"""ProxCloud sandbox anti-spoof guard (docs/managed-sandbox.md §2.6).

PVE's nftables bridge firewall accepts ICMP and ICMPv6 error types in its `vm-out`/`vm-in` chains
BEFORE the per-NIC `ipfilter` and the DROP policy run, and its `pre-vm-out` accepts conntrack
established/related before `ipfilter` too, so a sandbox worker can send an ICMP error (or a packet
matching a flow tracked on the node's bridge) with any forged source. This guard installs a host
nftables chain on the sandbox bridge, ahead of PVE's chain (bridge prerouting, priority -300), that
drops any IPv4 packet leaving a sandbox worker's tap unless its source is one of that worker's own
pinned addresses (exactly PVE's `ipfilter-net<N>` set for the NIC, nomatch entries honoured). A NIC
with no `ipfilter` set yet (the portal's sync has not fenced it) gets PVE's own behaviour, no pin,
except that every ICMP error from it is dropped. The sandbox is IPv4-only, so every ICMPv6 error
from a worker is spoofed and dropped outright.

Driven from PVE's own cluster config (/etc/pve, replicated to every node): the portal's sync keeps
each worker's `ipfilter` correct, this guard mirrors it ahead of the early accepts that let traffic bypass it.
Legitimate own-source traffic falls through to PVE's chain (our base chain's accept is not terminal for
the hook). Run on a timer on every node that can host a sandbox worker; each node pins every worker of
the pool (rules match the tap name, so they are inert where the tap is absent), so a worker that
live-migrates or is HA-restarted onto a node is covered the moment its tap appears.

Usage: sandbox-icmp-guard.py <pool> <vnet>   (also reads POOL/VNET from the environment)
"""
import glob
import ipaddress
import os
import re
import subprocess
import sys

PVE = "/etc/pve"
TABLE = "proxcloud-sandbox"
# The error types PVE's `allow-icmp` chain accepts before the per-NIC checks, i.e. the ones that bypass
# `ipfilter`. Dropping a forged-source one of these closes the hole; echo/ping (type 8/0) is untouched.
ICMP4_ERRORS = "destination-unreachable, source-quench, time-exceeded, redirect, parameter-problem"
ICMP6_ERRORS = "destination-unreachable, packet-too-big, time-exceeded, parameter-problem"


def pool_vmids(pool):
    """The VMIDs of a pool, from /etc/pve/user.cfg (pool:<id>:<comment>:<csv vmids>:...)."""
    try:
        for line in open(PVE + "/user.cfg"):
            f = line.rstrip("\n").split(":")
            if len(f) >= 4 and f[0] == "pool" and f[1] == pool:
                return [v for v in f[3].split(",") if v]
    except FileNotFoundError:
        pass
    return []


def sandbox_nics(vmid, vnet):
    """NIC indices of a VM whose net<N> is on the sandbox vnet with firewall=1. The config is read from
    whichever node holds it (/etc/pve/nodes/*/qemu-server, not the local-node symlink), so the guard on
    every node pins every worker, a migrating or HA-restarted one included."""
    nics = []
    confs = glob.glob("%s/nodes/*/qemu-server/%s.conf" % (PVE, vmid))
    if not confs:
        return nics
    conf = open(confs[0]).read().split("\n[", 1)[0]  # current config, not snapshot sections
    for m in re.finditer(r"^net(\d+):\s*(.+)$", conf, re.M):
        n, opts = m.group(1), m.group(2)
        kv = dict(p.split("=", 1) for p in opts.split(",") if "=" in p)
        if kv.get("bridge") == vnet and kv.get("firewall") == "1":
            nics.append(n)
    return nics


def ipfilter(vmid, n):
    """The IPv4 entries of a NIC's `[IPSET ipfilter-net<N>]` in /etc/pve/firewall/<vmid>.fw: the worker's
    pinned source addresses, as (allow, nomatch) collapsed networks, or None if the file or the set is
    absent. An entry that is not an IPv4 address or CIDR (an alias, a typo) is left out, which drops
    more, never less, and is reported on stderr."""
    want = "[IPSET ipfilter-net%s]" % n
    allow, nomatch, found, insec = [], [], False, False
    try:
        lines = open("%s/firewall/%s.fw" % (PVE, vmid)).read().splitlines()
    except FileNotFoundError:
        return None
    for line in lines:
        s = line.strip()
        if s.startswith("["):
            insec = s.split("#", 1)[0].strip() == want
            found = found or insec
            continue
        if insec and s and not s.startswith("#"):
            addr = s.split("#", 1)[0].split()[0]
            neg = addr.startswith("!")
            addr = addr.lstrip("!")
            if ":" in addr:  # IPv6: the sandbox is IPv4-only
                continue
            try:
                net = ipaddress.IPv4Network(addr, strict=False)
            except ValueError:
                sys.stderr.write("vm %s net%s: ipfilter entry %r is not an IPv4 address, ignored\n" % (vmid, n, addr))
                continue
            (nomatch if neg else allow).append(net)
    if not found:
        return None
    return list(ipaddress.collapse_addresses(allow)), list(ipaddress.collapse_addresses(nomatch))


def nftset(nets):
    return "{ %s }" % ", ".join(str(x) for x in nets)


def ruleset(pool, vnet):
    taps = []  # (tap, (allow, nomatch) or None)
    for vmid in pool_vmids(pool):
        for n in sandbox_nics(vmid, vnet):
            taps.append(("tap%si%s" % (vmid, n), ipfilter(vmid, n)))
    lines = []
    if taps:
        names = "{ %s }" % ", ".join('"%s"' % t for t, _ in taps)
        lines.append("    iifname %s icmpv6 type { %s } counter drop" % (names, ICMP6_ERRORS))
        for tap, pin in taps:
            if pin is None:  # not fenced yet: PVE pins nothing, we still drop every ICMP error
                lines.append('    iifname "%s" meta l4proto icmp icmp type { %s } counter drop' % (tap, ICMP4_ERRORS))
                continue
            allow, nomatch = pin
            if nomatch:
                lines.append('    iifname "%s" ip saddr %s counter drop' % (tap, nftset(nomatch)))
            if allow:
                lines.append('    iifname "%s" ip saddr != %s counter drop' % (tap, nftset(allow)))
            else:  # an empty set pins nothing: PVE then drops every IPv4 frame, so do we
                lines.append('    iifname "%s" meta protocol ip counter drop' % tap)
    body = "\n".join(lines)
    # Atomic replace of the whole table: add-then-delete so the delete never fails on a fresh node.
    return (
        "add table bridge %s\n"
        "delete table bridge %s\n"
        "table bridge %s {\n"
        "  chain antispoof {\n"
        "    type filter hook prerouting priority -300; policy accept;\n"
        "%s\n"
        "  }\n"
        "}\n"
    ) % (TABLE, TABLE, TABLE, body)


def main():
    pool = sys.argv[1] if len(sys.argv) > 1 else os.environ.get("POOL", "")
    vnet = sys.argv[2] if len(sys.argv) > 2 else os.environ.get("VNET", "")
    if not pool or not vnet:
        sys.exit("usage: sandbox-icmp-guard.py <pool> <vnet>")
    rs = ruleset(pool, vnet)
    if "--print" in sys.argv:
        sys.stdout.write(rs)
        return
    subprocess.run(["nft", "-f", "-"], input=rs, text=True, check=True)


if __name__ == "__main__":
    main()
