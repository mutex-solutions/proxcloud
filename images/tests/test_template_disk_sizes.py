"""Template virtual disk sizes (owner decision 2026-11-14): 5 GiB native OS, 8 GiB Kubernetes node, and the 20 GiB
of templates built before that stays accepted. The builders, the sanitizer, its in-capsule stream gate and the copy
policy must stay self-contained, so each repeats its numbers; this test keeps them equal. It also pins the checks
that stop a build whose disk filled up, because the new sizes leave much less room."""
import importlib.util
from pathlib import Path
import re
import sys
import unittest

IMAGES = Path(__file__).resolve().parents[1]


def load(name, file):
    spec = importlib.util.spec_from_file_location(name, IMAGES / file)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


sys.path.insert(0, str(IMAGES))
try:
    import k8s_copy_policy
    import vm_artifact
    import vm_sanitize
    from vm_export import stream_sanitized
finally:
    sys.path.pop(0)
build = load("disk_sizes_build", "build.py")
k8s_config = load("disk_sizes_k8s_config", "k8s-build-config.py")
LEGACY = 20 << 30


class TemplateDiskSizeTests(unittest.TestCase):
    def test_one_size_per_variant_plus_the_legacy_20_gib(self):
        self.assertEqual(vm_sanitize.VIRTUAL_BYTES, {"os": (5 << 30, LEGACY), "kubernetes": (8 << 30, LEGACY)})
        self.assertEqual(build.DEFAULT_BUILD_DISK_GIB << 30, vm_sanitize.VIRTUAL_BYTES["os"][0])
        self.assertEqual(k8s_config.K8S_BUILD_DISK_GIB << 30, vm_sanitize.VIRTUAL_BYTES["kubernetes"][0])
        self.assertEqual(k8s_copy_policy.VIRTUAL_BYTES, vm_sanitize.VIRTUAL_BYTES["kubernetes"])
        self.assertEqual(stream_sanitized.VIRTUAL_BYTES, vm_sanitize.VIRTUAL_BYTES)

    def test_every_accepted_size_fits_the_artifact_contract_and_is_whole_gib(self):
        for sizes in vm_sanitize.VIRTUAL_BYTES.values():
            for size in sizes:
                self.assertTrue(vm_artifact.MIN_VIRTUAL_BYTES <= size <= vm_artifact.MAX_VIRTUAL_BYTES)
                self.assertEqual(size % (1 << 30), 0)  # The cold boot reads the import back as size=<n>G.
        self.assertEqual(vm_artifact.MIN_VIRTUAL_BYTES, min(min(s) for s in vm_sanitize.VIRTUAL_BYTES.values()))

    def test_portal_contract_has_the_same_bounds(self):
        go = (IMAGES.parent / "internal/templateupdates/vmartifact.go").read_text()
        bounds = {name: int(n) << 30 for name, n in re.findall(r"(M(?:in|ax)VirtualSizeBytes) += ([0-9]+) << 30", go)}
        self.assertEqual(bounds, {"MinVirtualSizeBytes": vm_artifact.MIN_VIRTUAL_BYTES,
                                  "MaxVirtualSizeBytes": vm_artifact.MAX_VIRTUAL_BYTES})

    def test_a_full_disk_stops_the_native_build(self):
        """Every provisioning command runs under `sh -eu`, APT is never allowed to fail, and a nearly full
        root filesystem is refused before the clone can become a template."""
        template = (IMAGES / "packer/template.pkr.hcl").read_text()
        self.assertIn('execute_command = "sudo -n sh -eu {{ .Path }}"', template)
        self.assertIn('scripts         = ["scripts/base.sh", "scripts/cleanup.sh"]', template)
        base = (IMAGES / "packer/scripts/base.sh").read_text()
        for line in base.splitlines():
            if line.startswith("apt-get"):
                self.assertNotRegex(line, r"\|\||;|&&|2>")
        self.assertRegex(base, r"(?m)^df -Pk /\napt-get update -qq\n.*dist-upgrade\n.*\ndf -Pk /$")
        self.assert_free_space_guard((IMAGES / "packer/scripts/cleanup.sh").read_text(), before="fstrim")

    def test_a_nearly_full_kubernetes_node_image_is_refused(self):
        cleanup = (IMAGES / "k8s-build-cleanup.sh").read_text()
        self.assertIn("set -euo pipefail", cleanup)
        self.assert_free_space_guard(cleanup, before="sync")

    def assert_free_space_guard(self, script, before):
        guard = ('MIN_FREE_KIB=1048576\n(?:df -Pk /\n)?set -- \\$\\(df -Pk / \\| tail -n 1\\)\n'
                 '\\[ "\\$4" -ge "\\$MIN_FREE_KIB" \\] \\|\\| \\{ echo "[^"\n]*" >&2; exit 1; \\}\n' + before)
        self.assertRegex(script, guard)


if __name__ == "__main__":
    unittest.main()
