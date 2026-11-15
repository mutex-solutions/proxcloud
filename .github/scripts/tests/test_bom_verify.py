#!/usr/bin/env python3
"""bom_verify.py against a fake opener: no network, standard library only."""
import email.message
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import unittest
import urllib.error

ROOT = Path(__file__).resolve().parents[3]
spec = importlib.util.spec_from_file_location("bom_verify", ROOT / ".github/scripts/bom_verify.py")
bv = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bv)
bv.time.sleep = lambda _: None  # retries without the wait

A, B, C = ("sha256:" + c * 64 for c in "abc")
SCRIPT = b"#!/bin/sh\necho k3s\n"
BOM = """# digest: a comment holds no pin
schemaVersion: 1
release: 0.8.0-dev   # main
seed:
  image: {package: vm-os, tag: os-1, digest: "%s"}
  k3s:
    installScript: {url: "https://example.test/v1%%2Bk3s1/install.sh", sha256: %s}
images:
  kubeVip: "reg.test/kube-vip/kube-vip:v0.8.9@%s"
components:
  - name: a
    chart: {repo: reg.test/org/charts, name: ours, version: 0.1.0, digest: "%s"}
  - name: b
    chart: {repo: reg.test/org/charts, name: ours, version: 0.1.0, digest: "%s"}
  - name: c
    chart: {repo: "https://charts.test/repo", name: cilium, version: 1.18.16, digest: "%s"}
""" % (A, hashlib.sha256(SCRIPT).hexdigest(), B, A, A, C)
INDEX = """apiVersion: v1
entries:
  cilium-extra:
  - digest: %s
    name: cilium-extra
    version: 1.18.16
  cilium:
  - apiVersion: v2
    dependencies:
    - name: sub
      version: 1.18.16
      digest: %s
    digest: %s
    name: cilium
    version: 1.18.16
  - digest: %s
    version: "1.18.15"
generated: "2026-11-15T00:00:00Z"
""" % ("d" * 64, "e" * 64, "c" * 64, "f" * 64)


class Response(io.BytesIO):
    def __init__(self, body=b"", headers=None, status=200):
        super().__init__(body)
        self.status, self.headers = status, headers or {}


def http_error(url, code, headers=None):
    message = email.message.Message()
    for key, value in (headers or {}).items():
        message[key] = value
    return urllib.error.HTTPError(url, code, "", message, io.BytesIO())


class Registry:
    """Fake opener: OCI manifests behind an anonymous token, one chart index and one file."""

    def __init__(self):
        self.manifests = {"ghcr.io/mutex-solutions/vm-os:os-1": A, "reg.test/kube-vip/kube-vip:v0.8.9": B,
                          "reg.test/org/charts/ours:0.1.0": A}
        self.files = {"https://charts.test/repo/index.yaml": INDEX.encode(), "https://example.test/v1%2Bk3s1/install.sh": SCRIPT}
        self.calls, self.fail = [], {}

    def __call__(self, req, timeout=None):
        url = req.full_url
        self.calls.append((req.get_method(), url))
        if self.fail.get(url):
            raise self.fail[url].pop(0)
        if url.startswith("https://auth.test/token"):
            return Response(json.dumps({"token": "anon"}).encode())
        if "/v2/" in url:
            host, rest = url[len("https://"):].split("/v2/")
            ref = "%s/%s" % (host, rest.replace("/manifests/", ":"))
            if req.get_header("Authorization") != "Bearer anon":
                raise http_error(url, 401, {"WWW-Authenticate": 'Bearer realm="https://auth.test/token",service="reg",scope="repository:x:pull"'})
            if ref not in self.manifests:
                raise http_error(url, 404)
            digest = self.manifests[ref]
            return Response(headers={"Docker-Content-Digest": digest} if digest else {})
        if url in self.files:
            return Response(self.files[url])
        raise http_error(url, 404)


def run(text=BOM, registry=None, **kwargs):
    lines = []
    failures = bv.verify(text, registry or Registry(), out=lines.append, **kwargs)
    return failures, lines


