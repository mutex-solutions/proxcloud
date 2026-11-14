"""Contract tests for .github/workflows/vm-artifact-export.yml: one dispatch from a template to a
published, attested and anonymously verified image.

Text only: no GitHub Actions, runner, docker, registry or PVE. Where an added line next to an intact one
would defeat a check, the test compares the whole list (steps, expressions, actions, permissions).
"""
from pathlib import Path
import re
import sys
import unittest

IMAGES = Path(__file__).resolve().parents[1]
ROOT = IMAGES.parent
sys.path.insert(0, str(IMAGES))
from vm_export import pipeline  # noqa: E402
import vm_publish_stage  # noqa: E402
import vm_sanitize  # noqa: E402

WORKFLOWS = ROOT / ".github" / "workflows"
TEXT = (WORKFLOWS / "vm-artifact-export.yml").read_text()
HEAD, BODY = TEXT.split("\njobs:\n", 1)
JOBS = dict(zip(re.findall(r"^  ([a-z]+):$", BODY, flags=re.M), re.split(r"^  [a-z]+:\n", BODY, flags=re.M)[1:]))
EXPORT, PUBLISH, ATTEST, VERIFY = (JOBS[name] for name in ("export", "publish", "attest", "verify"))
CHECKOUT = "actions/checkout@11bd71901bbe5b1630ceea73d27597364c9af683 # v4.2.2"
UPLOAD = "actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7.0.2"
ORAS = "oras-project/setup-oras@22ce207df3b08e061f537244349aac6ae1d214f6 # v1; official pin reviewed separately"
DOWNLOAD = "actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8.0.2"
ATTESTATION = "actions/attest-build-provenance@4d101475d8b20a2381f78447822ac1eab6504dd8 # v4.2.2"
SECRETS = ["PVE_URL", "PVE_TOKEN_ID", "PVE_TOKEN_SECRET", "PVE_NODES", "PVE_TEMPLATE_STORAGE", "PVE_IMPORT_STORAGE",
           "PVE_ISO_STORAGE", "PVE_BRIDGE", "BUILD_GATEWAY", "BUILD_NAMESERVER", "EXPORT_TEST_ADDRESSES"]
VARIABLES = ["vars.EXPORT_VLAN || '0'", "vars.EXPORT_SOURCE_MOUNT || '/srv/pc-ci-images'"]
LAB_ENV = ("".join("          %s: ${{ secrets.%s }}\n" % (key, key) for key in SECRETS)
           + "".join("          %s: ${{ %s }}\n" % (item.split(" ")[0][5:], item) for item in VARIABLES))
TOKEN_ENV = "".join("          %s: ${{ secrets.%s }}\n" % (key, key) for key in SECRETS[:3])
RECOVER = "        env:\n" + TOKEN_ENV + "        run: exec python3 -m vm_export.pipeline recover"
DISPATCHED = ("variant", "os", "kubernetes_version")
GUARD = "github.repository == 'mutex-solutions/proxcloud' && github.ref == 'refs/heads/main'"


def steps(job):
    """[(first line, whole text)] of a job's steps."""
    found = re.split(r"^      - ", job.split("    steps:\n", 1)[1], flags=re.M)[1:]
    return [(step.splitlines()[0], step) for step in found]


def permissions(job):
    block = re.search(r"^    permissions:\n((?:      [a-z-]+: [a-z]+\n)+)", job, flags=re.M)
    return dict(line.strip().split(": ") for line in block[1].splitlines())


