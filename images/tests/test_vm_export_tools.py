"""Pure contract tests for the VM export tool image: its Containerfile, smoke script and workflow.

Text only: no docker, network, registry or GitHub Actions. Where a line added next to an intact one
would defeat a check (a second package source, a later flag, `|| :`), the test compares the whole
construct with the reviewed one instead of looking for substrings.
"""
from pathlib import Path
import re
import shlex
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
LOCK = (TOOLS / "packages.lock").read_text()
WORKFLOW = (WORKFLOWS / "vm-export-tools.yml").read_text()
CHECK, PUBLISH = WORKFLOW.split("\n  check:\n", 1)[1].split("\n  publish:\n", 1)
PINNED_USE = re.compile(r"uses: ([A-Za-z0-9_./-]+@[0-9a-f]{40}) # v\S+")
DOCKER = re.compile(r"\bdocker ((?:image |manifest )?[a-z]+)")
REQUESTED = ["qemu-utils", "python3", "python3-guestfs", "supermin", "linux-image-amd64", "e2fsprogs"]
CHECK_IF = "github.event_name == 'pull_request'"
PUBLISH_IF = ("github.event_name != 'pull_request' && github.repository == 'mutex-solutions/proxcloud'"
              " && github.ref == 'refs/heads/main'")


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

    def test_the_package_layer_is_exactly_the_reviewed_one(self):
        """One pinned snapshot, signed sources only, the six requested packages, no privileged file left."""
        self.assertEqual(len(self.run), 1)
        run, pinned = re.subn(r"; snapshot=20[0-9]{6}T[0-9]{6}Z; ", "; snapshot=PINNED; ", " ".join(self.run[0].split()))
        self.assertEqual(pinned, 1)
        source = 'echo "deb $options $archive/%s/$snapshot %s main"'
        self.assertEqual(run.split("; "), [
            "RUN set -eux", "snapshot=PINNED", "archive=http://snapshot.debian.org/archive",
            # A snapshot's Release file expires; the signature is still verified with the base keyring.
            "options='[signed-by=/usr/share/keyrings/debian-archive-keyring.gpg check-valid-until=no]'",
            "rm /etc/apt/sources.list.d/debian.sources",
            "{ " + source % ("debian", "bookworm"), source % ("debian", "bookworm-updates"),
            source % ("debian-security", "bookworm-security"), "} > /etc/apt/sources.list",
            "export DEBIAN_FRONTEND=noninteractive", "apt-get -o Acquire::Retries=5 update",
            "apt-get -o Acquire::Retries=5 upgrade -y",
            "apt-get -o Acquire::Retries=5 install -y --no-install-recommends " + " ".join(REQUESTED),
            "rm -rf /var/lib/apt/lists/*", "chmod -R a+rX /boot /lib/modules",
            "find / -xdev -type f -perm /6000 -exec chmod a-s {} +",
            "setcap -r /usr/lib/x86_64-linux-gnu/gstreamer1.0/gstreamer-1.0/gst-ptp-helper"])

    def test_lock_file_is_a_package_list_with_what_was_requested_and_no_sudo(self):
        rows = LOCK.splitlines()
        self.assertTrue(LOCK.endswith("\n") and len(rows) > 100)
        for row in rows:
            self.assertRegex(row, r"^[a-z0-9][a-z0-9.+-]*(:amd64)?\t[0-9A-Za-z.+:~-]+$")
        names = [row.split("\t")[0].split(":")[0] for row in rows]
        self.assertEqual(names, sorted(set(names)))
        self.assertLessEqual(set(REQUESTED), set(names))
        self.assertFalse({"sudo", "libguestfs-tools"} & set(names))

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
        head, body = SMOKE.split("    /usr/bin/python3 -I -c '\n", 1)
        script = [line.rstrip("\\") for line in head.splitlines() if line and not line.startswith("#")]
        self.assertEqual(script[:3], ["set -eu", 'image="${1:?usage: smoke.sh IMAGE}"', "uid=1002"])
        tmpfs = ":rw,exec,nosuid,nodev,size=%s,mode=0700,uid=$uid,gid=$uid"
        # The whole command, so a later flag cannot override an earlier one. The environment is the
        # exact one a capsule gets from `env -i`.
        self.assertEqual(shlex.split(" ".join(script[3:])), [
            "exec", "docker", "run", "--rm", "--pull=never", "--network=none", "--read-only", "--user=$uid:$uid",
            "--cap-drop=ALL", "--security-opt=no-new-privileges", "--pids-limit=256", "--memory=4g", "--shm-size=128m",
            "--tmpfs", "/tmp" + tmpfs % "1g", "--tmpfs", "/scratch" + tmpfs % "2g",
            "--entrypoint", "/usr/bin/env", "$image", "-i", *capsule.ENV])
        self.assertTrue(body.endswith("' \"$uid\"\n"))
        for item in ("pwd.getpwuid(uid)", "import guestfs", '"/usr/bin/qemu-img", "--version"',
                     'assert not mode & 0o6000 and "security.capability" not in os.listxattr(path), path',
                     'g.set_backend_setting("force_tcg", "true")', 'format="qcow2", readonly=True', "\ng.launch()\n",
                     '\nassert g.list_devices() == ["/dev/sda", "/dev/sdb"]\n', '\ng.e2fsck("/dev/sdb", correct=True)\n'):
            self.assertIn(item, body)
        # The passwd probe is the only place that may swallow an error or stop early.
        self.assertEqual([body.count(item) for item in ("try:", "except", "except KeyError:", "SystemExit", "exit")],
                         [1, 1, 1, 1, 0])


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
                     "workflow_run", "schedule:", "docker/login-action", "docker/build-push-action", "set +e",
                     "exit 0"):
            self.assertNotIn(item, WORKFLOW)
        # Expressions never reach a shell script: this is the complete list, all in if/env/with/concurrency.
        self.assertEqual(sorted(set(re.findall(r"\$\{\{ (.+?) \}\}", WORKFLOW))), [
            "github.event.pull_request.head.sha || github.sha", "github.event.pull_request.number || github.ref",
            "github.event_name == 'pull_request'", "github.sha", "github.token"])

    def test_no_step_is_skipped_or_allowed_to_fail(self):
        self.assertEqual(re.findall(r"^ +if: (.+)$", WORKFLOW, flags=re.M),
                         [CHECK_IF, "always()", PUBLISH_IF, "always()", "always()"])
        self.assertEqual(WORKFLOW.count("run: |\n          set -euo pipefail\n"), WORKFLOW.count("run: |"))
        smoke = 'images/vm_export/tools/smoke.sh "$LOCAL_IMAGE"'
        self.assertEqual(re.findall(r"run: (?!\|)(.+)", WORKFLOW), [smoke, smoke, "docker logout ghcr.io"])
        # Outside the whitelisted expressions, `||` appears once: the image ID is one of the two digests.
        shell = re.sub(r"\$\{\{ .+? \}\}", "", WORKFLOW)
        self.assertEqual([line.strip() for line in shell.splitlines() if "||" in line],
                         ['test "$image_id" = "$config" || test "$image_id" = "${reference#*@}"'])

    def test_pull_request_job_cannot_log_in_or_push(self):
        self.assertTrue(CHECK.startswith("    if: " + CHECK_IF + "\n"))
        self.assertEqual(DOCKER.findall(CHECK), ["build", "image inspect", "run"])
        for item in ("permissions:", "packages:", "docker login", "docker push", "docker tag", "github.token",
                     "GH_TOKEN", "$PACKAGE"):
            self.assertNotIn(item, CHECK)

    def test_publish_job_is_main_only_in_this_repository(self):
        self.assertTrue(PUBLISH.startswith("    if: " + PUBLISH_IF + "\n"))
        self.assertIn("  PACKAGE: ghcr.io/mutex-solutions/proxcloud-vm-export-tools\n", WORKFLOW)

    def test_both_jobs_build_smoke_scan_and_gate_identically(self):
        shared = CHECK.split("    steps:\n", 1)[1].split("\n  # Trusted runs only.", 1)[0]
        self.assertIn(shared, PUBLISH)
        for item in ("docker build --pull --platform linux/amd64 --provenance=false --load",
                     '--file images/vm_export/tools/Containerfile --build-arg "REVISION=$SOURCE_SHA"',
                     'run: images/vm_export/tools/smoke.sh "$LOCAL_IMAGE"',
                     "\n          diff -u images/vm_export/tools/packages.lock result/packages.txt\n",
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

    def test_push_follows_every_check_under_one_never_replaced_tag(self):
        order = ["docker build --pull", "smoke.sh", "tools/trivy image --config '' --ignorefile '' --disable-telemetry \\\n"
                 "            --skip-version-check --ignore-unfixed=false", "--expected-image-id", "docker login ghcr.io",
                 "stage.absent_tag(status, raw)", 'docker push "$PACKAGE:$tag"', "docker manifest inspect",
                 'test "$image_id" = "$config"', "digest/vm-export-tools-image.json", '>> "$GITHUB_STEP_SUMMARY"',
                 "name: vm-export-tools-digest-${{ github.sha }}", "docker logout ghcr.io"]
        positions = [PUBLISH.index(item) for item in order]
        self.assertEqual(positions, sorted(positions))
        self.assertIn('tag="$(sha256sum images/vm_export/tools/Containerfile | cut -c1-16)-$SOURCE_SHA"', PUBLISH)
        self.assertIn('[[ "$tag" =~ ^[0-9a-f]{16}-[0-9a-f]{40}$ ]]', PUBLISH)
        # Every docker command of the job, so `docker image tag` or a second push is not a way around.
        self.assertEqual(DOCKER.findall(PUBLISH), DOCKER.findall(CHECK) + [
            "login", "tag", "push", "image inspect", "manifest inspect", "logout"])
        self.assertIn('          PY\n          docker tag "$LOCAL_IMAGE" "$PACKAGE:$tag"\n          docker push "$PACKAGE:$tag"\n',
                      PUBLISH)
        self.assertIn('"image_id": "%s"', PUBLISH)
        self.assertIn("--password-stdin", PUBLISH)
        self.assertIn("      - name: Remove registry login\n        if: always()\n", PUBLISH)
        for item in (":latest", "--force", "docker buildx imagetools create", "MANUAL_TAG", "inputs."):
            self.assertNotIn(item, WORKFLOW)

    def test_tag_probe_is_the_reviewed_one_of_vm_artifacts(self):
        """Verbatim, so nothing can exit, catch or skip before `absent_tag`. Three known differences."""
        indent = " " * 10
        ours = PUBLISH.split(indent + "python3 - \"$tag\" <<'PY'\n", 1)[1].split(indent + "PY\n", 1)[0]
        first = indent + "class NoRedirect(urllib.request.HTTPRedirectHandler):\n"
        last = indent + "stage.absent_tag(status, raw)  # Existing tag, 401/403, timeout and other failures all stop.\n"
        probe = first + (WORKFLOWS / "vm-artifacts.yml").read_text().split(first, 1)[1].split(last, 1)[0] + last
        oci, docker = "application/vnd.oci.image.", "application/vnd.docker.distribution.manifest."
        accept = "'Accept': '%s'})" % (oci + "manifest.v1+json, " + docker + "v2+json")
        # This image could also be pushed as an index, so the probe asks for those media types too.
        wider = "'Accept': '%s'\n%s'%s'})" % (oci + "index.v1+json, " + oci + "manifest.v1+json, ", " " * 24,
                                             docker + "list.v2+json, " + docker + "v2+json")
        for old, new in (("plan['package']", "os.environ['PACKAGE']"), ("plan['reference'].split(':', 1)[1]", "sys.argv[1]"),
                         (accept, wider)):
            self.assertEqual(probe.count(old), 1)
            probe = probe.replace(old, new)
        self.assertEqual(ours, "".join(indent + line + "\n" for line in (
            "import base64, json, os, sys, urllib.error, urllib.parse, urllib.request", "from pathlib import Path",
            "sys.path.insert(0, str(Path(os.environ['GITHUB_WORKSPACE']) / 'images'))",
            "import vm_publish_stage as stage")) + probe)


if __name__ == "__main__":
    unittest.main(verbosity=2)
