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


def registered_pve_provider(providers, name, required=False):
    """Choose only one well-formed CLOUD/proxmox row. A name collision must not reconfigure another provider."""
    if not isinstance(providers, list) or any(not isinstance(row, dict) for row in providers):
        raise ValueError("invalid provider list; registration stopped")
    matches = [row for row in providers if row.get("name") == name]
    if not matches and not required:
        return {}
    if len(matches) != 1:
        raise ValueError("expected one uniquely named Proxmox provider; registration stopped")
    row = matches[0]
    provider_id, config = row.get("id"), row.get("config")
    if (row.get("type") != "CLOUD" or not isinstance(config, dict) or config.get("provider") != "proxmox"
            or not isinstance(provider_id, str) or not provider_id
            or any(c.isspace() or c in "/?#" for c in provider_id)):
        raise ValueError("existing provider name is not a valid CLOUD/proxmox row; registration stopped")
    return row


def pve_connection(site, api_url, existing=None, fingerprint=""):
    """Stage 60's connection overlay, not a replacement provider document. Null optional site values keep
    the console's endpoints/trust. A new CA explicitly selects verification, never the insecure bypass.
    Validation is local only; it neither discovers endpoints nor reads credentials/certificates from disk."""
    import copy
    import re
    import ssl
    from urllib.parse import urlsplit

    fresh = existing is None
    existing = existing or {}
    out = {"apiUrl": api_url}
    for key in ("apiUrls", "caCert", "fingerprint", "insecureTls"):
        if key in existing:
            out[key] = copy.deepcopy(existing[key])

    urls = site.get("portal_api_urls")
    if urls is not None:
        if not isinstance(urls, list):
            raise ValueError("pve.portal_api_urls must be a list of portal-reachable HTTP(S) URLs")
        for u in urls:
            try:
                parsed = urlsplit(u) if isinstance(u, str) else None
                valid = (parsed is not None and parsed.scheme in ("http", "https") and parsed.hostname
                         and parsed.port != 0 and parsed.username is None and parsed.password is None
                         and "?" not in u and "#" not in u
                         and parsed.path.rstrip("/") in ("", "/api2/json") and not any(c.isspace() for c in u))
            except ValueError:
                valid = False
            if not valid:
                # Do not echo a URL: a mistaken userinfo/query could contain a credential.
                raise ValueError("pve.portal_api_urls entries must be HTTP(S) origins (optional /api2/json), without credentials, query or fragment")
        out["apiUrls"] = list(urls)  # order and every endpoint stay; [] deliberately clears only this list

    ca = site.get("cluster_ca")
    if ca is not None:
        if not isinstance(ca, str) or not ca.strip() or len(ca.encode()) > 65536:
            raise ValueError("pve.cluster_ca must be a non-empty PEM certificate bundle of at most 64 KiB; omit it to keep installed trust")
        # Accept certificates only, never private keys or ignored text around a valid certificate.
        if not re.fullmatch(r"(?:\s*-----BEGIN CERTIFICATE-----\s+[A-Za-z0-9+/=\s]+-----END CERTIFICATE-----\s*)+", ca):
            raise ValueError("pve.cluster_ca must contain only PEM CERTIFICATE blocks")
        try:
            ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT).load_verify_locations(cadata=ca)
        except (ssl.SSLError, ValueError):
            raise ValueError("pve.cluster_ca contains an invalid PEM certificate") from None
        out["caCert"] = ca  # canonical provider field, consumed by ExternalService.PVEConfig().CACert
        out["insecureTls"] = False  # the transport checks this BEFORE caCert; an old true must not bypass it

    pin = site.get("fingerprint")
    if not pin and ca is None and fresh:
        pin = fingerprint  # fresh legacy site: stage 00's pinned leaf, not an invented trust mode
    if pin:
        if not isinstance(pin, str) or not re.fullmatch(r"[0-9a-fA-F]{64}", pin.strip().replace(":", "")):
            raise ValueError("pve.fingerprint must be a SHA-256 certificate fingerprint")
        out["fingerprint"] = pin.strip()
    elif ca is None and fresh:
        raise ValueError("no pinned PVE fingerprint: run preflight, or supply pve.cluster_ca")
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


def node_with_address(nodes, address):
    """The name of the node in `kubectl get nodes -o json` that has address. None is an error: the gateway would
    have no node to answer on."""
    for n in nodes.get("items", []):
        if any(a.get("address") == address for a in n.get("status", {}).get("addresses", [])):
            return n["metadata"]["name"]
    raise ValueError("no k3s node has address %s" % address)


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


