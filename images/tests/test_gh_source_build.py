"""Data-only contracts for the isolated GitHub CLI source-build candidate.

These stdlib tests inspect text only: no Go/source execution, dependency install,
container/image build, authentication, or network. Passing does NOT prove native
build success, unit-test success, CVE remediation, or runtime scan acceptance.
"""
from pathlib import Path
import re
import unittest

ROOT = Path(__file__).resolve().parents[2]
DOCKERFILE = ROOT / "deploy/security/gh/Dockerfile"
COMMIT = "63837fb778d6a39f02be0ce76ff89f2bc8d74bdc"
GO_MOD_SHA256 = "db11ba3547fe6420c763b5fb208bb5e41a44598895f94481be01ea967fc421e2"


class GhSourceBuildTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.source = DOCKERFILE.read_text()
        cls.commands = "\n".join(
            line for line in cls.source.splitlines()
            if not line.lstrip().startswith("#")
        )

    def test_exact_policy_toolchain_and_artifact_only_stages(self):
        self.assertEqual(re.findall(r"^FROM (.+)$", self.commands, re.M), [
            "golang:1.27.4-alpine AS builder", "scratch AS artifacts",
        ])
        self.assertIn("COPY --from=builder /out/ /", self.commands)
        self.assertIn("GOTOOLCHAIN=local", self.commands)
        self.assertIn("CGO_ENABLED=0", self.commands)
        self.assertIn("RUN apk add --no-cache ca-certificates git", self.commands)
        for forbidden in ("ENTRYPOINT", "CMD ", "ADD ", "ARG ",
                          "/usr/local/bin", "install-gh.sh", "docker push"):
            self.assertNotIn(forbidden, self.commands)

    def test_literal_commit_checkout_hash_gate_before_go_execution(self):
        clone = "git clone --no-checkout https://github.com/cli/cli.git cli"
        checkout = f"git checkout --detach {COMMIT}"
        check = f'test "$(git rev-parse HEAD)" = {COMMIT}'
        hash_gate = f"printf '%s  %s\\n' {GO_MOD_SHA256} go.mod"
        for expected in (clone, checkout, check, hash_gate, "| sha256sum -c -",
                         'test -z "$(git status --porcelain)"'):
            self.assertIn(expected, self.commands)
        self.assertLess(self.commands.index(clone), self.commands.index(checkout))
        self.assertLess(self.commands.index(checkout), self.commands.index(check))
        self.assertLess(self.commands.index(check), self.commands.index(hash_gate))
        self.assertLess(self.commands.index(hash_gate), self.commands.index("go list"))
        self.assertNotRegex(self.commands, r"\b(?:GH_COMMIT|GH_VERSION)=")

    def test_minimal_dependency_change_fails_closed_not_a_downgrade(self):
        original = 'test "$(go list -m -f \'{{.Version}}\' golang.org/x/net)" = v0.60.0'
        edit = "go mod edit -require=golang.org/x/net@v0.62.0"
        fixed = 'test "$(go list -m -f \'{{.Version}}\' golang.org/x/net)" = v0.62.0'
        for expected in (original, edit, "go mod tidy", fixed, "go mod verify",
                         "GOPROXY=https://proxy.golang.org", "GOSUMDB=sum.golang.org"):
            self.assertIn(expected, self.commands)
        self.assertLess(self.commands.index(original), self.commands.index(edit))
        self.assertLess(self.commands.index(edit), self.commands.index("go mod tidy"))
        self.assertLess(self.commands.index("go mod tidy"), self.commands.index(fixed))
        self.assertIn("git diff --exit-code -- . ':!go.mod' ':!go.sum'", self.commands)
        self.assertEqual(re.findall(r"go mod edit[^;\n]*", self.commands), [edit])
        for bypass in ("GOSUMDB=off", "GOINSECURE", "go get ", "-u ",
                       "replace=", "|| true", "|| :", "--insecure", "GOTOOLCHAIN=auto"):
            self.assertNotIn(bypass, self.commands)

    def test_future_build_retains_version_identity_and_embedded_module_evidence(self):
        self.assertIn("go test -mod=readonly ./internal/build ./internal/ghrepo golang.org/x/net/html", self.commands)
        self.assertIn("go build -mod=readonly -trimpath -buildvcs=false", self.commands)
        self.assertIn("-X github.com/cli/cli/v2/internal/build.Version=2.103.3+proxcloud1", self.commands)
        self.assertIn("-o /out/gh ./cmd/gh", self.commands)
        self.assertIn("go version -m /out/gh > /out/go-buildinfo.txt", self.commands)
        self.assertIn('$2 == "golang.org/x/net" && $3 == "v0.62.0"', self.commands)
        self.assertIn("END { exit !found }", self.commands)
        self.assertIn("HOME=/tmp/gh-empty-home GH_CONFIG_DIR=/tmp/gh-empty-home", self.commands)
        self.assertIn("/out/gh --version > /out/gh-version.txt", self.commands)
        self.assertIn("'gh version 2.103.3+proxcloud1'*", self.commands)
        self.assertNotIn("auth login", self.commands)
        self.assertNotIn("--mount=type=secret", self.commands)

    def test_export_license_modified_source_diff_and_honest_provenance(self):
        for expected in (
            "cp go.mod go.sum /out/upstream/",
            "cp go.mod go.sum /out/modified/",
            "git diff -- go.mod go.sum > /out/source-dependency-diff.patch",
            "test -s /out/source-dependency-diff.patch",
            "go list -mod=readonly -m -json all > /out/modules.json",
            "cp LICENSE /out/LICENSE",
            "SOURCE_BUILD_CANDIDATE_NOT_RUNTIME_SECURITY_VALIDATED",
            "not the official release binary",
            "NOT a complete SPDX/CycloneDX SBOM",
            "Fresh artifact/stdlib/module and integrated runtime scans are required",
            "No scanner suppressions",
            "sha256sum gh LICENSE source-commit.txt source-dependency-diff.patch",
            "> SHA256SUMS",
        ):
            self.assertIn(expected, self.commands)
        self.assertNotIn("trivy ignore", self.commands)
        self.assertNotIn(".trivyignore", self.commands)


if __name__ == "__main__":
    unittest.main(verbosity=2)
