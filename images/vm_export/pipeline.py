"""Driver of the VM export job: one stage per workflow step, each bound to the previous stage's record.

    cd images && python3 -m vm_export.pipeline <stage>

Stages run in this order only (<work>/stages.json lists what completed; a stage refuses unless exactly
the earlier ones did):

    plan       validate the dispatch inputs and the run identity; create the private work directory
    config     validate the runner configuration (secrets, variables) without any API call
    tools      pull the pinned tool image by registry digest, with an empty Docker client configuration
    copy       PVE template guard, read-only copy from the read-only storage mount, guard again
    inventory  read-only inventory capsule over the private copy
    review     kubernetes: derive the review from the committed baseline; os: the eligibility gate
    sanitize   offline sanitizer capsule, sealed disk
    postscan   independent read-only capsule over the sealed disk
    coldboot   two clones through the PVE API, guest checks, guarded cleanup
    bundle     the three-file bundle (vm_artifact.prepare), re-checked exactly as the publisher checks it
    evidence   the one public evidence record (vm_export.evidence)

Two more commands are not stages:

    recover    guarded cold-boot cleanup (vm_export.coldboot cleanup) of every state directory on this
               runner: this run's and any an earlier run left. A directory is deleted only when its
               cleanup is recorded clean; otherwise the command fails. The workflow runs it before the
               first stage that touches the lab and again whatever happened (`if: always()`).
    purge      whatever happened before (`if: always()`, last): deletes the private copy, the scratch and
               every other work file. A capsule container that was not proven stopped is removed first.

The work directory is $RUNNER_TEMP/vm-export; the runner empties it after every job. The cold-boot state
(what a run created on the lab) must outlive a failed job, so it lives in $HOME/.proxcloud-vm-export-coldboot,
one directory per run. Settings come from the environment (the workflow maps inputs, secrets and
variables to it); nothing private is printed: a refusal is one fixed reason code.
"""
import argparse
import datetime
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import sys
import types

import vm_artifact
import vm_publish_stage
import vm_sanitize
from vm_export import bundle, capsule, coldboot, copy, evidence, inventory, postscan, review, sanitize

HERE = Path(__file__).resolve().parent
STAGES = ("plan", "config", "tools", "copy", "inventory", "review", "sanitize", "postscan", "coldboot", "bundle",
          "evidence")
DIRS = ("private", "scratch", "docker-anonymous", "inventory", "review", "sanitize", "postscan", "coldboot", "bundle",
        "evidence")
UNRESOLVED = "unresolved-container"  # The id of a capsule container that was not proven stopped.
FILES = ("plan.json", "stages.json", "stages.json.tmp", "tools.json", "source.json", UNRESOLVED)
STATE_ROOT = ".proxcloud-vm-export-coldboot"
OSES = ("ubuntu-2404", "ubuntu-2604")
EVIDENCE = "vm-export-evidence.json"
REQUIRED = ("PVE_URL", "PVE_TOKEN_ID", "PVE_TOKEN_SECRET", "PVE_NODES", "PVE_TEMPLATE_STORAGE", "PVE_BRIDGE",
            "BUILD_GATEWAY", "EXPORT_TEST_ADDRESSES")
# Secrets too, but not needed by every export: the import storage (native), the ISO storage (kubernetes;
# falls back to the template storage, as in images/k8s-node-template.sh) and the name servers.
OPTIONAL = ("PVE_IMPORT_STORAGE", "PVE_ISO_STORAGE", "BUILD_NAMESERVER")
CLOCK_SKEW = 300  # Seconds allowed between the two lab clocks behind a file time and a VM creation time.
CLONE_VMIDS = (9290, 9299)
WORK_ROOM = 48 << 30  # Private copy aside: sanitizer scratch (24 GiB floor), sealed disk and its bundle copy.
# The sanitizer runs under TCG: about 12 minutes on the workstation it was measured on. The launcher's own
# default (30 minutes) leaves little room for a slower runner; raise this if a real run hits the wall limit.
SANITIZE_WALL_SECONDS = 3600


class Refused(Exception):
    """A fixed reason code: never a value of the environment, a path or guest data."""


def need(ok, code):
    if not ok:
        raise Refused(code)


