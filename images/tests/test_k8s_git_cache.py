"""Offline real-Git cache checks; all objects/files are synthetic private fixtures."""
import importlib.util
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("git_cache_config", ROOT / "images/k8s-build-config.py")
config = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(config)


@unittest.skipUnless(shutil.which("git"), "real Git executable required")
class RealGitCacheTests(unittest.TestCase):
    def git(self, repo, *args):
        return subprocess.run(["git", "-c", "core.hooksPath=/dev/null", "-C", str(repo), *args],
                              check=True, capture_output=True, text=True).stdout

    def fixture(self, parent):
        repo = parent / "cache"
        repo.mkdir()
        self.git(repo, "init", "--quiet")
        self.git(repo, "config", "user.name", "Dummy Fixture")
        self.git(repo, "config", "user.email", "fixture@example.invalid")
        source = repo / "images/capi/Makefile"
        source.parent.mkdir(parents=True)
        source.write_text("# immutable dummy build input\n", encoding="utf-8")
        self.git(repo, "add", "images/capi/Makefile")
        self.git(repo, "commit", "--quiet", "-m", "fixture")
        self.git(repo, "tag", config.IMAGE_BUILDER_REF)
        self.git(repo, "remote", "add", "origin", "https://github.com/kubernetes-sigs/image-builder")
        commit = self.git(repo, "rev-parse", "HEAD").strip()
        return repo, source, commit

    def test_real_git_hidden_index_flags_are_rejected(self):
        for flag in ("--assume-unchanged", "--skip-worktree"):
            with self.subTest(flag=flag), tempfile.TemporaryDirectory() as tmp:
                repo, source, commit = self.fixture(Path(tmp))
                self.git(repo, "update-index", flag, "images/capi/Makefile")
                source.write_text("# changed but hidden by Git index\n", encoding="utf-8")
                self.assertEqual(self.git(repo, "status", "--porcelain"), "")
                with patch.object(config, "IMAGE_BUILDER_COMMIT", commit):
                    with self.assertRaisesRegex(ValueError, "use a new WORK"):
                        config.verify_checkout(repo)
                self.assertIn("changed but hidden", source.read_text())

    def test_real_git_missing_or_foreign_origin_is_rejected(self):
        for origin in (None, "https://foreign.example.invalid/private"):
            with self.subTest(origin=origin), tempfile.TemporaryDirectory() as tmp:
                repo, _, commit = self.fixture(Path(tmp))
                self.git(repo, "remote", "remove", "origin")
                if origin:
                    self.git(repo, "remote", "add", "origin", origin)
                with patch.object(config, "IMAGE_BUILDER_COMMIT", commit):
                    with self.assertRaisesRegex(ValueError, "use a new WORK") as error:
                        config.verify_checkout(repo)
                self.assertNotIn("foreign.example.invalid", str(error.exception))

    def test_real_git_export_ignores_post_gate_worktree_change(self):
        with tempfile.TemporaryDirectory() as tmp:
            parent = Path(tmp)
            repo, source, commit = self.fixture(parent)
            run = parent / "run"
            run.mkdir(mode=0o700)
            original_verify = config.verify_checkout

            def verified_then_changed(checkout):
                original_verify(checkout)
                source.write_text("# changed after validation\n", encoding="utf-8")

            with patch.object(config, "IMAGE_BUILDER_COMMIT", commit):
                with patch.object(config, "verify_checkout", side_effect=verified_then_changed):
                    config.export_checkout(repo, run)
            exported = run / "image-builder/images/capi/Makefile"
            self.assertEqual(exported.read_text(), "# immutable dummy build input\n")
            self.assertFalse((run / "image-builder/.git").exists())
            self.assertEqual(source.read_text(), "# changed after validation\n")


if __name__ == "__main__":
    unittest.main(verbosity=2)
