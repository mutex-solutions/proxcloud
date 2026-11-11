#!/usr/bin/env python3
"""Data-only frontend builder/runtime contract, not an image or browser test."""
from pathlib import Path
import re
import unittest

import yaml

ROOT = Path(__file__).resolve().parents[2]


class RuntimeBaselineTests(unittest.TestCase):
    def test_exact_builder_and_runtime_tags(self):
        dockerfile = (ROOT / "web/Dockerfile").read_text(encoding="utf-8")
        self.assertEqual(re.findall(r"^FROM (.+)$", dockerfile, re.MULTILINE), [
            "node:24.22.2-alpine AS build", "nginx:1.30.6-alpine",
        ])
        self.assertIn("ARG APP\n", dockerfile)
        self.assertIn("COPY ${APP}/package.json ${APP}/package-lock.json ./", dockerfile)
        self.assertIn("RUN npm ci", dockerfile)
        self.assertIn("COPY --from=build /app/dist /usr/share/nginx/html", dockerfile)
        self.assertIn('ENTRYPOINT ["/docker-entrypoint-proxcloud.sh"]', dockerfile)

    def test_ci_node_matches_build_series_and_patch(self):
        workflow = yaml.safe_load((ROOT / ".github/workflows/test.yml").read_text(encoding="utf-8"))
        steps = workflow["jobs"]["spa-smoke"]["steps"]
        setup = [step for step in steps if str(step.get("uses", "")).startswith("actions/setup-node@")]
        self.assertEqual(len(setup), 1)
        self.assertEqual(setup[0]["with"]["node-version"], "24.22.2")
        self.assertNotIn("node-version-file", setup[0]["with"])


if __name__ == "__main__":
    unittest.main(verbosity=2)
