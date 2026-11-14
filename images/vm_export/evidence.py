"""The ONE public evidence record of an accepted export run: hashes, check results, versions and run identity.

Built by allowlist. Every key is named here and every string must match the exact pattern or value named
for it, so nothing private can ride along: no host path, node, storage, pool, VMID, address, token id,
container id or guest data (the inventory itself stays private; only its sha256 is published). check()
re-validates the finished record against the schema and refuses any other key, and any string that
looks like an address or a path.
"""
import hashlib
import json
import re

import vm_artifact
import vm_publish_stage
from vm_export import capsule, coldboot_guest, postscan_probe

HEX = r"[0-9a-f]{64}"
# The only strings that may contain "/": public URLs and registry references, each with its own pattern.
SLASH_OK = ("evidence.repository", "evidence.run_ref", "evidence.reference", "evidence.tool_image.reference",
            "evidence.source.upstream_url")
PRIVATE = re.compile(r"(?<![0-9.])(?:[0-9]{1,3}\.){3}[0-9]{1,3}(?![0-9.])"  # IPv4
                     r"|(?:[0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}"  # MAC
                     r"|[!\\\s]|@pve|\.\.")  # token id, odd characters


class EvidenceRefused(ValueError):
    """Names the refused field only, never its value."""


def need(ok, field):
    if not ok:
        raise EvidenceRefused("evidence refused: " + field)


def true(value):
    return value is True


def false(value):
    return value is False


def none(value):
    return value is None


def exactly(expected):
    return lambda value: type(value) is type(expected) and value == expected


def count(value):
    return type(value) is int and 0 <= value < 1 << 20


def size(value):
    return type(value) is int and 0 < value <= 100 << 30


def check_ids(plan):
    k8s = plan["kubernetes_version"]
    return {"inventory": "k8s-inventory-" + k8s if k8s else plan["os"] + "-inventory",
            "sanitize": "sanitize-" + plan["os"] + ("-k8s-" + k8s if k8s else ""),
            "postscan": "postscan-" + ("k8s-" + k8s if k8s else plan["os"])}