def plain_dir(env, key, code):
    base = env.get(key, "")
    need(re.fullmatch(r"/[A-Za-z0-9/._-]+", base) is not None and ".." not in base and os.path.isdir(base), code)
    return Path(base)


def workdir(env):
    return plain_dir(env, "RUNNER_TEMP", "runner_temp_refused") / "vm-export"


def state_root(env):
    """Where the cold-boot state of each run lives: the runner user's home, NOT the runner's temp directory
    (emptied after every job), so an unclean cleanup still has its record when the next run starts."""
    return plain_dir(env, "HOME", "runner_home_refused") / STATE_ROOT


def load(path):
    return json.loads(Path(path).read_bytes().decode("ascii"))


def write(path, obj):
    capsule.write_new(path, (json.dumps(obj, indent=2, sort_keys=True) + "\n").encode("ascii"))


def plan(env, now=None):
    """The validated dispatch inputs and run identity. Reads nothing else: no secret, no API."""
    variant, os_name, version, vmid = (env.get("EXPORT_" + key, "") for key in
                                       ("VARIANT", "OS", "KUBERNETES_VERSION", "TEMPLATE_VMID"))
    version = "" if version == "none" else version  # The dispatch form's "no version" choice.
    need(variant in ("os", "kubernetes") and os_name in OSES, "invalid_inputs")
    need((os_name == "ubuntu-2604" and version in vm_sanitize.K8S_VERSIONS) if variant == "kubernetes"
         else version == "", "invalid_inputs")
    need(re.fullmatch(r"[1-9][0-9]{2,8}", vmid, flags=re.ASCII) is not None, "invalid_inputs")
    need(not CLONE_VMIDS[0] <= int(vmid) <= CLONE_VMIDS[1], "invalid_inputs")  # Reserved for the cold-boot clones.
    sha, run_id, attempt = (env.get("GITHUB_" + key, "") for key in ("SHA", "RUN_ID", "RUN_ATTEMPT"))
    need(env.get("GITHUB_REPOSITORY") == vm_publish_stage.REPOSITORY and env.get("GITHUB_REF") == "refs/heads/main"
         and env.get("GITHUB_EVENT_NAME") == "workflow_dispatch", "invalid_run")
    need(re.fullmatch(r"[0-9a-f]{40}", sha) is not None and re.fullmatch(r"[1-9][0-9]{0,19}", run_id) is not None
         and re.fullmatch(r"[1-9][0-9]{0,3}", attempt) is not None, "invalid_run")
    # A re-run would export with this run's commit, however old: main may have moved since. The hosted jobs
    # of a run may be re-run (they reuse the export job of attempt 1); the export itself needs a new dispatch.
    need(attempt == "1", "rerun_refused_dispatch_a_new_run")
    build_version = (now or datetime.datetime.now(datetime.timezone.utc)).strftime("%Y%m%d%H%M%S")
    name = "%s-%s-%s-%s" % ("vm-k8s-export" if variant == "kubernetes" else "vm-export", os_name, build_version, sha[:12])
    need(vm_publish_stage.selector(name) == (os_name, build_version, sha[:12], variant), "invalid_run")
    return {"variant": variant, "os": os_name, "kubernetes_version": version or None, "template_vmid": int(vmid),
            "source_commit": sha, "run_id": int(run_id), "run_attempt": int(attempt), "build_version": build_version,
            "artifact_name": name}


