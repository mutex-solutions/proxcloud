#!/usr/bin/env python3
"""Which CI jobs a pull request can affect, and the honest verdict of a gate over them.

  ci_changes.py detect   print the outputs of the `changes` job of test.yml, docker.yml and
                         security-scan.yml (docs/ci-release.md, "What runs when")
  ci_changes.py classify the same for a file list on stdin (one path per line), to try a change locally:
                         git diff --name-only --no-renames origin/main...HEAD | python3 .github/scripts/ci_changes.py classify
  ci_changes.py gate     read NEEDS (the workflow's `toJSON(needs)`) and exit 1 unless every needed job
                         either succeeded or was skipped because `changes` said its inputs did not change

A pull request runs a job only when a changed file is among the job's inputs. Everything runs when the
event is not a pull request (push to main, tag, schedule, manual run), when the diff cannot be computed or
is empty, when a file under .github/ changed, and when a changed file is in no row of RULES below.
Standard library only.
"""
import json
import os
import re
import subprocess
import sys

# scan name -> dockerfile, build context, APP build argument, image published by docker.yml (None: scanned only).
IMAGES = {
    "api": ("deploy/Dockerfile", ".", "", "proxcloud-api"),
    "web-client": ("web/Dockerfile", "web", "client", "proxcloud-web"),
    "web-admin": ("web/Dockerfile", "web", "admin", "proxcloud-admin"),
    "sandbox-proxy": ("deploy/sandbox-proxy.Dockerfile", ".", "", "proxcloud-sandbox-proxy"),
    "sandbox-runtime": ("deploy/sandbox-runtime.Dockerfile", ".", "", "proxcloud-sandbox-runtime"),
    "sandbox-import": ("deploy/sandbox-import.Dockerfile", ".", "", None),
    # Evaluation only: never selected by build.sh/base or published.
    "controlled-base-prototype": ("deploy/templates/base/Dockerfile.controlled", "deploy/templates", "", None),
}
SPA = ("client", "admin")
JOBS = ("go_test", "installer", "template_images", "base_images")
EVERYTHING = frozenset(JOBS) | {"filesystem"} | frozenset(IMAGES) | {"spa:" + app for app in SPA}

GO_IMAGES = ("api", "sandbox-proxy", "sandbox-runtime")
TESTS = ("go_test", "installer", "filesystem")
TEMPLATES = r"deploy/templates/(?!images\.yaml$)"

# (area, pattern matched from the start of the path, what a match runs). Every matching row applies.
# A path that matches no row is unknown and runs everything, so a new top-level directory is never skipped
# by accident. Root files that are not listed (Makefile, .dockerignore, docker-compose.yml, ...) are unknown
# on purpose.
RULES = (
    # Documentation. docs/ and the root Markdown files run nothing by themselves; the rows after this one
    # name the files a test reads (TestDocsReadByTestsRunThoseTests keeps the list honest for Go tests).
    # Markdown under web/*/src/docs/content is bundled into the consoles: it is web code, not this row.
    ("docs", r"docs/|[^/]+\.md$|LICENSE$", ()),
    ("docs", r"docs/(proxmox-permissions|vm-artifact-publishing|billing)\.md$|CHANGELOG\.md$", ("go_test",)),
    ("docs", r"docs/releases/|CHANGELOG\.md$", ("installer",)),
    # Go. The installer job runs too: its script tests read Go sources (images/tests, release policy).
    ("go", r"(cmd|internal|pkg)/|go\.(mod|sum)$", TESTS + GO_IMAGES),
    ("go", r"cmd/sandbox-import/|internal/sandboximport/|go\.(mod|sum)$", ("sandbox-import",)),
    ("go", r"test/", TESTS),
    # deploy/: Go embeds (deploy/embed.go), charts rendered by Go tests, files read by the script tests.
    ("deploy", r"deploy/", TESTS),
    ("deploy", r"deploy/(seed/|platform/|embed\.go$|(mgmt|dbaas)-cluster/argocd-health\.yaml$|Dockerfile$)", ("api",)),
    ("deploy", r"deploy/sandbox-proxy\.Dockerfile$", ("sandbox-proxy",)),
    ("deploy", r"deploy/sandbox-runtime\.Dockerfile$", ("sandbox-runtime",)),
    ("deploy", r"deploy/sandbox-import\.Dockerfile$", ("sandbox-import",)),
    # The eleven sandbox templates (about 15 minutes). The gateway template compiles cmd/mcp-gateway with
    # the root go.mod. images.yaml holds the published digests: it changes no local build.
    ("templates", TEMPLATES + r"|cmd/mcp-gateway/|go\.(mod|sum)$", ("template_images",)),
    ("templates", TEMPLATES, ("controlled-base-prototype",)),
    # The upstream bases are the FROM lines of these files, and images.yaml names the external Python image.
    ("bases", r"deploy/templates/|deploy/[^/]*Dockerfile$|web/Dockerfile$", ("base_images",)),
    ("installer", r"installer/", TESTS),
    ("images", r"images/", ("installer", "filesystem")),
    ("web_client", r"web/client/", ("spa:client", "web-client", "filesystem")),
    ("web_admin", r"web/admin/", ("spa:admin", "web-admin", "filesystem")),
    # web/Dockerfile, nginx.conf, the entrypoint, web/tests: both console images, and the installer job
    # (web/tests/test_runtime_baseline.py).
    ("web_shared", r"web/(?!client/|admin/)", ("installer", "web-client", "web-admin", "filesystem")),
    # Workflows, this script and the scan scripts: Go and script tests read them, and they decide what runs.
    ("ci", r"\.github/", tuple(EVERYTHING)),
)
RULES = tuple((area, re.compile(pattern), frozenset(runs)) for area, pattern, runs in RULES)


