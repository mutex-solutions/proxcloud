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


def kept_profiles(detected, site, existing):
    """The profiles a run writes. The site's list is authoritative; otherwise what is on the provider stays as the
    operator left it and only a newly detected storage is added."""
    return site or {**detected, **existing}


def kept_default_profile(profiles, detected, site, existing):
    """The default a run writes: the site's pick, else what the provider has, so a storage added later does not
    move new VMs. A default whose profile is gone falls back to the detected one."""
    return site or (existing if existing in profiles else detected)


def storage_classes(fio, site, existing):
    """config.storageClasses a run writes: what is stored (a class confirmed in the admin console), then the
    latest fio suggestion, then the site's class, which wins."""
    out = {k: dict(v) for k, v in existing.items()}
    for storage, r in fio.items():
        out.setdefault(storage, {})["suggestedClass"] = r["class"]
    for storage, cls in site.items():
        out.setdefault(storage, {})["class"] = cls
    return out


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


def ip_cidrs(entries):
    """Addresses, CIDRs and first-last ranges (services.network.nodes) as the CIDRs that cover exactly them."""
    import ipaddress

    out = []
    for e in entries:
        first, _, last = str(e).partition("-")
        if not last:
            out.append(str(ipaddress.ip_network(first.strip(), strict=False)))
            continue
        nets = ipaddress.summarize_address_range(ipaddress.ip_address(first.strip()), ipaddress.ip_address(last.strip()))
        out.extend(str(n) for n in nets)
    return out


def pve_acl_missing(rows, wanted):
    """Which wanted ACL entries GET /access/acl does not hold yet. A wanted entry is {path, role,
    propagate, user, token}; both the user and its privsep token need it, unless one of them is empty."""
    have = {(r["path"], r["roleid"], r["type"], r["ugid"], int(r.get("propagate", 1))) for r in rows}
    missing = []
    for w in wanted:
        for typ, ugid, key in (("user", w["user"], "users"), ("token", w["token"], "tokens")):
            if ugid and (w["path"], w["role"], typ, ugid, int(w["propagate"])) not in have:
                missing.append({"path": w["path"], "roles": w["role"], "propagate": int(w["propagate"]), key: ugid})
    return missing


def boot_iso_problem(storages, iso, boot):
    """Why `iso` cannot hold bootstrap ISOs (kubeadm join tokens) for workers booting from `boot`, or "". CAPMOX takes
    the first iso store its token sees, in no fixed order, so the token must see one, and a node-local one. Mirrors
    BootISOProblem in internal/cloud/pve/bootiso.go."""
    rows = {s["storage"]: s for s in storages}
    s = rows.get(iso)
    if s is None:
        return f"storage {iso} does not exist"
    if "iso" not in _content(s):
        return f"storage {iso} does not take ISO images"
    if int(s.get("shared", 0)) or s.get("type") in ("rbd", "cephfs", "nfs", "cifs", "iscsi", "iscsidirect", "zfs", "pbs", "glusterfs", "drbd"):
        return f"storage {iso} is shared, and whoever reaches it reads the join tokens of booting nodes: use a node-local store such as local"
    if boot != iso and "iso" in _content(rows.get(boot, {})):
        return f"boot storage {boot} also takes ISO images, so CAPMOX may put the bootstrap ISO on either"
    return ""


def iso_stores(storages):
    """The enabled stores with iso content: the ones go-proxmox may pick for a bootstrap ISO."""
    return [s["storage"] for s in storages if "iso" in _content(s) and not int(s.get("disable", 0))]


def visible_stores(results):
    """Of uri results of GET /access/permissions?path=/storage/<item>, the stores the user or token sees: PVE lists a
    store to whoever holds Datastore.Audit or Datastore.AllocateSpace on it."""
    out = []
    for r in results:
        privs = (r.get("json") or {}).get("data", {}).get("/storage/" + r["item"]) or {}
        if "Datastore.Audit" in privs or "Datastore.AllocateSpace" in privs:
            out.append(r["item"])
    return out


class FilterModule:
    def filters(self):
        return {
            "pc_classify_storages": classify_storages,
            "pc_default_profile": default_profile,
            "pc_kept_profiles": kept_profiles,
            "pc_kept_default_profile": kept_default_profile,
            "pc_storage_classes": storage_classes,
            "pc_fio_class": fio_class,
            "pc_template_nodes": template_nodes,
            "pc_acl_missing": pve_acl_missing,
            "pc_boot_iso_problem": boot_iso_problem,
            "pc_iso_stores": iso_stores,
            "pc_visible_stores": visible_stores,
            "pc_ip_cidrs": ip_cidrs,
        }
