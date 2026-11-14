"""Pure contract tests for the VM export tool image: its Containerfile, smoke script and workflow.

Text only: no docker, network, registry or GitHub Actions.
"""
from pathlib import Path
import re
import sys
import unittest

IMAGES = Path(__file__).resolve().parents[1]
ROOT = IMAGES.parent
sys.path.insert(0, str(IMAGES))
from vm_export import capsule  # noqa: E402

TOOLS = IMAGES / "vm_export" / "tools"
WORKFLOWS = ROOT / ".github" / "workflows"
CONTAINERFILE = (TOOLS / "Containerfile").read_text()
SMOKE = (TOOLS / "smoke.sh").read_text()
WORKFLOW = (WORKFLOWS / "vm-export-tools.yml").read_text()
CHECK, PUBLISH = WORKFLOW.split("\n  check:\n", 1)[1].split("\n  publish:\n", 1)
PINNED_USE = re.compile(r"uses: ([A-Za-z0-9_./-]+@[0-9a-f]{40}) # v\S+")


def instructions(text):
    """Containerfile instructions without comments, continuation lines joined."""
    lines = [line for line in text.splitlines() if not line.lstrip().startswith("#")]
    return [row.strip() for row in re.sub(r"\\\n", " ", "\n".join(lines)).splitlines() if row.strip()]


class ContainerfileTests(unittest.TestCase):
    def setUp(self):
        self.rows = instructions(CONTAINERFILE)
        self.run = [row for row in self.rows if row.startswith("RUN ")]

    def test_one_stage_from_a_digest_pinned_base(self):
        self.assertEqual([row for row in self.rows if row.startswith("FROM ")],
                         [self.rows[0]])
        self.assertRegex(self.rows[0], r"^FROM debian@sha256:[0-9a-f]{64}$")

    def test_every_package_comes_from_one_pinned_snapshot(self):
        self.assertEqual(len(self.run), 1)
        run = self.run[0]
        self.assertEqual(len(re.findall(r"snapshot=20[0-9]{6}T[0-9]{6}Z;", run)), 1)
        self.assertIn("archive=http://snapshot.debian.org/archive;", run)
        self.assertIn("rm /etc/apt/sources.list.d/debian.sources;", run)
        sources = re.findall(r'echo "deb ([^"]+)"', run)
        self.assertEqual(sources, ["$options $archive/debian/$snapshot bookworm main",
                                   "$options $archive/debian/$snapshot bookworm-updates main",
                                   "$options $archive/debian-security/$snapshot bookworm-security main"])
        # A snapshot's Release file expires; the signature is still verified with the base keyring.
        self.assertIn("options='[signed-by=/usr/share/keyrings/debian-archive-keyring.gpg check-valid-until=no]';", run)
        for item in ("deb.debian.org", "security.debian.org", "trusted=yes", "allow-unauthenticated",
                     "AllowUnauthenticated", "AllowInsecureRepositories", "--force-yes", "curl", "wget", "| sh"):
            self.assertNotIn(item, CONTAINERFILE)
        self.assertLess(run.index("> /etc/apt/sources.list;"), run.index("apt-get"))

    def test_only_what_a_capsule_executes_is_requested(self):
        packages = re.search(r"install -y --no-install-recommends ([a-z0-9 .+-]+);", self.run[0])[1].split()
        self.assertEqual(sorted(packages), ["e2fsprogs", "linux-image-amd64", "python3", "python3-guestfs",
                                            "qemu-utils", "supermin"])
        self.assertIn("chmod -R a+rX /boot /lib/modules", self.run[0])

    def test_no_remote_add_no_secret_no_account_and_a_numeric_non_root_user(self):
        names = [row.split()[0] for row in self.rows]
        self.assertEqual(sorted(set(names)), ["ARG", "ENV", "FROM", "LABEL", "RUN", "USER"])
        self.assertEqual([row for row in self.rows if row.startswith("ARG ")], ["ARG REVISION=unknown"])
        self.assertEqual([row for row in self.rows if row.startswith("USER ")], ["USER 1000:1000"])
        for item in ("useradd", "adduser", "--mount=type=secret", "TOKEN", "PASSWORD"):
            self.assertNotIn(item, CONTAINERFILE)

    def test_image_environment_matches_the_capsule_and_labels_are_set(self):
        env = next(row for row in self.rows if row.startswith("ENV "))[4:].split()
        expected = [item for item in capsule.ENV if item.startswith(("HOME=", "TMPDIR=", "LIBGUESTFS_"))]
        self.assertEqual(sorted(env), sorted(expected))
        label = next(row for row in self.rows if row.startswith("LABEL "))
        self.assertIn('org.opencontainers.image.source="https://github.com/mutex-solutions/proxcloud"', label)
        self.assertIn('org.opencontainers.image.revision="$REVISION"', label)
        self.assertRegex(label, r'org\.opencontainers\.image\.description="[^"]{20,}"')


