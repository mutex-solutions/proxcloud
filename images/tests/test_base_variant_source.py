"""Source-only optional variant contracts; authored, NOT executed in this task.

Read recipe/metadata as data only. No Docker, package commands, network, imports
of target code or runtime/SDK/CVE acceptance. Run later only when authorized.
"""
import json
from pathlib import Path
import re
import unittest

ROOT = Path(__file__).resolve().parents[2]
BASE = ROOT / "deploy/templates/base"
PACKAGES = {
    "media": ["imagemagick", "libmagickwand-dev", "libopenexr-dev", "libheif-dev",
              "libaom-dev", "libjpeg-dev", "libpng-dev", "libtiff-dev", "libwebp-dev"],
    "xml": ["libxml2-dev", "libxslt1-dev", "libglib2.0-dev"],
    "gui": ["libx11-dev", "libxext-dev", "libxrender-dev", "libxt-dev", "libsm-dev",
            "libice-dev", "libcairo2-dev", "tk-dev", "fonts-dejavu-core"],
    "database-dev": ["libpq-dev", "default-libmysqlclient-dev", "libkrb5-dev", "libldap-dev"],
    "vcs": ["mercurial", "subversion"],
}
GROUPS = {
    "media": ["image_processing", "media"],
    "xml": ["xml_glib"],
    "gui": ["x11_tk_rendering_fonts"],
    "database-dev": ["database_auth_headers"],
    "vcs": ["legacy_vcs"],
}


def instructions(source):
    """Normalize this deliberately simple recipe without executing instructions."""
    lines = [line for line in source.splitlines()
             if line.strip() and not line.lstrip().startswith("#")]
    logical = re.sub(r"\\\n\s*", " ", "\n".join(lines))
    return [" ".join(line.split()) for line in logical.splitlines()]


class BaseVariantSourceTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.source = (BASE / "Dockerfile.variants").read_text()
        cls.metadata = json.loads((BASE / "variants.json").read_text())
        cls.commands = instructions(cls.source)
        cls.stages = {}
        current = None
        for command in cls.commands[1:]:
            if command.startswith("FROM "):
                current = command.rsplit(" ", 1)[1]
                cls.stages[current] = []
            else:
                cls.stages[current].append(command)

    def test_base_required_without_default_or_shared_optional_layer(self):
        self.assertEqual(self.commands[0], "ARG BASE")
        self.assertEqual([c for c in self.commands if c.startswith("ARG ")], ["ARG BASE"])
        self.assertEqual([c for c in self.commands if c.startswith("FROM ")],
                         ["FROM ${BASE} AS " + name for name in PACKAGES]
                         + ["FROM ${BASE} AS require-explicit-target"])
        self.assertEqual(list(self.stages), list(PACKAGES) + ["require-explicit-target"])
        self.assertEqual(self.stages["require-explicit-target"], [
            "USER root",
            "RUN printf '%s\\n' 'Select --target media|xml|gui|database-dev|vcs explicitly.' >&2; exit 1",
        ])
        self.assertTrue(self.metadata["base_contract"]["required"])
        self.assertIsNone(self.metadata["base_contract"]["default"])
        self.assertIn("local", self.metadata["base_contract"]["source"])
        self.assertIn("cannot", self.metadata["base_contract"]["provenance_enforcement"])
        self.assertNotIn("e2bdev/base", self.source)

    def test_exact_stage_instructions_allow_only_apt_and_cache_cleanup(self):
        # Exact instruction comparison also prevents arbitrary scripts, pip/npm,
        # package purges, scanner ignores, COPY/ADD, ENV, permission/user changes,
        # package database erasure, accidental fallback and push commands.
        for name, packages in PACKAGES.items():
            with self.subTest(target=name):
                expected_label = (
                    'LABEL org.proxcloud.variant="' + name + '" '
                    'org.proxcloud.variant.source-category="' + ",".join(GROUPS[name]) + '" '
                    'org.proxcloud.variant.status="notbuilt/unvalidated"'
                )
                expected_run = (
                    "RUN apt-get update && apt-get upgrade -y "
                    "&& DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends "
                    + " ".join(packages) + " && rm -rf /var/lib/apt/lists/*"
                )
                self.assertEqual(self.stages[name], ["USER root", expected_label,
                                                    expected_run, "WORKDIR /home/user"])
                self.assertEqual(self.metadata["variants"][name]["packages"], packages)
                self.assertEqual(self.metadata["variants"][name]["source_groups"], GROUPS[name])

    def test_explicit_selection_and_nondefault_boundary(self):
        self.assertEqual(self.metadata["status"], "notbuilt/unvalidated")
        self.assertFalse(self.metadata["default_changed"])
        self.assertEqual(self.metadata["selection"]["targets"], list(PACKAGES))
        self.assertTrue(self.metadata["selection"]["must_supply_target"])
        self.assertIsNone(self.metadata["selection"]["combined_target"])
        self.assertEqual(self.metadata["selection"]["default_guard_stage"], "require-explicit-target")
        self.assertIn("fails intentionally", self.metadata["selection"]["no_target_behavior"])
        example = self.metadata["selection"]["future_build_only"]
        self.assertIn("--build-arg BASE=<REVIEWED_CONTROLLED_CORE_LOCAL_TAG>", example)
        self.assertIn("--target <EXPLICIT_VARIANT>", example)
        self.assertIn("--pull=false", example)
        self.assertIn("NOT executed", self.metadata["selection"]["command_note"])
        self.assertIn("NONDEFAULT", self.source)
        default = (BASE / "Dockerfile").read_text()
        self.assertNotIn("Dockerfile.variants", default)
        self.assertNotIn("variants.json", default)

    def test_recorded_apt_and_capability_uncertainties_are_not_success_claims(self):
        resolution = self.metadata["package_resolution"]
        self.assertIn("TODO", resolution["status"])
        self.assertIn("official Debian 13 APT", resolution["status"])
        special = " ".join(resolution["special_review"])
        for name in ("imagemagick", "libldap-dev", "default-libmysqlclient-dev", "tk-dev"):
            self.assertIn(name, special)
        variants = self.metadata["variants"]
        self.assertIn("NOT supported", variants["gui"]["tkinter_support"])
        self.assertIn("_tkinter", variants["gui"]["tkinter_support"])
        self.assertIn("does not rebuild Python", variants["gui"]["tkinter_support"])
        db_gaps = " ".join(variants["database-dev"]["gaps_and_required_checks"])
        for caveat in ("an addition", "GSS", "libdb5.3-dev", "SSH remains in BASE"):
            self.assertIn(caveat, db_gaps)
        self.assertNotIn("openssh-client", PACKAGES["database-dev"])
        self.assertIn("GdkPixbuf/Pango", " ".join(variants["gui"]["gaps_and_required_checks"]))
        self.assertIn("No ffmpeg or libvips baseline", " ".join(variants["media"]["gaps_and_required_checks"]))

    def test_fresh_security_sdk_and_provenance_acceptance_remain_required(self):
        acceptance = " ".join(self.metadata["acceptance_before_selection_or_publish"])
        for requirement in ("official APT", "SBOM", "Fresh raw/final", "unresolved",
                            "unchanged gates", "SDK/MCP", "owner approval",
                            "notbuilt/unvalidated"):
            self.assertIn(requirement, acceptance)
        self.assertEqual(self.metadata["source_matrix"],
                         "/home/mishuhashi/security-audit-skill/proxcloud/run-1/cve-pr209/"
                         "controlled-base-feature-matrix.json")
        self.assertEqual(self.metadata["context"], "deploy/templates")
        self.assertEqual(self.metadata["recipe"], "deploy/templates/base/Dockerfile.variants")


if __name__ == "__main__":
    unittest.main()
