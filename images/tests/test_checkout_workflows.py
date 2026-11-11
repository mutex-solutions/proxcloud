"""Offline synthetic Git regression; authored only, not executed for this change."""
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import textwrap
import unittest

ROOT = Path(__file__).resolve().parents[2]
GIT = shutil.which("git")
WORKFLOWS = {
    "vm-templates.yml": ("images/build.py", "images/packer/template.pkr.hcl", "-f", 1),
    "k8s-node-templates.yml": ("images/k8s-node-template.sh", "images/k8s-build-config.py", "-x", 1),
    "sandbox-templates.yml": ("deploy/templates/build.sh", "deploy/templates/images.yaml", "-x", 2),
}
HELPERS = ("images/vm_artifact.py", "images/vm_publish_stage.py", "images/vm_stage_local.py")


def preflights(text):
    return [textwrap.dedent(block) for block in re.findall(
        r"      - uses: actions/checkout@v7\n      - name: Require full source checkout\n"
        r"        shell: bash\n        run: \|\n((?:          .+\n)+)", text)]


class WorkflowSourceTests(unittest.TestCase):
    def test_every_template_checkout_immediately_requires_fixed_full_source(self):
        for name, (entry, companion, mode, count) in WORKFLOWS.items():
            with self.subTest(workflow=name):
                text = (ROOT / ".github/workflows" / name).read_text()
                scripts = preflights(text)
                self.assertEqual(len(scripts), count)
                self.assertEqual(text.count("uses: actions/checkout@"), count)
                expected = (
                    'set -euo pipefail\n'
                    'git -C "$GITHUB_WORKSPACE" sparse-checkout disable\n'
                    'test "$(git -C "$GITHUB_WORKSPACE" config --bool --get core.sparseCheckout)" = false\n'
                    f'test {mode} "$GITHUB_WORKSPACE/{entry}" && test -f "$GITHUB_WORKSPACE/{companion}"\n')
                self.assertEqual(scripts, [expected] * count)
                for tail in text.split("- name: Require full source checkout\n")[1:]:
                    self.assertLess(tail.index("sparse-checkout disable"), tail.index("- name: Build"))
                triggers = text.split("on:\n", 1)[1].split("permissions:", 1)[0]
                self.assertNotIn("pull_request_target:", triggers)
                self.assertIn("permissions:\n  contents: read\n", text)
                if name != "sandbox-templates.yml":
                    self.assertNotIn("pull_request:", triggers)
                    self.assertIn("runs-on: [self-hosted, proxcloud-pve]", text)
                    self.assertIn("group: vm-templates\n  cancel-in-progress: false", text)
                else:
                    self.assertIn("  pull_request:\n", triggers)
                    self.assertEqual(text.count("runs-on: ubuntu-latest"), 2)


@unittest.skipUnless(GIT and shutil.which("bash"), "local Git and Bash required")
class RealGitCheckoutTests(unittest.TestCase):
    def test_inherited_noncone_sparse_checkout_restores_all_source_without_changing_head(self):
        for name in WORKFLOWS:
            with self.subTest(workflow=name), tempfile.TemporaryDirectory() as tmp:
                parent = Path(tmp)
                repo, home = parent / "repo", parent / "home"
                repo.mkdir(); home.mkdir()
                env = dict(PATH=str(Path(GIT).parent) + os.pathsep + os.defpath, HOME=str(home),
                           XDG_CONFIG_HOME=str(home / ".config"), GIT_CONFIG_NOSYSTEM="1",
                           GIT_CONFIG_GLOBAL=os.devnull, GIT_ATTR_NOSYSTEM="1", GIT_TERMINAL_PROMPT="0",
                           GITHUB_WORKSPACE=str(repo))

                def git(*args):
                    return subprocess.run([GIT, "-c", "core.hooksPath=/dev/null", "-C", str(repo), *args],
                                          env=env, umask=0o022, check=True, capture_output=True, text=True).stdout.strip()

                git("init", "--quiet")
                for key, value in (("user.name", "Fixture"), ("user.email", "fixture@example.invalid"),
                                   ("commit.gpgsign", "false"), ("core.filemode", "true"),
                                   ("core.autocrlf", "false"), ("core.fsmonitor", "false")):
                    git("config", key, value)
                paths = set(HELPERS) | {"src/unrelated.txt"}
                paths.update(path for entry, companion, _, _ in WORKFLOWS.values() for path in (entry, companion))
                for path in paths:
                    target = repo / path
                    target.parent.mkdir(parents=True, exist_ok=True)
                    target.write_text("#!/bin/sh\n# synthetic fixture only\n")
                    target.chmod(0o755 if path.endswith(".sh") else 0o644)
                git("add", "."); git("commit", "--quiet", "-m", "synthetic fixture")
                head, index = git("rev-parse", "HEAD"), git("ls-files", "--stage")
                git("sparse-checkout", "set", "--no-cone", *HELPERS)
                git("config", "--worktree", "--unset", "core.sparseCheckoutCone")  # Sparse-checkout uses worktree config.
                git("checkout", "--force", "--detach", head)  # A successful checkout need not clear sparsity.
                self.assertEqual(git("config", "--bool", "--get", "core.sparseCheckout"), "true")
                for entry, _, _, _ in WORKFLOWS.values():
                    self.assertFalse((repo / entry).exists())
                    self.assertIn("synthetic fixture", git("show", "HEAD:" + entry))
                self.assertEqual(git("status", "--porcelain"), "")
                script = preflights((ROOT / ".github/workflows" / name).read_text())[0]
                for _ in range(2):  # Inherited sparse mode, then an already-full checkout (idempotent).
                    subprocess.run(["bash", "-c", script], cwd=parent, env=env, umask=0o022, check=True,
                                   capture_output=True, text=True)
                    self.assertEqual(git("config", "--bool", "--get", "core.sparseCheckout"), "false")
                    self.assertEqual(git("rev-parse", "HEAD"), head)
                    self.assertEqual(git("ls-files", "--stage"), index)
                    self.assertEqual(git("status", "--porcelain", "--untracked-files=all"), "")
                    self.assertFalse(any(row.startswith("S ") for row in git("ls-files", "-t").splitlines()))
                    for path in paths:
                        self.assertEqual((repo / path).read_text(), "#!/bin/sh\n# synthetic fixture only\n")
                        self.assertEqual((repo / path).stat().st_mode & 0o777, 0o755 if path.endswith(".sh") else 0o644)


if __name__ == "__main__":
    unittest.main(verbosity=2)
