#!/usr/bin/env python3
"""Fetch the third-party charts of deploy/platform/charts-mirror.yaml exactly as pinned and republish them under
oci://ghcr.io/mutex-solutions/charts-mirror (stdlib only; shells out to helm and git).

Without --push nothing is published: every chart is fetched, verified and packaged into --out, and the mirror is only
read. Every step fails closed, and all charts are verified and compared with the mirror before the first push.

Per kind of entry (the shapes are described in the manifest):
  https  `helm pull --repo --version`; the sha256 of the .tgz must equal the pinned index digest.
  oci    `helm pull oci://<repo>/<name>@<digest>`; helm must report that digest.
  git    `git fetch` of the tag, which must still point at the pinned commit; no symlink in the chart directory; the
         dependencies of Chart.yaml and Chart.lock must be exactly the one pinned as vendor* (fetched like an https
         chart into charts/) or none; LICENSE and NOTICE of the repository root are copied into the chart and the
         NOTICE gets one paragraph that states what was changed; then `helm package --version --app-version`.
In every case the name and version inside the archive must be the ones of the manifest.

Fetching is anonymous by construction: those helm calls get an empty registry and Docker configuration, and git runs
without any configuration or prompt. Only the calls to the mirror in a --push run use the ambient helm login.

The script does not push over a version the mirror serves. A served version is compared file by file (path and
sha256 of every file in the archive): equal content is skipped, different content fails. `helm package` is not
byte-reproducible (it stamps the current time on every file), so for a git entry the archive's sha256 differs per
run; what is pinned afterwards is the manifest digest of the one archive that was pushed.

Only a definite "not found" from the mirror means that a version is absent. GHCR answers "denied" for a package that
does not exist (anonymously; measured) and, as far as we know, for one the caller may not read, and it moves a tag
on push instead of refusing, so with
--push a refusal (denied, unauthorized, 401, 403) or any other answer fails the run before the first push. Without
--push the read is anonymous and nothing is pushed: a refusal is reported as "not compared" and does not fail.

Output: one table (also <out>/charts-mirror.md) and <out>/charts-mirror.json, with the digest to pin per chart. When
a run fails after at least one chart was verified, both files are still written, marked incomplete, with the error
and whatever digests the run got.
"""
import argparse
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile

MANIFEST = "deploy/platform/charts-mirror.yaml"
MIRROR = "ghcr.io/mutex-solutions/charts-mirror"
LICENCES = ("Apache-2.0", "MIT")  # licences that allow redistribution of the unchanged chart
MAX_CHART = 64 << 20
TIMEOUT = 600
LICENCE_FILES = ("LICENSE", "NOTICE")
CHANGE_NOTICE = """\
This Helm chart is a repackaging of %s at tag %s
(commit %s), directory %s, published under %s.
Changed from that commit: Chart.yaml (version set to %s and appVersion set to %s, and
rewritten by `helm package`); LICENSE and NOTICE (copied from the repository root into the chart, and this
paragraph added to NOTICE). A dependency that Chart.lock pins is included under charts/ as its publisher's
unchanged archive. Nothing else was changed.
"""

_V = r'"?([^",{}\s]+)"?'
_PAIR = r"(\w+): %s" % _V
_ENTRY = re.compile(r"^  - \{(%s(?:, %s)*)\}$" % (_PAIR.replace("(", "(?:"), _PAIR.replace("(", "(?:")))
_COMMENT = re.compile(r"(^|\s)#.*$")
_NAME = re.compile(r"[a-z0-9]([-a-z0-9]*[a-z0-9])?")
_VERSION = re.compile(r"v?[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?")  # exact, and no "+": an OCI tag has none
_DIGEST = re.compile(r"sha256:[0-9a-f]{64}")
_COMMIT = re.compile(r"[0-9a-f]{40}")
_HTTPS = re.compile(r"https://[A-Za-z0-9.-]+(/[A-Za-z0-9._/-]*)?")
_OCI = re.compile(r"[a-z0-9.-]+\.[a-z]+(/[a-z0-9._-]+)+")
_GIT = re.compile(r"https://github\.com/[A-Za-z0-9._-]+/[A-Za-z0-9._-]+")
_TAG = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]*")
_PATH = re.compile(r"[A-Za-z0-9_-]+(/[A-Za-z0-9_-]+)*")
_ABSENT = re.compile(r"not found|name unknown|manifest unknown", re.I)
_REFUSED = re.compile(r"denied|unauthori[sz]ed|forbidden|\b40[13]\b", re.I)
_REPORTED = re.compile(r"^Digest: (sha256:[0-9a-f]{64})\s*$", re.M)
_LOCKED = re.compile(r"^- name: (\S+)\n\s+repository: (\S+)\n\s+version: (\S+)$", re.M)