class BomVerifyTest(unittest.TestCase):
    def test_all_pins_match_one_line_per_item(self):
        registry = Registry()
        failures, lines = run(registry=registry)
        self.assertEqual(failures, 0, lines)
        self.assertEqual(len(lines), 5)  # the chart pinned twice is one item
        self.assertTrue(all(line.startswith("ok ") for line in lines), lines)
        self.assertIn("ok   oci reg.test/org/charts/ours:0.1.0 (line 12,14) " + A, lines)
        self.assertEqual({method for method, url in registry.calls if "/v2/" in url}, {"HEAD"})

    def test_moved_digests_fail(self):
        registry = Registry()
        registry.manifests["reg.test/org/charts/ours:0.1.0"] = B
        registry.files["https://example.test/v1%2Bk3s1/install.sh"] = b"changed"
        registry.files["https://charts.test/repo/index.yaml"] = INDEX.replace("c" * 64, "0" * 64).encode()
        failures, lines = run(registry=registry)
        self.assertEqual(failures, 3, lines)
        self.assertEqual(sum("the registry serves" in line for line in lines), 3)

    def test_fails_closed(self):
        ref, index = "reg.test/kube-vip/kube-vip:v0.8.9", "https://charts.test/repo/index.yaml"
        cases = {
            "no digest header": lambda r: r.manifests.update({ref: ""}),
            "tag gone": lambda r: r.manifests.pop(ref),
            "version gone from the index": lambda r: r.files.update({index: INDEX.replace("version: 1.18.16", "version: 1.18.17").encode()}),
            "index gone": lambda r: r.files.pop(index),
            "network down": lambda r: r.fail.update({index: [urllib.error.URLError("down")] * bv.ATTEMPTS}),
            "server errors": lambda r: r.fail.update({index: [http_error(index, 503)] * bv.ATTEMPTS}),
        }
        for name, breakage in cases.items():
            with self.subTest(name):
                registry = Registry()
                breakage(registry)
                failures, lines = run(registry=registry)
                self.assertEqual(failures, 1, lines)

    def test_retries_are_bounded_and_recover(self):
        index = "https://charts.test/repo/index.yaml"
        registry = Registry()
        registry.fail[index] = [urllib.error.URLError("reset"), http_error(index, 429)]
        self.assertEqual(run(registry=registry)[0], 0)
        self.assertEqual(sum(url == index for _, url in registry.calls), 3)

    def test_download_size_cap(self):
        failures, lines = run(max_download=4)
        self.assertEqual(failures, 1)
        self.assertIn("is larger than 4 bytes", "\n".join(lines))

    def test_pending_is_skipped_on_main_and_refused_at_a_release(self):
        text = BOM.replace('name: cilium, version: 1.18.16, digest: "%s"' % C, "name: cilium, version: 0.0.0+latest, digest: pending")
        failures, lines = run(text)
        self.assertEqual(failures, 0, lines)
        self.assertEqual(sum(line.startswith("skip ") for line in lines), 1)
        failures, lines = run(text, release=True)
        self.assertEqual(failures, 2, lines)  # the -dev release and the chart
        out = "\n".join(lines)
        for want in ("needs a BOM without -dev", "digest pending is not allowed", "version 0.0.0+latest is not allowed"):
            self.assertIn(want, out)
        self.assertEqual(run(BOM.replace("0.8.0-dev", "0.8.0"), release=True)[0], 0)

    def test_an_unreadable_pin_fails_the_run(self):
        for line in ('  extra: {name: x, digest: "%s"}' % A, "  other: {sha256: %s}" % ("a" * 64), '  odd: "reg.test/x@%s"' % A):
            with self.subTest(line), self.assertRaises(bv.VerifyError):
                bv.parse_bom(BOM + line + "\n")
        with self.assertRaises(bv.VerifyError):
            bv.parse_bom("schemaVersion: 1\nrelease: 1.0.0\n")

    def test_http_is_refused(self):
        with self.assertRaises(bv.VerifyError):
            bv.parse_bom(BOM.replace("https://charts.test", "http://charts.test"))
        with self.assertRaises(bv.VerifyError):
            bv.fetch(Registry(), "http://example.test/install.sh")

    def test_the_repository_bom_parses(self):
        release, items = bv.parse_bom((ROOT / bv.BOM).read_text(encoding="utf-8"))
        self.assertRegex(release, r"^\d+\.\d+\.\d+")
        self.assertEqual({item["kind"] for item in items}, {"oci", "index", "url"})
        self.assertGreaterEqual(len(items), 20)


if __name__ == "__main__":
    unittest.main()
