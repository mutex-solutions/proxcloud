#!/usr/bin/env python3
"""The proxcloud-clusterctl image (deploy/clusterctl): entrypoint, pins, source build and workflow. Standard library plus PyYAML.

  python3 deploy/tests/test_clusterctl_image.py

No cluster, registry or Docker daemon: the entrypoint runs with /bin/sh against a fake kubectl (which answers the
clusterctl inventory) and a fake clusterctl (which records how it was called). `shellcheck` reads the entrypoint
when installed, and is required when CI is set.
"""
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import unittest

import yaml

ROOT = Path(__file__).resolve().parents[2]
IMAGE = ROOT / "deploy/clusterctl"
ENTRYPOINT = IMAGE / "clusterctl-init.sh"
DOCKERFILE = (IMAGE / "Dockerfile").read_text()
WORKFLOW = (ROOT / ".github/workflows/clusterctl.yml").read_text()
VERSIONS = yaml.safe_load((ROOT / "installer/playbooks/group_vars/all/versions.yml").read_text())["pc_versions"]
DEFAULTS = yaml.safe_load((ROOT / "installer/playbooks/group_vars/all/defaults.yml").read_text())["pc_defaults"]

# The workload's provider set (installer/playbooks/files/services-platform.sh) at the installer's versions; the
# seed's differs only in its kubeadm control plane and in having no add-on. The entrypoint cases below use fixed
# versions of their own: they test the comparison, not the pins.
REAL = ["--core", "cluster-api:" + VERSIONS["capi"], "--bootstrap", "kubeadm:" + VERSIONS["capi"],
        "--control-plane", "kamaji:" + VERSIONS["capi_kamaji"], "--infrastructure", "proxmox:" + VERSIONS["capmox"],
        "--ipam", "in-cluster:" + VERSIONS["capi_ipam"], "--addon", "helm:" + VERSIONS["caaph"]]
WANTED = ["--core", "cluster-api:v1.14.4", "--bootstrap", "kubeadm:v1.14.4", "--control-plane", "kamaji:v0.22.0",
          "--infrastructure", "proxmox:v0.9.2", "--ipam", "in-cluster:v1.1.1", "--addon", "helm:v0.6.4"]
INVENTORY = {"CoreProvider cluster-api", "BootstrapProvider kubeadm", "ControlPlaneProvider kamaji",
             "InfrastructureProvider proxmox", "IPAMProvider in-cluster", "AddonProvider helm"}

# FAKE_CRD empty: no inventory CRD. FAKE_INVENTORY: the "type name version" lines kubectl's jsonpath prints.
FAKE_KUBECTL = r"""#!/bin/sh
echo "kubectl $*" >> "$FAKE_LOG"
[ -z "${FAKE_KUBECTL_FAIL-}" ] || { echo "The connection to the server was refused" >&2; exit 1; }
case "$*" in
  "get crd providers.clusterctl.cluster.x-k8s.io --ignore-not-found -o name")
    [ -z "${FAKE_CRD-}" ] || echo customresourcedefinition.apiextensions.k8s.io/providers.clusterctl.cluster.x-k8s.io ;;
  "get providers.clusterctl.cluster.x-k8s.io -A -o jsonpath="*) printf '%s' "${FAKE_INVENTORY-}" ;;
  *) echo "unexpected kubectl call: $*" >&2; exit 3 ;;
esac
"""
FAKE_CLUSTERCTL = r"""#!/bin/sh
echo "clusterctl $* | PROXMOX_URL=[$PROXMOX_URL] PROXMOX_TOKEN=[$PROXMOX_TOKEN] PROXMOX_SECRET=[$PROXMOX_SECRET]" >> "$FAKE_LOG"
"""


def inventory(*rows):
    return "".join(row + "\n" for row in rows)