class SmokeTests(unittest.TestCase):
    def test_capsule_confinement_as_an_arbitrary_uid_without_a_passwd_entry(self):
        for item in ("set -eu", "uid=1002", '--user="$uid:$uid"', "--pull=never", "--network=none", "--read-only",
                     "--cap-drop=ALL", "--security-opt=no-new-privileges", "--entrypoint /usr/bin/env",
                     '--tmpfs "/tmp:', '--tmpfs "/scratch:', "/usr/bin/python3 -I -c", "pwd.getpwuid(uid)",
                     "import guestfs", '"/usr/bin/qemu-img", "--version"', 'g.set_backend_setting("force_tcg", "true")',
                     'format="qcow2", readonly=True', "g.launch()", "g.list_devices()"):
            self.assertIn(item, SMOKE)
        for item in ("--privileged", "--device", "/dev/kvm", "--cap-add", "docker.sock", " -v ", "--volume", "--mount"):
            self.assertNotIn(item, SMOKE)
        for item in capsule.ENV:  # The exact environment a capsule gets from `env -i`.
            self.assertIn(" " + item, SMOKE.replace("\\\n   ", ""))


class WorkflowTests(unittest.TestCase):
    def test_actions_are_sha_pins_already_used_by_another_workflow(self):
        used = re.findall(r"uses: (\S+)", WORKFLOW)
        pinned = PINNED_USE.findall(WORKFLOW)
        self.assertEqual(len(used), 5)
        self.assertEqual(used, pinned)
        elsewhere = set()
        for path in WORKFLOWS.glob("*.yml"):
            if path.name != "vm-export-tools.yml":
                elsewhere.update(PINNED_USE.findall(path.read_text()))
        self.assertLessEqual(set(pinned), elsewhere)
        self.assertEqual({item.split("@")[0] for item in pinned}, {"actions/checkout", "actions/upload-artifact"})

    def test_triggers_and_least_privilege(self):
        triggers = WORKFLOW.split("\non:\n", 1)[1].split("\npermissions:", 1)[0]
        paths = '    paths: ["images/vm_export/tools/**", ".github/workflows/vm-export-tools.yml"]\n'
        self.assertEqual(triggers, "  push:\n    branches: [main]\n" + paths
                         + "  pull_request:\n    branches: [main]\n" + paths + "  workflow_dispatch:\n")
        self.assertIn("\npermissions:\n  contents: read\n", WORKFLOW)
        self.assertEqual(WORKFLOW.count("permissions:"), 2)
        self.assertEqual(re.findall(r"^\s+[a-z-]+: write$", WORKFLOW, flags=re.M), ["      packages: write"])
        self.assertIn("    permissions:\n      contents: read\n      packages: write\n", PUBLISH)
        self.assertIn("  cancel-in-progress: ${{ github.event_name == 'pull_request' }}\n", WORKFLOW)
        self.assertEqual(WORKFLOW.count("runs-on: ubuntu-24.04"), 2)
        self.assertEqual(WORKFLOW.count("persist-credentials: false"), 2)
        for item in ("self-hosted", "secrets.", "proxcloud-pve", "continue-on-error", "|| true", "pull_request_target",
                     "workflow_run", "schedule:", "docker/login-action", "docker/build-push-action"):
            self.assertNotIn(item, WORKFLOW)
        # Expressions never reach a shell script: this is the complete list, all in if/env/with/concurrency.
        self.assertEqual(sorted(set(re.findall(r"\$\{\{ (.+?) \}\}", WORKFLOW))), [
            "github.event.pull_request.head.sha || github.sha", "github.event.pull_request.number || github.ref",
            "github.event_name == 'pull_request'", "github.sha", "github.token"])

    def test_pull_request_job_cannot_log_in_or_push(self):
        self.assertTrue(CHECK.startswith("    if: github.event_name == 'pull_request'\n"))
        for item in ("permissions:", "packages:", "docker login", "docker push", "docker tag", "github.token",
                     "GH_TOKEN", "$PACKAGE"):
            self.assertNotIn(item, CHECK)

    def test_publish_job_is_main_only_in_this_repository(self):
        self.assertTrue(PUBLISH.startswith(
            "    if: github.event_name != 'pull_request' && github.repository == 'mutex-solutions/proxcloud'"
            " && github.ref == 'refs/heads/main'\n"))
        self.assertIn("  PACKAGE: ghcr.io/mutex-solutions/proxcloud-vm-export-tools\n", WORKFLOW)

    def test_both_jobs_build_smoke_scan_and_gate_identically(self):
        shared = CHECK.split("    steps:\n", 1)[1].split("\n  # Trusted runs only.", 1)[0]
        self.assertIn(shared, PUBLISH)
        for item in ("docker build --pull --platform linux/amd64 --provenance=false --load",
                     '--file images/vm_export/tools/Containerfile --build-arg "REVISION=$SOURCE_SHA"',
                     'run: images/vm_export/tools/smoke.sh "$LOCAL_IMAGE"',
                     "--ignore-unfixed=false --exit-on-eol 1", "--config '' --ignorefile ''",
                     '--expected-image-id "$(<result/image-id.txt)" result/scan.json'):
            self.assertIn(item, shared)
        self.assertNotIn("SNAPSHOT", WORKFLOW)  # The snapshot pin lives in the Containerfile only.
        # The scanner release and the gate are the ones of security-scan.yml, not a copy that can drift.
        scan = (WORKFLOWS / "security-scan.yml").read_text()
        for line in re.findall(r"^  TRIVY_(?:VERSION|SHA256): .+$", WORKFLOW, flags=re.M):
            self.assertIn(line + "\n", scan)
        self.assertEqual(len(re.findall(r"^  TRIVY_(?:VERSION|SHA256): ", WORKFLOW, flags=re.M)), 2)
        for name in ("Download and verify official Trivy release before extracting binary",
                     "Download fresh vulnerability database", "Scan actual local image including unfixed vulnerabilities"):
            step = "      - name: " + name + "\n"
            ours = shared.split(step, 1)[1].split("\n\n", 1)[0].replace('"$LOCAL_IMAGE"', '"security-scan:${SOURCE_SHA}"')
            self.assertIn(ours, scan, name)

    def test_push_follows_every_check_under_one_immutable_never_replaced_tag(self):
        order = ["docker build --pull", "smoke.sh", "tools/trivy image --config '' --ignorefile '' --disable-telemetry \\\n"
                 "            --skip-version-check --ignore-unfixed=false", "--expected-image-id", "docker login ghcr.io",
                 "stage.absent_tag(status, raw)", 'docker push "$PACKAGE:$tag"', "docker manifest inspect",
                 'test "$image_id" = "$config"', "digest/vm-export-tools-image.json", '>> "$GITHUB_STEP_SUMMARY"',
                 "name: vm-export-tools-digest-${{ github.sha }}", "docker logout ghcr.io"]
        positions = [PUBLISH.index(item) for item in order]
        self.assertEqual(positions, sorted(positions))
        self.assertIn('tag="$(sha256sum images/vm_export/tools/Containerfile | cut -c1-16)-$SOURCE_SHA"', PUBLISH)
        self.assertIn('[[ "$tag" =~ ^[0-9a-f]{16}-[0-9a-f]{40}$ ]]', PUBLISH)
        self.assertEqual(WORKFLOW.count("docker push"), 1)
        self.assertEqual(re.findall(r"docker tag .+", WORKFLOW), ['docker tag "$LOCAL_IMAGE" "$PACKAGE:$tag"'])
        self.assertIn("--password-stdin", PUBLISH)
        self.assertIn("      - name: Remove registry login\n        if: always()\n", PUBLISH)
        for item in (":latest", "--force", "docker buildx imagetools create", "MANUAL_TAG", "inputs."):
            self.assertNotIn(item, WORKFLOW)


if __name__ == "__main__":
    unittest.main(verbosity=2)