def settings(env, selected):
    """The validated runner configuration. Refusals name the variable, never its value."""
    env = {key: (env.get(key) or "").strip() for key in REQUIRED + OPTIONAL + ("EXPORT_VLAN", "EXPORT_SOURCE_MOUNT")}
    for key in REQUIRED + (() if selected["kubernetes_version"] else ("PVE_IMPORT_STORAGE",)):
        need(env[key] != "", "config_missing_" + key)
    for key in REQUIRED + OPTIONAL:
        # GitHub drops a job output that contains a masked value, and every secret of this job is masked. A
        # secret that can occur in a hex digest or in the artifact name would cost the publisher its inputs
        # after the whole export: refuse it now. (The VLAN is a variable for this reason: never a secret.)
        need(re.fullmatch(r"[0-9a-f]{1,16}", env[key]) is None and (env[key] or " ") not in selected["artifact_name"],
             "config_%s_would_hide_a_job_output" % key)
    need(re.fullmatch(r"[a-z0-9-]{1,63}", env["PVE_NODES"]) is not None, "config_PVE_NODES_must_be_one_node")
    vlan = env["EXPORT_VLAN"] or "0"
    need(re.fullmatch(r"0|[1-9][0-9]{0,3}", vlan) is not None and int(vlan) <= 4094, "config_EXPORT_VLAN")
    mount = env["EXPORT_SOURCE_MOUNT"] or "/srv/" + copy.STORAGE
    need(re.fullmatch(r"/[A-Za-z0-9/._-]+", mount) is not None and ".." not in mount, "config_EXPORT_SOURCE_MOUNT")
    cfg = {"node": env["PVE_NODES"], "storage": env["PVE_TEMPLATE_STORAGE"], "import_storage": env["PVE_IMPORT_STORAGE"],
           "iso_storage": env["PVE_ISO_STORAGE"] or env["PVE_TEMPLATE_STORAGE"],
           "bridge": env["PVE_BRIDGE"], "vlan": int(vlan), "mount": mount, "gateway": env["BUILD_GATEWAY"],
           "addresses": [a.strip() for a in env["EXPORT_TEST_ADDRESSES"].split(",")],
           # As images/build.py: the name server falls back to the gateway.
           "nameservers": re.split(r"[\s,]+", env["BUILD_NAMESERVER"] or env["BUILD_GATEWAY"])}
    try:  # The cold boot's own rules (two addresses of one network, the gateway in it), before any work.
        coldboot.settings(types.SimpleNamespace(
            test_address=cfg["addresses"], gateway=cfg["gateway"], nameserver=cfg["nameservers"],
            vmid_range="%d-%d" % CLONE_VMIDS, node=cfg["node"], storage=cfg["storage"], pool=coldboot.POOL,
            bridge=cfg["bridge"], vlan=cfg["vlan"], variant=selected["variant"], os=selected["os"],
            kubernetes_version=selected["kubernetes_version"], sanitize_run=None))
    except coldboot.Refused:
        raise Refused("config_cold_boot_settings") from None
    return cfg


def masks(cfg):
    """Values GitHub would not mask by itself: a secret is masked as a whole, not its parts."""
    values = [cfg["node"], cfg["gateway"], *cfg["nameservers"], *cfg["addresses"], *(a.split("/")[0] for a in cfg["addresses"])]
    return ["::add-mask::" + value for value in dict.fromkeys(values)]


def pve():
    from build import PVE  # Token client from PVE_URL/PVE_TOKEN_ID/PVE_TOKEN_SECRET.
    return coldboot.Client(PVE())  # Errors carry the HTTP status only, never a response body.