class EntrypointTests(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory(prefix="pc-clusterctl-test-")
        self.addCleanup(tmp.cleanup)
        self.bin = Path(tmp.name)
        self.log = self.bin / "calls.log"
        for name, body in (("kubectl", FAKE_KUBECTL), ("clusterctl", FAKE_CLUSTERCTL)):
            (self.bin / name).write_text(body)
            (self.bin / name).chmod(0o755)

    def run_entrypoint(self, *args, crd=True, rows="", **env):
        environ = {"PATH": "%s:/usr/bin:/bin" % self.bin, "FAKE_LOG": str(self.log),
                   "CLUSTERCTL_INIT_CONFIG": str(IMAGE / "clusterctl.yaml"),
                   "FAKE_CRD": "yes" if crd else "", "FAKE_INVENTORY": rows,
                   # A caller-set credential must never reach the provider manifests.
                   "PROXMOX_URL": "https://pve.example:8006", "PROXMOX_TOKEN": "t", "PROXMOX_SECRET": "s", **env}
        return subprocess.run(["/bin/sh", str(ENTRYPOINT), *args], env=environ, capture_output=True, text=True,
                              timeout=30)

    def clusterctl_calls(self):
        calls = self.log.read_text().splitlines() if self.log.exists() else []
        return [call for call in calls if call.startswith("clusterctl ")]

    def init(self, *flags):
        return ("clusterctl init --config %s " % (IMAGE / "clusterctl.yaml") + " ".join(flags) + " --wait-providers"
                " | PROXMOX_URL=[] PROXMOX_TOKEN=[] PROXMOX_SECRET=[]")

    def test_empty_cluster_installs_every_provider_once(self):
        result = self.run_entrypoint(*WANTED, crd=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.clusterctl_calls(), [self.init(*WANTED)])
        # Without the inventory CRD there is nothing to list.
        self.assertNotIn("kubectl get providers", self.log.read_text())

    def test_partial_cluster_installs_only_what_is_missing(self):
        rows = inventory("CoreProvider cluster-api v1.14.4", "BootstrapProvider kubeadm v1.14.4",
                         "ControlPlaneProvider kubeadm v1.14.4", "InfrastructureProvider proxmox v0.9.2")
        result = self.run_entrypoint(*WANTED, rows=rows)
        self.assertEqual(result.returncode, 0, result.stderr)
        # An installed kubeadm control plane is not the kamaji one: kamaji is still missing.
        self.assertEqual(self.clusterctl_calls(), [self.init(
            "--control-plane", "kamaji:v0.22.0", "--ipam", "in-cluster:v1.1.1", "--addon", "helm:v0.6.4")])

    def test_equal_versions_are_a_no_op(self):
        rows = inventory("CoreProvider cluster-api v1.14.4", "BootstrapProvider kubeadm v1.14.4",
                         "ControlPlaneProvider kamaji v0.22.0", "InfrastructureProvider proxmox v0.9.2",
                         "IPAMProvider in-cluster v1.1.1", "AddonProvider helm v0.6.4")
        result = self.run_entrypoint(*WANTED, rows=rows)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("nothing to do", result.stdout)
        self.assertEqual(self.clusterctl_calls(), [])

    def test_a_different_version_is_refused_and_nothing_is_installed(self):
        # One provider at another version, one missing: the refusal wins, the missing one is not installed either.
        rows = inventory("CoreProvider cluster-api v1.14.4", "BootstrapProvider kubeadm v1.14.4",
                         "ControlPlaneProvider kamaji v0.22.0", "InfrastructureProvider proxmox v0.9.1",
                         "IPAMProvider in-cluster v1.1.1")
        result = self.run_entrypoint(*WANTED, rows=rows)
        self.assertEqual(result.returncode, 1)
        self.assertIn("refusing to change Cluster API provider versions", result.stderr)
        self.assertIn("InfrastructureProvider proxmox: installed v0.9.1, wanted v0.9.2", result.stderr)
        self.assertNotIn("AddonProvider", result.stderr)
        self.assertEqual(self.clusterctl_calls(), [])

    def test_two_installed_instances_are_refused(self):
        rows = inventory("CoreProvider cluster-api v1.14.4", "CoreProvider cluster-api v1.14.4")
        result = self.run_entrypoint("--core", "cluster-api:v1.14.4", rows=rows)
        self.assertEqual(result.returncode, 1)
        self.assertIn("installed v1.14.4 v1.14.4, wanted v1.14.4", result.stderr)
        self.assertEqual(self.clusterctl_calls(), [])

    def test_an_unreachable_cluster_stops_before_clusterctl(self):
        result = self.run_entrypoint(*WANTED, FAKE_KUBECTL_FAIL="1")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.clusterctl_calls(), [])

    def test_malformed_arguments_are_refused(self):
        for args in ([], ["--core"], ["--runtime-extension", "x:v1.0.0"], ["--core", "cluster-api"],
                     ["--core", "cluster-api:latest"], ["--core", "cluster-api:v1.14"], ["--core", "Cluster API:v1.0.0"],
                     ["--core", "cluster-api:v1.14.4 --addon x:v1.0.0"], ["--core", "cluster-api:v1.14.4\n--addon"],
                     ["--core", "*:v1.0.0"]):
            with self.subTest(args=args):
                result = self.run_entrypoint(*args, crd=False)
                self.assertEqual(result.returncode, 2)
                self.assertIn("usage: clusterctl-init", result.stderr)
                self.assertEqual(self.clusterctl_calls(), [])

    def test_a_provider_the_image_does_not_carry_is_refused(self):
        # clusterctl adds its built-in providers (docker, metal3, ...) to clusterctl.yaml's: an unbaked name would
        # install from GitHub. A baked name at another version, or under another type, is refused the same way.
        for args in (["--infrastructure", "docker:v1.14.4"], ["--ipam", "metal3:v1.10.0"],
                     ["--infrastructure", "proxmox:v0.9.3"], ["--core", "kubeadm:v1.14.4"]):
            with self.subTest(args=args):
                result = self.run_entrypoint("--core", "cluster-api:v1.14.4", *args, crd=False)
                self.assertEqual(result.returncode, 2)
                self.assertIn("%s %s is not baked into this image" % tuple(args[1].split(":")), result.stderr)
                self.assertEqual(self.clusterctl_calls(), [])
        # The seed's kubeadm control plane is baked.
        result = self.run_entrypoint("--control-plane", "kubeadm:v1.14.4", crd=False)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_script_is_posix_and_shellcheck_clean(self):
        self.assertTrue(os.access(ENTRYPOINT, os.X_OK))
        self.assertEqual(subprocess.run(["/bin/sh", "-n", str(ENTRYPOINT)], capture_output=True).returncode, 0)
        if not shutil.which("shellcheck"):
            if os.environ.get("CI"):
                self.fail("shellcheck is required in CI")
            self.skipTest("shellcheck not installed")
        result = subprocess.run(["shellcheck", "--shell=sh", str(ENTRYPOINT)], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stdout)