def schema(plan):
    """The exact shape of the record for this run's variant. A str is a pattern, a callable a predicate."""
    k8s = plan["kubernetes_version"]
    ids = check_ids(plan)
    source = "source.raw" if k8s else "source.qcow2"
    # Names derived from the validated run identity: no free-form string is accepted for them.
    commit = plan["source_commit"] if type(plan["source_commit"]) is str else ""
    tag = "%s-%s-%s-%s" % (plan["os"], "k8s-" + k8s if k8s else "os", plan["build_version"], commit[:12])
    name = "%s-%s-%s-%s" % ("vm-k8s-export" if k8s else "vm-export", plan["os"], plan["build_version"], commit[:12])
    package = "ghcr.io/%s-vm-%s%s" % (vm_publish_stage.REPOSITORY, plan["os"], "-k8s" if k8s else "")
    shared = ("vm_sanitize.py", "k8s_copy_policy.py")
    review = ("k8s-review.json",) if k8s else ()

    def run(check, pins):
        return {"check_id": exactly(check), "image": exactly(plan["tool_image_id"]), "exit_code": exactly(0),
                "attach_exit_code": exactly(0), "stop_reason": none, "cgroup_empty_or_absent": true,
                "cleanup_errors": exactly([]), "stderr_bytes_discarded": count, "pins": dict.fromkeys(pins, HEX)}
    guest = coldboot_guest.COMMON + (coldboot_guest.K8S if k8s else coldboot_guest.OS_ONLY)
    clone = {"suffix": r"[ab]", "all_passed": true, "checks": dict.fromkeys(guest, true), "snapd_present": true,
             "snapd_changes_error_lines": count, "cloud_init_recoverable_warnings": count}
    return {
        "kind": exactly("VmExportEvidence"), "schema_version": exactly(1),
        "repository": exactly(vm_publish_stage.REPOSITORY), "source_commit": r"[0-9a-f]{40}",
        "run_id": lambda v: type(v) is int and v > 0, "run_attempt": lambda v: type(v) is int and v > 0,
        "run_ref": exactly("https://github.com/%s/actions/runs/%s" % (vm_publish_stage.REPOSITORY, plan["run_id"])),
        "variant": exactly("kubernetes" if k8s else "os"), "os": r"ubuntu-2[46]04",
        "kubernetes_version": lambda v: v is None if k8s is None else v == k8s and k8s in vm_publish_stage.K8S_VERSIONS,
        "build_version": r"[0-9]{14}", "artifact_name": exactly(name), "image_tag": exactly(tag),
        "reference": exactly(package + ":" + tag),
        "tool_image": {"reference": exactly(capsule.IMAGE_REFERENCE), "local_id": exactly(plan["tool_image_id"])},
        "source": {"raw_sha256": HEX, "raw_bytes": size, "source_identity": "sha256:" + HEX,
                   "upstream_checksum": r"(?:[0-9a-f]{64}|[0-9a-f]{128})",
                   "upstream_url": r"https://(?:cloud-images|releases)\.ubuntu\.com/[A-Za-z0-9._/-]{1,200}",
                   "template_unchanged_stopped": true, "source_identity_before_after_equal": true,
                   "source_hash_reread_equal": true, "disk_parsed_or_guest_executed": false},
        "inventory": dict(run(ids["inventory"], (source, "inspect_k8s.py" if k8s else "inspect_os.py", *shared)),
                          clean=true, accepted=true, inventory_sha256=HEX),
        "review": ({"sha256": HEX, "package_policy_sha256": HEX, "baseline_sha256": HEX} if k8s
                   else {"sanitize_eligible": true}),
        "sanitize": dict(run(ids["sanitize"], (source, "stream_sanitized.py", *shared, *review)),
                         sealed_after_cgroup_stop=true, disk_sha256=HEX, disk_bytes=size, virtual_size_bytes=size,
                         checks=dict.fromkeys(vm_artifact.CHECKS, true)),
        "postscan": dict(run(ids["postscan"], ("source.qcow2", "postscan_probe.py", *shared, *review)), clean=true,
                         disk_sha256=HEX, result=exactly(postscan_probe.expected("kubernetes" if k8s else "os",
                                                                                 k8s or plan["os"]))),
        "coldboot": {"accepted": true, "reason": none, "step": exactly("done"), "pve_tls_verified": lambda v: type(v) is bool,
                     "api_node_room": r"checked|unreadable", "disk_sha256": HEX, "clones": [clone, clone],
                     "distinct_identity": dict.fromkeys(("machine_id_sha256", "host_key_sha256", "snapd_serial_sha256",
                                                         "snapd_device_key_sha256", "snapd_present"), true),
                     "cleanup": {"clones": {"a": exactly("destroyed"), "b": exactly("destroyed")},
                                 "import_volume": exactly("deleted"), "private_key_deleted": true},
                     "protected_set_unchanged": true, "storage_set_unchanged": true},
        "bundle": {"manifest_sha256": HEX, "disk_sha256": HEX, "disk_bytes": size, "virtual_size_bytes": size},
    }


def conform(value, shape, path="evidence"):
    """Exact keys at every level; every leaf passes its pattern or predicate."""
    if type(shape) is dict:
        need(type(value) is dict and set(value) == set(shape), path)
        for key in shape:
            conform(value[key], shape[key], path + "." + key)
    elif type(shape) is list:
        need(type(value) is list and len(value) == len(shape), path)
        for index, item in enumerate(shape):
            conform(value[index], item, "%s[%d]" % (path, index))
    elif type(shape) is str:
        need(type(value) is str and re.fullmatch(shape, value, flags=re.ASCII) is not None, path)
    else:
        need(shape(value) is True, path)


def scan(value, path="evidence"):
    """Belt and braces after the schema: no key or string that looks like an address, a path or a token id."""
    if type(value) is dict:
        for key, item in value.items():
            need(type(key) is str and re.fullmatch(r"[A-Za-z0-9_.-]{1,64}", key) is not None
                 and PRIVATE.search(key) is None, path)
            scan(item, path + "." + key)
    elif type(value) is list:
        for item in value:
            scan(item, path)
    elif type(value) is str:
        need(PRIVATE.search(value) is None and (path in SLASH_OK or "/" not in value), path)
    else:
        need(value is None or type(value) in (bool, int), path)


def check(evidence, plan):
    conform(evidence, schema(plan))
    scan(evidence)
    return evidence


def pick(row, *names):
    return {name: row[name] for name in names}


def capsule_run(row, *more):
    """The public part of a capsule run record: never the container id, the cgroup path or a host path."""
    public = pick(row, "check_id", "image", "exit_code", "attach_exit_code", "stop_reason", "cgroup_empty_or_absent",
                  "cleanup_errors", "stderr_bytes_discarded", *more)
    # By file name (the guest path is a fixed container path); the schema names the exact set per capsule.
    public["pins"] = {guest.rsplit("/", 1)[-1]: digest for guest, digest in row["pins"].items()}
    need(len(public["pins"]) == len(row["pins"]), "pins")
    return public


