"""Synthetic SOURCE-ONLY fixtures. UNRUN; not evidence about template100.

No guest libraries, target execution, disk reads, network, host globbing or writes.
Only imports reviewed local pure policy plus standard-library test helpers when a
future reviewer explicitly chooses to run these tests. No command-line entrypoint.
"""
from copy import deepcopy
from pathlib import Path
import sys
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
try:
    from k8s_copy_policy import (CLUSTER, REFUSAL, VIRTUAL_BYTES, CopyPlan,
                                 PolicyRefusal, validate_inventory)
finally:
    sys.path.pop(0)


def fixture():
    """Hypothetical inventory, NOT discovered facts or a ready-for-public receipt."""
    return {
        "parent": {"format": "raw", "virtual_bytes": VIRTUAL_BYTES,
                   "raw_bytes": VIRTUAL_BYTES, "source_hash_verified": True, "source_stopped": True},
        "os": {"distro": "ubuntu", "major": 26, "minor": 4},
        "layout": {"bios": "seabios", "partition_table": "msdos", "root_fs": "ext4",
                   "root_count": 1, "partition_count": 1, "root_partition_number": 1,
                   "mountpoints": ["/"], "esp_count": 0,
                   "extra_filesystems": 0, "external_fstab": False, "unsafe_links": 0,
                   "swap_entries": 0},
        "accounts": [{"name": "root", "uid": 0, "home": "/root", "shell": "/bin/bash",
                      "kind": "root", "passwd_marker": "x", "shadow_marker": "*"},
                     {"name": "builder", "uid": 1000, "home": "/home/builder", "shell": "/bin/bash",
                      "kind": "native_login", "passwd_marker": "x", "shadow_marker": "!"}],
        "firstboot": {"network_backend": "netplan-systemd-networkd", "cloud_default_user": "ubuntu",
                      "cloud_base_config_reviewed": True},
        "k8s": {"version": "1.35.10", "kubelet_enabled": True, "kubelet_active": False,
                "runtime_version": "2.1.0", "cluster": dict.fromkeys(CLUSTER, 0),
                "bootstrap_token_files": 0,
                "kubernetes_packages": dict.fromkeys(("kubeadm", "kubelet", "kubectl"), "1.35.10"),
                "retained_package_policy_reviewed": True, "package_policy_sha256": "c" * 64},
        "cloud_dropins": [],
        "hygiene": {"unsafe_apt_repo_urls": 0, "registry_config_credentials": 0,
                    "unclassified_credentials": 0},
        # Real-image class: builder in 6 supplementary groups; snapd registered (serial, key-id,
        # serial assertions, one device key), no session macaroon and no store users.
        "identity": {"builder_supplementary_groups": 6,
                     "snapd": {"state": True, "device_serial": True, "device_key_id": True,
                               "session_macaroon": False, "users": 0, "serial_assertions": 4,
                               "device_keys": 1}},
    }


