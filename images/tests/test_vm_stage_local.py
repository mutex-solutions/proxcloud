"""Authored, NOT run: synthetic bytes/private temporary directories only; no VM/PVE."""
import contextlib
import io
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
try:
    import vm_stage_local as stage
finally:
    sys.path.pop(0)
a = stage.contract


class LocalStageTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(); self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.staging = self.root / "staging"; self.staging.mkdir(mode=0o700)
        self.temp = self.root / "runner"; self.temp.mkdir(mode=0o700)
        self.name = "vm-export-ubuntu-2604-20261111000000-aaaaaaaaaaaa"
        self.source = self.staging / self.name; self.source.mkdir(mode=0o700)
        self.target = self.temp / "proxcloud-vm-payload"
        disk = b"synthetic bytes, deliberately not a QCOW2 image"
        request = dict(kind="vm-template", os="ubuntu-2604", arch="amd64", variant="os",
                       source_commit="a" * 40, upstream_checksum="b" * 64, build_version="20261111000000")
        receipt = dict(kind="CleanExportReceipt", source_identity="sha256:" + "c" * 64,
                       source_commit=request["source_commit"], request_sha256=a.sha(a.encode(request)),
                       run_ref="https://github.com/mutex-solutions/proxcloud/actions/runs/1",
                       disk_sha256=a.sha(disk), disk_bytes=len(disk), virtual_size_bytes=20 << 30,
                       checks={key: True for key in a.CHECKS})
        manifest = dict(request, schema_version=1, image_tag=a.image_tag(request), clean_export_receipt=receipt,
                        disk=dict(filename="disk.qcow2", format="qcow2", sha256=a.sha(disk),
                                  bytes=len(disk), virtual_size_bytes=20 << 30))
        raw = a.encode(manifest)
        self.payload = {"disk.qcow2": disk, "manifest.json": raw, "SHA256SUMS": a.sums(manifest, raw)}
        for name, data in self.payload.items():
            (self.source / name).write_bytes(data); (self.source / name).chmod(0o600)
        self.pins = [a.sha(raw), a.sha(disk)]
        for mocked in (patch.object(stage, "SOURCE_ROOT", str(self.staging)),
                       patch.dict(os.environ, {"RUNNER_TEMP": str(self.temp)})):
            mocked.start(); self.addCleanup(mocked.stop)

    def test_exact_copy_and_exclusive_destination(self):
        stage.stage(self.name, *self.pins)
        self.assertEqual({p.name: p.read_bytes() for p in self.target.iterdir()}, self.payload)
        self.assertEqual(self.target.stat().st_mode & 0o777, 0o700)
        with self.assertRaises(FileExistsError): stage.stage(self.name, *self.pins)
        self.assertEqual((self.target / "disk.qcow2").read_bytes(), self.payload["disk.qcow2"])

    def test_inputs_pins_names_and_permissions_refused_before_copy(self):
        for name, pins in [("../" + self.name, self.pins), (self.name + "\n", self.pins),
                           (self.name.replace("20261111000000", "20260230000000"), self.pins),
                           (self.name.replace("ubuntu-2604", "debian-13"), self.pins),
                           (self.name, ["A" * 64, self.pins[1]]), (self.name, ["0" * 64, self.pins[1]]),
                           (self.name, [self.pins[0], "0" * 64])]:
            with self.subTest(name=name, pins=pins), self.assertRaises((OSError, ValueError)):
                stage.stage(name, *pins)
            self.assertFalse(self.target.exists())
        self.source.chmod(0o770)
        with self.assertRaises(ValueError): stage.stage(self.name, *self.pins)
        self.source.chmod(0o700)
        with patch.object(stage.os, "getuid", return_value=os.getuid() + 1), self.assertRaises(ValueError):
            stage.stage(self.name, *self.pins)
        with patch.dict(os.environ, {"RUNNER_TEMP": "relative"}), self.assertRaises(ValueError):
            stage.stage(self.name, *self.pins)
        with contextlib.redirect_stderr(io.StringIO()) as errors:
            self.assertEqual(stage.main(["--staging-dir", "/private/path"]), 2)
        self.assertNotIn("/private/path", errors.getvalue())

    def test_writable_temp_or_ancestor_refused_before_copy(self):
        for directory in (self.temp, self.root):
            with self.subTest(directory=directory.name):
                prior = directory.stat().st_mode & 0o777
                directory.chmod(0o777)
                try:
                    with self.assertRaises(ValueError):
                        stage.stage(self.name, *self.pins)
                    self.assertFalse(self.target.exists())
                finally:
                    directory.chmod(prior)

    def test_hardlinks_symlink_parents_files_and_total_bound_refused(self):
        os.link(self.source / "disk.qcow2", self.root / "hardlink")
        with self.assertRaises(ValueError): stage.stage(self.name, *self.pins)
        (self.root / "hardlink").unlink()
        (self.source / "disk.qcow2").unlink()
        (self.source / "disk.qcow2").symlink_to(self.root / "outside")
        with self.assertRaises(OSError): stage.stage(self.name, *self.pins)
        (self.source / "disk.qcow2").unlink(); (self.source / "disk.qcow2").write_bytes(self.payload["disk.qcow2"])
        (self.source / "disk.qcow2").chmod(0o600)
        alias = self.root / "alias"; alias.symlink_to(self.staging, target_is_directory=True)
        with patch.object(stage, "SOURCE_ROOT", str(alias)), self.assertRaises(OSError): stage.stage(self.name, *self.pins)
        total = sum(map(len, self.payload.values()))
        with patch.object(a, "MAX_DISK_BYTES", total), self.assertRaises(ValueError): stage.stage(self.name, *self.pins)
        self.assertFalse(self.target.exists())
        self.target.symlink_to(self.source, target_is_directory=True)
        with self.assertRaises(FileExistsError): stage.stage(self.name, *self.pins)
