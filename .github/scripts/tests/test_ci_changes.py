#!/usr/bin/env python3
"""Which jobs a pull request runs (ci_changes.py), and what the required gate says about skipped jobs.

Standard library and local Git only: no network, no GitHub API.
"""
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[3]
spec = importlib.util.spec_from_file_location("ci_changes", ROOT / ".github/scripts/ci_changes.py")
ci = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ci)

ALL_SCANS = ["filesystem", *ci.IMAGES]
PUBLISHED = ["proxcloud-api", "proxcloud-web", "proxcloud-admin", "proxcloud-sandbox-proxy", "proxcloud-sandbox-runtime"]


def run(*paths):
    out = ci.classify(paths)
    return {
        "jobs": {job for job in ci.JOBS if out[job] == "true"},
        "spa": json.loads(out["spa_smoke"]),
        "scan": [leg["image"] for leg in json.loads(out["scan"])],
        "images": [leg["image"] for leg in json.loads(out["images"])],
        "full": out["full"] == "true",
        "docs_only": out["docs_only"] == "true",
        "out": out,
    }


class ClassifyTests(unittest.TestCase):
    def assertFull(self, got):
        self.assertTrue(got["full"], got["out"]["reason"])
        self.assertEqual(got["jobs"], set(ci.JOBS))
        self.assertEqual(got["spa"], ["client", "admin"])
        self.assertEqual(got["scan"], ALL_SCANS)
        self.assertEqual(got["images"], PUBLISHED)
        self.assertEqual(got["out"]["not_run"], "")
        self.assertFalse(got["docs_only"])

    def test_docs_only_runs_nothing(self):
        got = run("docs/install.md", "README.md", "docs/adr/0009-managed-services-platform-lifecycle.md",
                  "docs/architecture/c4/CHANGELOG.md", "CONTRIBUTING.md", "LICENSE")
        self.assertEqual((got["jobs"], got["spa"], got["scan"], got["images"]), (set(), [], [], []))
        self.assertTrue(got["docs_only"])
        self.assertFalse(got["full"])
        self.assertEqual(got["out"]["not_run"], ",".join([*ALL_SCANS, "template-images", "base-images"]))

    def test_docs_that_tests_read_run_those_tests_and_no_image(self):
        for path, jobs in (("CHANGELOG.md", {"go_test", "installer"}),
                           ("docs/releases/v0.9.0.md", {"installer"}),
                           ("docs/proxmox-permissions.md", {"go_test"}),
                           ("docs/vm-artifact-publishing.md", {"go_test"}),
                           ("docs/billing.md", {"go_test"})):
            with self.subTest(path=path):
                got = run(path)
                self.assertEqual(got["jobs"], jobs)
                self.assertEqual((got["spa"], got["scan"], got["images"]), ([], [], []))
                self.assertTrue(got["docs_only"])

    def test_console_docs_are_console_code(self):
        got = run("web/admin/src/docs/content/guide/install.md")
        self.assertEqual((got["spa"], got["scan"], got["images"], got["jobs"]),
                         (["admin"], ["filesystem", "web-admin"], ["proxcloud-admin"], set()))
        self.assertFalse(got["docs_only"])
        got = run("web/client/src/docs/content/guide/sandbox.md")
        self.assertEqual((got["spa"], got["scan"], got["images"]), (["client"], ["filesystem", "web-client"], ["proxcloud-web"]))

    def test_shared_web_files_build_both_consoles(self):
        got = run("web/Dockerfile")
        self.assertEqual(got["scan"], ["filesystem", "web-client", "web-admin"])
        self.assertEqual(got["images"], ["proxcloud-web", "proxcloud-admin"])
        self.assertEqual(got["jobs"], {"installer", "base_images"})  # web/tests reads it; its FROM lines are bases
        self.assertEqual(got["spa"], [])

    def test_go_change(self):
        got = run("internal/platform/project/e2b.go")
        self.assertEqual(got["jobs"], {"go_test", "installer"})
        self.assertEqual(got["scan"], ["filesystem", "api", "sandbox-proxy", "sandbox-runtime"])
        self.assertEqual(got["images"], ["proxcloud-api", "proxcloud-sandbox-proxy", "proxcloud-sandbox-runtime"])
        self.assertEqual(got["spa"], [])
        self.assertIn("sandbox-import", run("internal/sandboximport/copy.go")["scan"])
        self.assertIn("sandbox-import", run("cmd/sandbox-import/main.go")["scan"])

    def test_go_modules_reach_every_go_build(self):
        for path in ("go.mod", "go.sum"):
            got = run(path)
            self.assertEqual(got["jobs"], {"go_test", "installer", "template_images"})
            self.assertEqual(got["scan"], ["filesystem", "api", "sandbox-proxy", "sandbox-runtime", "sandbox-import"])

    def test_node_lockfile(self):
        got = run("web/client/package-lock.json")
        self.assertEqual((got["spa"], got["scan"]), (["client"], ["filesystem", "web-client"]))

    def test_template_images_only_for_their_inputs(self):
        for path in ("deploy/templates/desktop/Dockerfile", "deploy/templates/build.sh", "cmd/mcp-gateway/main.go"):
            self.assertIn("template_images", run(path)["jobs"], path)
        for path in ("deploy/templates/images.yaml", "internal/cloud/sandbox/build.go", "deploy/charts/proxcloud/values.yaml",
                     "installer/playbooks/site.yml", "web/client/src/main.tsx", "docs/managed-sandbox.md"):
            self.assertNotIn("template_images", run(path)["jobs"], path)
        # images.yaml names the external Python base and is read by Go and script tests.
        self.assertEqual(run("deploy/templates/images.yaml")["jobs"], {"go_test", "installer", "base_images"})
        self.assertIn("controlled-base-prototype", run("deploy/templates/common/setup-user.sh")["scan"])

    def test_deploy_feeds_embeds_charts_and_tests(self):
        got = run("deploy/charts/proxmox-kamaji-cluster/templates/cluster.yaml")
        self.assertEqual((got["jobs"], got["scan"], got["images"], got["spa"]),
                         ({"go_test", "installer"}, ["filesystem"], [], []))
        for path in ("deploy/embed.go", "deploy/platform/platform-bom.yaml", "deploy/seed/flavors.json",
                     "deploy/mgmt-cluster/argocd-health.yaml", "deploy/Dockerfile"):
            self.assertEqual(run(path)["images"], ["proxcloud-api"], path)
        self.assertEqual(run("deploy/sandbox-runtime.Dockerfile")["scan"], ["filesystem", "sandbox-runtime"])
        self.assertIn("base_images", run("deploy/sandbox-proxy.Dockerfile")["jobs"])

    def test_installer_and_vm_images_have_their_own_tests(self):
        self.assertEqual(run("installer/playbooks/55-sandbox-cluster.yml")["jobs"], {"go_test", "installer"})
        got = run("images/build.py")
        self.assertEqual((got["jobs"], got["scan"], got["images"]), ({"installer"}, ["filesystem"], []))

    def test_ci_definition_runs_everything(self):
        for path in (".github/scripts/ci_changes.py", ".github/scripts/tests/test_ci_changes.py",
                     ".github/workflows/security-scan.yml", ".github/workflows/test.yml", ".github/workflows/docker.yml",
                     ".github/workflows/helm.yml", ".github/scripts/check_scan_report.py"):
            with self.subTest(path=path):
                self.assertFull(run("docs/install.md", path))

    def test_unknown_area_runs_everything(self):
        for path in ("newdir/file.go", "Makefile", ".dockerignore", "docker-compose.yml", ".gitignore", "tasks/todo.md.bak"):
            with self.subTest(path=path):
                got = run("docs/install.md", path)
                self.assertFull(got)
                self.assertIn(path, got["out"]["reason"])

    def test_empty_diff_runs_everything(self):
        self.assertFull(run())
        self.assertFull(run("", "  "))

    def test_rename_across_areas_and_deleted_file_count_by_path(self):
        # --no-renames gives both names of a move, and a deleted file is only a path: both classify like an edit.
        got = run("docs/old-guide.md", "web/client/src/docs/content/old-guide.md")
        self.assertEqual((got["spa"], got["docs_only"]), (["client"], False))
        self.assertEqual(run("internal/platform/admin/removed.go")["jobs"], {"go_test", "installer"})

    def test_outputs_are_single_line_strings(self):
        for paths in ((), ("docs/a.md",), ("internal/x.go", "web/admin/a.ts")):
            for key, value in ci.classify(paths).items():
                self.assertIsInstance(value, str)
                self.assertNotIn("\n", value, key)

    def test_matrices_carry_what_the_workflows_read(self):
        out = ci.classify(("internal/x.go", "web/admin/a.ts"))
        api = next(leg for leg in json.loads(out["scan"]) if leg["image"] == "api")
        self.assertEqual(api, {"image": "api", "dockerfile": "deploy/Dockerfile", "context": ".", "app": ""})
        admin = next(leg for leg in json.loads(out["images"]) if leg["image"] == "proxcloud-admin")
        self.assertEqual(admin, {"image": "proxcloud-admin", "dockerfile": "web/Dockerfile", "context": "web", "build-args": "APP=admin"})
        for dockerfile, context, _, _ in ci.IMAGES.values():
            self.assertTrue((ROOT / dockerfile).is_file(), dockerfile)
            self.assertTrue((ROOT / context).is_dir(), context)

    def test_every_tracked_file_is_in_a_known_area_or_on_the_reviewed_unknown_list(self):
        if not shutil.which("git") or not (ROOT / ".git").exists():
            self.skipTest("needs the Git checkout")
        tracked = subprocess.run(["git", "-C", str(ROOT), "ls-files"], check=True, capture_output=True, text=True).stdout.splitlines()
        unknown = {p for p in tracked if not any(pattern.match(p) for _, pattern, _ in ci.RULES)}
        # Unknown runs everything. A new top-level directory lands here until it gets a row in RULES.
        self.assertEqual(unknown, {".codegraph/.gitignore", ".dockerignore", ".env.example", ".gitattributes",
                                   ".githooks/pre-commit", ".gitignore", "Makefile", "docker-compose.yml"})

    def test_docs_read_by_go_tests_run_go_test(self):
        # A Go test opens a document through a relative path literal ("../../docs/x.md"); prose comments never
        # use one. A new such read must be added to RULES, or a pull request that edits the document skips the test.
        literal = re.compile(r'"(?:\.\./)+((?:docs/[^"]+|[A-Z_]+\.md))"')
        found = set()
        for path in ROOT.glob("**/*_test.go"):
            if "node_modules" not in path.parts:
                found.update(literal.findall(path.read_text(encoding="utf-8", errors="replace")))
        self.assertIn("docs/proxmox-permissions.md", found)
        for doc in sorted(found):
            self.assertIn("go_test", run(doc)["jobs"], f"{doc} is read by a Go test: add it to RULES in ci_changes.py")

    def test_docs_read_by_script_tests_are_the_known_ones(self):
        readers = set()
        for folder in (".github/scripts/tests", "installer/tests", "images/tests", "deploy/tests", "web/tests"):
            for path in (ROOT / folder).glob("*.py"):
                if path.name != "test_ci_changes.py" and re.search(r'ROOT / "(docs|CHANGELOG\.md)', path.read_text(encoding="utf-8")):
                    readers.add(path.name)
        self.assertEqual(readers, {"test_release_notes.py"}, "a script test reads docs/ or CHANGELOG.md: add its files to RULES")


