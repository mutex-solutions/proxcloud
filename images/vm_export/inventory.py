"""Parent launcher: one read-only inventory capsule over a copied template disk (k8s RAW or native qcow2).

    cd images && python3 -m vm_export.inventory k8s --source RAW --kubernetes-version V \
        --containerd-version 2.3.2 --runc-version 1.4.3 --scratch-base DIR --out-dir DIR
    cd images && python3 -m vm_export.inventory os --os ubuntu-2404 --source QCOW2 --scratch-base DIR --out-dir DIR

Writes <out-dir>/inventory.json (the gated single probe line, only on PASS) and
<out-dir>/inventory-run.json (termination record, pins, fixed reason codes). Never prints guest data.
"""
import argparse
import json
from pathlib import Path
import re
import sys

from vm_export import capsule

HERE = Path(__file__).resolve().parent
IMAGES = HERE.parent
K8S_TOP = {"phase", "policy_result", "observed_backend", "facts", "shadow_marker_classes", "unknown_kind_accounts",
           "swap", "hygiene_detail", "packages", "package_status_word", "runtime", "cloud_base_default_user",
           "dropin_default_users", "snapd", "token_files", "sudoers", "readonly_no_guest_code_or_mutation",
           "publication_eligible", "close_error"}
OS_TOP = {"phase", "os", "layout", "accounts", "cloud_dropins", "snapd", "hygiene", "sanitize_eligible",
          "readonly_no_guest_code_or_mutation", "publication_eligible", "close_error"}
VERSION = r"[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}"


def valid_k8s(row):
    return (set(row) == K8S_TOP and row["phase"] == "k8s-readonly-inventory-v1"
            and re.fullmatch(r"PASS_WITH_PLACEHOLDER_REVIEW|REFUSED|ERROR_[A-Za-z]{1,40}", str(row["policy_result"]))
            is not None and row["readonly_no_guest_code_or_mutation"] is True and row["publication_eligible"] is False)


def valid_os(row):
    return (set(row) == OS_TOP and row["phase"] == "os-readonly-inventory-v1" and type(row["sanitize_eligible"]) is bool
            and row["readonly_no_guest_code_or_mutation"] is True and row["publication_eligible"] is False)


def launch(check, source, guest_source, probe, probe_args, validator, scratch_base, out_dir, wall):
    out_dir = Path(out_dir)
    # Never bind a probe under a stdlib module name: /bootstrap is first on the probe's sys.path, so a
    # /bootstrap/inspect.py would shadow `inspect` (dataclasses imports it) and break the policy import.
    binds = {str(Path(source)): guest_source, str(HERE / probe): "/bootstrap/" + probe,
             str(IMAGES / "vm_sanitize.py"): "/bootstrap/vm_sanitize.py",
             str(IMAGES / "k8s_copy_policy.py"): "/bootstrap/k8s_copy_policy.py"}
    pins = {guest: capsule.pinned_input(host) for host, guest in binds.items()}
    scratch = capsule.prepare_scratch(scratch_base, check, 4 << 30)
    try:
        s = capsule.spec(check, binds, scratch, ["/usr/bin/python3", "-I", "/bootstrap/" + probe, *probe_args],
                         memory="6g", wall=wall, cpu_seconds=wall)
        sink = capsule.JsonSink()
        term = capsule.run(s, sink)
    except capsule.Unresolved:
        scratch = None
        raise
    finally:
        if scratch is not None:
            capsule.remove_scratch(scratch)
    clean, record = capsule.gate_json(term, sink.data, validator)
    report = dict(term, check_id=check, image=s["image"], pins=pins, clean=clean,
                  result=record if not clean else {"phase": record["phase"]})
    if clean:
        capsule.write_new(out_dir / "inventory.json", (json.dumps(record, sort_keys=True) + "\n").encode("ascii"))
    capsule.write_new(out_dir / "inventory-run.json", (json.dumps(report, indent=2, sort_keys=True) + "\n").encode())
    print(json.dumps({"check_id": check, "clean": clean, "stop_reason": term["stop_reason"],
                      "exit_code": term["exit_code"]}, sort_keys=True))
    return 0 if clean else 1


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="variant", required=True)
    k = sub.add_parser("k8s")
    k.add_argument("--kubernetes-version", required=True)
    k.add_argument("--containerd-version", required=True)
    k.add_argument("--runc-version", required=True)
    o = sub.add_parser("os")
    o.add_argument("--os", required=True, choices=("ubuntu-2404", "ubuntu-2604", "debian-12", "debian-13"))
    for p in (k, o):
        p.add_argument("--source", required=True)
        p.add_argument("--scratch-base", required=True)
        p.add_argument("--out-dir", required=True)
        p.add_argument("--check-id")
        p.add_argument("--wall-seconds", type=int, default=2400)
    a = parser.parse_args(argv)
    if a.variant == "k8s":
        versions = (a.kubernetes_version, a.containerd_version, a.runc_version)
        if not all(re.fullmatch(VERSION, v) for v in versions):
            parser.error("invalid version")
        check = a.check_id or "k8s-inventory-" + a.kubernetes_version
        return launch(check, a.source, "/input/source.raw", "inspect_k8s.py", list(versions), valid_k8s,
                      a.scratch_base, a.out_dir, a.wall_seconds)
    check = a.check_id or a.os + "-inventory"
    return launch(check, a.source, "/input/source.qcow2", "inspect_os.py", [a.os], valid_os,
                  a.scratch_base, a.out_dir, a.wall_seconds)


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (capsule.CapsuleError, OSError) as exc:
        print("inventory refused: " + (str(exc) if isinstance(exc, capsule.CapsuleError) else type(exc).__name__),
              file=sys.stderr)
        sys.exit(2)