def build(plan, source, receipt, inventory_run, review, baseline, sanitize_run, postscan_run, coldboot, manifest,
          manifest_bytes):
    """plan: the pipeline plan plus tool_image_id. review/baseline: the bytes of the derived k8s-review.json
    and of its committed baseline (None for a native export). Raises EvidenceRefused (field names only)
    when a record is missing a field, the records do not name the same bytes, or the result fails check()."""
    try:
        k8s = plan["kubernetes_version"]
        facts = sanitize_run["facts"]
        raw = "/input/source.raw" if k8s else "/input/source.qcow2"
        disk = facts["disk_sha256"]
        need(receipt["raw_sha256"] == inventory_run["pins"][raw] == sanitize_run["pins"][raw], "source_chain")
        need(disk == postscan_run["disk_sha256"] == postscan_run["pins"]["/input/source.qcow2"]
             == coldboot["disk_sha256"] == manifest["disk"]["sha256"], "disk_chain")
        need(manifest == vm_artifact.decode(manifest_bytes), "manifest")
        vm_artifact.validate(manifest)
        export = manifest["clean_export_receipt"]
        need((manifest["source_commit"], manifest["build_version"], manifest["os"], manifest.get("kubernetes_version"))
             == (plan["source_commit"], plan["build_version"], plan["os"], k8s), "manifest_plan")
        if k8s:
            digest = hashlib.sha256(review).hexdigest()
            need(digest == sanitize_run["pins"]["/input/k8s-review.json"]
                 == postscan_run["pins"]["/input/k8s-review.json"], "review_chain")
            reviewed = {"sha256": digest, "package_policy_sha256": json.loads(review)["package_policy_sha256"],
                        "baseline_sha256": hashlib.sha256(baseline).hexdigest()}
        else:
            reviewed = {"sanitize_eligible": inventory_run["accepted"]}
        evidence = {
            "kind": "VmExportEvidence", "schema_version": 1, "repository": vm_publish_stage.REPOSITORY,
            **pick(plan, "source_commit", "run_id", "run_attempt", "variant", "os", "kubernetes_version", "build_version",
                   "artifact_name"),
            "run_ref": export["run_ref"], "image_tag": manifest["image_tag"],
            "reference": vm_artifact.plan(manifest)["reference"],
            "tool_image": {"reference": capsule.IMAGE_REFERENCE, "local_id": plan["tool_image_id"]},
            "source": dict(pick(receipt, "raw_sha256", "raw_bytes", "template_unchanged_stopped",
                                "source_identity_before_after_equal", "source_hash_reread_equal",
                                "disk_parsed_or_guest_executed"),
                           source_identity=export["source_identity"], **pick(source, "upstream_checksum", "upstream_url")),
            "inventory": capsule_run(inventory_run, "clean", "accepted", "inventory_sha256"),
            "review": reviewed,
            "sanitize": dict(capsule_run(sanitize_run, "sealed_after_cgroup_stop"),
                             **pick(facts, "disk_sha256", "disk_bytes", "virtual_size_bytes", "checks")),
            "postscan": capsule_run(postscan_run, "clean", "disk_sha256", "result"),
            "coldboot": dict(pick(coldboot, "accepted", "reason", "step", "pve_tls_verified", "api_node_room", "disk_sha256",
                                  "distinct_identity"),
                             clones=[pick(row, "suffix", "all_passed", "checks", "snapd_present",
                                          "snapd_changes_error_lines", "cloud_init_recoverable_warnings")
                                     for row in coldboot["clones"]],
                             cleanup=pick(coldboot["cleanup"], "clones", "import_volume", "private_key_deleted"),
                             protected_set_unchanged=coldboot["protected_set"]["unchanged"],
                             storage_set_unchanged=coldboot["storage_set"]["unchanged"]),
            "bundle": {"manifest_sha256": hashlib.sha256(manifest_bytes).hexdigest(), "disk_sha256": disk,
                       "disk_bytes": manifest["disk"]["bytes"],
                       "virtual_size_bytes": manifest["disk"]["virtual_size_bytes"]},
        }
    except (KeyError, TypeError, AttributeError, ValueError) as exc:
        if isinstance(exc, EvidenceRefused):
            raise
        raise EvidenceRefused("evidence refused: incomplete records (%s)" % type(exc).__name__) from None
    return check(evidence, plan)


def encode(evidence):
    return (json.dumps(evidence, indent=2, sort_keys=True) + "\n").encode("ascii")
