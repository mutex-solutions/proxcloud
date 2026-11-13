"""The accepted Kubernetes node template versions are one explicit allowlist, repeated in three modules that must stay
self-contained (the sanitizer and the copy policy import nothing from this repository). This test keeps them equal,
and checks the copy policy accepts each listed version only with matching package versions."""
from copy import deepcopy
from pathlib import Path
import sys
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
try:
    import k8s_copy_policy
    import vm_publish_stage
    import vm_sanitize
finally:
    sys.path.pop(0)
sys.path.insert(0, str(Path(__file__).resolve().parent))
try:
    from test_k8s_copy_policy import fixture
finally:
    sys.path.pop(0)


class KubernetesVersionsTests(unittest.TestCase):
    def test_the_three_allowlists_are_equal(self):
        self.assertEqual(vm_publish_stage.K8S_VERSIONS, vm_sanitize.K8S_VERSIONS)
        self.assertEqual(vm_publish_stage.K8S_VERSIONS, k8s_copy_policy.K8S_VERSIONS)
        self.assertIn("1.35.10", vm_publish_stage.K8S_VERSIONS)
        self.assertEqual(len(set(vm_publish_stage.K8S_VERSIONS)), len(vm_publish_stage.K8S_VERSIONS))

    def test_copy_policy_accepts_each_version_with_its_own_packages(self):
        for version in k8s_copy_policy.K8S_VERSIONS:
            with self.subTest(version=version):
                facts = deepcopy(fixture())
                facts["k8s"]["version"] = version
                facts["k8s"]["kubernetes_packages"] = dict.fromkeys(("kubeadm", "kubelet", "kubectl"), version)
                self.assertTrue(k8s_copy_policy.validate_inventory(facts).copy_only)

    def test_copy_policy_refuses_unlisted_versions_and_mismatched_packages(self):
        for version, packages in (("1.38.0", "1.38.0"), ("1.36.5", "1.36.5"), ("1.36.6", "1.35.10"), ("1.37.2", "1.36.6")):
            with self.subTest(version=version, packages=packages):
                facts = deepcopy(fixture())
                facts["k8s"]["version"] = version
                facts["k8s"]["kubernetes_packages"] = dict.fromkeys(("kubeadm", "kubelet", "kubectl"), packages)
                with self.assertRaises(k8s_copy_policy.PolicyRefusal):
                    k8s_copy_policy.validate_inventory(facts)


if __name__ == "__main__":
    unittest.main()