# The ranges deploy/sandbox-cluster/network-policy.yaml denies to every sandbox pod (egressDeny). The installer tests
# check the two lists match.
SANDBOX_GUARDRAIL_DENY = ("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16")


def _cidrs(value):
    return [str(v) for v in (value if isinstance(value, (list, tuple)) else [value]) if v]


def sandbox_net_problem(net, cidr_pool, avoid=None):
    """Why sandbox.network cannot be used, or "". The isolation rules fence the sandbox subnet off the tenant pool and
    the blocked networks in both directions, so it must overlap none of them, nor any other network in avoid
    ({setting name: CIDR or list of CIDRs}); every address the cluster takes must sit inside the subnet."""
    import ipaddress

    try:
        sub = ipaddress.ip_network(str(net.get("subnet") or ""), strict=True)
    except ValueError:
        return "sandbox.network.subnet must be an IPv4 network such as 192.0.2.0/24, got %r" % net.get("subnet")
    if sub.version != 4:
        return "sandbox.network.subnet must be IPv4: the sandbox guardrail names IPv4 ranges only"
    for name, value in [("network.cidr_pool", cidr_pool)] + sorted((avoid or {}).items()):
        for c in _cidrs(value):
            if sub.overlaps(ipaddress.ip_network(c, strict=False)):
                return "sandbox.network.subnet %s overlaps %s %s" % (sub, name, c)
    try:
        gw = ipaddress.ip_address(str(net.get("gateway") or ""))
    except ValueError:
        return "sandbox.network.gateway is required"
    if gw not in sub:
        return "sandbox.network.gateway %s is outside %s" % (gw, sub)
    for key in ("nodes", "metallb"):
        first, _, last = str(net.get(key) or "").partition("-")
        try:
            lo, hi = ipaddress.ip_address(first.strip()), ipaddress.ip_address((last or first).strip())
        except ValueError:
            return "sandbox.network.%s must be a first-last range inside %s" % (key, sub)
        if lo not in sub or hi not in sub or lo > hi or lo <= gw <= hi:
            return "sandbox.network.%s %s must be inside %s and leave out the gateway" % (key, net.get(key), sub)
    snat = str(net.get("snat") or "").strip()
    if snat == "node":
        return ""
    if snat == "":
        return ("sandbox.network.snat is required and has no default (the right value is site-specific): set it to the "
                "sandbox's own egress address so abuse reports and blocklists name the sandbox, not the nodes, the "
                "portal or the tenants. Use an IPv4 address on the exit node's uplink network that the uplink routes to "
                "that node and nothing else uses (stage 55 puts it on the exit node, inventory group pve, root ssh). "
                "Or set it to 'node' to share the exit node's own address, only for a site with one public address that "
                "accepts the nodes and the portal sharing it with tenant egress.")
    try:
        addr = ipaddress.ip_address(snat)
    except ValueError:
        return ("sandbox.network.snat must be the sandbox's own egress address (an IPv4 address the uplink routes to "
                "the exit node, used by nothing else), or 'node' to share the exit node's address; got %r" % snat)
    if addr.version != 4 or addr in sub or any(addr in ipaddress.ip_network(c, strict=False) for c in _cidrs(cidr_pool)):
        return "sandbox.network.snat %s must be an IPv4 address outside %s and network.cidr_pool" % (addr, sub)
    return ""


def sandbox_guardrail_problem(nets):
    """Which of nets ({setting name: CIDR or list}) the sandbox guardrail does not deny, or "". A sandbox pod reaches
    whatever lies outside SANDBOX_GUARDRAIL_DENY without leaving gVisor, and the PVE isolation rules do not stop it
    from reaching the services network (tenants need that one)."""
    import ipaddress

    deny = [ipaddress.ip_network(d) for d in SANDBOX_GUARDRAIL_DENY]
    for name, value in sorted(nets.items()):
        for c in _cidrs(value):
            n = ipaddress.ip_network(c, strict=False)
            if not any(n.version == d.version and n.subnet_of(d) for d in deny):
                return ("%s %s is outside the ranges the sandbox guardrail denies (%s), so a sandbox pod could reach "
                        "it: use private addresses there, or add the range to deploy/sandbox-cluster/network-policy.yaml"
                        % (name, n, ", ".join(SANDBOX_GUARDRAIL_DENY)))
    return ""


