"""SOURCE-ONLY prototype: no IO, target imports, execution, or publication authority.

Adaptation point after independent source PR/review: inside the parent's confined
COPY capsule, after read-only layout/account/prejoin inventory and BEFORE cleanup,
call validate_inventory. Replace (do not extend) frozen cleanup's blanket cloud
cfg.d removal and ubuntu-account assumption with reviewed recommendations. Retain
safe/symlink checks, journal reset/free-space zeroing and fresh readonly postscan
in that future backend. This module implements NONE of those actions.

Inventory is bounded metadata from a trusted confined producer, NOT guest-provided
JSON, log contents, hashes/passwords, or an assertion of cleanliness. Parent must
verify raw framing/EOF/20GiB/source hash/stopped-source binding independently;
parent facts below are required inputs, not verification by this module. Input is
root volume ONLY: old EFI vars and cloud-init media are never candidate artifacts.
Per-build cloud drop-in review must come independently of inventory, with exact name/hash
coverage (including 50-cloud-init/network-disable/lab files if present). There is
NO Ubuntu26 vendor whitelist yet. Never supply synthetic fixtures as evidence.
Reviewed against a read-only inventory of the real template root (owner decision):
netplan-systemd-networkd, and the UPSTREAM cloud-init default user `ubuntu` from the
packaged base cloud.cfg. cloud-init creates `ubuntu` at first boot, so it must NOT
pre-exist; `builder` stays the exact retained native_login account, locked, with no
SSH keys, no supplementary group (sudo/adm/lxd/... are dropped copy-only) and sudo
policy bound to parent-reviewed stock bytes. snapd's registered device identity is
reset to unregistered (copy-only). Another default user, a pre-existing ubuntu
account, missing builder, unknown backend/config, or another partition needs
inspection and separate policy review, NOT guessing or rewriting source facts.
No return value satisfies vm_artifact checks or changes its kubernetes contract.
"""
from dataclasses import dataclass
import re

VIRTUAL_BYTES = 20 << 30
REFUSAL = "k8s copy policy refused"
# GUEST paths/patterns, descriptive only; never expand these on the host.
COPY_WIPE = ("/home/builder/.ssh", "/root/.ssh", "/etc/ssh/ssh_host_*",
             "/var/lib/cloud", "/run/cloud-init", "/var/log/cloud-init*",
             "/var/log/installer", "/var/lib/dbus/machine-id", "/etc/apt/auth.conf",
             "/etc/apt/auth.conf.d/*", "/root/.docker/config.json",
             "/home/builder/.docker/config.json",
             # snapd registered identity from image seeding (owner decision: reset to unregistered;
             # snapd re-registers on first boot). Device key files and the serial assertion subtree;
             # backend must verify both are gone/empty afterwards. state.json is rewritten (RESET).
             "/var/lib/snapd/device/private-keys-v1/*", "/var/lib/snapd/assertions/asserts-v0/serial")
DEFAULT_USER = "ubuntu"  # Upstream cloud-init default user (owner decision); created at first boot.
RETAIN = ("/etc/kubernetes", "/etc/containerd", "/etc/crictl.yaml",
          "/etc/cni/net.d", "/opt/cni/bin", "/var/lib/dpkg", "/var/lib/apt/extended_states",
          "kubeadm/kubelet/kubectl and CRI binaries", "package holds and runtime versions",
          "kernel/initramfs/GRUB", "kubelet units and enabled=True/active=False",
          "reviewed netplan-systemd-networkd mechanism and upstream ubuntu default-user base cloud config",
          "locked builder local account: own primary group only, no keys, no supplementary groups",
          "stock sudoers and sudoers.d README bound to parent-reviewed sha256 (no other sudo rule)",
          "snapd seed, snaps, brand/model and all other state.json content")
IDENTITY_RESET = ("drop builder from every supplementary group (group members, gshadow admins+members)",
                  "reset snapd to unregistered: remove state.json data.auth.device serial, key-id and "
                  "session-macaroon (keep brand/model and everything else)")