class TriggerTests(unittest.TestCase):
    def test_manual_dispatch_on_main_is_the_only_way_in(self):
        triggers = HEAD.split("\non:\n", 1)[1].split("\npermissions:", 1)[0]
        self.assertEqual(re.findall(r"^  ([a-z_]+):$", triggers, flags=re.M), ["workflow_dispatch"])
        for event in ("pull_request", "pull_request_target", "push:", "schedule", "workflow_run", "workflow_call",
                      "release:", "repository_dispatch"):
            self.assertNotIn(event, triggers)
        self.assertTrue(HEAD.endswith("\npermissions: {}\n"))
        # No workflow-level env, defaults or concurrency: a top-level `env:` would hand a secret to every job.
        self.assertEqual(re.findall(r"^([A-Za-z_-]+):", TEXT, flags=re.M), ["name", "on", "permissions", "jobs"])
        self.assertEqual(list(JOBS), ["export", "publish", "attest", "verify"])
        self.assertTrue(EXPORT.startswith("    if: " + GUARD + "\n"))
        # Every later job hangs on the export job (and so on its guard) through `needs`.
        self.assertIn("    needs: export\n", PUBLISH)
        self.assertIn("    needs: [export, publish]\n", ATTEST)
        self.assertIn("    needs: [export, publish, attest]\n", VERIFY)
        self.assertEqual(re.findall(r"^    needs: (.+)$", BODY, flags=re.M), ["export", "[export, publish]", "[export, publish, attest]"])
        self.assertEqual(re.findall(r"^    if: (.+)$", BODY, flags=re.M), [GUARD])  # No job runs on failure or always().
        for item in ("continue-on-error", "|| true", "set +e", "pull_request", "matrix:", ":latest", "secrets: inherit",
                     "workflow_run"):
            self.assertNotIn(item, BODY)
        # The lab secrets live in ONE environment, limited to main by the owner; only the export job names it.
        self.assertEqual(re.findall(r"(?m)^ +environment: *(.*)$", BODY), ["vm-artifact-export"])
        self.assertIn("    environment: vm-artifact-export\n", EXPORT)

    def test_inputs_are_choices_equal_to_the_code_allowlists(self):
        inputs = HEAD.split("    inputs:\n", 1)[1].split("\npermissions:", 1)[0]
        self.assertEqual(re.findall(r"^      ([a-z_]+):$", inputs, flags=re.M), ["variant", "os", "kubernetes_version", "template_vmid"])
        options = dict(zip(("variant", "os", "kubernetes_version"), re.findall(r"^        options: \[(.*)\]$", inputs, flags=re.M)))
        self.assertEqual(options["variant"], "os, kubernetes")
        self.assertEqual(options["os"].split(", "), list(pipeline.OSES))
        # "none" is the form's empty choice (a choice option cannot be an empty string).
        self.assertEqual(options["kubernetes_version"],
                         ", ".join(["none"] + ['"%s"' % version for version in vm_sanitize.K8S_VERSIONS]))
        self.assertIn("        default: none\n", inputs)
        self.assertEqual(vm_sanitize.K8S_VERSIONS, vm_publish_stage.K8S_VERSIONS)
        self.assertEqual(inputs.count("type: choice"), 3)
        self.assertEqual(inputs.count("type: string"), 1)  # template_vmid: digits, validated by the plan stage.
        # Each input reaches the environment of the plan step; what was dispatched also reaches the
        # publisher and the verifier, which refuse a bundle that stands for anything else. Never a shell line.
        for name in DISPATCHED:
            self.assertEqual(TEXT.count("${{ inputs.%s }}" % name), 3)
            self.assertEqual(EXPORT.count("inputs." + name), 1)
        self.assertEqual(TEXT.count("inputs.template_vmid"), 1)
        plan = dict(steps(EXPORT))["name: Validate the dispatch inputs before any secret or API use"]
        self.assertEqual(plan.split("\n", 1)[1], (
            "        env:\n          EXPORT_VARIANT: ${{ inputs.variant }}\n          EXPORT_OS: ${{ inputs.os }}\n"
            "          EXPORT_KUBERNETES_VERSION: ${{ inputs.kubernetes_version }}\n"
            "          EXPORT_TEMPLATE_VMID: ${{ inputs.template_vmid }}\n"
            "        run: exec python3 -m vm_export.pipeline plan\n"))
        self.assertNotIn("inputs.", ATTEST)
        for job in (PUBLISH, VERIFY):
            self.assertEqual(re.findall(r"inputs\.([a-z_]+)", job), list(DISPATCHED))

    def test_every_expression_is_a_known_one_and_none_sits_in_a_shell_line(self):
        self.assertEqual(sorted(set(re.findall(r"\$\{\{ (.+?) \}\}", TEXT))), sorted(
            ["github.sha", "github.run_id", "github.run_attempt", "github.token", "github.repository", "github.workflow_ref",
             "github.workspace", "runner.temp", "inputs.variant", "inputs.os", "inputs.kubernetes_version",
             "inputs.template_vmid", *VARIABLES, *("secrets." + key for key in SECRETS),
             *("steps.bundle.outputs." + key for key in ("artifact_name", "manifest_sha256", "disk_sha256")),
             "steps.evidence.outputs.evidence_sha256",
             *("needs.export.outputs." + key for key in ("artifact_name", "manifest_sha256", "disk_sha256", "evidence_sha256")),
             "needs.publish.outputs.package", "needs.publish.outputs.digest"]))
        for name, job in JOBS.items():
            if name == "publish":
                continue
            for first, step in steps(job):
                script = step.split("        run: ", 1)[1] if "        run: " in step else ""
                self.assertNotIn("${{", script, first)

    def test_actions_are_the_reviewed_full_sha_pins(self):
        self.assertEqual(re.findall(r"uses: (.+)", TEXT), [
            CHECKOUT, UPLOAD, UPLOAD, "./.github/workflows/vm-artifacts.yml", ATTESTATION, ATTESTATION, CHECKOUT, DOWNLOAD,
            ORAS])
        # Checkout, upload, download and ORAS pins are the ones other workflows of this repository already use.
        elsewhere = "".join(path.read_text() for path in WORKFLOWS.glob("*.yml") if path.name != "vm-artifact-export.yml")
        for pin in (CHECKOUT, UPLOAD, DOWNLOAD, ORAS):
            self.assertIn("uses: " + pin, elsewhere)
        self.assertEqual(TEXT.count("version: 1.3.0"), 1)
        self.assertTrue((WORKFLOWS / "vm-artifacts.yml").is_file())