@unittest.skipUnless(shutil.which("git"), "local Git required")
class DetectTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.env = dict(PATH=os.environ.get("PATH", os.defpath), HOME=self.tmp.name, GIT_CONFIG_NOSYSTEM="1",
                        GIT_CONFIG_GLOBAL=os.devnull, GIT_AUTHOR_NAME="t", GIT_AUTHOR_EMAIL="t@example.invalid",
                        GIT_COMMITTER_NAME="t", GIT_COMMITTER_EMAIL="t@example.invalid")
        self.git("init", "--quiet", "-b", "main")
        self.git("config", "commit.gpgsign", "false")
        self.commit({"docs/a.md": "a", "internal/x.go": "x", "web/client/a.ts": "a", "docs/guide.md": "g"})

    def git(self, *args):
        return subprocess.run(("git", "-c", "core.hooksPath=/dev/null", "-C", self.tmp.name) + args, env=self.env,
                              check=True, capture_output=True, text=True).stdout

    def commit(self, files, remove=()):
        for name, text in files.items():
            path = Path(self.tmp.name, name)
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(text, encoding="utf-8")
        for name in remove:
            Path(self.tmp.name, name).unlink()
        self.git("add", "-A")
        self.git("commit", "--quiet", "--allow-empty", "-m", "c")
        return self.git("rev-parse", "HEAD").strip()

    def detect(self, base, head, event="pull_request"):
        return ci.detect({"EVENT_NAME": event, "BASE_SHA": base, "HEAD_SHA": head}, self.git)

    def test_only_the_pull_requests_own_commits_count(self):
        fork = self.git("rev-parse", "HEAD").strip()
        self.git("checkout", "--quiet", "-b", "pr")
        head = self.commit({"docs/a.md": "changed"})
        self.git("checkout", "--quiet", "main")
        base = self.commit({"internal/x.go": "main moved on"})  # not part of the pull request
        out = self.detect(base, head)
        self.assertEqual((out["docs_only"], out["full"], out["go_test"]), ("true", "false", "false"))
        self.assertEqual(self.detect(fork, head)["docs_only"], "true")

    def test_base_that_is_not_main(self):
        self.git("checkout", "--quiet", "-b", "release-0.9")
        base = self.commit({"internal/x.go": "release only"})
        self.git("checkout", "--quiet", "-b", "pr")
        head = self.commit({"web/client/a.ts": "b"})
        out = self.detect(base, head)
        self.assertEqual((out["spa_smoke"], out["go_test"], out["full"]), ('["client"]', "false", "false"))

    def test_rename_across_areas_and_delete(self):
        base = self.git("rev-parse", "HEAD").strip()
        self.git("mv", "docs/guide.md", "web/client/guide.md")
        moved = self.commit({})
        out = self.detect(base, moved)
        self.assertEqual((out["spa_smoke"], out["docs_only"], out["areas"]), ('["client"]', "false", "docs,web_client"))
        deleted = self.commit({}, remove=("internal/x.go",))
        self.assertEqual(self.detect(moved, deleted)["go_test"], "true")

    def test_no_change_and_unusable_commits_run_everything(self):
        head = self.git("rev-parse", "HEAD").strip()
        for base, sha in ((head, head), ("", head), ("0" * 40, head), (head, "f" * 40), ("main; id", head)):
            with self.subTest(base=base, head=sha):
                out = self.detect(base, sha)
                self.assertEqual((out["full"], out["template_images"]), ("true", "true"))

    def test_other_events_run_everything(self):
        head = self.git("rev-parse", "HEAD").strip()
        for event in ("push", "schedule", "workflow_dispatch", "merge_group", ""):
            out = self.detect(head, head, event)
            self.assertEqual((out["full"], out["not_run"]), ("true", ""), event)
            self.assertEqual(len(json.loads(out["images"])), 5)