_BASE = ("name", "version", "licence")
_VENDOR = ("vendorName", "vendorVersion", "vendorRepo", "vendorDigest")
_KEYS = {
    "repo": _BASE + ("repo", "digest"),
    "git": _BASE + ("git", "tag", "commit", "path", "appVersion"),
}
_RULES = {"name": _NAME, "version": _VERSION, "digest": _DIGEST, "commit": _COMMIT, "git": _GIT, "tag": _TAG,
          "path": _PATH, "appVersion": _VERSION, "vendorName": _NAME, "vendorVersion": _VERSION, "vendorRepo": _HTTPS,
          "vendorDigest": _DIGEST}


class MirrorError(Exception):
    pass


def parse_manifest(text):
    """Return the entries, each a dict with `kind` https|oci|git. Any line in another shape fails."""
    entries, seen, state = [], set(), "head"
    for n, raw in enumerate(text.splitlines(), 1):
        line = _COMMENT.sub("", raw).rstrip()
        if not line:
            continue
        if state == "head" and line == "schemaVersion: 1":
            state = "version"
        elif state == "version" and line == "charts:":
            state = "charts"
        elif state == "charts" and _ENTRY.match(line):
            pairs = re.findall(_PAIR, _ENTRY.match(line).group(1))
            entry = dict(pairs)
            if len(entry) != len(pairs):
                raise MirrorError("%s line %d: a key is given twice" % (MANIFEST, n))
            entries.append(_check(entry, n))
            if entry["name"] in seen:
                raise MirrorError("%s line %d: chart %s is listed twice" % (MANIFEST, n, entry["name"]))
            seen.add(entry["name"])
        else:
            raise MirrorError("%s line %d: a line in a shape this script does not read" % (MANIFEST, n))
    if not entries:
        raise MirrorError("%s lists no charts" % MANIFEST)
    return entries


def _check(entry, n):
    where = "%s line %d" % (MANIFEST, n)
    kind = "git" if "git" in entry else "repo"
    keys = set(entry) - set(_VENDOR) if kind == "git" else set(entry)
    vendor = set(entry) & set(_VENDOR)
    if keys != set(_KEYS[kind]) or (vendor and vendor != set(_VENDOR)):
        raise MirrorError("%s: the keys are not those of one of the three shapes" % where)
    for key, value in entry.items():
        rule = _RULES.get(key)
        if rule and not rule.fullmatch(value):
            raise MirrorError("%s: %s %r is not valid" % (where, key, value))
    if entry["licence"] not in LICENCES:
        raise MirrorError("%s: licence %r is not one of %s" % (where, entry["licence"], ", ".join(LICENCES)))
    if kind == "repo":
        kind = "https" if _HTTPS.fullmatch(entry["repo"]) else "oci" if _OCI.fullmatch(entry["repo"]) else None
        if not kind or (entry["repo"] + "/").startswith(MIRROR + "/"):
            raise MirrorError("%s: repo %r is neither a bare OCI repository nor https://" % (where, entry["repo"]))
    entry["kind"] = kind
    return entry


def run(cmd, env=None, cwd=None):
    """Run one command; returns (exit code, stdout and stderr together)."""
    try:
        done = subprocess.run(cmd, env=env, cwd=cwd, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                              stderr=subprocess.STDOUT, timeout=TIMEOUT, check=False)
    except (OSError, subprocess.TimeoutExpired) as err:
        return 1, "%s: %s" % (cmd[0], err)
    return done.returncode, done.stdout.decode("utf-8", "replace")