def sandbox_edge_problem(sb):
    """Why the sandbox's public entry (stage 56) cannot be built from sandbox (the `sandbox` settings), or "".
    sandbox-proxy takes one MetalLB address; the entry appliance one address on the sandbox vnet that no worker, load
    balancer or gateway holds (the workers admit it alone), and a public address on its own bridge."""
    import ipaddress

    net, proxy, entry, tls = sb.get("network", {}), sb.get("proxy", {}), sb.get("entry", {}), sb.get("tls", {})
    domain = str(sb.get("domain") or "").strip(".")
    if not domain or "." not in domain or "*" in domain:
        return "sandbox.domain is required, e.g. sbx.example.org: sandboxes answer at https://<port>-<id>.<domain>"
    sub = ipaddress.ip_network(str(net["subnet"]))

    def span(key):
        first, _, last = str(net.get(key) or "").partition("-")
        return ipaddress.ip_address(first.strip()), ipaddress.ip_address((last or first).strip())

    try:
        p = ipaddress.ip_address(str(proxy.get("address") or ""))
    except ValueError:
        return "sandbox.proxy.address is required: the MetalLB address of sandbox-proxy, inside sandbox.network.metallb"
    lo, hi = span("metallb")
    if not lo <= p <= hi:
        return "sandbox.proxy.address %s must be inside sandbox.network.metallb %s" % (p, net.get("metallb"))
    if tls.get("issuer") not in ("acme", "secret", "selfsigned"):
        return "sandbox.tls.issuer must be acme, secret or selfsigned, got %r" % tls.get("issuer")
    if tls["issuer"] == "secret" and not tls.get("secret"):
        return "sandbox.tls.issuer secret needs sandbox.tls.secret: a kubernetes.io/tls Secret in pc-sandbox on the services cluster"
    if tls["issuer"] == "acme":
        pdns = sb.get("powerdns", {})
        if not (tls.get("acme", {}).get("email") and pdns.get("api_url") and pdns.get("nameserver")):
            return ("sandbox.tls.issuer acme needs sandbox.tls.acme.email and the zone on PowerDNS (sandbox.powerdns.api_url, "
                    ".nameserver and the vault's pdns_api_key) for the DNS-01 challenge; otherwise use issuer secret")
    pub = entry.get("public", {})
    if not pub.get("bridge"):
        return ""  # no appliance: the operator forwards TCP 443 to sandbox.proxy.address
    try:
        a = ipaddress.ip_address(str(entry.get("address") or ""))
    except ValueError:
        return "sandbox.entry.address is required with an appliance: its address on the sandbox vnet"
    if a not in sub or a == ipaddress.ip_address(str(net.get("gateway"))) or any(
            span(k)[0] <= a <= span(k)[1] for k in ("nodes", "metallb")):
        return "sandbox.entry.address %s must be inside %s, off the gateway, the workers and the MetalLB range" % (a, sub)
    try:
        pa = ipaddress.ip_interface(str(pub.get("address") or ""))
        gw = ipaddress.ip_address(str(pub.get("gateway") or ""))
    except ValueError:
        return "sandbox.entry.public.address (CIDR) and .gateway are required with sandbox.entry.public.bridge"
    if pa.ip in sub or "/" not in str(pub.get("address")) or gw not in pa.network:
        return "sandbox.entry.public.address %s must be a CIDR outside %s whose network holds the gateway %s" % (
            pub.get("address"), sub, gw)
    return ""


def _split_ref(ref):
    """'host/path/name:tag@sha256:d' -> (repo, tag, digest); tag '' when absent, digest '' when not pinned."""
    ref = str(ref).strip()
    repo, _, digest = ref.partition("@")
    tag = ""
    if repo.rfind(":") > repo.rfind("/"):
        repo, tag = repo.rsplit(":", 1)
    return repo, tag, digest


def sandbox_images(pins, host):
    """The mirror list for the sandbox registry (docs/managed-sandbox.md §6): every pinned image of
    deploy/templates/images.yaml (name -> ref@sha256:...) copied to <host>/library/<last path part>:<tag>@<digest>, the
    same digest. A pin without a digest, a bad name or two pins on one target are refused, so a site never pulls an
    unpinned or wrong built-in image."""
    import re

    out, seen = [], {}
    for name, ref in sorted((pins or {}).items()):
        if not re.match(r"^[a-z0-9][a-z0-9-]{0,62}$", str(name)):
            raise ValueError("pinned image name %r: lowercase letters, digits and '-'" % name)
        repo, tag, digest = _split_ref(ref)
        if not re.match(r"^sha256:[0-9a-f]{64}$", digest) or "/" not in repo:
            raise ValueError("pinned image %s = %r: want <registry>/<repository>[:tag]@sha256:<64 hex>" % (name, ref))
        trepo = "%s/library/%s" % (host, repo.rsplit("/", 1)[1])
        if trepo in seen:
            raise ValueError("pinned images %s and %s land on the same repository %s" % (seen[trepo], name, trepo))
        seen[trepo] = name
        # skopeo takes a tag or a digest, not both: source_ref and target_ref are by digest, target_tagged by tag.
        out.append({"name": name, "source": str(ref).strip(), "source_ref": repo + "@" + digest,
                    "target": "%s:%s@%s" % (trepo, tag or "pinned", digest), "target_ref": trepo + "@" + digest,
                    "target_tagged": "%s:%s" % (trepo, tag or "pinned"), "digest": digest})
    return out