def adds():
    """(sha256, url, destination) of every ADD of the Dockerfile."""
    return re.findall(r"^ADD --checksum=sha256:([0-9a-f]{64}) \\\n    (https://\S+) (\S+)$", DOCKERFILE, flags=re.M)


class PinTests(unittest.TestCase):
    def test_every_download_is_https_and_checksummed(self):
        self.assertEqual(len(adds()), 16)
        self.assertEqual(len(re.findall(r"^ADD ", DOCKERFILE, flags=re.M)), 16)
        self.assertNotIn("curl", DOCKERFILE)
        self.assertNotIn("wget", DOCKERFILE)
        froms = re.findall(r"^FROM (.+)$", DOCKERFILE, flags=re.M)
        self.assertEqual(len(froms), 5)
        self.assertRegex(froms[0], r"^golang@sha256:[0-9a-f]{64} AS go$")
        self.assertEqual(froms[1:3], ["go AS clusterctl", "go AS kubectl"])
        self.assertRegex(froms[3], r"^alpine@sha256:[0-9a-f]{64} AS fetch$")
        self.assertRegex(froms[4], r"^alpine@sha256:[0-9a-f]{64}$")

    def test_binaries_are_built_from_the_pinned_source_with_only_the_module_patch(self):
        # No step may rewrite go.mod or go.sum, fetch another toolchain or skip the checksum database: the modules
        # that go into the binaries are the ones of upstream's go.sum plus the lines of the reviewed patch.
        env = DOCKERFILE.split("\nENV CGO_ENABLED=0 ", 1)[1].split("\n\n", 1)[0].replace("\\\n", " ").split()
        for item in ("GOTOOLCHAIN=local", "GOWORK=off", 'GOFLAGS="-mod=readonly', '-trimpath"', "GONOSUMDB=",
                     "GONOPROXY=", "GOPRIVATE=", "GOINSECURE=", "GOPROXY=https://proxy.golang.org",
                     "GOSUMDB=sum.golang.org"):
            self.assertIn(item, env)
        for item in ("go get", "go mod", "-mod=mod", "-mod=vendor", "GOFLAGS=-", "apk add", "apt-get"):
            self.assertNotIn(item, DOCKERFILE)
        wanted = dict(re.findall(r"\b(X_NET|X_TEXT)=(v[0-9.]+)", DOCKERFILE))
        self.assertEqual(sorted(wanted), ["X_NET", "X_TEXT"])
        # The GitHub archive has one top-level directory, kubernetes-src.tar.gz has none.
        for name, version, strip in (("cluster-api", VERSIONS["capi"], " --strip-components=1"),
                                     ("kubernetes", "v" + DEFAULTS["services"]["k8s_version"], "")):
            with self.subTest(name=name):
                patch = "%s-%s.patch" % (name, version)
                self.assertIn("\nCOPY patches/%s /src/\n" % patch, DOCKERFILE)
                self.assertIn("tar -xzf ../%s.tar.gz%s; git apply ../%s; \\\n" % (name, strip, patch), DOCKERFILE)
                text = (IMAGE / "patches" / patch).read_text()
                self.assertEqual(re.findall(r"^(?:---|\+\+\+) (\S+)$", text, flags=re.M),
                                 ["a/go.mod", "b/go.mod", "a/go.sum", "b/go.sum"])
                for module, version in (("net", wanted["X_NET"]), ("text", wanted["X_TEXT"])):
                    self.assertRegex(text, r"(?m)^\+\tgolang\.org/x/%s %s( // indirect)?$" % (module, re.escape(version)))
                    for suffix in ("", "/go.mod"):
                        self.assertRegex(text, r"(?m)^\+golang\.org/x/%s %s%s h1:[A-Za-z0-9+/]{43}=$"
                                         % (module, re.escape(version), suffix))
                # Module requirements and the go directive only: no replace, no other module family.
                changed = re.findall(r"^[+-](?![+-])(.*)$", text.split("--- a/go.sum")[0], flags=re.M)
                for line in changed:
                    self.assertRegex(line, r"^(\tgolang\.org/x/[a-z]+ v[0-9.]+( // indirect)?|go 1\.[0-9]+\.[0-9]+)$")
        self.assertEqual(sorted(path.name for path in (IMAGE / "patches").iterdir()),
                         ["cluster-api-%s.patch" % VERSIONS["capi"],
                          "kubernetes-v%s.patch" % DEFAULTS["services"]["k8s_version"]])
        # The build itself reads the module versions back from each binary.
        for binary in ("clusterctl", "kubectl"):
            for module, variable in (("net", "X_NET"), ("text", "X_TEXT")):
                self.assertIn('grep -q "golang.org/x/%s[[:space:]]$%s[[:space:]]" /out/%s.modules'
                              % (module, variable, binary), DOCKERFILE)
            self.assertIn("\nCOPY --from=%s /out/%s /out/usr/local/bin/%s\n" % (binary, binary, binary), DOCKERFILE)

    def test_versions_equal_the_installer(self):
        downloads = {destination: url for _, url, destination in adds()}
        kubectl = "v" + DEFAULTS["services"]["k8s_version"]
        self.assertEqual(downloads["/src/cluster-api.tar.gz"],
                         "https://github.com/kubernetes-sigs/cluster-api/archive/refs/tags/%s.tar.gz" % VERSIONS["capi"])
        self.assertEqual(downloads["/src/kubernetes.tar.gz"], "https://dl.k8s.io/%s/kubernetes-src.tar.gz" % kubectl)
        # The binaries report the upstream versions, and the offline build check compares them. Commit and build date
        # are not derivable here: a version change updates them by hand (see the Dockerfile header).
        self.assertEqual(re.findall(r"-X \$v\.gitVersion=(\S+)", DOCKERFILE), [VERSIONS["capi"], kubectl])
        self.assertEqual(re.findall(r"-X \$v\.gitMajor=(\S+) -X \$v\.gitMinor=(\S+)", DOCKERFILE),
                         [tuple(version[1:].split(".")[:2]) for version in (VERSIONS["capi"], kubectl)])
        self.assertIn('test "$(clusterctl version -o short)" = %s; ' % VERSIONS["capi"], DOCKERFILE)
        self.assertIn('kubectl version --client | grep -Fx "Client Version: %s"; ' % kubectl, DOCKERFILE)
        repository = sorted(destination.split("/")[5] + "/" + destination.split("/")[6]
                            for destination in downloads if destination.startswith("/out/opt/"))
        self.assertEqual(sorted(set(repository)), sorted([
            "cluster-api/" + VERSIONS["capi"], "bootstrap-kubeadm/" + VERSIONS["capi"],
            "control-plane-kubeadm/" + VERSIONS["capi"], "control-plane-kamaji/" + VERSIONS["capi_kamaji"],
            "infrastructure-proxmox/" + VERSIONS["capmox"], "ipam-in-cluster/" + VERSIONS["capi_ipam"],
            "addon-helm/" + VERSIONS["caaph"]]))
        # What lands in {label}/{version}/ is that version's release asset, and a file clusterctl reads.
        config = yaml.safe_load((IMAGE / "clusterctl.yaml").read_text())
        files = {Path(provider["url"]).name for provider in config["providers"]} | {"metadata.yaml"}
        for _, url, destination in adds():
            if destination.startswith("/out/opt/"):
                with self.subTest(url=url):
                    self.assertEqual(re.search(r"/releases/download/([^/]+)/", url).group(1), destination.split("/")[6])
                    self.assertIn(Path(url).name, files)
        for flag, provider in zip(REAL[::2], REAL[1::2]):
            self.assertIn("%s=%s" % (flag, provider), DOCKERFILE)  # the offline build check covers the workload set
        self.assertIn("--control-plane=kubeadm:%s" % VERSIONS["capi"], DOCKERFILE)  # and the seed's control plane
        version = re.search(r'org\.opencontainers\.image\.version="(v[0-9.]+)-[0-9]+"', DOCKERFILE).group(1)
        self.assertEqual(version, VERSIONS["capi"])

    def test_config_points_every_provider_at_a_baked_components_file(self):
        config = yaml.safe_load((IMAGE / "clusterctl.yaml").read_text())
        baked = {destination + Path(url).name for _, url, destination in adds() if destination.endswith("/")}
        types = set()
        for provider in config["providers"]:
            path = provider["url"].replace("/opt/clusterctl/", "/out/opt/clusterctl/", 1)
            self.assertIn(path, baked, provider)
            self.assertIn(str(Path(path).parent) + "/metadata.yaml", baked, provider)
            types.add("%s %s" % (provider["type"], provider["name"]))
        self.assertLessEqual(INVENTORY, types)
        self.assertIn("ControlPlaneProvider kubeadm", types)
        # cert-manager is never baked: a missing one fails on this path instead of a GitHub download.
        self.assertFalse(any("cert-manager" in path for path in baked))
        self.assertTrue(config["cert-manager"]["url"].startswith("/opt/clusterctl/repository/cert-manager/"))
        self.assertEqual(config["cert-manager"]["version"], VERSIONS["cert_manager_chart"])

    def test_runtime_is_offline_and_non_root(self):
        self.assertIn("\nUSER 65532:65532\n", DOCKERFILE)
        self.assertIn("\nENV HOME=/tmp CLUSTERCTL_DISABLE_VERSIONCHECK=true\n", DOCKERFILE)
        self.assertIn("\nRUN --network=none set -eu; \\\n", DOCKERFILE)
        self.assertIn('\nENTRYPOINT ["/usr/local/bin/clusterctl-init"]\n', DOCKERFILE)
        # The entrypoint tests override the config path; by default it is the one the image carries.
        self.assertIn("\nCONFIG=${CLUSTERCTL_INIT_CONFIG:-/opt/clusterctl/clusterctl.yaml}\n", ENTRYPOINT.read_text())
        self.assertIn("\nCOPY clusterctl.yaml /out/opt/clusterctl/clusterctl.yaml\n", DOCKERFILE)