class PermissionTests(unittest.TestCase):
    def test_least_privilege_per_job(self):
        self.assertEqual(permissions(EXPORT), {"contents": "read"})
        self.assertEqual(permissions(PUBLISH), {"contents": "read", "actions": "read", "packages": "write"})
        self.assertEqual(permissions(ATTEST), {"id-token": "write", "attestations": "write", "packages": "write"})
        self.assertEqual(permissions(VERIFY), {"contents": "read"})
        self.assertEqual(TEXT.count("permissions:"), 5)
        for name in ("export", "publish", "verify"):
            for item in ("id-token", "attestations"):
                self.assertNotIn(item, JOBS[name], name)
        # The reusable publisher cannot hold more than the caller grants, and asks for exactly that.
        publisher = (WORKFLOWS / "vm-artifacts.yml").read_text()
        self.assertIn("    permissions:\n      contents: read\n      actions: read\n      packages: write\n", publisher)

    def test_export_job_holds_no_registry_credential(self):
        self.assertIn("    runs-on: [self-hosted, linux, x64, proxcloud-vm-artifact-export]\n", EXPORT)
        self.assertEqual(vm_publish_stage.EXPORT_LABELS, ["self-hosted", "linux", "x64", "proxcloud-vm-artifact-export"])
        self.assertEqual(vm_publish_stage.EXPORT_JOB, "export")
        self.assertNotRegex(BODY, r"(?m)^    name:")  # The publisher finds the job by its id, which is its name.
        for item in ("packages", "github.token", "GH_TOKEN", "GITHUB_TOKEN", "docker login", "oras", "ghcr.io", "password",
                     "curl ", "docker push", "docker tag"):
            self.assertNotIn(item, EXPORT, item)
        self.assertNotRegex(EXPORT, r"(?m)(?:^|[\s;|&(])gh\s")
        self.assertIn("          persist-credentials: false\n", EXPORT)
        # The tool image is pulled by the pinned digest with an empty Docker client configuration.
        source = (IMAGES / "vm_export" / "pipeline.py").read_text()
        self.assertIn('capsule.pull(self.work / "docker-anonymous")', source)

    def test_export_shares_the_template_builders_concurrency_group(self):
        group = "    concurrency:\n      group: vm-templates\n      cancel-in-progress: false\n"
        self.assertIn(group, EXPORT)
        for name in ("vm-templates.yml", "k8s-node-templates.yml"):
            self.assertIn("concurrency:\n  group: vm-templates\n  cancel-in-progress: false\n", (WORKFLOWS / name).read_text())
        self.assertEqual(TEXT.count("concurrency:"), 1)

    def test_anonymous_job_has_no_token_no_login_and_no_packages_permission(self):
        for item in ("github.token", "GH_TOKEN", "GITHUB_TOKEN", "secrets.", "login", "password", "packages", "id-token",
                     "Authorization", "--username", "docker ", "self-hosted"):
            self.assertNotIn(item, VERIFY, item)
        self.assertIn("    runs-on: ubuntu-24.04\n", VERIFY)
        found = steps(VERIFY)
        self.assertEqual([first for first, _ in found], [
            "uses: " + CHECKOUT, "uses: " + DOWNLOAD, "uses: " + ORAS,
            "name: Pull the digest anonymously, verify the three files and write the final summary"])
        # The helpers that judge the pull are the ones of the dispatched commit, not of a moving branch.
        self.assertEqual(found[0][1].split("      #", 1)[0], (
            "uses: " + CHECKOUT + "\n        with:\n          repository: mutex-solutions/proxcloud\n"
            "          ref: ${{ github.sha }}\n          persist-credentials: false\n          sparse-checkout: |\n"
            "            images/vm_artifact.py\n            images/vm_publish_stage.py\n"
            "          sparse-checkout-cone-mode: false\n"))
        # The evidence record: the artifact the export job uploaded next to the bundle, one JSON file.
        self.assertEqual(found[1][1], (
            "uses: " + DOWNLOAD + "\n        with:\n          name: ${{ needs.export.outputs.artifact_name }}-evidence\n"
            "          path: ${{ runner.temp }}/evidence\n"))
        script = dict(steps(VERIFY))["name: Pull the digest anonymously, verify the three files and write the final summary"]
        order = ['reference="$(python3 images/vm_publish_stage.py reference)"', "printf '{\"auths\":{}}\\n' > \"$anonymous/config.json\"",
                 'env -i PATH="$PATH" HOME="$anonymous/home" DOCKER_CONFIG="$anonymous" XDG_CONFIG_HOME="$anonymous" \\\n',
                 'oras pull --registry-config "$anonymous/config.json" --no-tty --output "$anonymous/pull" "$reference"',
                 "test \"$(<\"$anonymous/config.json\")\" = '{\"auths\":{}}'",
                 'python3 images/vm_artifact.py verify --staging-dir "$anonymous/pull" \\\n',
                 '--expected-manifest-sha256 "$EXPECTED_MANIFEST_SHA256" --expected-disk-sha256 "$EXPECTED_DISK_SHA256"',
                 'python3 images/vm_publish_stage.py pulled --staging-dir "$anonymous/pull" \\\n',
                 '--evidence "$RUNNER_TEMP/evidence/vm-export-evidence.json" >> "$GITHUB_STEP_SUMMARY"']
        positions = [script.index(item) for item in order]
        self.assertEqual(positions, sorted(positions))
        self.assertIn("          set -euo pipefail\n", script)
        # The hashes are the ones the export job recorded; the reference is the one the publisher pushed.
        for line in ("      ARTIFACT_NAME: ${{ needs.export.outputs.artifact_name }}\n",
                     "      EXPECTED_MANIFEST_SHA256: ${{ needs.export.outputs.manifest_sha256 }}\n",
                     "      EXPECTED_DISK_SHA256: ${{ needs.export.outputs.disk_sha256 }}\n",
                     "      PACKAGE: ${{ needs.publish.outputs.package }}\n", "      DIGEST: ${{ needs.publish.outputs.digest }}\n",
                     "      PUBLISHER_WORKFLOW_REF: ${{ github.workflow_ref }}\n", "      PUBLISHER_SHA: ${{ github.sha }}\n",
                     "      EXPORT_RUN_ID: ${{ github.run_id }}\n", "      EXPORT_RUN_ATTEMPT: ${{ github.run_attempt }}\n",
                     "      EVIDENCE_SHA256: ${{ needs.export.outputs.evidence_sha256 }}\n",
                     *("      EXPORT_%s: ${{ inputs.%s }}\n" % (name.upper(), name) for name in DISPATCHED)):
            self.assertIn(line, VERIFY)
        self.assertEqual(sorted(re.findall(r"^      ([A-Z_0-9]+): ", VERIFY, flags=re.M)),
                         sorted(vm_publish_stage.ENVIRONMENT + ("PACKAGE", "DIGEST", "EVIDENCE_SHA256")))
        self.assertEqual(vm_publish_stage.EVIDENCE, pipeline.EVIDENCE)


