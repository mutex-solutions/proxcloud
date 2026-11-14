"""Parent launcher: independent read-only postscan capsule over the sealed export.

    cd images && python3 -m vm_export.postscan k8s --disk disk.qcow2 --sanitize-run sanitize-run.json \
        --review k8s-review.json --kubernetes-version 1.37.2 --scratch-base DIR --out-dir DIR
    cd images && python3 -m vm_export.postscan os --os ubuntu-2404 --disk disk.qcow2 \
        --sanitize-run sanitize-run.json --scratch-base DIR --out-dir DIR

The sanitize run must be sealed; the sealed disk must be read-only, single-link and hash to its facts,
and a k8s review must be the very review the sanitizer bound. The probe's single line must equal the
fixed expected schema exactly; writes <out-dir>/postscan.json (only on PASS) and <out-dir>/postscan-run.json.
"""
import argparse
import json
from pathlib import Path
import re
import stat
import subprocess
import sys

from vm_export import capsule
from vm_export.postscan_probe import OSES, expected

HERE = Path(__file__).resolve().parent
IMAGES = HERE.parent


def launch(variant, value, disk, run_path, review, scratch_base, out_dir, wall):
    out_dir, disk = Path(out_dir), Path(disk)
    capsule.need(not any((out_dir / n).exists() for n in ("postscan.json", "postscan-run.json")),
                 "output already exists")
    sanitized = json.loads(Path(run_path).read_text())
    capsule.need(sanitized["sealed_after_cgroup_stop"] is True and type(sanitized["facts"]) is dict,
                 "sanitize run not sealed")
    facts = sanitized["facts"]
    st = disk.lstat()
    capsule.need(stat.S_ISREG(st.st_mode) and st.st_nlink == 1 and not st.st_mode & 0o222
                 and st.st_size == facts["disk_bytes"], "sealed disk identity")
    key = "kubernetes_version" if variant == "kubernetes" else "os"
    capsule.need(facts["variant"] == variant and facts.get(key) == value, "sanitize facts variant")
    binds = {str(disk): "/input/source.qcow2", str(HERE / "postscan_probe.py"): "/bootstrap/postscan_probe.py",
             str(IMAGES / "vm_sanitize.py"): "/bootstrap/vm_sanitize.py",
             str(IMAGES / "k8s_copy_policy.py"): "/bootstrap/k8s_copy_policy.py"}
    if variant == "kubernetes":
        binds[str(Path(review))] = "/input/k8s-review.json"
    pins = capsule.pin_all(binds)
    capsule.need(pins["/input/source.qcow2"] == facts["disk_sha256"], "sealed disk hash")
    if variant == "kubernetes":
        capsule.need(pins["/input/k8s-review.json"] == sanitized["pins"]["/input/k8s-review.json"],
                     "review differs from the sanitized one")
    check = "postscan-" + (("k8s-" + value) if variant == "kubernetes" else value)
    flag = "--k8s" if variant == "kubernetes" else "--os"
    scratch = capsule.prepare_scratch(scratch_base, check, 4 << 30)
    try:
        s = capsule.spec(check, binds, scratch, ["/usr/bin/python3", "-I", "/bootstrap/postscan_probe.py", flag, value],
                         memory="6g", wall=wall, cpu_seconds=wall)
        sink = capsule.JsonSink()
        term = capsule.run(s, sink, pins)
    except capsule.Unresolved:
        scratch = None
        raise
    finally:
        if scratch is not None:
            capsule.remove_scratch(scratch)
    want = expected(variant, value)
    clean, record = capsule.gate_json(term, sink.data, lambda row: row == want)
    if clean:
        capsule.write_new(out_dir / "postscan.json", (json.dumps(record, sort_keys=True) + "\n").encode("ascii"))
    report = dict(term, check_id=check, image=s["image"], pins=pins, clean=clean, result=record,
                  disk_sha256=facts["disk_sha256"])
    capsule.write_new(out_dir / "postscan-run.json", (json.dumps(report, indent=2, sort_keys=True) + "\n").encode())
    print(json.dumps({"check_id": check, "clean": clean, "stop_reason": term["stop_reason"],
                      "exit_code": term["exit_code"]}, sort_keys=True))
    return 0 if clean else 1


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="variant", required=True)
    k = sub.add_parser("k8s")
    k.add_argument("--review", required=True)
    k.add_argument("--kubernetes-version", required=True)
    o = sub.add_parser("os")
    o.add_argument("--os", required=True, choices=OSES)
    for p in (k, o):
        p.add_argument("--disk", required=True)
        p.add_argument("--sanitize-run", required=True)
        p.add_argument("--scratch-base", required=True)
        p.add_argument("--out-dir", required=True)
        p.add_argument("--wall-seconds", type=int, default=2400)
    a = parser.parse_args(argv)
    if a.variant == "k8s":
        if not re.fullmatch(r"[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}", a.kubernetes_version):
            parser.error("invalid version")
        return launch("kubernetes", a.kubernetes_version, a.disk, a.sanitize_run, a.review, a.scratch_base,
                      a.out_dir, a.wall_seconds)
    return launch("os", a.os, a.disk, a.sanitize_run, None, a.scratch_base, a.out_dir, a.wall_seconds)


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (capsule.CapsuleError, OSError, ValueError, KeyError, TypeError, subprocess.SubprocessError) as exc:
        print("postscan refused: " + (str(exc) if isinstance(exc, capsule.CapsuleError) else type(exc).__name__),
              file=sys.stderr)
        sys.exit(2)
