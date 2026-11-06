"""Decisions the installer makes from Proxmox data, kept apart from the playbooks so they can be tested
(tests/test_filters.py)."""


def _content(s):
    return [c for c in str(s.get("content", "")).split(",") if c]


def classify_storages(storages):
    """Map GET /storage rows to tier and proposed profiles (plan S19, the installer's half of
    pve/capability.go). A profile slug maps to one storage; HDD-class is the fio result's call."""
    out = {"tier": "standard", "profiles": {}, "templates": [], "imports": [], "backups": []}
    for s in sorted(storages, key=lambda s: s["storage"]):
        kind, content, shared = s.get("type"), _content(s), bool(s.get("shared")) or s.get("type") in ("nfs", "cifs", "rbd", "cephfs", "pbs")
        if kind == "rbd":
            out["tier"] = "full"
        if "import" in content:
            out["imports"].append(s["storage"])
        if "backup" in content:
            out["backups"].append(s["storage"])
        if "images" not in content:
            continue
        if kind == "rbd":
            out["profiles"].setdefault("ceph", {"storages": {"*": s["storage"]}, "shared": True, "snapshots": True, "thin": True, "liveMigrate": True})
        elif kind == "zfspool":
            out["profiles"].setdefault("std", {"storages": {"*": s["storage"]}, "snapshots": True, "thin": True, "replication": True})
        elif kind == "lvmthin":
            out["profiles"].setdefault("local-lvm", {"storages": {"*": s["storage"]}, "snapshots": True, "thin": True})
        elif kind in ("nfs", "cifs"):
            if "iso" in content or "import" in content:
                out["templates"].append(s["storage"])  # the template store is not a tenant profile
            else:
                out["profiles"].setdefault("capacity", {"storages": {"*": s["storage"]}, "shared": True, "liveMigrate": True, "label": "Capacity HDD (NFS)"})
    return out


def default_profile(profiles, wanted=""):
    """The profile new VMs land on: the operator's pick, else the fastest type detected."""
    if wanted:
        return wanted
    for slug in ("ceph", "std", "local-lvm"):
        if slug in profiles:
            return slug
    return sorted(profiles)[0] if profiles else ""


def fio_class(p99_ms, iops, performance_ms=1, standard_ms=5, capacity_iops=2000):
    """Suggested class from a benchmark (plan S19). NFS randread can be flattered by the NAS ARC, so
    latency decides first and IOPS can only downgrade."""
    if p99_ms > standard_ms or iops < capacity_iops:
        return "capacity"
    return "performance" if p99_ms < performance_ms else "standard"


def template_nodes(nodes, profile_storage_shared, per_node="auto"):
    """Nodes that need a template copy. PVE clones to another node only from shared to shared storage,
    so anything but shared VM disks needs one copy per node."""
    names = sorted(nodes)
    if per_node == "auto":
        per_node = not profile_storage_shared
    return names if per_node else names[:1]


def pve_acl_missing(rows, wanted):
    """Which wanted ACL entries GET /access/acl does not hold yet. A wanted entry is {path, role,
    propagate, user, token}; both the user and its privsep token need it."""
    have = {(r["path"], r["roleid"], r["type"], r["ugid"], int(r.get("propagate", 1))) for r in rows}
    missing = []
    for w in wanted:
        for typ, ugid, key in (("user", w["user"], "users"), ("token", w["token"], "tokens")):
            if (w["path"], w["role"], typ, ugid, int(w["propagate"])) not in have:
                missing.append({"path": w["path"], "roles": w["role"], "propagate": int(w["propagate"]), key: ugid})
    return missing


class FilterModule:
    def filters(self):
        return {
            "pc_classify_storages": classify_storages,
            "pc_default_profile": default_profile,
            "pc_fio_class": fio_class,
            "pc_template_nodes": template_nodes,
            "pc_acl_missing": pve_acl_missing,
        }