CLUSTER = {"kubernetes_pki", "admin_conf", "bootstrap_kubelet_conf", "kubelet_conf",
           "kubelet_pki", "etcd_db", "etcd_other", "static_manifests", "workloads"}


class PolicyRefusal(ValueError):
    """Fixed bounded text: never echo guest input, paths, or secrets."""

    def __init__(self):
        super().__init__(REFUSAL)


@dataclass(frozen=True)
class CopyPlan:
    status: str = "NEEDS_INSPECTION"
    public_eligible: bool = False
    copy_only: bool = True
    wipe: tuple = ()
    retain: tuple = ()
    reset: tuple = ()
    reviewed_dropins: tuple = ()
    remaining: tuple = ("independent source review", "confined copy implementation",
                        "independent postscan", "fresh BIOS coldclone acceptance",
                        "explicit public approval and anonymous pull")


def _need(ok):
    if not ok:
        raise PolicyRefusal()


def _keys(value, names):
    _need(type(value) is dict and set(value) == set(names))


def _text(value, pattern):
    return type(value) is str and len(value) <= 128 and re.fullmatch(pattern, value, re.ASCII) is not None


def _count(value):
    _need(type(value) is int and 0 <= value <= 20000)


def _dropin_name(value):
    # *.cfg, or cloud-init's shipped README by exact name only; it still needs an exact
    # reviewed (name, sha256, action) row like every other entry. No other relaxation.
    return value == "README" or _text(value, r"[A-Za-z0-9_-][A-Za-z0-9_.-]{0,95}\.cfg")