def k8s_images():
    spec = importlib.util.spec_from_file_location("k8s_build_config", HERE.parent / "k8s-build-config.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module.K8S_IMAGES


def baseline(selected):
    return HERE / "baselines" / (selected["os"] + "-k8s.json")


def template_identity(selected, client, node):
    """(name, exact tag list) the template guard must find. A Kubernetes node template is fully named by
    the inputs (images/k8s-node-template.sh); a native one carries its build time (images/build.py)."""
    os_name, version = selected["os"], selected["kubernetes_version"]
    if version:
        return "%s-kube-v%s" % (os_name, version), ["proxcloud", "pc-k8s", "pc-k8s-" + version.replace(".", "-"),
                                                    "pc-k8s-os-" + os_name]
    config = client.get("/nodes/%s/qemu/%d/config" % (node, selected["template_vmid"]))
    found = re.fullmatch(r"pc-%s-([0-9]{12})" % re.escape(os_name), str(config.get("name")), flags=re.ASCII)
    need(found is not None, "template_name")
    return found[0], ["proxcloud", "pc-public", "pc-img-" + os_name, "pc-v-" + found[1]]


def upstream(selected, api, cfg, name):
    """(checksum, url) of the vendor image the template was built from, from repository source only.

    Kubernetes: the installer ISO pinned in images/k8s-build-config.py at THIS commit. Native: the checksum
    the vendor publishes NOW; images/build.py verifies the vendor image against it at build time and keeps
    exactly one download per OS, named by the first 12 hex of that checksum, and the template must be the
    newest published one of its OS. Neither is recorded on the template, so both are accepted only when
    the node holds the vendor file of that name and that file is not newer than the template: a pin moved,
    or a vendor image downloaded by a later (even failed) build, refuses instead of being attributed."""
    # ponytail: a bound, not a proof (a vendor file that already was on the node when the template was built
    # from another one still passes). Record the checksum on the template in the two builders to close it.
    os_name, node, vmid = selected["os"], cfg["node"], selected["template_vmid"]
    if selected["kubernetes_version"]:
        image = k8s_images()[os_name]
        checksum, url, storage, content, wanted = image["iso_sha256"], image["iso_url"], cfg["iso_storage"], "iso", image["iso_name"]
    else:
        import build
        image = build.IMAGES[os_name]
        try:
            checksum = build.checksum_of(image)
        except (SystemExit, OSError, ValueError):
            raise Refused("upstream_checksum_unavailable") from None
        need(type(checksum) is str and re.fullmatch(r"[0-9a-f]{64}", checksum) is not None, "upstream_checksum_unavailable")
        url, storage, content, wanted = image["url"], cfg["import_storage"], "import", "pc-%s-%s.qcow2" % (os_name, checksum[:12])
    listed = api.call("GET", "/nodes/%s/storage/%s/content" % (node, storage), content=content)
    held = {str(v.get("volid")).rsplit("/", 1)[-1]: v.get("ctime") for v in listed}
    # Native: exactly one download of this OS. Kubernetes: the ISO store also holds other things.
    need(wanted in held and (content == "iso" or [n for n in held if n.startswith("pc-%s-" % os_name)] == [wanted]),
         "upstream_image_is_not_the_built_one")
    if content == "import":
        versions = build.versions_of(api.call("GET", "/cluster/resources", type="vm"), os_name, node)
        need(bool(versions) and versions[-1] == (name.rsplit("-", 1)[1], vmid), "template_not_newest")
    created = re.search(r"(?:^|,)ctime=([0-9]{1,12})(?:,|$)", str(api.call("GET", "/nodes/%s/qemu/%d/config" % (node, vmid)).get("meta")))
    need(type(held[wanted]) is int and held[wanted] > 0 and created is not None, "upstream_image_time_unknown")
    need(held[wanted] <= int(created[1]) + CLOCK_SKEW, "upstream_image_newer_than_the_template")
    return checksum, url


def os_gate(inventory_bytes, run, receipt):
    """The native eligibility gate: the inventory bytes are the ones a clean, accepted run recorded, that
    run read the file of the guarded copy, and the inventory says the image can be sanitized."""
    try:
        ok = (run["clean"] is True and run["accepted"] is True and run["stop_reason"] is None
              and run["cleanup_errors"] == [] and run["cgroup_empty_or_absent"] is True
              and run["inventory_sha256"] == hashlib.sha256(inventory_bytes).hexdigest()
              and run["pins"]["/input/source.qcow2"] == receipt["raw_sha256"]
              and receipt["kind"] == "TemplateDiskCopy" and receipt["template_unchanged_stopped"] is True
              and json.loads(inventory_bytes)["sanitize_eligible"] is True)
    except (KeyError, TypeError, ValueError):
        ok = False
    need(ok, "os_inventory_not_eligible")


class Run:
    """One stage call: the work directory, the plan and the environment."""

    def __init__(self, work, selected, env):
        self.work, self.plan, self.env = work, selected, env
        self.k8s = selected["kubernetes_version"]
        self.source = work / "private" / ("source.raw" if self.k8s else "source.qcow2")
        self.receipt = work / "private" / "copy-receipt.json"
        self.review = work / "review" / "k8s-review.json"
        self.sanitize_run = work / "sanitize" / "sanitize-run.json"
        self.disk = work / "sanitize" / "disk.qcow2"

    def variant_args(self):
        return ["k8s", "--kubernetes-version", self.k8s] if self.k8s else ["os", "--os", self.plan["os"]]

    def capsule_args(self, name):
        return ["--scratch-base", str(self.work / "scratch"), "--out-dir", str(self.work / name)]

    def config(self):
        for line in masks(settings(self.env, self.plan)):
            print(line)
        return 0

    def tools(self):
        write(self.work / "tools.json", {"reference": capsule.IMAGE_REFERENCE,
                                         "local_id": capsule.pull(self.work / "docker-anonymous")})
        return 0

    def copy(self):
        cfg, selected = settings(self.env, self.plan), self.plan
        need(os.path.isdir(cfg["mount"]) and os.statvfs(cfg["mount"]).f_flag & os.ST_RDONLY, "source_mount_not_read_only")
        api = pve()
        client = types.SimpleNamespace(get=lambda path: api.call("GET", path))
        name, tags = template_identity(selected, client, cfg["node"])
        snapshot = copy.template_guard(client, cfg["node"], selected["template_vmid"], name, tags)
        need(snapshot["volume"].endswith(self.source.suffix), "template_disk_format")
        size = os.lstat(copy.template_source(snapshot, cfg["mount"])).st_size
        # Kubernetes: a raw disk, exactly its virtual size. Native: a QCOW2 file, bounded as the sanitizer bounds it.
        need(size in vm_sanitize.VIRTUAL_BYTES["kubernetes"] if self.k8s
             else 0 < size <= vm_sanitize.NATIVE_SOURCE_MAX_BYTES, "template_disk_size")
        free = os.statvfs(self.work)
        need(free.f_bavail * free.f_frsize >= size + WORK_ROOM, "insufficient_work_space")
        checksum, url = upstream(selected, api, cfg, name)
        receipt = copy.guarded_copy(client, cfg["node"], selected["template_vmid"], name, tags, cfg["mount"], size,
                                    str(self.source))
        need(receipt["template"] == snapshot, "template_changed")
        write(self.receipt, receipt)
        write(self.work / "source.json", {"upstream_checksum": checksum, "upstream_url": url})
        print(json.dumps({"raw_sha256": receipt["raw_sha256"], "raw_bytes": receipt["raw_bytes"]}))
        return 0

    def inventory(self):
        versions = []
        if self.k8s:
            policy = load(baseline(self.plan))["package_policy"]
            versions = ["--containerd-version", policy["containerd"], "--runc-version", policy["runc"]]
        return inventory.main([*self.variant_args(), *versions, "--source", str(self.source), "--copy-receipt",
                               str(self.receipt), *self.capsule_args("inventory")])

    def review_stage(self):
        records = self.work / "inventory"
        if self.k8s:
            return review.main(["derive", "--baseline", str(baseline(self.plan)), "--kubernetes-version", self.k8s,
                                "--inventory-run", str(records / "inventory-run.json"), "--copy-receipt", str(self.receipt),
                                "--out", str(self.review), "--inventory", str(records / "inventory.json")])
        os_gate((records / "inventory.json").read_bytes(), load(records / "inventory-run.json"), load(self.receipt))
        print(json.dumps({"sanitize_eligible": True}))
        return 0

    def sanitize(self):
        reviewed = ["--review", str(self.review)] if self.k8s else []
        return sanitize.main([*self.variant_args(), *reviewed, "--source", str(self.source), "--copy-receipt",
                              str(self.receipt), *self.capsule_args("sanitize"), "--wall-seconds",
                              str(SANITIZE_WALL_SECONDS)])

    def postscan(self):
        reviewed = ["--review", str(self.review)] if self.k8s else []
        return postscan.main([*self.variant_args(), *reviewed, "--disk", str(self.disk), "--sanitize-run",
                              str(self.sanitize_run), *self.capsule_args("postscan")])

    def sealed(self):
        """The sealed sanitize run and its facts, only with a clean postscan of exactly that disk."""
        run, scan = load(self.sanitize_run), load(self.work / "postscan" / "postscan-run.json")
        need(run["sealed_after_cgroup_stop"] is True and run["disk"] == str(self.disk) and scan["clean"] is True
             and scan["disk_sha256"] == run["facts"]["disk_sha256"], "postscan_not_bound_to_the_sealed_disk")
        return run

    def coldboot(self):
        self.sealed()
        cfg = settings(self.env, self.plan)
        root = state_root(self.env)
        os.makedirs(root, 0o700, exist_ok=True)
        # `recover` ran before this stage: never start a cold boot next to a run that is not recorded clean
        # (its clones still hold the two test addresses).
        need(os.listdir(root) == [], "leftover_cold_boot_state")
        state = root / ("%d-%d" % (self.plan["run_id"], self.plan["run_attempt"]))
        os.mkdir(state, 0o700)
        argv = ["run", "--sanitize-run", str(self.sanitize_run), "--variant", self.plan["variant"], "--os", self.plan["os"],
                "--node", cfg["node"], "--storage", cfg["storage"], "--bridge", cfg["bridge"], "--vlan", str(cfg["vlan"]),
                "--gateway", cfg["gateway"], "--vmid-range", "%d-%d" % CLONE_VMIDS, "--state-dir", str(state),
                "--out", str(self.work / "coldboot" / coldboot.RESULT)]
        argv += ["--kubernetes-version", self.k8s] if self.k8s else []
        for flag, values in (("--test-address", cfg["addresses"]), ("--nameserver", cfg["nameservers"])):
            for value in values:
                argv += [flag, value]
        return coldboot.main(argv)

    def accepted(self):
        """The cold-boot result of exactly the sealed disk and variant."""
        run, result = self.sealed(), load(self.work / "coldboot" / coldboot.RESULT)
        need(result["kind"] == "ColdBootAcceptance" and result["accepted"] is True and result["reason"] is None
             and result["disk_sha256"] == run["facts"]["disk_sha256"]
             and (result["variant"], result["os"], result["kubernetes_version"])
             == (self.plan["variant"], self.plan["os"], self.k8s), "cold_boot_not_accepted")
        return run, result

    def bundle(self):
        selected, staging = self.plan, self.work / "bundle"
        run, _ = self.accepted()
        receipt, source = load(self.receipt), load(self.work / "source.json")
        q = bundle.request(selected["os"], selected["source_commit"], source["upstream_checksum"],
                           selected["build_version"], self.k8s, source["upstream_url"])
        facts = bundle.sealed_facts(run, receipt["raw_sha256"])
        # The template build run is not recorded on a template: the source identity is scoped to this export run.
        r = bundle.receipt(q, facts, selected["run_id"], selected["run_id"], receipt["raw_sha256"])
        manifest = bundle.stage(str(staging), q, r, str(self.disk))
        for name in ("request.json", "clean-export-receipt.json"):  # Both are inside manifest.json.
            os.unlink(staging / name)
        pins = {"manifest_sha256": hashlib.sha256((staging / "manifest.json").read_bytes()).hexdigest(),
                "disk_sha256": manifest["disk"]["sha256"]}
        # Exactly the publisher's check: three files, the pinned hashes, the name's OS, time, commit and variant.
        chosen = vm_publish_stage.inputs(str(selected["run_id"]), str(selected["run_attempt"]), selected["artifact_name"],
                                         pins["manifest_sha256"], pins["disk_sha256"], selected["source_commit"],
                                         vm_publish_stage.REPOSITORY, vm_publish_stage.WORKFLOW_REF, selected["variant"],
                                         selected["os"], self.k8s or "none")
        reference = vm_publish_stage.publication_plan(str(staging), chosen)["reference"]
        outputs = dict(pins, artifact_name=selected["artifact_name"])
        self.output(outputs)
        print(json.dumps(dict(outputs, reference=reference), sort_keys=True))
        return 0

    def output(self, values):
        if self.env.get("GITHUB_OUTPUT"):
            with open(self.env["GITHUB_OUTPUT"], "a", encoding="ascii") as f:
                f.write("".join("%s=%s\n" % item for item in sorted(values.items())))

    def evidence(self):
        _, result = self.accepted()
        manifest_bytes = (self.work / "bundle" / "manifest.json").read_bytes()
        record = evidence.build(
            dict(self.plan, tool_image_id=load(self.work / "tools.json")["local_id"]), load(self.work / "source.json"),
            load(self.receipt), load(self.work / "inventory" / "inventory-run.json"),
            self.review.read_bytes() if self.k8s else None, baseline(self.plan).read_bytes() if self.k8s else None,
            load(self.sanitize_run), load(self.work / "postscan" / "postscan-run.json"), result,
            vm_artifact.decode(manifest_bytes), manifest_bytes)
        data = evidence.encode(record)
        capsule.write_new(self.work / "evidence" / EVIDENCE, data)
        digest = hashlib.sha256(data).hexdigest()
        self.output({"evidence_sha256": digest})
        # The record itself is printed by the hosted `verify` job: this job's log and summary are masked, and
        # a masked copy next to the unmasked artifact would show where a masked value occurs in public data.
        if self.env.get("GITHUB_STEP_SUMMARY"):
            with open(self.env["GITHUB_STEP_SUMMARY"], "a", encoding="ascii") as f:
                f.write("## VM export staged (NOT yet published)\n\nEvidence record `%s`, sha256 `%s`: uploaded as the "
                        "artifact `%s-evidence`; the `verify` job prints it.\n" % (EVIDENCE, digest, self.plan["artifact_name"]))
        print(json.dumps({"evidence": EVIDENCE, "evidence_sha256": digest, "reference": record["reference"]}, sort_keys=True))
        return 0


HANDLERS = {"config": Run.config, "tools": Run.tools, "copy": Run.copy, "inventory": Run.inventory,
            "review": Run.review_stage, "sanitize": Run.sanitize, "postscan": Run.postscan, "coldboot": Run.coldboot,
            "bundle": Run.bundle, "evidence": Run.evidence}


def run_stage(work, name, env, now=None):
    """Run one stage after exactly the earlier ones; record it only when it returned 0."""
    record = work / "stages.json"
    if name == "plan":
        selected = plan(env, now)
        os.mkdir(work, 0o700)  # A leftover work directory refuses: nothing of another run is reused.
        for sub in DIRS:
            os.mkdir(work / sub, 0o700)
        write(work / "plan.json", selected)
        done, code = [], 0
        print(json.dumps({key: selected[key] for key in ("artifact_name", "build_version", "variant", "os",
                                                         "kubernetes_version")}, sort_keys=True))
    else:
        done = load(record) if os.path.lexists(record) else None
        need(done == list(STAGES[:STAGES.index(name)]), "stage_out_of_order")
        code = HANDLERS[name](Run(work, load(work / "plan.json"), env))
    if code == 0:
        tmp = work / "stages.json.tmp"
        tmp.write_text(json.dumps(done + [name]) + "\n", encoding="ascii")
        os.replace(tmp, record)
    return code


def recover(env):
    """Guarded cold-boot cleanup of every state directory on this runner. A directory is deleted only when
    its cleanup is recorded clean (or its run never wrote a state file: it changed nothing on the lab);
    otherwise it stays, as the only record of what is left on the lab, and the command fails."""
    root = state_root(env)
    left = 0
    for state in sorted(root.iterdir()) if os.path.lexists(root) else ():
        if os.path.lexists(state / coldboot.STATE) and coldboot.main(["cleanup", "--state-dir", str(state)]) != 0:
            left += 1
        else:
            capsule.remove_scratch(state)
    if left:
        print("recover: %d cold-boot run(s) not recorded clean on the lab; the state is kept" % left, file=sys.stderr)
    return 1 if left else 0


def purge(work):
    """Delete the private copy, the scratch and every other work file (the cold-boot state is not here: see
    recover). A capsule container that could not be proven stopped is removed first; while docker still
    lists it, nothing it may write to is deleted under it and the step fails."""
    if not os.path.lexists(work):
        return 0
    if os.path.lexists(work / UNRESOLVED):
        need(capsule.force_remove((work / UNRESOLVED).read_text(encoding="ascii").strip()), "capsule_container_still_present")
    for name in DIRS:
        if os.path.lexists(work / name):
            capsule.remove_scratch(work / name)
    for name in FILES:
        (work / name).unlink(missing_ok=True)
    os.rmdir(work)
    return 0


FIXED = (Refused, capsule.CapsuleError, coldboot.Refused, coldboot.ApiError, copy.CopyRefused, evidence.EvidenceRefused)
COMMANDS = {"recover": lambda work, env: recover(env), "purge": lambda work, env: purge(work)}


def main(argv=None, env=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("stage", choices=STAGES + tuple(COMMANDS))
    stage = parser.parse_args(argv).stage
    env = os.environ if env is None else env
    work = None
    try:
        work = workdir(env)
        return COMMANDS[stage](work, env) if stage in COMMANDS else run_stage(work, stage, env)
    except Exception as exc:  # noqa: BLE001 - fixed texts only; anything else is reduced to its class name
        if isinstance(exc, capsule.Unresolved) and work is not None:
            # The message ends with the container id: `purge` removes exactly that container first.
            (work / UNRESOLVED).write_text(str(exc).rsplit(" ", 1)[-1] + "\n", encoding="ascii")
        print("%s refused: %s" % (stage, str(exc) if isinstance(exc, FIXED) else type(exc).__name__), file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