def classify(paths, full_reason=""):
    """Outputs (all strings) for the changed paths. full_reason set: everything runs, for that reason."""
    paths = sorted({p.strip() for p in paths if p.strip()})
    runs, areas, reason = set(), set(), full_reason
    if not reason and not paths:
        reason = "no changed file was found"
    for path in paths if not reason else ():
        matched = [(area, run) for area, pattern, run in RULES if pattern.match(path)]
        if not matched:
            reason = f"{path} is in no known area"
            break
        for area, run in matched:
            areas.add(area)
            runs |= run
        if "ci" in areas:
            reason = f"{path} is part of the CI definition"
            break
    full = bool(reason)
    if full:
        runs = set(EVERYTHING)
    scans = (["filesystem"] if "filesystem" in runs else []) + [name for name in IMAGES if name in runs]
    not_run = [name for name in ["filesystem", *IMAGES] if name not in scans]
    not_run += [job.replace("_", "-") for job in ("template_images", "base_images") if job not in runs]
    out = {job: str(job in runs).lower() for job in JOBS}
    out["spa_smoke"] = json.dumps([app for app in SPA if "spa:" + app in runs])
    out["scan"] = json.dumps([
        {"image": name, "dockerfile": "", "context": ".", "app": ""} if name == "filesystem" else
        {"image": name, "dockerfile": IMAGES[name][0], "context": IMAGES[name][1], "app": IMAGES[name][2]}
        for name in scans])
    out["images"] = json.dumps([
        {"image": published, "dockerfile": dockerfile, "context": context, "build-args": "APP=" + app if app else ""}
        for name, (dockerfile, context, app, published) in IMAGES.items() if published and name in runs])
    out["not_run"] = ",".join(not_run)
    out["full"] = str(full).lower()
    out["docs_only"] = str(not full and areas == {"docs"}).lower()
    out["areas"] = ",".join(sorted(areas))
    out["reason"] = ("everything runs: " + reason if full else
                     f"{len(paths)} changed file(s) in: {out['areas']}")
    return out


def detect(env, git=None):
    """Classify the pull request of this event; any doubt about the diff runs everything."""
    if env.get("EVENT_NAME") != "pull_request":
        return classify((), f"event {env.get('EVENT_NAME') or 'unknown'} is not a pull request")
    git = git or (lambda *args: subprocess.run(("git",) + args, check=True, capture_output=True, text=True).stdout)
    base, head = env.get("BASE_SHA", ""), env.get("HEAD_SHA", "")
    try:
        if not all(re.fullmatch(r"[0-9a-f]{40}", sha) for sha in (base, head)):
            raise ValueError("base or head commit missing")
        # What the pull request changes: from where it left its base branch (whatever that branch is) to
        # its head. --no-renames lists both names of a moved file, so both areas run.
        fork = git("merge-base", base, head).strip()
        return classify(git("diff", "--name-only", "--no-renames", fork, head).splitlines())
    except (ValueError, OSError, subprocess.CalledProcessError) as error:
        return classify((), f"the diff could not be computed ({type(error).__name__})")


def expected(value):
    """An output of `changes` that asks for a job: 'true', or a JSON list that is not empty."""
    return value not in ("", "false", "[]")


def gate(needs):
    """(ok, Markdown lines) for the `needs` context of a gate job. Never passes on a job that did not look
    unless `changes` succeeded and said the job's inputs are unchanged."""
    changes = needs.get("changes") or {}
    outputs = changes.get("outputs") or {}
    if changes.get("result") != "success" or "reason" not in outputs:
        return False, [f"- changes: **{changes.get('result', 'missing')}**: change detection did not succeed, so no skipped job can be trusted. BLOCKED."]
    ok, lines = True, [f"Change detection: {outputs['reason']}.", ""]
    for job in sorted(set(needs) - {"changes"}):
        result, asked = needs[job].get("result"), expected(outputs.get(job.replace("-", "_"), ""))
        if result == "success":
            lines.append(f"- {job}: ran, **success**")
        elif result == "skipped" and not asked:
            lines.append(f"- {job}: **not applicable**, skipped because no changed file is among its inputs")
        else:
            ok = False
            why = "its inputs changed, so it had to run" if result == "skipped" else "it had to succeed"
            lines.append(f"- {job}: **{result}**: {why}. BLOCKED.")
    if outputs.get("not_run"):
        lines += ["", "Not scanned in this run (inputs unchanged): " + outputs["not_run"].replace(",", ", ")
                  + ". The scheduled run of security-scan and every push to main scan all of them."]
    return ok, lines


def main(argv):
    command = argv[1] if len(argv) > 1 else ""
    if command in ("detect", "classify"):
        out = detect(os.environ) if command == "detect" else classify(sys.stdin.read().splitlines())
        for key, value in out.items():
            print(f"{key}={value}")
        return 0
    if command == "gate":
        ok, lines = gate(json.loads(os.environ["NEEDS"]))
        text = "\n".join([f"## {os.environ.get('GITHUB_JOB', 'gate')}: {'passed' if ok else 'BLOCKED'}", "", *lines])
        print(text)
        if os.environ.get("GITHUB_STEP_SUMMARY"):
            with open(os.environ["GITHUB_STEP_SUMMARY"], "a", encoding="utf-8") as handle:
                handle.write(text + "\n")
        return 0 if ok else 1
    print(__doc__, file=sys.stderr)
    return 2


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