class Mirror:
    """One run. `run` is the command runner (tests pass a fake), `scratch` an empty directory."""

    def __init__(self, runner, scratch, out, push=False):
        self.run, self.scratch, self.out, self.push = runner, scratch, out, push
        anonymous = os.path.join(scratch, "anonymous")
        os.makedirs(anonymous)
        helm = {"HELM_REPOSITORY_CONFIG": os.path.join(scratch, "repositories.yaml"),
                "HELM_REPOSITORY_CACHE": os.path.join(scratch, "repository-cache")}
        self.ambient = dict(os.environ, **helm)
        self.anonymous = dict(self.ambient, HELM_REGISTRY_CONFIG=os.path.join(anonymous, "registry.json"),
                              DOCKER_CONFIG=anonymous)
        self.git_env = dict(os.environ, GIT_CONFIG_GLOBAL=os.devnull, GIT_CONFIG_NOSYSTEM="1", GIT_TERMINAL_PROMPT="0")
        self.count = 0

    def _dir(self, parent=None):
        self.count += 1
        path = os.path.join(parent or self.scratch, "d%d" % self.count)
        os.makedirs(path)
        return path

    def _must(self, cmd, env, cwd=None):
        code, output = self.run(cmd, env, cwd)
        if code:
            raise MirrorError("%s failed: %s" % (" ".join(cmd[:3]), output.strip()[-2000:]))
        return output

    def _pulled(self, cmd, env, into=None):
        """Run a `helm pull` into a new directory; returns (exit code, output, the one .tgz or None)."""
        into = into or self._dir()
        code, output = self.run(cmd + ["-d", into], env, None)
        files = sorted(f for f in os.listdir(into) if f.endswith(".tgz"))
        if not code and len(files) != 1:
            raise MirrorError("%s left %d archives, want one" % (" ".join(cmd[:3]), len(files)))
        return code, output, os.path.join(into, files[0]) if not code else None

    def pull_https(self, repo, name, version, digest, into=None):
        code, output, path = self._pulled(["helm", "pull", name, "--repo", repo, "--version", version],
                                          self.anonymous, into)
        if code:
            raise MirrorError("helm pull %s %s from %s failed: %s" % (name, version, repo, output.strip()[-2000:]))
        got = "sha256:" + sha256_file(path)
        if got != digest:
            raise MirrorError("%s %s from %s: pinned %s, downloaded %s" % (name, version, repo, digest, got))
        return path

    def pull_oci(self, repo, name, digest, env):
        ref = "oci://%s/%s@%s" % (repo, name, digest)
        code, output, path = self._pulled(["helm", "pull", ref], env)
        if code:
            raise MirrorError("helm pull %s failed: %s" % (ref, output.strip()[-2000:]))
        if _REPORTED.findall(output) != [digest]:
            raise MirrorError("helm pull %s did not report that digest" % ref)
        return path

    def build_git(self, entry):
        src = self._dir()
        self._must(["git", "init", "--quiet", src], self.git_env)
        self._must(["git", "fetch", "--quiet", "--depth", "1", entry["git"], "refs/tags/" + entry["tag"]],
                   self.git_env, src)
        got = self._must(["git", "rev-parse", "--verify", "FETCH_HEAD^{commit}"], self.git_env, src).strip()
        if got != entry["commit"]:
            raise MirrorError("%s tag %s points at %s, the manifest pins %s" % (entry["git"], entry["tag"], got, entry["commit"]))
        self._must(["git", "checkout", "--quiet", "--detach", "FETCH_HEAD"], self.git_env, src)
        chart = os.path.join(src, entry["path"])
        if not os.path.isfile(os.path.join(chart, "Chart.yaml")):
            raise MirrorError("%s has no %s/Chart.yaml at %s" % (entry["git"], entry["path"], entry["tag"]))
        # helm package follows links, so a link could put a file of this machine into a public package.
        for root, dirs, files in os.walk(chart):
            for item in dirs + files:
                if os.path.islink(os.path.join(root, item)):
                    raise MirrorError("%s: %s is a symlink" % (entry["name"], os.path.join(root, item)))
        self._vendor(entry, chart)
        self._licence(entry, src, chart)
        packed = self._dir()
        self._must(["helm", "package", chart, "--version", entry["version"], "--app-version", entry["appVersion"],
                    "-d", packed], self.anonymous)
        files = os.listdir(packed)
        if len(files) != 1:
            raise MirrorError("helm package %s left %d files, want one" % (entry["name"], len(files)))
        path = os.path.join(packed, files[0])
        held = _members(path)
        for item in LICENCE_FILES:  # a .helmignore of the chart could have dropped them
            if "%s/%s" % (entry["name"], item) not in held:
                raise MirrorError("%s: the packaged chart holds no %s" % (entry["name"], item))
        return path

    def _licence(self, entry, src, chart):
        """Apache-2.0 section 4 for a chart we repackage: give the licence, keep upstream's NOTICE, state the change.

        LICENSE is the one of the repository root, unchanged. NOTICE is the root's NOTICE (when it has one) followed
        by one paragraph of ours. The text has no date, so the content digest stays equal across runs."""
        name = entry["name"]
        for item in LICENCE_FILES:
            if os.path.lexists(os.path.join(chart, item)):
                raise MirrorError("%s: the chart directory already holds %s" % (name, item))
        texts = {}
        for item in LICENCE_FILES:
            path = os.path.join(src, item)
            if os.path.islink(path) or (os.path.lexists(path) and not os.path.isfile(path)):
                raise MirrorError("%s: %s of %s is not a regular file" % (name, item, entry["git"]))
            if os.path.isfile(path):
                with open(path, encoding="utf-8") as handle:
                    texts[item] = handle.read()
        if not texts.get("LICENSE", "").strip():
            raise MirrorError("%s: %s has no LICENSE at %s" % (name, entry["git"], entry["tag"]))
        changed = CHANGE_NOTICE % (entry["git"], entry["tag"], entry["commit"], entry["path"], "oci://" + MIRROR,
                                   entry["version"], entry["appVersion"])
        notice = texts.get("NOTICE", "").rstrip("\n")
        texts["NOTICE"] = (notice + "\n\n" if notice else "") + changed
        for item, text in texts.items():
            with open(os.path.join(chart, item), "w", encoding="utf-8") as handle:
                handle.write(text)

    def _vendor(self, entry, chart):
        """The chart's dependencies must be exactly the pinned one, or none."""
        name = entry["name"]
        with open(os.path.join(chart, "Chart.yaml"), encoding="utf-8") as handle:
            declared = bool(re.search(r"^dependencies:", handle.read(), re.M))
        lock = os.path.join(chart, "Chart.lock")
        locked = []
        if os.path.exists(lock):
            with open(lock, encoding="utf-8") as handle:
                text = handle.read()
            locked = _LOCKED.findall(text)
            if len(locked) != len(re.findall(r"^- ", text, re.M)):
                raise MirrorError("%s: a Chart.lock this script does not read" % name)
        if os.path.exists(os.path.join(chart, "charts")):
            raise MirrorError("%s: the chart directory already holds charts/" % name)
        want = [(entry["vendorName"], entry["vendorRepo"], entry["vendorVersion"])] if "vendorName" in entry else []
        if declared != bool(want) or locked != want:
            raise MirrorError("%s: Chart.yaml %s dependencies and Chart.lock pins %s, the manifest pins %s"
                              % (name, "has" if declared else "has no", locked or "none", want or "none"))
        if want:
            into = os.path.join(chart, "charts")
            os.makedirs(into)
            path = self.pull_https(entry["vendorRepo"], entry["vendorName"], entry["vendorVersion"],
                                   entry["vendorDigest"], into)
            identity(path, entry["vendorName"], entry["vendorVersion"])

    def fetch(self, entry):
        if entry["kind"] == "https":
            path = self.pull_https(entry["repo"], entry["name"], entry["version"], entry["digest"])
        elif entry["kind"] == "oci":
            path = self.pull_oci(entry["repo"], entry["name"], entry["digest"], self.anonymous)
        else:
            path = self.build_git(entry)
        identity(path, entry["name"], entry["version"], entry.get("appVersion"))
        final = os.path.join(self.out, "%s-%s.tgz" % (entry["name"], entry["version"]))
        shutil.move(path, final)
        return final

    def served(self, entry, path):
        """The manifest digest under which the mirror already serves this version with the same files; "" when the
        mirror definitely does not serve the version; None when a run without --push was refused the read (the
        package is absent or private, and nothing is pushed on that answer). Other content, a refusal with --push,
        or any other answer fails."""
        name, version = entry["name"], entry["version"]
        ref = "oci://%s/%s" % (MIRROR, name)
        code, output, served = self._pulled(["helm", "pull", ref, "--version", version],
                                            self.ambient if self.push else self.anonymous)
        if code:
            refused = _REFUSED.search(output)
            if _ABSENT.search(output) and not refused:
                return ""
            if refused and not self.push:
                return None
            raise MirrorError("cannot tell whether %s %s is published: %s" % (name, version, output.strip()[-2000:]))
        digests = _REPORTED.findall(output)
        if len(digests) != 1:
            raise MirrorError("helm pull %s %s reported no digest" % (ref, version))
        if content(served) != content(path):
            raise MirrorError("%s %s is already published with different content (%s); a version is never "
                              "replaced" % (name, version, digests[0]))
        return digests[0]

    def publish(self, entry, path):
        """Push one version the mirror does not serve and read it back. Returns its manifest digest."""
        name, version = entry["name"], entry["version"]
        digests = _REPORTED.findall(self._must(["helm", "push", path, "oci://" + MIRROR], self.ambient))
        if len(digests) != 1:
            raise MirrorError("helm push %s %s reported no digest; the version may be published" % (name, version))
        back = self.pull_oci(MIRROR, name, digests[0], self.ambient)
        if sha256_file(back) != sha256_file(path):
            raise MirrorError("%s %s: the mirror serves other bytes than were pushed (%s)" % (name, version, digests[0]))
        return digests[0]


