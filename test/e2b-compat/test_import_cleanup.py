"""No-network regression for independent CI jobs reusing one digest artifact."""
import importlib.util
import os
from pathlib import Path
import sys
import types
import unittest
from unittest.mock import Mock
import urllib.error

os.environ.setdefault("E2B_API_KEY", "test-key")
os.environ.setdefault("SANDBOX_IMAGE", "registry.example/test@sha256:" + "0" * 64)
sys.modules.setdefault("e2b", types.SimpleNamespace(Sandbox=object))
spec = importlib.util.spec_from_file_location("digest_smoke", Path(__file__).with_name("import_digest.py"))
smoke = importlib.util.module_from_spec(spec)
spec.loader.exec_module(smoke)


class CleanupTests(unittest.TestCase):
    def test_reused_job_never_deletes_shared_template(self):
        smoke.api = Mock()
        smoke.cleanup_template("shared", ["run-name", "run-alias"], created=False)
        smoke.api.assert_called_once_with("DELETE", "/templates/shared/aliases", {"aliases": ["run-name", "run-alias"]})

    def test_creator_deletes_unshared_disposable_template(self):
        smoke.api = Mock(side_effect=[{"names": ["run-name", "run-alias"]}, None])
        smoke.cleanup_template("shared", ["run-name", "run-alias"], created=True)
        self.assertEqual(smoke.api.call_args.args, ("DELETE", "/templates/shared"))

    def test_creator_preserves_artifact_adopted_by_another_job(self):
        smoke.api = Mock(side_effect=[{"names": ["run-name", "run-alias", "other-job"]}, None])
        smoke.cleanup_template("shared", ["run-name", "run-alias"], created=True)
        self.assertEqual(smoke.api.call_args.args, ("DELETE", "/templates/shared/aliases", {"aliases": ["run-alias"]}))

    def test_already_cleaned_is_idempotent(self):
        smoke.api = Mock(side_effect=urllib.error.HTTPError("", 404, "missing", {}, None))
        smoke.cleanup_template("shared", ["run-name"], created=False)


if __name__ == "__main__":
    unittest.main()