class CopyPolicyTests(unittest.TestCase):
    def refuses(self, facts, review=()):
        with self.assertRaises(PolicyRefusal) as caught:
            validate_inventory(facts, review)
        self.assertEqual(str(caught.exception), REFUSAL)

    def test_plan_is_not_public_or_execution_receipt(self):
        facts = fixture()
        before = deepcopy(facts)
        plan = validate_inventory(facts)
        self.assertEqual(facts, before)
        self.assertFalse(plan.public_eligible)
        self.assertTrue(plan.copy_only)
        self.assertEqual(CopyPlan().status, "NEEDS_INSPECTION")
        self.assertNotIn("/etc/cloud/cloud.cfg.d/*", plan.wipe)
        self.assertNotIn("/etc/kubernetes", plan.wipe)
        self.assertNotIn("/etc/kubernetes/manifests", plan.wipe)  # Placeholder is retained, not wiped.
        # Snapd device key: exact entries-only COPY wipe; credential.secret is never a wipe target.
        self.assertIn("/var/lib/snapd/device/private-keys-v1/*", plan.wipe)
        self.assertIn("/var/lib/snapd/assertions/asserts-v0/serial", plan.wipe)
        self.assertEqual(sorted(p for p in plan.wipe if "snapd" in p),  # Never seed/snaps/state.json/assertions.
                         ["/var/lib/snapd/assertions/asserts-v0/serial", "/var/lib/snapd/device/private-keys-v1/*"])
        self.assertFalse([p for p in plan.wipe if "credential" in p])
        self.assertFalse([p for p in plan.wipe if "group" in p or "sudoers" in p])  # Rewritten, never wiped.
        self.assertTrue(any("supplementary group" in r for r in plan.reset))
        self.assertTrue(any("unregistered" in r and "serial" in r for r in plan.reset))
        self.assertIn("/etc/containerd", plan.retain)
        self.assertIn("kubelet units and enabled=True/active=False", plan.retain)

    def test_missing_unknown_and_extra_facts(self):
        self.refuses(None)
        for key in fixture():
            f = fixture()
            del f[key]
            self.refuses(f)
        f = fixture()
        f["checks"] = {"offline_sanitization": True}  # Cannot forge a receipt here.
        self.refuses(f)
        f = fixture()
        f["accounts"][1]["name"] = "UNKNOWN"
        self.refuses(f)

    def test_unexpected_layout_version_account_or_credentials(self):
        cases = [("layout", "esp_count", 1), ("layout", "external_fstab", True),
                 ("layout", "mountpoints", ["/", "/mnt/external"]),
                 ("layout", "partition_table", "UNKNOWN"), ("layout", "root_fs", "xfs"),
                 ("layout", "extra_filesystems", 1), ("layout", "unsafe_links", 1),
                 ("os", "minor", 10), ("k8s", "version", "1.35.11"),
                 ("k8s", "kubelet_active", True), ("k8s", "kubelet_enabled", False),
                 ("parent", "format", "qcow2"), ("parent", "raw_bytes", VIRTUAL_BYTES - 1),
                 ("parent", "virtual_bytes", VIRTUAL_BYTES + 1),
                 ("parent", "source_hash_verified", False),
                 ("hygiene", "unsafe_apt_repo_urls", 1),
                 ("hygiene", "registry_config_credentials", 1)]
        for section, key, value in cases:
            with self.subTest(section=section, key=key):
                f = fixture()
                f[section][key] = value
                self.refuses(f)
        for key, value in (("name", "ubuntu"), ("shadow_marker", "$y$synthetic-not-a-secret"),
                           ("shadow_marker", "!$6$synthetic"), ("passwd_marker", "plaintext")):
            f = fixture()
            f["accounts"][1][key] = value
            self.refuses(f)

    def test_existing_cluster_state_is_refusal_not_wipe(self):
        for key in CLUSTER:
            f = fixture()
            f["k8s"]["cluster"][key] = 1
            self.refuses(f)

    def test_exact_types_not_truthiness_or_normalization(self):
        for section, key, value in (("parent", "source_stopped", 1),
                                    ("parent", "source_hash_verified", 1),
                                    ("k8s", "kubelet_enabled", 1), ("k8s", "kubelet_active", 0),
                                    ("layout", "external_fstab", 0), ("layout", "esp_count", False),
                                    ("layout", "root_count", True), ("os", "minor", "04"),
                                    ("os", "distro", "Ubuntu"), ("os", "major", 26.0)):
            f = fixture()
            f[section][key] = value
            self.refuses(f)
        for value in (False, -1, 20001, "0", None):
            f = fixture()
            f["k8s"]["cluster"]["etcd_db"] = value
            self.refuses(f)

    def test_cloud_dropins_require_independent_exact_build_review(self):
        f = fixture()
        f["cloud_dropins"] = [{"name": "50-cloud-init.cfg", "sha256": "a" * 64,
                               "regular_no_symlinks": True}]
        self.refuses(f)  # No default vendor whitelist, even for a plausible name.
        review = (("50-cloud-init.cfg", "a" * 64, "preserve_vendor"),)
        plan = validate_inventory(f, review)
        self.assertEqual(plan.reviewed_dropins, review)
        f["cloud_dropins"][0]["regular_no_symlinks"] = 1
        self.refuses(f, review)
        f["cloud_dropins"][0]["regular_no_symlinks"] = True
        f["cloud_dropins"][0]["sha256"] = "b" * 64
        self.refuses(f, review)
        self.refuses(fixture(), review)

    def test_required_firstboot_partition_and_package_facts_fail_closed(self):
        fields = (("firstboot", "network_backend"), ("firstboot", "cloud_default_user"),
                  ("firstboot", "cloud_base_config_reviewed"), ("layout", "partition_count"),
                  ("layout", "root_partition_number"), ("k8s", "retained_package_policy_reviewed"),
                  ("k8s", "package_policy_sha256"), ("k8s", "kubernetes_packages"))
        for section, key in fields:
            for unknown in (None, "UNKNOWN", False):
                f = fixture()
                f[section][key] = unknown
                self.refuses(f)
            f = fixture()
            del f[section][key]
            self.refuses(f)
        for section, key, wrong in (("firstboot", "network_backend", "NetworkManager"),
                                   ("firstboot", "cloud_default_user", "builder"),
                                   ("firstboot", "cloud_default_user", "root"),
                                   ("firstboot", "cloud_default_user", "Ubuntu"),
                                   ("firstboot", "cloud_base_config_reviewed", 1),
                                   ("layout", "partition_count", 2),
                                   ("layout", "partition_count", True),
                                   ("layout", "root_partition_number", 2),
                                   ("k8s", "retained_package_policy_reviewed", 1),
                                   ("k8s", "package_policy_sha256", "not-reviewed")):
            f = fixture()
            f[section][key] = wrong
            self.refuses(f)
        for package in ("kubeadm", "kubelet", "kubectl"):
            f = fixture()
            f["k8s"]["kubernetes_packages"][package] = "1.35.11"
            self.refuses(f)

    def test_account_profile_must_match_fixed_cleanup_scope(self):
        for key, wrong in (("home", "/srv/builder"), ("home", "/home/../private"),
                           ("shell", "/bin/zsh"), ("kind", "UNKNOWN"), ("uid", 1001)):
            f = fixture()
            f["accounts"][1][key] = wrong
            self.refuses(f)
        for key in ("home", "shell", "kind"):
            f = fixture()
            del f["accounts"][1][key]
            self.refuses(f)
        f = fixture()
        f["accounts"][0]["home"] = "/other-root"
        self.refuses(f)

    def test_refusal_never_echoes_untrusted_content(self):
        f = fixture()
        f["k8s"]["runtime_version"] = "secret/path/" + "x" * 4096
        self.refuses(f)
        f = fixture()
        f["cloud_dropins"] = [{"name": "../private.cfg", "sha256": "a" * 64,
                               "regular_no_symlinks": True}]
        self.refuses(f)

    def test_base_passwd_sync_exact_profile_only(self):
        sync = {"name": "sync", "uid": 4, "home": "/bin", "shell": "/bin/sync", "kind": "system",
                "passwd_marker": "x", "shadow_marker": "*"}
        f = fixture()
        f["accounts"].append(dict(sync))
        validate_inventory(f)
        for key, wrong in (("uid", 5), ("home", "/"), ("kind", "UNKNOWN"), ("name", "sync2")):
            f = fixture()
            f["accounts"].append(dict(sync, **{key: wrong}))
            self.refuses(f)
        f = fixture()
        f["accounts"].append(dict(sync, name="games", uid=5))  # /bin/sync is not a general exception.
        self.refuses(f)

    def test_swap_is_refusal_not_wipe(self):
        for value in (1, 3, True, None, "0", -1):
            f = fixture()
            f["layout"]["swap_entries"] = value
            self.refuses(f)
        f = fixture()
        del f["layout"]["swap_entries"]
        self.refuses(f)

    def test_upstream_ubuntu_default_user_and_locked_builder(self):
        # Owner decision: cloud-init's upstream default user `ubuntu`, created at first boot.
        self.assertEqual(validate_inventory(fixture()).status, "COPY_POLICY_ONLY_NEEDS_REVIEW")
        f = fixture()
        f["firstboot"]["cloud_default_user"] = "builder"  # builder is never the cloud default user.
        self.refuses(f)
        for account in ({"name": "ubuntu", "uid": 1001, "home": "/home/ubuntu", "shell": "/bin/bash",
                         "kind": "native_login", "passwd_marker": "x", "shadow_marker": "!"},
                        {"name": "ubuntu", "uid": 999, "home": "/nonexistent", "shell": "/usr/sbin/nologin",
                         "kind": "system", "passwd_marker": "x", "shadow_marker": "!*"}):
            f = fixture()
            f["accounts"].append(account)  # A pre-existing ubuntu account refuses, whatever its shape.
            self.refuses(f)
        for marker in ("$6$synthetic", "!$6$synthetic", "", "UNKNOWN"):
            f = fixture()
            f["accounts"][1]["shadow_marker"] = marker  # builder must stay locked, unrecoverably.
            self.refuses(f)
        f = fixture()
        del f["accounts"][1]  # builder remains the exact retained native_login account.
        self.refuses(f)

    def test_identity_facts_reset_plan_and_store_credentials_refuse(self):
        for groups in (0, 6, 20000):  # Recorded only: the copy drops builder from all of them.
            f = fixture()
            f["identity"]["builder_supplementary_groups"] = groups
            validate_inventory(f)
        f = fixture()
        f["identity"]["snapd"] = {"state": False, "device_serial": False, "device_key_id": False,
                                  "session_macaroon": False, "users": 0, "serial_assertions": 0,
                                  "device_keys": 0}
        validate_inventory(f)  # Unseeded/absent snapd is fine.
        for key, value in (("session_macaroon", True), ("users", 1), ("users", 3)):
            f = fixture()
            f["identity"]["snapd"][key] = value  # Store user credentials: refuse, never wipe.
            self.refuses(f)
        for section, key, value in (("identity", "builder_supplementary_groups", True),
                                    ("identity", "builder_supplementary_groups", -1),
                                    ("identity", "builder_supplementary_groups", None)):
            f = fixture()
            f[section][key] = value
            self.refuses(f)
        for key, value in (("state", 1), ("device_serial", "yes"), ("session_macaroon", 0),
                           ("users", False), ("serial_assertions", -1), ("device_keys", 20001)):
            f = fixture()
            f["identity"]["snapd"][key] = value
            self.refuses(f)
        f = fixture()
        f["identity"]["snapd"].update(state=False)  # Serial/key-id claimed without a state file: incoherent.
        self.refuses(f)
        for missing in ("builder_supplementary_groups", "snapd"):
            f = fixture()
            del f["identity"][missing]
            self.refuses(f)
        f = fixture()
        del f["identity"]["snapd"]["users"]
        self.refuses(f)
        f = fixture()
        f["identity"]["snapd"]["serial"] = "never-a-value"  # Presence/count only; no extra keys.
        self.refuses(f)

    def test_readme_only_by_exact_reviewed_row(self):
        f = fixture()
        f["cloud_dropins"] = [{"name": "README", "sha256": "d" * 64, "regular_no_symlinks": True}]
        self.refuses(f)  # Present but unreviewed.
        review = (("README", "d" * 64, "preserve_vendor"),)
        self.assertEqual(validate_inventory(f, review).reviewed_dropins, review)
        self.refuses(f, (("README", "e" * 64, "preserve_vendor"),))
        for name in ("readme", "README.txt", "README.md", "NOTES", "README/x"):
            f = fixture()
            f["cloud_dropins"] = [{"name": name, "sha256": "d" * 64, "regular_no_symlinks": True}]
            self.refuses(f, ((name, "d" * 64, "preserve_vendor"),))