class ExportJobTests(unittest.TestCase):
    def setUp(self):
        self.steps = steps(EXPORT)
        self.names = [first for first, _ in self.steps]

    def test_steps_are_exactly_the_reviewed_ones_in_order(self):
        self.assertEqual(self.names, [
            "uses: " + CHECKOUT, "name: Require full source checkout",
            "name: Validate the dispatch inputs before any secret or API use",
            "name: Validate the runner configuration without any API call",
            "name: Guarded cleanup of what an earlier run left on the lab",
            "name: Pull the pinned tool image by digest (no stored registry credential)",
            "name: Template guard and read-only copy from the storage mount", "name: Inventory in a read-only capsule",
            "name: Review against the committed baseline (kubernetes) or the eligibility gate (os)",
            "name: Sanitize offline and seal the disk", "name: Postscan the sealed disk in an independent capsule",
            "name: Cold boot two clones through the PVE API", "name: Bundle the three files and re-check them as the publisher will",
            "name: Build the public evidence record", "name: Upload the three-file bundle",
            "name: Upload the public evidence record", "name: Guarded cold-boot cleanup from the state file",
            "name: Delete the private copy, the scratch and every work file"])

    def test_driver_stages_run_in_the_drivers_own_order_one_per_step(self):
        called = re.findall(r"python3 -m vm_export\.pipeline ([a-z]+)", EXPORT)
        # `recover` first runs once the configuration is known and before anything touches the lab.
        self.assertEqual(called, [*pipeline.STAGES[:2], "recover", *pipeline.STAGES[2:], "recover", "purge"])
        self.assertEqual(sorted(pipeline.COMMANDS), ["purge", "recover"])
        self.assertEqual(re.findall(r"^        run: (?!\|)(.+)$", EXPORT, flags=re.M),
                         ["exec python3 -m vm_export.pipeline " + name for name in called])
        # No other python, no inline shell logic: the one `run: |` block is the checkout check.
        self.assertEqual(EXPORT.count("run: |"), 1)
        self.assertEqual(EXPORT.count("python3 "), len(called))
        self.assertEqual(EXPORT.count("        run: |\n          set -euo pipefail\n"), 1)
        self.assertIn("    defaults:\n      run:\n        shell: bash\n        working-directory: images\n", EXPORT)

    def test_checkout_is_the_exact_commit_in_full_without_credentials(self):
        self.assertEqual(self.steps[0][1], "uses: " + CHECKOUT + "\n        with:\n          repository: mutex-solutions/proxcloud\n"
                         "          ref: ${{ github.sha }}\n          persist-credentials: false\n")
        self.assertEqual(self.steps[1][1], (
            "name: Require full source checkout\n        working-directory: ${{ github.workspace }}\n        run: |\n"
            "          set -euo pipefail\n"
            '          git -C "$GITHUB_WORKSPACE" sparse-checkout disable\n'
            '          test "$(git -C "$GITHUB_WORKSPACE" config --bool --get core.sparseCheckout)" = false\n'
            '          test "$(git -C "$GITHUB_WORKSPACE" rev-parse HEAD)" = "$GITHUB_SHA"\n'
            "          # The runner keeps its workspace between jobs. Files an older commit or a sparse checkout left\n"
            "          # behind are removed, and the tree must then be exactly the commit.\n"
            '          git -C "$GITHUB_WORKSPACE" clean -ffdxq\n'
            '          test -z "$(git -C "$GITHUB_WORKSPACE" status --porcelain --untracked-files=all --ignored)"\n'
            '          test -f "$GITHUB_WORKSPACE/images/vm_export/pipeline.py" && test -f "$GITHUB_WORKSPACE/images/vm_sanitize.py"\n\n'
            "      # Each stage refuses unless exactly the earlier ones completed (images/vm_export/pipeline.py).\n"))

    def test_secrets_reach_only_the_steps_that_need_the_lab(self):
        with_secrets = [first for first, step in self.steps if "secrets." in step or "vars." in step]
        self.assertEqual(with_secrets, [
            "name: Validate the runner configuration without any API call",
            "name: Guarded cleanup of what an earlier run left on the lab",
            "name: Template guard and read-only copy from the storage mount", "name: Cold boot two clones through the PVE API",
            "name: Guarded cold-boot cleanup from the state file"])
        by_name = dict(self.steps)
        for first, stage in zip((with_secrets[0], *with_secrets[2:4]), ("config", "copy", "coldboot")):
            self.assertEqual(by_name[first].split("\n", 1)[1].rstrip("\n"),
                             "        env:\n" + LAB_ENV + "        run: exec python3 -m vm_export.pipeline " + stage)
        self.assertEqual(by_name[with_secrets[1]].split("\n", 1)[1].rstrip("\n"), RECOVER)
        self.assertEqual(sorted(SECRETS), sorted(pipeline.REQUIRED + pipeline.OPTIONAL))
        # Nothing before the plan step can see a secret, and the secrets are step-scoped, never job-wide.
        self.assertNotIn("secrets.", EXPORT.split("    steps:\n", 1)[0])
        self.assertNotIn("secrets.", "".join(step for _, step in self.steps[:3]))
        self.assertNotRegex(EXPORT.split("    steps:\n", 1)[0], r"(?m)^    env:")
        self.assertEqual(sorted(set(re.findall(r"secrets\.([A-Z_]+)", TEXT))), sorted(SECRETS))
        # The VLAN is a short number: as a secret it would be masked, and GitHub drops a job output that contains
        # a masked value (the hashes and the name are full of digits). It is a variable, never a secret, here.
        self.assertEqual(re.findall(r"vars\.([A-Z_]+)", TEXT), ["EXPORT_VLAN", "EXPORT_SOURCE_MOUNT"] * 3)
        self.assertNotIn("PVE_BUILD_VLAN", TEXT)

    def test_cleanup_and_purge_always_run_last(self):
        self.assertEqual(self.steps[-2][1], (
            "name: Guarded cold-boot cleanup from the state file\n        if: always()\n" + RECOVER + "\n"))
        self.assertEqual(self.steps[-1][1].split("\n\n", 1)[0], (
            "name: Delete the private copy, the scratch and every work file\n        if: always()\n"
            "        run: exec python3 -m vm_export.pipeline purge"))
        self.assertEqual(re.findall(r"^        if: (.+)$", EXPORT, flags=re.M), ["always()", "always()"])
        # The work directory the steps name is the driver's; the cold-boot state is NOT under the runner's
        # temp directory (the runner empties it after every job).
        source = (IMAGES / "vm_export" / "pipeline.py").read_text()
        self.assertIn('return plain_dir(env, "RUNNER_TEMP", "runner_temp_refused") / "vm-export"', source)
        self.assertIn('return plain_dir(env, "HOME", "runner_home_refused") / STATE_ROOT', source)

    def test_bundle_artifact_is_the_three_files_under_the_name_the_publisher_expects(self):
        by_name = dict(self.steps)
        self.assertEqual(by_name["name: Upload the three-file bundle"], (
            "name: Upload the three-file bundle\n        uses: " + UPLOAD + "\n        with:\n"
            "          name: ${{ steps.bundle.outputs.artifact_name }}\n          path: |\n"
            "            ${{ runner.temp }}/vm-export/bundle/disk.qcow2\n"
            "            ${{ runner.temp }}/vm-export/bundle/manifest.json\n"
            "            ${{ runner.temp }}/vm-export/bundle/SHA256SUMS\n"
            "          compression-level: 0\n          if-no-files-found: error\n          retention-days: 1\n"))
        self.assertEqual(sorted(vm_publish_stage.MEMBERS), ["SHA256SUMS", "disk.qcow2", "manifest.json"])
        bundle = by_name["name: Bundle the three files and re-check them as the publisher will"]
        self.assertEqual(bundle, "name: Bundle the three files and re-check them as the publisher will\n        id: bundle\n"
                                 "        run: exec python3 -m vm_export.pipeline bundle\n")
        self.assertIn("    outputs:\n      artifact_name: ${{ steps.bundle.outputs.artifact_name }}\n"
                      "      manifest_sha256: ${{ steps.bundle.outputs.manifest_sha256 }}\n"
                      "      disk_sha256: ${{ steps.bundle.outputs.disk_sha256 }}\n"
                      "      evidence_sha256: ${{ steps.evidence.outputs.evidence_sha256 }}\n    steps:\n", EXPORT)

    def test_public_evidence_is_a_separate_artifact_of_one_json(self):
        evidence = dict(self.steps)["name: Upload the public evidence record"]
        self.assertEqual(evidence.split("\n\n", 1)[0], (
            "name: Upload the public evidence record\n        uses: " + UPLOAD + "\n        with:\n"
            "          name: ${{ steps.bundle.outputs.artifact_name }}-evidence\n"
            "          path: ${{ runner.temp }}/vm-export/evidence/vm-export-evidence.json\n"
            "          if-no-files-found: error\n          retention-days: 90"))
        self.assertEqual(pipeline.EVIDENCE, "vm-export-evidence.json")
        self.assertEqual(dict(self.steps)["name: Build the public evidence record"], (
            "name: Build the public evidence record\n        id: evidence\n"
            "        run: exec python3 -m vm_export.pipeline evidence\n\n"))
        # The evidence artifact can never be taken for a bundle: the publisher's selectors refuse its name
        # (and the publisher lists the run's artifacts by the exact bundle name).
        for name in ("vm-k8s-export-ubuntu-2604-20261114120000-cccccccccccc", "vm-export-ubuntu-2404-20261114120000-cccccccccccc"):
            vm_publish_stage.selector(name)
            with self.assertRaises(ValueError):
                vm_publish_stage.selector(name + "-evidence")
        self.assertEqual(EXPORT.count("uses: " + UPLOAD), 2)