def validate_inventory(facts, reviewed_dropins=()):
    """Return recommendations only; missing/unknown/mismatched facts fail closed.

    Exact schema below; booleans are bool, counts/versions are int, never 0/1 bool
    substitutes. accounts contains passwd *markers*, not shadow hash bytes;
    shadow_marker accepts only unrecoverable locked markers. No auto-normalizing.
    cluster counts include symlink/dangling entries: never follow or ignore them. The
    producer excludes from static_manifests ONLY image-builder's exact 0-byte regular
    non-symlink `.kubelet-keep` placeholder; any other entry still counts and refuses.
    hygiene.unclassified_credentials excludes ONLY regular, single-link, non-dot snapd
    device-key files that the `private-keys-v1/*` COPY_WIPE removes; systemd
    credential.secret, and any other/irregular entry there, still counts and refuses.
    identity records builder's supplementary-group count (any count: the copy drops
    builder from all of them), whether builder's passwd gid is its own unshared
    `builder` group (must be True; never rewritten) and snapd registration
    PRESENCE/counts only. Store
    user credentials (session macaroon or any snapd user) refuse; otherwise the plan
    resets snapd to unregistered.
    bootstrap_token_files is only a bounded count of reviewed prejoin token files;
    backend must curate exact guest leaves separately, not erase cluster identity.
    reviewed_dropins: tuple of (basename, sha256, 'preserve_vendor'|'remove_build'),
    independently approved for THIS source; default is deliberately no whitelist.
    firstboot.cloud_base_config_reviewed must cover observed base config and the
    effective default user/network mechanism, not just presence of cloud-init.
    firstboot.cloud_default_user must be exactly the upstream "ubuntu", which must not
    already exist in accounts (cloud-init creates it); builder is never the default user.
    retained_package_policy_reviewed/package_policy_sha256 are parent review
    preconditions binding observed package versions, holds, CRI config and exact
    runtime baseline. This pure validator cannot authenticate that review or hash;
    runtime_version syntax alone does NOT establish a supported/clean CRI version.
    kubernetes_packages records normalized upstream release facts from a reviewed
    producer, not raw distro package-version strings silently rewritten here.
    """
    _keys(facts, ("parent", "os", "layout", "accounts", "k8s", "cloud_dropins", "hygiene", "firstboot",
                  "identity"))
    p = facts["parent"]
    _keys(p, ("format", "virtual_bytes", "raw_bytes", "source_hash_verified", "source_stopped"))
    _need(type(p["format"]) is str and p["format"] == "raw")
    for key in ("virtual_bytes", "raw_bytes"):
        _need(type(p[key]) is int and p[key] == VIRTUAL_BYTES)
    _need(p["source_hash_verified"] is True and p["source_stopped"] is True)
    o = facts["os"]
    _keys(o, ("distro", "major", "minor"))
    _need(type(o["distro"]) is str and o["distro"] == "ubuntu")
    _need(type(o["major"]) is int and o["major"] == 26)
    _need(type(o["minor"]) is int and o["minor"] == 4)
    l = facts["layout"]
    _keys(l, ("bios", "partition_table", "partition_count", "root_partition_number",
              "root_fs", "root_count", "mountpoints", "esp_count", "extra_filesystems",
              "external_fstab", "unsafe_links", "swap_entries"))
    for key, expected in (("bios", "seabios"), ("partition_table", "msdos"), ("root_fs", "ext4")):
        _need(type(l[key]) is str and l[key] == expected)
    for key in ("root_count", "partition_count", "root_partition_number"):
        _need(type(l[key]) is int and l[key] == 1)
    _need(type(l["mountpoints"]) is list and l["mountpoints"] == ["/"])
    _need(l["external_fstab"] is False)
    # swap_entries: swap files (/swap.img, /swapfile), ACTIVE (uncommented) fstab swap rows and
    # swap units; a commented fstab row is inert and not counted by the producer. Refusal,
    # never removal, so the parent learns the template's real state.
    for key in ("esp_count", "extra_filesystems", "unsafe_links", "swap_entries"):
        _count(l[key])
        _need(l[key] == 0)
    accounts = facts["accounts"]
    _need(type(accounts) is list and 2 <= len(accounts) <= 256)
    seen = set()
    for a in accounts:
        _keys(a, ("name", "uid", "home", "shell", "kind", "passwd_marker", "shadow_marker"))
        _need(_text(a["name"], r"[a-z_][a-z0-9_-]{0,30}\$?") and a["name"] not in seen)
        seen.add(a["name"])
        _need(type(a["uid"]) is int and 0 <= a["uid"] <= 65534)
        _need(type(a["passwd_marker"]) is str and a["passwd_marker"] in ("x", "!", "*", "!!"))
        _need(type(a["shadow_marker"]) is str and a["shadow_marker"] in ("!", "!!", "*", "!*"))
        _need(_text(a["home"], r"/(?:[A-Za-z0-9_-]+/)*[A-Za-z0-9_-]*"))
        _need(type(a["shell"]) is str and type(a["kind"]) is str)
        _need((a["name"] == "root" and a["uid"] == 0 and a["home"] == "/root" and
               a["shell"] == "/bin/bash" and a["kind"] == "root") or
              (a["name"] == "builder" and a["uid"] == 1000 and a["home"] == "/home/builder" and
               a["shell"] == "/bin/bash" and a["kind"] == "native_login") or
              (a["name"] not in ("root", "builder") and a["kind"] == "system" and
               a["shell"] in ("/usr/sbin/nologin", "/sbin/nologin", "/bin/false") and
               (0 < a["uid"] < 1000 or (a["name"] == "nobody" and a["uid"] == 65534))) or
              # base-passwd's fixed sync entry only; no other /bin/sync or shell exception.
              (a["name"] == "sync" and a["uid"] == 4 and a["home"] == "/bin" and
               a["shell"] == "/bin/sync" and a["kind"] == "system"))
    _need({"root", "builder"} <= seen)
    b = facts["firstboot"]
    _keys(b, ("network_backend", "cloud_default_user", "cloud_base_config_reviewed"))
    _need(type(b["network_backend"]) is str and b["network_backend"] == "netplan-systemd-networkd")
    _need(type(b["cloud_default_user"]) is str and b["cloud_default_user"] == DEFAULT_USER)
    # cloud-init creates the default user at first boot: a pre-existing ubuntu account refuses,
    # whatever its kind/shell (a nologin uid<1000 'system' ubuntu row is not acceptable either).
    _need(DEFAULT_USER not in seen and b["cloud_base_config_reviewed"] is True)
    ident = facts["identity"]
    _keys(ident, ("builder_supplementary_groups", "builder_primary_group_own", "snapd"))
    _count(ident["builder_supplementary_groups"])  # Recorded; any count is removed by the copy.
    # The primary gid is NOT rewritten: it must already be builder's own, unshared group (sudo %group
    # rules and group-owned sockets also match a primary gid, e.g. gid 27 = sudo).
    _need(ident["builder_primary_group_own"] is True)
    sn = ident["snapd"]
    _keys(sn, ("state", "device_serial", "device_key_id", "session_macaroon", "users",
               "serial_assertions", "device_keys"))
    for key in ("state", "device_serial", "device_key_id", "session_macaroon"):
        _need(type(sn[key]) is bool)
    for key in ("users", "serial_assertions", "device_keys"):
        _count(sn[key])
    # Store user credentials are not machine identity that snapd regenerates: refuse, never wipe.
    _need(sn["session_macaroon"] is False and sn["users"] == 0)
    _need(sn["state"] or not (sn["device_serial"] or sn["device_key_id"]))
    k = facts["k8s"]
    _keys(k, ("version", "kubelet_enabled", "kubelet_active", "cluster", "runtime_version",
              "bootstrap_token_files", "kubernetes_packages", "retained_package_policy_reviewed",
              "package_policy_sha256"))
    _need(type(k["version"]) is str and k["version"] == "1.35.10")
    _need(k["kubelet_enabled"] is True and k["kubelet_active"] is False)
    _keys(k["kubernetes_packages"], ("kubeadm", "kubelet", "kubectl"))
    for version in k["kubernetes_packages"].values():
        _need(type(version) is str and version == "1.35.10")
    _need(k["retained_package_policy_reviewed"] is True)
    _need(_text(k["package_policy_sha256"], r"[0-9a-f]{64}"))
    _need(_text(k["runtime_version"], r"[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}(?:[-+][A-Za-z0-9.-]{1,48})?"))
    _keys(k["cluster"], CLUSTER)
    for count in k["cluster"].values():
        _count(count)
        _need(count == 0)  # Existing cluster/workload state is NOT a deletion recommendation.
    _count(k["bootstrap_token_files"])
    h = facts["hygiene"]
    _keys(h, ("unsafe_apt_repo_urls", "registry_config_credentials", "unclassified_credentials"))
    for count in h.values():
        _count(count)
        _need(count == 0)  # Unknown URLs/embedded credentials require separate curated review.
    dropins = facts["cloud_dropins"]
    _need(type(dropins) is list and len(dropins) <= 128)
    _need(type(reviewed_dropins) is tuple and len(reviewed_dropins) <= 128)
    reviewed = {}
    for row in reviewed_dropins:
        _need(type(row) is tuple and len(row) == 3)
        name, digest, action = row
        _need(_dropin_name(name))
        _need(_text(digest, r"[0-9a-f]{64}") and type(action) is str and
              action in ("preserve_vendor", "remove_build") and name not in reviewed)
        reviewed[name] = (digest, action)
    observed = set()
    for d in dropins:
        _keys(d, ("name", "sha256", "regular_no_symlinks"))
        _need(_dropin_name(d["name"]))
        _need(_text(d["sha256"], r"[0-9a-f]{64}") and d["regular_no_symlinks"] is True)
        _need(d["name"] not in observed and d["name"] in reviewed)
        observed.add(d["name"])
        _need(d["sha256"] == reviewed[d["name"]][0])
    _need(observed == set(reviewed))
    return CopyPlan(status="COPY_POLICY_ONLY_NEEDS_REVIEW", wipe=COPY_WIPE, retain=RETAIN,
                    reset=("empty /etc/machine-id", "lock account tables; preserve locked builder account",
                           "ubuntu default user is created by cloud-init at first boot, not shipped",
                           *IDENTITY_RESET,
                           "generic personal-state/network cleanup and journal/free-space reset",
                           "curate reviewed prejoin bootstrap-token leaves; never cluster state"),
                    reviewed_dropins=reviewed_dropins)
