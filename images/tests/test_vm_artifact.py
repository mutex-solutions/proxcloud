"""Authored, NOT run: python3 -m unittest discover -s images/tests -p test_vm_artifact.py -v
Only synthetic bytes/local temporary directories; never parse QCOW2 or contact PVE/GHCR.
"""
import contextlib
import copy
import importlib.util
import io
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
SPEC = importlib.util.spec_from_file_location("vm_artifact", Path(__file__).resolve().parents[1] / "vm_artifact.py")
a = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(a)

class ArtifactTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(); self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.disk = b"QFI\xfb\x00\x00\x00\x03" + bytes(96)  # Deliberately NOT a valid QCOW2.
        (self.root / "disk.qcow2").write_bytes(self.disk)
        self.fd = os.open(self.root, os.O_RDONLY | os.O_DIRECTORY); self.addCleanup(os.close, self.fd)
        self.q = dict(kind="vm-template", os="ubuntu-2604", arch="amd64", variant="os", source_commit="a" * 40,
                      upstream_checksum="b" * 64, build_version="20261111000000")
        self.r = dict(kind="CleanExportReceipt", source_identity="sha256:" + "c" * 64, source_commit="a" * 40,
                      run_ref="https://github.com/mutex-solutions/proxcloud/actions/runs/1", request_sha256=a.sha(a.encode(self.q)),
                      disk_sha256=a.sha(self.disk), disk_bytes=len(self.disk), virtual_size_bytes=20 << 30,
                      checks={k: True for k in a.CHECKS})
    def prepare(self):
        for name, obj in (("request.json", self.q), ("clean-export-receipt.json", self.r)):
            (self.root / name).write_bytes(a.encode(obj))
        return a.prepare(self.fd)
    def test_roundtrip_pins_and_plan_are_not_attestation(self):
        m = self.prepare(); raw = (self.root / "manifest.json").read_bytes()
        self.assertEqual(a.verify(self.fd, a.sha(raw), a.sha(self.disk)), m)
        self.assertEqual((self.root / "SHA256SUMS").read_bytes(), a.sums(m, raw))
        p = a.plan(m); self.assertFalse(p["execute"] or p["publish_eligible"] or any(p["release_gates"].values()))
        self.assertIn("--image-spec", p["argv"]); self.assertIn(a.ARTIFACT_TYPE, p["argv"])
        with self.assertRaises(OSError): a.prepare(self.fd)  # Never overwrite outputs.
        (self.root / "SHA256SUMS").write_bytes(b"wrong\n")
        with self.assertRaises(ValueError): a.verify(self.fd)
        (self.root / "SHA256SUMS").write_bytes(a.sums(m, raw))
        for pins in (("0" * 64, None), (None, "0" * 64), ("sha256:" + "a" * 64, None)):
            with self.subTest(pins=pins), self.assertRaises(ValueError): a.verify(self.fd, *pins)
        (self.root / "disk.qcow2").write_bytes(self.disk + b"tamper")
        with self.assertRaises(ValueError): a.verify(self.fd)
    def test_strict_schema_and_receipt(self):
        m = self.prepare()
        mutations = [((), "token", "secret"), (("disk",), "filename", "../other"), (("disk",), "filename", "disk.qcow2\r\n"),
                     ((), "build_version", "latest"), ((), "build_version", "20260230000000"), ((), "os", "debian-14"),
                     ((), "source_commit", "a" * 39), ((), "arch", "arm64"), (("disk",), "bytes", True),
                     (("disk",), "bytes", a.MAX_DISK_BYTES), (("disk",), "virtual_size_bytes", 19 << 30),
                     (("clean_export_receipt",), "run_ref", "https://user:secret@github.com/x"),
                     (("clean_export_receipt",), "source_identity", "node/9200"), (("clean_export_receipt",), "request_sha256", "0" * 64)]
        mutations += [(("clean_export_receipt", "checks"), k, v) for k in a.CHECKS for v in (False, 1, "true")]
        for path, key, value in mutations:
            bad = copy.deepcopy(m); obj = bad
            for part in path: obj = obj[part]
            obj[key] = value
            with self.subTest(key=key, value=value), self.assertRaises(ValueError): a.validate(bad)
    def test_selectors_provenance_and_duplicate_keys(self):
        for slug in ("ubuntu-2404", "ubuntu-2604", "debian-12", "debian-13"):
            a.request_valid(dict(self.q, os=slug))
        a.request_valid(dict(self.q, variant="kubernetes", kubernetes_version="1.35.10"))
        for changes in ({"variant": "kubernetes"}, {"kubernetes_version": "1.2.3"},
                        {"variant": "kubernetes", "os": "debian-13", "kubernetes_version": "1.35.10"},
                        {"variant": "kubernetes", "kubernetes_version": "v1.2.3"}):
            with self.assertRaises(ValueError): a.request_valid(dict(self.q, **changes))
        a.request_valid(dict(self.q, provenance={"upstream_url": "https://cloud-images.ubuntu.com/noble/current/image.img"}))
        for url in ("https://cloud-images.ubuntu.com@evil.test/x", "https://cloud-images.ubuntu.com/x?token=secret", "https://cloud-images.ubuntu.com/../x", "https://192.0.2.1/x"):
            with self.assertRaises(ValueError): a.request_valid(dict(self.q, provenance={"upstream_url": url}))
        with self.assertRaises(ValueError): a.decode(b'{"kind":"a","kind":"b"}')
    def test_missing_receipt_false_claims_symlink_and_size_refused(self):
        (self.root / "request.json").write_bytes(a.encode(self.q))
        with self.assertRaises(OSError): a.prepare(self.fd)
        self.r["checks"]["offline_sanitization"] = False
        with self.assertRaises(ValueError): self.prepare()
        self.assertFalse((self.root / "manifest.json").exists())
        (self.root / "manifest.json").symlink_to(self.root / "request.json")
        with self.assertRaises(OSError): a.verify(self.fd)
        with patch.object(a, "MAX_DISK_BYTES", len(self.disk)), self.assertRaises(ValueError): a.disk_hash(self.fd)
        with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(a.main(["prepare", "--staging-dir", "relative"]), 2)
