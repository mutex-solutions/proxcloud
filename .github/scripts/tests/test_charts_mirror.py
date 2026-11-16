#!/usr/bin/env python3
"""charts_mirror.py against a fake helm and git: no network, no registry, standard library only."""
import hashlib
import importlib.util
import io
import os
from pathlib import Path
import re
import tarfile
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[3]


def load(name):
    spec = importlib.util.spec_from_file_location(name, ROOT / ".github/scripts" / (name + ".py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


cm = load("charts_mirror")
bv = load("bom_verify")

COMMIT = "c" * 40
OCI_DIGEST = "sha256:" + "a" * 64


def tgz(name, version, extra=None, mtime=0, app=None):
    """A chart archive as bytes."""
    chart = "name: %s\nversion: %s\n" % (name, version) + ("appVersion: %s\n" % app if app else "")
    files = dict({"Chart.yaml": chart, "templates/a.yaml": "kind: A\n"}, **(extra or {}))
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w:gz") as tar:
        for path, text in files.items():
            info = tarfile.TarInfo("%s/%s" % (name, path))
            info.size, info.mtime = len(text), mtime
            tar.addfile(info, io.BytesIO(text.encode()))
    return buf.getvalue()


def sha(data):
    return "sha256:" + hashlib.sha256(data).hexdigest()


CILIUM, ARGO, ETCD = tgz("cilium", "1.2.3"), tgz("argo-cd", "9.9.9"), tgz("kamaji-etcd", "0.15.0")
MANIFEST = """# a comment
schemaVersion: 1
charts:
  - {name: cilium, version: 1.2.3, repo: "https://charts.test/cilium", digest: "%s", licence: Apache-2.0}
  - {name: argo-cd, version: 9.9.9, repo: reg.test/argo/charts, digest: "%s", licence: Apache-2.0}
  - {name: kamaji, version: 26.11.2-edge, git: "https://github.com/clastix/kamaji", tag: 26.11.2-edge, commit: %s, path: charts/kamaji, appVersion: 26.11.2-edge, licence: Apache-2.0, vendorName: kamaji-etcd, vendorVersion: 0.15.0, vendorRepo: "https://charts.test/clastix", vendorDigest: "%s"}
""" % (sha(CILIUM), OCI_DIGEST, COMMIT, sha(ETCD))
LOCK = "dependencies:\n- name: kamaji-etcd\n  repository: https://charts.test/clastix\n  version: 0.15.0\ndigest: sha256:x\n"


class Fake:
    """helm and git as charts_mirror.run: records every command, serves charts from dicts."""

    def __init__(self):
        self.https = {("https://charts.test/cilium", "cilium", "1.2.3"): CILIUM,
                      ("https://charts.test/clastix", "kamaji-etcd", "0.15.0"): ETCD}
        self.oci = {"reg.test/argo/charts/argo-cd@" + OCI_DIGEST: ARGO}
        self.mirror = {}        # (name, version) -> (bytes, digest)
        self.mirror_error = "Error: GET https://ghcr.io/token: response status code 403: denied"
        self.tag_commit = COMMIT
        self.tree = {"charts/kamaji/Chart.yaml": "name: kamaji\nversion: 0.0.0+latest\nappVersion: latest\n"
                                                 "dependencies:\n- name: kamaji-etcd\n  version: \">=0.15.0\"\n",
                     "charts/kamaji/Chart.lock": LOCK, "charts/kamaji/templates/a.yaml": "kind: A\n"}
        self.symlink = None
        self.calls = []
        self.clock = 0

    def __call__(self, cmd, env, cwd):
        self.calls.append((cmd, env, cwd))
        return getattr(self, cmd[0] + "_" + cmd[1].replace("-", "_"))(cmd[2:], env, cwd)

    def helm_pull(self, args, env, cwd):
        into = args[args.index("-d") + 1]
        if args[0].startswith("oci://" + cm.MIRROR + "/"):
            name = args[0].rsplit("/", 1)[1]
            if "@" in name:
                name, digest = name.split("@")
                found = [v for (n, _), v in self.mirror.items() if n == name and v[1] == digest]
            else:
                found = [v for key, v in self.mirror.items() if key == (name, args[args.index("--version") + 1])]
            if not found:
                return 1, self.mirror_error
            Path(into, name + ".tgz").write_bytes(found[0][0])
            return 0, "Pulled: x\nDigest: %s\n" % found[0][1]
        if args[0].startswith("oci://"):
            data = self.oci.get(args[0][len("oci://"):])
            if data is None:
                return 1, "Error: not found"
            Path(into, "chart@sha256-x.tgz").write_bytes(data)
            return 0, "Pulled: x\nDigest: %s\n" % args[0].rsplit("@", 1)[1]
        key = (args[args.index("--repo") + 1], args[0], args[args.index("--version") + 1])
        if key not in self.https:
            return 1, "Error: chart not found in repo"
        Path(into, "%s-%s.tgz" % (key[1], key[2])).write_bytes(self.https[key])
        return 0, ""

    def helm_package(self, args, env, cwd):
        chart, into = Path(args[0]), args[args.index("-d") + 1]
        version, app = args[args.index("--version") + 1], args[args.index("--app-version") + 1]
        name = re.search(r"^name: (\S+)", (chart / "Chart.yaml").read_text(), re.M).group(1)
        extra = {str(p.relative_to(chart)): "x" for p in chart.rglob("*") if p.is_file() and p.name != "Chart.yaml"}
        self.clock += 1  # like helm: another file time on every run
        Path(into, "%s-%s.tgz" % (name, version)).write_bytes(tgz(name, version, extra, self.clock, app))
        return 0, "Successfully packaged chart"

    def helm_push(self, args, env, cwd):
        data = Path(args[0]).read_bytes()
        name, version = re.fullmatch(r"(.+?)-(v?\d+\..*)\.tgz", os.path.basename(args[0])).groups()
        digest = "sha256:%064d" % (len(self.mirror) + 1)
        self.mirror[(name, version)] = (data, digest)
        return 0, "Pushed: %s/%s:%s\nDigest: %s\n" % (cm.MIRROR, name, version, digest)

    def git_init(self, args, env, cwd):
        return 0, ""

    def git_fetch(self, args, env, cwd):
        return 0, ""

    def git_rev_parse(self, args, env, cwd):
        return 0, self.tag_commit + "\n"

    def git_checkout(self, args, env, cwd):
        for path, text in self.tree.items():
            target = Path(cwd, path)
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text(text)
        if self.symlink:
            os.symlink("/etc/hostname", Path(cwd, self.symlink))
        return 0, ""

    def pushes(self):
        return [cmd for cmd, _, _ in self.calls if cmd[:2] == ["helm", "push"]]


class Base(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.out = os.path.join(self.tmp.name, "out")
        self.fake = Fake()

    def go(self, text=MANIFEST, push=False):
        return cm.mirror(text, self.fake, self.out, push, log=lambda _: None)

    def fails(self, want, text=MANIFEST, push=False):
        with self.assertRaises(cm.MirrorError) as caught:
            self.go(text, push)
        self.assertIn(want, str(caught.exception))
        return caught.exception


class ManifestTest(unittest.TestCase):
    def test_shapes(self):
        kinds = [e["kind"] for e in cm.parse_manifest(MANIFEST)]
        self.assertEqual(kinds, ["https", "oci", "git"])

    def test_refused(self):
        line = '  - {name: cilium, version: 1.2.3, repo: "https://charts.test/cilium", digest: "%s", licence: Apache-2.0}' % sha(CILIUM)
        head = "schemaVersion: 1\ncharts:\n"
        for bad, want in (
                (line.replace("1.2.3", "0.0.0+latest"), "version"),
                (line.replace("1.2.3", ">=1.2.3"), "version"),
                (line.replace(sha(CILIUM), "pending"), "digest"),
                (line.replace("https://", "http://"), "neither"),
                (line.replace('"https://charts.test/cilium"', cm.MIRROR), "neither"),
                (line.replace("Apache-2.0", "GPL-3.0"), "licence"),
                (line.replace(", licence: Apache-2.0", ""), "keys"),
                (line.replace("}", ", tag: v1}"), "keys"),
                (line.replace("}", ", vendorName: x}"), "keys"),
                (line.replace("}", ", name: other}"), "twice"),
                (line + "\n" + line, "listed twice"),
                (line.replace("  - {", "  - name: cilium\n    x: {"), "shape"),
                ("  - {name: k, version: 1.0.0, git: \"https://evil.test/a/b\", tag: v1, commit: %s, path: charts/k, appVersion: 1.0.0, licence: MIT}" % COMMIT, "git"),
                ("  - {name: k, version: 1.0.0, git: \"https://github.com/a/b\", tag: v1, commit: %s, path: ../k, appVersion: 1.0.0, licence: MIT}" % COMMIT, "path"),
                ("  - {name: k, version: 1.0.0, git: \"https://github.com/a/b\", tag: v1, commit: abc, path: k, appVersion: 1.0.0, licence: MIT}", "commit"),
        ):
            with self.assertRaises(cm.MirrorError, msg=bad) as caught:
                cm.parse_manifest(head + bad + "\n")
            self.assertIn(want, str(caught.exception))
        for text in ("", "schemaVersion: 2\ncharts:\n" + line, head, "charts:\n" + line):
            with self.assertRaises(cm.MirrorError):
                cm.parse_manifest(text)

    def test_manifest_equals_the_bom(self):
        """Every third-party chart of the BOM is in the manifest with the same pin, and nothing else is.

        A BOM chart from an upstream repository must be a manifest entry with that repo, version and digest; one at a
        moving 0.0.0+ version must be a git entry. A BOM chart already taken from the mirror must be a manifest entry
        of that name and version (its BOM digest is the mirror's manifest digest, which this file cannot know)."""
        entries = {e["name"]: e for e in cm.parse_manifest((ROOT / cm.MANIFEST).read_text())}
        bom = (ROOT / bv.BOM).read_text()
        used = set()
        charts = re.findall(r"\{repo: %s, name: %s, version: %s, digest: %s\}" % ((bv._V,) * 4), bom)
        self.assertTrue(charts)
        for repo, name, version, digest in charts:
            if repo.startswith("ghcr.io/mutex-solutions/") and repo != cm.MIRROR:
                continue  # our own charts
            self.assertIn(name, entries, "BOM chart %s is not in %s" % (name, cm.MANIFEST))
            entry = entries[name]
            used.add(name)
            if repo == cm.MIRROR:
                self.assertEqual(version, entry["version"], name)
            elif version.startswith("0.0.0+"):
                self.assertEqual((entry["kind"], digest), ("git", "pending"), name)
            else:
                self.assertEqual((repo, version, digest), (entry.get("repo"), entry["version"], entry.get("digest")), name)
        self.assertEqual(used, set(entries), "a manifest chart that the BOM does not use")
        # The git entries carry the image the BOM pins: tag = chart version = appVersion = image tag.
        image = re.search(r"values: \{image: \{tag: ([^}\s]+)\}\}", bom).group(1)
        for entry in entries.values():
            if entry["kind"] == "git":
                self.assertEqual({entry["tag"], entry["version"], entry["appVersion"]}, {image}, entry["name"])


class FetchTest(Base):
    def test_dry_run_verifies_packages_and_pushes_nothing(self):
        rows = self.go()
        self.assertEqual([r["result"] for r in rows], ["not published (would push)"] * 3)
        self.assertEqual(rows[0]["archive_sha256"], sha(CILIUM)[7:])
        self.assertEqual(sorted(os.listdir(self.out)), ["argo-cd-9.9.9.tgz", "cilium-1.2.3.tgz", "kamaji-26.11.2-edge.tgz"])
        self.assertEqual(self.fake.pushes(), [])
        package = [cmd for cmd, _, _ in self.fake.calls if cmd[:2] == ["helm", "package"]][0]
        self.assertEqual(package[3:7], ["--version", "26.11.2-edge", "--app-version", "26.11.2-edge"])
        with tarfile.open(os.path.join(self.out, "kamaji-26.11.2-edge.tgz")) as tar:
            self.assertIn("kamaji/charts/kamaji-etcd-0.15.0.tgz", tar.getnames())

    def test_no_credentials_reach_a_fetch(self):
        self.go()
        for cmd, env, _ in self.fake.calls:
            if cmd[0] == "helm":
                self.assertIn("anonymous", env["HELM_REGISTRY_CONFIG"], cmd)
                self.assertEqual(os.path.dirname(env["HELM_REGISTRY_CONFIG"]), env["DOCKER_CONFIG"], cmd)
            else:
                self.assertEqual((env["GIT_CONFIG_GLOBAL"], env["GIT_TERMINAL_PROMPT"]), (os.devnull, "0"))

    def test_https_digest_mismatch(self):
        self.fake.https[("https://charts.test/cilium", "cilium", "1.2.3")] = tgz("cilium", "1.2.3", {"evil": "x"})
        self.fails("pinned " + sha(CILIUM))

    def test_oci_digest_not_reported(self):
        real = self.fake.helm_pull
        self.fake.helm_pull = lambda args, env, cwd: (
            (real(args, env, cwd)[0], "Digest: sha256:%s\n" % ("b" * 64)) if "reg.test" in args[0] else real(args, env, cwd))
        self.fails("did not report that digest")

    def test_archive_is_another_chart(self):
        self.fake.oci["reg.test/argo/charts/argo-cd@" + OCI_DIGEST] = tgz("argo-cd", "9.9.8")
        self.fails("the manifest says 9.9.9")
        self.fake.oci["reg.test/argo/charts/argo-cd@" + OCI_DIGEST] = tgz("other", "9.9.9")
        self.out += "2"
        self.fails("does not hold exactly argo-cd/Chart.yaml")

    def test_tag_moved(self):
        self.fake.tag_commit = "d" * 40
        self.fails("points at " + "d" * 40)

    def test_symlink_in_the_chart(self):
        self.fake.symlink = "charts/kamaji/templates/link"
        self.fails("is a symlink")

    def test_dependencies_must_be_the_pinned_one(self):
        self.fake.tree["charts/kamaji/Chart.lock"] = LOCK.replace("0.15.0", "0.16.0")
        self.fails("the manifest pins")
        self.fake.tree["charts/kamaji/Chart.lock"] = LOCK + "- name: second\n  repository: https://x.test\n  version: 1.0.0\n"
        self.out += "2"
        self.fails("the manifest pins")
        del self.fake.tree["charts/kamaji/Chart.lock"]  # declared, but not locked
        self.out += "2"
        self.fails("the manifest pins")

    def test_unlisted_dependency(self):
        text = re.sub(r", vendorName.*\}", "}", MANIFEST)
        self.fails("has dependencies", text)

    def test_vendored_archive_mismatch(self):
        self.fake.https[("https://charts.test/clastix", "kamaji-etcd", "0.15.0")] = tgz("kamaji-etcd", "0.15.0", {"evil": "x"})
        self.fails("pinned " + sha(ETCD))

    def test_out_must_be_empty(self):
        os.makedirs(self.out)
        Path(self.out, "old.tgz").write_text("x")
        self.fails("is not empty")


class PublishTest(Base):
    def test_push_then_skip(self):
        rows = self.go(push=True)
        self.assertEqual([r["result"] for r in rows], ["pushed"] * 3)
        self.assertEqual(len(self.fake.pushes()), 3)
        self.assertTrue(all(cm._DIGEST.fullmatch(r["digest"]) for r in rows))
        self.assertEqual(rows[0]["reference"], cm.MIRROR + "/cilium:1.2.3")
        # A second run: nothing is pushed, also not the repackaged chart, whose bytes differ but whose files do not.
        self.out += "2"
        again = self.go(push=True)
        self.assertEqual([r["result"] for r in again], ["already published, same content"] * 3)
        self.assertEqual([r["digest"] for r in again], [r["digest"] for r in rows])
        self.assertNotEqual(again[2]["archive_sha256"], rows[2]["archive_sha256"])
        self.assertEqual(again[2]["content"], rows[2]["content"])
        self.assertEqual(len(self.fake.pushes()), 3)

    def test_different_content_is_never_replaced(self):
        self.fake.mirror[("argo-cd", "9.9.9")] = (tgz("argo-cd", "9.9.9", {"templates/b.yaml": "x"}), OCI_DIGEST)
        self.fails("already published with different content", push=True)
        self.assertEqual(self.fake.pushes(), [])  # not even the chart before it

    def test_unknown_mirror_answer_fails_closed(self):
        self.fake.mirror_error = "Error: 503 Service Unavailable"
        self.fails("cannot tell whether", push=True)
        self.assertEqual(self.fake.pushes(), [])

    def test_nothing_is_pushed_when_a_later_chart_fails(self):
        self.fake.tag_commit = "d" * 40
        self.fails("points at", push=True)
        self.assertEqual(self.fake.pushes(), [])

    def test_read_back_must_serve_the_pushed_bytes(self):
        push = self.fake.helm_push

        def tampering(args, env, cwd):
            code, output = push(args, env, cwd)
            key = next(k for k in self.fake.mirror if k[0] == "cilium")
            self.fake.mirror[key] = (tgz("cilium", "1.2.3", {"evil": "x"}), self.fake.mirror[key][1])
            return code, output
        self.fake.helm_push = tampering
        self.fails("serves other bytes", push=True)

    def test_only_mirror_calls_use_the_login(self):
        self.go(push=True)
        for cmd, env, _ in self.fake.calls:
            mirror = any(cm.MIRROR in arg for arg in cmd)
            if cmd[0] == "helm":
                self.assertEqual("HELM_REGISTRY_CONFIG" not in env or "anonymous" not in env["HELM_REGISTRY_CONFIG"], mirror, cmd)


class WorkflowTest(unittest.TestCase):
    def test_only_the_publish_job_may_write_packages_and_push(self):
        text = (ROOT / ".github/workflows/charts-mirror.yml").read_text()
        check, publish = text.split("\n  publish:\n")
        self.assertNotIn("packages: write", check)
        self.assertNotIn("--push", check)
        self.assertNotIn("helm registry login", check)
        self.assertEqual(publish.count("packages: write"), 1)
        self.assertIn("git merge-base --is-ancestor", publish)
        self.assertLess(publish.index("git merge-base --is-ancestor"), publish.index("helm registry login"))
        # Both jobs install the same helm, by sha256.
        self.assertEqual(text.count('printf \'%s  %s\\n\' "$HELM_SHA256"'), 2)
        for uses in re.findall(r"uses: (\S+)", text):
            self.assertRegex(uses, r"@[0-9a-f]{40}$")


if __name__ == "__main__":
    unittest.main()