class HostedJobTests(unittest.TestCase):
    def test_publish_is_the_reusable_publisher_fed_only_by_the_export_job(self):
        self.assertEqual(PUBLISH.split("\n\n", 1)[0], (
            "    needs: export\n    permissions:\n      contents: read\n      actions: read\n      packages: write\n"
            "    uses: ./.github/workflows/vm-artifacts.yml\n    with:\n"
            "      artifact_name: ${{ needs.export.outputs.artifact_name }}\n"
            "      expected_manifest_sha256: ${{ needs.export.outputs.manifest_sha256 }}\n"
            "      expected_disk_sha256: ${{ needs.export.outputs.disk_sha256 }}\n"
            "      # What was dispatched: the publisher refuses a bundle that stands for anything else.\n"
            "      variant: ${{ inputs.variant }}\n      os: ${{ inputs.os }}\n"
            "      kubernetes_version: ${{ inputs.kubernetes_version }}\n"
            "      # Not used by the publisher: it must only have arrived, so that nothing is pushed which `attest` and\n"
            "      # `verify` could not finish (GitHub drops a job output that contains a masked value).\n"
            "      evidence_sha256: ${{ needs.export.outputs.evidence_sha256 }}"))
        # Every output of the export job reaches the publisher, which refuses before it pushes if one is empty.
        outputs = re.findall(r"^      ([a-z0-9_]+): \$\{\{ steps\.", EXPORT, flags=re.M)
        self.assertEqual(outputs, ["artifact_name", "manifest_sha256", "disk_sha256", "evidence_sha256"])
        self.assertEqual(re.findall(r"needs\.export\.outputs\.([a-z0-9_]+)", PUBLISH), outputs)
        publisher = (WORKFLOWS / "vm-artifacts.yml").read_text()
        self.assertIn("for key in ('ARTIFACT_NAME', 'EXPECTED_MANIFEST_SHA256', 'EXPECTED_DISK_SHA256', 'EVIDENCE_SHA256')):\n"
                      "              raise SystemExit(", publisher)
        self.assertEqual(vm_publish_stage.WORKFLOW_REF,
                         "mutex-solutions/proxcloud/.github/workflows/vm-artifact-export.yml@refs/heads/main")

    def test_attestation_is_for_the_pushed_digest_and_is_the_only_job_that_can_sign(self):
        names = [first for first, _ in steps(ATTEST)]
        verification = "name: Verify the attestation exactly as the final summary tells anyone to"
        self.assertEqual(names, ["name: Validate the published reference", "name: Authenticate for the attestation only",
                                 "uses: " + ATTESTATION, verification, "uses: " + ATTESTATION, "name: Remove registry login"])
        found = steps(ATTEST)
        by_name = dict(found)
        self.assertEqual(found[2][1], (
            "uses: " + ATTESTATION + "\n        with:\n          subject-name: ${{ needs.publish.outputs.package }}\n"
            "          subject-digest: ${{ needs.publish.outputs.digest }}\n          push-to-registry: true\n"))
        # The run fails unless the command it prints verifies: this workflow, on main, and no other signer.
        self.assertEqual(by_name[verification], (
            verification + "\n        env:\n          GH_TOKEN: ${{ github.token }}\n        shell: bash\n        run: |\n"
            "          set -euo pipefail\n"
            '          gh attestation verify "oci://$PACKAGE@$DIGEST" ' + vm_publish_stage.VERIFY_FLAGS + " > /dev/null\n"
            "      # The evidence record is an Actions artifact with a limited life: its sha256 is attested, so a kept copy\n"
            "      # stays verifiable (gh attestation verify vm-export-evidence.json ...) after the artifact expired.\n"))
        self.assertEqual(vm_publish_stage.VERIFY_FLAGS, (
            "--repo mutex-solutions/proxcloud --signer-workflow mutex-solutions/proxcloud/.github/workflows/"
            "vm-artifact-export.yml --source-ref refs/heads/main"))
        self.assertEqual(found[4][1], (
            "uses: " + ATTESTATION + "\n        with:\n          subject-name: vm-export-evidence.json\n"
            "          subject-digest: sha256:${{ needs.export.outputs.evidence_sha256 }}\n"))
        check = by_name["name: Validate the published reference"]
        self.assertIn('[[ "$PACKAGE" =~ ^ghcr\\.io/mutex-solutions/proxcloud-vm-ubuntu-2[46]04(-k8s)?$ ]]\n', check)
        self.assertIn('[[ "$DIGEST" =~ ^sha256:[0-9a-f]{64}$ ]]\n', check)
        self.assertIn('[[ "$EVIDENCE_SHA256" =~ ^[0-9a-f]{64}$ ]]\n', check)
        self.assertIn("          set -euo pipefail\n", check)
        self.assertIn("    env:\n      PACKAGE: ${{ needs.publish.outputs.package }}\n      DIGEST: ${{ needs.publish.outputs.digest }}\n"
                      "      EVIDENCE_SHA256: ${{ needs.export.outputs.evidence_sha256 }}\n    steps:\n", ATTEST)
        self.assertIn("--password-stdin", by_name["name: Authenticate for the attestation only"])
        self.assertEqual(by_name["name: Remove registry login"].split("\n\n", 1)[0],
                         "name: Remove registry login\n        if: always()\n        shell: bash\n        run: docker logout ghcr.io")
        self.assertIn("    runs-on: ubuntu-24.04\n", ATTEST)
        for item in ("self-hosted", "secrets.", "actions/checkout", "contents:"):
            self.assertNotIn(item, ATTEST, item)
        self.assertEqual(TEXT.count("attest-build-provenance"), 2)

    def test_hosted_jobs_never_touch_the_lab(self):
        for name in ("attest", "verify"):
            for item in ("self-hosted", "secrets.", "PVE_", "proxcloud-pve", "qemu", "vm_export"):
                self.assertNotIn(item, JOBS[name], (name, item))
        self.assertEqual(BODY.count("self-hosted"), 1)


if __name__ == "__main__":
    unittest.main(verbosity=2)
