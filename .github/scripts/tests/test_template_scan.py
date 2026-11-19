"""Offline template scanner fixtures: mocked subprocesses only, never Docker/builds."""

import importlib.util
import io
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch


SCRIPT = Path(__file__).resolve().parents[1] / "scan_template_images.py"
BUILD = Path(__file__).resolve().parents[3] / "deploy/templates/build.sh"
SPEC = importlib.util.spec_from_file_location("scan_template_images", SCRIPT)
SCAN = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(SCAN)
SHA = "a" * 40
IMAGE_ID = "sha256:" + "b" * 64


class TemplateTests(unittest.TestCase):
    def test_fixed_eleven_refs_exclude_gpu_and_external_python(self):
        refs = SCAN.expected_refs(SHA)
        self.assertEqual(len(refs), 11)
        self.assertNotIn("code-interpreter-gpu", refs)
        self.assertNotIn("python", refs)
        seen = set()
        for name, ref in refs.items():
            self.assertEqual(SCAN.parse_ref(f"{name} {ref}", refs, seen), (name, ref))
        self.assertEqual(seen, set(SCAN.NAMES))

    def test_sha_is_not_a_shell_or_ref_input(self):
        for sha in ("", "A" * 40, SHA + ";echo bad", "a" * 39, "a" * 41):
            with self.subTest(sha=sha), self.assertRaises(ValueError):
                SCAN.expected_refs(sha)

    def test_malformed_unknown_wrong_sha_or_duplicate_fail_closed(self):
        refs = SCAN.expected_refs(SHA)
        bad = ["", "base", "base " + refs["base"] + " extra",
               "base " + refs["base"] + "@sha256:" + "b" * 64,
               "base " + refs["base"].replace(SHA, "c" * 40),
               "base security-scan.local/sandbox-node:" + SHA,
               "code-interpreter-gpu security-scan.local/sandbox-code-interpreter-gpu:" + SHA,
               "::warning::bad", "base " + refs["base"] + "\r"]
        for line in bad:
            with self.subTest(line=line), self.assertRaises(ValueError):
                SCAN.parse_ref(line, refs, set())
        with self.assertRaises(ValueError):
            SCAN.parse_ref("base " + refs["base"], refs, {"base"})

    def scan_fixture(self, scan_rc=0, gate_rc=0, inspect_id=IMAGE_ID, timeout=False):
        calls = []
        def fake_run(argv, deadline, limit, **kwargs):
            calls.append((argv, limit))
            if argv[0] == "docker":
                return subprocess.CompletedProcess(argv, 0, inspect_id + "\n", "")
            if timeout:
                raise subprocess.TimeoutExpired(argv, limit)
            return subprocess.CompletedProcess(argv, gate_rc if argv[0] == SCAN.sys.executable else scan_rc)
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            with patch.object(SCAN, "run_command", side_effect=fake_run):
                status = SCAN.scan_image(root, "base", SCAN.expected_refs(SHA)["base"], 999999999)
            target = root / "result/templates/base"
            self.assertEqual((target / "status.txt").read_text().strip(), str(status))
            self.assertTrue((target / "gate.txt").exists())
        return status, calls

    def test_clean_vulnerable_invalid_and_engine_statuses(self):
        for scan_rc, gate_rc, expected in ((0, 0, 0), (0, 1, 1), (0, 2, 2),
                                           (1, 0, 2), (1, 1, 1), (3, 1, 2)):
            with self.subTest(scan_rc=scan_rc, gate_rc=gate_rc):
                status, calls = self.scan_fixture(scan_rc, gate_rc)
                self.assertEqual(status, expected)
                self.assertEqual([limit for _, limit in calls], [30, 600, 30])
                scan = calls[1][0]
                self.assertIn("--list-all-pkgs", scan)
                self.assertIn("--ignore-unfixed=false", scan)
                self.assertEqual(scan[-1], IMAGE_ID)
                self.assertIn("--expected-image-id", calls[2][0])

    def test_invalid_inspect_and_scan_timeout_are_incomplete(self):
        status, calls = self.scan_fixture(inspect_id="unknown")
        self.assertEqual(status, 2)
        self.assertEqual(len(calls), 1)
        self.assertEqual(self.scan_fixture(timeout=True)[0], 2)

    def test_wall_budget_limits_each_command(self):
        with patch.object(SCAN.time, "monotonic", return_value=100), \
                patch.object(SCAN.subprocess, "run") as run:
            SCAN.run_command(["fixture"], 120, 600)
            self.assertEqual(run.call_args.kwargs["timeout"], 15)
            with self.assertRaises(TimeoutError):
                SCAN.run_command(["fixture"], 104, 600)

    def test_missing_is_split_into_the_failed_target_and_those_not_built(self):
        missing = set(SCAN.NAMES) - {"base"}
        failed, not_built = SCAN.split_missing(missing, 1)
        self.assertEqual(failed, "mcp-gateway")
        self.assertEqual(not_built, list(SCAN.NAMES[2:]))
        self.assertEqual(SCAN.split_missing(set(), 0), (None, []))
        self.assertEqual(SCAN.split_missing(missing, None), (None, []))
        self.assertEqual(SCAN.split_missing(missing, -9), (None, []))

    def run_builder(self, names, fail, times=99):
        """build.sh with a stub docker on PATH that fails the first `times` builds of one image name."""
        with tempfile.TemporaryDirectory() as directory:
            stub = Path(directory) / "docker"
            stub.write_text('#!/bin/sh\ncase " $* " in *"/sandbox-$FAIL:"*) ;; *) exit 0;; esac\n'
                            'n=$(($(cat "$0.n" 2>/dev/null || echo 0) + 1)); echo "$n" > "$0.n"\n'
                            'echo "stub: build $n of $FAIL" >&2; [ "$n" -gt "$TIMES" ] || exit 7\n', encoding="utf-8")
            stub.chmod(0o755)
            env = dict(os.environ, PATH=directory + os.pathsep + os.environ["PATH"], FAIL=fail, TIMES=str(times),
                       PUSH="0", REGISTRY="security-scan.local", TAG=SHA, RETRY_PAUSE="0")
            return subprocess.run([str(BUILD), *names], env=env, capture_output=True, text=True, timeout=60)

    def test_builder_names_the_failed_target_and_those_not_built(self):
        done = self.run_builder(SCAN.NAMES, "mcp-gateway")
        self.assertEqual(done.returncode, 7)
        self.assertEqual(done.stdout, "base " + SCAN.expected_refs(SHA)["base"] + "\n")
        self.assertIn("build.sh: FAILED: mcp-gateway (exit 7); NOT BUILT: " + " ".join(SCAN.NAMES[2:]) + "\n",
                      done.stderr)
        # A dependency that is not in the list: base fails while codex is the target.
        done = self.run_builder(("codex", "node"), "base")
        self.assertEqual(done.returncode, 7)
        self.assertIn("build.sh: FAILED: base (exit 7); NOT BUILT: codex node\n", done.stderr)
        done = self.run_builder(("node", "browser"), "browser")
        self.assertIn("build.sh: FAILED: browser (exit 7); NOT BUILT: none\n", done.stderr)
        # A template that built is not named as failed when the run ends on a later error.
        done = self.run_builder(("node", "bogus"), "none")
        self.assertEqual(done.returncode, 2)
        self.assertIn("unknown template bogus", done.stderr)
        self.assertNotIn("FAILED", done.stderr)

    def test_builder_tries_a_failed_build_three_times_and_no_more(self):
        done = self.run_builder(("node", "browser", "desktop"), "browser", times=2)
        self.assertEqual(done.returncode, 0)
        self.assertEqual([line.split()[0] for line in done.stdout.splitlines()], ["node", "browser", "desktop"])
        self.assertEqual(done.stderr.count("stub: build"), 3)
        self.assertIn("build.sh: browser failed (exit 7), attempt 2 of 3; next attempt in 0s\n", done.stderr)
        self.assertNotIn("FAILED", done.stderr)
        # A build that keeps failing is not tried a fourth time, and ends the run as it did before.
        done = self.run_builder(("node", "browser", "desktop"), "browser")
        self.assertEqual(done.returncode, 7)
        self.assertEqual(done.stderr.count("stub: build"), 3)
        self.assertNotIn("attempt 3 of 3", done.stderr)
        self.assertTrue(done.stderr.endswith("build.sh: FAILED: browser (exit 7); NOT BUILT: desktop\n"))

    def test_builder_reports_nothing_when_every_target_builds(self):
        done = self.run_builder(SCAN.NAMES, "none")
        self.assertEqual(done.returncode, 0)
        self.assertEqual(len(done.stdout.splitlines()), len(SCAN.NAMES))
        self.assertNotIn("FAILED", done.stderr)


if __name__ == "__main__":
    unittest.main()
