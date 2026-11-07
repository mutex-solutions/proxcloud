#!/usr/bin/env python3
"""ProxCloud sandbox ICMP anti-spoof guard (docs/managed-sandbox.md §2.6).

PVE's nftables bridge firewall accepts ICMP and ICMPv6 error types in its `vm-out`/`vm-in` chains
BEFORE the per-NIC `ipfilter` and the DROP policy run, so a sandbox worker can send an ICMP error
with any forged source and it is neither filtered nor dropped. This guard installs a host nftables
chain on the sandbox bridge, ahead of PVE's chain (bridge prerouting, priority -300), that drops an
ICMP error leaving a sandbox worker's tap unless its source is one of that worker's own pinned
addresses (exactly PVE's `ipfilter-net<N>` set for the NIC). The sandbox is IPv4-only, so every
ICMPv6 error from a worker is spoofed and dropped outright.

Driven from PVE's own cluster config (/etc/pve, replicated to every node): the portal's sync keeps
each worker's `ipfilter` correct, this guard mirrors it for the ICMP error types PVE lets bypass it.
Legitimate own-source errors fall through to PVE's chain (our base chain's accept is not terminal for
the hook). Run on a timer on every node that can host a sandbox worker.

Usage: sandbox-icmp-guard.py <pool> <vnet>   (also reads POOL/VNET from the environment)
"""
import os
import re
import subprocess
import sys

TABLE = "proxcloud-sandbox"
# The error types PVE's `allow-icmp` chain accepts before the per-NIC checks, i.e. the ones that bypass
# `ipfilter`. Dropping a forged-source one of these closes the hole; echo/ping (type 8/0) is untouched.
ICMP4_ERRORS = "destination-unreachable, source-quench, time-exceeded, redirect, parameter-problem"
ICMP6_ERRORS = "destination-unreachable, packet-too-big, time-exceeded, parameter-problem"


def pool_vmids(pool):
    """The VMIDs of a pool, from /etc/pve/user.cfg (pool:<id>:<comment>:<csv vmids>:...)."""
    try:
        for line in open("/etc/pve/user.cfg"):
            f = line.rstrip("\n").split(":")
            if len(f) >= 4 and f[0] == "pool" and f[1] == pool:
                return [v for v in f[3].split(",") if v]
    except FileNotFoundError:
        pass
    return []


def sandbox_nics(vmid, vnet):
    """NIC indices of a VM whose net<N> is on the sandbox vnet with firewall=1 AND whose tap is up on
    THIS node (so the guard on each node only pins its own local workers)."""
    nics = []
    try:
        conf = open("/etc/pve/qemu-server/%s.conf" % vmid).read()
    except FileNotFoundError:
        return nics
    for m in re.finditer(r"^net(\d+):\s*(.+)$", conf, re.M):
        n, opts = m.group(1), m.group(2)
        kv = dict(p.split("=", 1) for p in opts.split(",") if "=" in p)
        if kv.get("bridge") == vnet and kv.get("firewall") == "1" and os.path.isdir("/sys/class/net/tap%si%s" % (vmid, n)):
            nics.append(n)
    return nics


def ipfilter(vmid, n):
    """The IPv4 entries of a NIC's `[IPSET ipfilter-net<N>]` in /etc/pve/firewall/<vmid>.fw: the worker's
    pinned source addresses. Returns [] if the file or the set is absent (then: drop every error)."""
    want = "[IPSET ipfilter-net%s]" % n
    out, insec = [], False
    try:
        lines = open("/etc/pve/firewall/%s.fw" % vmid).read().splitlines()
    except FileNotFoundError:
        return out
    for line in lines:
        s = line.strip()
        if s.startswith("["):
            insec = s.split("#", 1)[0].strip() == want
            continue
        if insec and s and not s.startswith("#"):
            addr = s.split("#", 1)[0].strip().lstrip("+")
            if addr and ":" not in addr:  # IPv4 only
                out.append(addr)
    return out


def ruleset(pool, vnet):
    taps = []  # (tap, [pinned ipv4])
    for vmid in pool_vmids(pool):
        for n in sandbox_nics(vmid, vnet):
            taps.append(("tap%si%s" % (vmid, n), ipfilter(vmid, n)))
    lines = []
    if taps:
        names = "{ %s }" % ", ".join('"%s"' % t for t, _ in taps)
        lines.append("    iifname %s icmpv6 type { %s } drop" % (names, ICMP6_ERRORS))
        for tap, pinned in taps:
            src = (" ip saddr != { %s }" % ", ".join(pinned)) if pinned else ""
            lines.append('    iifname "%s" meta l4proto icmp icmp type { %s }%s drop' % (tap, ICMP4_ERRORS, src))
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