def sandbox_templates(templates, pins, host=""):
    """The sandbox provider's templates with every image "pin:<name>" resolved: to the mirrored ref on the sandbox
    registry when host is set, else to the pinned source (straight from its registry). Other images stay as given."""
    mirror = {i["name"]: i for i in sandbox_images(pins, host or "x")}
    out = {}
    for name, t in (templates or {}).items():
        t = dict(t)
        img = str(t.get("image", ""))
        if img.startswith("pin:"):
            key = img[4:]
            if key not in mirror:
                raise ValueError("template %s: no pinned image %r in deploy/templates/images.yaml" % (name, key))
            t["image"] = mirror[key]["target"] if host else mirror[key]["source"]
        out[name] = t
    return out


_PREPULL_SCHEMAS = {
    # schema name -> the policy list that must cover its properties
    "io.k8s.api.core.v1.PodSpec": "pod",
    "io.k8s.api.core.v1.Container": "container",
    "io.k8s.api.apps.v1.DaemonSetSpec": "daemonset",
    "io.k8s.api.core.v1.PodTemplateSpec": "template",
}


def prepull_policy_gaps(openapi_docs, fields):
    """The fields the prepull admission policy would leave writable on this cluster (docs/template-updates.md §4.3).
    openapi_docs: the cluster's /openapi/v3/api/v1 and /openapi/v3/apis/apps/v1 documents (parsed JSON); fields:
    pc_prepull_policy_fields. Every property of PodSpec, Container, DaemonSetSpec and PodTemplateSpec must be compared
    by the policy or be one of its deliberately uncompared fields. Returns sorted "<Schema>.<property>" strings; [] when
    covered. A schema missing from the documents is itself a gap (the check cannot prove anything then)."""
    schemas = {}
    for doc in openapi_docs or []:
        if isinstance(doc, dict):
            schemas.update(((doc.get("components") or {}).get("schemas")) or {})
    gaps = []
    for schema, key in sorted(_PREPULL_SCHEMAS.items()):
        s = schemas.get(schema)
        if not isinstance(s, dict) or not isinstance(s.get("properties"), dict):
            gaps.append("%s (schema not served)" % schema)
            continue
        covered = set(fields.get(key) or []) | set((fields.get("uncompared") or {}).get(schema) or [])
        gaps += ["%s.%s" % (schema.rsplit(".", 1)[1], p) for p in sorted(set(s["properties"]) - covered)]
    return gaps


_SANDBOX_TAG = r"^([0-9]{8})-[0-9a-f]{7}$"
_PIN_SOURCE = "ghcr.io/mutex-solutions/sandbox-%s:%s@%s"


