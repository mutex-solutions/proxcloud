#!/usr/bin/env python3
"""Data-only API Dockerfile regression checks; never build or run an image.

These checks do not establish runtime compatibility or vulnerability status.
Hosted container smoke tests and a fresh scan of the built digest are required.
Run explicitly with: python3 -m unittest discover -s .github/scripts/tests
    -p test_api_runtime_contract.py
"""
import json
from pathlib import Path
import shlex
import unittest


ROOT = Path(__file__).resolve().parents[3]


def instructions(source):
    """Read the simple Dockerfile's logical instructions without a Docker daemon."""
    result = []
    pending = ""
    for line in source.splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        continued = line.endswith("\\")
        pending += line[:-1].strip() + " " if continued else line
        if not continued:
            keyword, value = pending.split(None, 1)
            result.append((keyword.upper(), value))
            pending = ""
    if pending:
        raise ValueError("unterminated Dockerfile continuation")
    return result


class APIRuntimeContractTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.source = (ROOT / "deploy/Dockerfile").read_text(encoding="utf-8")
        cls.steps = instructions(cls.source)
        stages = [i for i, (op, _) in enumerate(cls.steps) if op == "FROM"]
        if len(stages) != 2:
            raise AssertionError("API image must retain its build and runtime stages")
        cls.build = cls.steps[:stages[-1]]
        cls.runtime = cls.steps[stages[-1]:]

    def test_static_go_builder_and_source_boundary_are_unchanged(self):
        self.assertEqual(self.build, [
            ("FROM", "golang:1.27.4-bookworm AS build"),
            ("WORKDIR", "/src"),
            ("COPY", "go.mod go.sum ./"),
            ("RUN", "go mod download"),
            ("COPY", "cmd ./cmd"),
            ("COPY", "internal ./internal"),
            ("COPY", "pkg ./pkg"),
            ("RUN", 'CGO_ENABLED=0 GOOS=linux go build -trimpath '
             '-ldflags="-s -w" -o /out/proxcloud-api ./cmd/api'),
        ])

    def test_runtime_is_exact_alpine_release(self):
        self.assertEqual(self.runtime[0], ("FROM", "alpine:3.24.4"))

    def test_packages_upgrade_before_minimal_client_install(self):
        runs = [value for op, value in self.runtime if op == "RUN"]
        # Exact allow-list also prevents installing a local PostgreSQL server,
        # deleting package metadata to hide findings, or extra shell actions.
        self.assertEqual([shlex.split(value) for value in runs], [
            ["apk", "upgrade", "--no-cache", "&&", "apk", "add", "--no-cache",
             "bash", "ca-certificates", "postgresql18-client"],
            ["mkdir", "-p", "/opt/proxcloud/api"],
            ["adduser", "-S", "-D", "-H", "-u", "10001", "-G", "root", "proxcloud"],
        ])
        self.assertNotIn("useradd", " ".join(runs))

    def test_binary_identity_mount_ports_and_entrypoint_are_preserved(self):
        non_runs = [(op, value) for op, value in self.runtime if op != "RUN"]
        self.assertEqual(non_runs, [
            ("FROM", "alpine:3.24.4"),
            ("COPY", "--from=build /out/proxcloud-api /usr/local/bin/proxcloud-api"),
            ("USER", "10001"),
            ("EXPOSE", "8080 8081"),
            ("ENTRYPOINT", '["/usr/local/bin/proxcloud-api"]'),
        ])
        entrypoint = next(value for op, value in self.runtime if op == "ENTRYPOINT")
        self.assertEqual(json.loads(entrypoint), ["/usr/local/bin/proxcloud-api"])
        # No runtime WORKDIR existed before this change; keep its default cwd.
        self.assertFalse(any(op == "WORKDIR" for op, _ in self.runtime))

    def test_psql_authentication_boundary_is_explicit(self):
        self.assertIn("supports password/TLS", self.source)
        self.assertIn("Kerberos/GSSAPI seeding is not provided", self.source)
        self.assertIn("static Go/pgx database authentication is unchanged", self.source)


if __name__ == "__main__":
    unittest.main()