class GateTests(unittest.TestCase):
    JOBS = ("scan", "template-images", "base-images")

    def needs(self, paths, results, changes="success"):
        needs = {"changes": {"result": changes, "outputs": ci.classify(paths) if changes == "success" else {}}}
        needs.update({job: {"result": result, "outputs": {}} for job, result in zip(self.JOBS, results)})
        return needs

    def test_full_run_needs_every_job_green(self):
        ok, lines = ci.gate(self.needs((".github/workflows/test.yml",), ("success",) * 3))
        self.assertTrue(ok)
        self.assertNotIn("not applicable", "\n".join(lines))
        for index in range(3):
            for bad in ("failure", "cancelled", "skipped"):
                results = ["success"] * 3
                results[index] = bad
                ok, lines = ci.gate(self.needs((".github/workflows/test.yml",), results))
                self.assertFalse(ok, (index, bad))
                self.assertIn(f"{self.JOBS[index]}: **{bad}**", "\n".join(lines))

    def test_docs_only_passes_and_says_nothing_was_scanned(self):
        ok, lines = ci.gate(self.needs(("docs/install.md",), ("skipped",) * 3))
        text = "\n".join(lines)
        self.assertTrue(ok)
        self.assertEqual(text.count("**not applicable**"), 3)
        self.assertIn("Not scanned in this run (inputs unchanged): filesystem, api,", text)
        self.assertIn("template-images, base-images", text)

    def test_go_change_skips_templates_only(self):
        paths = ("internal/x.go",)
        ok, lines = ci.gate(self.needs(paths, ("success", "skipped", "skipped")))
        self.assertTrue(ok)
        self.assertIn("- scan: ran, **success**", lines)
        self.assertIn("web-client", lines[-1])
        self.assertFalse(ci.gate(self.needs(paths, ("skipped", "skipped", "skipped")))[0])  # scan had to run
        self.assertFalse(ci.gate(self.needs(paths, ("failure", "skipped", "skipped")))[0])
        self.assertFalse(ci.gate(self.needs(paths, ("success", "cancelled", "skipped")))[0])
        self.assertFalse(ci.gate(self.needs(paths, ("success", "failure", "skipped")))[0])  # never excused

    def test_failed_or_missing_change_detection_blocks(self):
        for state in ("failure", "cancelled", "skipped"):
            ok, lines = ci.gate(self.needs((), ("skipped",) * 3, changes=state))
            self.assertFalse(ok)
            self.assertIn("change detection did not succeed", lines[0])
        self.assertFalse(ci.gate({job: {"result": "skipped", "outputs": {}} for job in self.JOBS})[0])
        self.assertFalse(ci.gate({"changes": {"result": "success", "outputs": {}}, "scan": {"result": "skipped"}})[0])

    def test_gate_command_exit_codes(self):
        script = str(ROOT / ".github/scripts/ci_changes.py")
        for results, code in ((("skipped",) * 3, 0), (("failure", "skipped", "skipped"), 1)):
            with tempfile.TemporaryDirectory() as tmp:
                summary = Path(tmp, "summary.md")
                env = dict(os.environ, NEEDS=json.dumps(self.needs(("docs/a.md",), results)), GITHUB_STEP_SUMMARY=str(summary),
                           GITHUB_JOB="security-gate")
                done = subprocess.run(["python3", script, "gate"], env=env, capture_output=True, text=True)
                self.assertEqual(done.returncode, code, done.stdout + done.stderr)
                self.assertIn("## security-gate: " + ("passed" if code == 0 else "BLOCKED"), summary.read_text())


if __name__ == "__main__":
    unittest.main(verbosity=2)