def sandbox_portal_pins(pins, portal, host):
    """Merge deploy/templates/images.yaml (pins: name -> ref) with the portal's sandboxBuiltinPin records (portal: the
    list GET /admin/template-updates/sandbox/pins returns) for the sandbox registry host, so a make upgrade never renders
    an image older than the one the portal pinned (docs/template-updates.md §4). Per built-in image (matched on the
    registry repository <host>/library/sandbox-<name>, the same mapping as sandbox_images):

      no record, or the same digest                 -> images.yaml
      the record replaced exactly images.yaml's      -> the record (a newer mirror, or a deliberate rollback)
      otherwise the newer <YYYYMMDD> tag date wins; a record for an image images.yaml lacks is added

    Two different digests that cannot be ordered (same date, or a tag outside <YYYYMMDD>-<sha7>) raise ValueError: the
    upgrade stops rather than guess. Returns {"pins": merged name -> ref, "notes": [...], "commit": [images.yaml lines]}."""
    import re

    out = dict(pins or {})
    notes, commit = [], []
    by_repo = {}
    for name, ref in out.items():
        repo, tag, digest = _split_ref(ref)
        by_repo["%s/library/%s" % (host, repo.rsplit("/", 1)[-1])] = (name, tag, digest)
    for p in sorted(portal or [], key=lambda x: str(x.get("id", ""))):
        if not isinstance(p, dict) or p.get("registry") != host:
            continue
        name, tag, digest = str(p.get("template", "")), str(p.get("tag", "")), str(p.get("digest", ""))
        repo = "%s/library/sandbox-%s" % (host, name)
        if (p.get("id") != repo or not re.match(r"^[a-z0-9][a-z0-9-]{0,62}$", name) or not re.match(_SANDBOX_TAG, tag)
                or not re.match(r"^sha256:[0-9a-f]{64}$", digest)):
            raise ValueError("the portal's sandbox pin record %r is malformed; fix or delete it before upgrading" % p.get("id"))
        source = _PIN_SOURCE % (name, tag, digest)
        line = "  %s: %s" % (name, source)
        if repo not in by_repo:
            key = "sandbox-" + name if ("sandbox-" + name) not in out and name in out else name
            out[key] = source
            notes.append("sandbox-%s: images.yaml has no entry; the portal's pin %s:%s is kept" % (name, tag, digest))
            commit.append(line)
            continue
        key, ytag, ydigest = by_repo[repo]
        if ydigest == digest:
            continue
        _, _, prev_digest = _split_ref(p.get("previous", ""))
        if prev_digest and prev_digest == ydigest:
            winner = "pin"
        else:
            ym, pm = re.match(_SANDBOX_TAG, ytag or ""), re.match(_SANDBOX_TAG, tag)
            if not ym or ym.group(1) == pm.group(1):
                raise ValueError(
                    "sandbox-%s: deploy/templates/images.yaml pins %s and the portal pinned %s:%s (%s); which is newer "
                    "cannot be told. Commit the image you want in images.yaml (the portal's line is: %s) and run make "
                    "upgrade again" % (name, ydigest, tag, digest, p.get("action", "mirror"), line.strip()))
            winner = "pin" if pm.group(1) > ym.group(1) else "yaml"
        if winner == "pin":
            out[key] = source
            notes.append("sandbox-%s: keeping the portal's %s %s:%s over images.yaml's %s" % (name, p.get("action", "pin"), tag, digest, ydigest))
            commit.append("  %s: %s" % (key, source))
        else:
            notes.append("sandbox-%s: images.yaml's %s is newer than the portal's pin %s:%s" % (name, ytag, tag, digest))
    return {"pins": out, "notes": notes, "commit": commit}


def sandbox_registry_problem(sb, services_metallb, object_store_mode):
    """Why the sandbox registry (stage 55, Zot on the services cluster) cannot be built from these settings, or ""."""
    import ipaddress

    reg = sb.get("registry", {})
    try:
        a = ipaddress.ip_address(str(reg.get("address") or ""))
    except ValueError:
        return "sandbox.registry.address must be an IPv4 address: the registry's MetalLB address on the services cluster"
    first, _, last = str(services_metallb or "").partition("-")
    try:
        lo, hi = ipaddress.ip_address(first.strip()), ipaddress.ip_address((last or first).strip())
    except ValueError:
        return "services.network.metallb must be set (a-b) to place the sandbox registry"
    if a.version != 4 or not lo <= a <= hi:
        return "sandbox.registry.address %s must be inside services.network.metallb %s" % (a, services_metallb)
    if object_store_mode not in ("vm", "truenas-app", "ceph-rgw", "external"):
        return "object_store.mode %r has no bucket for the sandbox registry" % object_store_mode
    return ""


class FilterModule:
    def filters(self):
        return {
            "pc_sandbox_images": sandbox_images,
            "pc_sandbox_templates": sandbox_templates,
            "pc_sandbox_portal_pins": sandbox_portal_pins,
            "pc_prepull_policy_gaps": prepull_policy_gaps,
            "pc_sandbox_registry_problem": sandbox_registry_problem,
            "pc_classify_storages": classify_storages,
            "pc_default_profile": default_profile,
            "pc_kept_profiles": kept_profiles,
            "pc_kept_default_profile": kept_default_profile,
            "pc_storage_classes": storage_classes,
            "pc_pve_connection": pve_connection,
            "pc_registered_pve_provider": registered_pve_provider,
            "pc_fio_class": fio_class,
            "pc_template_nodes": template_nodes,
            "pc_acl_missing": pve_acl_missing,
            "pc_boot_iso_problem": boot_iso_problem,
            "pc_iso_stores": iso_stores,
            "pc_visible_stores": visible_stores,
            "pc_ip_cidrs": ip_cidrs,
            "pc_node_with_address": node_with_address,
            "pc_sandbox_net_problem": sandbox_net_problem,
            "pc_sandbox_guardrail_problem": sandbox_guardrail_problem,
            "pc_sandbox_edge_problem": sandbox_edge_problem,
        }
