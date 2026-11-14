#!/usr/bin/env python3
"""Data-only release policy, installer version floor and app release contract tests.

Uses the installer lane's existing Python/PyYAML; no subprocesses, publication,
package installation, registry probes, Ansible execution or cluster access.
"""
import contextlib
import copy
import importlib.util
import io
from pathlib import Path
import re
import unittest
from unittest import mock

import yaml


ROOT = Path(__file__).resolve().parents[3]


def load_module(name, relative_path):
    spec = importlib.util.spec_from_file_location(name, ROOT / relative_path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


policy = load_module("release_policy", ".github/scripts/release_policy.py")
jobs = load_module("installer_jobs", "installer/filter_plugins/jobs.py")


def read_yaml(relative_path):
    return yaml.safe_load((ROOT / relative_path).read_text(encoding="utf-8"))


class ReleasePolicyTests(unittest.TestCase):
    def test_stable_is_explicitly_latest_not_prerelease(self):
        for tag in ("v0.4.0", "v1.2.3", "v10.20.30"):
            with self.subTest(tag=tag):
                self.assertEqual(policy.release_policy(tag), {
                    "tag": tag, "version": tag[1:], "flags": ["--latest"],
                })

    def test_rc_is_explicitly_prerelease_and_never_latest(self):
        for tag in ("v0.4.0-rc.1", "v1.2.3-rc.0", "v1.2.3-rc.10"):
            with self.subTest(tag=tag):
                self.assertEqual(policy.release_policy(tag), {
                    "tag": tag, "version": tag[1:],
                    "flags": ["--prerelease", "--latest=false"],
                })

    def test_malformed_or_non_app_release_tags_fail_closed(self):
        for tag in (None, "", "0.4.0", "v0.4", "v0.4.0.1", "v01.4.0", "v0.04.0", "v0.4.00",
                    "v0.4.0-rc", "v0.4.0-rc.01", "v0.4.0-rc.-1", "v0.4.0-RC.1",
                    "v0.4.0-alpha.1", "v0.4.0+build.1", "v0.4.0-rc.1+build.1",
                    "v0.4.0\n", "v0.4.0-rc.1\r\n", "v0.4.0\nflags=--latest",
                    " v0.4.0", "v0.4.0 ", "v0.4.0;echo bad", "v0.4.0-rc.$(id)",
                    "v０.4.0", "database-cluster-v0.5.1"):
            with self.subTest(tag=tag):
                with self.assertRaises(ValueError):
                    policy.release_policy(tag)

    def test_main_reads_environment_and_emits_only_validated_outputs(self):
        output = io.StringIO()
        with mock.patch.dict(policy.os.environ, {"GITHUB_REF_NAME": "v0.4.0-rc.1"}, clear=True):
            with contextlib.redirect_stdout(output):
                self.assertEqual(policy.main(), 0)
        self.assertEqual(output.getvalue(),
                         "tag=v0.4.0-rc.1\nversion=0.4.0-rc.1\nflags=--prerelease --latest=false\n")

    def test_invalid_environment_emits_no_step_outputs_or_untrusted_tag(self):
        for tag in ("", "v0.4.0\n::notice::injected", "v0.4.0-rc.01"):
            with self.subTest(tag=tag):
                output, errors = io.StringIO(), io.StringIO()
                with mock.patch.dict(policy.os.environ, {"GITHUB_REF_NAME": tag}, clear=True):
                    with contextlib.redirect_stdout(output), contextlib.redirect_stderr(errors):
                        self.assertEqual(policy.main(), 1)
                self.assertEqual(output.getvalue(), "")
                self.assertIn("::error::app release tag must be", errors.getvalue())
                self.assertNotIn("::notice::", errors.getvalue())


class InstallerReleaseFloorTests(unittest.TestCase):
    def setUp(self):
        self.config = read_yaml("installer/playbooks/group_vars/all/defaults.yml")["pc_defaults"]
        self.config["images"]["build"] = False

    def test_valid_semver_uses_broker_free_base_floor_not_stable_precedence(self):
        # Any valid prerelease of the broker-free base is eligible, not only RCs.
        # This syntax check is deliberately not proof the named artifact exists.
        for release in ("v0.4.0-rc.1", "0.4.0-rc.10", "v0.4.0", "0.4.1", "v1.0.0",
                        "v0.4.0-alpha", "v0.4.0-alpha.0", "v0.4.0-0", "v0.4.0-01a",
                        "v0.4.0+build.01", "v0.4.0-rc.1+build.001"):
            with self.subTest(release=release):
                self.assertEqual(jobs.jobs_config_problem(self.config, release), "")

    def test_old_broker_releases_and_malformed_semver_are_refused(self):
        for release in ("v0.3.29", "v0.3.999-rc.1+build.1", "", "dev-abcdef0", "v0.4", "v0.4.0.1",
                        "v00.4.0", "v0.04.0", "v0.4.00", "v0.4.0-rc.01", "v0.4.0-00",
                        "v0.4.0-alpha.01", "v0.4.0-rc..1", "v0.4.0-", "v0.4.0+",
                        "v0.4.0+build..1", "v0.4.0+build_1", "v0.4.0++build", "v0.4.0-rc.1+build/1",
                        "v0.4.0\n", "v0.4.0-rc.1\r\n", "v0.4.0\nother=value", " v0.4.0", "v0.4.0 ", "v０.4.0"):
            with self.subTest(release=release):
                self.assertIn("published broker-free release", jobs.jobs_config_problem(self.config, release))

    def test_source_builds_remain_possible_before_publication(self):
        self.config["images"]["build"] = True
        self.assertEqual(jobs.jobs_config_problem(self.config, "unpublished-checkout"), "")

    def test_rc_does_not_bypass_existing_worker_duration_and_database_guards(self):
        cases = [
            ("jobs", "worker_concurrency", 0), ("jobs", "worker_concurrency", 17),
            ("jobs", "worker_concurrency", True), ("jobs", "poll_interval", "99ms"),
            ("jobs", "poll_interval", "61s"), ("jobs", "lease_duration", "9s"),
            ("jobs", "lease_duration", "11m"), ("jobs", "scheduler_enabled", "false"),
            ("jobs", "debug_triggers", "true"), ("database", "instances", 0),
            ("database", "status_read_replica", True), ("images", "build", "false"),
        ]
        for section, key, value in cases:
            with self.subTest(section=section, key=key, value=value):
                config = copy.deepcopy(self.config)
                config[section][key] = value
                self.assertIn(section + "." + key, jobs.jobs_config_problem(config, "v0.4.0-rc.1"))


class ReleaseContractTests(unittest.TestCase):
    def test_chart_images_and_installer_pin_exact_same_stable_release(self):
        chart = read_yaml("deploy/charts/proxcloud/Chart.yaml")
        versions = read_yaml("installer/playbooks/group_vars/all/versions.yml")["pc_versions"]
        self.assertEqual(chart["version"], "0.5.2")
        self.assertEqual(chart["appVersion"], "v0.5.2")
        self.assertEqual(versions["proxcloud"], chart["appVersion"])
        pinned = policy.release_policy(versions["proxcloud"])
        self.assertEqual(pinned["version"], chart["version"])
        # The pinned app release is stable: published as latest, never a prerelease.
        self.assertEqual(pinned["flags"], ["--latest"])
        # The Docker Compose install (deploy/compose) defaults to the same release, in one place.
        compose_script = (ROOT / "deploy/compose/proxcloud-up.sh").read_text(encoding="utf-8")
        self.assertEqual(re.findall(r'(?m)^DEFAULT_VERSION="([^"]*)"$', compose_script), [chart["appVersion"]])
        compose_file = (ROOT / "deploy/compose/compose.yaml").read_text(encoding="utf-8")
        self.assertEqual(set(re.findall(r"proxcloud-(?:api|web|admin):(\S+)", compose_file)),
                         {"${PROXCLOUD_VERSION:?run", "${PROXCLOUD_VERSION}"})
        defaults = read_yaml("installer/playbooks/group_vars/all/defaults.yml")["pc_defaults"]
        self.assertIs(defaults["images"]["build"], True)
        self.assertIs(defaults["jobs"]["scheduler_enabled"], False)
        self.assertIs(defaults["jobs"]["debug_triggers"], False)

    def test_managed_chart_versions_stay_independent(self):
        self.assertEqual(read_yaml("deploy/charts/database-cluster/Chart.yaml")["version"], "0.5.1")
        self.assertEqual(read_yaml("deploy/charts/proxmox-kamaji-cluster/Chart.yaml")["version"], "0.1.10")

    def test_image_and_app_chart_publication_validate_before_effects(self):
        build = read_yaml(".github/workflows/docker.yml")["jobs"]["build"]["steps"]
        compute = next(step for step in build if step.get("id") == "meta")
        push = next(step for step in build if step.get("name") == "Build and push")
        self.assertLess(build.index(compute), build.index(push))
        self.assertIn('if [ "$GITHUB_REF_TYPE" = "tag" ]; then', compute["run"])
        self.assertIn('python3 .github/scripts/release_policy.py >> "$GITHUB_OUTPUT"', compute["run"])
        self.assertNotIn("${{ github.ref_name }}", compute["run"])
        publish = read_yaml(".github/workflows/helm.yml")["jobs"]["publish"]["steps"]
        validate = next(step for step in publish if step.get("id") == "policy")
        login = next(step for step in publish if step.get("name") == "ghcr login")
        package = next(step for step in publish if step.get("name") == "Package + publish chart at the tag version")
        self.assertLess(publish.index(validate), publish.index(login))
        self.assertLess(publish.index(validate), publish.index(package))
        self.assertEqual(validate["env"]["GITHUB_REF_NAME"], "${{ github.ref_name }}")
        self.assertEqual(validate["run"], 'python3 .github/scripts/release_policy.py >> "$GITHUB_OUTPUT"')
        self.assertEqual(package["env"]["RELEASE_TAG"], "${{ steps.policy.outputs.tag }}")
        self.assertEqual(package["env"]["RELEASE_VERSION"], "${{ steps.policy.outputs.version }}")
        self.assertIn('--app-version "$RELEASE_TAG"', package["run"])
        self.assertNotIn("${{", package["run"])

    def test_release_job_uses_validated_environment_and_array_flags(self):
        workflow = read_yaml(".github/workflows/docker.yml")
        steps = workflow["jobs"]["release"]["steps"]
        validate = next(step for step in steps if step.get("id") == "policy")
        self.assertEqual(validate["env"]["GITHUB_REF_NAME"], "${{ github.ref_name }}")
        self.assertEqual(validate["run"], 'python3 .github/scripts/release_policy.py >> "$GITHUB_OUTPUT"')
        notes = next(step for step in steps if step.get("name") == "Assemble release notes from CHANGELOG")
        create = next(step for step in steps if step.get("name") == "Create GitHub Release")
        self.assertLess(steps.index(validate), steps.index(notes))
        self.assertLess(steps.index(validate), steps.index(create))
        self.assertEqual(notes["env"]["RELEASE_VERSION"], "${{ steps.policy.outputs.version }}")
        self.assertEqual(create["env"]["RELEASE_TAG"], "${{ steps.policy.outputs.tag }}")
        self.assertEqual(create["env"]["RELEASE_FLAGS"], "${{ steps.policy.outputs.flags }}")
        for step in (validate, notes, create):
            self.assertNotIn("${{", step["run"])
        self.assertIn('read -r -a release_flags <<< "$RELEASE_FLAGS"', create["run"])
        self.assertIn('"${release_flags[@]}"', create["run"])
        self.assertIn('--verify-tag', create["run"])
        self.assertNotIn("eval ", create["run"])
        test_steps = read_yaml(".github/workflows/test.yml")["jobs"]["installer"]["steps"]
        installer = next(step for step in test_steps if step.get("name") == "Check the installer")
        self.assertIn(".venv/bin/python ../.github/scripts/tests/test_release_policy.py", installer["run"])


if __name__ == "__main__":
    unittest.main(verbosity=2)