def sha256_file(path):
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for chunk in iter(lambda: handle.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _members(path):
    """{path: bytes} of the regular files of a chart archive; anything else in it fails."""
    if os.path.getsize(path) > MAX_CHART:
        raise MirrorError("%s is larger than %d bytes" % (path, MAX_CHART))
    files, size = {}, 0
    try:
        with tarfile.open(path, "r:gz") as tar:
            for member in tar:
                if member.isdir():
                    continue
                if not member.isfile() or member.name in files:
                    raise MirrorError("%s: %s is not one regular file" % (path, member.name))
                size += member.size
                if size > 4 * MAX_CHART:
                    raise MirrorError("%s unpacks to more than %d bytes" % (path, 4 * MAX_CHART))
                files[member.name] = tar.extractfile(member).read()
    except (tarfile.TarError, OSError, EOFError) as err:
        raise MirrorError("%s is not a chart archive: %s" % (path, err))
    return files


def content(path):
    """A digest of the archive's files (path and sha256 of each), independent of file times and compression."""
    files = _members(path)
    lines = ["%s  %s\n" % (hashlib.sha256(files[name]).hexdigest(), name) for name in sorted(files)]
    return "sha256:" + hashlib.sha256("".join(lines).encode()).hexdigest()


def identity(path, name, version, app_version=None):
    """The archive's Chart.yaml must carry this name and version: helm push derives the reference from them."""
    files = _members(path)
    charts = [n for n in files if n.count("/") == 1 and n.endswith("/Chart.yaml")]
    if charts != [name + "/Chart.yaml"]:
        raise MirrorError("%s does not hold exactly %s/Chart.yaml" % (path, name))
    text = files[charts[0]].decode("utf-8", "replace")
    for key, want in (("name", name), ("version", version), ("appVersion", app_version)):
        got = [v.strip("\"'") for v in re.findall(r"^%s:[ \t]*(\S+)[ \t]*$" % key, text, re.M)]
        if want is not None and got != [want]:
            raise MirrorError("%s: Chart.yaml has %s %s, the manifest says %s" % (path, key, got, want))


def table(rows):
    lines = ["| chart | version | source | chart archive sha256 | content | mirror manifest digest | result |",
             "|---|---|---|---|---|---|---|"]
    for row in rows:
        lines.append("| %s | %s | %s | `%s` | `%s` | %s | %s |" % (
            row["name"], row["version"], row["source"], row["archive_sha256"], row["content"],
            "`%s`" % row["digest"] if row["digest"] else "-", row["result"]))
    return "\n".join(lines) + "\n"


NOT_COMPARED = "not compared: the mirror refuses an anonymous read (absent or private)"


def mirror(text, runner, out, push=False, log=print, rows=None):
    """Fetch and verify every chart, then compare with the mirror and (with push) publish. Returns the rows.

    A caller that passes `rows` finds in it what the run reached when this raises."""
    rows = [] if rows is None else rows
    entries = parse_manifest(text)
    os.makedirs(out, exist_ok=True)
    if os.listdir(out):
        raise MirrorError("%s is not empty" % out)
    with tempfile.TemporaryDirectory(prefix="charts-mirror-") as scratch:
        job = Mirror(runner, scratch, out, push)
        paths = []
        for entry in entries:
            path = job.fetch(entry)
            source = {"https": "%s", "oci": "oci://%s", "git": "%s"}[entry["kind"]] % (
                entry.get("repo") or "%s@%s (%s)" % (entry["git"], entry["tag"], entry["commit"][:12]))
            paths.append(path)
            rows.append({"name": entry["name"], "version": entry["version"], "source": source, "kind": entry["kind"],
                         "archive_sha256": sha256_file(path), "content": content(path), "digest": "",
                         "result": "verified, not compared with the mirror",
                         "reference": "%s/%s:%s" % (MIRROR, entry["name"], entry["version"])})
            log("verified %s %s" % (entry["name"], entry["version"]))
        absent = []
        for entry, row, path in zip(entries, rows, paths):  # every version is compared before the first push
            found = job.served(entry, path)
            row["digest"] = found or ""
            row["result"] = ("already published, same content" if found else
                             NOT_COMPARED if found is None else "not published")
            absent.append(found == "")
        for entry, row, path, missing in zip(entries, rows, paths, absent):
            if missing and push:
                row["result"] = "push attempted, not confirmed"
                row["digest"] = job.publish(entry, path)
                row["result"] = "pushed"
            elif missing:
                row["result"] = "not published (would push)"
            log("%s %s: %s %s" % (row["name"], row["version"], row["result"], row["digest"]))
    return rows


def record(rows, out, push, error=None):
    """Write <out>/charts-mirror.md and .json; returns the table. With `error` both say that the run is incomplete."""
    text = table(rows)
    if error:
        text = "**Incomplete run, failed with:** %s\n\n%s" % (" ".join(error.split())[:2000], text)
    with open(os.path.join(out, "charts-mirror.md"), "w", encoding="utf-8") as handle:
        handle.write(text)
    with open(os.path.join(out, "charts-mirror.json"), "w", encoding="utf-8") as handle:
        json.dump({"mirror": MIRROR, "pushed": push, "complete": not error, "error": error, "charts": rows}, handle,
                  indent=2)
        handle.write("\n")
    return text


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--manifest", default=MANIFEST)
    parser.add_argument("--out", default="charts-mirror-out", help="an empty or missing directory for the result")
    parser.add_argument("--push", action="store_true", help="publish what the mirror does not serve yet")
    args = parser.parse_args(argv)
    rows, error = [], None
    try:
        with open(args.manifest, encoding="utf-8") as handle:
            text = handle.read()
        mirror(text, run, args.out, args.push, rows=rows)
    except (MirrorError, OSError) as err:
        error = str(err)
    if rows or not error:  # a run that failed before the first chart has nothing to record
        print(record(rows, args.out, args.push, error), end="")
    if error:
        print("FAIL %s" % error)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