class WorkflowTests(unittest.TestCase):
    def setUp(self):
        self.check, self.publish = WORKFLOW.split("\n  check:\n", 1)[1].split("\n  publish:\n", 1)

    def test_actions_are_sha_pins_already_used_elsewhere(self):
        used = re.findall(r"uses: (\S+)", WORKFLOW)
        self.assertEqual(len(used), 5)
        pinned = re.findall(r"uses: ([A-Za-z0-9_./-]+@[0-9a-f]{40}) # v\S+", WORKFLOW)
        self.assertEqual(used, pinned)
        export = (ROOT / ".github/workflows/vm-export-tools.yml").read_text()
        for pin in pinned:
            self.assertIn("uses: " + pin, export)

    def test_triggers_and_least_privilege(self):
        triggers = WORKFLOW.split("\non:\n", 1)[1].split("\npermissions:", 1)[0]
        self.assertEqual(triggers, '  push:\n    tags: ["proxcloud-clusterctl-v*"]\n  pull_request:\n'
                                   '    branches: [main]\n'
                                   '    paths: ["deploy/clusterctl/**", ".github/workflows/clusterctl.yml"]\n')
        self.assertIn("\npermissions:\n  contents: read\n", WORKFLOW)
        self.assertEqual(re.findall(r"^\s+[a-z-]+: write$", WORKFLOW, flags=re.M), ["      packages: write"])
        for item in ("self-hosted", "secrets.", "continue-on-error", "|| true", "pull_request_target", "workflow_run",
                     "workflow_dispatch", "docker/login-action", "docker/build-push-action", ":latest", "set +e"):
            self.assertNotIn(item, WORKFLOW)

    def test_pull_request_job_cannot_log_in_or_push(self):
        self.assertTrue(self.check.startswith("    if: github.event_name == 'pull_request'\n"))
        for item in ("permissions:", "docker login", "docker push", "docker tag", "github.token", "GH_TOKEN"):
            self.assertNotIn(item, self.check)

    def test_publish_runs_only_on_the_tag_in_this_repository_after_the_same_gate(self):
        self.assertTrue(self.publish.startswith(
            "    if: github.event_name == 'push' && github.repository == 'mutex-solutions/proxcloud'"
            " && startsWith(github.ref, 'refs/tags/proxcloud-clusterctl-v')\n"))
        self.assertIn("  PACKAGE: ghcr.io/mutex-solutions/proxcloud-clusterctl\n", WORKFLOW)
        shared = self.check.split("      - name: Build the image locally without publishing\n", 1)[1].split(
            "      - name: Preserve reports", 1)[0]
        self.assertIn(shared, self.publish)
        self.assertIn('--expected-image-id "$(<result/image-id.txt)" result/scan.json', shared)
        scan = (ROOT / ".github/workflows/security-scan.yml").read_text()
        for line in re.findall(r"^  TRIVY_(?:VERSION|SHA256): .+$", WORKFLOW, flags=re.M):
            self.assertIn(line + "\n", scan)
        order = ['git merge-base --is-ancestor "$SOURCE_SHA" origin/main', "docker build --pull", "--expected-image-id",
                 "docker login ghcr.io", 'org.opencontainers.image.version"}}\')" = "$tag"',
                 "stage.absent_tag(status, raw)", 'docker push "$PACKAGE:$tag"', "docker manifest inspect",
                 "docker logout ghcr.io"]
        positions = [self.publish.index(item) for item in order]
        self.assertEqual(positions, sorted(positions))
        self.assertIn('[[ "$version" =~ ^v[0-9]+\\.[0-9]+\\.[0-9]+-[0-9]+$ ]]', self.publish)

    def test_tag_probe_is_the_reviewed_one_of_vm_export_tools(self):
        def probe(text):
            return text.split("python3 - \"$tag\" <<'PY'\n", 1)[1].split("          PY\n", 1)[0]
        self.assertEqual(probe(self.publish), probe((ROOT / ".github/workflows/vm-export-tools.yml").read_text()))


if __name__ == "__main__":
    unittest.main(verbosity=2)
